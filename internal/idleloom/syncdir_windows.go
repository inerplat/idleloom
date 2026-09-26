//go:build windows

package idleloom

// syncDir is a no-op on Windows.
//
// There is no directory flush to perform: FlushFileBuffers, which File.Sync
// calls, rejects a directory handle, so the Unix implementation would fail
// every write rather than harden it. Durability of the rename itself comes
// from MoveFileEx, which os.Rename uses and which orders the metadata update
// with the file data.
func syncDir(string) error {
	return nil
}
