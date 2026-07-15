package v1alpha1

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

var (
	sha256Pattern       = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	uuidPattern         = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	ociReferencePattern = regexp.MustCompile(`^oci://[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$`)
	ollamaModelPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}:[a-z0-9][a-z0-9._-]{0,63}$`)
	ggufFilePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,190}\.gguf$`)
	runParameterPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,62}$`)
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
		if workload.Spec.Model == nil || workload.Spec.Server == nil || workload.Spec.Batch != nil || workload.Spec.Shell != nil || workload.Spec.Train != nil {
			errs = append(errs, field.Invalid(specPath, workload.Spec, "Server mode requires model and server and forbids batch, shell, and train"))
		} else {
			if problems := validation.IsDNS1123Subdomain(workload.Spec.Model.CatalogRef); len(problems) > 0 {
				errs = append(errs, field.Invalid(specPath.Child("model", "catalogRef"), workload.Spec.Model.CatalogRef, strings.Join(problems, "; ")))
			}
			errs = append(errs, validateWorkloadServer(workload.Spec.Server, specPath.Child("server"))...)
		}
	case WorkloadModeBatch:
		if workload.Spec.Model == nil || workload.Spec.Server != nil || workload.Spec.Batch == nil || workload.Spec.Shell != nil || workload.Spec.Train != nil {
			errs = append(errs, field.Invalid(specPath, workload.Spec, "Batch mode requires model and batch and forbids server and shell"))
		} else {
			if problems := validation.IsDNS1123Subdomain(workload.Spec.Model.CatalogRef); len(problems) > 0 {
				errs = append(errs, field.Invalid(specPath.Child("model", "catalogRef"), workload.Spec.Model.CatalogRef, strings.Join(problems, "; ")))
			}
			errs = append(errs, validateBatchInference(workload.Spec.Batch, specPath.Child("batch"))...)
		}
	case WorkloadModeShell:
		if workload.Spec.Shell == nil || workload.Spec.Model != nil || workload.Spec.Server != nil || workload.Spec.Batch != nil || workload.Spec.Train != nil {
			errs = append(errs, field.Invalid(specPath, workload.Spec, "Shell mode requires shell and forbids model, server, and batch"))
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
	case WorkloadModeTrain:
		if workload.Spec.Train == nil || workload.Spec.Model != nil || workload.Spec.Server != nil || workload.Spec.Batch != nil || workload.Spec.Shell != nil {
			errs = append(errs, field.Invalid(specPath, workload.Spec, "Train mode requires train and forbids model, server, batch, and shell"))
		} else {
			errs = append(errs, validateTraining(workload.Spec.Train, specPath.Child("train"))...)
			if workload.Spec.Run == nil || workload.Spec.Run.Task != "train" {
				errs = append(errs, field.Invalid(specPath.Child("run"), workload.Spec.Run, "Train mode requires run.task=train"))
			}
		}
	default:
		errs = append(errs, field.NotSupported(specPath.Child("mode"), workload.Spec.Mode, []string{WorkloadModeServer, WorkloadModeBatch, WorkloadModeShell, WorkloadModeTrain}))
	}
	if workload.Spec.Run != nil {
		errs = append(errs, validateRunSpec(workload.Spec.Run, specPath.Child("run"))...)
		expectedTask := map[string]string{
			WorkloadModeServer: "serve", WorkloadModeBatch: "infer", WorkloadModeShell: "shell", WorkloadModeTrain: "train",
		}[workload.Spec.Mode]
		if expectedTask != "" && workload.Spec.Run.Task != expectedTask {
			errs = append(errs, field.Invalid(specPath.Child("run", "task"), workload.Spec.Run.Task, fmt.Sprintf("must be %q for %s mode", expectedTask, workload.Spec.Mode)))
		}
	}
	if workload.Spec.Resources.UnifiedMemoryRequest.Sign() <= 0 {
		errs = append(errs, field.Invalid(specPath.Child("resources", "unifiedMemoryRequest"), workload.Spec.Resources.UnifiedMemoryRequest.String(), "must be positive"))
	}
	return errs.ToAggregate()
}

func ValidateModel(model *IdleloomModel) error {
	var errs field.ErrorList
	specPath := field.NewPath("spec")
	if model.Spec.Family != ModelFamilyQwen35 && model.Spec.Family != ModelFamilyOllamaGGUF && model.Spec.Family != ModelFamilyGGUF {
		errs = append(errs, field.NotSupported(specPath.Child("family"), model.Spec.Family, []string{ModelFamilyQwen35, ModelFamilyOllamaGGUF, ModelFamilyGGUF}))
	}
	if model.Spec.RuntimeProfile != RuntimeProfileMLXLMV1 && model.Spec.RuntimeProfile != RuntimeProfileOllamaGGUFV1 && model.Spec.RuntimeProfile != RuntimeProfileLlamaCppMetalV1 {
		errs = append(errs, field.NotSupported(specPath.Child("runtimeProfile"), model.Spec.RuntimeProfile, []string{RuntimeProfileMLXLMV1, RuntimeProfileOllamaGGUFV1, RuntimeProfileLlamaCppMetalV1}))
	}
	if model.Spec.RuntimeProfile == RuntimeProfileMLXLMV1 && model.Spec.Family != ModelFamilyQwen35 {
		errs = append(errs, field.Invalid(specPath.Child("family"), model.Spec.Family, "mlx-lm-v1 requires qwen3.5"))
	}
	if model.Spec.RuntimeProfile == RuntimeProfileOllamaGGUFV1 && model.Spec.Family != ModelFamilyOllamaGGUF {
		errs = append(errs, field.Invalid(specPath.Child("family"), model.Spec.Family, "ollama-gguf-v1 requires ollama-gguf"))
	}
	if model.Spec.RuntimeProfile == RuntimeProfileLlamaCppMetalV1 && model.Spec.Family != ModelFamilyGGUF {
		errs = append(errs, field.Invalid(specPath.Child("family"), model.Spec.Family, "llama-cpp-metal-v1 requires gguf"))
	}
	errs = append(errs, validateArtifact(model.Spec.Artifact, model.Spec.RuntimeProfile, specPath.Child("artifact"))...)
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
	if assignment.Spec.Run != nil {
		errs = append(errs, validateRunSpec(assignment.Spec.Run, specPath.Child("run"))...)
	}
	intents := 0
	for _, present := range []bool{assignment.Spec.Model != nil, assignment.Spec.Shell != nil, assignment.Spec.Training != nil} {
		if present {
			intents++
		}
	}
	if intents != 1 {
		errs = append(errs, field.Invalid(specPath, assignment.Spec, "exactly one model, shell, or training run is required"))
	} else if assignment.Spec.Model != nil {
		if assignment.Spec.Run != nil {
			expectedTask := "serve"
			if assignment.Spec.Model.Batch != nil {
				expectedTask = "infer"
			}
			if assignment.Spec.Run.Task != expectedTask {
				errs = append(errs, field.Invalid(specPath.Child("run", "task"), assignment.Spec.Run.Task, fmt.Sprintf("must be %q for the resolved model intent", expectedTask)))
			}
		}
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
		if assignment.Spec.Model.Batch != nil {
			errs = append(errs, validateBatchInference(assignment.Spec.Model.Batch, specPath.Child("model", "batch"))...)
		}
		if assignment.Spec.Model.Server != nil {
			errs = append(errs, validateResolvedServer(assignment.Spec.Model.Server, specPath.Child("model", "server"))...)
		}
		if (assignment.Spec.Model.Batch == nil) == (assignment.Spec.Model.Server == nil) {
			errs = append(errs, field.Invalid(specPath.Child("model"), assignment.Spec.Model, "exactly one batch or server intent is required"))
		}
	} else if assignment.Spec.Shell != nil {
		if assignment.Spec.Run != nil && assignment.Spec.Run.Task != "shell" {
			errs = append(errs, field.Invalid(specPath.Child("run", "task"), assignment.Spec.Run.Task, "must be shell for the resolved shell intent"))
		}
		if strings.TrimSpace(assignment.Spec.Shell.Script) == "" || strings.ContainsRune(assignment.Spec.Shell.Script, '\x00') {
			errs = append(errs, field.Invalid(specPath.Child("shell", "script"), assignment.Spec.Shell.Script, "must contain a non-empty script without NUL bytes"))
		}
		if len([]byte(assignment.Spec.Shell.Script)) > 64<<10 {
			errs = append(errs, field.TooLong(specPath.Child("shell", "script"), "", 64<<10))
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
	} else {
		errs = append(errs, validateResolvedTraining(assignment.Spec.Training, specPath.Child("training"))...)
		if assignment.Spec.Run == nil || assignment.Spec.Run.Task != "train" {
			errs = append(errs, field.Invalid(specPath.Child("run"), assignment.Spec.Run, "resolved training requires run.task=train"))
		}
	}
	return errs.ToAggregate()
}

func validateRunSpec(run *WorkloadRunSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if run == nil {
		return append(errs, field.Required(path, "run metadata is required"))
	}
	if run.Task != "train" && run.Task != "infer" && run.Task != "serve" && run.Task != "shell" {
		errs = append(errs, field.NotSupported(path.Child("task"), run.Task, []string{"train", "infer", "serve", "shell"}))
	}
	if problems := validation.IsDNS1123Label(run.Experiment); len(problems) > 0 {
		errs = append(errs, field.Invalid(path.Child("experiment"), run.Experiment, strings.Join(problems, "; ")))
	}
	if run.Attempt < 1 || run.Attempt > 1000 {
		errs = append(errs, field.Invalid(path.Child("attempt"), run.Attempt, "must be between 1 and 1000"))
	}
	if len(run.Parameters) > 64 {
		errs = append(errs, field.TooMany(path.Child("parameters"), len(run.Parameters), 64))
	}
	for name, value := range run.Parameters {
		parameterPath := path.Child("parameters").Key(name)
		if !runParameterPattern.MatchString(name) || strings.HasPrefix(name, "IDLELOOM_") {
			errs = append(errs, field.Invalid(parameterPath, name, "must be a safe environment name and must not use the IDLELOOM_ prefix"))
		}
		text := string(value)
		if utf8.RuneCountInString(text) > 4096 || strings.ContainsRune(text, '\x00') {
			errs = append(errs, field.Invalid(parameterPath, "", "must not contain NUL bytes or exceed 4096 characters"))
		}
	}
	return errs
}

func validateTraining(training *WorkloadTraining, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if training == nil {
		return append(errs, field.Required(path, "training intent is required"))
	}
	if training.RuntimeProfile != RuntimeProfileMLXTrainV1 {
		errs = append(errs, field.NotSupported(path.Child("runtimeProfile"), training.RuntimeProfile, []string{RuntimeProfileMLXTrainV1}))
	}
	if training.Source.Inline == "" || strings.ContainsRune(training.Source.Inline, '\x00') {
		errs = append(errs, field.Invalid(path.Child("source", "inline"), "", "must contain a non-empty Python program without NUL bytes"))
	}
	if len([]byte(training.Source.Inline)) > 64<<10 {
		errs = append(errs, field.TooLong(path.Child("source", "inline"), "", 64<<10))
	}
	if training.Network != "" && training.Network != ShellNetworkNone && training.Network != ShellNetworkOutbound {
		errs = append(errs, field.NotSupported(path.Child("network"), training.Network, []string{ShellNetworkNone, ShellNetworkOutbound}))
	}
	if training.TimeoutSeconds < 0 || training.TimeoutSeconds > 86400 {
		errs = append(errs, field.Invalid(path.Child("timeoutSeconds"), training.TimeoutSeconds, "must be between 1 and 86400 when set"))
	}
	return errs
}

func validateResolvedTraining(training *ResolvedTraining, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if training == nil {
		return append(errs, field.Required(path, "resolved training run is required"))
	}
	errs = append(errs, validateTraining(&WorkloadTraining{
		RuntimeProfile: training.RuntimeProfile,
		Source:         WorkloadTrainingSource{Inline: training.Source},
		Network:        training.Network,
		TimeoutSeconds: training.TimeoutSeconds,
	}, path)...)
	if training.TimeoutSeconds < 1 || training.TimeoutSeconds > 86400 {
		errs = append(errs, field.Invalid(path.Child("timeoutSeconds"), training.TimeoutSeconds, "must be between 1 and 86400"))
	}
	if !sha256Pattern.MatchString(training.SourceDigest) {
		errs = append(errs, field.Invalid(path.Child("sourceDigest"), training.SourceDigest, "must be a sha256 digest"))
	}
	if training.UnifiedMemoryRequest.Sign() <= 0 {
		errs = append(errs, field.Invalid(path.Child("unifiedMemoryRequest"), training.UnifiedMemoryRequest.String(), "must be positive"))
	}
	return errs
}

func validateWorkloadServer(server *WorkloadServer, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if server == nil {
		return errs
	}
	if problems := validation.IsDNS1035Label(server.ServiceName); len(problems) > 0 {
		errs = append(errs, field.Invalid(path.Child("serviceName"), server.ServiceName, strings.Join(problems, "; ")))
	}
	if problems := validation.IsDNS1123Subdomain(server.ModelAlias); len(problems) > 0 {
		errs = append(errs, field.Invalid(path.Child("modelAlias"), server.ModelAlias, strings.Join(problems, "; ")))
	}
	return errs
}

func validateResolvedServer(server *ResolvedServer, path *field.Path) field.ErrorList {
	errs := validateWorkloadServer(&WorkloadServer{ServiceName: server.ServiceName, ModelAlias: server.ModelAlias}, path)
	if server.AuthSecretName != ServingAuthSecretName {
		errs = append(errs, field.Invalid(path.Child("authSecretName"), server.AuthSecretName, fmt.Sprintf("must be %q", ServingAuthSecretName)))
	}
	if server.Port != NativeServingPort {
		errs = append(errs, field.Invalid(path.Child("port"), server.Port, fmt.Sprintf("must be %d", NativeServingPort)))
	}
	return errs
}

func validateBatchInference(batch *WorkloadBatchInference, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if batch == nil {
		return append(errs, field.Required(path, "batch inference is required"))
	}
	if strings.TrimSpace(batch.Prompt) == "" || strings.ContainsRune(batch.Prompt, '\x00') {
		errs = append(errs, field.Invalid(path.Child("prompt"), batch.Prompt, "must contain a non-empty prompt without NUL bytes"))
	}
	if len([]byte(batch.Prompt)) > 16<<10 {
		errs = append(errs, field.TooLong(path.Child("prompt"), "", 16<<10))
	}
	if batch.MaxTokens < 1 || batch.MaxTokens > 512 {
		errs = append(errs, field.Invalid(path.Child("maxTokens"), batch.MaxTokens, "must be between 1 and 512"))
	}
	if batch.TimeoutSeconds < 0 || batch.TimeoutSeconds > 3600 {
		errs = append(errs, field.Invalid(path.Child("timeoutSeconds"), batch.TimeoutSeconds, "must be between 1 and 3600 when set"))
	}
	return errs
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

func validateArtifact(artifact ModelArtifact, runtimeProfile string, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if artifact.OCIReference != "" {
		parsed, err := url.Parse(artifact.OCIReference)
		if err != nil || !ociReferencePattern.MatchString(artifact.OCIReference) || parsed.Scheme != "oci" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			errs = append(errs, field.Invalid(path.Child("ociReference"), artifact.OCIReference, "must be a credential-free OCI reference pinned by digest"))
		} else {
			at := strings.LastIndex(parsed.Path, "@")
			if at <= 1 || !sha256Pattern.MatchString(parsed.Path[at+1:]) {
				errs = append(errs, field.Invalid(path.Child("ociReference"), artifact.OCIReference, "must end in @sha256:<64 lowercase hex characters>"))
			}
		}
	}
	if artifact.OllamaModel != "" && !ollamaModelPattern.MatchString(artifact.OllamaModel) {
		errs = append(errs, field.Invalid(path.Child("ollamaModel"), artifact.OllamaModel, "must be an unqualified lowercase Ollama model and tag"))
	}
	if artifact.GGUFFile != "" && !ggufFilePattern.MatchString(artifact.GGUFFile) {
		errs = append(errs, field.Invalid(path.Child("ggufFile"), artifact.GGUFFile, "must be a safe GGUF filename without a path"))
	}
	sources := 0
	for _, present := range []bool{artifact.OCIReference != "", artifact.OllamaModel != "", artifact.GGUFFile != ""} {
		if present {
			sources++
		}
	}
	if sources != 1 {
		errs = append(errs, field.Invalid(path, artifact, "exactly one OCI, Ollama, or direct GGUF source is required"))
	}
	if !sha256Pattern.MatchString(artifact.ManifestDigest) {
		errs = append(errs, field.Invalid(path.Child("manifestDigest"), artifact.ManifestDigest, "must be sha256:<64 lowercase hex characters>"))
	}
	switch runtimeProfile {
	case RuntimeProfileMLXLMV1:
		if artifact.OCIReference == "" || artifact.Format != ArtifactFormatSafetensorsV1 || artifact.Signature == nil {
			errs = append(errs, field.Invalid(path, artifact, "mlx-lm-v1 requires a signed OCI mlx-safetensors-v1 artifact"))
		}
	case RuntimeProfileOllamaGGUFV1:
		if artifact.OllamaModel == "" || artifact.Format != ArtifactFormatGGUFV1 || artifact.Signature != nil {
			errs = append(errs, field.Invalid(path, artifact, "ollama-gguf-v1 requires an unsigned local Ollama gguf-v1 artifact pinned by manifest digest"))
		}
	case RuntimeProfileLlamaCppMetalV1:
		if artifact.GGUFFile == "" || artifact.Format != ArtifactFormatGGUFV1 || artifact.Signature != nil {
			errs = append(errs, field.Invalid(path, artifact, "llama-cpp-metal-v1 requires an unsigned managed GGUF file pinned by SHA-256"))
		}
	}
	if artifact.SizeBytes <= 0 {
		errs = append(errs, field.Invalid(path.Child("sizeBytes"), artifact.SizeBytes, "must be positive"))
	} else if artifact.SizeBytes > maxArtifactBytes {
		errs = append(errs, field.Invalid(path.Child("sizeBytes"), artifact.SizeBytes, fmt.Sprintf("must not exceed %d bytes", maxArtifactBytes)))
	}
	if artifact.Signature != nil && (strings.TrimSpace(artifact.Signature.Issuer) == "" || strings.TrimSpace(artifact.Signature.Subject) == "") {
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
		return "", fmt.Errorf("the OCI reference %q is not pinned by digest", reference)
	}
	return parsed.Path[at+1:], nil
}
