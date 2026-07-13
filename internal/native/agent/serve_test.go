package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/devruntime"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func TestServingHandlerRequiresAPIKeyAndReturnsChatCompletion(t *testing.T) {
	runner := &fakeBatchRunner{alive: true, result: devruntime.GenerateResponse{Text: "ready", ElapsedMillis: 7}}
	agent := &DevAgent{}
	handler := http.MaxBytesHandler(servingHandler(runner, "qwen3-5-0-8b", []byte("0123456789abcdef0123456789abcdef"), agent), 64<<10)
	body := `{"model":"qwen3-5-0-8b","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
	authorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer 0123456789abcdef0123456789abcdef")
	handler.ServeHTTP(authorized, request)
	if authorized.Code != http.StatusOK || !strings.Contains(authorized.Body.String(), `"object":"chat.completion"`) || !strings.Contains(authorized.Body.String(), `"content":"ready"`) {
		t.Fatalf("chat response = %d %s", authorized.Code, authorized.Body.String())
	}
	if runner.request.MaxTokens != 32 || !strings.Contains(runner.request.Prompt, "User: hello") {
		t.Fatalf("generation request = %#v", runner.request)
	}
	models := httptest.NewRecorder()
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRequest.Header.Set("X-Idleloom-API-Key", "0123456789abcdef0123456789abcdef")
	handler.ServeHTTP(models, modelsRequest)
	if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), "qwen3-5-0-8b") {
		t.Fatalf("models response = %d %s", models.Code, models.Body.String())
	}
}

func TestServeProcessBindsAndStopsWithUnderlyingModel(t *testing.T) {
	runner := &fakeBatchRunner{alive: true}
	actualAddress := ""
	listen := func(network, _ string) (net.Listener, error) {
		listener, err := net.Listen(network, "127.0.0.1:0")
		if err == nil {
			actualAddress = listener.Addr().String()
		}
		return listener, err
	}
	process, err := startServeProcessWithListener(
		runner, "192.0.2.10:18080", "qwen3-5-0-8b", []byte("0123456789abcdef0123456789abcdef"), &DevAgent{}, listen,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Get("http://" + actualAddress + "/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}
	if err := process.Stop(); err != nil {
		t.Fatal(err)
	}
	if process.Alive() || runner.stopCalls != 1 {
		t.Fatalf("stopped process alive=%v stopCalls=%d", process.Alive(), runner.stopCalls)
	}
}

func TestResolveServingKeyChecksAssignmentIdentity(t *testing.T) {
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "idleloom-host-studio"},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{UID: types.UID("workload-uid")},
			Model: &nativev1alpha1.ResolvedModel{Server: &nativev1alpha1.ResolvedServer{
				ServiceName: "qwen-chat", AuthSecretName: nativev1alpha1.ServingAuthSecretName,
			}},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000",
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: nativev1alpha1.ServingAuthSecretName, Namespace: assignment.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "idleloom-controller",
				"ai.idleloom.io/workload-uid":  "workload-uid",
				"ai.idleloom.io/execution-id":  assignment.Spec.ExecutionID,
				"ai.idleloom.io/service-name":  "qwen-chat",
			},
		},
		Data: map[string][]byte{"api-key": []byte("0123456789abcdef0123456789abcdef")},
	}
	agent := &DevAgent{config: DevAgentConfig{Kubernetes: kubernetesfake.NewSimpleClientset(secret)}}
	key, err := agent.resolveServingKey(context.Background(), assignment)
	if err != nil || string(key) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("resolveServingKey = %q, %v", key, err)
	}
	secret.Labels["ai.idleloom.io/execution-id"] = "different"
	agent.config.Kubernetes = kubernetesfake.NewSimpleClientset(secret)
	if _, err := agent.resolveServingKey(context.Background(), assignment); err == nil {
		t.Fatal("resolveServingKey accepted a Secret for another execution")
	}
}

func TestHostAdvertisesNativeServiceOnlyWhenWireKubeConnected(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	handshake := metav1.NewMicroTime(now.Add(-time.Second))
	host := &nativev1alpha1.IdleloomHost{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomHost"},
		ObjectMeta: metav1.ObjectMeta{Name: "host", Namespace: "idleloom-host-studio", Generation: 1},
		Spec:       nativev1alpha1.IdleloomHostSpec{AgentID: "studio.native"},
	}
	client := dynamicfake.NewSimpleDynamicClient(scheme, host)
	agent := &DevAgent{config: DevAgentConfig{
		Dynamic: client, AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()), Platform: fakeAgentPlatform{},
		ServeListenAddress: "192.0.2.10:18080", Now: func() time.Time { return now },
		PrepareRuntime: func(context.Context, func(string)) (devruntime.Receipt, error) { return devruntime.Receipt{}, nil },
		ConnectivityStatus: func() (nativev1alpha1.HostConnectivityStatus, error) {
			return nativev1alpha1.HostConnectivityStatus{
				Mode: nativev1alpha1.ConnectivityModeWireKubeLeaf, Address: "192.0.2.10/32",
				LastHandshakeTime: &handshake,
			}, nil
		},
	}}
	if err := agent.updateHostStatus(context.Background(), host, false, ""); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.HostsGVR).Namespace(host.Namespace).Get(context.Background(), host.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(updated.Status.Capabilities, nativev1alpha1.CapabilityNativeServiceV1) {
		t.Fatalf("connected capabilities = %v", updated.Status.Capabilities)
	}
	agent.config.ConnectivityStatus = nil
	if err := agent.updateHostStatus(context.Background(), &updated, false, ""); err != nil {
		t.Fatal(err)
	}
	object, err = client.Resource(nativekube.HostsGVR).Namespace(host.Namespace).Get(context.Background(), host.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(updated.Status.Capabilities, nativev1alpha1.CapabilityNativeServiceV1) {
		t.Fatalf("API-only capabilities = %v", updated.Status.Capabilities)
	}
}
