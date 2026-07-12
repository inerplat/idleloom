package devruntime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	maxProtocolLine = 2 << 20
	maxStderrBytes  = 64 << 10
)

type ProcessConfig struct {
	Layout       Layout
	DeniedPaths  []string
	ReadyTimeout time.Duration
	Nonce        string
	OnSpawn      func(int) error
}

type GenerateRequest struct {
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"maxTokens"`
}

type GenerateResponse struct {
	Text          string `json:"text"`
	ElapsedMillis int64  `json:"elapsedMillis"`
}

type protocolRequest struct {
	ID string `json:"id"`
	GenerateRequest
}

type protocolResponse struct {
	Type          string `json:"type"`
	ID            string `json:"id,omitempty"`
	Text          string `json:"text,omitempty"`
	ElapsedMillis int64  `json:"elapsedMillis,omitempty"`
	Error         string `json:"error,omitempty"`
}

type Process struct {
	requestMu sync.Mutex
	lifeMu    sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	responses chan protocolResponse
	done      chan struct{}
	waitMu    sync.Mutex
	waitErr   error
	stderr    *boundedBuffer
}

func Start(ctx context.Context, config ProcessConfig) (*Process, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("sandboxed MLX runner requires macOS")
	}
	if _, err := Verify(config.Layout); err != nil {
		return nil, err
	}
	if len(config.Nonce) != 64 {
		return nil, fmt.Errorf("runner nonce must contain 64 lowercase hex characters")
	}
	if _, err := hex.DecodeString(config.Nonce); err != nil || strings.ToLower(config.Nonce) != config.Nonce {
		return nil, fmt.Errorf("runner nonce must contain 64 lowercase hex characters")
	}
	profile, err := sandboxProfile(config.Layout, config.DeniedPaths)
	if err != nil {
		return nil, err
	}
	profilePath := filepath.Join(config.Layout.Root, "runtime", "runner.sb")
	if err := atomicWrite(profilePath, []byte(profile), 0o600); err != nil {
		return nil, err
	}
	python := filepath.Join(config.Layout.Venv, "bin", "python")
	cmd := exec.Command("/usr/bin/sandbox-exec", "-f", profilePath, python, "-I", config.Layout.Runner, config.Layout.Model, config.Nonce)
	cmd.Dir = config.Layout.Work
	cmd.Env = runnerEnv(config.Layout.Work)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &boundedBuffer{limit: maxStderrBytes}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start sandboxed MLX runner: %w", err)
	}
	process := &Process{
		cmd:       cmd,
		stdin:     stdin,
		responses: make(chan protocolResponse, 1),
		done:      make(chan struct{}),
		stderr:    stderr,
	}
	go process.readResponses(stdout)
	go func() {
		err := cmd.Wait()
		process.waitMu.Lock()
		process.waitErr = err
		process.waitMu.Unlock()
		close(process.done)
	}()
	if config.OnSpawn != nil {
		if err := config.OnSpawn(process.PID()); err != nil {
			_ = process.Stop()
			return nil, fmt.Errorf("record spawned MLX runner: %w", err)
		}
	}
	readyTimeout := config.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = 5 * time.Minute
	}
	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	select {
	case response, ok := <-process.responses:
		if !ok || response.Type != "ready" {
			process.Stop()
			return nil, fmt.Errorf("MLX runner did not report readiness: %s", process.Stderr())
		}
	case <-process.done:
		return nil, fmt.Errorf("MLX runner exited before readiness: %w: %s", process.WaitError(), process.Stderr())
	case <-readyCtx.Done():
		process.Stop()
		return nil, fmt.Errorf("wait for MLX runner readiness: %w", readyCtx.Err())
	}
	return process, nil
}

func (p *Process) Generate(ctx context.Context, request GenerateRequest) (GenerateResponse, error) {
	if request.Prompt == "" || len([]byte(request.Prompt)) > 16<<10 {
		return GenerateResponse{}, fmt.Errorf("prompt must contain 1 to 16384 UTF-8 bytes")
	}
	if request.MaxTokens < 1 || request.MaxTokens > 512 {
		return GenerateResponse{}, fmt.Errorf("maxTokens must be between 1 and 512")
	}
	p.requestMu.Lock()
	defer p.requestMu.Unlock()
	id, err := randomID()
	if err != nil {
		return GenerateResponse{}, err
	}
	data, err := json.Marshal(protocolRequest{ID: id, GenerateRequest: request})
	if err != nil {
		return GenerateResponse{}, err
	}
	data = append(data, '\n')
	if _, err := p.stdin.Write(data); err != nil {
		return GenerateResponse{}, fmt.Errorf("send request to MLX runner: %w", err)
	}
	select {
	case response, ok := <-p.responses:
		if !ok {
			return GenerateResponse{}, fmt.Errorf("MLX runner output closed: %s", p.Stderr())
		}
		if response.ID != id {
			p.Stop()
			return GenerateResponse{}, fmt.Errorf("MLX runner returned an unexpected request identity")
		}
		if response.Type == "error" {
			return GenerateResponse{}, errors.New(response.Error)
		}
		if response.Type != "result" || len([]byte(response.Text)) > 1<<20 {
			p.Stop()
			return GenerateResponse{}, fmt.Errorf("MLX runner returned an invalid response")
		}
		return GenerateResponse{Text: response.Text, ElapsedMillis: response.ElapsedMillis}, nil
	case <-p.done:
		return GenerateResponse{}, fmt.Errorf("MLX runner exited: %w: %s", p.WaitError(), p.Stderr())
	case <-ctx.Done():
		p.Stop()
		return GenerateResponse{}, ctx.Err()
	}
}

func (p *Process) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *Process) Stop() error {
	if p == nil {
		return nil
	}
	p.lifeMu.Lock()
	defer p.lifeMu.Unlock()
	return p.stopLocked()
}

func (p *Process) Alive() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
	}
	return p.PID() > 0
}

func (p *Process) stopLocked() error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	pid := p.cmd.Process.Pid
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("kill MLX runner process group: %w", err)
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
		return fmt.Errorf("MLX runner process group did not exit within 10 seconds")
	}
	if err := unix.Kill(-pid, 0); err == nil || !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("MLX runner process group %d is still alive", pid)
	}
	return nil
}

func (p *Process) WaitError() error {
	if p == nil {
		return nil
	}
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	return p.waitErr
}

func (p *Process) Stderr() string {
	if p == nil || p.stderr == nil {
		return ""
	}
	return p.stderr.String()
}

func (p *Process) readResponses(reader io.Reader) {
	defer close(p.responses)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxProtocolLine)
	for scanner.Scan() {
		var response protocolResponse
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&response); err != nil {
			p.stderr.Write([]byte("invalid runner response: " + err.Error()))
			return
		}
		p.responses <- response
	}
	if err := scanner.Err(); err != nil {
		p.stderr.Write([]byte("runner output: " + err.Error()))
	}
}

func sandboxProfile(layout Layout, denied []string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if inside(layout.Root, home) {
		return "", fmt.Errorf("development runtime root must be outside the user home for sandbox isolation")
	}
	metalCaches, err := metalCompilerCaches()
	if err != nil {
		return "", err
	}
	for i, cache := range metalCaches {
		if err := os.MkdirAll(cache, 0o700); err != nil {
			return "", fmt.Errorf("create Metal compiler cache: %w", err)
		}
		metalCaches[i], err = canonicalPath(cache)
		if err != nil {
			return "", err
		}
	}
	work, err := canonicalPath(layout.Work)
	if err != nil {
		return "", err
	}
	python, err := canonicalPath(filepath.Join(layout.Venv, "bin", "python"))
	if err != nil {
		return "", err
	}
	pythonApp := "/Library/Frameworks/Python.framework/Versions/3.12/Resources/Python.app/Contents/MacOS/Python"
	denied = append([]string{home}, denied...)
	var rules strings.Builder
	rules.WriteString("(version 1)\n(deny default)\n")
	rules.WriteString("(allow process-fork)\n")
	rules.WriteString("(allow process-exec (literal \"")
	rules.WriteString(escapeSandbox(python))
	rules.WriteString("\") (literal \"")
	rules.WriteString(escapeSandbox(pythonApp))
	rules.WriteString("\"))\n")
	rules.WriteString("(allow signal (target self))\n(allow sysctl-read)\n")
	rules.WriteString("(allow mach-lookup (global-name \"com.apple.MTLCompilerService\"))\n")
	rules.WriteString("(allow iokit-open)\n(allow iokit-get-properties)\n")
	rules.WriteString("(allow file-read*)\n")
	for _, name := range denied {
		if name == "" {
			continue
		}
		absolute, err := canonicalPath(name)
		if err != nil {
			return "", err
		}
		rules.WriteString("(deny file-read* (subpath \"")
		rules.WriteString(escapeSandbox(absolute))
		rules.WriteString("\"))\n")
		rules.WriteString("(deny file-write* (subpath \"")
		rules.WriteString(escapeSandbox(absolute))
		rules.WriteString("\"))\n")
	}
	rules.WriteString("(allow file-write* (subpath \"")
	rules.WriteString(escapeSandbox(work))
	rules.WriteString("\"))\n")
	for _, cache := range metalCaches {
		rules.WriteString("(allow file-write* (subpath \"")
		rules.WriteString(escapeSandbox(cache))
		rules.WriteString("\"))\n")
		rules.WriteString("(allow file-issue-extension (require-all (subpath \"")
		rules.WriteString(escapeSandbox(cache))
		rules.WriteString("\") (extension-class \"com.apple.app-sandbox.read\" \"com.apple.app-sandbox.read-write\")))\n")
	}
	pythonResources := "/Library/Frameworks/Python.framework/Versions/3.12/Resources/Python.app/Contents/Resources"
	rules.WriteString("(allow file-issue-extension (require-all (subpath \"")
	rules.WriteString(escapeSandbox(pythonResources))
	rules.WriteString("\") (extension-class \"com.apple.app-sandbox.read\")))\n")
	rules.WriteString("(deny network*)\n")
	return rules.String(), nil
}

func canonicalPath(name string) (string, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	if os.IsNotExist(err) {
		return absolute, nil
	}
	return "", err
}

func metalCompilerCaches() ([]string, error) {
	output, err := exec.Command("/usr/bin/getconf", "DARWIN_USER_CACHE_DIR").Output()
	if err != nil {
		return nil, fmt.Errorf("locate Darwin user cache: %w", err)
	}
	root := strings.TrimSpace(string(output))
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("Darwin user cache is not absolute")
	}
	pythonCache := filepath.Join(root, "org.python.python")
	return []string{
		filepath.Join(pythonCache, "com.apple.metal"),
		filepath.Join(pythonCache, "com.apple.metalfe"),
		filepath.Join(pythonCache, "com.apple.gpuarchiver"),
	}, nil
}

func runnerEnv(work string) []string {
	return []string{
		"HOME=/var/empty",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"PYTHONHASHSEED=0",
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTHONNOUSERSITE=1",
		"HF_HUB_OFFLINE=1",
		"TRANSFORMERS_OFFLINE=1",
		"TOKENIZERS_PARALLELISM=false",
		"TMPDIR=" + work,
	}
}

func inside(name, directory string) bool {
	name, _ = filepath.Abs(name)
	directory, _ = filepath.Abs(directory)
	relative, err := filepath.Rel(directory, name)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func escapeSandbox(value string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(value)
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

type boundedBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		b.data = append(b.data, data...)
	}
	return written, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(bytes.TrimSpace(b.data))
}
