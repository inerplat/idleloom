package execution

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorePersistsAndAdoptsExactProcessIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "execution.json")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	intent := testRecord()
	if err := store.Begin(intent); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := store.CanAdopt(intent, intent); err == nil {
		t.Fatal("CanAdopt accepted an intent without a complete process identity")
	}
	process := intent
	process.PID = 123
	process.ProcessStartToken = "start-1"
	process.Executable = "/opt/idleloom/runtime/mlx-lm"
	process.RuntimeVersion = "mlx-lm-v1.0.0"
	process.Nonce = "nonce-1"
	if err := store.UpdateProcess(intent, process); err != nil {
		t.Fatalf("UpdateProcess: %v", err)
	}
	replacement := process
	replacement.PID++
	replacement.ProcessStartToken = "start-2"
	if err := store.UpdateProcess(intent, replacement); err == nil {
		t.Fatal("UpdateProcess replaced a complete process identity")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open persisted store: %v", err)
	}
	if err := reopened.CanAdopt(intent, process); err != nil {
		t.Fatalf("CanAdopt: %v", err)
	}
	wrong := process
	wrong.ProcessStartToken = "reused-pid"
	if err := reopened.CanAdopt(intent, wrong); err == nil {
		t.Fatal("CanAdopt accepted a reused PID identity")
	}
	if err := reopened.Clear(intent); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close reopened store: %v", err)
	}
}

func TestStoreRejectsDifferentExecution(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first := testRecord()
	if err := store.Begin(first); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	second := first
	second.FencingEpoch++
	if err := store.Begin(second); !errors.Is(err, ErrExecutionBusy) {
		t.Fatalf("Begin different execution error = %v, want ErrExecutionBusy", err)
	}
	if err := store.Clear(second); err == nil {
		t.Fatal("Clear removed a different execution")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestStorePersistsTerminalExecutionBeforeStatusPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "execution.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	planned := testRecord()
	if err := store.Begin(planned); err != nil {
		t.Fatal(err)
	}
	running := planned
	running.PID = 123
	running.ProcessStartToken = "start-1"
	if err := store.UpdateProcess(planned, running); err != nil {
		t.Fatal(err)
	}
	exitErr := errors.New("exit status 7")
	if err := store.Complete(running, exitErr); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	current := reopened.Current()
	if current == nil || !current.Completed || current.ExitError != exitErr.Error() {
		t.Fatalf("terminal record = %#v", current)
	}
}

func TestStoreHoldsLifetimeProcessLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "execution.json")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	if _, err := Open(path); !errors.Is(err, ErrStoreLocked) {
		t.Fatalf("Open second error = %v, want ErrStoreLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}
	second, err := Open(path)
	if err != nil {
		t.Fatalf("Open after unlock: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close second: %v", err)
	}
	if err := first.Begin(testRecord()); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("Begin after Close error = %v, want ErrStoreClosed", err)
	}
}

func TestBeginRequiresExactPlannedIdentity(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = store.Close() }()
	first := testRecord()
	if err := store.Begin(first); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	different := first
	different.Nonce = "different"
	if err := store.Begin(different); !errors.Is(err, ErrExecutionBusy) {
		t.Fatalf("Begin different plan error = %v, want ErrExecutionBusy", err)
	}
}

func TestOpenRejectsUnknownOrOversizedState(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "execution.json")
	unknown := `{"schemaVersion":1,"workloadUID":"w","workloadGeneration":1,"assignmentUID":"a","executionID":"e","fencingEpoch":1,"executable":"x","runtimeVersion":"v","nonce":"n","future":true}`
	if err := os.WriteFile(path, []byte(unknown), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted an unknown state field")
	}
	unsupported := strings.Replace(unknown, `"schemaVersion":1`, `"schemaVersion":2`, 1)
	unsupported = strings.Replace(unsupported, `,"future":true`, "", 1)
	if err := os.WriteFile(path, []byte(unsupported), 0o600); err != nil {
		t.Fatalf("WriteFile unsupported: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted an unknown state schema")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxStateBytes+1)), 0o600); err != nil {
		t.Fatalf("WriteFile oversized: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open accepted oversized state")
	}
}

func testRecord() Record {
	return Record{
		SchemaVersion: SchemaVersionV1,
		WorkloadUID:   "workload-uid", WorkloadGeneration: 1,
		AssignmentUID: "assignment-uid", ExecutionID: "execution-id", FencingEpoch: 1,
		Executable: "/opt/idleloom/runtime/mlx-lm", RuntimeVersion: "mlx-lm-v1.0.0", Nonce: "nonce-1",
	}
}
