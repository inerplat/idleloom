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
	LlamaCppRuntimeProfile = "llama-cpp-metal-v1"
	LlamaCppArtifactFormat = "gguf-v1"
	LlamaCppFamilyGGUF     = "gguf"
)

var (
	llamaCppVersionPattern = regexp.MustCompile(`(?m)^version: ([0-9]+) \(([0-9A-Za-z._-]+)\)$`)
	llamaCppDevicePattern  = regexp.MustCompile(`(?m)^\s*(MTL[0-9]+):\s+.+$`)
	ggufFileNamePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,190}\.gguf$`)
	fullGPUOffloadPattern  = regexp.MustCompile(`offloaded ([0-9]+)/([0-9]+) layers to GPU`)
	llamaCppRequiredFlags  = []string{
		"--model", "--host", "--port", "--ctx-size", "--parallel",
		"--n-gpu-layers", "--device", "--fit", "--cache-ram", "--no-ui",
	}
)

type LlamaCppRuntime struct {
	Executable      string
	Version         string
	Build           int
	Device          string
	ModelsDirectory string
}

type LlamaCppModel struct {
	Name           string
	ManifestDigest string
	Family         string
	Format         string
	SizeBytes      int64
}

type LlamaCppProcessConfig struct {
	Runtime       LlamaCppRuntime
	Model         LlamaCppModel
	ContextLength int
	WorkDirectory string
	DeniedPaths   []string
	ReadyTimeout  time.Duration
	OnSpawn       func(int) error
}

type llamaCppCachedModel struct {
	info  os.FileInfo
	model LlamaCppModel
}

type LlamaCppDiscovery struct {
	mu     sync.Mutex
	models map[string]llamaCppCachedModel
}

type LlamaCppProcess struct {
	requestMu sync.Mutex
	lifeMu    sync.Mutex
	cmd       *exec.Cmd
	client    *http.Client
	baseURL   string
	model     LlamaCppModel
	modelPath string
	workDir   string
	done      chan struct{}
	waitMu    sync.Mutex
	waitErr   error
	stopped   bool
	stderr    *boundedBuffer
	metal     *llamaCppMetalProbe
}

type llamaCppMetalProbe struct {
	mu      sync.Mutex
	pending []byte
	device  string
	loaded  bool
	full    bool
}

func FindLlamaCpp(ctx context.Context, explicit, modelsDirectory string) (LlamaCppRuntime, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return LlamaCppRuntime{}, fmt.Errorf("native llama.cpp runtime requires macOS on Apple Silicon")
	}
	executable := explicit
	if executable == "" {
		var err error
		executable, err = exec.LookPath("llama-server")
		if err != nil {
			return LlamaCppRuntime{}, fmt.Errorf("find llama-server: install llama.cpp and ensure llama-server is on PATH")
		}
	}
	canonicalExecutable, err := canonicalPath(executable)
	if err != nil {
		return LlamaCppRuntime{}, err
	}
	versionOutput, err := runLlamaCppProbe(ctx, canonicalExecutable, "--version")
	if err != nil {
		return LlamaCppRuntime{}, fmt.Errorf("read llama.cpp version: %w", err)
	}
	match := llamaCppVersionPattern.FindSubmatch(bytes.TrimSpace(versionOutput))
	if len(match) != 3 {
		return LlamaCppRuntime{}, fmt.Errorf("llama-server returned an unrecognized version")
	}
	build, err := strconv.Atoi(string(match[1]))
	if err != nil || build < 1 {
		return LlamaCppRuntime{}, fmt.Errorf("llama-server returned an invalid build number")
	}
	helpOutput, err := runLlamaCppProbe(ctx, canonicalExecutable, "--help")
	if err != nil {
		return LlamaCppRuntime{}, fmt.Errorf("read llama.cpp capabilities: %w", err)
	}
	if missing := missingLlamaCppFlags(string(helpOutput)); len(missing) > 0 {
		return LlamaCppRuntime{}, fmt.Errorf("llama-server build %d is missing required options: %s", build, strings.Join(missing, ", "))
	}
	deviceOutput, err := runLlamaCppProbe(ctx, canonicalExecutable, "--list-devices")
	if err != nil {
		return LlamaCppRuntime{}, fmt.Errorf("list llama.cpp devices: %w", err)
	}
	deviceMatch := llamaCppDevicePattern.FindSubmatch(deviceOutput)
	if len(deviceMatch) != 2 {
		return LlamaCppRuntime{}, fmt.Errorf("llama.cpp does not expose an Apple Metal device")
	}
	if modelsDirectory == "" {
		modelsDirectory = filepath.Join(DefaultRoot(), "models", "gguf")
	}
	if err := os.MkdirAll(modelsDirectory, 0o700); err != nil {
		return LlamaCppRuntime{}, fmt.Errorf("create llama.cpp model directory: %w", err)
	}
	modelsInfo, err := os.Lstat(modelsDirectory)
	if err != nil {
		return LlamaCppRuntime{}, fmt.Errorf("inspect llama.cpp model directory: %w", err)
	}
	if modelsInfo.Mode()&os.ModeSymlink != 0 || !modelsInfo.IsDir() {
		return LlamaCppRuntime{}, fmt.Errorf("llama.cpp model directory must be a real directory")
	}
	if err := os.Chmod(modelsDirectory, 0o700); err != nil {
		return LlamaCppRuntime{}, fmt.Errorf("restrict llama.cpp model directory: %w", err)
	}
	canonicalModels, err := canonicalPath(modelsDirectory)
	if err != nil {
		return LlamaCppRuntime{}, err
	}
	return LlamaCppRuntime{
		Executable: canonicalExecutable, Version: string(match[1]) + "-" + string(match[2]), Build: build,
		Device: string(deviceMatch[1]), ModelsDirectory: canonicalModels,
	}, nil
}

func runLlamaCppProbe(ctx context.Context, executable string, arguments ...string) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, executable, arguments...).CombinedOutput()
	if err != nil {
		if probeCtx.Err() != nil {
			return nil, probeCtx.Err()
		}
		return nil, err
	}
	return output, nil
}

func missingLlamaCppFlags(help string) []string {
	var missing []string
	for _, flag := range llamaCppRequiredFlags {
		if !strings.Contains(help, flag) {
			missing = append(missing, flag)
		}
	}
	return missing
}

func (discovery *LlamaCppDiscovery) Discover(ctx context.Context, runtime LlamaCppRuntime) ([]LlamaCppModel, error) {
	if runtime.ModelsDirectory == "" {
		return nil, fmt.Errorf("llama.cpp model directory is required")
	}
	discovery.mu.Lock()
	defer discovery.mu.Unlock()
	if discovery.models == nil {
		discovery.models = make(map[string]llamaCppCachedModel)
	}
	entries, err := os.ReadDir(runtime.ModelsDirectory)
	if err != nil {
		return nil, fmt.Errorf("read llama.cpp model directory: %w", err)
	}
	root, err := os.OpenRoot(runtime.ModelsDirectory)
	if err != nil {
		return nil, fmt.Errorf("open llama.cpp model directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	seen := make(map[string]struct{})
	models := make([]LlamaCppModel, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !ggufFileNamePattern.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(runtime.ModelsDirectory, entry.Name())
		initial, err := os.Lstat(path)
		if err != nil || validateLockedRegularInfo(path, initial, 0) != nil {
			continue
		}
		file, err := root.Open(entry.Name())
		if err != nil {
			continue
		}
		info, statErr := file.Stat()
		if statErr != nil || !os.SameFile(initial, info) || validateLockedRegularInfo(path, info, 0) != nil {
			_ = file.Close()
			continue
		}
		cached, cachedOK := discovery.models[entry.Name()]
		if cachedOK && sameStableFileInfo(cached.info, info) {
			_ = file.Close()
			models = append(models, cached.model)
			seen[entry.Name()] = struct{}{}
			continue
		}
		model, inspectErr := inspectLlamaCppFile(ctx, file, entry.Name(), info)
		_ = file.Close()
		if inspectErr != nil {
			continue
		}
		after, err := os.Lstat(path)
		if err != nil || !sameStableFileInfo(info, after) {
			continue
		}
		discovery.models[entry.Name()] = llamaCppCachedModel{info: info, model: model}
		models = append(models, model)
		seen[entry.Name()] = struct{}{}
	}
	for name := range discovery.models {
		if _, ok := seen[name]; !ok {
			delete(discovery.models, name)
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	if len(models) > 64 {
		models = models[:64]
	}
	return models, nil
}

func VerifyLlamaCppModel(ctx context.Context, runtime LlamaCppRuntime, expected LlamaCppModel) error {
	if !ggufFileNamePattern.MatchString(expected.Name) {
		return fmt.Errorf("invalid llama.cpp GGUF filename %q", expected.Name)
	}
	modelPath := filepath.Join(runtime.ModelsDirectory, expected.Name)
	initial, err := os.Lstat(modelPath)
	if err != nil {
		return fmt.Errorf("inspect llama.cpp GGUF model: %w", err)
	}
	if err := validateLockedRegularInfo(modelPath, initial, 0); err != nil {
		return err
	}
	root, err := os.OpenRoot(runtime.ModelsDirectory)
	if err != nil {
		return fmt.Errorf("open llama.cpp model directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(expected.Name)
	if err != nil {
		return fmt.Errorf("open llama.cpp GGUF model: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(initial, info) || validateLockedRegularInfo(modelPath, info, 0) != nil {
		return fmt.Errorf("llama.cpp GGUF model changed while it was opened")
	}
	actual, err := inspectLlamaCppFile(ctx, file, expected.Name, info)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("installed llama.cpp GGUF model does not match the pinned catalog entry")
	}
	after, err := os.Lstat(modelPath)
	if err != nil || !sameStableFileInfo(info, after) {
		return fmt.Errorf("llama.cpp GGUF model changed during verification")
	}
	return nil
}

func inspectLlamaCppFile(ctx context.Context, file *os.File, name string, initial os.FileInfo) (LlamaCppModel, error) {
	if file == nil || initial == nil || validateLockedRegularInfo(name, initial, 0) != nil || initial.Size() < 4 || initial.Size() > 64<<30 {
		return LlamaCppModel{}, fmt.Errorf("llama.cpp model must be a regular GGUF file no larger than 64 GiB")
	}
	hash := sha256.New()
	buffer := make([]byte, 4<<20)
	var first [4]byte
	written := 0
	for {
		if err := ctx.Err(); err != nil {
			return LlamaCppModel{}, err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			if written < len(first) {
				written += copy(first[written:], buffer[:count])
			}
			_, _ = hash.Write(buffer[:count])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return LlamaCppModel{}, readErr
		}
	}
	if string(first[:]) != "GGUF" {
		return LlamaCppModel{}, fmt.Errorf("%s does not contain a GGUF header", name)
	}
	after, err := file.Stat()
	if err != nil {
		return LlamaCppModel{}, err
	}
	if !sameStableFileInfo(initial, after) {
		return LlamaCppModel{}, fmt.Errorf("llama.cpp GGUF model changed while it was being verified")
	}
	return LlamaCppModel{
		Name: name, ManifestDigest: "sha256:" + hex.EncodeToString(hash.Sum(nil)),
		Family: LlamaCppFamilyGGUF, Format: LlamaCppArtifactFormat, SizeBytes: initial.Size(),
	}, nil
}

func sameStableFileInfo(left, right os.FileInfo) bool {
	leftLinks, leftOK := fileLinkCount(left)
	rightLinks, rightOK := fileLinkCount(right)
	return left != nil && right != nil && leftOK && rightOK && leftLinks == 1 && rightLinks == 1 &&
		os.SameFile(left, right) && left.Mode() == right.Mode() && left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

func StartLlamaCpp(ctx context.Context, config LlamaCppProcessConfig) (*LlamaCppProcess, error) {
	if config.Runtime.Executable == "" || config.Runtime.ModelsDirectory == "" || config.Runtime.Device == "" {
		return nil, fmt.Errorf("resolved llama.cpp runtime is required")
	}
	if config.ContextLength < 128 || config.ContextLength > 8192 {
		return nil, fmt.Errorf("llama.cpp context length must be between 128 and 8192")
	}
	if err := VerifyLlamaCppModel(ctx, config.Runtime, config.Model); err != nil {
		return nil, fmt.Errorf("verify local llama.cpp model: %s", redactPaths(err.Error(), config.Runtime.ModelsDirectory, config.WorkDirectory))
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
	host, port, found := strings.Cut(address, ":")
	if !found || host != "127.0.0.1" {
		return nil, fmt.Errorf("allocate llama.cpp loopback address")
	}
	modelPath := filepath.Join(config.Runtime.ModelsDirectory, config.Model.Name)
	profile, err := llamaCppSandboxProfile(config.Runtime, modelPath, config.WorkDirectory, config.DeniedPaths)
	if err != nil {
		return nil, err
	}
	profilePath := filepath.Join(config.WorkDirectory, "llama-cpp.sb")
	if err := atomicWrite(profilePath, []byte(profile), 0o600); err != nil {
		return nil, err
	}
	command := exec.Command("/usr/bin/sandbox-exec", "-f", profilePath, config.Runtime.Executable,
		"-lv", "4", "--model", modelPath, "--host", host, "--port", port,
		"--ctx-size", strconv.Itoa(config.ContextLength), "--parallel", "1",
		"--n-gpu-layers", "999", "--device", config.Runtime.Device, "--fit", "off",
		"--cache-ram", "0", "--no-ui")
	command.Dir = config.WorkDirectory
	command.Env = []string{
		"HOME=" + filepath.Join(config.WorkDirectory, "home"), "TMPDIR=" + filepath.Join(config.WorkDirectory, "tmp"),
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8",
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr := &boundedBuffer{limit: maxStderrBytes}
	metal := &llamaCppMetalProbe{device: config.Runtime.Device}
	command.Stdout = io.MultiWriter(stderr, metal)
	command.Stderr = io.MultiWriter(stderr, metal)
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start sandboxed llama-server: %w", err)
	}
	transport := &http.Transport{Proxy: nil}
	process := &LlamaCppProcess{
		cmd: command, client: &http.Client{Transport: transport}, baseURL: "http://" + address,
		model: config.Model, modelPath: modelPath, workDir: config.WorkDirectory,
		done: make(chan struct{}), stderr: stderr, metal: metal,
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
			return nil, fmt.Errorf("record spawned llama-server: %w", err)
		}
	}
	readyTimeout := config.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = 5 * time.Minute
	}
	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := process.waitReady(readyCtx); err != nil {
		_ = process.Stop()
		return nil, err
	}
	if !metal.FullOffload() {
		_ = process.Stop()
		return nil, fmt.Errorf("llama.cpp did not fully offload the pinned model to %s: %s", config.Runtime.Device, process.diagnostics())
	}
	return process, nil
}

func (p *LlamaCppProcess) Generate(ctx context.Context, request GenerateRequest) (GenerateResponse, error) {
	if request.Prompt == "" || len([]byte(request.Prompt)) > 16<<10 {
		return GenerateResponse{}, fmt.Errorf("prompt must contain 1 to 16384 UTF-8 bytes")
	}
	if request.MaxTokens < 1 || request.MaxTokens > 512 {
		return GenerateResponse{}, fmt.Errorf("maxTokens must be between 1 and 512")
	}
	p.requestMu.Lock()
	defer p.requestMu.Unlock()
	payload := struct {
		Messages    []map[string]string `json:"messages"`
		MaxTokens   int                 `json:"max_tokens"`
		Stream      bool                `json:"stream"`
		Temperature float64             `json:"temperature"`
	}{
		Messages:  []map[string]string{{"role": "user", "content": request.Prompt}},
		MaxTokens: request.MaxTokens, Stream: false, Temperature: 0,
	}
	started := time.Now()
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error any `json:"error,omitempty"`
	}
	if err := p.doJSON(ctx, http.MethodPost, "/v1/chat/completions", payload, &response); err != nil {
		return GenerateResponse{}, err
	}
	if response.Error != nil {
		return GenerateResponse{}, fmt.Errorf("llama.cpp returned an API error")
	}
	if len(response.Choices) != 1 || len([]byte(response.Choices[0].Message.Content)) > 1<<20 {
		return GenerateResponse{}, fmt.Errorf("llama.cpp returned an invalid generation response")
	}
	return GenerateResponse{Text: response.Choices[0].Message.Content, ElapsedMillis: time.Since(started).Milliseconds()}, nil
}

func (p *LlamaCppProcess) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *LlamaCppProcess) Alive() bool {
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

func (p *LlamaCppProcess) Stop() error {
	if p == nil {
		return nil
	}
	p.lifeMu.Lock()
	defer p.lifeMu.Unlock()
	p.waitMu.Lock()
	p.stopped = true
	p.waitMu.Unlock()
	if p.client != nil {
		if transport, ok := p.client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	pid := p.PID()
	if pid == 0 {
		return nil
	}
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("kill llama.cpp process group: %w", err)
	}
	select {
	case <-p.done:
		if err := waitProcessGroupGone(pid, time.Second); err != nil {
			return fmt.Errorf("llama.cpp %w", err)
		}
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("llama.cpp process group did not stop")
	}
}

func (p *LlamaCppProcess) Stderr() string {
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

func (p *LlamaCppProcess) WaitError() error {
	if p == nil {
		return nil
	}
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	return p.waitErr
}

func (p *LlamaCppProcess) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/health", nil)
		response, err := p.client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-p.done:
			return fmt.Errorf("llama-server exited before readiness: %w: %s", p.WaitError(), p.diagnostics())
		case <-ctx.Done():
			return fmt.Errorf("wait for llama-server readiness: %w: %s", ctx.Err(), p.diagnostics())
		case <-ticker.C:
		}
	}
}

func (p *LlamaCppProcess) doJSON(ctx context.Context, method, path string, input, output any) error {
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	limited := io.LimitReader(response.Body, 2<<20)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(limited)
		return fmt.Errorf("llama.cpp API returned %s: %s", response.Status, strings.TrimSpace(p.redactPrivatePaths(string(body))))
	}
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode llama.cpp API response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode llama.cpp API response: trailing data")
	}
	return nil
}

func (p *LlamaCppProcess) diagnostics() string {
	text := p.redactPrivatePaths(p.stderr.String())
	lines := strings.Split(text, "\n")
	if len(lines) > 80 {
		lines = lines[len(lines)-80:]
	}
	text = strings.Join(lines, "\n")
	if len(text) > 16<<10 {
		text = text[len(text)-(16<<10):]
	}
	return text
}

func (p *LlamaCppProcess) redactPrivatePaths(text string) string {
	return redactPaths(text, p.modelPath, filepath.Dir(p.modelPath), p.workDir)
}

func (probe *llamaCppMetalProbe) Write(data []byte) (int, error) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	probe.pending = append(probe.pending, data...)
	if len(probe.pending) > 16<<10 {
		probe.pending = probe.pending[len(probe.pending)-(16<<10):]
	}
	text := string(probe.pending)
	if strings.Contains(text, "loaded MTL backend") && strings.Contains(text, "using device "+probe.device) {
		probe.loaded = true
	}
	for _, match := range fullGPUOffloadPattern.FindAllStringSubmatch(text, -1) {
		loaded, firstErr := strconv.Atoi(match[1])
		total, secondErr := strconv.Atoi(match[2])
		if firstErr == nil && secondErr == nil && loaded > 0 && loaded == total {
			probe.full = true
		}
	}
	return len(data), nil
}

func (probe *llamaCppMetalProbe) FullOffload() bool {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	return probe.loaded && probe.full
}

func llamaCppSandboxProfile(runtime LlamaCppRuntime, modelPath, workDirectory string, denied []string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	work, err := canonicalPath(workDirectory)
	if err != nil {
		return "", err
	}
	model, err := canonicalPath(modelPath)
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
	rules.WriteString("(allow file-read*)\n(allow file-write* (literal \"/dev/null\"))\n")
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
	fmt.Fprintf(&rules, "(allow file-read* (literal \"%s\"))\n", escapeSandbox(model))
	fmt.Fprintf(&rules, "(allow file-write* (subpath \"%s\"))\n", escapeSandbox(work))
	if filepath.IsAbs(cache) {
		fmt.Fprintf(&rules, "(allow file-write* (subpath \"%s\"))\n", escapeSandbox(cache))
	}
	rules.WriteString("(allow network-bind network-inbound (local ip \"localhost:*\"))\n")
	rules.WriteString("(allow network-outbound (remote ip \"localhost:*\"))\n")
	return rules.String(), nil
}
