package devruntime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSandboxedMLXInference(t *testing.T) {
	root := os.Getenv("IDLELOOM_NATIVE_E2E_ROOT")
	if root == "" {
		t.Skip("set IDLELOOM_NATIVE_E2E_ROOT to run the Metal integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := (Preparer{Root: root}).PrepareRuntime(ctx); err != nil {
		t.Fatalf("prepare locked runtime: %v", err)
	}
	process, err := Start(ctx, ProcessConfig{
		Layout:       NewLayout(root),
		Nonce:        strings.Repeat("1", 64),
		ReadyTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := process.Stop(); err != nil {
			t.Error(err)
		}
	}()
	result, err := process.Generate(ctx, GenerateRequest{Prompt: "Reply with one short word.", MaxTokens: 8})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Text) == "" {
		t.Fatal("model returned an empty response")
	}
}

func TestSandboxedMLXTraining(t *testing.T) {
	root := os.Getenv("IDLELOOM_NATIVE_E2E_ROOT")
	if root == "" {
		t.Skip("set IDLELOOM_NATIVE_E2E_ROOT to run the Metal integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := (Preparer{Root: root}).PrepareRuntime(ctx); err != nil {
		t.Fatalf("prepare locked runtime: %v", err)
	}
	work := t.TempDir()
	var output bytes.Buffer
	process, err := StartTraining(ctx, TrainingConfig{
		Layout: NewLayout(root), WorkDirectory: work,
		Source: `
from pathlib import Path
import mlx.core as mx

mx.set_default_device(mx.gpu)
values = mx.arange(1, 1025, dtype=mx.float32)
loss = mx.mean(mx.square(values))
mx.eval(loss)
Path("checkpoint.txt").write_text(f"{float(loss):.8f}\n")
print(f"device={mx.default_device()}")
print(f"loss={float(loss):.8f}")
`,
		Network: "None", Timeout: time.Minute, Output: &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()
	for process.Alive() {
		select {
		case <-deadline.C:
			_ = process.Stop()
			t.Fatal("training process did not finish")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := process.WaitError(); err != nil {
		t.Fatalf("training process failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "device=Device(gpu, 0)") || !strings.Contains(output.String(), "loss=") {
		t.Fatalf("training output did not confirm Metal execution:\n%s", output.String())
	}
	checkpoint, err := os.ReadFile(filepath.Join(work, "checkpoint.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(checkpoint)) == "" {
		t.Fatal("training checkpoint is empty")
	}
}

func TestSandboxedOllamaGGUFInference(t *testing.T) {
	modelName := os.Getenv("IDLELOOM_OLLAMA_E2E_MODEL")
	if modelName == "" {
		t.Skip("set IDLELOOM_OLLAMA_E2E_MODEL to run the Ollama GGUF integration test")
	}
	runtime, err := FindOllama("", "")
	if err != nil {
		t.Fatal(err)
	}
	model, err := InspectOllamaModel(runtime, modelName)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	workDirectory, err := os.MkdirTemp("/var/tmp", "idleloom-ollama-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(workDirectory) }()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	stateDirectory := filepath.Join(home, "Library", "Application Support", "idleloom", "native")
	process, err := StartOllama(ctx, OllamaProcessConfig{
		Runtime: runtime, Model: model, ContextLength: 2048,
		WorkDirectory: workDirectory, DeniedPaths: []string{stateDirectory, filepath.Join(stateDirectory, "agent.kubeconfig")},
		ReadyTimeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := process.Stop(); err != nil {
			t.Error(err)
		}
	}()
	result, err := process.Generate(ctx, GenerateRequest{Prompt: "Reply with only: ready", MaxTokens: 8})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Text) == "" {
		t.Fatal("Ollama returned an empty response")
	}
}

func TestSandboxedLlamaCppGGUFInference(t *testing.T) {
	root := os.Getenv("IDLELOOM_NATIVE_E2E_ROOT")
	modelName := os.Getenv("IDLELOOM_LLAMA_CPP_E2E_MODEL")
	if root == "" || modelName == "" {
		t.Skip("set IDLELOOM_NATIVE_E2E_ROOT and IDLELOOM_LLAMA_CPP_E2E_MODEL to run the llama.cpp GGUF integration test")
	}
	modelsDirectory := filepath.Join(root, "models", "gguf")
	runtime, err := FindLlamaCpp(context.Background(), "", modelsDirectory)
	if err != nil {
		t.Fatal(err)
	}
	models, err := (&LlamaCppDiscovery{}).Discover(context.Background(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	var model LlamaCppModel
	for _, candidate := range models {
		if candidate.Name == modelName {
			model = candidate
			break
		}
	}
	if model.Name == "" {
		t.Fatalf("managed llama.cpp model %q was not discovered", modelName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	workDirectory, err := os.MkdirTemp("/var/tmp", "idleloom-llama-cpp-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(workDirectory) }()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	stateDirectory := filepath.Join(home, "Library", "Application Support", "idleloom", "native")
	process, err := StartLlamaCpp(ctx, LlamaCppProcessConfig{
		Runtime: runtime, Model: model, ContextLength: 2048,
		WorkDirectory: workDirectory, DeniedPaths: []string{stateDirectory, filepath.Join(stateDirectory, "agent.kubeconfig")},
		ReadyTimeout: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := process.Stop(); err != nil {
			t.Error(err)
		}
	}()
	result, err := process.Generate(ctx, GenerateRequest{Prompt: "Reply with only: ready", MaxTokens: 8})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result.Text) == "" {
		t.Fatal("llama.cpp returned an empty response")
	}
}
