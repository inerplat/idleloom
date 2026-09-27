//go:build darwin || linux

package procgroup

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func setGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(pid int) error {
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	return nil
}

func groupGone(pid int) (bool, error) {
	err := unix.Kill(-pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return true, nil
	}
	// EPERM means the group exists but belongs to another user.
	if err != nil && !errors.Is(err, unix.EPERM) {
		return false, fmt.Errorf("inspect process group %d: %w", pid, err)
	}
	return false, nil
}

func processAlive(pid int) (bool, error) {
	err := unix.Kill(pid, 0)
	if err == nil || errors.Is(err, unix.EPERM) {
		return true, nil
	}
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	return false, err
}
