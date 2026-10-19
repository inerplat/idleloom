package devruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
)

func TestValidateMaxTokensBounds(t *testing.T) {
	for _, maxTokens := range []int{1, 512, 4096, MaxGenerateTokens} {
		if err := validateMaxTokens(maxTokens); err != nil {
			t.Fatalf("validateMaxTokens(%d): %v", maxTokens, err)
		}
	}
	for _, maxTokens := range []int{0, -1, MaxGenerateTokens + 1} {
		if err := validateMaxTokens(maxTokens); err == nil {
			t.Fatalf("validateMaxTokens(%d) accepted an out-of-range budget", maxTokens)
		}
	}
}

func TestMaxGenerateTokensMatchesAPIAndRunner(t *testing.T) {
	if MaxGenerateTokens != int(nativev1alpha1.MaxBatchInferenceTokens) {
		t.Fatalf("MaxGenerateTokens = %d, API admits %d", MaxGenerateTokens, nativev1alpha1.MaxBatchInferenceTokens)
	}
	runner, err := RunnerSource()
	if err != nil {
		t.Fatal(err)
	}
	if bound := fmt.Sprintf("max_tokens > %d", MaxGenerateTokens); !strings.Contains(string(runner), bound) {
		t.Fatalf("runner.py does not enforce %q", bound)
	}
}

func TestLlamaCppGeneratePassesBudgetAboveFormerLimit(t *testing.T) {
	const budget = 4096
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var payload struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(response, err.Error(), http.StatusBadRequest)
			return
		}
		if payload.MaxTokens != budget {
			t.Errorf("max_tokens = %d, want %d", payload.MaxTokens, budget)
		}
		_, _ = response.Write([]byte(`{"choices":[{"message":{"content":"ready"}}]}`))
	}))
	defer server.Close()
	process := &LlamaCppProcess{client: server.Client(), baseURL: server.URL, model: LlamaCppModel{Name: "model.gguf"}}
	if _, err := process.Generate(context.Background(), GenerateRequest{Prompt: "hello", MaxTokens: budget}); err != nil {
		t.Fatal(err)
	}
}
