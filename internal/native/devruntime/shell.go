package devruntime

import (
	"bytes"
	"context"
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

type ShellConfig struct {
	Layout        Layout
	WorkDirectory string
	Script        string
	Isolation     string
	Network       string
	Timeout       time.Duration
	DeniedPaths   []string
	Output        io.Writer
	OnSpawn       func(int) error
}

type ShellProcess struct {
	cmd     *exec.Cmd
	done    chan struct{}
	mu      sync.Mutex
	waitErr error
	stderr  *boundedBuffer
}

type synchronizedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (writer *synchronizedWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.writer.Write(data)
}

func (writer *synchronizedWriter) Flush() {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if flusher, ok := writer.writer.(interface{ Flush() }); ok {
		flusher.Flush()
	}
}

func StartShell(ctx context.Context, config ShellConfig) (*ShellProcess, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("native shell execution requires macOS")
	}
	if strings.TrimSpace(config.Script) == "" || strings.ContainsRune(config.Script, '\x00') {
		return nil, fmt.Errorf("shell script is required")
	}
	if config.Timeout <= 0 {
		config.Timeout = time.Hour
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	work := config.WorkDirectory
	if work == "" {
		work = config.Layout.Work
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	var command *exec.Cmd
	shell := "/bin/zsh"
	switch config.Isolation {
	case "Host":
		if config.Network != "Outbound" {
			return nil, fmt.Errorf("host shell isolation requires outbound network access")
		}
		command = exec.Command(shell, "-lc", config.Script)
		command.Env = hostShellEnvironment(work)
	case "Sandbox":
		if config.Network != "None" && config.Network != "Outbound" {
			return nil, fmt.Errorf("sandbox shell network must be None or Outbound")
		}
		profile, err := shellSandboxProfile(work, config.DeniedPaths, config.Network == "Outbound")
		if err != nil {
			return nil, err
		}
		profilePath := filepath.Join(config.Layout.Root, "runtime", "shell.sb")
		if err := atomicWrite(profilePath, []byte(profile), 0o600); err != nil {
			return nil, err
		}
		command = exec.Command("/usr/bin/sandbox-exec", "-f", profilePath, shell, "-lc", config.Script)
		command.Env = []string{
			"HOME=/var/empty", "LANG=C.UTF-8", "LC_ALL=C.UTF-8",
			"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR=" + work,
		}
	default:
		return nil, fmt.Errorf("unsupported shell isolation %q", config.Isolation)
	}
	return startCommandProcess(ctx, command, work, config.Timeout, config.Output, config.OnSpawn)
}

func startCommandProcess(ctx context.Context, command *exec.Cmd, work string, timeout time.Duration, output io.Writer, onSpawn func(int) error) (*ShellProcess, error) {
	command.Dir = work
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr := &boundedBuffer{limit: maxStderrBytes}
	if output == nil {
		output = io.Discard
	}
	synchronizedOutput := &synchronizedWriter{writer: output}
	command.Stdout = synchronizedOutput
	command.Stderr = io.MultiWriter(synchronizedOutput, stderr)
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start native shell: %w", err)
	}
	process := &ShellProcess{cmd: command, done: make(chan struct{}), stderr: stderr}
	if onSpawn != nil {
		if err := onSpawn(process.PID()); err != nil {
			_ = unix.Kill(-process.PID(), unix.SIGKILL)
			_ = command.Wait()
			close(process.done)
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		_ = process.Stop()
		return nil, err
	}
	go func() {
		err := command.Wait()
		synchronizedOutput.Flush()
		process.mu.Lock()
		process.waitErr = err
		process.mu.Unlock()
		close(process.done)
	}()
	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-timer.C:
			_ = process.Stop()
		case <-process.done:
		}
	}()
	return process, nil
}

func hostShellEnvironment(workDirectory string) []string {
	home, _ := os.UserHomeDir()
	return []string{
		"HOME=" + home,
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"SHELL=/bin/zsh",
		"TMPDIR=" + workDirectory,
	}
}

func (process *ShellProcess) Alive() bool {
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		return false
	}
	select {
	case <-process.done:
		return false
	default:
		return true
	}
}

func (process *ShellProcess) PID() int {
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		return 0
	}
	return process.cmd.Process.Pid
}

func (process *ShellProcess) Stop() error {
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		return nil
	}
	pid := process.cmd.Process.Pid
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	select {
	case <-process.done:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("native shell process group did not stop")
	}
}

func (process *ShellProcess) Generate(context.Context, GenerateRequest) (GenerateResponse, error) {
	return GenerateResponse{}, fmt.Errorf("generate is unavailable for shell workloads")
}

func (process *ShellProcess) Stderr() string {
	if process == nil || process.stderr == nil {
		return ""
	}
	return process.stderr.String()
}

func (process *ShellProcess) WaitError() error {
	if process == nil {
		return nil
	}
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.waitErr
}

func shellSandboxProfile(workDirectory string, denied []string, outboundNetwork bool) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	work, err := canonicalPath(workDirectory)
	if err != nil {
		return "", err
	}
	denied = append([]string{home, "/Users", "/Volumes", "/Network"}, denied...)
	var rules bytes.Buffer
	rules.WriteString("(version 1)\n(deny default)\n")
	rules.WriteString("(allow process*)\n(allow signal (target self))\n(allow sysctl-read)\n")
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
		fmt.Fprintf(&rules, "(deny file-read* (subpath \"%s\"))\n", escapeSandbox(absolute))
		fmt.Fprintf(&rules, "(deny file-write* (subpath \"%s\"))\n", escapeSandbox(absolute))
	}
	fmt.Fprintf(&rules, "(allow file-write* (subpath \"%s\"))\n", escapeSandbox(work))
	if outboundNetwork {
		rules.WriteString("(allow network-outbound)\n")
	} else {
		rules.WriteString("(deny network*)\n")
	}
	return rules.String(), nil
}
