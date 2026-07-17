package idleloom

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaintainerCommandUsesWorkerSubcommand(t *testing.T) {
	statePath := "/tmp/idleloom state.json"
	arguments := maintainerCommandArguments(statePath)
	if got, want := strings.Join(arguments, " "), "worker maintain --state "+statePath; got != want {
		t.Fatalf("maintainer arguments = %q, want %q", got, want)
	}
	if !maintainerCommandMatches("/opt/homebrew/bin/idlectl "+strings.Join(arguments, " "), statePath) {
		t.Fatal("worker maintainer command was not recognized")
	}
	if maintainerCommandMatches("/opt/homebrew/bin/idlectl maintain --state "+statePath, statePath) {
		t.Fatal("legacy maintainer command was accepted")
	}
}

func TestMaintainRemovesMetadataOnShutdown(t *testing.T) {
	statePath := maintainerTestState(t, PhaseLocalGone)
	app := &App{Out: io.Discard, Err: io.Discard}
	if err := app.Maintain(context.Background(), statePath); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	canonical, err := canonicalStatePath(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(maintainerMetadataFile(canonical)); !os.IsNotExist(err) {
		t.Fatalf("maintainer metadata remains: %v", err)
	}
}

func TestMaintainStopsWhenRuntimeIsNotRunning(t *testing.T) {
	statePath := maintainerTestState(t, PhaseReady)
	app := &App{Out: io.Discard, Err: io.Discard, Runtime: rejectingRuntime{}}
	if err := app.Maintain(context.Background(), statePath); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
	canonical, err := canonicalStatePath(statePath)
	if err != nil {
		t.Fatal(err)
	}
	held, err := maintainerLockHeld(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("maintainer remained active for a stopped runtime")
	}
}

func TestMaintainRejectsDuplicateOwner(t *testing.T) {
	statePath := maintainerTestState(t, PhaseReady)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	app := &App{Out: io.Discard, Err: io.Discard, Runtime: runningRuntime{}}
	go func() { done <- app.Maintain(ctx, statePath) }()
	canonical, err := canonicalStatePath(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitUntil(context.Background(), time.Second, func() bool {
		held, _ := maintainerLockHeld(canonical)
		return held
	}); err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := app.Maintain(context.Background(), statePath); err == nil {
		cancel()
		t.Fatal("duplicate maintainer acquired the state lock")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("first maintainer: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first maintainer did not stop")
	}
}

func TestStopMaintainerDiscardsUnlockedStaleMetadata(t *testing.T) {
	statePath := maintainerTestState(t, PhaseReady)
	canonical, err := canonicalStatePath(statePath)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	startedAt, err := processStartIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMaintainerMetadata(canonical, maintainerProcessData{
		PID: os.Getpid(), Nonce: "stale", StatePath: canonical,
		Executable: executable, StartedAt: startedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := stopMaintainer(statePath); err != nil {
		t.Fatalf("StopMaintainer: %v", err)
	}
	if _, err := os.Stat(maintainerMetadataFile(canonical)); !os.IsNotExist(err) {
		t.Fatalf("stale metadata remains: %v", err)
	}
}

func TestFailedDeleteKeepsRunningMaintainer(t *testing.T) {
	statePath := maintainerTestState(t, PhaseReady)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	app := &App{Out: io.Discard, Err: io.Discard, Runtime: runningRuntime{}}
	go func() { done <- app.Maintain(ctx, statePath) }()
	canonical, err := canonicalStatePath(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitUntil(context.Background(), time.Second, func() bool {
		held, _ := maintainerLockHeld(canonical)
		return held
	}); err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := app.Delete(context.Background(), statePath, false, false); err == nil {
		cancel()
		t.Fatal("delete unexpectedly succeeded with a missing kubeconfig")
	}
	held, err := maintainerLockHeld(canonical)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if !held {
		cancel()
		t.Fatal("failed delete stopped the certificate maintainer")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("maintainer: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("maintainer did not stop")
	}
}

func TestRotateAndCleanupMaintainerFiles(t *testing.T) {
	statePath := maintainerTestState(t, PhaseLocalGone)
	canonical, err := canonicalStatePath(statePath)
	if err != nil {
		t.Fatal(err)
	}
	logPath := canonical + ".maintainer.log"
	if err := os.WriteFile(logPath, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rotateMaintainerLog(logPath, 1024); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("rotated log is missing: %v", err)
	}
	if err := cleanupMaintainerFiles(statePath); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{logPath, logPath + ".1", maintainerMetadataFile(canonical)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("maintainer artifact remains at %s: %v", path, err)
		}
	}
	held, err := maintainerLockHeld(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("maintainer lock remains held after cleanup")
	}
}

func maintainerTestState(t *testing.T, phase string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	state := State{
		NodeName: "worker-a", Phase: phase, KubeconfigPath: filepath.Join(t.TempDir(), "missing-kubeconfig"),
		Runtime: RuntimeState{NodeName: "worker-a", GuestIP: "172.20.10.2"},
	}
	if err := SaveState(path, state); err != nil {
		t.Fatal(err)
	}
	return path
}

type runningRuntime struct{ rejectingRuntime }

func (runningRuntime) Status(context.Context, *RuntimeState) (WorkerStatus, error) {
	return WorkerStatus{VM: "running", Network: "running"}, nil
}
