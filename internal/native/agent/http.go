package agent

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/inerplat/idleloom/internal/native/devruntime"
)

type endpointReceipt struct {
	Address string `json:"address"`
	Token   string `json:"token"`
}

func (a *DevAgent) startHTTP() error {
	listener, err := net.Listen("tcp", a.config.ListenAddress)
	if err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(listener.Addr().String())
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return errors.Join(errors.New("native development endpoint must bind to loopback"), listener.Close())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.authorize(a.handleHealth))
	mux.HandleFunc("/v1/generate", a.authorize(a.handleGenerate))
	a.server = &http.Server{
		Handler:           http.MaxBytesHandler(mux, 20<<10),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      6 * time.Minute,
		IdleTimeout:       30 * time.Second,
	}
	a.serverErrors = make(chan error, 1)
	receipt, err := json.MarshalIndent(endpointReceipt{Address: "http://" + listener.Addr().String(), Token: a.endpointToken}, "", "  ")
	if err != nil {
		return errors.Join(err, listener.Close())
	}
	if err := writePrivate(filepath.Join(a.config.StateDirectory, "endpoint.json"), append(receipt, '\n')); err != nil {
		return errors.Join(err, listener.Close())
	}
	go func() {
		if err := a.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.serverErrors <- err
		}
	}()
	return nil
}

func (a *DevAgent) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		expected := "Bearer " + a.endpointToken
		provided := request.Header.Get("Authorization")
		if len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(response, request)
	}
}

func (a *DevAgent) handleHealth(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.mu.RLock()
	running := a.process != nil && a.process.Alive()
	a.mu.RUnlock()
	if !running {
		http.Error(response, "no native workload is running", http.StatusServiceUnavailable)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_, _ = response.Write([]byte("{\"ready\":true}\n"))
}

func (a *DevAgent) handleGenerate(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input devruntime.GenerateRequest
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		http.Error(response, "invalid request", http.StatusBadRequest)
		return
	}
	a.mu.RLock()
	process := a.process
	a.mu.RUnlock()
	if process == nil || !process.Alive() {
		http.Error(response, "no native workload is running", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 5*time.Minute)
	defer cancel()
	started := time.Now()
	a.appendLog(started, "inference request started: maxTokens=%d", input.MaxTokens)
	result, err := process.Generate(ctx, input)
	if err != nil {
		a.appendLog(time.Now(), "inference request failed after %s: %v", time.Since(started).Round(time.Millisecond), err)
		http.Error(response, err.Error(), http.StatusBadGateway)
		return
	}
	a.appendLog(time.Now(), "inference request completed: elapsed=%dms outputBytes=%d", result.ElapsedMillis, len([]byte(result.Text)))
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(result)
}

func writePrivate(name string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), ".endpoint-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, name)
}
