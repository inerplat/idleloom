package projection

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestReconcileCreatesIsolatedRunningProjection(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	workload, assignment := testProjectionObjects(now)
	dynamicClient := testDynamicClient(t, workload, assignment)
	kubernetesClient := kubernetesfake.NewClientset()
	controller := &Controller{
		Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return now },
		ProbeLogs: func(context.Context, string, string) error { return nil },
	}
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref := refFor(assignment)
	pod, err := kubernetesClient.CoreV1().Pods(ref.PodNamespace).Get(context.Background(), ref.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.NodeName != ref.NodeName || pod.Status.Phase != corev1.PodRunning || pod.Labels[LabelAssignmentUID] != string(assignment.UID) {
		t.Fatalf("managed Pod was not projected correctly: %#v", pod)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("managed Pod can mount a service account token")
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatal("managed Pod does not have exactly one container")
	}
	slot := pod.Spec.Containers[0].Resources.Requests["native.idleloom.io/execution-slot"]
	if slot.Value() != 1 {
		t.Fatal("managed Pod does not reserve exactly one execution slot")
	}
	node, err := kubernetesClient.CoreV1().Nodes().Get(context.Background(), ref.NodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !node.Spec.Unschedulable || len(node.Spec.Taints) != 2 || node.Status.Capacity.Pods().Value() != 1 {
		t.Fatalf("ephemeral Node is not isolated: %#v", node.Spec)
	}
	if status := nodeReadyStatus(node); status != corev1.ConditionTrue {
		t.Fatalf("Node Ready = %s, want True", status)
	}
	if len(node.Status.Addresses) != 0 || node.Annotations[AnnotationConnectivity] != "outbound-only" {
		t.Fatal("projection Node advertised unsupported control-plane connectivity")
	}
	if pod.Status.PodIP != "" || len(pod.Status.PodIPs) != 0 {
		t.Fatal("managed Pod advertised unsupported Pod networking")
	}
	assertPodCreatedBeforeNode(t, kubernetesClient.Actions())
	for _, action := range dynamicClient.Actions() {
		if action.GetResource() == nativekube.AssignmentsGVR && action.GetVerb() != "get" && action.GetVerb() != "list" && action.GetVerb() != "watch" {
			t.Fatalf("projection controller wrote Assignment state with %s", action.GetVerb())
		}
	}
}

func TestProjectionAnnotationsIncludeCommonRunIdentity(t *testing.T) {
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			Shell: &nativev1alpha1.ResolvedShell{},
			Run:   &nativev1alpha1.WorkloadRunSpec{Task: "shell", Experiment: "smoke", Attempt: 1},
		},
	}
	annotations := projectionAnnotations(assignment, false)
	if annotations["native.idleloom.io/run-task"] != "shell" || annotations["native.idleloom.io/experiment"] != "smoke" {
		t.Fatalf("run annotations = %#v", annotations)
	}
}

func TestProjectionUsesPinnedOllamaArtifactIdentity(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
		Model: &nativev1alpha1.ResolvedModel{Artifact: nativev1alpha1.ModelArtifact{
			OllamaModel: "qwen3.5:9b", ManifestDigest: digest,
		}},
	}}
	want := "ollama://local/qwen3.5:9b@" + digest
	if got := projectionAnnotations(assignment, false)[AnnotationModelArtifact]; got != want {
		t.Fatalf("model artifact annotation = %q, want %q", got, want)
	}
}

func TestProjectionUsesPinnedLlamaCppArtifactIdentity(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
		Model: &nativev1alpha1.ResolvedModel{Artifact: nativev1alpha1.ModelArtifact{
			GGUFFile: "llama-3.2-3b.gguf", ManifestDigest: digest,
		}},
	}}
	want := "gguf://managed/llama-3.2-3b.gguf@" + digest
	if got := projectionAnnotations(assignment, false)[AnnotationModelArtifact]; got != want {
		t.Fatalf("model artifact annotation = %q, want %q", got, want)
	}
}

func TestReconcilePublishesLogsEndpointOnlyForConnectedWireKubeHost(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	workload, assignment := testProjectionObjects(now)
	dynamicClient := testDynamicClient(t, workload, assignment)
	hostObject, err := dynamicClient.Resource(nativekube.HostsGVR).Namespace(assignment.Namespace).Get(context.Background(), "host", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var host nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(hostObject, &host); err != nil {
		t.Fatal(err)
	}
	host.Status.Connectivity = &nativev1alpha1.HostConnectivityStatus{
		Mode: nativev1alpha1.ConnectivityModeWireKubeLeaf, Provider: nativev1alpha1.ConnectivityProviderWireKube,
		Transport: nativev1alpha1.ConnectivityTransportRelay, Address: "198.18.18.52/32",
	}
	hostHeartbeat := metav1.NewMicroTime(now)
	host.Status.ObservedGeneration = host.Generation
	host.Status.LastHeartbeatTime = &hostHeartbeat
	host.Status.Conditions = []metav1.Condition{{
		Type: nativev1alpha1.HostConditionConnected, Status: metav1.ConditionTrue, ObservedGeneration: host.Generation,
	}}
	updatedHost, err := nativekube.ToUnstructured(&host)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dynamicClient.Resource(nativekube.HostsGVR).Namespace(assignment.Namespace).UpdateStatus(context.Background(), updatedHost, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	kubernetesClient := kubernetesfake.NewClientset()
	controller := &Controller{
		Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return now },
		ProbeLogs: func(context.Context, string, string) error { return nil },
	}
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref := refFor(assignment)
	node, err := kubernetesClient.CoreV1().Nodes().Get(context.Background(), ref.NodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(node.Status.Addresses) != 1 || node.Status.Addresses[0].Address != "198.18.18.52" || node.Status.DaemonEndpoints.KubeletEndpoint.Port != 10250 {
		t.Fatalf("kubelet endpoint = %#v %#v", node.Status.Addresses, node.Status.DaemonEndpoints)
	}
	pod, err := kubernetesClient.CoreV1().Pods(ref.PodNamespace).Get(context.Background(), ref.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Annotations[AnnotationLogs] != "supported" || pod.Annotations[AnnotationKubeletAPI] != "logs-only" {
		t.Fatalf("Pod capability annotations = %#v", pod.Annotations)
	}
	controller.ProbeLogs = func(context.Context, string, string) error { return fmt.Errorf("the API server log probe failed") }
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	node, err = kubernetesClient.CoreV1().Nodes().Get(context.Background(), ref.NodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod, err = kubernetesClient.CoreV1().Pods(ref.PodNamespace).Get(context.Background(), ref.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(node.Status.Addresses) != 0 || node.Status.DaemonEndpoints.KubeletEndpoint.Port != 0 || pod.Annotations[AnnotationLogs] != "unsupported" {
		t.Fatalf("failed log probe remained advertised: node=%#v pod=%#v", node.Status, pod.Annotations)
	}
}

func TestEnsureNodeRejectsMutationOutsideProjectionContract(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	_, assignment := testProjectionObjects(now)
	ref := refFor(assignment)
	node := managedNode(assignment, ref.NodeName, false)
	node.Labels[corev1.LabelOSStable] = "linux"
	kubernetesClient := kubernetesfake.NewClientset(node)
	controller := &Controller{Kubernetes: kubernetesClient}
	if _, err := controller.ensureNode(context.Background(), assignment, ref.NodeName, false); err == nil || !strings.Contains(err.Error(), "not the expected projection") {
		t.Fatalf("mutated Node error = %v", err)
	}
}

func TestReconcileProjectsInitiallyStaleAssignmentAsNotReady(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	workload, assignment := testProjectionObjects(now)
	heartbeat := metav1.NewMicroTime(now.Add(-2 * time.Minute))
	assignment.Status.LastHeartbeatTime = &heartbeat
	dynamicClient := testDynamicClient(t, workload, assignment)
	kubernetesClient := kubernetesfake.NewClientset()
	controller := &Controller{Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return now }}
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref := refFor(assignment)
	pod, err := kubernetesClient.CoreV1().Pods(ref.PodNamespace).Get(context.Background(), ref.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Status.Phase != corev1.PodPending || pod.Status.Reason != "IdleloomLeaseExpired" || podReadyStatus(pod) != corev1.ConditionFalse {
		t.Fatalf("stale Pod status = %s/%s", pod.Status.Phase, pod.Status.Reason)
	}
	node, err := kubernetesClient.CoreV1().Nodes().Get(context.Background(), ref.NodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if status := nodeReadyStatus(node); status != corev1.ConditionUnknown {
		t.Fatalf("stale Node Ready = %s, want Unknown", status)
	}
}

func TestStateForPreservesCompletedShellAfterHeartbeatExpires(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	_, assignment := testProjectionObjects(now)
	assignment.Spec.Model = nil
	assignment.Spec.Shell = &nativev1alpha1.ResolvedShell{
		Script: "echo ready", Isolation: nativev1alpha1.ShellIsolationSandbox,
		Network: nativev1alpha1.ShellNetworkNone, TimeoutSeconds: 30,
		UnifiedMemoryRequest: resource.MustParse("1Gi"),
	}
	assignment.Status.Phase = nativev1alpha1.PhaseSucceeded
	stale := metav1.NewMicroTime(now.Add(-10 * time.Minute))
	assignment.Status.LastHeartbeatTime = &stale
	state := stateFor(assignment, now)
	if state.podPhase != corev1.PodSucceeded || state.exitCode != 0 {
		t.Fatalf("completed shell state = %#v", state)
	}
}

func TestStateForTreatsServerFailureAsRestartable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	_, assignment := testProjectionObjects(now)
	assignment.Spec.Model.Server = &nativev1alpha1.ResolvedServer{ServiceName: "qwen-chat"}
	assignment.Status.Phase = nativev1alpha1.PhaseFailed
	state := stateFor(assignment, now)
	if state.podPhase != corev1.PodPending || state.ready || state.reason != "IdleloomServerRestarting" {
		t.Fatalf("restartable server state = %#v", state)
	}
}

func TestReconcileDoesNotRegressRunningPodPhaseDuringLeaseLoss(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	workload, assignment := testProjectionObjects(now)
	dynamicClient := testDynamicClient(t, workload, assignment)
	kubernetesClient := kubernetesfake.NewClientset()
	controller := &Controller{Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return now }}
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	heartbeat := metav1.NewMicroTime(now.Add(-2 * time.Minute))
	assignment.Status.LastHeartbeatTime = &heartbeat
	controller.Dynamic = testDynamicClient(t, workload, assignment)
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod, err := kubernetesClient.CoreV1().Pods(refFor(assignment).PodNamespace).Get(context.Background(), refFor(assignment).PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Status.Phase != corev1.PodRunning || podReadyStatus(pod) != corev1.ConditionFalse || pod.Status.Reason != "IdleloomLeaseExpired" {
		t.Fatalf("running Pod regressed during lease loss: phase=%s ready=%s reason=%s", pod.Status.Phase, podReadyStatus(pod), pod.Status.Reason)
	}
}

func TestReconcileDoesNotRegressRunningPodPhaseWhileStopping(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	workload, assignment := testProjectionObjects(now)
	dynamicClient := testDynamicClient(t, workload, assignment)
	kubernetesClient := kubernetesfake.NewClientset()
	controller := &Controller{Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return now }}
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assignment.Spec.DesiredState = nativev1alpha1.AssignmentDesiredStopped
	controller.Dynamic = testDynamicClient(t, workload, assignment)
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod, err := kubernetesClient.CoreV1().Pods(refFor(assignment).PodNamespace).Get(context.Background(), refFor(assignment).PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Status.Phase != corev1.PodRunning || podReadyStatus(pod) != corev1.ConditionFalse || pod.Status.Reason != "IdleloomStopping" {
		t.Fatalf("running Pod regressed while stopping: phase=%s ready=%s reason=%s", pod.Status.Phase, podReadyStatus(pod), pod.Status.Reason)
	}
}

func TestReconcileDoesNotRegressTerminalPodPhase(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	workload, assignment := testProjectionObjects(now)
	assignment.Status.Phase = nativev1alpha1.PhaseSucceeded
	dynamicClient := testDynamicClient(t, workload, assignment)
	kubernetesClient := kubernetesfake.NewClientset()
	controller := &Controller{Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return now }}
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	assignment.Status.Phase = nativev1alpha1.PhaseRunning
	controller.Dynamic = testDynamicClient(t, workload, assignment)
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod, err := kubernetesClient.CoreV1().Pods(refFor(assignment).PodNamespace).Get(context.Background(), refFor(assignment).PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Status.Phase != corev1.PodSucceeded || len(pod.Status.ContainerStatuses) != 1 || pod.Status.ContainerStatuses[0].State.Terminated == nil {
		t.Fatalf("terminal Pod regressed after stale assignment state: %#v", pod.Status)
	}
}

func TestReconcileGarbageCollectsProjectionAfterAssignmentDeletion(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	workload, assignment := testProjectionObjects(now)
	dynamicClient := testDynamicClient(t, workload, assignment)
	kubernetesClient := kubernetesfake.NewClientset()
	controller := &Controller{Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return now }}
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := dynamicClient.Resource(nativekube.AssignmentsGVR).Namespace(assignment.Namespace).Delete(context.Background(), assignment.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	kubernetesClient.ClearActions()
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref := refFor(assignment)
	if _, err := kubernetesClient.CoreV1().Nodes().Get(context.Background(), ref.NodeName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan Node still exists: %v", err)
	}
	if _, err := kubernetesClient.CoreV1().Pods(ref.PodNamespace).Get(context.Background(), ref.PodName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan Pod still exists: %v", err)
	}
	assertPodDeletedBeforeNode(t, kubernetesClient.Actions())
}

func TestReconcileRejectsMutatedManagedPod(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	workload, assignment := testProjectionObjects(now)
	ref := refFor(assignment)
	mutated := managedPod(workload, assignment, ref, false)
	privileged := true
	mutated.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &privileged}
	dynamicClient := testDynamicClient(t, workload, assignment)
	kubernetesClient := kubernetesfake.NewClientset(mutated)
	controller := &Controller{Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return now }}
	if err := controller.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("projection accepted a mutated managed Pod")
	}
	if _, err := kubernetesClient.CoreV1().Nodes().Get(context.Background(), ref.NodeName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Node was created after Pod contract failure: %v", err)
	}
}

func TestValidateManagedNodeAllowsNodeLifecycleTaints(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	_, assignment := testProjectionObjects(now)
	desired := managedNode(assignment, refFor(assignment).NodeName, false)
	actual := desired.DeepCopy()
	taintedAt := metav1.NewTime(now)
	actual.Spec.Taints = append(actual.Spec.Taints,
		corev1.Taint{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoSchedule},
		corev1.Taint{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoExecute, TimeAdded: &taintedAt},
	)
	if err := validateManagedNode(actual, desired, assignment, false); err != nil {
		t.Fatalf("node lifecycle taints were rejected: %v", err)
	}

	actual.Spec.Taints = append(actual.Spec.Taints,
		corev1.Taint{Key: "native.idleloom.io/foreign", Effect: corev1.TaintEffectNoSchedule},
	)
	if err := validateManagedNode(actual, desired, assignment, false); err == nil {
		t.Fatal("unrelated Node taint was accepted")
	}
}

func TestValidateManagedPodAllowsKnownAPIServerDefaults(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	workload, assignment := testProjectionObjects(now)
	desired := managedPod(workload, assignment, refFor(assignment), false)
	actual := desired.DeepCopy()
	actual.Spec.SecurityContext = &corev1.PodSecurityContext{}
	actual.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry-default"}}
	seconds := int64(300)
	actual.Spec.Tolerations = append(actual.Spec.Tolerations,
		corev1.Toleration{Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
		corev1.Toleration{Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds},
		corev1.Toleration{Key: "native.idleloom.io/execution-slot", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	)
	if err := validateManagedPod(actual, desired, assignment); err != nil {
		t.Fatalf("known API server defaults were rejected: %v", err)
	}
	actual.Spec.Tolerations = append(actual.Spec.Tolerations,
		corev1.Toleration{Key: "native.idleloom.io/foreign-resource", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	)
	if err := validateManagedPod(actual, desired, assignment); err == nil {
		t.Fatal("unrelated extended resource toleration was accepted")
	}
}

func TestTransientWorkloadReadFailurePreservesActiveProjection(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	workload, assignment := testProjectionObjects(now)
	dynamicClient := testDynamicClient(t, workload, assignment)
	kubernetesClient := kubernetesfake.NewClientset()
	controller := &Controller{Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return now }}
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	dynamicClient.PrependReactor("get", "idleloomworkloads", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("temporary API failure")
	})
	if err := controller.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("transient Workload read failure was not reported")
	}
	ref := refFor(assignment)
	if _, err := kubernetesClient.CoreV1().Nodes().Get(context.Background(), ref.NodeName, metav1.GetOptions{}); err != nil {
		t.Fatalf("active Node was garbage collected after Workload read failure: %v", err)
	}
	if _, err := kubernetesClient.CoreV1().Pods(ref.PodNamespace).Get(context.Background(), ref.PodName, metav1.GetOptions{}); err != nil {
		t.Fatalf("active Pod was garbage collected after Workload read failure: %v", err)
	}
}

func TestReconcileIsIdempotentAcrossControllerRestart(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	workload, assignment := testProjectionObjects(now)
	dynamicClient := testDynamicClient(t, workload, assignment)
	kubernetesClient := kubernetesfake.NewClientset()
	current := now
	controller := &Controller{Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return current }}
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref := refFor(assignment)
	first, err := kubernetesClient.CoreV1().Pods(ref.PodNamespace).Get(context.Background(), ref.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	transition := first.Status.Conditions[0].LastTransitionTime
	current = current.Add(time.Minute)
	restarted := &Controller{Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return current }}
	if err := restarted.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	pods, err := kubernetesClient.CoreV1().Pods(ref.PodNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: LabelProjection + "=" + ProjectionValue})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := kubernetesClient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{LabelSelector: LabelProjection + "=" + ProjectionValue})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 || len(nodes.Items) != 1 {
		t.Fatalf("projection counts after restart = pods %d, nodes %d", len(pods.Items), len(nodes.Items))
	}
	if !pods.Items[0].Status.Conditions[0].LastTransitionTime.Equal(&transition) {
		t.Fatal("unchanged Pod condition transition time was reset")
	}
}

func TestReconcileDeletesProjectionOnlyAfterExactStopAcknowledgement(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	workload, assignment := testProjectionObjects(now)
	dynamicClient := testDynamicClient(t, workload, assignment)
	kubernetesClient := kubernetesfake.NewClientset()
	controller := &Controller{Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return now }}
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assignment.Spec.DesiredState = nativev1alpha1.AssignmentDesiredStopped
	assignment.Status.ObservedGeneration = assignment.Generation
	assignment.Status.Phase = nativev1alpha1.PhaseStopped
	assignment.Status.AgentID = "agent.native"
	assignment.Status.StopAcknowledgement = &nativev1alpha1.StopAcknowledgement{
		AssignmentUID: assignment.UID, ObservedGeneration: assignment.Generation,
		ExecutionID: assignment.Spec.ExecutionID, FencingEpoch: assignment.Spec.FencingEpoch,
		StoppedAt: *assignment.Status.LastHeartbeatTime,
	}
	controller.Dynamic = testDynamicClient(t, workload, assignment)
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ref := refFor(assignment)
	if _, err := kubernetesClient.CoreV1().Nodes().Get(context.Background(), ref.NodeName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Node remained after exact stop acknowledgement: %v", err)
	}
	if _, err := kubernetesClient.CoreV1().Pods(ref.PodNamespace).Get(context.Background(), ref.PodName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Pod remained after exact stop acknowledgement: %v", err)
	}
}

func TestReconcileRetriesNodeStatusConflict(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	workload, assignment := testProjectionObjects(now)
	dynamicClient := testDynamicClient(t, workload, assignment)
	kubernetesClient := kubernetesfake.NewClientset()
	conflicted := false
	kubernetesClient.PrependReactor("update", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "status" || conflicted {
			return false, nil, nil
		}
		conflicted = true
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, refFor(assignment).NodeName, nil)
	})
	controller := &Controller{Dynamic: dynamicClient, Kubernetes: kubernetesClient, Now: func() time.Time { return now }}
	if err := controller.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !conflicted {
		t.Fatal("test did not inject a Node status conflict")
	}
	node, err := kubernetesClient.CoreV1().Nodes().Get(context.Background(), refFor(assignment).NodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if nodeReadyStatus(node) != corev1.ConditionTrue {
		t.Fatal("Node status was not written after conflict retry")
	}
}

func testProjectionObjects(now time.Time) (*nativev1alpha1.IdleloomWorkload, *nativev1alpha1.IdleloomWorkloadAssignment) {
	workload := &nativev1alpha1.IdleloomWorkload{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "qwen", Namespace: "tenant", UID: types.UID("11111111-1111-4111-8111-111111111111"), Generation: 1,
		},
	}
	heartbeat := metav1.NewMicroTime(now)
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: nativev1alpha1.AssignmentMailboxName, Namespace: "idleloom-host-example",
			UID: types.UID("22222222-2222-4222-8222-222222222222"), Generation: 1, CreationTimestamp: metav1.NewTime(now.Add(-time.Second)),
		},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			HostRef: nativev1alpha1.ObjectReference{
				Name: "host", UID: types.UID("44444444-4444-4444-8444-444444444444"),
			},
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{
				Namespace: workload.Namespace, Name: workload.Name, UID: workload.UID, Generation: workload.Generation,
			},
			Model: &nativev1alpha1.ResolvedModel{Artifact: nativev1alpha1.ModelArtifact{
				OCIReference: "oci://development.invalid/model@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}},
			ExecutionID: "33333333-3333-4333-8333-333333333333", FencingEpoch: 1, LeaseDurationSeconds: 30,
		},
		Status: nativev1alpha1.IdleloomWorkloadAssignmentStatus{
			Phase: nativev1alpha1.PhaseRunning, ExecutionID: "33333333-3333-4333-8333-333333333333",
			FencingEpoch: 1, LastHeartbeatTime: &heartbeat,
		},
	}
	return workload, assignment
}

func testDynamicClient(t *testing.T, objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	listKinds := map[schema.GroupVersionResource]string{
		nativekube.WorkloadsGVR: "IdleloomWorkloadList", nativekube.AssignmentsGVR: "IdleloomWorkloadAssignmentList",
		nativekube.HostsGVR: "IdleloomHostList",
	}
	for _, object := range objects {
		assignment, ok := object.(*nativev1alpha1.IdleloomWorkloadAssignment)
		if !ok {
			continue
		}
		objects = append(objects, &nativev1alpha1.IdleloomHost{
			TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomHost"},
			ObjectMeta: metav1.ObjectMeta{
				Name: assignment.Spec.HostRef.Name, Namespace: assignment.Namespace, UID: assignment.Spec.HostRef.UID,
			},
		})
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objects...)
}

func nodeReadyStatus(node *corev1.Node) corev1.ConditionStatus {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status
		}
	}
	return ""
}

func podReadyStatus(pod *corev1.Pod) corev1.ConditionStatus {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status
		}
	}
	return ""
}

func assertPodCreatedBeforeNode(t *testing.T, actions []clienttesting.Action) {
	t.Helper()
	podIndex := -1
	nodeIndex := -1
	for index, action := range actions {
		if action.GetVerb() != "create" {
			continue
		}
		switch action.GetResource().Resource {
		case "pods":
			podIndex = index
		case "nodes":
			nodeIndex = index
		}
	}
	if podIndex < 0 || nodeIndex < 0 || podIndex > nodeIndex {
		t.Fatalf("create order pod/node = %d/%d", podIndex, nodeIndex)
	}
}

func assertPodDeletedBeforeNode(t *testing.T, actions []clienttesting.Action) {
	t.Helper()
	podIndex := -1
	nodeIndex := -1
	for index, action := range actions {
		if action.GetVerb() != "delete" {
			continue
		}
		switch action.GetResource().Resource {
		case "pods":
			podIndex = index
		case "nodes":
			nodeIndex = index
		}
	}
	if podIndex < 0 || nodeIndex < 0 || podIndex > nodeIndex {
		t.Fatalf("delete order pod/node = %d/%d", podIndex, nodeIndex)
	}
}
