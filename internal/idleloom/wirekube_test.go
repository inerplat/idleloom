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
