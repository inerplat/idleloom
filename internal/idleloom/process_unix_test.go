//go:build darwin || linux

package idleloom

import "testing"

func TestRuntimeProcessesStartInTheirOwnSession(t *testing.T) {
	command := detachedCommand("true")
	if command.SysProcAttr == nil || !command.SysProcAttr.Setsid {
		t.Fatal("runtime process is not detached into its own session")
	}
}
