package controller

import (
	"context"
	"strings"
	"testing"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func memoryProfileModel(name, digest string) *nativev1alpha1.IdleloomModel {
	return &nativev1alpha1.IdleloomModel{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomModel"},
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid"), Generation: 1},
		Spec: nativev1alpha1.IdleloomModelSpec{
			Family: nativev1alpha1.ModelFamilyGGUF, RuntimeProfile: nativev1alpha1.RuntimeProfileLlamaCppMetalV1,
			Artifact: nativev1alpha1.ModelArtifact{
				GGUFFile: name + ".gguf", ManifestDigest: digest,
				Format: nativev1alpha1.ArtifactFormatGGUFV1, SizeBytes: 1 << 30,
			},
			MinimumUnifiedMemory: resource.MustParse("8Gi"), MaxContextLength: 2048, MaxConcurrentRequests: 1,
		},
	}
}

func memoryProfileHost(namespace string, digest string, memory *nativev1alpha1.ModelMemoryProfile) *nativev1alpha1.IdleloomHost {
	return &nativev1alpha1.IdleloomHost{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomHost"},
		ObjectMeta: metav1.ObjectMeta{Name: "host", Namespace: namespace, UID: types.UID(namespace + "-uid")},
		Status: nativev1alpha1.IdleloomHostStatus{
			AvailableModels: []nativev1alpha1.HostModelStatus{{
				RuntimeProfile: nativev1alpha1.RuntimeProfileLlamaCppMetalV1,
				Name:           "model.gguf", ManifestDigest: digest,
				Family: nativev1alpha1.ModelFamilyGGUF, Format: nativev1alpha1.ArtifactFormatGGUFV1,
				SizeBytes: 1 << 30, Memory: memory,
			}},
		},
	}
}

func modelStatusListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		nativekube.ModelsGVR: "IdleloomModelList", nativekube.HostsGVR: "IdleloomHostList",
	}
}

// asUnstructured seeds fixtures the way the API server stores them. Mixing
// typed seeds with the unstructured writes the reconciler issues breaks the
// fake client's list conversion.
func asUnstructured(t *testing.T, objects ...any) []runtime.Object {
	t.Helper()
	out := make([]runtime.Object, 0, len(objects))
	for _, object := range objects {
		converted, err := nativekube.ToUnstructured(object)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, converted)
	}
	return out
}

func readModel(t *testing.T, client *dynamicfake.FakeDynamicClient, name string) nativev1alpha1.IdleloomModel {
	t.Helper()
	object, err := client.Resource(nativekube.ModelsGVR).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var model nativev1alpha1.IdleloomModel
	if err := nativekube.FromUnstructured(object, &model); err != nil {
		t.Fatal(err)
	}
	return model
}

func TestModelStatusAggregatesMeasuredProfile(t *testing.T) {
	scheme := runtime.NewScheme()
	digest := "sha256:" + strings.Repeat("d", 64)
	measured := &nativev1alpha1.ModelMemoryProfile{
		Architecture: "qwen35", BlockCount: 65, KVBytesPerToken: 133120, TrainedContextLength: 262144,
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, modelStatusListKinds(),
		asUnstructured(t, memoryProfileModel("qwen", digest), memoryProfileHost("idleloom-host-a", digest, measured))...)
	reconciler := &Reconciler{Dynamic: client, Coordination: kubernetesfake.NewClientset().CoordinationV1()}
	if err := reconciler.reconcileModelStatuses(context.Background()); err != nil {
		t.Fatal(err)
	}
	model := readModel(t, client, "qwen")
	if model.Status.MemoryProfile == nil || *model.Status.MemoryProfile != *measured {
		t.Fatalf("memory profile = %#v, want the host measurement", model.Status.MemoryProfile)
	}
	condition := apiMeta.FindStatusCondition(model.Status.Conditions, nativev1alpha1.ModelConditionMemoryProfileMeasured)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "Measured" {
		t.Fatalf("condition = %#v", condition)
	}
	// A second pass with identical inputs must not write again.
	before := len(client.Actions())
	if err := reconciler.reconcileModelStatuses(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, action := range client.Actions()[before:] {
		if action.GetVerb() == "update" {
			t.Fatal("unchanged aggregation issued a status update")
		}
	}
}

func TestModelStatusWithdrawsOnConflict(t *testing.T) {
	scheme := runtime.NewScheme()
	digest := "sha256:" + strings.Repeat("e", 64)
	left := &nativev1alpha1.ModelMemoryProfile{Architecture: "qwen35", BlockCount: 65, KVBytesPerToken: 133120, TrainedContextLength: 262144}
	right := &nativev1alpha1.ModelMemoryProfile{Architecture: "qwen35", BlockCount: 65, KVBytesPerToken: 999424, TrainedContextLength: 262144}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, modelStatusListKinds(),
		asUnstructured(t, memoryProfileModel("qwen", digest),
			memoryProfileHost("idleloom-host-a", digest, left),
			memoryProfileHost("idleloom-host-b", digest, right))...)
	reconciler := &Reconciler{Dynamic: client, Coordination: kubernetesfake.NewClientset().CoordinationV1()}
	if err := reconciler.reconcileModelStatuses(context.Background()); err != nil {
		t.Fatal(err)
	}
	model := readModel(t, client, "qwen")
	if model.Status.MemoryProfile != nil {
		t.Fatalf("conflicting measurements still published %#v", model.Status.MemoryProfile)
	}
	condition := apiMeta.FindStatusCondition(model.Status.Conditions, nativev1alpha1.ModelConditionMemoryProfileMeasured)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ConflictingMeasurements" {
		t.Fatalf("condition = %#v", condition)
	}
}

func TestModelStatusReportsUnparseableHeader(t *testing.T) {
	scheme := runtime.NewScheme()
	digest := "sha256:" + strings.Repeat("f", 64)
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, modelStatusListKinds(),
		asUnstructured(t, memoryProfileModel("qwen", digest), memoryProfileHost("idleloom-host-a", digest, nil))...)
	reconciler := &Reconciler{Dynamic: client, Coordination: kubernetesfake.NewClientset().CoordinationV1()}
	if err := reconciler.reconcileModelStatuses(context.Background()); err != nil {
		t.Fatal(err)
	}
	model := readModel(t, client, "qwen")
	condition := apiMeta.FindStatusCondition(model.Status.Conditions, nativev1alpha1.ModelConditionMemoryProfileMeasured)
	if model.Status.MemoryProfile != nil || condition == nil || condition.Reason != "UnparseableHeader" {
		t.Fatalf("profile = %#v condition = %#v", model.Status.MemoryProfile, condition)
	}
}

func TestModelStatusKeepsProfileWhenNoHostReports(t *testing.T) {
	scheme := runtime.NewScheme()
	digest := "sha256:" + strings.Repeat("a", 64)
	model := memoryProfileModel("qwen", digest)
	model.Status.MemoryProfile = &nativev1alpha1.ModelMemoryProfile{
		Architecture: "qwen35", BlockCount: 65, KVBytesPerToken: 133120, TrainedContextLength: 262144,
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, modelStatusListKinds(), asUnstructured(t, model)...)
	reconciler := &Reconciler{Dynamic: client, Coordination: kubernetesfake.NewClientset().CoordinationV1()}
	if err := reconciler.reconcileModelStatuses(context.Background()); err != nil {
		t.Fatal(err)
	}
	updated := readModel(t, client, "qwen")
	if updated.Status.MemoryProfile == nil {
		t.Fatal("host churn dropped a previously measured profile")
	}
}
