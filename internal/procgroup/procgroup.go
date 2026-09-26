// Package procgroup manages child process groups portably.
//
// Idleloom spawns model runners and shells that fork their own children; when
// a run is cancelled the whole tree must die, not just the process idlectl
// started. On Unix that means putting the child in its own process group and
// signalling the negated PID. The Windows implementations exist so idlectl
// builds there for the Linux Worker commands — the callers are all Native
// Metal paths that already refuse to run off macOS.
package procgroup

import "os/exec"

// SetGroup makes the command the leader of a new process group so its whole
// tree can be signalled at once.
func SetGroup(command *exec.Cmd) {
	setGroup(command)
}

// Kill force-terminates the process group led by pid. A group that has
// already exited is not an error.
func Kill(pid int) error {
	return killGroup(pid)
}

// Gone reports whether the process group led by pid has fully exited.
func Gone(pid int) (bool, error) {
	return groupGone(pid)
}

// Alive reports whether the single process pid exists. A process owned by
// another user counts as alive.
func Alive(pid int) (bool, error) {
	return processAlive(pid)
}
