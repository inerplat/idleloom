package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/api/resource"
)

type Platform interface {
	KrunkitRunning(context.Context) (bool, error)
	AllocatableMemory(context.Context) (resource.Quantity, error)
	ProcessStartToken(int) (string, error)
	ProcessAlive(int) (bool, error)
	FindRunnerPIDs(context.Context, string, string) ([]int, error)
	KillProcessGroupAndWait(context.Context, int) error
}

type DarwinPlatform struct{}

func (DarwinPlatform) KrunkitRunning(ctx context.Context) (bool, error) {
	err := exec.CommandContext(ctx, "pgrep", "-x", "krunkit").Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("inspect krunkit process state: %w", err)
}

func (DarwinPlatform) AllocatableMemory(ctx context.Context) (resource.Quantity, error) {
	output, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("read unified memory capacity: %w", err)
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil || bytes <= 0 {
		return resource.Quantity{}, fmt.Errorf("invalid unified memory capacity %q", strings.TrimSpace(string(output)))
	}
	return *resource.NewQuantity(bytes*7/10, resource.BinarySI), nil
}

func (DarwinPlatform) ProcessStartToken(pid int) (string, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", fmt.Errorf("read process start token: %w", err)
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", fmt.Errorf("process %d has no start token", pid)
	}
	sum := sha256.Sum256([]byte(strconv.Itoa(pid) + "\x00" + value))
	return hex.EncodeToString(sum[:]), nil
}

func (DarwinPlatform) ProcessAlive(pid int) (bool, error) {
	err := unix.Kill(pid, 0)
	if err == nil || errors.Is(err, unix.EPERM) {
		return true, nil
	}
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	return false, err
}

func (DarwinPlatform) FindRunnerPIDs(ctx context.Context, runner, nonce string) ([]int, error) {
	output, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.Contains(line, runner) || !strings.Contains(line, nonce) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (DarwinPlatform) KillProcessGroupAndWait(ctx context.Context, pid int) error {
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := unix.Kill(-pid, 0); errors.Is(err, unix.ESRCH) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("process group %d did not exit", pid)
		case <-ticker.C:
		}
	}
}
