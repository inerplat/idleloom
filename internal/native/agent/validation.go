package agent

import (
	"fmt"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/execution"
)

// ValidateMailbox fails closed before an agent accepts a host-scoped
// Assignment. Physical GPU state must still be rechecked by HostResourceArbiter
// immediately before process launch.
func ValidateMailbox(assignment *nativev1alpha1.IdleloomWorkloadAssignment, host *nativev1alpha1.IdleloomHost, agentID string, current *execution.Record) error {
	if err := nativev1alpha1.ValidateHost(host); err != nil {
		return fmt.Errorf("validate host mailbox: %w", err)
	}
	if err := nativev1alpha1.ValidateAssignment(assignment); err != nil {
		return fmt.Errorf("validate assignment mailbox: %w", err)
	}
	if assignment.UID == "" {
		return fmt.Errorf("assignment UID is required")
	}
	if assignment.Namespace != host.Namespace {
		return fmt.Errorf("assignment namespace %q does not match host namespace %q", assignment.Namespace, host.Namespace)
	}
	if assignment.Spec.HostRef.Name != host.Name || assignment.Spec.HostRef.UID != host.UID {
		return fmt.Errorf("assignment targets a different host identity")
	}
	if agentID == "" || host.Spec.AgentID != agentID {
		return fmt.Errorf("agent identity %q does not own host mailbox %q", agentID, host.Spec.AgentID)
	}
	if current == nil {
		return nil
	}
	if assignment.Spec.FencingEpoch < current.FencingEpoch {
		return fmt.Errorf("assignment fencing epoch %d is older than accepted epoch %d", assignment.Spec.FencingEpoch, current.FencingEpoch)
	}
	if assignment.Spec.FencingEpoch == current.FencingEpoch {
		if string(assignment.UID) != current.AssignmentUID || assignment.Spec.ExecutionID != current.ExecutionID {
			return fmt.Errorf("assignment reuses fencing epoch %d for a different execution", assignment.Spec.FencingEpoch)
		}
	}
	return nil
}
