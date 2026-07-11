package idleloom

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateInitOptions(t *testing.T) {
	valid := InitOptions{
		NodeName: "mac-mini-idle",
		CPUs:     4,
		MemoryMB: 8192,
		DiskMB:   40960,
		Taint:    "example.com/dedicated=gpu:NoSchedule",
		Network:  NetworkWireKube,
		Timeout:  time.Minute,
		TokenTTL: time.Minute,
	}
	if err := validateInitOptions(valid); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	tooSmall := valid
	tooSmall.MemoryMB = 2048
	if err := validateInitOptions(tooSmall); err == nil {
		t.Fatal("expected 2 GiB VM to be rejected")
	}
	badTaint := valid
	badTaint.Taint = "dedicated"
	if err := validateInitOptions(badTaint); err == nil {
		t.Fatal("expected malformed taint to be rejected")
	}
	unsafeTaint := valid
	unsafeTaint.Taint = "example.com/dedicated=gpu:NoSchedule;rm -rf /"
	if err := validateInitOptions(unsafeTaint); err == nil {
		t.Fatal("expected shell metacharacters in taint to be rejected")
	}
}

func TestDeleteValidatesRuntimeBeforeLoadingCluster(t *testing.T) {
	sentinel := errors.New("runtime ownership rejected")
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := State{
		NodeName: "worker-a", KubeconfigPath: filepath.Join(t.TempDir(), "missing-kubeconfig"),
		Phase: PhaseReady, Runtime: RuntimeState{NodeName: "worker-a", RuntimeDir: "/tmp/idleloom-worker-a"},
	}
	if err := SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	app := &App{Out: io.Discard, Err: io.Discard, Now: time.Now, Runtime: rejectingRuntime{err: sentinel}}
	err := app.Delete(context.Background(), statePath, false, false)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Delete error = %v, want runtime validation error", err)
	}
}

func TestLocalDeleteDoesNotAdvancePhaseWhenValidationFails(t *testing.T) {
	sentinel := errors.New("runtime ownership rejected")
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := State{NodeName: "worker-a", Phase: PhaseReady, Runtime: RuntimeState{NodeName: "worker-a", RuntimeDir: "/tmp/idleloom-worker-a"}}
	if err := SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	app := &App{Out: io.Discard, Err: io.Discard, Now: time.Now, Runtime: rejectingRuntime{err: sentinel}}
	if err := app.Delete(context.Background(), statePath, false, true); !errors.Is(err, sentinel) {
		t.Fatalf("Delete error = %v, want validation error", err)
	}
	got, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != PhaseReady {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseReady)
	}
}

func TestLocalDeleteFailureLeavesPendingPhase(t *testing.T) {
	sentinel := errors.New("runtime delete failed")
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := State{NodeName: "worker-a", Phase: PhaseReady, Runtime: RuntimeState{NodeName: "worker-a"}}
	if err := SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	app := &App{Out: io.Discard, Err: io.Discard, Now: time.Now, Runtime: deletingRuntime{err: sentinel}}
	if err := app.Delete(context.Background(), statePath, false, true); !errors.Is(err, sentinel) {
		t.Fatalf("Delete error = %v, want delete error", err)
	}
	got, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != PhaseLocalDeleting {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseLocalDeleting)
	}
}

type rejectingRuntime struct {
	err error
}

type deletingRuntime struct {
	rejectingRuntime
	err error
}

func (r deletingRuntime) Validate(context.Context, RuntimeState) error { return nil }
func (r deletingRuntime) Delete(context.Context, RuntimeState) error   { return r.err }

func (r rejectingRuntime) Preflight(context.Context) error { return nil }
func (r rejectingRuntime) Plan(context.Context, RuntimeConfig) (RuntimeState, error) {
	return RuntimeState{}, nil
}
func (r rejectingRuntime) Create(context.Context, *RuntimeState) error  { return nil }
func (r rejectingRuntime) Validate(context.Context, RuntimeState) error { return r.err }
func (r rejectingRuntime) Start(context.Context, *RuntimeState) error   { return nil }
func (r rejectingRuntime) WaitReady(context.Context, RuntimeState, time.Duration) error {
	return nil
}
func (r rejectingRuntime) Stop(context.Context, RuntimeState) error   { return nil }
func (r rejectingRuntime) Delete(context.Context, RuntimeState) error { return nil }
func (r rejectingRuntime) InstallBundle(context.Context, RuntimeState, string) error {
	return nil
}
func (r rejectingRuntime) RemoveBootstrapIdentity(context.Context, RuntimeState) error { return nil }
func (r rejectingRuntime) Status(context.Context, *RuntimeState) (WorkerStatus, error) {
	return WorkerStatus{}, nil
}
