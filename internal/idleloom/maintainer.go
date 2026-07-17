package idleloom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	servingCSRMaintenanceInterval = 30 * time.Second
	maintainerLogMaxBytes         = 8 * 1024 * 1024
)

var errMaintainerRuntimeStopped = errors.New("worker runtime is not running")

type maintainerProcessData struct {
	PID        int    `json:"pid"`
	Nonce      string `json:"nonce"`
	StatePath  string `json:"statePath"`
	Executable string `json:"executable"`
	StartedAt  string `json:"startedAt"`
}

func (a *App) Maintain(ctx context.Context, statePath string) error {
	canonicalState, err := canonicalStatePath(statePath)
	if err != nil {
		return err
	}
	lock, err := acquireMaintainerLock(canonicalState)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	state, err := LoadState(canonicalState)
	if err != nil {
		return err
	}
	if state.Phase == PhaseLocalDeleting || state.Phase == PhaseLocalGone {
		return nil
	}

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find Idleloom executable: %w", err)
	}
	nonce, err := NewNetworkReservationID()
	if err != nil {
		return err
	}
	startedAt, err := processStartIdentity(os.Getpid())
	if err != nil {
		return err
	}
	metadata := maintainerProcessData{
		PID: os.Getpid(), Nonce: nonce, StatePath: canonicalState,
		Executable: executable, StartedAt: startedAt,
	}
	if err := writeMaintainerMetadata(canonicalState, metadata); err != nil {
		return err
	}
	defer removeMaintainerMetadataIfOwned(canonicalState, nonce)

	ticker := time.NewTicker(servingCSRMaintenanceInterval)
	defer ticker.Stop()
	for {
		if err := a.approveServingCSRsOnce(ctx, canonicalState); err != nil {
			if errors.Is(err, errMaintainerRuntimeStopped) {
				return nil
			}
			_, _ = fmt.Fprintf(a.Err, "idleloom maintainer: %v\n", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (a *App) approveServingCSRsOnce(ctx context.Context, statePath string) error {
	lock, err := AcquireStateLock(ctx, statePath)
	if err != nil {
		return err
	}
	state, err := LoadState(statePath)
	closeErr := lock.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if state.Phase == PhaseLocalDeleting || state.Phase == PhaseLocalGone || state.Runtime.GuestIP == "" {
		return errMaintainerRuntimeStopped
	}
	runtimeStatus, err := a.Runtime.Status(ctx, &state.Runtime)
	if err != nil {
		return err
	}
	if runtimeStatus.VM != "running" || runtimeStatus.Network != "running" {
		return errMaintainerRuntimeStopped
	}
	cluster, err := LoadCluster(ctx, state.KubeconfigPath, state.Context)
	if err != nil {
		return err
	}
	if err := ValidateRuntimeNetworkReservation(ctx, cluster.Client, state.NetworkLease, state.NetworkLeaseUID, state.NodeName, state.NetworkReservationID, state.Runtime); err != nil {
		return err
	}
	return ApproveKubeletServingCSR(ctx, cluster.Client, state.NodeName, state.Runtime.GuestIP, state.CreatedAt, false, 0)
}

func startMaintainer(ctx context.Context, statePath string, stderr io.Writer) error {
	canonicalState, err := canonicalStatePath(statePath)
	if err != nil {
		return err
	}
	held, err := maintainerLockHeld(canonicalState)
	if err != nil {
		return err
	}
	if held {
		_, running, err := readAndValidateMaintainer(canonicalState)
		if err != nil {
			return err
		}
		if running {
			return nil
		}
		return fmt.Errorf("certificate maintainer lock is held without a valid owner")
	}
	_ = os.Remove(maintainerMetadataFile(canonicalState))

	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find Idleloom executable for certificate maintenance: %w", err)
	}
	logPath := canonicalState + ".maintainer.log"
	if err := rotateMaintainerLog(logPath, maintainerLogMaxBytes); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open certificate maintainer log: %w", err)
	}
	command := detachedCommand(executable, maintainerCommandArguments(canonicalState)...)
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		return errors.Join(fmt.Errorf("start certificate maintainer: %w", err), logFile.Close())
	}
	pid := command.Process.Pid
	_ = command.Process.Release()
	_ = logFile.Close()
	if err := waitUntil(ctx, 5*time.Second, func() bool {
		metadata, running, _ := readAndValidateMaintainer(canonicalState)
		return running && metadata.PID == pid
	}); err != nil {
		cleanupErr := terminatePID(pid, filepath.Base(executable), 5*time.Second)
		if cleanupErr != nil {
			_, _ = fmt.Fprintf(stderr, "warning: failed to clean up certificate maintainer: %v\n", cleanupErr)
		}
		return fmt.Errorf("wait for certificate maintainer: %w", err)
	}
	return nil
}

func stopMaintainer(statePath string) error {
	canonicalState, err := canonicalStatePath(statePath)
	if err != nil {
		return err
	}
	held, err := maintainerLockHeld(canonicalState)
	if err != nil {
		return err
	}
	if !held {
		_ = os.Remove(maintainerMetadataFile(canonicalState))
		return nil
	}
	metadata, running, err := readAndValidateMaintainer(canonicalState)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("certificate maintainer lock is held without a valid owner")
	}
	process, err := os.FindProcess(metadata.PID)
	if err != nil {
		return fmt.Errorf("find certificate maintainer process %d: %w", metadata.PID, err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal certificate maintainer process %d: %w", metadata.PID, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := waitUntil(ctx, 10*time.Second, func() bool {
		held, _ := maintainerLockHeld(canonicalState)
		return !held
	}); err != nil {
		current, running, validateErr := readAndValidateMaintainer(canonicalState)
		if validateErr != nil {
			return validateErr
		}
		if !running || current.Nonce != metadata.Nonce {
			return fmt.Errorf("certificate maintainer identity changed while stopping")
		}
		if err := terminatePID(metadata.PID, filepath.Base(metadata.Executable), 5*time.Second); err != nil {
			return err
		}
	}
	removeMaintainerMetadataIfOwned(canonicalState, metadata.Nonce)
	return nil
}

type maintainerLock struct {
	file *os.File
}

func acquireMaintainerLock(statePath string) (*maintainerLock, error) {
	file, err := os.OpenFile(maintainerLockFile(statePath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open certificate maintainer lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeErr := file.Close()
		if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
			return nil, errors.Join(fmt.Errorf("certificate maintainer is already running"), closeErr)
		}
		return nil, errors.Join(fmt.Errorf("lock certificate maintainer: %w", err), closeErr)
	}
	return &maintainerLock{file: file}, nil
}

func (l *maintainerLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func maintainerLockHeld(statePath string) (bool, error) {
	file, err := os.OpenFile(maintainerLockFile(statePath), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("open certificate maintainer lock: %w", err)
	}
	defer func() { _ = file.Close() }()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
			return true, nil
		}
		return false, fmt.Errorf("inspect certificate maintainer lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		return false, err
	}
	return false, nil
}

func readAndValidateMaintainer(statePath string) (maintainerProcessData, bool, error) {
	data, err := os.ReadFile(maintainerMetadataFile(statePath))
	if os.IsNotExist(err) {
		return maintainerProcessData{}, false, nil
	}
	if err != nil {
		return maintainerProcessData{}, false, fmt.Errorf("read certificate maintainer metadata: %w", err)
	}
	var metadata maintainerProcessData
	if err := json.Unmarshal(data, &metadata); err != nil {
		return maintainerProcessData{}, false, fmt.Errorf("decode certificate maintainer metadata: %w", err)
	}
	if metadata.PID <= 0 || metadata.Nonce == "" || metadata.StatePath != statePath || metadata.Executable == "" || metadata.StartedAt == "" {
		return metadata, false, fmt.Errorf("certificate maintainer metadata is incomplete")
	}
	process, err := os.FindProcess(metadata.PID)
	if err != nil || process.Signal(syscall.Signal(0)) != nil || processIsZombie(metadata.PID) {
		return metadata, false, nil
	}
	startedAt, err := processStartIdentity(metadata.PID)
	if err != nil || startedAt != metadata.StartedAt {
		return metadata, false, nil
	}
	args, err := exec.Command("ps", "-p", strconv.Itoa(metadata.PID), "-o", "args=").Output()
	if err != nil {
		return metadata, false, nil
	}
	if !maintainerCommandMatches(string(args), statePath) {
		return metadata, false, nil
	}
	return metadata, true, nil
}

func maintainerCommandArguments(statePath string) []string {
	return []string{"worker", "maintain", "--state", statePath}
}

func maintainerCommandMatches(commandLine, statePath string) bool {
	expected := " " + strings.Join(maintainerCommandArguments(statePath), " ")
	return strings.HasSuffix(strings.TrimSpace(commandLine), expected)
}

func processStartIdentity(pid int) (string, error) {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", fmt.Errorf("read process %d start time: %w", pid, err)
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", fmt.Errorf("process %d has no start time", pid)
	}
	return value, nil
}

func writeMaintainerMetadata(statePath string, metadata maintainerProcessData) error {
	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode certificate maintainer metadata: %w", err)
	}
	if err := atomicWriteFile(maintainerMetadataFile(statePath), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write certificate maintainer metadata: %w", err)
	}
	return nil
}

func removeMaintainerMetadataIfOwned(statePath, nonce string) {
	data, err := os.ReadFile(maintainerMetadataFile(statePath))
	if err != nil {
		return
	}
	var metadata maintainerProcessData
	if json.Unmarshal(data, &metadata) == nil && metadata.Nonce == nonce {
		_ = os.Remove(maintainerMetadataFile(statePath))
	}
}

func canonicalStatePath(statePath string) (string, error) {
	resolved, err := resolveStatePath(statePath)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve canonical state path %s: %w", resolved, err)
	}
	return canonical, nil
}

func rotateMaintainerLog(path string, maxBytes int64) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect certificate maintainer log: %w", err)
	}
	if info.Size() <= maxBytes {
		return nil
	}
	backup := path + ".1"
	if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove old certificate maintainer log: %w", err)
	}
	if err := os.Rename(path, backup); err != nil {
		return fmt.Errorf("rotate certificate maintainer log: %w", err)
	}
	return nil
}

func cleanupMaintainerFiles(statePath string) error {
	canonical, err := canonicalStatePath(statePath)
	if err != nil {
		return err
	}
	lock, err := acquireMaintainerLock(canonical)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	for _, path := range []string{
		maintainerMetadataFile(canonical),
		canonical + ".maintainer.log", canonical + ".maintainer.log.1",
	} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove certificate maintainer file %s: %w", path, err)
		}
	}
	return nil
}

func maintainerMetadataFile(statePath string) string {
	return statePath + ".maintainer.json"
}

func maintainerLockFile(statePath string) string {
	return statePath + ".maintainer.lock"
}
