package agent

import (
	"strings"
	"testing"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/execution"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestValidateMailboxAcceptsExactHostAndFreshEpoch(t *testing.T) {
	host, assignment := validMailbox()
	current := execution.Record{SchemaVersion: execution.SchemaVersionV1, WorkloadUID: "old-workload", WorkloadGeneration: 1, AssignmentUID: "old-assignment", ExecutionID: "old-execution", FencingEpoch: 6, Executable: "/runtime/mlx", RuntimeVersion: "v1", Nonce: "old"}
	if err := ValidateMailbox(&assignment, &host, "studio-agent", &current); err != nil {
		t.Fatalf("ValidateMailbox: %v", err)
	}
}

func TestValidateMailboxRejectsWrongIdentityAndStaleEpoch(t *testing.T) {
	host, assignment := validMailbox()
	tests := []struct {
		name   string
		mutate func(*nativev1alpha1.IdleloomWorkloadAssignment, *nativev1alpha1.IdleloomHost, *execution.Record)
	}{
		{name: "wrong-name", mutate: func(a *nativev1alpha1.IdleloomWorkloadAssignment, _ *nativev1alpha1.IdleloomHost, _ *execution.Record) {
			a.Name = "other"
		}},
		{name: "wrong-host", mutate: func(a *nativev1alpha1.IdleloomWorkloadAssignment, _ *nativev1alpha1.IdleloomHost, _ *execution.Record) {
			a.Spec.HostRef.UID = "other"
		}},
		{name: "stale-epoch", mutate: func(a *nativev1alpha1.IdleloomWorkloadAssignment, _ *nativev1alpha1.IdleloomHost, current *execution.Record) {
			current.FencingEpoch = a.Spec.FencingEpoch + 1
		}},
		{name: "reused-epoch", mutate: func(a *nativev1alpha1.IdleloomWorkloadAssignment, _ *nativev1alpha1.IdleloomHost, current *execution.Record) {
			current.FencingEpoch = a.Spec.FencingEpoch
			current.AssignmentUID = "other"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := host.DeepCopy()
			assignment := assignment.DeepCopy()
			current := &execution.Record{SchemaVersion: execution.SchemaVersionV1, WorkloadUID: "old", WorkloadGeneration: 1, AssignmentUID: "old", ExecutionID: "old", FencingEpoch: 6, Executable: "/runtime/mlx", RuntimeVersion: "v1", Nonce: "nonce"}
			test.mutate(assignment, host, current)
			if err := ValidateMailbox(assignment, host, "studio-agent", current); err == nil {
				t.Fatal("ValidateMailbox accepted an unsafe mailbox")
			}
		})
	}
}

func validMailbox() (nativev1alpha1.IdleloomHost, nativev1alpha1.IdleloomWorkloadAssignment) {
	digest := "sha256:" + strings.Repeat("a", 64)
	host := nativev1alpha1.IdleloomHost{
		ObjectMeta: metav1.ObjectMeta{Namespace: "idleloom-host-studio", Name: "host", UID: types.UID("host-uid")},
		Spec:       nativev1alpha1.IdleloomHostSpec{AgentID: "studio-agent"},
	}
	assignment := nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{Namespace: host.Namespace, Name: nativev1alpha1.AssignmentMailboxName, UID: types.UID("assignment-uid")},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef:  nativev1alpha1.WorkloadObjectReference{Namespace: "default", Name: "qwen", UID: types.UID("workload-uid"), Generation: 1},
			HostRef:      nativev1alpha1.ObjectReference{Name: host.Name, UID: host.UID},
			Model: &nativev1alpha1.ResolvedModel{
				CatalogRef:           nativev1alpha1.ObjectReference{Name: "qwen-approved", UID: types.UID("model-uid")},
				Family:               nativev1alpha1.ModelFamilyQwen35,
				RuntimeProfile:       nativev1alpha1.RuntimeProfileMLXLMV1,
				Artifact:             nativev1alpha1.ModelArtifact{OCIReference: "oci://registry.example/qwen@" + digest, ManifestDigest: digest, Format: nativev1alpha1.ArtifactFormatSafetensorsV1, SizeBytes: 1024, Signature: nativev1alpha1.SignaturePolicy{Issuer: "issuer", Subject: "subject"}},
				UnifiedMemoryRequest: resource.MustParse("12Gi"), MaxContextLength: 2048, MaxConcurrentRequests: 1,
			},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 7, LeaseDurationSeconds: 30,
		},
	}
	return host, assignment
}
