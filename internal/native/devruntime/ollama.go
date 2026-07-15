package devruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	OllamaRuntimeProfile = "ollama-gguf-v1"
	OllamaArtifactFormat = "gguf-v1"
	OllamaFamilyGGUF     = "ollama-gguf"
)

var (
	ollamaModelNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}:[a-z0-9][a-z0-9._-]{0,63}$`)
	ollamaVersionPattern   = regexp.MustCompile(`(?m)ollama(?: version is)? ([0-9]+)\.([0-9]+)\.([0-9]+)(?:\s|$)`)
)

type OllamaRuntime struct {
	Executable      string
	Version         string
	ModelsDirectory string
}

type OllamaModel struct {
	Name           string
	ManifestDigest string
	Family         string
	Format         string
	SizeBytes      int64
}

type ollamaDescriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type ollamaManifest struct {
	SchemaVersion int                `json:"schemaVersion"`
	MediaType     string             `json:"mediaType"`
	Config        ollamaDescriptor   `json:"config"`
	Layers        []ollamaDescriptor `json:"layers"`
}

type ollamaConfigBlob struct {
	ModelFormat string `json:"model_format"`
}

type OllamaProcessConfig struct {
	Runtime       OllamaRuntime
	Model         OllamaModel
	ContextLength int
	WorkDirectory string
	DeniedPaths   []string
	ReadyTimeout  time.Duration
	OnSpawn       func(int) error
}

type OllamaProcess struct {
	requestMu sync.Mutex
	lifeMu    sync.Mutex
	cmd       *exec.Cmd
	client    *http.Client
	baseURL   string
	model     OllamaModel
	context   int
	modelsDir string
	workDir   string
	done      chan struct{}
	waitMu    sync.Mutex
	waitErr   error
	stopped   bool
	stderr    *boundedBuffer
}

func FindOllama(explicit, modelsDirectory string) (OllamaRuntime, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return OllamaRuntime{}, fmt.Errorf("native Ollama runtime requires macOS on Apple Silicon")
	}
	executable := explicit
	if executable == "" {
		var err error
		executable, err = exec.LookPath("ollama")
		if err != nil {
			return OllamaRuntime{}, fmt.Errorf("find Ollama: install Ollama 0.17.1 or later and ensure ollama is on PATH")
		}
	}
	canonicalExecutable, err := canonicalPath(executable)
	if err != nil {
		return OllamaRuntime{}, err
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, canonicalExecutable, "--version").CombinedOutput()
	if err != nil {
		if probeCtx.Err() != nil {
			return OllamaRuntime{}, fmt.Errorf("read Ollama version: %w", probeCtx.Err())
		}
		return OllamaRuntime{}, fmt.Errorf("read Ollama version: %w", err)
	}
	match := ollamaVersionPattern.FindStringSubmatch(string(output))
	if len(match) != 4 {
		return OllamaRuntime{}, fmt.Errorf("ollama returned an unrecognized version")
	}
	version := strings.Join(match[1:], ".")
	if !ollamaVersionAtLeast(match[1:], 0, 17, 1) {
		return OllamaRuntime{}, fmt.Errorf("native GGUF runtime requires Ollama 0.17.1 or later; found %s", version)
	}
	if modelsDirectory == "" {
		modelsDirectory = os.Getenv("OLLAMA_MODELS")
	}
	if modelsDirectory == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return OllamaRuntime{}, err
		}
		modelsDirectory = filepath.Join(home, ".ollama", "models")
	}
	canonicalModels, err := canonicalPath(modelsDirectory)
	if err != nil {
		return OllamaRuntime{}, err
	}
	info, err := os.Stat(canonicalModels)
	if err != nil || !info.IsDir() {
		return OllamaRuntime{}, fmt.Errorf("ollama model store is unavailable; install a local GGUF model first")
	}
	return OllamaRuntime{Executable: canonicalExecutable, Version: version, ModelsDirectory: canonicalModels}, nil
}

func DiscoverOllamaModels(runtime OllamaRuntime) ([]OllamaModel, error) {
	root := filepath.Join(runtime.ModelsDirectory, "manifests", "registry.ollama.ai", "library")
	models, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []OllamaModel
	for _, modelDirectory := range models {
		if !modelDirectory.IsDir() {
			continue
		}
		tags, err := os.ReadDir(filepath.Join(root, modelDirectory.Name()))
		if err != nil {
			return nil, err
		}
		for _, tag := range tags {
			if tag.IsDir() {
				continue
			}
			name := modelDirectory.Name() + ":" + tag.Name()
			if !ollamaModelNamePattern.MatchString(name) {
				continue
			}
			model, err := inspectOllamaManifest(runtime.ModelsDirectory, name, false, context.Background())
			if err != nil || model.Format != OllamaArtifactFormat || model.Family != OllamaFamilyGGUF {
				continue
			}
			result = append(result, model)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	if len(result) > 64 {
		result = result[:64]
	}
	return result, nil
}

func InspectOllamaModel(runtime OllamaRuntime, name string) (OllamaModel, error) {
	return inspectOllamaManifest(runtime.ModelsDirectory, name, false, context.Background())
}

func VerifyOllamaModel(ctx context.Context, runtime OllamaRuntime, expected OllamaModel) error {
	actual, err := inspectOllamaManifest(runtime.ModelsDirectory, expected.Name, true, ctx)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("installed Ollama model does not match the pinned GGUF catalog entry")
	}
	return nil
}

func StartOllama(ctx context.Context, config OllamaProcessConfig) (*OllamaProcess, error) {
	if config.Runtime.Executable == "" || config.Runtime.ModelsDirectory == "" {
		return nil, fmt.Errorf("resolved Ollama runtime is required")
	}
	if config.ContextLength < 128 || config.ContextLength > 8192 {
		return nil, fmt.Errorf("ollama context length must be between 128 and 8192")
	}
	if err := VerifyOllamaModel(ctx, config.Runtime, config.Model); err != nil {
		return nil, fmt.Errorf("verify local Ollama model: %s", redactPaths(err.Error(), config.Runtime.ModelsDirectory, config.WorkDirectory))
	}
	if err := os.MkdirAll(config.WorkDirectory, 0o700); err != nil {
		return nil, err
	}
	for _, directory := range []string{filepath.Join(config.WorkDirectory, "home"), filepath.Join(config.WorkDirectory, "tmp")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
	}
	address, err := unusedLoopbackAddress()
	if err != nil {
		return nil, err
	}
	profile, err := ollamaSandboxProfile(config.Runtime, config.WorkDirectory, config.DeniedPaths)
	if err != nil {
		return nil, err
	}
	profilePath := filepath.Join(config.WorkDirectory, "ollama.sb")
	if err := atomicWrite(profilePath, []byte(profile), 0o600); err != nil {
		return nil, err
	}
	command := exec.Command("/usr/bin/sandbox-exec", "-f", profilePath, config.Runtime.Executable, "serve")
	command.Dir = config.WorkDirectory
	command.Env = []string{
		"HOME=" + filepath.Join(config.WorkDirectory, "home"), "TMPDIR=" + filepath.Join(config.WorkDirectory, "tmp"),
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8",
		"OLLAMA_HOST=" + address, "OLLAMA_MODELS=" + config.Runtime.ModelsDirectory,
		"OLLAMA_CONTEXT_LENGTH=" + strconv.Itoa(config.ContextLength), "OLLAMA_KEEP_ALIVE=24h",
		"OLLAMA_MAX_LOADED_MODELS=1", "OLLAMA_MAX_QUEUE=1", "OLLAMA_NUM_PARALLEL=1",
		"OLLAMA_NO_CLOUD=true", "OLLAMA_NOPRUNE=true",
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr := &boundedBuffer{limit: maxStderrBytes}
	command.Stdout = stderr
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start sandboxed Ollama daemon: %w", err)
	}
	process := &OllamaProcess{
		cmd: command, client: &http.Client{Transport: &http.Transport{Proxy: nil}},
		baseURL: "http://" + address, model: config.Model, context: config.ContextLength,
		modelsDir: config.Runtime.ModelsDirectory, workDir: config.WorkDirectory,
		done: make(chan struct{}), stderr: stderr,
	}
	go func() {
		err := command.Wait()
		process.waitMu.Lock()
		process.waitErr = err
		process.waitMu.Unlock()
		close(process.done)
	}()
	if config.OnSpawn != nil {
		if err := config.OnSpawn(process.PID()); err != nil {
			_ = process.Stop()
			return nil, fmt.Errorf("record spawned Ollama daemon: %w", err)
		}
	}
	readyTimeout := config.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = 2 * time.Minute
	}
	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := process.waitReady(readyCtx); err != nil {
		_ = process.Stop()
		return nil, err
	}
	if err := process.warmAndVerifyMetal(readyCtx); err != nil {
		_ = process.Stop()
		return nil, err
	}
	return process, nil
}

func (p *OllamaProcess) Generate(ctx context.Context, request GenerateRequest) (GenerateResponse, error) {
	if request.Prompt == "" || len([]byte(request.Prompt)) > 16<<10 {
		return GenerateResponse{}, fmt.Errorf("prompt must contain 1 to 16384 UTF-8 bytes")
	}
	if request.MaxTokens < 1 || request.MaxTokens > 512 {
		return GenerateResponse{}, fmt.Errorf("maxTokens must be between 1 and 512")
	}
	p.requestMu.Lock()
	defer p.requestMu.Unlock()
	payload := struct {
		Model     string `json:"model"`
		Prompt    string `json:"prompt"`
		Stream    bool   `json:"stream"`
		Think     bool   `json:"think"`
		KeepAlive string `json:"keep_alive"`
		Options   struct {
			NumPredict int `json:"num_predict"`
			NumContext int `json:"num_ctx"`
		} `json:"options"`
	}{Model: p.model.Name, Prompt: request.Prompt, Stream: false, Think: false, KeepAlive: "24h"}
	payload.Options.NumPredict = request.MaxTokens
	payload.Options.NumContext = p.context
	started := time.Now()
	var response struct {
		Response string `json:"response"`
		Done     bool   `json:"done"`
		Error    string `json:"error"`
	}
	if err := p.doJSON(ctx, http.MethodPost, "/api/generate", payload, &response); err != nil {
		return GenerateResponse{}, err
	}
	if response.Error != "" {
		return GenerateResponse{}, fmt.Errorf("ollama generation failed: %s", p.redactPrivatePaths(response.Error))
	}
	if !response.Done || len([]byte(response.Response)) > 1<<20 {
		return GenerateResponse{}, fmt.Errorf("ollama returned an invalid generation response")
	}
	return GenerateResponse{Text: response.Response, ElapsedMillis: time.Since(started).Milliseconds()}, nil
}

func (p *OllamaProcess) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *OllamaProcess) Alive() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return p.PID() > 0
	}
}

func (p *OllamaProcess) Stop() error {
	if p == nil {
		return nil
	}
	p.lifeMu.Lock()
	defer p.lifeMu.Unlock()
	p.waitMu.Lock()
	p.stopped = true
	p.waitMu.Unlock()
	pid := p.PID()
	if pid == 0 {
		return nil
	}
	if p.client != nil {
		transport, ok := p.client.Transport.(*http.Transport)
		if ok {
			transport.CloseIdleConnections()
		}
	}
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("kill Ollama process group: %w", err)
	}
	select {
	case <-p.done:
		if err := waitProcessGroupGone(pid, time.Second); err != nil {
			return fmt.Errorf("ollama %w", err)
		}
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("ollama process group did not stop")
	}
}

func (p *OllamaProcess) Stderr() string {
	if p == nil || p.stderr == nil {
		return ""
	}
	p.waitMu.Lock()
	stopped := p.stopped
	waitErr := p.waitErr
	p.waitMu.Unlock()
	if stopped || waitErr == nil {
		return ""
	}
	return p.diagnostics()
}

func (p *OllamaProcess) WaitError() error {
	if p == nil {
		return nil
	}
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	return p.waitErr
}

func (p *OllamaProcess) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastObservation string
	for {
		var tags struct {
			Models []struct {
				Name   string `json:"name"`
				Digest string `json:"digest"`
			} `json:"models"`
		}
		err := p.doJSON(ctx, http.MethodGet, "/api/tags", nil, &tags)
		if err == nil {
			for _, model := range tags.Models {
				if model.Name == p.model.Name && "sha256:"+strings.TrimPrefix(model.Digest, "sha256:") == p.model.ManifestDigest {
					return nil
				}
			}
			lastObservation = fmt.Sprintf("Ollama daemon does not yet expose the pinned local model %s", p.model.Name)
		} else {
			lastObservation = p.redactPrivatePaths(err.Error())
		}
		select {
		case <-p.done:
			return fmt.Errorf("ollama exited before readiness: %w: %s: %s", p.WaitError(), lastObservation, p.diagnostics())
		case <-ctx.Done():
			return fmt.Errorf("wait for Ollama readiness: %w: %s: %s", ctx.Err(), lastObservation, p.diagnostics())
		case <-ticker.C:
		}
	}
}

func (p *OllamaProcess) warmAndVerifyMetal(ctx context.Context) error {
	payload := struct {
		Model     string `json:"model"`
		Prompt    string `json:"prompt"`
		Stream    bool   `json:"stream"`
		Think     bool   `json:"think"`
		KeepAlive string `json:"keep_alive"`
		Options   struct {
			NumPredict int `json:"num_predict"`
			NumContext int `json:"num_ctx"`
		} `json:"options"`
	}{Model: p.model.Name, Prompt: "Reply with one word.", KeepAlive: "24h"}
	payload.Options.NumPredict = 1
	payload.Options.NumContext = p.context
	var generated struct {
		Done  bool   `json:"done"`
		Error string `json:"error"`
	}
	if err := p.doJSON(ctx, http.MethodPost, "/api/generate", payload, &generated); err != nil {
		return fmt.Errorf("warm pinned Ollama model: %w: %s", err, p.diagnostics())
	}
	if !generated.Done || generated.Error != "" {
		return fmt.Errorf("warm pinned Ollama model: %s", p.redactPrivatePaths(generated.Error))
	}
	var loaded struct {
		Models []struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			SizeVRAM int64  `json:"size_vram"`
		} `json:"models"`
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		loaded.Models = nil
		if err := p.doJSON(ctx, http.MethodGet, "/api/ps", nil, &loaded); err != nil {
			return fmt.Errorf("verify Ollama Metal placement: %w", err)
		}
		for _, model := range loaded.Models {
			if model.Name == p.model.Name && fullOllamaMetalPlacement(model.Size, model.SizeVRAM) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("ollama loaded %s without full Metal placement: loaded=%+v diagnostics=%s", p.model.Name, loaded.Models, p.diagnostics())
}

func fullOllamaMetalPlacement(size, sizeVRAM int64) bool {
	return size > 0 && sizeVRAM == size
}

func (p *OllamaProcess) diagnostics() string {
	text := p.redactPrivatePaths(p.stderr.String())
	lines := strings.Split(text, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "ssh-ed25519 ") || strings.HasPrefix(trimmed, "Your new public key is:") ||
			(strings.Contains(trimmed, "[GIN]") && strings.Contains(trimmed, `GET      "/api/ps"`)) {
			continue
		}
		filtered = append(filtered, line)
	}
	if len(filtered) > 60 {
		filtered = filtered[len(filtered)-60:]
	}
	text = strings.Join(filtered, "\n")
	if len(text) > 16<<10 {
		text = text[len(text)-(16<<10):]
	}
	return text
}

func (p *OllamaProcess) redactPrivatePaths(text string) string {
	return redactPaths(text, p.modelsDir, p.workDir)
}

func redactPaths(text string, paths ...string) string {
	for _, path := range paths {
		if path != "" {
			text = strings.ReplaceAll(text, path, "<private-path>")
		}
	}
	return text
}

func (p *OllamaProcess) doJSON(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	limited := io.LimitReader(response.Body, 2<<20)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(limited)
		return fmt.Errorf("the Ollama API returned %s: %s", response.Status, strings.TrimSpace(p.redactPrivatePaths(string(data))))
	}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode Ollama API response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode Ollama API response: trailing data")
	}
	return nil
}

func inspectOllamaManifest(modelsDirectory, name string, strong bool, ctx context.Context) (OllamaModel, error) {
	if !ollamaModelNamePattern.MatchString(name) {
		return OllamaModel{}, fmt.Errorf("invalid Ollama model %q", name)
	}
	root, err := os.OpenRoot(modelsDirectory)
	if err != nil {
		return OllamaModel{}, fmt.Errorf("open Ollama model store: %w", err)
	}
	defer func() { _ = root.Close() }()
	modelName, tag, _ := strings.Cut(name, ":")
	manifestPath := filepath.Join("manifests", "registry.ollama.ai", "library", modelName, tag)
	data, err := readRootRegularFile(root, manifestPath, 1<<20)
	if err != nil {
		return OllamaModel{}, fmt.Errorf("read Ollama manifest for %s: %w", name, err)
	}
	manifestDigest := "sha256:" + digest(data)
	var manifest ollamaManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return OllamaModel{}, fmt.Errorf("decode Ollama manifest for %s: %w", name, err)
	}
	if manifest.SchemaVersion != 2 || manifest.Config.Digest == "" || manifest.Config.Size <= 0 || len(manifest.Layers) == 0 {
		return OllamaModel{}, fmt.Errorf("the Ollama manifest for %s is incomplete", name)
	}
	descriptors := append([]ollamaDescriptor{manifest.Config}, manifest.Layers...)
	var size int64
	for _, descriptor := range descriptors {
		if !sha256Digest(descriptor.Digest) || descriptor.Size <= 0 {
			return OllamaModel{}, fmt.Errorf("the Ollama manifest for %s contains an invalid blob descriptor", name)
		}
		blobPath := filepath.Join("blobs", strings.Replace(descriptor.Digest, ":", "-", 1))
		if err := verifyOllamaBlob(ctx, root, blobPath, descriptor, strong); err != nil {
			return OllamaModel{}, fmt.Errorf("verify Ollama model %s: %w", name, err)
		}
		if descriptor.Size > (64<<30)-size {
			return OllamaModel{}, fmt.Errorf("the Ollama model %s exceeds the supported size", name)
		}
		size += descriptor.Size
	}
	configData, err := readRootRegularFile(root, filepath.Join("blobs", strings.Replace(manifest.Config.Digest, ":", "-", 1)), 1<<20)
	if err != nil {
		return OllamaModel{}, err
	}
	if int64(len(configData)) != manifest.Config.Size || "sha256:"+digest(configData) != manifest.Config.Digest {
		return OllamaModel{}, fmt.Errorf("the Ollama model %s config blob does not match its descriptor", name)
	}
	var modelConfig ollamaConfigBlob
	if err := json.Unmarshal(configData, &modelConfig); err != nil {
		return OllamaModel{}, fmt.Errorf("decode Ollama model metadata: %w", err)
	}
	family := ""
	format := ""
	if modelConfig.ModelFormat == "gguf" {
		format = OllamaArtifactFormat
		family = OllamaFamilyGGUF
	}
	return OllamaModel{Name: name, ManifestDigest: manifestDigest, Family: family, Format: format, SizeBytes: size}, nil
}

func verifyOllamaBlob(ctx context.Context, root *os.Root, path string, descriptor ollamaDescriptor, strong bool) error {
	file, err := root.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != descriptor.Size {
		return fmt.Errorf("blob %s is not a regular file with the declared size", descriptor.Digest)
	}
	if !strong {
		return nil
	}
	hash := sha256.New()
	buffer := make([]byte, 4<<20)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != descriptor.Digest {
		return fmt.Errorf("blob %s content digest does not match its name", descriptor.Digest)
	}
	after, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, after) || after.Size() != info.Size() || after.Mode() != info.Mode() || !after.ModTime().Equal(info.ModTime()) {
		return fmt.Errorf("blob %s changed while it was being verified", descriptor.Digest)
	}
	return nil
}

func readRootRegularFile(root *os.Root, path string, limit int64) ([]byte, error) {
	file, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		return nil, fmt.Errorf("%s is not a bounded regular file", filepath.Base(path))
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != info.Size() || int64(len(data)) > limit {
		return nil, fmt.Errorf("%s changed while it was being read", filepath.Base(path))
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, after) || after.Size() != info.Size() || after.Mode() != info.Mode() || !after.ModTime().Equal(info.ModTime()) {
		return nil, fmt.Errorf("%s changed while it was being read", filepath.Base(path))
	}
	return data, nil
}

func sha256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && strings.ToLower(value) == value
}

func ollamaVersionAtLeast(parts []string, major, minor, patch int) bool {
	if len(parts) != 3 {
		return false
	}
	values := make([]int, 3)
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return false
		}
		values[index] = value
	}
	minimum := []int{major, minor, patch}
	for index := range values {
		if values[index] != minimum[index] {
			return values[index] > minimum[index]
		}
	}
	return true
}

func unusedLoopbackAddress() (string, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", err
	}
	return address, nil
}

func ollamaSandboxProfile(runtime OllamaRuntime, workDirectory string, denied []string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	work, err := canonicalPath(workDirectory)
	if err != nil {
		return "", err
	}
	cacheOutput, err := exec.Command("/usr/bin/getconf", "DARWIN_USER_CACHE_DIR").Output()
	if err != nil {
		return "", err
	}
	cache := strings.TrimSpace(string(cacheOutput))
	denied = append([]string{home, "/Users", "/Volumes", "/Network"}, denied...)
	var rules strings.Builder
	rules.WriteString("(version 1)\n(deny default)\n")
	rules.WriteString("(allow process-fork)\n(allow process-exec (literal \"")
	rules.WriteString(escapeSandbox(runtime.Executable))
	rules.WriteString("\"))\n")
	rules.WriteString("(allow signal (target self))\n(allow sysctl-read)\n")
	rules.WriteString("(allow mach-lookup (global-name \"com.apple.MTLCompilerService\"))\n")
	rules.WriteString("(allow iokit-open)\n(allow iokit-get-properties)\n")
	rules.WriteString("(allow file-read*)\n")
	rules.WriteString("(allow file-write* (literal \"/dev/null\"))\n")
	for _, path := range denied {
		if path == "" {
			continue
		}
		absolute, err := canonicalPath(path)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&rules, "(deny file-read* (subpath \"%s\"))\n", escapeSandbox(absolute))
		fmt.Fprintf(&rules, "(deny file-write* (subpath \"%s\"))\n", escapeSandbox(absolute))
	}
	fmt.Fprintf(&rules, "(allow file-read* (subpath \"%s\"))\n", escapeSandbox(runtime.ModelsDirectory))
	fmt.Fprintf(&rules, "(allow file-write* (subpath \"%s\"))\n", escapeSandbox(work))
	if filepath.IsAbs(cache) {
		fmt.Fprintf(&rules, "(allow file-write* (subpath \"%s\"))\n", escapeSandbox(cache))
	}
	rules.WriteString("(allow network-bind network-inbound (local ip \"localhost:*\"))\n")
	rules.WriteString("(allow network-outbound (remote ip \"localhost:*\"))\n")
	return rules.String(), nil
}
