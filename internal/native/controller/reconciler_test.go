package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func TestReflectAssignmentFencesStaleRunningStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	heartbeat := metav1.NewMicroTime(now.Add(-31*time.Second - nativev1alpha1.HeartbeatClockSkewAllowance))
	workload := &nativev1alpha1.IdleloomWorkload{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "workload", Namespace: "tenant", UID: types.UID("workload-uid"), Generation: 1,
		},
		Status: nativev1alpha1.IdleloomWorkloadStatus{AssignmentRef: &nativev1alpha1.NamespacedObjectReference{
			Namespace: "host-ns", Name: nativev1alpha1.AssignmentMailboxName, UID: types.UID("assignment-uid"),
		}},
	}
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: nativev1alpha1.AssignmentMailboxName, Namespace: "host-ns", UID: types.UID("assignment-uid"), CreationTimestamp: metav1.NewTime(now.Add(-time.Minute)),
		},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning, LeaseDurationSeconds: 30,
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{Namespace: "tenant", Name: "workload", UID: workload.UID, Generation: 1},
		},
		Status: nativev1alpha1.IdleloomWorkloadAssignmentStatus{Phase: nativev1alpha1.PhaseRunning, LastHeartbeatTime: &heartbeat},
	}
	listKinds := map[schema.GroupVersionResource]string{
		nativekube.WorkloadsGVR: "IdleloomWorkloadList", nativekube.AssignmentsGVR: "IdleloomWorkloadAssignmentList",
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, workload, assignment)
	reconciler := &Reconciler{Dynamic: client, Now: func() time.Time { return now }}
	if err := reconciler.reflectAssignment(context.Background(), workload.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.WorkloadsGVR).Namespace("tenant").Get(context.Background(), "workload", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomWorkload
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != nativev1alpha1.PhaseFenced {
		t.Fatalf("workload phase = %q, want %q", updated.Status.Phase, nativev1alpha1.PhaseFenced)
	}
	condition := apiMeta.FindStatusCondition(updated.Status.Conditions, nativev1alpha1.WorkloadConditionReady)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "AssignmentHeartbeatStale" {
		t.Fatalf("ready condition = %#v", condition)
	}
}

func TestReflectServingAssignmentWaitsForEndpointReadiness(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	heartbeat := metav1.NewMicroTime(now)
	workload := &nativev1alpha1.IdleloomWorkload{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "serve", Namespace: "tenant", UID: types.UID("workload-uid"), Generation: 1,
		},
		Spec: nativev1alpha1.IdleloomWorkloadSpec{
			Mode: nativev1alpha1.WorkloadModeServer,
			Server: &nativev1alpha1.WorkloadServer{
				ServiceName: "serve", ModelAlias: "qwen",
			},
		},
		Status: nativev1alpha1.IdleloomWorkloadStatus{AssignmentRef: &nativev1alpha1.NamespacedObjectReference{
			Namespace: "host-ns", Name: nativev1alpha1.AssignmentMailboxName, UID: types.UID("assignment-uid"),
		}},
	}
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: nativev1alpha1.AssignmentMailboxName, Namespace: "host-ns", UID: types.UID("assignment-uid"), CreationTimestamp: metav1.NewTime(now),
		},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning, LeaseDurationSeconds: 30,
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{Namespace: "tenant", Name: "serve", UID: workload.UID, Generation: 1},
		},
		Status: nativev1alpha1.IdleloomWorkloadAssignmentStatus{Phase: nativev1alpha1.PhaseRunning, LastHeartbeatTime: &heartbeat},
	}
	listKinds := map[schema.GroupVersionResource]string{
		nativekube.WorkloadsGVR: "IdleloomWorkloadList", nativekube.AssignmentsGVR: "IdleloomWorkloadAssignmentList",
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, workload, assignment)
	reconciler := &Reconciler{Dynamic: client, Now: func() time.Time { return now }}
	if err := reconciler.reflectAssignment(context.Background(), workload.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.WorkloadsGVR).Namespace(workload.Namespace).Get(context.Background(), workload.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomWorkload
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	condition := apiMeta.FindStatusCondition(updated.Status.Conditions, nativev1alpha1.WorkloadConditionReady)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "ServingEndpointPending" {
		t.Fatalf("serving ready condition = %#v", condition)
	}

	readyTransition := metav1.NewTime(now.Add(-time.Minute))
	apiMeta.SetStatusCondition(&updated.Status.Conditions, metav1.Condition{
		Type: nativev1alpha1.WorkloadConditionReady, Status: metav1.ConditionTrue,
		ObservedGeneration: updated.Generation, LastTransitionTime: readyTransition,
		Reason: "ServingEndpointReady", Message: "the Native serving Service has a ready WireKube endpoint",
	})
	readyObject, err := nativekube.ToUnstructured(&updated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resource(nativekube.WorkloadsGVR).Namespace(updated.Namespace).UpdateStatus(context.Background(), readyObject, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.reflectAssignment(context.Background(), &updated); err != nil {
		t.Fatal(err)
	}
	object, err = client.Resource(nativekube.WorkloadsGVR).Namespace(workload.Namespace).Get(context.Background(), workload.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	condition = apiMeta.FindStatusCondition(updated.Status.Conditions, nativev1alpha1.WorkloadConditionReady)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "ServingEndpointReady" || !condition.LastTransitionTime.Equal(&readyTransition) {
		t.Fatalf("preserved serving ready condition = %#v", condition)
	}
}

func TestAssignmentPhaseAllowsBoundedPastClockSkew(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	heartbeat := metav1.NewMicroTime(now.Add(-30*time.Second - nativev1alpha1.HeartbeatClockSkewAllowance + time.Second))
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning, LeaseDurationSeconds: 30,
		},
		Status: nativev1alpha1.IdleloomWorkloadAssignmentStatus{
			Phase: nativev1alpha1.PhaseRunning, LastHeartbeatTime: &heartbeat,
		},
	}
	phase, stale := (&Reconciler{Now: func() time.Time { return now }}).assignmentPhase(assignment)
	if stale || phase != nativev1alpha1.PhaseRunning {
		t.Fatalf("assignment phase/stale = %s/%v, want Running/false", phase, stale)
	}
}

func TestReconcileCycleReservesSelectedHost(t *testing.T) {
	cycle := &reconcileCycle{hosts: []nativev1alpha1.IdleloomHost{
		{ObjectMeta: metav1.ObjectMeta{UID: types.UID("first")}},
		{ObjectMeta: metav1.ObjectMeta{UID: types.UID("second")}},
	}}
	cycle.reserveHost(types.UID("first"))
	if cycle.hosts[0].Status.ActiveAssignmentUID == "" {
		t.Fatal("selected host was not reserved for the reconciliation cycle")
	}
	if cycle.hosts[1].Status.ActiveAssignmentUID != "" {
		t.Fatal("unselected host was reserved")
	}
}

func TestReconcileOnceReusesModelAndHostSnapshot(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	model := &nativev1alpha1.IdleloomModel{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomModel"},
		ObjectMeta: metav1.ObjectMeta{Name: "shared-model", UID: types.UID("model-uid")},
	}
	workload := func(name string) *nativev1alpha1.IdleloomWorkload {
		return &nativev1alpha1.IdleloomWorkload{
			TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: "tenant", UID: types.UID(name + "-uid"), Generation: 1, Finalizers: []string{nativev1alpha1.WorkloadFinalizer},
			},
			Spec: nativev1alpha1.IdleloomWorkloadSpec{
				Mode:  nativev1alpha1.WorkloadModeServer,
				Model: &nativev1alpha1.WorkloadModelReference{CatalogRef: model.Name},
			},
		}
	}
	listKinds := map[schema.GroupVersionResource]string{
		nativekube.WorkloadsGVR: "IdleloomWorkloadList", nativekube.ModelsGVR: "IdleloomModelList", nativekube.HostsGVR: "IdleloomHostList",
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, model, workload("one"), workload("two"))
	reconciler := &Reconciler{Dynamic: client, Coordination: kubernetesfake.NewClientset().CoordinationV1()}
	if err := reconciler.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("reconcile unexpectedly found an eligible host")
	}
	var modelGets, hostLists int
	for _, action := range client.Actions() {
		if action.GetVerb() == "get" && action.GetResource() == nativekube.ModelsGVR {
			modelGets++
		}
		if action.GetVerb() == "list" && action.GetResource() == nativekube.HostsGVR {
			hostLists++
		}
	}
	if modelGets != 1 || hostLists != 1 {
		t.Fatalf("model gets/host lists = %d/%d, want 1/1 per reconciliation cycle", modelGets, hostLists)
	}
}

func TestBatchWorkloadResolvesModelCatalog(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	model := &nativev1alpha1.IdleloomModel{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomModel"},
		ObjectMeta: metav1.ObjectMeta{Name: "batch-model", UID: types.UID("model-uid")},
		Spec: nativev1alpha1.IdleloomModelSpec{
			Family: nativev1alpha1.ModelFamilyQwen35, RuntimeProfile: nativev1alpha1.RuntimeProfileMLXLMV1,
			Artifact: nativev1alpha1.ModelArtifact{
				OCIReference: "oci://registry.example/model@" + digest, ManifestDigest: digest,
				Format: nativev1alpha1.ArtifactFormatSafetensorsV1, SizeBytes: 1024,
				Signature: &nativev1alpha1.SignaturePolicy{Issuer: "https://issuer.example", Subject: "publisher"},
			},
			MinimumUnifiedMemory: resource.MustParse("8Gi"), MaxContextLength: 2048, MaxConcurrentRequests: 1,
		},
	}
	workload := &nativev1alpha1.IdleloomWorkload{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "batch", Namespace: "tenant", UID: types.UID("workload-uid"), Generation: 1,
			Finalizers: []string{nativev1alpha1.WorkloadFinalizer},
		},
		Spec: nativev1alpha1.IdleloomWorkloadSpec{
			Mode:      nativev1alpha1.WorkloadModeBatch,
			Model:     &nativev1alpha1.WorkloadModelReference{CatalogRef: model.Name},
			Batch:     &nativev1alpha1.WorkloadBatchInference{Prompt: "hello", MaxTokens: 32},
			Resources: nativev1alpha1.WorkloadResources{UnifiedMemoryRequest: resource.MustParse("8Gi")},
		},
	}
	listKinds := map[schema.GroupVersionResource]string{
		nativekube.ModelsGVR: "IdleloomModelList", nativekube.HostsGVR: "IdleloomHostList",
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, model, workload)
	reconciler := &Reconciler{Dynamic: client, Coordination: kubernetesfake.NewClientset().CoordinationV1()}
	if err := reconciler.reconcileWorkload(context.Background(), workload.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	updatedObject, err := client.Resource(nativekube.WorkloadsGVR).Namespace(workload.Namespace).Get(context.Background(), workload.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomWorkload
	if err := nativekube.FromUnstructured(updatedObject, &updated); err != nil {
		t.Fatal(err)
	}
	condition := apiMeta.FindStatusCondition(updated.Status.Conditions, nativev1alpha1.WorkloadConditionReady)
	if updated.Status.Phase != nativev1alpha1.PhaseScheduling || condition == nil || condition.Reason != "Queued" {
		t.Fatalf("queued workload status = phase %q condition %#v", updated.Status.Phase, condition)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "get" && action.GetResource() == nativekube.ModelsGVR {
			return
		}
	}
	t.Fatal("batch reconcile did not resolve its model catalog entry")
}

func TestCompletedAssignmentMailboxIsArchivedAndReclaimed(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	oldWorkload := &nativev1alpha1.IdleloomWorkload{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "old-run", Namespace: "tenant", UID: types.UID("old-workload-uid"), Generation: 1,
			Finalizers: []string{nativev1alpha1.WorkloadFinalizer},
		},
		Status: nativev1alpha1.IdleloomWorkloadStatus{
			SchedulingIntent: &nativev1alpha1.WorkloadSchedulingIntent{
				WorkloadGeneration: 1,
				HostRef:            nativev1alpha1.NamespacedObjectReference{Namespace: "host-ns", Name: "host", UID: types.UID("host-uid")},
				ExecutionID:        "11111111-1111-4111-8111-111111111111", FencingEpoch: 1,
			},
			AssignmentRef: &nativev1alpha1.NamespacedObjectReference{
				Namespace: "host-ns", Name: nativev1alpha1.AssignmentMailboxName, UID: types.UID("old-assignment-uid"),
			},
		},
	}
	newWorkload := &nativev1alpha1.IdleloomWorkload{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "new-run", Namespace: "tenant", UID: types.UID("new-workload-uid"), Generation: 1,
			Finalizers: []string{nativev1alpha1.WorkloadFinalizer},
		},
		Spec: nativev1alpha1.IdleloomWorkloadSpec{
			Mode:      nativev1alpha1.WorkloadModeShell,
			Shell:     &nativev1alpha1.WorkloadShell{Script: "echo new"},
			Resources: nativev1alpha1.WorkloadResources{UnifiedMemoryRequest: resource.MustParse("1Gi")},
		},
		Status: nativev1alpha1.IdleloomWorkloadStatus{SchedulingIntent: &nativev1alpha1.WorkloadSchedulingIntent{
			WorkloadGeneration: 1,
			HostRef:            nativev1alpha1.NamespacedObjectReference{Namespace: "host-ns", Name: "host", UID: types.UID("host-uid")},
			ExecutionID:        "22222222-2222-4222-8222-222222222222", FencingEpoch: 2,
		}},
	}
	assignment := terminalShellAssignment(oldWorkload, now)
	listKinds := map[schema.GroupVersionResource]string{
		nativekube.WorkloadsGVR: "IdleloomWorkloadList", nativekube.AssignmentsGVR: "IdleloomWorkloadAssignmentList",
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, oldWorkload, newWorkload, assignment)
	reconciler := &Reconciler{Dynamic: client, Now: func() time.Time { return now }}
	if err := reconciler.createAssignmentFromIntent(context.Background(), newWorkload.DeepCopy(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resource(nativekube.AssignmentsGVR).Namespace("host-ns").Get(context.Background(), nativev1alpha1.AssignmentMailboxName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("terminal assignment was not reclaimed: %v", err)
	}
	oldObject, err := client.Resource(nativekube.WorkloadsGVR).Namespace("tenant").Get(context.Background(), "old-run", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var archived nativev1alpha1.IdleloomWorkload
	if err := nativekube.FromUnstructured(oldObject, &archived); err != nil {
		t.Fatal(err)
	}
	if archived.Status.Phase != nativev1alpha1.PhaseSucceeded || archived.Status.AssignmentRef != nil || archived.Status.SchedulingIntent != nil {
		t.Fatalf("archived workload status = %#v", archived.Status)
	}
	newObject, err := client.Resource(nativekube.WorkloadsGVR).Namespace("tenant").Get(context.Background(), "new-run", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var queued nativev1alpha1.IdleloomWorkload
	if err := nativekube.FromUnstructured(newObject, &queued); err != nil {
		t.Fatal(err)
	}
	condition := apiMeta.FindStatusCondition(queued.Status.Conditions, nativev1alpha1.WorkloadConditionReady)
	if condition == nil || condition.Reason != "MailboxReclaimed" {
		t.Fatalf("new workload condition = %#v", condition)
	}
}

func TestRunningAssignmentMailboxLeavesNextWorkloadQueued(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	newWorkload := &nativev1alpha1.IdleloomWorkload{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
		ObjectMeta: metav1.ObjectMeta{Name: "new-run", Namespace: "tenant", UID: types.UID("new-workload-uid"), Generation: 1},
		Status: nativev1alpha1.IdleloomWorkloadStatus{SchedulingIntent: &nativev1alpha1.WorkloadSchedulingIntent{
			WorkloadGeneration: 1,
			HostRef:            nativev1alpha1.NamespacedObjectReference{Namespace: "host-ns", Name: "host", UID: types.UID("host-uid")},
			ExecutionID:        "22222222-2222-4222-8222-222222222222", FencingEpoch: 2,
		}},
	}
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{Name: nativev1alpha1.AssignmentMailboxName, Namespace: "host-ns", UID: types.UID("running-assignment"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef:  nativev1alpha1.WorkloadObjectReference{Namespace: "tenant", Name: "old-run", UID: types.UID("old-workload"), Generation: 1},
			HostRef:      nativev1alpha1.ObjectReference{Name: "host", UID: types.UID("host-uid")},
			ExecutionID:  "11111111-1111-4111-8111-111111111111", FencingEpoch: 1,
		},
		Status: nativev1alpha1.IdleloomWorkloadAssignmentStatus{Phase: nativev1alpha1.PhaseRunning},
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		nativekube.WorkloadsGVR: "IdleloomWorkloadList", nativekube.AssignmentsGVR: "IdleloomWorkloadAssignmentList",
	}, newWorkload, assignment)
	reconciler := &Reconciler{Dynamic: client}
	if err := reconciler.createAssignmentFromIntent(context.Background(), newWorkload.DeepCopy(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resource(nativekube.AssignmentsGVR).Namespace("host-ns").Get(context.Background(), nativev1alpha1.AssignmentMailboxName, metav1.GetOptions{}); err != nil {
		t.Fatal("running assignment was deleted")
	}
	object, err := client.Resource(nativekube.WorkloadsGVR).Namespace("tenant").Get(context.Background(), "new-run", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var queued nativev1alpha1.IdleloomWorkload
	if err := nativekube.FromUnstructured(object, &queued); err != nil {
		t.Fatal(err)
	}
	condition := apiMeta.FindStatusCondition(queued.Status.Conditions, nativev1alpha1.WorkloadConditionReady)
	if condition == nil || condition.Reason != "HostBusy" {
		t.Fatalf("queued condition = %#v", condition)
	}
}

func TestServerWorkloadRecoversAfterObservedAssignmentFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	workload := &nativev1alpha1.IdleloomWorkload{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "server", Namespace: "tenant", UID: types.UID("server-workload"), Generation: 1,
			Finalizers: []string{nativev1alpha1.WorkloadFinalizer},
		},
		Spec: nativev1alpha1.IdleloomWorkloadSpec{
			Mode:   nativev1alpha1.WorkloadModeServer,
			Model:  &nativev1alpha1.WorkloadModelReference{CatalogRef: "model"},
			Server: &nativev1alpha1.WorkloadServer{ServiceName: "server", ModelAlias: "model"},
		},
		Status: nativev1alpha1.IdleloomWorkloadStatus{
			Phase: nativev1alpha1.PhaseFailed,
			AssignmentRef: &nativev1alpha1.NamespacedObjectReference{
				Namespace: "host-ns", Name: nativev1alpha1.AssignmentMailboxName, UID: types.UID("assignment"),
			},
		},
	}
	heartbeat := metav1.NewMicroTime(now)
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: nativev1alpha1.AssignmentMailboxName, Namespace: "host-ns", UID: types.UID("assignment"),
			Generation: 1, CreationTimestamp: metav1.NewTime(now),
		},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{
				Namespace: workload.Namespace, Name: workload.Name, UID: workload.UID, Generation: workload.Generation,
			},
			ExecutionID: "11111111-1111-4111-8111-111111111111", FencingEpoch: 1, LeaseDurationSeconds: 30,
		},
		Status: nativev1alpha1.IdleloomWorkloadAssignmentStatus{
			ObservedGeneration: 1, Phase: nativev1alpha1.PhaseRunning, AgentID: "studio.native",
			ExecutionID: "11111111-1111-4111-8111-111111111111", FencingEpoch: 1, LastHeartbeatTime: &heartbeat,
		},
	}
	client := dynamicfake.NewSimpleDynamicClient(scheme, workload, assignment)
	reconciler := &Reconciler{Dynamic: client, Now: func() time.Time { return now }}
	if err := reconciler.reconcileWorkload(context.Background(), workload.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.WorkloadsGVR).Namespace(workload.Namespace).Get(context.Background(), workload.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomWorkload
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != nativev1alpha1.PhaseRunning {
		t.Fatalf("server workload did not recover from Failed: %#v", updated.Status)
	}
}

func terminalShellAssignment(workload *nativev1alpha1.IdleloomWorkload, now time.Time) *nativev1alpha1.IdleloomWorkloadAssignment {
	heartbeat := metav1.NewMicroTime(now)
	return &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: nativev1alpha1.AssignmentMailboxName, Namespace: "host-ns", UID: types.UID("old-assignment-uid"), Generation: 1,
		},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef:  nativev1alpha1.WorkloadObjectReference{Namespace: workload.Namespace, Name: workload.Name, UID: workload.UID, Generation: workload.Generation},
			HostRef:      nativev1alpha1.ObjectReference{Name: "host", UID: types.UID("host-uid")},
			Shell:        &nativev1alpha1.ResolvedShell{Script: "echo old"},
			ExecutionID:  "11111111-1111-4111-8111-111111111111", FencingEpoch: 1,
		},
		Status: nativev1alpha1.IdleloomWorkloadAssignmentStatus{
			ObservedGeneration: 1, Phase: nativev1alpha1.PhaseSucceeded, AgentID: "studio.native",
			ExecutionID: "11111111-1111-4111-8111-111111111111", FencingEpoch: 1, LastHeartbeatTime: &heartbeat,
		},
	}
}

func TestDeletingWorkloadStopsAssignmentFromSchedulingIntent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.NewTime(time.Now())
	workload := &nativev1alpha1.IdleloomWorkload{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
		ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "tenant", UID: types.UID("workload-uid"), Generation: 1, Finalizers: []string{nativev1alpha1.WorkloadFinalizer}, DeletionTimestamp: &now},
		Status: nativev1alpha1.IdleloomWorkloadStatus{SchedulingIntent: &nativev1alpha1.WorkloadSchedulingIntent{
			WorkloadGeneration: 1,
			HostRef:            nativev1alpha1.NamespacedObjectReference{Namespace: "host-ns", Name: "host", UID: types.UID("host-uid")},
			ExecutionID:        "11111111-1111-4111-8111-111111111111",
			FencingEpoch:       7,
		}},
	}
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{Name: nativev1alpha1.AssignmentMailboxName, Namespace: "host-ns", UID: types.UID("assignment-uid"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef:  nativev1alpha1.WorkloadObjectReference{Namespace: "tenant", Name: "workload", UID: workload.UID, Generation: 1},
			HostRef:      nativev1alpha1.ObjectReference{Name: "host", UID: types.UID("host-uid")},
			ExecutionID:  "11111111-1111-4111-8111-111111111111", FencingEpoch: 7,
		},
	}
	listKinds := map[schema.GroupVersionResource]string{nativekube.WorkloadsGVR: "IdleloomWorkloadList", nativekube.AssignmentsGVR: "IdleloomWorkloadAssignmentList"}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, workload, assignment)
	reconciler := &Reconciler{Dynamic: client}
	if err := reconciler.reconcileDeleting(context.Background(), workload.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.AssignmentsGVR).Namespace("host-ns").Get(context.Background(), "active", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.DesiredState != nativev1alpha1.AssignmentDesiredStopped {
		t.Fatal("controller did not request stop for assignment discovered from scheduling intent")
	}
}

func TestDeletingArchivedWorkloadIgnoresReusedMailbox(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.NewTime(time.Now())
	workload := &nativev1alpha1.IdleloomWorkload{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "archived", Namespace: "tenant", UID: types.UID("archived-workload"), Generation: 1,
			Finalizers: []string{nativev1alpha1.WorkloadFinalizer}, DeletionTimestamp: &now,
		},
		Status: nativev1alpha1.IdleloomWorkloadStatus{SchedulingIntent: &nativev1alpha1.WorkloadSchedulingIntent{
			WorkloadGeneration: 1,
			HostRef:            nativev1alpha1.NamespacedObjectReference{Namespace: "host-ns", Name: "host", UID: types.UID("host-uid")},
			ExecutionID:        "11111111-1111-4111-8111-111111111111", FencingEpoch: 1,
		}},
	}
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{Name: nativev1alpha1.AssignmentMailboxName, Namespace: "host-ns", UID: types.UID("current-assignment")},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{Namespace: "tenant", Name: "current", UID: types.UID("current-workload"), Generation: 1},
			ExecutionID: "22222222-2222-4222-8222-222222222222", FencingEpoch: 2,
		},
	}
	listKinds := map[schema.GroupVersionResource]string{nativekube.WorkloadsGVR: "IdleloomWorkloadList", nativekube.AssignmentsGVR: "IdleloomWorkloadAssignmentList"}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, workload, assignment)
	reconciler := &Reconciler{Dynamic: client}
	if err := reconciler.reconcileDeleting(context.Background(), workload.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.WorkloadsGVR).Namespace("tenant").Get(context.Background(), workload.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomWorkload
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if contains(updated.Finalizers, nativev1alpha1.WorkloadFinalizer) {
		t.Fatalf("archived workload finalizer remains: %#v", updated.Finalizers)
	}
}

func TestExistingAssignmentCompletesPersistedIntentAfterControllerCrash(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	workload := &nativev1alpha1.IdleloomWorkload{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
		ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "tenant", UID: types.UID("workload-uid"), Generation: 1, Finalizers: []string{nativev1alpha1.WorkloadFinalizer}},
		Spec: nativev1alpha1.IdleloomWorkloadSpec{
			Mode:  nativev1alpha1.WorkloadModeServer,
			Model: &nativev1alpha1.WorkloadModelReference{CatalogRef: "model"},
		},
		Status: nativev1alpha1.IdleloomWorkloadStatus{SchedulingIntent: &nativev1alpha1.WorkloadSchedulingIntent{
			WorkloadGeneration: 1,
			HostRef:            nativev1alpha1.NamespacedObjectReference{Namespace: "host-ns", Name: "host", UID: types.UID("host-uid")},
			ModelRef:           &nativev1alpha1.ObjectReference{Name: "model", UID: types.UID("model-uid")},
			ExecutionID:        "11111111-1111-4111-8111-111111111111",
			FencingEpoch:       7,
		}},
	}
	model := &nativev1alpha1.IdleloomModel{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomModel"},
		ObjectMeta: metav1.ObjectMeta{Name: "model", UID: types.UID("model-uid")},
	}
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{Name: nativev1alpha1.AssignmentMailboxName, Namespace: "host-ns", UID: types.UID("assignment-uid")},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{Namespace: "tenant", Name: "workload", UID: workload.UID, Generation: 1},
			HostRef:     nativev1alpha1.ObjectReference{Name: "host", UID: types.UID("host-uid")},
			ExecutionID: "11111111-1111-4111-8111-111111111111", FencingEpoch: 7,
		},
	}
	listKinds := map[schema.GroupVersionResource]string{
		nativekube.WorkloadsGVR: "IdleloomWorkloadList", nativekube.ModelsGVR: "IdleloomModelList",
		nativekube.HostsGVR: "IdleloomHostList", nativekube.AssignmentsGVR: "IdleloomWorkloadAssignmentList",
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, workload, model, assignment)
	clientset := kubernetesfake.NewClientset()
	reconciler := &Reconciler{Dynamic: client, Coordination: clientset.CoordinationV1()}
	if err := reconciler.reconcileWorkload(context.Background(), workload.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	updatedObject, err := client.Resource(nativekube.WorkloadsGVR).Namespace("tenant").Get(context.Background(), "workload", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomWorkload
	if err := nativekube.FromUnstructured(updatedObject, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.AssignmentRef == nil || updated.Status.AssignmentRef.UID != assignment.UID {
		t.Fatalf("existing assignment was not adopted: %#v", updated.Status.AssignmentRef)
	}
}
