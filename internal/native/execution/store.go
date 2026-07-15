package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	SchemaVersionV1 = 1
	maxStateBytes   = 64 << 10
)

var (
	ErrExecutionBusy = errors.New("another native execution is recorded")
	ErrStoreLocked   = errors.New("native execution store is locked by another agent")
	ErrStoreClosed   = errors.New("native execution store is closed")
)

type Record struct {
	SchemaVersion      int    `json:"schemaVersion"`
	WorkloadUID        string `json:"workloadUID"`
	WorkloadGeneration int64  `json:"workloadGeneration"`
	AssignmentUID      string `json:"assignmentUID"`
	ExecutionID        string `json:"executionID"`
	FencingEpoch       int64  `json:"fencingEpoch"`
	PID                int    `json:"pid,omitempty"`
	ProcessStartToken  string `json:"processStartToken,omitempty"`
	Executable         string `json:"executable,omitempty"`
	RuntimeVersion     string `json:"runtimeVersion,omitempty"`
	Nonce              string `json:"nonce,omitempty"`
	Completed          bool   `json:"completed,omitempty"`
	ExitError          string `json:"exitError,omitempty"`
}

type Store struct {
	mu     sync.Mutex
	path   string
	lock   *os.File
	record *Record
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create native execution state directory: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open native execution lock: %w", err)
	}
	if err := lock.Chmod(0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("set native execution lock permissions: %w", err), lock.Close())
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		closeErr := lock.Close()
		if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
			return nil, errors.Join(ErrStoreLocked, closeErr)
		}
		return nil, errors.Join(fmt.Errorf("lock native execution store: %w", err), closeErr)
	}
	store := &Store{path: path, lock: lock}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, errors.Join(fmt.Errorf("read native execution state: %w", err), store.Close())
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("stat native execution state: %w", err), store.Close())
	}
	if info.Size() > maxStateBytes {
		return nil, errors.Join(fmt.Errorf("native execution state is %d bytes; maximum is %d", info.Size(), maxStateBytes), store.Close())
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("read native execution state: %w", err), store.Close())
	}
	var record Record
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, errors.Join(fmt.Errorf("decode native execution state: %w", err), store.Close())
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.Join(fmt.Errorf("decode native execution state: trailing data"), store.Close())
	}
	if err := validateRecord(record); err != nil {
		return nil, errors.Join(fmt.Errorf("validate native execution state: %w", err), store.Close())
	}
	store.record = &record
	return store, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	unlockErr := unix.Flock(int(s.lock.Fd()), unix.LOCK_UN)
	closeErr := s.lock.Close()
	s.lock = nil
	return errors.Join(unlockErr, closeErr)
}

func (s *Store) Current() *Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record == nil {
		return nil
	}
	copy := *s.record
	return &copy
}

func (s *Store) Begin(record Record) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return ErrStoreClosed
	}
	if s.record != nil {
		if samePlannedIdentity(*s.record, record) {
			return nil
		}
		return fmt.Errorf("%w: assignment=%s execution=%s epoch=%d", ErrExecutionBusy, s.record.AssignmentUID, s.record.ExecutionID, s.record.FencingEpoch)
	}
	if record.PID != 0 || record.ProcessStartToken != "" {
		return fmt.Errorf("begin requires a planned process identity without PID or start token")
	}
	if err := persist(s.path, record); err != nil {
		return err
	}
	s.record = &record
	return nil
}

func (s *Store) UpdateProcess(expected Record, process Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return ErrStoreClosed
	}
	if s.record == nil || !sameExecution(*s.record, expected) {
		return fmt.Errorf("native execution changed before process state was recorded")
	}
	if s.record.Completed {
		return fmt.Errorf("native execution is already completed")
	}
	if !sameExecution(expected, process) {
		return fmt.Errorf("process state belongs to a different native execution")
	}
	if s.record.Executable != process.Executable || s.record.RuntimeVersion != process.RuntimeVersion || s.record.Nonce != process.Nonce {
		return fmt.Errorf("process identity does not match the planned executable, runtime, and nonce")
	}
	if process.PID <= 0 || process.ProcessStartToken == "" || process.Executable == "" || process.RuntimeVersion == "" || process.Nonce == "" {
		return fmt.Errorf("complete process identity is required")
	}
	if processIdentityComplete(*s.record) {
		if sameProcess(*s.record, process) {
			return nil
		}
		return fmt.Errorf("refusing to replace an already recorded process identity")
	}
	if err := persist(s.path, process); err != nil {
		return err
	}
	s.record = &process
	return nil
}

func (s *Store) Complete(expected Record, exitErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return ErrStoreClosed
	}
	if s.record == nil || !sameExecution(*s.record, expected) {
		return fmt.Errorf("native execution changed before completion was recorded")
	}
	if !processIdentityComplete(*s.record) {
		return fmt.Errorf("cannot complete an execution without a recorded process identity")
	}
	if s.record.Completed {
		expectedError := ""
		if exitErr != nil {
			expectedError = exitErr.Error()
			if len(expectedError) > 1024 {
				expectedError = expectedError[:1024]
			}
		}
		if s.record.ExitError == expectedError {
			return nil
		}
		return fmt.Errorf("native execution completion is already recorded")
	}
	completed := *s.record
	completed.Completed = true
	completed.ExitError = ""
	if exitErr != nil {
		completed.ExitError = exitErr.Error()
		if len(completed.ExitError) > 1024 {
			completed.ExitError = completed.ExitError[:1024]
		}
	}
	if err := persist(s.path, completed); err != nil {
		return err
	}
	s.record = &completed
	return nil
}

func (s *Store) CanAdopt(expected Record, observed Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return ErrStoreClosed
	}
	if s.record == nil || !sameExecution(*s.record, expected) {
		return fmt.Errorf("native execution journal does not match the assignment")
	}
	if !processIdentityComplete(*s.record) || !processIdentityComplete(observed) || !sameProcess(*s.record, observed) {
		return fmt.Errorf("observed process identity does not match the durable journal")
	}
	return nil
}

func (s *Store) Clear(expected Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return ErrStoreClosed
	}
	if s.record == nil {
		return nil
	}
	if !sameExecution(*s.record, expected) {
		return fmt.Errorf("refusing to clear a different native execution")
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove native execution state: %w", err)
	}
	if err := syncDirectory(filepath.Dir(s.path)); err != nil {
		return err
	}
	s.record = nil
	return nil
}

func validateRecord(record Record) error {
	if record.SchemaVersion != SchemaVersionV1 {
		return fmt.Errorf("unsupported native execution state schema %d", record.SchemaVersion)
	}
	if record.WorkloadUID == "" || record.WorkloadGeneration < 1 || record.AssignmentUID == "" || record.ExecutionID == "" || record.FencingEpoch < 1 {
		return fmt.Errorf("workload UID/generation, assignment UID, execution ID, and positive fencing epoch are required")
	}
	if record.Executable == "" || record.RuntimeVersion == "" || record.Nonce == "" {
		return fmt.Errorf("planned executable, runtime version, and nonce are required")
	}
	if (record.PID == 0) != (record.ProcessStartToken == "") {
		return fmt.Errorf("the PID and process start token must be recorded together")
	}
	if record.Completed && !processIdentityComplete(record) {
		return fmt.Errorf("completed execution requires a complete process identity")
	}
	if !record.Completed && record.ExitError != "" {
		return fmt.Errorf("exit error requires a completed execution")
	}
	return nil
}

func sameExecution(left, right Record) bool {
	return left.WorkloadUID == right.WorkloadUID &&
		left.WorkloadGeneration == right.WorkloadGeneration &&
		left.AssignmentUID == right.AssignmentUID &&
		left.ExecutionID == right.ExecutionID &&
		left.FencingEpoch == right.FencingEpoch
}

func samePlannedIdentity(left, right Record) bool {
	return sameExecution(left, right) &&
		left.Executable == right.Executable &&
		left.RuntimeVersion == right.RuntimeVersion &&
		left.Nonce == right.Nonce
}

func processIdentityComplete(record Record) bool {
	return record.PID > 0 && record.ProcessStartToken != "" && record.Executable != "" && record.RuntimeVersion != "" && record.Nonce != ""
}

func sameProcess(left, right Record) bool {
	return sameExecution(left, right) &&
		left.PID == right.PID &&
		left.ProcessStartToken == right.ProcessStartToken &&
		left.Executable == right.Executable &&
		left.RuntimeVersion == right.RuntimeVersion &&
		left.Nonce == right.Nonce
}

func persist(path string, record Record) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create native execution state directory: %w", err)
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode native execution state: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".native-execution-*")
	if err != nil {
		return fmt.Errorf("create temporary native execution state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.Join(fmt.Errorf("set native execution state permissions: %w", err), temporary.Close())
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		return errors.Join(fmt.Errorf("write native execution state: %w", err), temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync native execution state: %w", err), temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close native execution state: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace native execution state: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open native execution state directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync native execution state directory: %w", err), directory.Close())
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close native execution state directory: %w", err)
	}
	return nil
}
