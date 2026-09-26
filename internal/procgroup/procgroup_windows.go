//go:build windows

package procgroup

import (
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func setGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

func killGroup(pid int) error {
	return fmt.Errorf("process group termination is not supported on Windows (pid %d)", pid)
}

func groupGone(pid int) (bool, error) {
	return false, fmt.Errorf("process group inspection is not supported on Windows (pid %d)", pid)
}

func processAlive(pid int) (bool, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false, nil
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		return false, err
	}
	// 259 is STILL_ACTIVE: any other value means the process has exited.
	return code == 259, nil
}
