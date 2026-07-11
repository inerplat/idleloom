package state

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestStoreExclusiveAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := PreparedClaim{UID: "claim-1", Pool: "node-1", Device: "renderd128"}
	if err := store.Reserve(first); err != nil {
		t.Fatalf("Reserve(first) error = %v", err)
	}
	if err := store.Reserve(first); err != nil {
		t.Fatalf("idempotent Reserve(first) error = %v", err)
	}
	if err := store.Reserve(PreparedClaim{UID: "claim-2", Pool: "node-1", Device: "renderd128"}); !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("Reserve(second) error = %v, want ErrDeviceBusy", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Release("claim-1"); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if err := reopened.Reserve(PreparedClaim{UID: "claim-2", Pool: "node-1", Device: "renderd128"}); err != nil {
		t.Fatalf("Reserve(second after release) error = %v", err)
	}
}
