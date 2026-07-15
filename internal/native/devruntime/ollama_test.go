package devruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscoverAndVerifyOllamaModel(t *testing.T) {
	runtime, expected, modelBlob := writeOllamaStore(t)
	models, err := DiscoverOllamaModels(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != expected {
		t.Fatalf("models = %#v, want %#v", models, expected)
	}
	if err := VerifyOllamaModel(context.Background(), runtime, expected); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modelBlob, []byte("broken-model"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyOllamaModel(context.Background(), runtime, expected); err == nil || !strings.Contains(err.Error(), "content digest") {
		t.Fatalf("tampered model verification error = %v", err)
	}
}

func TestOllamaMetalPlacementRequiresCompleteOffload(t *testing.T) {
	if !fullOllamaMetalPlacement(8_515_484_608, 8_515_484_608) {
		t.Fatal("complete Ollama Metal placement was rejected")
	}
	if fullOllamaMetalPlacement(8_515_484_608, 4_000_000_000) || fullOllamaMetalPlacement(0, 0) {
		t.Fatal("partial or empty Ollama Metal placement was accepted")
	}
}

func TestDiscoverOllamaModelRejectsTamperedConfigBlob(t *testing.T) {
	runtime, _, _ := writeOllamaStore(t)
	manifestPath := filepath.Join(runtime.ModelsDirectory, "manifests", "registry.ollama.ai", "library", "qwen3.5", "9b")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest ollamaManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(runtime.ModelsDirectory, "blobs", strings.Replace(manifest.Config.Digest, ":", "-", 1))
	if err := os.WriteFile(configPath, []byte(strings.Repeat("x", int(manifest.Config.Size))), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverOllamaModels(runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectOllamaModel(runtime, "qwen3.5:9b"); err == nil || !strings.Contains(err.Error(), "config blob") {
		t.Fatalf("tampered config error = %v", err)
	}
}

func TestVerifyOllamaModelRejectsStoreSymlinkEscape(t *testing.T) {
	runtime, expected, _ := writeOllamaStore(t)
	manifestRoot := filepath.Join(runtime.ModelsDirectory, "manifests")
	outside := filepath.Join(t.TempDir(), "manifests")
	if err := os.Rename(manifestRoot, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, manifestRoot); err != nil {
		t.Fatal(err)
	}
	if err := VerifyOllamaModel(context.Background(), runtime, expected); err == nil {
		t.Fatal("Ollama model store symlink escape was accepted")
	}
}

func TestOllamaGenerateUsesPinnedLocalModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/generate" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		var payload struct {
			Model   string `json:"model"`
			Prompt  string `json:"prompt"`
			Stream  bool   `json:"stream"`
			Think   bool   `json:"think"`
			Options struct {
				NumPredict int `json:"num_predict"`
				NumContext int `json:"num_ctx"`
			} `json:"options"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "qwen3.5:9b" || payload.Prompt != "hello" || payload.Stream || payload.Think || payload.Options.NumPredict != 8 || payload.Options.NumContext != 2048 {
			t.Fatalf("payload = %#v", payload)
		}
		_, _ = response.Write([]byte(`{"response":"ready","done":true}`))
	}))
	defer server.Close()
	process := &OllamaProcess{
		client: server.Client(), baseURL: server.URL,
		model: OllamaModel{Name: "qwen3.5:9b"}, context: 2048, done: make(chan struct{}),
	}
	result, err := process.Generate(context.Background(), GenerateRequest{Prompt: "hello", MaxTokens: 8})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "ready" {
		t.Fatalf("result = %#v", result)
	}
}

func TestOllamaWaitReadyPollsUntilPinnedModelAppears(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/tags" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		if requests.Add(1) == 1 {
			_, _ = response.Write([]byte(`{"models":[]}`))
			return
		}
		_, _ = response.Write([]byte(`{"models":[{"name":"qwen3.5:9b","digest":"sha256:abc123"}]}`))
	}))
	defer server.Close()
	process := &OllamaProcess{
		client: server.Client(), baseURL: server.URL, done: make(chan struct{}),
		model: OllamaModel{Name: "qwen3.5:9b", ManifestDigest: "sha256:abc123"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := process.waitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if requests.Load() < 2 {
		t.Fatalf("readiness requests = %d, want at least 2", requests.Load())
	}
}

func TestOllamaGenerateRedactsPrivatePathsFromAPIErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "cannot open /private/models/blob", http.StatusInternalServerError)
	}))
	defer server.Close()
	process := &OllamaProcess{
		client: server.Client(), baseURL: server.URL, modelsDir: "/private/models",
		model: OllamaModel{Name: "qwen3.5:9b"}, context: 2048, done: make(chan struct{}),
	}
	_, err := process.Generate(context.Background(), GenerateRequest{Prompt: "hello", MaxTokens: 8})
	if err == nil || strings.Contains(err.Error(), "/private/models") || !strings.Contains(err.Error(), "<private-path>") {
		t.Fatalf("generation error = %v", err)
	}
}

func TestOllamaGenerateRejectsTrailingAPIData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(`{"response":"ready","done":true} {}`))
	}))
	defer server.Close()
	process := &OllamaProcess{
		client: server.Client(), baseURL: server.URL,
		model: OllamaModel{Name: "qwen3.5:9b"}, context: 2048, done: make(chan struct{}),
	}
	if _, err := process.Generate(context.Background(), GenerateRequest{Prompt: "hello", MaxTokens: 8}); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("trailing response error = %v", err)
	}
}

func TestOllamaVersionComparison(t *testing.T) {
	for _, test := range []struct {
		parts []string
		want  bool
	}{
		{[]string{"0", "17", "1"}, true},
		{[]string{"0", "21", "2"}, true},
		{[]string{"1", "0", "0"}, true},
		{[]string{"0", "17", "0"}, false},
		{[]string{"invalid", "17", "1"}, false},
	} {
		if got := ollamaVersionAtLeast(test.parts, 0, 17, 1); got != test.want {
			t.Fatalf("ollamaVersionAtLeast(%v) = %t, want %t", test.parts, got, test.want)
		}
	}
}

func TestOllamaDiagnosticsRedactsPathsAndNoise(t *testing.T) {
	buffer := &boundedBuffer{limit: maxStderrBytes}
	_, _ = buffer.Write([]byte("Your new public key is:\nssh-ed25519 public\n/private/models/blob\n[GIN] request GET      \"/api/ps\"\nuseful failure\n"))
	process := &OllamaProcess{stderr: buffer, modelsDir: "/private/models"}
	got := process.diagnostics()
	if strings.Contains(got, "/private/models") || strings.Contains(got, "ssh-ed25519") || strings.Contains(got, "/api/ps") || !strings.Contains(got, "useful failure") {
		t.Fatalf("diagnostics = %q", got)
	}
}

func writeOllamaStore(t *testing.T) (OllamaRuntime, OllamaModel, string) {
	t.Helper()
	root := t.TempDir()
	blobs := filepath.Join(root, "blobs")
	manifestDirectory := filepath.Join(root, "manifests", "registry.ollama.ai", "library", "qwen3.5")
	for _, directory := range []string{blobs, manifestDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configData := []byte(`{"model_format":"gguf","model_family":"qwen35","model_families":["qwen35"]}`)
	modelData := []byte("pinned-model")
	configDescriptor, configPath := writeOllamaBlob(t, blobs, "application/vnd.docker.container.image.v1+json", configData)
	modelDescriptor, modelPath := writeOllamaBlob(t, blobs, "application/vnd.ollama.image.model", modelData)
	manifest := ollamaManifest{
		SchemaVersion: 2, MediaType: "application/vnd.docker.distribution.manifest.v2+json",
		Config: configDescriptor, Layers: []ollamaDescriptor{modelDescriptor},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDirectory, "9b"), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = configPath
	expected := OllamaModel{
		Name: "qwen3.5:9b", ManifestDigest: "sha256:" + digest(manifestData),
		Family: OllamaFamilyGGUF, Format: OllamaArtifactFormat,
		SizeBytes: int64(len(configData) + len(modelData)),
	}
	return OllamaRuntime{Executable: "/usr/local/bin/ollama", Version: "0.21.2", ModelsDirectory: root}, expected, modelPath
}

func writeOllamaBlob(t *testing.T, directory, mediaType string, data []byte) (ollamaDescriptor, string) {
	t.Helper()
	hash := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(hash[:])
	path := filepath.Join(directory, strings.Replace(digest, ":", "-", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return ollamaDescriptor{MediaType: mediaType, Digest: digest, Size: int64(len(data))}, path
}
