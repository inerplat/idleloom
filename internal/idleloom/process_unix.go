//go:build darwin || linux

package idleloom

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// detachedCommand builds a command that survives idlectl exiting. A new
// session detaches it from the controlling terminal so a Ctrl-C in the
// launching shell does not reach it.
func detachedCommand(name string, args ...string) *exec.Cmd {
	command := exec.Command(name, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return command
}

// requestProcessTermination asks the process to shut down cleanly.
func requestProcessTermination(process *os.Process) error {
	return process.Signal(syscall.SIGTERM)
}

// processHasExited reports whether the process is gone. A zombie counts as
// exited: it has stopped running and only awaits reaping by its parent, which
// is never idlectl for a detached helper.
func processHasExited(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	if process.Signal(syscall.Signal(0)) != nil {
		return true
	}
	return processIsZombie(pid)
}

func processIsZombie(pid int) bool {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "state=").Output()
	if err != nil {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(string(output)), "Z")
}

// processExecutableName returns the executable backing pid. The second result
// is false when the name cannot be determined, including when the process has
// already gone away.
func processExecutableName(pid int) (string, bool) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return "", false
	}
	name := strings.TrimSpace(string(output))
	if name == "" {
		return "", false
	}
	return name, true
}

// processStartIdentity returns a stable representation of when pid started.
// Combined with the PID it distinguishes the original process from a later
// one that happened to be assigned the same PID.
func processStartIdentity(pid int) (string, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", fmt.Errorf("read process %d start time: %w", pid, err)
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", fmt.Errorf("process %d has no start time", pid)
	}
	return value, nil
}

// processRunsMaintainer reports whether pid is an idlectl certificate
// maintainer for statePath. It reads the full command line, which names both
// the subcommand and the state path.
func processRunsMaintainer(pid int, _ string, statePath string) bool {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.TrimSpace(string(output)), " maintain --state "+statePath)
}
