package idleloom

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	want := State{
		NodeName:             "mac-idle",
		KubeconfigPath:       "/tmp/kubeconfig",
		Context:              "test",
		Network:              NetworkWireKube,
		Taint:                "idleloom-dedicated=compute:NoSchedule",
		TaintConfigured:      true,
		TokenTTLSeconds:      1800,
		NetworkLease:         "idleloom-network-00001",
		NetworkLeaseUID:      "lease-uid",
		NetworkReservationID: "reservation-a",
		Runtime: RuntimeState{
			NodeName:   "mac-idle",
			RuntimeDir: "/tmp/idleloom/mac-idle",
			GuestIP:    "172.30.42.2",
			SSHPort:    22022,
		},
		CreatedAt: time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
	}
	if err := SaveState(path, want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got != want {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
}

func TestLoadStateRejectsInvalidResumeMetadata(t *testing.T) {
	for _, state := range []State{
		{NodeName: "worker-a", Runtime: RuntimeState{NodeName: "worker-a"}, Taint: "invalid", TaintConfigured: true},
		{NodeName: "worker-a", Runtime: RuntimeState{NodeName: "worker-a"}, TokenTTLSeconds: -1},
		{NodeName: "worker-a", Runtime: RuntimeState{NodeName: "worker-a"}, Taint: "idleloom-dedicated=compute:NoSchedule"},
	} {
		path := filepath.Join(t.TempDir(), "state.json")
		if err := SaveState(path, state); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadState(path); err == nil {
			t.Fatalf("invalid state was accepted: %#v", state)
		}
	}
}

func TestSaveStateLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	state := State{NodeName: "worker-a", Runtime: RuntimeState{NodeName: "worker-a"}}
	if err := SaveState(path, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".idleloom-write-") {
			t.Fatalf("temporary state file remains after atomic save: %s", entry.Name())
		}
	}
}

func TestEnsureStatePathAvailableRejectsExistingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := EnsureStatePathAvailable(path); err != nil {
		t.Fatalf("EnsureStatePathAvailable: %v", err)
	}
	if err := SaveState(path, State{NodeName: "test"}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if err := EnsureStatePathAvailable(path); err == nil {
		t.Fatal("expected existing state to be rejected")
	}
}

func TestStateLockSerializesCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	first, err := AcquireStateLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := AcquireStateLock(ctx, path); err == nil {
		t.Fatal("second command acquired an already-held state lock")
	}
}

func TestLoadStateRejectsDifferentRuntimeOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, State{NodeName: "worker-a", Runtime: RuntimeState{NodeName: "worker-b"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("state accepted a runtime owned by another node")
	}
}
