//go:build windows

package idleloom

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestRuntimeProcessesStartDetachedFromTheConsole(t *testing.T) {
	command := detachedCommand("cmd.exe")
	if command.SysProcAttr == nil {
		t.Fatal("runtime process has no creation flags")
	}
	flags := command.SysProcAttr.CreationFlags
	if flags&windows.DETACHED_PROCESS == 0 {
		t.Error("runtime process is not detached from the console")
	}
	if flags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Error("runtime process does not start its own process group")
	}
}
