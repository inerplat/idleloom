// Package filelock provides advisory, exclusive, non-blocking file locks that
// behave the same on Unix and Windows.
//
// Idleloom serialises concurrent idlectl invocations with lock files: the
// worker state file, the runtime directory, and the certificate maintainer
// each have one. Unix builds use flock(2); Windows builds use LockFileEx.
// Both release the lock when the file is closed, so Unlock is an explicit
// convenience rather than a correctness requirement.
package filelock

import "os"

// TryLock takes an exclusive lock on file without blocking.
//
// It reports ok=false with a nil error when another process already holds the
// lock, which callers treat as contention rather than failure. A non-nil error
// means the lock could not be evaluated at all.
func TryLock(file *os.File) (ok bool, err error) {
	return tryLock(file)
}

// Unlock releases a lock previously taken by TryLock. Closing the file also
// releases the lock, so Unlock is safe to skip on shutdown paths.
func Unlock(file *os.File) error {
	return unlock(file)
}
