package projection

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func TestLiveProjectionSpecMatchesAPIServerDefaults(t *testing.T) {
	kubeconfig := os.Getenv("IDLELOOM_PROJECTION_E2E_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set IDLELOOM_PROJECTION_E2E_KUBECONFIG to inspect a live alpha projection")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	kubernetesClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	assignments, err := dynamicClient.Resource(nativekube.AssignmentsGVR).Namespace(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{Limit: 1})
	if err != nil || len(assignments.Items) != 1 {
		t.Fatalf("list assignment: count=%d err=%v", len(assignments.Items), err)
	}
	var assignment nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(&assignments.Items[0], &assignment); err != nil {
		t.Fatal(err)
	}
	workloadObject, err := dynamicClient.Resource(nativekube.WorkloadsGVR).Namespace(assignment.Spec.WorkloadRef.Namespace).Get(context.Background(), assignment.Spec.WorkloadRef.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var workload nativev1alpha1.IdleloomWorkload
	if err := nativekube.FromUnstructured(workloadObject, &workload); err != nil {
		t.Fatal(err)
	}
	ref := refFor(&assignment)
	pod, err := kubernetesClient.CoreV1().Pods(ref.PodNamespace).Get(context.Background(), ref.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	desired := managedPod(&workload, &assignment, ref, false)
	actualSpec := normalizedProjectionSpec(pod.Spec)
	desiredSpec := normalizedProjectionSpec(desired.Spec)
	if !apiequality.Semantic.DeepEqual(actualSpec, desiredSpec) {
		actualData, _ := json.MarshalIndent(actualSpec, "", "  ")
		desiredData, _ := json.MarshalIndent(desiredSpec, "", "  ")
		t.Fatalf("live Pod spec differs from projection contract\nactual=%s\ndesired=%s", actualData, desiredData)
	}
}
