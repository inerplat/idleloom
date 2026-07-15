package devruntime

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

type TrainingConfig struct {
	Layout        Layout
	WorkDirectory string
	Source        string
	Network       string
	Timeout       time.Duration
	DeniedPaths   []string
	Parameters    map[string]string
	RunID         string
	Experiment    string
	Attempt       int32
	Output        io.Writer
	OnSpawn       func(int) error
}

func StartTraining(ctx context.Context, config TrainingConfig) (*ShellProcess, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return nil, fmt.Errorf("native MLX training requires macOS on Apple Silicon")
	}
	if config.Source == "" || strings.ContainsRune(config.Source, '\x00') || len([]byte(config.Source)) > 64<<10 {
		return nil, fmt.Errorf("training source must contain 1 to 65536 UTF-8 bytes without NUL characters")
	}
	if config.Network != "None" && config.Network != "Outbound" {
		return nil, fmt.Errorf("training network must be None or Outbound")
	}
	if config.Timeout <= 0 {
		config.Timeout = time.Hour
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := VerifyRuntime(config.Layout); err != nil {
		return nil, fmt.Errorf("verify locked MLX training runtime: %w", err)
	}
	work := config.WorkDirectory
	if work == "" {
		work = config.Layout.Work
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	sourcePath := filepath.Join(work, "train.py")
	if err := atomicWrite(sourcePath, []byte(config.Source), 0o500); err != nil {
		return nil, fmt.Errorf("stage training source: %w", err)
	}
	profile, err := shellSandboxProfile(work, config.DeniedPaths, config.Network == "Outbound")
	if err != nil {
		return nil, err
	}
	profilePath := filepath.Join(config.Layout.Root, "runtime", "training.sb")
	if err := atomicWrite(profilePath, []byte(profile), 0o600); err != nil {
		return nil, err
	}
	python := filepath.Join(config.Layout.Venv, "bin", "python")
	command := exec.Command("/usr/bin/sandbox-exec", "-f", profilePath, python, "-I", "-B", sourcePath)
	command.Env = []string{
		"HOME=/var/empty", "LANG=C.UTF-8", "LC_ALL=C.UTF-8",
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "TMPDIR=" + work,
		"PYTHONDONTWRITEBYTECODE=1", "PYTHONNOUSERSITE=1",
		"IDLELOOM_RUN_ID=" + config.RunID,
		"IDLELOOM_EXPERIMENT=" + config.Experiment,
		"IDLELOOM_ATTEMPT=" + strconv.FormatInt(int64(config.Attempt), 10),
	}
	names := make([]string, 0, len(config.Parameters))
	for name := range config.Parameters {
		if !runEnvironmentName(name) || strings.HasPrefix(name, "IDLELOOM_") || strings.ContainsRune(config.Parameters[name], '\x00') {
			return nil, fmt.Errorf("invalid training parameter %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		command.Env = append(command.Env, name+"="+config.Parameters[name])
	}
	return startCommandProcess(ctx, command, work, config.Timeout, config.Output, config.OnSpawn)
}

func runEnvironmentName(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] < 'A' || value[0] > 'Z' {
		return false
	}
	for _, character := range value[1:] {
		if character < 'A' || character > 'Z' {
			if character < '0' || character > '9' {
				if character != '_' {
					return false
				}
			}
		}
	}
	return true
}
