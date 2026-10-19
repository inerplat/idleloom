package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"sigs.k8s.io/yaml"
)

// The CRD rules below use only standard CEL, so they evaluate here exactly as
// the API server evaluates them; rules that need the Kubernetes CEL libraries
// are not selected.
func TestModelCRDRulesAcceptHuggingFaceAndKeepLockedSource(t *testing.T) {
	document := loadCRD(t, "ai.idleloom.io_idleloommodels.yaml")
	schema := versionSchema(t, document)
	profileRule := ruleWithMessage(t, schema, "runtime profile, model family, and artifact source must match")
	artifactSchema := schemaAt(t, schema, "properties", "spec", "properties", "artifact")
	sourceRule := ruleWithMessage(t, artifactSchema, "exactly one artifact source is required")

	accepted := map[string]IdleloomModel{
		"locked qwen":  validModel(),
		"hugging face": validHuggingFaceModel(),
	}
	for name, model := range accepted {
		t.Run("accept "+name, func(t *testing.T) {
			object := toCELObject(t, model)
			if !evaluateRule(t, profileRule, object) {
				t.Fatal("runtime profile rule rejected a valid model")
			}
			if !evaluateRule(t, sourceRule, object["spec"].(map[string]any)["artifact"]) {
				t.Fatal("artifact source rule rejected a valid model")
			}
		})
	}

	rejected := map[string]func(*IdleloomModel){
		"mlx family with OCI source":    func(m *IdleloomModel) { *m = validModel(); m.Spec.Family = ModelFamilyMLX },
		"qwen family with Hugging Face": func(m *IdleloomModel) { m.Spec.Family = ModelFamilyQwen35 },
		"gguf format":                   func(m *IdleloomModel) { m.Spec.Artifact.Format = ArtifactFormatGGUFV1 },
		"llama.cpp runtime":             func(m *IdleloomModel) { m.Spec.RuntimeProfile = RuntimeProfileLlamaCppMetalV1 },
	}
	for name, mutate := range rejected {
		t.Run("reject "+name, func(t *testing.T) {
			model := validHuggingFaceModel()
			mutate(&model)
			if evaluateRule(t, profileRule, toCELObject(t, model)) {
				t.Fatal("runtime profile rule accepted a mismatched model")
			}
		})
	}
	sourceRejected := map[string]func(*ModelArtifact){
		"signed Hugging Face": func(a *ModelArtifact) {
			a.Signature = &SignaturePolicy{Issuer: "https://issuer.example", Subject: "publisher"}
		},
		"Hugging Face and OCI": func(a *ModelArtifact) {
			a.OCIReference = "oci://registry.example/models/qwen@" + digest()
		},
		"Hugging Face and GGUF": func(a *ModelArtifact) { a.GGUFFile = "model.gguf" },
		"no source":             func(a *ModelArtifact) { a.HuggingFace = nil },
	}
	for name, mutate := range sourceRejected {
		t.Run("reject "+name, func(t *testing.T) {
			model := validHuggingFaceModel()
			mutate(&model.Spec.Artifact)
			artifact := toCELObject(t, model)["spec"].(map[string]any)["artifact"]
			if evaluateRule(t, sourceRule, artifact) {
				t.Fatal("artifact source rule accepted an invalid source set")
			}
		})
	}
}

func TestAssignmentCRDRuleAcceptsResolvedHuggingFaceModel(t *testing.T) {
	document := loadCRD(t, "ai.idleloom.io_idleloomworkloadassignments.yaml")
	modelSchema := schemaAt(t, versionSchema(t, document), "properties", "spec", "properties", "model")
	rule := ruleWithMessage(t, modelSchema, "resolved runtime profile, model family, and artifact source must match")
	model := validHuggingFaceModel()
	resolved := ResolvedModel{Family: model.Spec.Family, RuntimeProfile: model.Spec.RuntimeProfile, Artifact: model.Spec.Artifact}
	if !evaluateRule(t, rule, toCELObject(t, resolved)) {
		t.Fatal("resolved model rule rejected a Hugging Face MLX model")
	}
	resolved.Family = ModelFamilyQwen35
	if evaluateRule(t, rule, toCELObject(t, resolved)) {
		t.Fatal("resolved model rule accepted qwen3.5 with a Hugging Face source")
	}
}

func loadCRD(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "native", "crds", name))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func versionSchema(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	versions := schemaAt(t, document, "spec")["versions"].([]any)
	return schemaAt(t, versions[0].(map[string]any), "schema", "openAPIV3Schema")
}

func schemaAt(t *testing.T, node map[string]any, keys ...string) map[string]any {
	t.Helper()
	for _, key := range keys {
		next, ok := node[key].(map[string]any)
		if !ok {
			t.Fatalf("schema has no %q", key)
		}
		node = next
	}
	return node
}

func ruleWithMessage(t *testing.T, schema map[string]any, prefix string) string {
	t.Helper()
	validations, _ := schema["x-kubernetes-validations"].([]any)
	for _, item := range validations {
		validation := item.(map[string]any)
		if message, _ := validation["message"].(string); strings.HasPrefix(message, prefix) {
			return validation["rule"].(string)
		}
	}
	t.Fatalf("no CEL rule with message %q", prefix)
	return ""
}

func toCELObject(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func evaluateRule(t *testing.T, rule string, self any) bool {
	t.Helper()
	env, err := cel.NewEnv(cel.Variable("self", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	ast, issues := env.Compile(rule)
	if issues != nil && issues.Err() != nil {
		t.Fatalf("compile %q: %v", rule, issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := program.Eval(map[string]any{"self": self})
	if err != nil {
		t.Fatalf("evaluate %q: %v", rule, err)
	}
	accepted, ok := result.Value().(bool)
	if !ok {
		t.Fatalf("rule %q returned %T", rule, result.Value())
	}
	return accepted
}
