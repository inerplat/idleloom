//go:build darwin || linux

package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// VerifyExtractedTree checks the actual staging tree without following links.
// OCI signature verification must happen before this function is called.
func (p Policy) VerifyExtractedTree(root string, manifest Manifest) error {
	if err := p.ValidateDeclaration(manifest); err != nil {
		return err
	}
	rootDescriptor, rootStat, err := openPrivateStagingRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(rootDescriptor) }()
	expectedFiles := make(map[string]File, len(manifest.Files))
	expectedDirs := make(map[string]struct{})
	for _, file := range manifest.Files {
		expectedFiles[file.Path] = file
		for directory := path.Dir(file.Path); directory != "."; directory = path.Dir(directory) {
			expectedDirs[directory] = struct{}{}
			if directory == path.Dir(directory) {
				break
			}
		}
	}
	seen := make(map[string]struct{}, len(expectedFiles))
	err = filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			if !entry.IsDir() {
				return fmt.Errorf("artifact root is not a directory")
			}
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if _, expected := expectedDirs[relative]; !expected {
				return fmt.Errorf("artifact contains undeclared directory %q", relative)
			}
			return nil
		}
		expected, found := expectedFiles[relative]
		if !found {
			return fmt.Errorf("artifact contains undeclared file %q", relative)
		}
		if err := verifyRegularFileAt(rootDescriptor, relative, expected); err != nil {
			return fmt.Errorf("verify artifact file %q: %w", relative, err)
		}
		seen[relative] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	if err := verifyStagingRootIdentity(root, rootStat); err != nil {
		return err
	}
	for file := range expectedFiles {
		if _, found := seen[file]; !found {
			return fmt.Errorf("artifact is missing declared file %q", file)
		}
	}
	return nil
}

func openPrivateStagingRoot(root string) (int, unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(root, &stat); err != nil {
		return -1, stat, fmt.Errorf("inspect artifact staging root: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return -1, stat, fmt.Errorf("artifact staging root is not a directory")
	}
	if uint32(stat.Mode)&0o7777 != 0o700 {
		return -1, stat, fmt.Errorf("artifact staging root mode must be 0700")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return -1, stat, fmt.Errorf("artifact staging root is owned by UID %d, want %d", stat.Uid, os.Geteuid())
	}
	descriptor, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, stat, fmt.Errorf("open artifact staging root: %w", err)
	}
	return descriptor, stat, nil
}

func verifyStagingRootIdentity(root string, expected unix.Stat_t) error {
	var current unix.Stat_t
	if err := unix.Lstat(root, &current); err != nil {
		return fmt.Errorf("reinspect artifact staging root: %w", err)
	}
	if current.Dev != expected.Dev || current.Ino != expected.Ino {
		return fmt.Errorf("artifact staging root changed during verification")
	}
	return nil
}

func verifyRegularFileAt(rootDescriptor int, relative string, expected File) error {
	components := strings.Split(relative, "/")
	directory, err := unix.Dup(rootDescriptor)
	if err != nil {
		return err
	}
	for _, component := range components[:len(components)-1] {
		next, err := unix.Openat(directory, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		closeErr := unix.Close(directory)
		if err != nil {
			return errors.Join(err, closeErr)
		}
		if closeErr != nil {
			_ = unix.Close(next)
			return closeErr
		}
		directory = next
	}
	descriptor, err := unix.Openat(directory, components[len(components)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	closeErr := unix.Close(directory)
	if err != nil {
		return errors.Join(err, closeErr)
	}
	if closeErr != nil {
		_ = unix.Close(descriptor)
		return closeErr
	}
	file := os.NewFile(uintptr(descriptor), relative)
	if file == nil {
		_ = unix.Close(descriptor)
		return fmt.Errorf("open file descriptor")
	}
	defer func() { _ = file.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstat(descriptor, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("not a regular file")
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("hard-linked files are not allowed")
	}
	if stat.Size != expected.SizeBytes {
		return fmt.Errorf("size %d does not match manifest size %d", stat.Size, expected.SizeBytes)
	}
	mode := uint32(stat.Mode) & 0o7777
	if mode != 0o600 && mode != 0o644 {
		return fmt.Errorf("mode %#o is not allowed", mode)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if digest != expected.SHA256 {
		return fmt.Errorf("digest %s does not match manifest digest %s", digest, expected.SHA256)
	}
	return nil
}
