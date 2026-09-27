//go:build windows

package syncdir

// Sync is a no-op on Windows.
//
// There is no directory flush to perform: FlushFileBuffers, which File.Sync
// calls, rejects a directory handle, so the Unix implementation does not
// merely provide a weaker guarantee here — it fails with "Access is denied"
// on every write. Durability of the rename itself comes from MoveFileEx,
// which os.Rename uses and which orders the metadata update with the file
// data.
func Sync(string) error {
	return nil
}
