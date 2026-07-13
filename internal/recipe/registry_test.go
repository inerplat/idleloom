package recipe

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const (
	nativeRecipeID = "train/mlx-linear-regression@v1"
	workerRecipeID = "train/container-linear-regression@v1"
	workerServeID  = "serve/llama-vulkan@v1"
)

func TestDefaultRegistryListsPinnedBalancedRecipes(t *testing.T) {
	registry := mustRegistry(t)
	definitions := registry.List()
	if len(definitions) != 5 {
		t.Fatalf("recipe count = %d, want 5", len(definitions))
	}
	wantIDs := []string{
		"infer/llama-vulkan@v1", "infer/mlx-batch@v1", workerServeID, workerRecipeID, nativeRecipeID,
	}
	for index, want := range wantIDs {
		if definitions[index].ID() != want {
			t.Fatalf("recipe[%d] = %q, want %q", index, definitions[index].ID(), want)
		}
	}
	backends := map[string]bool{}
	for _, definition := range definitions {
		backends[definition.Backend] = true
		if !strings.Contains(definition.ID(), "@v1") {
			t.Fatalf("recipe is not version-pinned: %s", definition.ID())
		}
	}
	if !backends["native"] || !backends["worker"] {
		t.Fatalf("registry backends = %v", backends)
	}
}

func TestDetailsRequirePinnedIdentityAndExposeDefaults(t *testing.T) {
	registry := mustRegistry(t)
	if _, err := registry.Details("train/mlx-linear-regression"); err == nil || !strings.Contains(err.Error(), "version-pinned") {
		t.Fatalf("unpinned recipe error = %v", err)
	}
	details, err := registry.Details(nativeRecipeID)
	if err != nil {
		t.Fatal(err)
	}
	if details.Backend != "native" || details.Runtime != "mlx" || len(details.Parameters) != 3 {
		t.Fatalf("details = %#v", details)
	}
	if !strings.HasPrefix(details.Digest, "sha256:") || details.Example["unifiedMemory"] != "512Mi" {
		t.Fatalf("recipe provenance or example is missing: %#v", details)
	}
	if details.Parameters[0].Name != "namespace" || details.Parameters[0].Default != "default" {
		t.Fatalf("first parameter = %#v", details.Parameters[0])
	}
}

func TestNativeRenderIsDeterministicAndValid(t *testing.T) {
	registry := mustRegistry(t)
	options := RenderOptions{Name: "native-train", Values: []byte("namespace: training\nunifiedMemory: 768Mi\n")}
	first, err := registry.Render(nativeRecipeID, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Render(nativeRecipeID, options)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Manifest, second.Manifest) || first.InputDigest != second.InputDigest {
		t.Fatal("identical inputs produced different manifests")
	}
	var workload nativev1alpha1.IdleloomWorkload
	if err := yaml.UnmarshalStrict(first.Manifest, &workload); err != nil {
		t.Fatal(err)
	}
	if err := nativev1alpha1.ValidateWorkload(&workload); err != nil {
		t.Fatal(err)
	}
	if workload.Namespace != "training" || workload.Spec.Shell == nil || workload.Spec.Shell.TimeoutSeconds != 120 {
		t.Fatalf("workload = %#v", workload)
	}
	assertMetadataContract(t, workload.Labels, workload.Annotations, "native-train", "train", "native", "mlx", nativeRecipeID, first.RecipeDigest, first.InputDigest)
}

func TestWorkerRenderProducesRealPinnedJob(t *testing.T) {
	registry := mustRegistry(t)
	result, err := registry.Render(workerRecipeID, RenderOptions{Name: "worker-train"})
	if err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := yaml.UnmarshalStrict(result.Manifest, &job); err != nil {
		t.Fatal(err)
	}
	if job.APIVersion != "batch/v1" || job.Kind != "Job" || len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("job = %#v", job)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if !strings.Contains(container.Image, "@sha256:") || job.Spec.Template.Spec.NodeSelector["idleloom-worker"] != "true" {
		t.Fatalf("image=%q nodeSelector=%v", container.Image, job.Spec.Template.Spec.NodeSelector)
	}
	if job.Spec.Template.Spec.SecurityContext == nil || job.Spec.Template.Spec.SecurityContext.RunAsUser == nil || *job.Spec.Template.Spec.SecurityContext.RunAsUser == 0 {
		t.Fatalf("worker Job does not select a non-root numeric identity: %#v", job.Spec.Template.Spec.SecurityContext)
	}
	assertMetadataContract(t, job.Labels, job.Annotations, "worker-train", "train", "worker", "python", workerRecipeID, result.RecipeDigest, result.InputDigest)
	assertMetadataContract(t, job.Spec.Template.Labels, job.Spec.Template.Annotations, "worker-train", "train", "worker", "python", workerRecipeID, result.RecipeDigest, result.InputDigest)
}

func TestWorkerServeRenderProducesDRADeploymentAndService(t *testing.T) {
	registry := mustRegistry(t)
	result, err := registry.Render(workerServeID, RenderOptions{
		Name: "worker-serve", Values: []byte("apiKeySecret: worker-serve-auth\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(result.Manifest), 4096)
	var claimTemplate resourcev1.ResourceClaimTemplate
	if err := decoder.Decode(&claimTemplate); err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := decoder.Decode(&deployment); err != nil {
		t.Fatal(err)
	}
	var service corev1.Service
	if err := decoder.Decode(&service); err != nil {
		t.Fatal(err)
	}
	if claimTemplate.APIVersion != "resource.k8s.io/v1" || len(claimTemplate.Spec.Spec.Devices.Requests) != 1 {
		t.Fatalf("claim template = %#v", claimTemplate)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 || deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("deployment strategy = %#v replicas=%v", deployment.Spec.Strategy, deployment.Spec.Replicas)
	}
	podSpec := deployment.Spec.Template.Spec
	if len(podSpec.ResourceClaims) != 1 || podSpec.ResourceClaims[0].ResourceClaimTemplateName == nil || *podSpec.ResourceClaims[0].ResourceClaimTemplateName != "worker-serve" {
		t.Fatalf("pod resource claims = %#v", podSpec.ResourceClaims)
	}
	if len(podSpec.Containers) != 1 || podSpec.Containers[0].Command[0] != "/app/llama-server" || !strings.Contains(strings.Join(podSpec.Containers[0].Args, " "), "--api-key-file") {
		t.Fatalf("server container = %#v", podSpec.Containers)
	}
	if service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.Selector["app.kubernetes.io/component"] != "inference-server" {
		t.Fatalf("service = %#v", service.Spec)
	}
	assertMetadataContract(t, claimTemplate.Labels, claimTemplate.Annotations, "worker-serve", "serve", "worker", "llama-vulkan", workerServeID, result.RecipeDigest, result.InputDigest)
	assertMetadataContract(t, claimTemplate.Spec.Labels, claimTemplate.Spec.Annotations, "worker-serve", "serve", "worker", "llama-vulkan", workerServeID, result.RecipeDigest, result.InputDigest)
	assertMetadataContract(t, deployment.Labels, deployment.Annotations, "worker-serve", "serve", "worker", "llama-vulkan", workerServeID, result.RecipeDigest, result.InputDigest)
	assertMetadataContract(t, deployment.Spec.Template.Labels, deployment.Spec.Template.Annotations, "worker-serve", "serve", "worker", "llama-vulkan", workerServeID, result.RecipeDigest, result.InputDigest)
	assertMetadataContract(t, service.Labels, service.Annotations, "worker-serve", "serve", "worker", "llama-vulkan", workerServeID, result.RecipeDigest, result.InputDigest)
}

func TestAllEmbeddedExamplesRender(t *testing.T) {
	registry := mustRegistry(t)
	for _, definition := range registry.List() {
		item := registry.entries[definition.ID()]
		name := strings.ReplaceAll(definition.Backend+"-example", "/", "-")
		if _, err := registry.Render(definition.ID(), RenderOptions{Name: name, Values: item.example}); err != nil {
			t.Fatalf("render example for %s: %v", definition.ID(), err)
		}
	}
}

func TestRendererSupportsManifestBundlesWithOneExecutionRoot(t *testing.T) {
	registry := mustRegistry(t)
	item := registry.entries[workerRecipeID]
	item.template = append(item.template, []byte(`---
apiVersion: v1
kind: ConfigMap
metadata:
  name: run-config
  namespace: {{ quote .Namespace }}
  labels:
    app.kubernetes.io/managed-by: idleloom
    ai.idleloom.io/run: {{ quote .Name }}
    ai.idleloom.io/task: train
    ai.idleloom.io/backend: worker
    ai.idleloom.io/runtime: python
  annotations:
    ai.idleloom.io/recipe: {{ quote .RecipeID }}
    ai.idleloom.io/recipe-digest: {{ quote .RecipeDigest }}
    ai.idleloom.io/input-digest: {{ quote .InputDigest }}
data:
  mode: smoke
`)...)
	registry.entries[workerRecipeID] = item
	result, err := registry.Render(workerRecipeID, RenderOptions{Name: "worker-train"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Manifest), "kind: ConfigMap") {
		t.Fatal("companion manifest was not rendered")
	}
}

func TestRenderRejectsUnknownAndInvalidParameters(t *testing.T) {
	registry := mustRegistry(t)
	tests := []struct {
		name   string
		id     string
		values string
		want   string
	}{
		{name: "unknown", id: nativeRecipeID, values: "surprise: true\n", want: "unknown parameter"},
		{name: "namespace", id: nativeRecipeID, values: "namespace: Not_Valid\n", want: "Kubernetes namespace"},
		{name: "quantity", id: nativeRecipeID, values: "unifiedMemory: \"0\"\n", want: "positive Kubernetes quantity"},
		{name: "integer", id: workerRecipeID, values: "epochs: 1.5\n", want: "must be an integer"},
		{name: "duplicate", id: workerRecipeID, values: "epochs: 10\nepochs: 20\n", want: "decode values YAML"},
		{name: "model-url", id: "infer/llama-vulkan@v1", values: "modelURL: http://models.example/model.gguf\n", want: "HTTPS URL"},
		{name: "model-digest", id: "infer/llama-vulkan@v1", values: "modelSHA256: abc\n", want: "64 lowercase"},
		{name: "prompt", id: "infer/mlx-batch@v1", values: "prompt: \"\"\n", want: "at least 1"},
		{name: "serve-secret", id: workerServeID, values: "apiKeySecret: Invalid_Name\n", want: "DNS subdomain"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := registry.Render(test.id, RenderOptions{Name: "test-run", Values: []byte(test.values)})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	if _, err := registry.Render(nativeRecipeID, RenderOptions{Name: "INVALID_NAME"}); err == nil || !strings.Contains(err.Error(), "DNS label") {
		t.Fatalf("invalid run name error = %v", err)
	}
}

func TestInputDigestChangesWithNormalizedInput(t *testing.T) {
	registry := mustRegistry(t)
	defaultResult, err := registry.Render(workerRecipeID, RenderOptions{Name: "worker-train"})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := registry.Render(workerRecipeID, RenderOptions{Name: "worker-train", Values: []byte("epochs: 121\n")})
	if err != nil {
		t.Fatal(err)
	}
	if defaultResult.InputDigest == changed.InputDigest {
		t.Fatal("different normalized inputs produced the same digest")
	}
	if !strings.HasPrefix(defaultResult.InputDigest, "sha256:") || len(defaultResult.InputDigest) != len("sha256:")+64 {
		t.Fatalf("input digest = %q", defaultResult.InputDigest)
	}
	if _, err := json.Marshal(defaultResult.Values); err != nil {
		t.Fatalf("normalized values are not JSON encodable: %v", err)
	}
}

func TestInputDigestChangesWithRecipeContent(t *testing.T) {
	values := map[string]any{"namespace": "default"}
	firstRecipeDigest := recipeContentDigest([]byte("definition"), []byte("schema"), []byte("template one"))
	secondRecipeDigest := recipeContentDigest([]byte("definition"), []byte("schema"), []byte("template two"))
	first, err := inputDigest(workerRecipeID, firstRecipeDigest, "worker-train", values)
	if err != nil {
		t.Fatal(err)
	}
	second, err := inputDigest(workerRecipeID, secondRecipeDigest, "worker-train", values)
	if err != nil {
		t.Fatal(err)
	}
	if firstRecipeDigest == secondRecipeDigest || first == second {
		t.Fatal("changed recipe content retained the same provenance digest")
	}
}

func mustRegistry(t *testing.T) *Registry {
	t.Helper()
	registry, err := DefaultRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func assertMetadataContract(t *testing.T, labels, annotations map[string]string, name, task, backend, runtime, recipeID, recipeDigest, inputDigest string) {
	t.Helper()
	wantLabels := map[string]string{
		managedByLabel: "idleloom", runLabel: name, taskLabel: task, backendLabel: backend, runtimeLabel: runtime,
	}
	for key, want := range wantLabels {
		if labels[key] != want {
			t.Fatalf("label %s = %q, want %q", key, labels[key], want)
		}
	}
	if annotations[recipeAnno] != recipeID || annotations[recipeDigestAnno] != recipeDigest || annotations[inputAnno] != inputDigest {
		t.Fatalf("annotations = %v", annotations)
	}
}
