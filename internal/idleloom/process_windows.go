//go:build windows

package idleloom

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code Windows reports for a process that has not
// exited yet (STILL_ACTIVE / STATUS_PENDING).
const stillActive = 259

// detachedCommand builds a command that survives idlectl exiting.
// DETACHED_PROCESS drops the console the parent owns, and a new process group
// keeps a console Ctrl-C from propagating into the helper.
func detachedCommand(name string, args ...string) *exec.Cmd {
	command := exec.Command(name, args...)
	command.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	return command
}

// requestProcessTermination asks the process to shut down. Windows has no
// SIGTERM for an arbitrary process, so this terminates it outright; callers
// already treat termination as the escalation step after a graceful protocol
// request (the krunkit REST stop) has been tried.
func requestProcessTermination(process *os.Process) error {
	return process.Kill()
}

// processHasExited reports whether the process is gone. Windows has no zombie
// state, so an exit code other than STILL_ACTIVE is conclusive.
func processHasExited(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return true
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return true
	}
	return code != stillActive
}

// processExecutableName returns the executable backing pid. The second result
// is false when the name cannot be determined, including when the process has
// already gone away.
func processExecutableName(pid int) (string, bool) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	buffer := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return "", false
	}
	name := windows.UTF16ToString(buffer[:size])
	if name == "" {
		return "", false
	}
	return name, true
}

// processStartIdentity returns a stable representation of when pid started.
// Combined with the PID it distinguishes the original process from a later
// one that happened to be assigned the same PID.
func processStartIdentity(pid int) (string, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return "", fmt.Errorf("read process %d start time: %w", pid, err)
	}
	return strconv.FormatInt(creation.Nanoseconds(), 10), nil
}

// processRunsMaintainer reports whether pid is an idlectl certificate
// maintainer for statePath. Windows exposes no supported API for another
// process's command line, so this compares the executable image instead. The
// state path is already matched against the recorded metadata by the caller,
// and the start-time check above carries the PID-reuse guard.
func processRunsMaintainer(pid int, executable string, _ string) bool {
	name, ok := processExecutableName(pid)
	if !ok || executable == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(name), filepath.Clean(executable))
}
