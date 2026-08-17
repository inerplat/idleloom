package devruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// MLXServeConfig starts mlx_lm.server, the OpenAI-compatible HTTP server that
// ships inside the locked mlx-lm wheel, bound directly to the serving address.
// Nothing sits between the client and the runtime's own API.
type MLXServeConfig struct {
	Layout       Layout
	ServeAddress string
	DeniedPaths  []string
	ReadyTimeout time.Duration
	OnSpawn      func(int) error
}

// MLXServeProcess supervises a serving mlx_lm.server. It intentionally does
// not implement in-process generation: serving clients speak to the runtime's
// HTTP API directly, and the runner line protocol remains the batch path.
type MLXServeProcess struct {
	cmd     *exec.Cmd
	client  *http.Client
	baseURL string
	done    chan struct{}
	waitMu  sync.Mutex
	waitErr error
	stopped bool
	stderr  *boundedBuffer
	lifeMu  sync.Mutex
}

func StartMLXServe(ctx context.Context, config MLXServeConfig) (*MLXServeProcess, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("sandboxed MLX server requires macOS")
	}
	host, port, err := validateServeAddress(config.ServeAddress)
	if err != nil {
		return nil, err
	}
	if _, err := Verify(config.Layout); err != nil {
		return nil, err
	}
	profile, err := sandboxProfile(config.Layout, config.DeniedPaths, config.ServeAddress)
	if err != nil {
		return nil, err
	}
	profilePath := filepath.Join(config.Layout.Root, "runtime", "server.sb")
	if err := atomicWrite(profilePath, []byte(profile), 0o600); err != nil {
		return nil, err
	}
	python := filepath.Join(config.Layout.Venv, "bin", "python")
	command := exec.Command("/usr/bin/sandbox-exec", "-f", profilePath, python, "-I", "-B",
		"-m", "mlx_lm.server", "--model", config.Layout.Model, "--host", host, "--port", port)
	command.Dir = config.Layout.Work
	command.Env = runnerEnv(config.Layout.Work)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr := &boundedBuffer{limit: maxStderrBytes}
	command.Stdout = stderr
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start sandboxed MLX server: %w", err)
	}
	process := &MLXServeProcess{
		cmd: command, client: &http.Client{Transport: &http.Transport{Proxy: nil}},
		baseURL: "http://" + net.JoinHostPort(host, port), done: make(chan struct{}), stderr: stderr,
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
			return nil, fmt.Errorf("record spawned MLX server: %w", err)
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
	return process, nil
}

func (p *MLXServeProcess) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/health", nil)
		if err == nil {
			if response, err := p.client.Do(request); err == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("MLX server did not become ready")
		case <-p.done:
			return fmt.Errorf("MLX server exited during startup")
		case <-ticker.C:
		}
	}
}

func (p *MLXServeProcess) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *MLXServeProcess) Alive() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *MLXServeProcess) Stop() error {
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
	if transport, ok := p.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("kill MLX server process group: %w", err)
	}
	select {
	case <-p.done:
		if err := waitProcessGroupGone(pid, time.Second); err != nil {
			return fmt.Errorf("mlx server %w", err)
		}
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("MLX server process group did not stop")
	}
}

// Generate is intentionally unsupported: serving clients call the runtime's
// own HTTP API, and batch inference keeps using the runner line protocol.
func (p *MLXServeProcess) Generate(context.Context, GenerateRequest) (GenerateResponse, error) {
	return GenerateResponse{}, fmt.Errorf("the MLX serving process only answers its own HTTP API")
}

func (p *MLXServeProcess) Stderr() string {
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
	return p.stderr.String()
}

func (p *MLXServeProcess) WaitError() error {
	if p == nil {
		return nil
	}
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	return p.waitErr
}
