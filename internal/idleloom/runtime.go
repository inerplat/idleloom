package idleloom

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

type CommandRunner interface {
	Run(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = stdout
	command.Stderr = stderr
	return command.Run()
}

func (ExecRunner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return output, fmt.Errorf("%s: %w: %s", strings.Join(append([]string{name}, args...), " "), err, stderr.String())
	}
	return output, nil
}

type RuntimeConfig struct {
	NodeName   string
	CPUs       int
	MemoryMB   int
	DiskMB     int
	RuntimeDir string
	Network    RuntimeNetwork
}

type RuntimeNetwork struct {
	Subnet    string
	GatewayIP string
	GuestIP   string
	HostIP    string
	MAC       string
}

type WorkerStatus struct {
	VM      string
	Network string
}

type WorkerRuntime interface {
	Preflight(context.Context) error
	Plan(context.Context, RuntimeConfig) (RuntimeState, error)
	Create(context.Context, *RuntimeState) error
	Validate(context.Context, RuntimeState) error
	Start(context.Context, *RuntimeState) error
	WaitReady(context.Context, RuntimeState, time.Duration) error
	Stop(context.Context, RuntimeState) error
	Delete(context.Context, RuntimeState) error
	InstallBundle(context.Context, RuntimeState, string) error
	RemoveBootstrapIdentity(context.Context, RuntimeState) error
	Status(context.Context, *RuntimeState) (WorkerStatus, error)
}
