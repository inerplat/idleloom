package recipe

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	nativeServeID  = "serve/mlx-qwen@v1"
	ollamaInferID  = "infer/ollama-gguf@v1"
	ollamaServeID  = "serve/ollama-gguf@v1"
	llamaInferID   = "infer/llama-cpp-metal@v1"
	llamaServeID   = "serve/llama-cpp-metal@v1"
	workerRecipeID = "train/container-linear-regression@v1"
	workerInferID  = "infer/llama-vulkan@v1"
	workerServeID  = "serve/llama-vulkan@v1"
)

func TestDefaultRegistryListsPinnedBalancedRecipes(t *testing.T) {
	registry := mustRegistry(t)
	definitions := registry.List()
	if len(definitions) != 10 {
		t.Fatalf("recipe count = %d, want 10", len(definitions))
	}
	wantIDs := []string{
		llamaInferID, "infer/llama-vulkan@v1", "infer/mlx-batch@v1", ollamaInferID,
		llamaServeID, workerServeID, nativeServeID, ollamaServeID, workerRecipeID, nativeRecipeID,
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
	if details.Backend != "native" || details.Runtime != "mlx" || len(details.Parameters) != 7 {
		t.Fatalf("details = %#v", details)
	}
	if !strings.HasPrefix(details.Digest, "sha256:") || details.Example["unifiedMemory"] != "512Mi" {
		t.Fatalf("recipe provenance or example is missing: %#v", details)
	}
	parameterDefaults := make(map[string]any, len(details.Parameters))
	for _, parameter := range details.Parameters {
		parameterDefaults[parameter.Name] = parameter.Default
	}
	if parameterDefaults["namespace"] != "default" || fmt.Sprint(parameterDefaults["attempt"]) != "1" || fmt.Sprint(parameterDefaults["learningRate"]) != "0.08" {
		t.Fatalf("parameter defaults = %#v", parameterDefaults)
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
	if workload.Namespace != "training" || workload.Spec.Train == nil || workload.Spec.Train.TimeoutSeconds != 120 || workload.Spec.Run == nil || workload.Spec.Run.Experiment != "linear-regression" {
		t.Fatalf("workload = %#v", workload)
	}
	assertMetadataContract(t, workload.Labels, workload.Annotations, "native-train", "train", "native", "mlx", nativeRecipeID, first.RecipeDigest, first.InputDigest)
}

func TestNativeTrainingExperimentRequiresDNSLabel(t *testing.T) {
	registry := mustRegistry(t)
	values := []byte("experiment: team.training\n")
	if _, err := registry.Render(nativeRecipeID, RenderOptions{Name: "native-train", Values: values}); err == nil || !strings.Contains(err.Error(), "DNS label") {
		t.Fatalf("invalid experiment error = %v", err)
	}
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
	assertDedicatedToleration(t, job.Spec.Template.Spec)
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
	assertDedicatedToleration(t, podSpec)
	if len(podSpec.ResourceClaims) != 1 || podSpec.ResourceClaims[0].ResourceClaimTemplateName == nil || *podSpec.ResourceClaims[0].ResourceClaimTemplateName != "worker-serve" {
		t.Fatalf("pod resource claims = %#v", podSpec.ResourceClaims)
	}
	serverCommand := strings.Join(append(append([]string(nil), podSpec.Containers[0].Command...), podSpec.Containers[0].Args...), " ")
	if len(podSpec.Containers) != 1 || !strings.Contains(serverCommand, "/usr/bin/llama-server") || !strings.Contains(serverCommand, "Virtio-GPU Venus") || !strings.Contains(serverCommand, "--api-key-file") {
		t.Fatalf("server container = %#v", podSpec.Containers)
	}
	if !strings.HasPrefix(podSpec.Containers[0].Image, "quay.io/ramalama/ramalama@sha256:") {
		t.Fatalf("server image = %q", podSpec.Containers[0].Image)
	}
	if value := environmentValue(podSpec.Containers[0], "VK_ICD_FILENAMES"); value != "/usr/share/vulkan/icd.d/virtio_icd.aarch64.json" {
		t.Fatalf("server Vulkan ICD = %q", value)
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

func TestWorkerInferRenderToleratesDedicatedWorkers(t *testing.T) {
	registry := mustRegistry(t)
	result, err := registry.Render(workerInferID, RenderOptions{Name: "worker-infer"})
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(result.Manifest), 4096)
	var claim resourcev1.ResourceClaim
	if err := decoder.Decode(&claim); err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := decoder.Decode(&job); err != nil {
		t.Fatal(err)
	}
	if claim.APIVersion != "resource.k8s.io/v1" || job.APIVersion != "batch/v1" {
		t.Fatalf("unexpected worker inference bundle: claim=%#v job=%#v", claim.TypeMeta, job.TypeMeta)
	}
	assertDedicatedToleration(t, job.Spec.Template.Spec)
	inference := job.Spec.Template.Spec.Containers[0]
	inferenceCommand := strings.Join(append(append([]string(nil), inference.Command...), inference.Args...), " ")
	if !strings.Contains(inferenceCommand, "/usr/bin/llama-cli") || !strings.Contains(inferenceCommand, "Virtio-GPU Venus") || !strings.Contains(inferenceCommand, "--single-turn") || !strings.HasPrefix(inference.Image, "quay.io/ramalama/ramalama@sha256:") {
		t.Fatalf("inference runtime = %#v", job.Spec.Template.Spec.Containers[0])
	}
	if value := environmentValue(job.Spec.Template.Spec.Containers[0], "VK_ICD_FILENAMES"); value != "/usr/share/vulkan/icd.d/virtio_icd.aarch64.json" {
		t.Fatalf("inference Vulkan ICD = %q", value)
	}
}

func TestNativeServeRenderProducesWorkloadAndSelectorlessService(t *testing.T) {
	registry := mustRegistry(t)
	result, err := registry.Render(nativeServeID, RenderOptions{Name: "native-serve"})
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(result.Manifest), 4096)
	var workload nativev1alpha1.IdleloomWorkload
	if err := decoder.Decode(&workload); err != nil {
		t.Fatal(err)
	}
	var service corev1.Service
	if err := decoder.Decode(&service); err != nil {
		t.Fatal(err)
	}
	if workload.Spec.Mode != nativev1alpha1.WorkloadModeServer || workload.Spec.Server == nil || workload.Spec.Server.ServiceName != "native-serve" || workload.Spec.Server.ModelAlias != "qwen3-5-0-8b" {
		t.Fatalf("Native serving workload = %#v", workload.Spec)
	}
	if service.Spec.Type != corev1.ServiceTypeClusterIP || len(service.Spec.Selector) != 0 || service.Annotations["ai.idleloom.io/native-workload"] != workload.Name || service.Annotations["ai.idleloom.io/auth-secret"] != "native-serve-auth" {
		t.Fatalf("Native serving Service = %#v", service)
	}
	assertMetadataContract(t, workload.Labels, workload.Annotations, "native-serve", "serve", "native", "mlx", nativeServeID, result.RecipeDigest, result.InputDigest)
	assertMetadataContract(t, service.Labels, service.Annotations, "native-serve", "serve", "native", "mlx", nativeServeID, result.RecipeDigest, result.InputDigest)
}

func TestLlamaCppRecipesRenderRestrictedNativeWorkloads(t *testing.T) {
	registry := mustRegistry(t)
	infer, err := registry.Render(llamaInferID, RenderOptions{
		Name: "llama-infer", Values: []byte("model: llama-3-2-3b\nmaxTokens: 32\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var batch nativev1alpha1.IdleloomWorkload
	if err := yaml.UnmarshalStrict(infer.Manifest, &batch); err != nil {
		t.Fatal(err)
	}
	if err := nativev1alpha1.ValidateWorkload(&batch); err != nil {
		t.Fatal(err)
	}
	if batch.Spec.Mode != nativev1alpha1.WorkloadModeBatch || batch.Spec.Model == nil || batch.Spec.Model.CatalogRef != "llama-3-2-3b" || batch.Spec.Batch == nil || batch.Spec.Batch.MaxTokens != 32 {
		t.Fatalf("llama.cpp batch workload = %#v", batch.Spec)
	}
	assertMetadataContract(t, batch.Labels, batch.Annotations, "llama-infer", "infer", "native", "llama-cpp-metal", llamaInferID, infer.RecipeDigest, infer.InputDigest)

	serve, err := registry.Render(llamaServeID, RenderOptions{
		Name: "llama-serve", Values: []byte("model: llama-3-2-3b\nmodelAlias: llama-3-2-3b\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(serve.Manifest), 4096)
	var server nativev1alpha1.IdleloomWorkload
	if err := decoder.Decode(&server); err != nil {
		t.Fatal(err)
	}
	var service corev1.Service
	if err := decoder.Decode(&service); err != nil {
		t.Fatal(err)
	}
	if err := nativev1alpha1.ValidateWorkload(&server); err != nil {
		t.Fatal(err)
	}
	if server.Spec.Mode != nativev1alpha1.WorkloadModeServer || server.Spec.Server == nil || server.Spec.Server.ServiceName != "llama-serve" || len(service.Spec.Selector) != 0 {
		t.Fatalf("llama.cpp serving bundle workload=%#v service=%#v", server.Spec, service.Spec)
	}
	assertMetadataContract(t, server.Labels, server.Annotations, "llama-serve", "serve", "native", "llama-cpp-metal", llamaServeID, serve.RecipeDigest, serve.InputDigest)
	assertMetadataContract(t, service.Labels, service.Annotations, "llama-serve", "serve", "native", "llama-cpp-metal", llamaServeID, serve.RecipeDigest, serve.InputDigest)
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
		{name: "model-url-query", id: "infer/llama-vulkan@v1", values: "modelURL: https://models.example/model.gguf?token=secret\n", want: "HTTPS URL"},
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

func assertDedicatedToleration(t *testing.T, spec corev1.PodSpec) {
	t.Helper()
	for _, toleration := range spec.Tolerations {
		if toleration.Key == "idleloom-dedicated" && toleration.Operator == corev1.TolerationOpExists && toleration.Effect == corev1.TaintEffectNoSchedule {
			return
		}
	}
	t.Fatalf("pod spec does not tolerate dedicated Idleloom workers: %#v", spec.Tolerations)
}

func environmentValue(container corev1.Container, name string) string {
	for _, variable := range container.Env {
		if variable.Name == name {
			return variable.Value
		}
	}
	return ""
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
