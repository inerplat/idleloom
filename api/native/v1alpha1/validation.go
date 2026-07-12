package v1alpha1

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

var (
	sha256Pattern       = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	uuidPattern         = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	ociReferencePattern = regexp.MustCompile(`^oci://[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$`)
)

const (
	maxArtifactBytes      = int64(64 << 30)
	maxContextLength      = int32(8192)
	runtimeMemoryOverhead = int64(4 << 30)
	contextMemoryPerToken = int64(1 << 20)
)

func ValidateWorkload(workload *IdleloomWorkload) error {
	var errs field.ErrorList
	specPath := field.NewPath("spec")
	switch workload.Spec.Mode {
	case WorkloadModeServer:
		if workload.Spec.Model == nil || workload.Spec.Shell != nil {
			errs = append(errs, field.Invalid(specPath, workload.Spec, "Server mode requires model and forbids shell"))
		} else if problems := validation.IsDNS1123Subdomain(workload.Spec.Model.CatalogRef); len(problems) > 0 {
			errs = append(errs, field.Invalid(specPath.Child("model", "catalogRef"), workload.Spec.Model.CatalogRef, strings.Join(problems, "; ")))
		}
	case WorkloadModeShell:
		if workload.Spec.Shell == nil || workload.Spec.Model != nil {
			errs = append(errs, field.Invalid(specPath, workload.Spec, "Shell mode requires shell and forbids model"))
		} else {
			if strings.TrimSpace(workload.Spec.Shell.Script) == "" || strings.ContainsRune(workload.Spec.Shell.Script, '\x00') {
				errs = append(errs, field.Invalid(specPath.Child("shell", "script"), workload.Spec.Shell.Script, "must contain a non-empty script without NUL bytes"))
			}
			if len([]byte(workload.Spec.Shell.Script)) > 64<<10 {
				errs = append(errs, field.TooLong(specPath.Child("shell", "script"), "", 64<<10))
			}
			if workload.Spec.Shell.Isolation != "" && workload.Spec.Shell.Isolation != ShellIsolationSandbox && workload.Spec.Shell.Isolation != ShellIsolationHost {
				errs = append(errs, field.NotSupported(specPath.Child("shell", "isolation"), workload.Spec.Shell.Isolation, []string{ShellIsolationSandbox, ShellIsolationHost}))
			}
			if workload.Spec.Shell.Network != "" && workload.Spec.Shell.Network != ShellNetworkNone && workload.Spec.Shell.Network != ShellNetworkOutbound {
				errs = append(errs, field.NotSupported(specPath.Child("shell", "network"), workload.Spec.Shell.Network, []string{ShellNetworkNone, ShellNetworkOutbound}))
			}
			if workload.Spec.Shell.Isolation == ShellIsolationHost && workload.Spec.Shell.Network == ShellNetworkNone {
				errs = append(errs, field.Invalid(specPath.Child("shell", "network"), workload.Spec.Shell.Network, "Host isolation cannot enforce network isolation; use Outbound"))
			}
			if workload.Spec.Shell.TimeoutSeconds < 0 || workload.Spec.Shell.TimeoutSeconds > 86400 {
				errs = append(errs, field.Invalid(specPath.Child("shell", "timeoutSeconds"), workload.Spec.Shell.TimeoutSeconds, "must be between 1 and 86400 when set"))
			}
		}
	default:
		errs = append(errs, field.NotSupported(specPath.Child("mode"), workload.Spec.Mode, []string{WorkloadModeServer, WorkloadModeShell}))
	}
	if workload.Spec.Resources.UnifiedMemoryRequest.Sign() <= 0 {
		errs = append(errs, field.Invalid(specPath.Child("resources", "unifiedMemoryRequest"), workload.Spec.Resources.UnifiedMemoryRequest.String(), "must be positive"))
	}
	return errs.ToAggregate()
}

func ValidateModel(model *IdleloomModel) error {
	var errs field.ErrorList
	specPath := field.NewPath("spec")
	if model.Spec.Family != ModelFamilyQwen35 {
		errs = append(errs, field.NotSupported(specPath.Child("family"), model.Spec.Family, []string{ModelFamilyQwen35}))
	}
	if model.Spec.RuntimeProfile != RuntimeProfileMLXLMV1 {
		errs = append(errs, field.NotSupported(specPath.Child("runtimeProfile"), model.Spec.RuntimeProfile, []string{RuntimeProfileMLXLMV1}))
	}
	errs = append(errs, validateArtifact(model.Spec.Artifact, specPath.Child("artifact"))...)
	if model.Spec.MinimumUnifiedMemory.Sign() <= 0 {
		errs = append(errs, field.Invalid(specPath.Child("minimumUnifiedMemory"), model.Spec.MinimumUnifiedMemory.String(), "must be positive"))
	}
	if model.Spec.MaxContextLength < 128 {
		errs = append(errs, field.Invalid(specPath.Child("maxContextLength"), model.Spec.MaxContextLength, "must be at least 128"))
	} else if model.Spec.MaxContextLength > maxContextLength {
		errs = append(errs, field.Invalid(specPath.Child("maxContextLength"), model.Spec.MaxContextLength, fmt.Sprintf("must not exceed %d", maxContextLength)))
	}
	if model.Spec.MaxConcurrentRequests != 1 {
		errs = append(errs, field.NotSupported(specPath.Child("maxConcurrentRequests"), model.Spec.MaxConcurrentRequests, []string{"1"}))
	}
	minimum := MinimumUnifiedMemoryForModel(model.Spec.Artifact.SizeBytes, model.Spec.MaxContextLength)
	if model.Spec.MinimumUnifiedMemory.Cmp(minimum) < 0 {
		errs = append(errs, field.Invalid(specPath.Child("minimumUnifiedMemory"), model.Spec.MinimumUnifiedMemory.String(), fmt.Sprintf("must be at least %s for artifact, runtime, and context overhead", minimum.String())))
	}
	return errs.ToAggregate()
}

func ValidateHost(host *IdleloomHost) error {
	var errs field.ErrorList
	if host.Name != "" && host.Name != "host" {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), host.Name, "host mailbox object must be named host"))
	}
	if problems := validation.IsDNS1123Subdomain(host.Spec.AgentID); len(problems) > 0 {
		errs = append(errs, field.Invalid(field.NewPath("spec", "agentID"), host.Spec.AgentID, strings.Join(problems, "; ")))
	}
	if host.Spec.ShellAccess != "" && host.Spec.ShellAccess != ShellAccessDisabled && host.Spec.ShellAccess != ShellAccessSandboxed && host.Spec.ShellAccess != ShellAccessHost {
		errs = append(errs, field.NotSupported(field.NewPath("spec", "shellAccess"), host.Spec.ShellAccess, []string{ShellAccessDisabled, ShellAccessSandboxed, ShellAccessHost}))
	}
	return errs.ToAggregate()
}

func ValidateAssignment(assignment *IdleloomWorkloadAssignment) error {
	var errs field.ErrorList
	specPath := field.NewPath("spec")
	if assignment.Name != "" && assignment.Name != AssignmentMailboxName {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), assignment.Name, "assignment mailbox object must be named active"))
	}
	if assignment.Spec.DesiredState != AssignmentDesiredRunning && assignment.Spec.DesiredState != AssignmentDesiredStopped {
		errs = append(errs, field.NotSupported(specPath.Child("desiredState"), assignment.Spec.DesiredState, []string{AssignmentDesiredRunning, AssignmentDesiredStopped}))
	}
	if assignment.Spec.WorkloadRef.Namespace == "" || assignment.Spec.WorkloadRef.Name == "" || assignment.Spec.WorkloadRef.UID == "" || assignment.Spec.WorkloadRef.Generation < 1 {
		errs = append(errs, field.Invalid(specPath.Child("workloadRef"), assignment.Spec.WorkloadRef, "namespace, name, UID, and positive generation are required"))
	}
	if assignment.Spec.HostRef.Name == "" || assignment.Spec.HostRef.UID == "" {
		errs = append(errs, field.Invalid(specPath.Child("hostRef"), assignment.Spec.HostRef, "name and UID are required"))
	}
	if !uuidPattern.MatchString(assignment.Spec.ExecutionID) {
		errs = append(errs, field.Invalid(specPath.Child("executionID"), assignment.Spec.ExecutionID, "must be a lowercase UUID"))
	}
	if assignment.Spec.FencingEpoch < 1 {
		errs = append(errs, field.Invalid(specPath.Child("fencingEpoch"), assignment.Spec.FencingEpoch, "must be positive"))
	}
	if assignment.Spec.LeaseDurationSeconds < 10 || assignment.Spec.LeaseDurationSeconds > 300 {
		errs = append(errs, field.Invalid(specPath.Child("leaseDurationSeconds"), assignment.Spec.LeaseDurationSeconds, "must be between 10 and 300 seconds"))
	}
	if (assignment.Spec.Model == nil) == (assignment.Spec.Shell == nil) {
		errs = append(errs, field.Invalid(specPath, assignment.Spec, "exactly one model or shell is required"))
	} else if assignment.Spec.Model != nil {
		model := &IdleloomModel{Spec: IdleloomModelSpec{
			Family:                assignment.Spec.Model.Family,
			RuntimeProfile:        assignment.Spec.Model.RuntimeProfile,
			Artifact:              assignment.Spec.Model.Artifact,
			MinimumUnifiedMemory:  assignment.Spec.Model.UnifiedMemoryRequest,
			MaxContextLength:      assignment.Spec.Model.MaxContextLength,
			MaxConcurrentRequests: assignment.Spec.Model.MaxConcurrentRequests,
		}}
		if err := ValidateModel(model); err != nil {
			errs = append(errs, field.Invalid(specPath.Child("model"), assignment.Spec.Model, err.Error()))
		}
		if assignment.Spec.Model.CatalogRef.Name == "" || assignment.Spec.Model.CatalogRef.UID == "" {
			errs = append(errs, field.Invalid(specPath.Child("model", "catalogRef"), assignment.Spec.Model.CatalogRef, "name and UID are required"))
		}
	} else {
		if strings.TrimSpace(assignment.Spec.Shell.Script) == "" || strings.ContainsRune(assignment.Spec.Shell.Script, '\x00') {
			errs = append(errs, field.Invalid(specPath.Child("shell", "script"), assignment.Spec.Shell.Script, "must contain a non-empty script without NUL bytes"))
		}
		if assignment.Spec.Shell.Network != ShellNetworkNone && assignment.Spec.Shell.Network != ShellNetworkOutbound {
			errs = append(errs, field.NotSupported(specPath.Child("shell", "network"), assignment.Spec.Shell.Network, []string{ShellNetworkNone, ShellNetworkOutbound}))
		}
		if assignment.Spec.Shell.Isolation != ShellIsolationSandbox && assignment.Spec.Shell.Isolation != ShellIsolationHost {
			errs = append(errs, field.NotSupported(specPath.Child("shell", "isolation"), assignment.Spec.Shell.Isolation, []string{ShellIsolationSandbox, ShellIsolationHost}))
		}
		if assignment.Spec.Shell.Isolation == ShellIsolationHost && assignment.Spec.Shell.Network != ShellNetworkOutbound {
			errs = append(errs, field.Invalid(specPath.Child("shell", "network"), assignment.Spec.Shell.Network, "Host isolation requires Outbound network"))
		}
		if assignment.Spec.Shell.TimeoutSeconds < 1 || assignment.Spec.Shell.TimeoutSeconds > 86400 {
			errs = append(errs, field.Invalid(specPath.Child("shell", "timeoutSeconds"), assignment.Spec.Shell.TimeoutSeconds, "must be between 1 and 86400"))
		}
		if assignment.Spec.Shell.UnifiedMemoryRequest.Sign() <= 0 {
			errs = append(errs, field.Invalid(specPath.Child("shell", "unifiedMemoryRequest"), assignment.Spec.Shell.UnifiedMemoryRequest.String(), "must be positive"))
		}
	}
	return errs.ToAggregate()
}

func ValidateStopAcknowledgement(assignment *IdleloomWorkloadAssignment) error {
	if assignment.Spec.DesiredState != AssignmentDesiredStopped || assignment.Status.Phase != PhaseStopped {
		return fmt.Errorf("assignment has not reached the requested Stopped phase")
	}
	ack := assignment.Status.StopAcknowledgement
	if ack == nil {
		return fmt.Errorf("stop acknowledgement is missing")
	}
	if assignment.UID == "" || ack.AssignmentUID != assignment.UID {
		return fmt.Errorf("stop acknowledgement assignment UID does not match")
	}
	if ack.ObservedGeneration != assignment.Generation || assignment.Status.ObservedGeneration != assignment.Generation {
		return fmt.Errorf("stop acknowledgement has not observed the current assignment generation")
	}
	if ack.ExecutionID != assignment.Spec.ExecutionID || assignment.Status.ExecutionID != assignment.Spec.ExecutionID {
		return fmt.Errorf("stop acknowledgement execution ID does not match")
	}
	if ack.FencingEpoch != assignment.Spec.FencingEpoch || assignment.Status.FencingEpoch != assignment.Spec.FencingEpoch {
		return fmt.Errorf("stop acknowledgement fencing epoch does not match")
	}
	if assignment.Status.AgentID == "" {
		return fmt.Errorf("stop acknowledgement has no agent identity")
	}
	if ack.StoppedAt.IsZero() {
		return fmt.Errorf("stop acknowledgement timestamp is missing")
	}
	if assignment.Status.LastHeartbeatTime == nil || assignment.Status.LastHeartbeatTime.Before(&ack.StoppedAt) {
		return fmt.Errorf("stop acknowledgement is newer than the last agent heartbeat")
	}
	return nil
}

func validateArtifact(artifact ModelArtifact, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	parsed, err := url.Parse(artifact.OCIReference)
	if err != nil || !ociReferencePattern.MatchString(artifact.OCIReference) || parsed.Scheme != "oci" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		errs = append(errs, field.Invalid(path.Child("ociReference"), artifact.OCIReference, "must be a credential-free OCI reference pinned by digest"))
	} else {
		at := strings.LastIndex(parsed.Path, "@")
		if at <= 1 || !sha256Pattern.MatchString(parsed.Path[at+1:]) {
			errs = append(errs, field.Invalid(path.Child("ociReference"), artifact.OCIReference, "must end in @sha256:<64 lowercase hex characters>"))
		}
	}
	if !sha256Pattern.MatchString(artifact.ManifestDigest) {
		errs = append(errs, field.Invalid(path.Child("manifestDigest"), artifact.ManifestDigest, "must be sha256:<64 lowercase hex characters>"))
	}
	if artifact.Format != ArtifactFormatSafetensorsV1 {
		errs = append(errs, field.NotSupported(path.Child("format"), artifact.Format, []string{ArtifactFormatSafetensorsV1}))
	}
	if artifact.SizeBytes <= 0 {
		errs = append(errs, field.Invalid(path.Child("sizeBytes"), artifact.SizeBytes, "must be positive"))
	} else if artifact.SizeBytes > maxArtifactBytes {
		errs = append(errs, field.Invalid(path.Child("sizeBytes"), artifact.SizeBytes, fmt.Sprintf("must not exceed %d bytes", maxArtifactBytes)))
	}
	if strings.TrimSpace(artifact.Signature.Issuer) == "" || strings.TrimSpace(artifact.Signature.Subject) == "" {
		errs = append(errs, field.Invalid(path.Child("signature"), artifact.Signature, "issuer and subject are required"))
	}
	return errs
}

func MinimumUnifiedMemoryForModel(artifactBytes int64, contextLength int32) resource.Quantity {
	bytes := artifactBytes + runtimeMemoryOverhead + int64(contextLength)*contextMemoryPerToken
	return *resource.NewQuantity(bytes, resource.BinarySI)
}

func EffectiveUnifiedMemoryRequest(workload resource.Quantity, model resource.Quantity) resource.Quantity {
	if workload.Cmp(model) >= 0 {
		return workload.DeepCopy()
	}
	return model.DeepCopy()
}

func ArtifactDigestFromOCIReference(reference string) (string, error) {
	parsed, err := url.Parse(reference)
	if err != nil || !ociReferencePattern.MatchString(reference) || parsed.Scheme != "oci" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid OCI reference %q", reference)
	}
	at := strings.LastIndex(parsed.Path, "@")
	if at < 0 || !sha256Pattern.MatchString(parsed.Path[at+1:]) {
		return "", fmt.Errorf("OCI reference %q is not pinned by digest", reference)
	}
	return parsed.Path[at+1:], nil
}
