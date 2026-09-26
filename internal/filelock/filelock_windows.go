//go:build windows

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockLength is the byte range LockFileEx operates on. Locking a single byte
// is enough for an advisory whole-file lock as long as every participant
// agrees on the range, and it avoids depending on the file's size.
const lockLength = 1

func tryLock(file *os.File) (bool, error) {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, lockLength, 0, &overlapped,
	)
	if err == nil {
		return true, nil
	}
	// LockFileEx reports contention as ERROR_LOCK_VIOLATION; ERROR_IO_PENDING
	// cannot occur with LOCKFILE_FAIL_IMMEDIATELY but is treated as contention
	// defensively rather than as an unexpected failure.
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return false, nil
	}
	return false, err
}

func unlock(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, lockLength, 0, &overlapped)
}
