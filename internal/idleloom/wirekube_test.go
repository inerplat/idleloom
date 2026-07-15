package idleloom

import (
	"strings"
	"testing"
)

func TestValidateWireKubeStatusAllowsDeferredRegistrationWithoutReadyPeer(t *testing.T) {
	status := WireKubeStatus{
		Installed:             true,
		IncludeNodeInternalIP: true,
		AgentNamespace:        "wirekube-system",
		AgentName:             "wirekube-agent",
	}
	if err := validateWireKubeStatus(status, false); err != nil {
		t.Fatalf("deferred registration rejected: %v", err)
	}
	err := validateWireKubeStatus(status, true)
	if err == nil || !strings.Contains(err.Error(), "no ready ingress peers") {
		t.Fatalf("strict readiness error = %v", err)
	}
}

func TestValidateWireKubeStatusKeepsStructuralChecksForDeferredRegistration(t *testing.T) {
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
			err := validateWireKubeStatus(test.status, false)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}
