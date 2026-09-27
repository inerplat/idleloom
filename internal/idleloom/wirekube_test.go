package idleloom

import (
	"strings"
	"testing"
)

func TestValidateWireKubeStatusNeverRequiresReadyPeers(t *testing.T) {
	// A bootstrapping mesh has zero ready peers until this worker becomes the
	// first one; readiness is judged later by waitForWireKube on the worker's
	// own peer, so zero ready peers must never fail validation.
	status := WireKubeStatus{
		Installed:             true,
		IncludeNodeInternalIP: true,
		AgentNamespace:        "wirekube-system",
		AgentName:             "wirekube-agent",
	}
	if err := validateWireKubeStatus(status); err != nil {
		t.Fatalf("zero ready peers rejected: %v", err)
	}
}

func TestValidateWireKubeStatusKeepsStructuralChecks(t *testing.T) {
	tests := []struct {
		name   string
		status WireKubeStatus
		want   string
	}{
		{
			name: "missing agent",
			status: WireKubeStatus{
				Installed: true, IncludeNodeInternalIP: true,
			},
			want: "no WireKube agent DaemonSet",
		},
		{
			name: "node addresses disabled",
			status: WireKubeStatus{
				Installed: true, AgentNamespace: "wirekube-system", AgentName: "wirekube-agent",
			},
			want: "includeNodeInternalIP=true",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateWireKubeStatus(test.status)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}

// TestConflictingExternalPeerCoversBothKindsOfClaim matters because a Native
// Metal host reserves its mesh address before the WireKubePeer carrying it
// exists. Scanning peers alone would let a worker take an address
// "idlectl join" had already claimed, leaving both advertising it.
func TestConflictingExternalPeerCoversBothKindsOfClaim(t *testing.T) {
	const meshCIDR = "198.18.18.0/24"
	// worker1 hashes to 198.18.18.83 in this CIDR; see internal/meship.
	const contested = "198.18.18.83"

	assigned := []externalPeerClaim{{Name: "evening-mac", AssignedMeshIP: contested + "/32"}}
	if err := conflictingExternalPeer(assigned, contested, meshCIDR); err == nil {
		t.Error("an address already assigned to an external peer was accepted")
	} else if !strings.Contains(err.Error(), "evening-mac") {
		t.Errorf("error does not name the holder: %v", err)
	}

	// The same peer before its address has been assigned: only the display
	// name says which address it is going to take.
	pending := []externalPeerClaim{{Name: "evening-mac", DisplayName: "worker1"}}
	if err := conflictingExternalPeer(pending, contested, meshCIDR); err == nil {
		t.Error("an address a pending external peer will take was accepted")
	}

	clear := []externalPeerClaim{
		{Name: "other", DisplayName: "worker2", AssignedMeshIP: "198.18.18.180/32"},
		{Name: "unnamed"},
	}
	if err := conflictingExternalPeer(clear, contested, meshCIDR); err != nil {
		t.Errorf("an uncontested address was rejected: %v", err)
	}
}
