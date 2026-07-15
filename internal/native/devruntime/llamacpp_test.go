package devruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLlamaCppDiscoveryPinsStableGGUFFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "model.gguf")
	if err := os.WriteFile(path, append([]byte("GGUF"), []byte("pinned-model")...), 0o600); err != nil {
		t.Fatal(err)
	}
	discovery := &LlamaCppDiscovery{}
	runtime := LlamaCppRuntime{ModelsDirectory: directory}
	models, err := discovery.Discover(context.Background(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].Name != "model.gguf" || models[0].ManifestDigest == "" || models[0].Family != LlamaCppFamilyGGUF {
		t.Fatalf("models = %#v", models)
	}
	if err := VerifyLlamaCppModel(context.Background(), runtime, models[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("GGUF"), []byte("changed-model")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLlamaCppModel(context.Background(), runtime, models[0]); err == nil {
		t.Fatal("changed GGUF model matched the pinned identity")
	}
}

func TestLlamaCppDiscoveryRejectsSymlinksAndNonGGUFData(t *testing.T) {
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.gguf")
	if err := os.WriteFile(outside, []byte("GGUFoutside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "link.gguf")); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(directory, "inside.data")
	if err := os.WriteFile(inside, []byte("GGUFinside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inside.data", filepath.Join(directory, "internal-link.gguf")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "invalid.gguf"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := (&LlamaCppDiscovery{}).Discover(context.Background(), LlamaCppRuntime{ModelsDirectory: directory})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 {
		t.Fatalf("unsafe GGUF files were advertised: %#v", models)
	}
}

func TestLlamaCppDiscoveryAndVerificationRejectHardLinks(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source.data")
	if err := os.WriteFile(source, []byte("GGUFhardlink"), 0o600); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(directory, "model.gguf")
	if err := os.Link(source, modelPath); err != nil {
		t.Fatal(err)
	}
	runtime := LlamaCppRuntime{ModelsDirectory: directory}
	models, err := (&LlamaCppDiscovery{}).Discover(context.Background(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 0 {
		t.Fatalf("hard-linked GGUF was advertised: %#v", models)
	}
	expected := LlamaCppModel{
		Name: "model.gguf", ManifestDigest: "sha256:" + strings.Repeat("a", 64),
		Family: LlamaCppFamilyGGUF, Format: LlamaCppArtifactFormat, SizeBytes: 12,
	}
	if err := VerifyLlamaCppModel(context.Background(), runtime, expected); err == nil {
		t.Fatal("hard-linked GGUF passed verification")
	}
}

func TestLlamaCppMetalProbeRequiresBackendDeviceAndFullOffload(t *testing.T) {
	probe := &llamaCppMetalProbe{device: "MTL0"}
	_, _ = probe.Write([]byte("loaded MTL backend\nusing device MTL0 (Apple M4)\noffloaded 29/29 layers to GPU\n"))
	_, _ = probe.Write([]byte(strings.Repeat("later diagnostics\n", 2048)))
	if !probe.FullOffload() {
		t.Fatal("full Metal offload was not retained after bounded diagnostics rolled over")
	}
	partial := &llamaCppMetalProbe{device: "MTL0"}
	_, _ = partial.Write([]byte("loaded MTL backend\nusing device MTL0 (Apple M4)\noffloaded 20/29 layers to GPU\n"))
	if partial.FullOffload() {
		t.Fatal("partial GPU offload was accepted")
	}
}

func TestLlamaCppVersionAndDevicePatterns(t *testing.T) {
	version := llamaCppVersionPattern.FindStringSubmatch("version: 9960 (a935fbffe)")
	if len(version) != 3 || version[1] != "9960" {
		t.Fatalf("version match = %#v", version)
	}
	device := llamaCppDevicePattern.FindStringSubmatch("  MTL0: Apple M4 (18186 MiB, 18185 MiB free)")
	if len(device) != 2 || device[1] != "MTL0" {
		t.Fatalf("device match = %#v", device)
	}
	if ggufFileNamePattern.MatchString("../model.gguf") || ggufFileNamePattern.MatchString(strings.Repeat("a", 192)+".gguf") {
		t.Fatal("unsafe GGUF filename was accepted")
	}
	help := strings.Join(llamaCppRequiredFlags, " ")
	if missing := missingLlamaCppFlags(help); len(missing) != 0 {
		t.Fatalf("required options reported missing: %v", missing)
	}
	if missing := missingLlamaCppFlags(strings.Replace(help, "--fit", "", 1)); len(missing) != 1 || missing[0] != "--fit" {
		t.Fatalf("missing options = %v", missing)
	}
}

func TestLlamaCppGenerateUsesOpenAIChatAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		var payload struct {
			Messages  []map[string]string `json:"messages"`
			MaxTokens int                 `json:"max_tokens"`
			Stream    bool                `json:"stream"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Messages) != 1 || payload.Messages[0]["content"] != "hello" || payload.MaxTokens != 8 || payload.Stream {
			t.Fatalf("payload = %#v", payload)
		}
		_, _ = response.Write([]byte(`{"choices":[{"message":{"content":"ready"}}]}`))
	}))
	defer server.Close()
	process := &LlamaCppProcess{
		client: server.Client(), baseURL: server.URL,
		model: LlamaCppModel{Name: "model.gguf"},
	}
	result, err := process.Generate(context.Background(), GenerateRequest{Prompt: "hello", MaxTokens: 8})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "ready" {
		t.Fatalf("result = %#v", result)
	}
}

func TestLlamaCppGenerateRejectsErrorAndTrailingJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "error", body: `{"error":{"message":"failed"}}`},
		{name: "trailing", body: `{"choices":[{"message":{"content":"ready"}}]} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				_, _ = response.Write([]byte(test.body))
			}))
			defer server.Close()
			process := &LlamaCppProcess{client: server.Client(), baseURL: server.URL}
			if _, err := process.Generate(context.Background(), GenerateRequest{Prompt: "hello", MaxTokens: 8}); err == nil {
				t.Fatal("invalid llama.cpp response was accepted")
			}
		})
	}
}
