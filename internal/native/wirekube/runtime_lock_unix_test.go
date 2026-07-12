//go:build darwin || linux

package wirekube

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeLockAllowsOnlyOneRunner(t *testing.T) {
	directory := t.TempDir()
	first, err := AcquireRuntimeLock(directory)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := RuntimeLockIsHeld(directory); err != nil || !held {
		t.Fatalf("active runtime lock not detected: held=%v err=%v", held, err)
	}
	if _, err := AcquireRuntimeLock(directory); err == nil {
		t.Fatal("second runtime lock acquisition succeeded")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if held, err := RuntimeLockIsHeld(directory); err != nil || held {
		t.Fatalf("released runtime lock still appears held: held=%v err=%v", held, err)
	}
	second, err := AcquireRuntimeLock(directory)
	if err != nil {
		t.Fatal(err)
	}
	if second.InstanceID == first.InstanceID {
		t.Fatal("runtime instance ID was reused")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeLockRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, runtimeLockFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRuntimeLock(directory); err == nil {
		t.Fatal("AcquireRuntimeLock followed a symlink")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target mode changed to %o", info.Mode().Perm())
	}
}
