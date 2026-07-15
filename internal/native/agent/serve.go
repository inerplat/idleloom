package agent

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/devruntime"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type serveProcess struct {
	process Process
	server  *http.Server
	done    chan struct{}

	stopOnce sync.Once
	mu       sync.Mutex
	waitErr  error
	stopErr  error
}

type chatCompletionRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens,omitempty"`
	Stream    bool          `json:"stream,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionResponse struct {
	ID      string                 `json:"id"`
	Object  string                 `json:"object"`
	Created int64                  `json:"created"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
}

type chatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

func (a *DevAgent) resolveServingKey(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment) ([]byte, error) {
	if a.config.Kubernetes == nil || assignment.Spec.Model == nil || assignment.Spec.Model.Server == nil {
		return nil, fmt.Errorf("the Kubernetes client and resolved server assignment are required")
	}
	server := assignment.Spec.Model.Server
	secret, err := a.config.Kubernetes.CoreV1().Secrets(assignment.Namespace).Get(ctx, server.AuthSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get Native serving auth Secret: %w", err)
	}
	if secret.Labels["app.kubernetes.io/managed-by"] != "idleloom-controller" ||
		secret.Labels["ai.idleloom.io/workload-uid"] != string(assignment.Spec.WorkloadRef.UID) ||
		secret.Labels["ai.idleloom.io/execution-id"] != assignment.Spec.ExecutionID ||
		secret.Labels["ai.idleloom.io/service-name"] != server.ServiceName {
		return nil, fmt.Errorf("the Native serving auth Secret identity does not match the assignment")
	}
	key := secret.Data["api-key"]
	if len(key) < 32 || len(key) > 256 || strings.ContainsAny(string(key), "\r\n\x00") {
		return nil, fmt.Errorf("the Native serving API key must contain 32 to 256 bytes without line breaks")
	}
	return append([]byte(nil), key...), nil
}

func startServeProcess(process Process, address, modelAlias string, apiKey []byte, agent *DevAgent) (Process, error) {
	return startServeProcessWithListener(process, address, modelAlias, apiKey, agent, net.Listen)
}

func startServeProcessWithListener(process Process, address, modelAlias string, apiKey []byte, agent *DevAgent, listen func(string, string) (net.Listener, error)) (Process, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != fmt.Sprint(nativev1alpha1.NativeServingPort) {
		return nil, fmt.Errorf("the Native serving address must use port %d", nativev1alpha1.NativeServingPort)
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.To4() == nil {
		return nil, fmt.Errorf("the Native serving address must use the WireKube IPv4 address")
	}
	listener, err := listen("tcp4", address)
	if err != nil {
		return nil, fmt.Errorf("listen for Native serving: %w", err)
	}
	serve := &serveProcess{process: process, done: make(chan struct{})}
	mux := servingHandler(process, modelAlias, apiKey, agent)
	serve.server = &http.Server{
		Handler: http.MaxBytesHandler(mux, 64<<10), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 6 * time.Minute, IdleTimeout: 30 * time.Second,
	}
	go func() {
		err := serve.server.Serve(listener)
		serve.mu.Lock()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serve.waitErr = fmt.Errorf("the Native serving endpoint stopped: %w", err)
		}
		serve.mu.Unlock()
		close(serve.done)
	}()
	return serve, nil
}

func servingHandler(process Process, modelAlias string, apiKey []byte, agent *DevAgent) http.Handler {
	mux := http.NewServeMux()
	generationSlots := make(chan struct{}, 1)
	authorize := func(next http.HandlerFunc) http.HandlerFunc {
		return func(response http.ResponseWriter, request *http.Request) {
			expected := append([]byte("Bearer "), apiKey...)
			provided := []byte(request.Header.Get("Authorization"))
			authorized := len(provided) == len(expected) && subtle.ConstantTimeCompare(provided, expected) == 1
			if alternate := []byte(request.Header.Get("X-Idleloom-API-Key")); !authorized {
				authorized = len(alternate) == len(apiKey) && subtle.ConstantTimeCompare(alternate, apiKey) == 1
			}
			if !authorized {
				http.Error(response, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(response, request)
		}
	}
	mux.HandleFunc("/health", func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !process.Alive() {
			http.Error(response, "model process is unavailable", http.StatusServiceUnavailable)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	mux.HandleFunc("/v1/models", authorize(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{
			"object": "list", "data": []map[string]any{{"id": modelAlias, "object": "model", "owned_by": "idleloom"}},
		})
	}))
	mux.HandleFunc("/v1/chat/completions", authorize(func(response http.ResponseWriter, request *http.Request) {
		handleChatCompletion(response, request, process, modelAlias, generationSlots, agent)
	}))
	return mux
}

func handleChatCompletion(response http.ResponseWriter, request *http.Request, process Process, modelAlias string, generationSlots chan struct{}, agent *DevAgent) {
	if request.Method != http.MethodPost {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input chatCompletionRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF || input.Stream || input.Model != modelAlias {
		http.Error(response, "invalid request", http.StatusBadRequest)
		return
	}
	prompt, err := chatPrompt(input.Messages)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	maxTokens := input.MaxTokens
	if maxTokens == 0 {
		maxTokens = 128
	}
	if maxTokens < 1 || maxTokens > 512 {
		http.Error(response, "max_tokens must be between 1 and 512", http.StatusBadRequest)
		return
	}
	select {
	case generationSlots <- struct{}{}:
		defer func() { <-generationSlots }()
	default:
		response.Header().Set("Retry-After", "1")
		http.Error(response, "model is already processing a request", http.StatusTooManyRequests)
		return
	}
	started := time.Now()
	agent.appendLog(started, "serving request started: maxTokens=%d messages=%d", maxTokens, len(input.Messages))
	// The runner protocol cannot cancel one request without terminating the
	// shared model process, so client disconnects must not reach Generate.
	generateCtx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), 5*time.Minute)
	defer cancel()
	result, err := process.Generate(generateCtx, devruntime.GenerateRequest{Prompt: prompt, MaxTokens: maxTokens})
	if err != nil {
		agent.appendLog(time.Now(), "serving request failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		http.Error(response, "model generation failed", http.StatusBadGateway)
		return
	}
	id, err := randomCompletionID()
	if err != nil {
		http.Error(response, "create response identity", http.StatusInternalServerError)
		return
	}
	agent.appendLog(time.Now(), "serving request completed: elapsed=%dms outputBytes=%d", result.ElapsedMillis, len([]byte(result.Text)))
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(chatCompletionResponse{
		ID: id, Object: "chat.completion", Created: time.Now().Unix(), Model: modelAlias,
		Choices: []chatCompletionChoice{{Index: 0, Message: chatMessage{Role: "assistant", Content: result.Text}, FinishReason: "stop"}},
	})
}

func chatPrompt(messages []chatMessage) (string, error) {
	if len(messages) == 0 || len(messages) > 32 {
		return "", fmt.Errorf("messages must contain 1 to 32 entries")
	}
	var prompt strings.Builder
	for _, message := range messages {
		if message.Role != "system" && message.Role != "user" && message.Role != "assistant" {
			return "", fmt.Errorf("message role must be system, user, or assistant")
		}
		content := strings.TrimSpace(message.Content)
		if content == "" || strings.ContainsRune(content, '\x00') {
			return "", fmt.Errorf("message content must be non-empty")
		}
		if prompt.Len() > 0 {
			prompt.WriteByte('\n')
		}
		prompt.WriteString(strings.ToUpper(message.Role[:1]))
		prompt.WriteString(message.Role[1:])
		prompt.WriteString(": ")
		prompt.WriteString(content)
	}
	prompt.WriteString("\nAssistant:")
	if prompt.Len() > 16<<10 {
		return "", fmt.Errorf("combined messages exceed 16384 UTF-8 bytes")
	}
	return prompt.String(), nil
}

func randomCompletionID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "chatcmpl-" + hex.EncodeToString(value[:]), nil
}

func (p *serveProcess) Alive() bool {
	if p == nil || p.process == nil || !p.process.Alive() {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *serveProcess) Stop() error {
	if p == nil {
		return nil
	}
	p.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.stopErr = errors.Join(p.server.Shutdown(ctx), p.process.Stop())
		select {
		case <-p.done:
		case <-ctx.Done():
			p.stopErr = errors.Join(p.stopErr, fmt.Errorf("the Native serving endpoint did not stop: %w", ctx.Err()))
		}
	})
	return p.stopErr
}

func (p *serveProcess) Generate(ctx context.Context, request devruntime.GenerateRequest) (devruntime.GenerateResponse, error) {
	return p.process.Generate(ctx, request)
}

func (p *serveProcess) Stderr() string {
	if p == nil || p.process == nil {
		return ""
	}
	return p.process.Stderr()
}

func (p *serveProcess) WaitError() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return errors.Join(p.waitErr, p.process.WaitError())
}

func (p *serveProcess) PID() int {
	if process, ok := p.process.(interface{ PID() int }); ok {
		return process.PID()
	}
	return 0
}
