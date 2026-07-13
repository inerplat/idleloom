package v1alpha1

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestValidateWorkload(t *testing.T) {
	workload := validWorkload()
	if err := ValidateWorkload(&workload); err != nil {
		t.Fatalf("ValidateWorkload: %v", err)
	}
	workload.Spec.Model.CatalogRef = "https://models.example/qwen"
	workload.Spec.Resources.UnifiedMemoryRequest = resource.MustParse("0")
	if err := ValidateWorkload(&workload); err == nil {
		t.Fatal("ValidateWorkload accepted an arbitrary model reference and zero memory")
	}
}

func TestValidateShellWorkload(t *testing.T) {
	workload := IdleloomWorkload{Spec: IdleloomWorkloadSpec{
		Mode: WorkloadModeShell,
		Shell: &WorkloadShell{
			Script: "echo ready", Isolation: ShellIsolationHost,
			Network: ShellNetworkOutbound, TimeoutSeconds: 30,
		},
		Resources: WorkloadResources{UnifiedMemoryRequest: resource.MustParse("1Gi")},
	}}
	if err := ValidateWorkload(&workload); err != nil {
		t.Fatalf("ValidateWorkload: %v", err)
	}
	workload.Spec.Model = &WorkloadModelReference{CatalogRef: "qwen-approved"}
	if err := ValidateWorkload(&workload); err == nil {
		t.Fatal("ValidateWorkload accepted both shell and model execution")
	}
	workload.Spec.Model = nil
	workload.Spec.Shell.Network = ShellNetworkNone
	if err := ValidateWorkload(&workload); err == nil {
		t.Fatal("ValidateWorkload accepted host isolation with unenforceable network isolation")
	}
}

func TestValidateBatchInferenceWorkload(t *testing.T) {
	workload := validWorkload()
	workload.Spec.Mode = WorkloadModeBatch
	workload.Spec.Batch = &WorkloadBatchInference{Prompt: "Explain Kubernetes in one sentence.", MaxTokens: 64}
	if err := ValidateWorkload(&workload); err != nil {
		t.Fatalf("ValidateWorkload: %v", err)
	}
	workload.Spec.Batch.Prompt = ""
	workload.Spec.Batch.MaxTokens = 0
	if err := ValidateWorkload(&workload); err == nil {
		t.Fatal("ValidateWorkload accepted an empty batch request")
	}
}

func TestValidateConnectedServerWorkload(t *testing.T) {
	workload := validWorkload()
	workload.Spec.Server = &WorkloadServer{ServiceName: "qwen-chat", ModelAlias: "qwen3-5-0-8b"}
	if err := ValidateWorkload(&workload); err != nil {
		t.Fatalf("ValidateWorkload: %v", err)
	}
	workload.Spec.Server.ServiceName = "Invalid_Name"
	if err := ValidateWorkload(&workload); err == nil {
		t.Fatal("ValidateWorkload accepted an invalid serving Service name")
	}
	workload.Spec.Mode = WorkloadModeBatch
	workload.Spec.Server = &WorkloadServer{ServiceName: "qwen-chat", ModelAlias: "qwen3-5-0-8b"}
	workload.Spec.Batch = &WorkloadBatchInference{Prompt: "hello", MaxTokens: 8}
	if err := ValidateWorkload(&workload); err == nil {
		t.Fatal("ValidateWorkload accepted server and batch intents together")
	}
}

func TestValidateModelRejectsMutableOrCredentialedArtifact(t *testing.T) {
	model := validModel()
	if err := ValidateModel(&model); err != nil {
		t.Fatalf("ValidateModel: %v", err)
	}
	for _, reference := range []string{
		"oci://registry.example/models/qwen:latest",
		"oci://user:token@registry.example/models/qwen@" + digest(),
		"oci://registry.example/models/qwen@" + digest() + "?token=secret",
	} {
		model := validModel()
		model.Spec.Artifact.OCIReference = reference
		if err := ValidateModel(&model); err == nil {
			t.Fatalf("ValidateModel accepted unsafe reference %q", reference)
		}
	}
}

func TestValidateModelRequiresConservativeMemoryReservation(t *testing.T) {
	model := validModel()
	model.Spec.MinimumUnifiedMemory = resource.MustParse("4Gi")
	if err := ValidateModel(&model); err == nil {
		t.Fatal("ValidateModel accepted memory that excludes runtime and context overhead")
	}
	model = validModel()
	model.Spec.MaxContextLength = 16384
	if err := ValidateModel(&model); err == nil {
		t.Fatal("ValidateModel accepted an unbounded context length")
	}
}

func TestValidateAssignment(t *testing.T) {
	model := validModel()
	assignment := IdleloomWorkloadAssignment{Spec: IdleloomWorkloadAssignmentSpec{
		DesiredState: AssignmentDesiredRunning,
		WorkloadRef:  WorkloadObjectReference{Namespace: "default", Name: "qwen", UID: types.UID("workload-uid"), Generation: 1},
		HostRef:      ObjectReference{Name: "studio", UID: types.UID("host-uid")},
		Model: &ResolvedModel{
			CatalogRef: ObjectReference{Name: model.Name, UID: types.UID("model-uid")},
			Family:     model.Spec.Family, RuntimeProfile: model.Spec.RuntimeProfile,
			Artifact: model.Spec.Artifact, UnifiedMemoryRequest: resource.MustParse("16Gi"),
			MaxContextLength: model.Spec.MaxContextLength, MaxConcurrentRequests: model.Spec.MaxConcurrentRequests,
		},
		ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 1, LeaseDurationSeconds: 30,
	}}
	if err := ValidateAssignment(&assignment); err != nil {
		t.Fatalf("ValidateAssignment: %v", err)
	}
	assignment.Spec.ExecutionID = "reused-process"
	if err := ValidateAssignment(&assignment); err == nil {
		t.Fatal("ValidateAssignment accepted a non-UUID execution ID")
	}
}

func TestValidateServingAssignment(t *testing.T) {
	model := validModel()
	assignment := IdleloomWorkloadAssignment{Spec: IdleloomWorkloadAssignmentSpec{
		DesiredState: AssignmentDesiredRunning,
		WorkloadRef:  WorkloadObjectReference{Namespace: "default", Name: "qwen", UID: types.UID("workload-uid"), Generation: 1},
		HostRef:      ObjectReference{Name: "studio", UID: types.UID("host-uid")},
		Model: &ResolvedModel{
			CatalogRef: ObjectReference{Name: model.Name, UID: types.UID("model-uid")},
			Family:     model.Spec.Family, RuntimeProfile: model.Spec.RuntimeProfile,
			Artifact: model.Spec.Artifact, UnifiedMemoryRequest: resource.MustParse("16Gi"),
			MaxContextLength: model.Spec.MaxContextLength, MaxConcurrentRequests: model.Spec.MaxConcurrentRequests,
			Server: &ResolvedServer{
				ServiceName: "qwen-chat", ModelAlias: "qwen3-5-0-8b",
				AuthSecretName: ServingAuthSecretName, Port: NativeServingPort,
			},
		},
		ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 1, LeaseDurationSeconds: 30,
	}}
	if err := ValidateAssignment(&assignment); err != nil {
		t.Fatalf("ValidateAssignment: %v", err)
	}
	assignment.Spec.Model.Server.AuthSecretName = "user-secret"
	if err := ValidateAssignment(&assignment); err == nil {
		t.Fatal("ValidateAssignment accepted an untrusted serving Secret name")
	}
}

func TestValidateStopAcknowledgementRequiresExactExecution(t *testing.T) {
	model := validModel()
	assignment := &IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("assignment-uid"), Generation: 2},
		Spec: IdleloomWorkloadAssignmentSpec{
			DesiredState: AssignmentDesiredStopped,
			WorkloadRef:  WorkloadObjectReference{Namespace: "default", Name: "qwen", UID: types.UID("workload-uid"), Generation: 1},
			HostRef:      ObjectReference{Name: "host", UID: types.UID("host-uid")},
			Model: &ResolvedModel{
				CatalogRef: ObjectReference{Name: model.Name, UID: types.UID("model-uid")},
				Family:     model.Spec.Family, RuntimeProfile: model.Spec.RuntimeProfile,
				Artifact: model.Spec.Artifact, UnifiedMemoryRequest: resource.MustParse("16Gi"),
				MaxContextLength: model.Spec.MaxContextLength, MaxConcurrentRequests: model.Spec.MaxConcurrentRequests,
			},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 9, LeaseDurationSeconds: 30,
		},
	}
	now := metav1.NowMicro()
	assignment.Status = IdleloomWorkloadAssignmentStatus{
		ObservedGeneration: 2,
		Phase:              "Stopped",
		AgentID:            "agent.native",
		ExecutionID:        assignment.Spec.ExecutionID,
		FencingEpoch:       assignment.Spec.FencingEpoch,
		LastHeartbeatTime:  &now,
		StopAcknowledgement: &StopAcknowledgement{
			AssignmentUID:      assignment.UID,
			ObservedGeneration: 2,
			ExecutionID:        assignment.Spec.ExecutionID,
			FencingEpoch:       assignment.Spec.FencingEpoch,
			StoppedAt:          now,
		},
	}
	if err := ValidateStopAcknowledgement(assignment); err != nil {
		t.Fatal(err)
	}
	assignment.Status.StopAcknowledgement.ExecutionID = "223e4567-e89b-42d3-a456-426614174000"
	if err := ValidateStopAcknowledgement(assignment); err == nil {
		t.Fatal("expected mismatched execution ID to be rejected")
	}
}

func TestEffectiveUnifiedMemoryRequest(t *testing.T) {
	got := EffectiveUnifiedMemoryRequest(resource.MustParse("8Gi"), resource.MustParse("12Gi"))
	if got.Cmp(resource.MustParse("12Gi")) != 0 {
		t.Fatalf("EffectiveUnifiedMemoryRequest = %s, want 12Gi", got.String())
	}
}

func validWorkload() IdleloomWorkload {
	return IdleloomWorkload{Spec: IdleloomWorkloadSpec{
		Mode:      WorkloadModeServer,
		Model:     &WorkloadModelReference{CatalogRef: "qwen-approved"},
		Resources: WorkloadResources{UnifiedMemoryRequest: resource.MustParse("16Gi")},
	}}
}

func validModel() IdleloomModel {
	return IdleloomModel{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen-approved"},
		Spec: IdleloomModelSpec{
			Family:         ModelFamilyQwen35,
			RuntimeProfile: RuntimeProfileMLXLMV1,
			Artifact: ModelArtifact{
				OCIReference:   "oci://registry.example/models/qwen@" + digest(),
				ManifestDigest: digest(),
				Format:         ArtifactFormatSafetensorsV1,
				SizeBytes:      1024,
				Signature:      SignaturePolicy{Issuer: "https://issuer.example", Subject: "model-publisher"},
			},
			MinimumUnifiedMemory:  resource.MustParse("12Gi"),
			MaxContextLength:      2048,
			MaxConcurrentRequests: 1,
		},
	}
}

func digest() string {
	return "sha256:" + strings.Repeat("a", 64)
}
