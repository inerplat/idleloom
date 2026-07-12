//go:build darwin || linux

package wirekube

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const runtimeLockFileName = "wirekube-runtime.lock"

type RuntimeLock struct {
	file       *os.File
	InstanceID string
}

func AcquireRuntimeLock(directory string) (*RuntimeLock, error) {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return nil, err
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() {
		return nil, fmt.Errorf("runtime directory must be a real directory")
	}
	directoryFD, err := unix.Open(directory, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	var directoryStat unix.Stat_t
	if err := unix.Fstat(directoryFD, &directoryStat); err != nil {
		_ = unix.Close(directoryFD)
		return nil, err
	}
	_ = unix.Close(directoryFD)
	if directoryStat.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("runtime directory must be owned by the connectivity service user")
	}
	if directoryInfo.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("runtime directory must not be group- or world-writable")
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, runtimeLockFileName)
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open runtime lock")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("runtime lock path must be a regular file")
	}
	var fileStat unix.Stat_t
	if err := unix.Fstat(fd, &fileStat); err != nil || fileStat.Uid != uint32(os.Geteuid()) {
		file.Close()
		return nil, fmt.Errorf("runtime lock must be owned by the connectivity service user")
	}
	if err := file.Chmod(0o644); err != nil {
		file.Close()
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another connectivity service instance is active: %w", err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		file.Close()
		return nil, err
	}
	return &RuntimeLock{file: file, InstanceID: hex.EncodeToString(nonce[:])}, nil
}

func RuntimeLockIsHeld(directory string) (bool, error) {
	path := filepath.Join(directory, runtimeLockFileName)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if err := unix.Flock(fd, unix.LOCK_UN); err != nil {
		return false, err
	}
	return false, nil
}

func (lock *RuntimeLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	err := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	if err != nil {
		return err
	}
	return closeErr
}
