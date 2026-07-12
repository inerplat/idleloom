package enroll

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistClusterTrustRejectsChangedIdentity(t *testing.T) {
	directory := t.TempDir()
	original := clusterTrust{Version: 1, Endpoint: "https://cluster.example:6443", SPKISHA256: strings.Repeat("a", 64)}
	if err := persistClusterTrust(directory, original, false); err != nil {
		t.Fatal(err)
	}
	changed := original
	changed.SPKISHA256 = strings.Repeat("b", 64)
	if err := persistClusterTrust(directory, changed, false); err == nil {
		t.Fatal("changed API identity was trusted without reset")
	}
	if err := persistClusterTrust(directory, changed, true); err != nil {
		t.Fatalf("reset trust: %v", err)
	}
	if err := persistClusterTrust(directory, changed, false); err != nil {
		t.Fatalf("persisted reset identity was not accepted: %v", err)
	}
	info, err := os.Stat(filepath.Join(directory, "cluster-trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("trust file permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestPersistClusterTrustRejectsChangedEndpoint(t *testing.T) {
	directory := t.TempDir()
	original := clusterTrust{Version: 1, Endpoint: "https://cluster.example:6443", SPKISHA256: strings.Repeat("a", 64)}
	if err := persistClusterTrust(directory, original, false); err != nil {
		t.Fatal(err)
	}
	changed := original
	changed.Endpoint = "https://other.example:6443"
	if err := persistClusterTrust(directory, changed, false); err == nil {
		t.Fatal("changed API endpoint was trusted without reset")
	}
}
