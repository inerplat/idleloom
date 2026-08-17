package controller

import (
	"context"
	"errors"
	"fmt"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// reconcileModelStatuses aggregates the memory geometry agents measured for
// each pinned artifact into the owning IdleloomModel's status. The controller
// is the only writer of model status; agents only ever report through their
// own host status. A digest identifies the exact bytes, so two hosts
// disagreeing about one digest means a broken parser somewhere, and the
// aggregation withdraws the profile rather than pick a side.
func (r *Reconciler) reconcileModelStatuses(ctx context.Context) error {
	modelList, err := r.Dynamic.Resource(nativekube.ModelsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list native models: %w", err)
	}
	hostList, err := r.Dynamic.Resource(nativekube.HostsGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list native hosts: %w", err)
	}
	type observation struct {
		profile  *nativev1alpha1.ModelMemoryProfile
		conflict bool
		reported bool
	}
	observed := make(map[string]*observation)
	for i := range hostList.Items {
		var host nativev1alpha1.IdleloomHost
		if err := nativekube.FromUnstructured(&hostList.Items[i], &host); err != nil {
			continue
		}
		for _, entry := range host.Status.AvailableModels {
			current := observed[entry.ManifestDigest]
			if current == nil {
				current = &observation{}
				observed[entry.ManifestDigest] = current
			}
			current.reported = true
			if entry.Memory == nil {
				continue
			}
			switch {
			case current.profile == nil:
				measured := *entry.Memory
				current.profile = &measured
			case *current.profile != *entry.Memory:
				current.conflict = true
			}
		}
	}
	var errs []error
	for i := range modelList.Items {
		var model nativev1alpha1.IdleloomModel
		if err := nativekube.FromUnstructured(&modelList.Items[i], &model); err != nil {
			errs = append(errs, err)
			continue
		}
		report := observed[model.Spec.Artifact.ManifestDigest]
		if report == nil {
			// No host carries this artifact right now. Keep whatever was
			// aggregated before instead of flapping the profile with host
			// churn; the digest pins the bytes, so a past measurement holds.
			continue
		}
		copy := model.DeepCopy()
		condition := metav1.Condition{
			Type:               nativev1alpha1.ModelConditionMemoryProfileMeasured,
			ObservedGeneration: model.Generation,
			LastTransitionTime: metav1.NewTime(r.now()),
		}
		switch {
		case report.conflict:
			copy.Status.MemoryProfile = nil
			condition.Status = metav1.ConditionFalse
			condition.Reason = "ConflictingMeasurements"
			condition.Message = "hosts reported different geometry for the same artifact digest; admission falls back to the conservative estimate"
		case report.profile != nil:
			copy.Status.MemoryProfile = report.profile
			condition.Status = metav1.ConditionTrue
			condition.Reason = "Measured"
			condition.Message = fmt.Sprintf("architecture %s uses %d KV cache bytes per context token", report.profile.Architecture, report.profile.KVBytesPerToken)
		default:
			copy.Status.MemoryProfile = nil
			condition.Status = metav1.ConditionFalse
			condition.Reason = "UnparseableHeader"
			condition.Message = "no host could read this artifact's header geometry; admission falls back to the conservative estimate"
		}
		existing := apiMeta.FindStatusCondition(model.Status.Conditions, nativev1alpha1.ModelConditionMemoryProfileMeasured)
		profileUnchanged := equalMemoryProfiles(model.Status.MemoryProfile, copy.Status.MemoryProfile)
		conditionUnchanged := existing != nil && existing.Status == condition.Status && existing.Reason == condition.Reason
		if profileUnchanged && conditionUnchanged {
			continue
		}
		apiMeta.SetStatusCondition(&copy.Status.Conditions, condition)
		object, err := nativekube.ToUnstructured(copy)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := r.Dynamic.Resource(nativekube.ModelsGVR).UpdateStatus(ctx, object, metav1.UpdateOptions{}); err != nil {
			errs = append(errs, fmt.Errorf("update model %s memory profile: %w", model.Name, err))
		}
	}
	return errors.Join(errs...)
}

func equalMemoryProfiles(left, right *nativev1alpha1.ModelMemoryProfile) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
