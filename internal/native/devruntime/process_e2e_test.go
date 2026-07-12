package devruntime

import (
	"context"
	"os"
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
