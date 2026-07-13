package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func TestEnsureServingSecretsCreatesMatchingScopedKeys(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	workload := servingWorkload()
	host := servingHost(time.Now().UTC())
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme, host)
	kubernetesClient := kubernetesfake.NewSimpleClientset()
	reconciler := &Reconciler{Dynamic: dynamicClient, Kubernetes: kubernetesClient}
	intent := &nativev1alpha1.WorkloadSchedulingIntent{
		HostRef:     nativev1alpha1.NamespacedObjectReference{Namespace: host.Namespace, Name: host.Name, UID: host.UID},
		ExecutionID: "123e4567-e89b-42d3-a456-426614174000",
	}
	if err := reconciler.ensureServingSecrets(context.Background(), workload, intent); err != nil {
		t.Fatal(err)
	}
	clientSecret, err := kubernetesClient.CoreV1().Secrets(workload.Namespace).Get(context.Background(), "qwen-chat-auth", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hostSecret, err := kubernetesClient.CoreV1().Secrets(host.Namespace).Get(context.Background(), nativev1alpha1.ServingAuthSecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clientSecret.Data["api-key"], hostSecret.Data["api-key"]) || len(clientSecret.Data["api-key"]) != 64 {
		t.Fatalf("serving Secret keys differ: client=%q host=%q", clientSecret.Data["api-key"], hostSecret.Data["api-key"])
	}
	if clientSecret.Immutable == nil || !*clientSecret.Immutable || hostSecret.Immutable == nil || !*hostSecret.Immutable {
		t.Fatal("serving Secrets are not immutable")
	}
	if err := reconciler.ensureServingSecrets(context.Background(), workload, intent); err != nil {
		t.Fatalf("idempotent ensureServingSecrets: %v", err)
	}
	if err := kubernetesClient.CoreV1().Secrets(workload.Namespace).Delete(context.Background(), clientSecret.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.ensureServingSecrets(context.Background(), workload, intent); err != nil {
		t.Fatalf("recover deleted client Secret: %v", err)
	}
	recovered, err := kubernetesClient.CoreV1().Secrets(workload.Namespace).Get(context.Background(), clientSecret.Name, metav1.GetOptions{})
	if err != nil || !reflect.DeepEqual(recovered.Data["api-key"], hostSecret.Data["api-key"]) {
		t.Fatalf("recovered client Secret = %#v, %v", recovered, err)
	}
}

func TestReconcileServingEndpointPublishesOnlyReadyConnectedAssignment(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	workload := servingWorkload()
	host := servingHost(now)
	heartbeat := metav1.NewMicroTime(now)
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: nativev1alpha1.AssignmentMailboxName, Namespace: host.Namespace,
			UID: types.UID("assignment-uid"), Generation: 1, CreationTimestamp: metav1.NewTime(now),
		},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{
				Namespace: workload.Namespace, Name: workload.Name, UID: workload.UID, Generation: workload.Generation,
			},
			HostRef: nativev1alpha1.ObjectReference{Name: host.Name, UID: host.UID},
			Model: &nativev1alpha1.ResolvedModel{Server: &nativev1alpha1.ResolvedServer{
				ServiceName: "qwen-chat", ModelAlias: "qwen3-5-0-8b",
				AuthSecretName: nativev1alpha1.ServingAuthSecretName, Port: nativev1alpha1.NativeServingPort,
			}},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 1, LeaseDurationSeconds: 30,
		},
		Status: nativev1alpha1.IdleloomWorkloadAssignmentStatus{
			ObservedGeneration: 1, Phase: nativev1alpha1.PhaseRunning, AgentID: "studio.native",
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 1,
			LastHeartbeatTime: &heartbeat,
		},
	}
	workload.Status.AssignmentRef = &nativev1alpha1.NamespacedObjectReference{
		Namespace: assignment.Namespace, Name: assignment.Name, UID: assignment.UID,
	}
	workload.Status.SchedulingIntent = &nativev1alpha1.WorkloadSchedulingIntent{
		HostRef:     nativev1alpha1.NamespacedObjectReference{Namespace: host.Namespace, Name: host.Name, UID: host.UID},
		ExecutionID: assignment.Spec.ExecutionID,
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "qwen-chat", Namespace: workload.Namespace, UID: types.UID("service-uid"),
			Annotations: map[string]string{servingWorkloadAnnotation: workload.Name},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.10",
			Ports: []corev1.ServicePort{{Name: "http", Protocol: corev1.ProtocolTCP, Port: 8000}},
		},
	}
	dynamicClient := dynamicfake.NewSimpleDynamicClient(scheme, assignment, host)
	kubernetesClient := kubernetesfake.NewSimpleClientset(service)
	reconciler := &Reconciler{Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return now }}
	if err := reconciler.reconcileServingEndpoint(context.Background(), workload); err != nil {
		t.Fatal(err)
	}
	slice, err := kubernetesClient.DiscoveryV1().EndpointSlices(workload.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if slice.AddressType != discoveryv1.AddressTypeIPv4 || len(slice.Endpoints) != 1 || slice.Endpoints[0].Addresses[0] != "192.0.2.10" || len(slice.Ports) != 1 || *slice.Ports[0].Port != nativev1alpha1.NativeServingPort {
		t.Fatalf("EndpointSlice = %#v", slice)
	}
	workload.Status.AssignmentRef = nil
	if err := reconciler.reconcileServingEndpoint(context.Background(), workload); err != nil {
		t.Fatal(err)
	}
	if _, err := kubernetesClient.DiscoveryV1().EndpointSlices(workload.Namespace).Get(context.Background(), service.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("EndpointSlice remained after assignment stopped being ready")
	}
}

func servingWorkload() *nativev1alpha1.IdleloomWorkload {
	return &nativev1alpha1.IdleloomWorkload{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "qwen", Namespace: "default", UID: types.UID("workload-uid"), Generation: 1,
		},
		Spec: nativev1alpha1.IdleloomWorkloadSpec{
			Mode:   nativev1alpha1.WorkloadModeServer,
			Server: &nativev1alpha1.WorkloadServer{ServiceName: "qwen-chat", ModelAlias: "qwen3-5-0-8b"},
		},
	}
}

func servingHost(now time.Time) *nativev1alpha1.IdleloomHost {
	handshake := metav1.NewMicroTime(now.Add(-time.Second))
	host := &nativev1alpha1.IdleloomHost{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomHost"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "host", Namespace: "idleloom-host-studio", UID: types.UID("host-uid"), Generation: 1,
		},
		Status: nativev1alpha1.IdleloomHostStatus{
			Connectivity: &nativev1alpha1.HostConnectivityStatus{
				Mode: nativev1alpha1.ConnectivityModeWireKubeLeaf, Address: "192.0.2.10/32", LastHandshakeTime: &handshake,
			},
		},
	}
	apiMeta.SetStatusCondition(&host.Status.Conditions, metav1.Condition{
		Type: nativev1alpha1.HostConditionConnected, Status: metav1.ConditionTrue,
		ObservedGeneration: 1, Reason: "Connected", LastTransitionTime: metav1.NewTime(now),
	})
	return host
}
