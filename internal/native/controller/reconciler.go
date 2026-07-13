package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/fencing"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	"github.com/inerplat/idleloom/internal/native/scheduler"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	coordinationclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
)

type Reconciler struct {
	Dynamic      dynamic.Interface
	Coordination coordinationclient.CoordinationV1Interface
	Planner      scheduler.Planner
	Now          func() time.Time
}

type reconcileCycle struct {
	reconciler  *Reconciler
	models      map[string]*nativev1alpha1.IdleloomModel
	hosts       []nativev1alpha1.IdleloomHost
	hostsLoaded bool
}

func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	if r.Dynamic == nil || r.Coordination == nil {
		return fmt.Errorf("dynamic and coordination clients are required")
	}
	list, err := r.Dynamic.Resource(nativekube.WorkloadsGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list native workloads: %w", err)
	}
	cycle := &reconcileCycle{reconciler: r, models: make(map[string]*nativev1alpha1.IdleloomModel)}
	var errs []error
	for i := range list.Items {
		var workload nativev1alpha1.IdleloomWorkload
		if err := nativekube.FromUnstructured(&list.Items[i], &workload); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := r.reconcileWorkloadWithCycle(ctx, &workload, cycle); err != nil {
			errs = append(errs, fmt.Errorf("reconcile workload %s/%s: %w", workload.Namespace, workload.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Reconciler) reconcileWorkload(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload) error {
	return r.reconcileWorkloadWithCycle(ctx, workload, &reconcileCycle{reconciler: r, models: make(map[string]*nativev1alpha1.IdleloomModel)})
}

func (r *Reconciler) reconcileWorkloadWithCycle(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload, cycle *reconcileCycle) error {
	if workload.DeletionTimestamp != nil {
		return r.reconcileDeleting(ctx, workload)
	}
	if !contains(workload.Finalizers, nativev1alpha1.WorkloadFinalizer) {
		copy := workload.DeepCopy()
		copy.Finalizers = append(copy.Finalizers, nativev1alpha1.WorkloadFinalizer)
		if err := r.updateWorkload(ctx, copy, false); err != nil {
			return err
		}
		return nil
	}
	if workload.Status.AssignmentRef != nil {
		return r.reflectAssignment(ctx, workload)
	}
	var model *nativev1alpha1.IdleloomModel
	if workload.Spec.Mode == nativev1alpha1.WorkloadModeServer || workload.Spec.Mode == nativev1alpha1.WorkloadModeBatch {
		if workload.Spec.Model == nil {
			return fmt.Errorf("model workload has no model reference")
		}
		var err error
		model, err = cycle.model(ctx, workload.Spec.Model.CatalogRef)
		if err != nil {
			return err
		}
	}
	if workload.Status.SchedulingIntent == nil {
		hosts, err := cycle.hostList(ctx)
		if err != nil {
			return err
		}
		return r.persistSchedulingIntent(ctx, workload, model, hosts, cycle)
	}
	return r.createAssignmentFromIntent(ctx, workload, model)
}

func (r *Reconciler) persistSchedulingIntent(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload, model *nativev1alpha1.IdleloomModel, hosts []nativev1alpha1.IdleloomHost, cycle *reconcileCycle) error {
	host, err := r.Planner.SelectHost(workload, model, hosts)
	if err != nil {
		return err
	}
	epoch, err := fencing.Allocate(ctx, r.Coordination.Leases(host.Namespace), host.UID)
	if err != nil {
		return fmt.Errorf("allocate fencing epoch: %w", err)
	}
	planned, err := r.Planner.PlanAssignment(workload, model, host, epoch)
	if err != nil {
		return err
	}
	copy := workload.DeepCopy()
	copy.Status.ObservedGeneration = workload.Generation
	copy.Status.Phase = nativev1alpha1.PhaseScheduling
	intent := &nativev1alpha1.WorkloadSchedulingIntent{
		WorkloadGeneration: workload.Generation,
		HostRef: nativev1alpha1.NamespacedObjectReference{
			Namespace: host.Namespace,
			Name:      host.Name,
			UID:       host.UID,
		},
		ExecutionID:  planned.Spec.ExecutionID,
		FencingEpoch: planned.Spec.FencingEpoch,
	}
	if model != nil {
		intent.ModelRef = &nativev1alpha1.ObjectReference{Name: model.Name, UID: model.UID}
	}
	copy.Status.SchedulingIntent = intent
	if err := r.updateWorkload(ctx, copy, true); err != nil {
		return err
	}
	cycle.reserveHost(host.UID)
	return nil
}

func (r *Reconciler) createAssignmentFromIntent(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload, model *nativev1alpha1.IdleloomModel) error {
	intent := workload.Status.SchedulingIntent
	if intent.WorkloadGeneration != workload.Generation || !intentMatchesModel(intent, model) {
		return fmt.Errorf("persisted scheduling intent does not match the current workload and model")
	}
	assignmentResource := r.Dynamic.Resource(nativekube.AssignmentsGVR).Namespace(intent.HostRef.Namespace)
	existing, err := assignmentResource.Get(ctx, nativev1alpha1.AssignmentMailboxName, metav1.GetOptions{})
	if err == nil {
		var assignment nativev1alpha1.IdleloomWorkloadAssignment
		if err := nativekube.FromUnstructured(existing, &assignment); err != nil {
			return err
		}
		if err := validateAssignmentIdentity(&assignment, workload, intent); err != nil {
			return err
		}
		return r.persistAssignmentReference(ctx, workload, &assignment)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get assignment mailbox: %w", err)
	}
	hostObject, err := r.Dynamic.Resource(nativekube.HostsGVR).Namespace(intent.HostRef.Namespace).Get(ctx, intent.HostRef.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get intended host: %w", err)
	}
	var host nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(hostObject, &host); err != nil {
		return err
	}
	if host.UID != intent.HostRef.UID {
		return fmt.Errorf("persisted scheduling intent refers to a replaced host")
	}
	planner := r.Planner
	planner.NewExecutionID = func() (string, error) { return intent.ExecutionID, nil }
	planned, err := planner.PlanAssignment(workload, model, &host, intent.FencingEpoch)
	if err != nil {
		return err
	}
	unstructured, err := nativekube.ToUnstructured(planned)
	if err != nil {
		return err
	}
	created, err := assignmentResource.Create(ctx, unstructured, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		created, err = assignmentResource.Get(ctx, nativev1alpha1.AssignmentMailboxName, metav1.GetOptions{})
	}
	if err != nil {
		return fmt.Errorf("create assignment: %w", err)
	}
	var assignment nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(created, &assignment); err != nil {
		return err
	}
	if err := validateAssignmentIdentity(&assignment, workload, intent); err != nil {
		return err
	}
	return r.persistAssignmentReference(ctx, workload, &assignment)
}

func intentMatchesModel(intent *nativev1alpha1.WorkloadSchedulingIntent, model *nativev1alpha1.IdleloomModel) bool {
	if model == nil {
		return intent.ModelRef == nil
	}
	return intent.ModelRef != nil && intent.ModelRef.Name == model.Name && intent.ModelRef.UID == model.UID
}

func (r *Reconciler) persistAssignmentReference(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload, assignment *nativev1alpha1.IdleloomWorkloadAssignment) error {
	copy := workload.DeepCopy()
	copy.Status.Phase = nativev1alpha1.PhaseAssigned
	copy.Status.AssignmentRef = &nativev1alpha1.NamespacedObjectReference{Namespace: assignment.Namespace, Name: assignment.Name, UID: assignment.UID}
	return r.updateWorkload(ctx, copy, true)
}

func (r *Reconciler) reflectAssignment(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload) error {
	ref := workload.Status.AssignmentRef
	object, err := r.Dynamic.Resource(nativekube.AssignmentsGVR).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("assigned mailbox disappeared")
	}
	if err != nil {
		return err
	}
	var assignment nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(object, &assignment); err != nil {
		return err
	}
	if assignment.UID != ref.UID || assignment.Spec.WorkloadRef.UID != workload.UID || assignment.Spec.WorkloadRef.Generation != workload.Generation {
		return fmt.Errorf("assignment identity does not match workload")
	}
	copy := workload.DeepCopy()
	copy.Status.ObservedGeneration = workload.Generation
	phase, stale := r.assignmentPhase(&assignment)
	copy.Status.Phase = phase
	copy.Status.ResolvedArtifactDigest = assignment.Status.ResolvedArtifactDigest
	condition := metav1.Condition{
		Type:               nativev1alpha1.WorkloadConditionReady,
		ObservedGeneration: workload.Generation,
		LastTransitionTime: metav1.NewTime(r.now()),
	}
	if stale {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "AssignmentHeartbeatStale"
		condition.Message = "the assigned native agent stopped renewing its execution lease"
	} else if phase == nativev1alpha1.PhaseRunning {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "AssignmentRunning"
		condition.Message = "the native assignment is running with a fresh heartbeat"
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "AssignmentNotReady"
		condition.Message = "the native assignment is not running"
	}
	apiMeta.SetStatusCondition(&copy.Status.Conditions, condition)
	return r.updateWorkload(ctx, copy, true)
}

func (r *Reconciler) assignmentPhase(assignment *nativev1alpha1.IdleloomWorkloadAssignment) (string, bool) {
	if assignment.Spec.DesiredState != nativev1alpha1.AssignmentDesiredRunning {
		return assignment.Status.Phase, false
	}
	lease := time.Duration(assignment.Spec.LeaseDurationSeconds) * time.Second
	last := assignment.CreationTimestamp.Time
	if assignment.Status.LastHeartbeatTime != nil {
		last = assignment.Status.LastHeartbeatTime.Time
	}
	now := r.now()
	if last.After(now.Add(nativev1alpha1.HeartbeatClockSkewAllowance)) || now.Sub(last) > lease+nativev1alpha1.HeartbeatClockSkewAllowance {
		return nativev1alpha1.PhaseFenced, true
	}
	if assignment.Status.Phase == "" {
		return nativev1alpha1.PhaseAssigned, false
	}
	return assignment.Status.Phase, false
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) reconcileDeleting(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload) error {
	if !contains(workload.Finalizers, nativev1alpha1.WorkloadFinalizer) {
		return nil
	}
	ref := workload.Status.AssignmentRef
	intent := workload.Status.SchedulingIntent
	if ref == nil && intent == nil {
		return r.removeFinalizer(ctx, workload)
	}
	namespace := ""
	name := nativev1alpha1.AssignmentMailboxName
	if ref != nil {
		namespace = ref.Namespace
		name = ref.Name
	} else {
		namespace = intent.HostRef.Namespace
	}
	resource := r.Dynamic.Resource(nativekube.AssignmentsGVR).Namespace(namespace)
	object, err := resource.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return r.removeFinalizer(ctx, workload)
	}
	if err != nil {
		return err
	}
	var assignment nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(object, &assignment); err != nil {
		return err
	}
	if ref != nil {
		if assignment.UID != ref.UID || assignment.Spec.WorkloadRef.UID != workload.UID {
			return fmt.Errorf("refusing to stop a mailbox owned by another workload")
		}
	} else if err := validateAssignmentIdentity(&assignment, workload, intent); err != nil {
		return fmt.Errorf("refusing to stop mailbox from scheduling intent: %w", err)
	}
	if assignment.Spec.DesiredState != nativev1alpha1.AssignmentDesiredStopped {
		assignment.Spec.DesiredState = nativev1alpha1.AssignmentDesiredStopped
		updated, err := nativekube.ToUnstructured(&assignment)
		if err != nil {
			return err
		}
		if _, err := resource.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("request assignment stop: %w", err)
		}
		return nil
	}
	if err := nativev1alpha1.ValidateStopAcknowledgement(&assignment); err != nil {
		return nil
	}
	if err := resource.Delete(ctx, assignment.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &assignment.UID}}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete stopped assignment: %w", err)
	}
	return r.removeFinalizer(ctx, workload)
}

func (r *Reconciler) removeFinalizer(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload) error {
	copy := workload.DeepCopy()
	copy.Finalizers = remove(copy.Finalizers, nativev1alpha1.WorkloadFinalizer)
	return r.updateWorkload(ctx, copy, false)
}

func (r *Reconciler) getModel(ctx context.Context, name string) (*nativev1alpha1.IdleloomModel, error) {
	object, err := r.Dynamic.Resource(nativekube.ModelsGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get model %s: %w", name, err)
	}
	var model nativev1alpha1.IdleloomModel
	if err := nativekube.FromUnstructured(object, &model); err != nil {
		return nil, err
	}
	return &model, nil
}

func (r *Reconciler) listHosts(ctx context.Context) ([]nativev1alpha1.IdleloomHost, error) {
	list, err := r.Dynamic.Resource(nativekube.HostsGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	hosts := make([]nativev1alpha1.IdleloomHost, 0, len(list.Items))
	for i := range list.Items {
		var host nativev1alpha1.IdleloomHost
		if err := nativekube.FromUnstructured(&list.Items[i], &host); err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

func (c *reconcileCycle) model(ctx context.Context, name string) (*nativev1alpha1.IdleloomModel, error) {
	if model := c.models[name]; model != nil {
		return model, nil
	}
	model, err := c.reconciler.getModel(ctx, name)
	if err != nil {
		return nil, err
	}
	c.models[name] = model
	return model, nil
}

func (c *reconcileCycle) hostList(ctx context.Context) ([]nativev1alpha1.IdleloomHost, error) {
	if c.hostsLoaded {
		return c.hosts, nil
	}
	hosts, err := c.reconciler.listHosts(ctx)
	if err != nil {
		return nil, err
	}
	c.hosts = hosts
	c.hostsLoaded = true
	return c.hosts, nil
}

func (c *reconcileCycle) reserveHost(uid types.UID) {
	for index := range c.hosts {
		if c.hosts[index].UID == uid {
			c.hosts[index].Status.ActiveAssignmentUID = types.UID("reserved-in-reconcile-cycle")
			return
		}
	}
}

func (r *Reconciler) updateWorkload(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload, status bool) error {
	object, err := nativekube.ToUnstructured(workload)
	if err != nil {
		return err
	}
	resource := r.Dynamic.Resource(nativekube.WorkloadsGVR).Namespace(workload.Namespace)
	if status {
		_, err = resource.UpdateStatus(ctx, object, metav1.UpdateOptions{})
	} else {
		_, err = resource.Update(ctx, object, metav1.UpdateOptions{})
	}
	return err
}

func validateAssignmentIdentity(assignment *nativev1alpha1.IdleloomWorkloadAssignment, workload *nativev1alpha1.IdleloomWorkload, intent *nativev1alpha1.WorkloadSchedulingIntent) error {
	if assignment.Spec.WorkloadRef.UID != workload.UID || assignment.Spec.WorkloadRef.Generation != workload.Generation || assignment.Spec.ExecutionID != intent.ExecutionID || assignment.Spec.FencingEpoch != intent.FencingEpoch || assignment.Spec.HostRef.UID != intent.HostRef.UID {
		return fmt.Errorf("existing assignment does not match the persisted scheduling intent")
	}
	return nil
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func remove(values []string, value string) []string {
	result := values[:0]
	for _, item := range values {
		if item != value {
			result = append(result, item)
		}
	}
	return result
}
