package syncdir

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSyncAfterARename is the case the package exists for, and the one that
// has to work on every platform: Windows has no directory flush, so an
// implementation that simply called File.Sync would fail here rather than
// return a weaker guarantee.
func TestSyncAfterARename(t *testing.T) {
	dir := t.TempDir()
	temporary := filepath.Join(dir, ".tmp")
	if err := os.WriteFile(temporary, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := Sync(dir); err != nil {
		t.Fatalf("Sync(%s): %v", dir, err)
	}
}

func TestSyncReportsAMissingDirectory(t *testing.T) {
	// Windows has nothing to open, so it cannot report this; everywhere else
	// a missing directory means the rename above never happened.
	err := Sync(filepath.Join(t.TempDir(), "absent"))
	if err == nil && runtimeIsWindows {
		return
	}
	if err == nil {
		t.Error("Sync accepted a directory that does not exist")
	}
}
