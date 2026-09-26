//go:build !darwin && !linux

package wirekube

import (
	"fmt"
	"os"
)

const runtimeLockFileName = "wirekube-runtime.lock"

type RuntimeLock struct {
	file       *os.File
	InstanceID string
}

// AcquireRuntimeLock is not available off macOS. The WireKube connected leaf
// runs as a privileged macOS service, and the Unix implementation of this lock
// depends on openat/O_NOFOLLOW and file ownership checks to make that safe.
// startPlatformTunnel refuses the same way, so this is never reached in
// practice; it exists so idlectl builds for the Linux Worker commands.
func AcquireRuntimeLock(string) (*RuntimeLock, error) {
	return nil, fmt.Errorf("the WireKube connected leaf currently requires macOS")
}

// RuntimeLockIsHeld reports that no connectivity service is running, which is
// always true on a platform that cannot start one.
func RuntimeLockIsHeld(string) (bool, error) {
	return false, nil
}

func (lock *RuntimeLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	err := lock.file.Close()
	lock.file = nil
	return err
}
