//go:build darwin || linux

package idleloom

import (
	"errors"
	"fmt"
	"os"
)

// syncDir flushes a directory entry so a rename into it survives a crash.
// Without it the file's contents are durable but the name pointing at them
// may not be.
func syncDir(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open parent directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync parent directory: %w", err), directory.Close())
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close parent directory: %w", err)
	}
	return nil
}
