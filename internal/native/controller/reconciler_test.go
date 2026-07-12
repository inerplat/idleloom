package controller

import (
	"context"
	"testing"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
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
	reconciler := &Reconciler{Dynamic: client, Coordination: kubernetesfake.NewSimpleClientset().CoordinationV1()}
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
	clientset := kubernetesfake.NewSimpleClientset()
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
