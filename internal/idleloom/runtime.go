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

// RunWithInput is Run with the command's standard input attached. The
// in-place worker backends stream bundles and scripts this way so nothing has
// to be staged on the guest filesystem first.
func (ExecRunner) RunWithInput(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = stdin
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

// RuntimeKind identifies the backend that owns a worker.
type RuntimeKind string

const (
	// RuntimeKrunkit provisions an ARM64 Linux VM on Apple Silicon.
	RuntimeKrunkit RuntimeKind = "krunkit"
	// RuntimeLinux enrolls the Linux host idlectl runs on.
	RuntimeLinux RuntimeKind = "linux"
	// RuntimeWSL2 enrolls a WSL2 distribution on a Windows host.
	RuntimeWSL2 RuntimeKind = "wsl2"
)

// ProvisionsVM reports whether the backend builds its own virtual machine.
// Those backends own a private subnet and need one reserved cluster-wide so
// two workers cannot advertise the same node address. The in-place backends
// take their address from the WireKube mesh instead, which is already unique
// per node name.
func (k RuntimeKind) ProvisionsVM() bool {
	return k == RuntimeKrunkit
}

type WorkerRuntime interface {
	// Backend identifies which runtime implementation this is. Enrollment
	// branches on it where a VM-provisioning backend and an in-place backend
	// genuinely differ, such as address allocation.
	Backend() RuntimeKind
	// GuestArch is the CPU architecture of the worker's Linux environment,
	// which selects the kubelet build to install.
	GuestArch() string
	Preflight(context.Context) error
	Plan(context.Context, RuntimeConfig) (RuntimeState, error)
	Create(context.Context, *RuntimeState) error
	Validate(context.Context, RuntimeState) error
	Start(context.Context, *RuntimeState) error
	WaitReady(context.Context, RuntimeState, time.Duration) error
	Stop(context.Context, RuntimeState) error
	Delete(context.Context, RuntimeState) error
	InstallBundle(context.Context, RuntimeState, string) error
	LoadImage(context.Context, RuntimeState, string) error
	RemoveBootstrapIdentity(context.Context, RuntimeState) error
	Status(context.Context, *RuntimeState) (WorkerStatus, error)
}
