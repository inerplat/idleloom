//go:build !windows

package syncdir

import (
	"errors"
	"fmt"
	"os"
)

// Sync flushes the directory entry at path.
func Sync(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open directory %s: %w", path, err)
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync directory %s: %w", path, err), directory.Close())
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close directory %s: %w", path, err)
	}
	return nil
}
