package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/devruntime"
	"github.com/inerplat/idleloom/internal/native/execution"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	"github.com/inerplat/idleloom/internal/native/kubeletbridge"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestEnsureProcessRejectsExitedProcess(t *testing.T) {
	process := &fakeProcess{}
	agent := &DevAgent{process: process}
	err := agent.ensureProcess(context.Background(), &nativev1alpha1.IdleloomWorkloadAssignment{})
	if !errors.Is(err, ErrProcessExited) {
		t.Fatalf("ensureProcess error = %v, want ErrProcessExited", err)
	}
	if process.stopCalls != 1 {
		t.Fatalf("Stop calls = %d, want 1", process.stopCalls)
	}
	if agent.process != nil {
		t.Fatal("exited process remained attached to agent")
	}
}

func TestProcessRunningForRequiresMatchingLiveExecution(t *testing.T) {
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("assignment-uid")},
		Spec:       nativev1alpha1.IdleloomWorkloadAssignmentSpec{ExecutionID: "123e4567-e89b-42d3-a456-426614174000"},
	}
	agent := &DevAgent{process: &fakeProcess{alive: true}, assignment: assignment.DeepCopy()}
	if !agent.processRunningFor(assignment) {
		t.Fatal("matching live execution was not recognized")
	}
	changed := assignment.DeepCopy()
	changed.Spec.ExecutionID = "223e4567-e89b-42d3-a456-426614174000"
	if agent.processRunningFor(changed) {
		t.Fatal("different execution was treated as already running")
	}
	agent.process = &fakeProcess{}
	if agent.processRunningFor(assignment) {
		t.Fatal("exited process was treated as running")
	}
}

func TestHealthAndGenerateRejectExitedProcess(t *testing.T) {
	agent := &DevAgent{process: &fakeProcess{}}
	health := httptest.NewRecorder()
	agent.handleHealth(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusServiceUnavailable {
		t.Fatalf("health status = %d, want %d", health.Code, http.StatusServiceUnavailable)
	}
	generate := httptest.NewRecorder()
	agent.handleGenerate(generate, httptest.NewRequest(http.MethodPost, "/v1/generate", strings.NewReader(`{"prompt":"hello"}`)))
	if generate.Code != http.StatusServiceUnavailable {
		t.Fatalf("generate status = %d, want %d", generate.Code, http.StatusServiceUnavailable)
	}
}

func TestClosePropagatesProcessStopError(t *testing.T) {
	stopErr := errors.New("stop failed")
	agent := &DevAgent{
		config:  DevAgentConfig{StateDirectory: t.TempDir()},
		process: &fakeProcess{alive: true, stopErr: stopErr},
	}
	if err := agent.Close(); !errors.Is(err, stopErr) {
		t.Fatalf("Close error = %v, want stop failure", err)
	}
}

func TestEvaluateConnectivityKeepsAPIOnlyComputeIndependent(t *testing.T) {
	status, condition := evaluateConnectivity(time.Unix(1_800_000_000, 0), nil)
	if status.Mode != nativev1alpha1.ConnectivityModeAPIOnly || condition.Status != metav1.ConditionFalse || condition.Reason != "APIOnly" {
		t.Fatalf("status=%#v condition=%#v", status, condition)
	}
}

func TestEvaluateConnectivityUsesHealthyRelaySessionInsteadOfHandshakeFreshness(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	handshake := metav1.NewMicroTime(now.Add(-time.Minute))
	status, condition := evaluateConnectivity(now, func() (nativev1alpha1.HostConnectivityStatus, error) {
		return nativev1alpha1.HostConnectivityStatus{
			Mode: nativev1alpha1.ConnectivityModeWireKubeLeaf, Provider: nativev1alpha1.ConnectivityProviderWireKube,
			Transport: nativev1alpha1.ConnectivityTransportRelay, LastHandshakeTime: &handshake,
		}, nil
	})
	if status.Mode != nativev1alpha1.ConnectivityModeWireKubeLeaf || condition.Status != metav1.ConditionTrue || condition.Reason != "WireKubeRelaySessionReady" {
		t.Fatalf("status=%#v condition=%#v", status, condition)
	}
	stale := metav1.NewMicroTime(now.Add(-4 * time.Minute))
	_, condition = evaluateConnectivity(now, func() (nativev1alpha1.HostConnectivityStatus, error) {
		return nativev1alpha1.HostConnectivityStatus{Mode: nativev1alpha1.ConnectivityModeWireKubeLeaf, LastHandshakeTime: &stale}, nil
	})
	if condition.Status != metav1.ConditionTrue || condition.Reason != "WireKubeRelaySessionReady" {
		t.Fatalf("stale condition = %#v", condition)
	}
}

func TestCompletedShellIsTerminalAndKeepsLogsAddressable(t *testing.T) {
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("assignment-uid"), Generation: 2},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef:  nativev1alpha1.WorkloadObjectReference{Namespace: "default"},
			Shell:        &nativev1alpha1.ResolvedShell{Script: "echo ready"},
			ExecutionID:  "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 4,
		},
		Status: nativev1alpha1.IdleloomWorkloadAssignmentStatus{
			ObservedGeneration: 2, Phase: nativev1alpha1.PhaseSucceeded, AgentID: "studio.native",
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 4,
		},
	}
	agent := &DevAgent{config: DevAgentConfig{AgentID: "studio.native"}, assignment: assignment}
	if phase, terminal := agent.completedAssignmentPhase(assignment); !terminal || phase != nativev1alpha1.PhaseSucceeded {
		t.Fatalf("completedAssignmentPhase = %q, %t", phase, terminal)
	}
	if target, ok := agent.resolveLogTarget(); !ok || target.AssignmentUID != string(assignment.UID) {
		t.Fatalf("resolveLogTarget = %#v, %t", target, ok)
	}
}

func TestServerRetryKeepsEarlierAssignmentLogs(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("assignment-uid")},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000",
		},
	}
	logs := kubeletbridge.NewLogBuffer(1 << 20)
	if err := logs.Reset(string(assignment.UID), now, "first start"); err != nil {
		t.Fatal(err)
	}
	if err := logs.Append(now.Add(time.Second), "server crashed"); err != nil {
		t.Fatal(err)
	}
	agent := &DevAgent{config: DevAgentConfig{Now: func() time.Time { return now.Add(2 * time.Second) }}, logs: logs}
	agent.beginAssignmentLog(assignment)
	entries := logs.Snapshot(string(assignment.UID), time.Time{}, -1)
	if len(entries) != 3 || entries[0].Message != "first start" || entries[1].Message != "server crashed" || !strings.Contains(entries[2].Message, "retry accepted") {
		t.Fatalf("retry logs = %#v", entries)
	}
}

func TestCompletedBatchIsTerminalButServerCanRetry(t *testing.T) {
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			Model:       &nativev1alpha1.ResolvedModel{Batch: &nativev1alpha1.WorkloadBatchInference{Prompt: "hello", MaxTokens: 8}},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 4,
		},
		Status: nativev1alpha1.IdleloomWorkloadAssignmentStatus{
			ObservedGeneration: 2, Phase: nativev1alpha1.PhaseFailed, AgentID: "studio.native",
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 4,
		},
	}
	agent := &DevAgent{config: DevAgentConfig{AgentID: "studio.native"}}
	if phase, terminal := agent.completedAssignmentPhase(assignment); !terminal || phase != nativev1alpha1.PhaseFailed {
		t.Fatalf("completed batch phase = %q, %t", phase, terminal)
	}
	assignment.Spec.Model.Batch = nil
	if phase, terminal := agent.completedAssignmentPhase(assignment); terminal || phase != "" {
		t.Fatalf("completed server phase = %q, %t", phase, terminal)
	}
}

func TestEnsureProcessClearsCrashedServerJournalForRetry(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("assignment-uid")},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{UID: types.UID("workload-uid"), Generation: 1},
			Model: &nativev1alpha1.ResolvedModel{Server: &nativev1alpha1.ResolvedServer{
				ServiceName: "qwen-chat", ModelAlias: "qwen3-5-0-8b",
			}},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 4,
		},
	}
	planned := execution.Record{
		SchemaVersion: execution.SchemaVersionV1, WorkloadUID: "workload-uid", WorkloadGeneration: 1,
		AssignmentUID: "assignment-uid", ExecutionID: assignment.Spec.ExecutionID, FencingEpoch: 4,
		Executable: "/runtime/python", RuntimeVersion: devruntime.RuntimeVersion, Nonce: "old-nonce",
	}
	if err := store.Begin(planned); err != nil {
		t.Fatal(err)
	}
	running := planned
	running.PID = 123
	running.ProcessStartToken = "start"
	if err := store.UpdateProcess(planned, running); err != nil {
		t.Fatal(err)
	}
	process := &fakeProcess{}
	agent := &DevAgent{
		config: DevAgentConfig{Platform: fakeAgentPlatform{}},
		store:  store, process: process, assignment: assignment.DeepCopy(),
	}
	if err := agent.ensureProcess(context.Background(), assignment); !errors.Is(err, ErrProcessExited) {
		t.Fatalf("ensureProcess error = %v, want ErrProcessExited", err)
	}
	if current := store.Current(); current != nil {
		t.Fatalf("crashed server journal was not cleared: %#v", current)
	}
	retry := planned
	retry.Nonce = "new-nonce"
	if err := store.Begin(retry); err != nil {
		t.Fatalf("server retry could not reserve a new process identity: %v", err)
	}
}

func TestEnsureProcessClearsSelfFencedServerJournalForRetry(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("assignment-uid")},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{UID: types.UID("workload-uid"), Generation: 1},
			Model:       &nativev1alpha1.ResolvedModel{Server: &nativev1alpha1.ResolvedServer{}},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 4,
		},
	}
	planned := execution.Record{
		SchemaVersion: execution.SchemaVersionV1, WorkloadUID: "workload-uid", WorkloadGeneration: 1,
		AssignmentUID: "assignment-uid", ExecutionID: assignment.Spec.ExecutionID, FencingEpoch: 4,
		Executable: "/runtime/python", RuntimeVersion: devruntime.RuntimeVersion, Nonce: "old-nonce",
	}
	if err := store.Begin(planned); err != nil {
		t.Fatal(err)
	}
	running := planned
	running.PID = 123
	running.ProcessStartToken = "start"
	if err := store.UpdateProcess(planned, running); err != nil {
		t.Fatal(err)
	}
	agent := &DevAgent{
		config: DevAgentConfig{Platform: fakeAgentPlatform{}, Layout: devruntime.NewLayout(t.TempDir())},
		store:  store,
	}
	_ = agent.ensureProcess(context.Background(), assignment)
	if current := store.Current(); current != nil {
		t.Fatalf("self-fenced server journal was not cleared: %#v", current)
	}
}

func TestEnsureProcessDoesNotReplayDurablyCompletedShell(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("assignment-uid")},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{UID: types.UID("workload-uid"), Generation: 1},
			Shell:       &nativev1alpha1.ResolvedShell{Script: "echo ready"},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 4,
		},
	}
	planned := execution.Record{
		SchemaVersion: execution.SchemaVersionV1, WorkloadUID: "workload-uid", WorkloadGeneration: 1,
		AssignmentUID: "assignment-uid", ExecutionID: assignment.Spec.ExecutionID, FencingEpoch: 4,
		Executable: "/bin/zsh", RuntimeVersion: nativev1alpha1.RuntimeProfileShellV1, Nonce: "nonce",
	}
	if err := store.Begin(planned); err != nil {
		t.Fatal(err)
	}
	running := planned
	running.PID = 123
	running.ProcessStartToken = "start"
	if err := store.UpdateProcess(planned, running); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(running, nil); err != nil {
		t.Fatal(err)
	}
	agent := &DevAgent{store: store}
	if err := agent.ensureProcess(context.Background(), assignment); !errors.Is(err, ErrProcessCompleted) {
		t.Fatalf("ensureProcess error = %v, want durable completion", err)
	}
}

func TestHigherFencingEpochClearsCompletedJournalBeforeNextAssignment(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	oldAssignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("old-assignment")},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{UID: types.UID("old-workload"), Generation: 1},
			Shell:       &nativev1alpha1.ResolvedShell{Script: "echo old"},
			ExecutionID: "11111111-1111-4111-8111-111111111111", FencingEpoch: 4,
		},
	}
	planned := execution.Record{
		SchemaVersion: execution.SchemaVersionV1, WorkloadUID: "old-workload", WorkloadGeneration: 1,
		AssignmentUID: "old-assignment", ExecutionID: oldAssignment.Spec.ExecutionID, FencingEpoch: 4,
		Executable: "/bin/zsh", RuntimeVersion: nativev1alpha1.RuntimeProfileShellV1, Nonce: "old-nonce",
	}
	if err := store.Begin(planned); err != nil {
		t.Fatal(err)
	}
	running := planned
	running.PID = 123
	running.ProcessStartToken = "start"
	if err := store.UpdateProcess(planned, running); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(running, nil); err != nil {
		t.Fatal(err)
	}
	newAssignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("new-assignment")},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{UID: types.UID("new-workload"), Generation: 1},
			Shell:       &nativev1alpha1.ResolvedShell{Script: "echo new"},
			ExecutionID: "22222222-2222-4222-8222-222222222222", FencingEpoch: 5,
		},
	}
	agent := &DevAgent{
		config: DevAgentConfig{Platform: fakeAgentPlatform{}, Layout: devruntime.NewLayout(t.TempDir())},
		store:  store, assignment: oldAssignment,
		runStatus: &nativev1alpha1.WorkloadRunStatus{ID: oldAssignment.Spec.ExecutionID},
	}
	if err := agent.fenceSupersededExecution(context.Background(), newAssignment); err != nil {
		t.Fatal(err)
	}
	if current := store.Current(); current != nil {
		t.Fatalf("superseded execution journal remains: %#v", current)
	}
	if agent.assignment != nil || agent.runStatus != nil {
		t.Fatalf("superseded in-memory state remains: assignment=%#v run=%#v", agent.assignment, agent.runStatus)
	}
	newPlan := planned
	newPlan.WorkloadUID = "new-workload"
	newPlan.AssignmentUID = "new-assignment"
	newPlan.ExecutionID = newAssignment.Spec.ExecutionID
	newPlan.FencingEpoch = 5
	newPlan.Nonce = "new-nonce"
	if err := store.Begin(newPlan); err != nil {
		t.Fatalf("next assignment could not reserve the journal: %v", err)
	}
}

func TestEnsureProcessPreparesLockedModelOnFirstUse(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	descriptor, err := devruntime.LockedModel()
	if err != nil {
		t.Fatal(err)
	}
	prepareCalls := 0
	startCalls := 0
	agent := &DevAgent{
		store: store,
		logs:  kubeletbridge.NewLogBuffer(1 << 20),
		config: DevAgentConfig{
			AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()),
			PrepareRuntime: func(context.Context, func(string)) (devruntime.Receipt, error) {
				prepareCalls++
				return devruntime.Receipt{
					ArtifactIdentity: descriptor.ArtifactIdentity, ManifestDigest: descriptor.ManifestDigest,
					RuntimeVersion: devruntime.RuntimeVersion,
				}, nil
			},
			StartProcess: func(_ context.Context, _ devruntime.ProcessConfig) (Process, error) {
				startCalls++
				return &fakeProcess{alive: true}, nil
			},
		},
	}
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("assignment-uid"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{
				Namespace: "default", Name: "inference", UID: types.UID("workload-uid"), Generation: 1,
			},
			Model: &nativev1alpha1.ResolvedModel{
				Family: nativev1alpha1.ModelFamilyQwen35, RuntimeProfile: nativev1alpha1.RuntimeProfileMLXLMV1,
				Artifact: nativev1alpha1.ModelArtifact{OCIReference: descriptor.ArtifactIdentity, ManifestDigest: descriptor.ManifestDigest},
			},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 1, LeaseDurationSeconds: 30,
		},
	}
	if err := agent.ensureProcess(context.Background(), assignment); err != nil {
		t.Fatal(err)
	}
	if prepareCalls != 1 || startCalls != 1 || agent.process == nil || !agent.process.Alive() {
		t.Fatalf("prepareCalls=%d startCalls=%d process=%#v", prepareCalls, startCalls, agent.process)
	}
	if err := agent.stopProcess(); err != nil {
		t.Fatal(err)
	}
}

func TestHostAdvertisesModelCapabilityWhenRuntimeIsPreparable(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	host := &nativev1alpha1.IdleloomHost{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomHost"},
		ObjectMeta: metav1.ObjectMeta{Name: "host", Namespace: "idleloom-host-studio", Generation: 1},
		Spec:       nativev1alpha1.IdleloomHostSpec{AgentID: "studio.native"},
	}
	client := dynamicfake.NewSimpleDynamicClient(scheme, host)
	agent := &DevAgent{config: DevAgentConfig{
		Dynamic: client, AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()), Platform: fakeAgentPlatform{},
		PrepareRuntime: func(context.Context, func(string)) (devruntime.Receipt, error) { return devruntime.Receipt{}, nil },
	}}
	if err := agent.updateHostStatus(context.Background(), host, false, ""); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.HostsGVR).Namespace(host.Namespace).Get(context.Background(), host.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(updated.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileMLXLMV1) || !slices.Contains(updated.Status.ModelFamilies, nativev1alpha1.ModelFamilyQwen35) || !slices.Contains(updated.Status.Capabilities, nativev1alpha1.CapabilityBatchInferenceV1) {
		t.Fatalf("preparable capabilities = %#v", updated.Status)
	}
}

func TestHostAdvertisesExactLocalOllamaModel(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	host := &nativev1alpha1.IdleloomHost{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomHost"},
		ObjectMeta: metav1.ObjectMeta{Name: "host", Namespace: "idleloom-host-studio", Generation: 1},
		Spec:       nativev1alpha1.IdleloomHostSpec{AgentID: "studio.native"},
	}
	client := dynamicfake.NewSimpleDynamicClient(scheme, host)
	model := devruntime.OllamaModel{
		Name: "qwen3.5:9b", ManifestDigest: "sha256:" + strings.Repeat("a", 64),
		Family: nativev1alpha1.ModelFamilyOllamaGGUF, Format: nativev1alpha1.ArtifactFormatGGUFV1, SizeBytes: 1024,
	}
	agent := &DevAgent{config: DevAgentConfig{
		Dynamic: client, AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()), Platform: fakeAgentPlatform{},
		ResolveOllama: func() (devruntime.OllamaRuntime, []devruntime.OllamaModel, error) {
			return devruntime.OllamaRuntime{Version: "0.21.2"}, []devruntime.OllamaModel{model}, nil
		},
	}}
	if err := agent.updateHostStatus(context.Background(), host, false, ""); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.HostsGVR).Namespace(host.Namespace).Get(context.Background(), host.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(updated.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileOllamaGGUFV1) || len(updated.Status.AvailableModels) != 1 || updated.Status.AvailableModels[0].ManifestDigest != model.ManifestDigest {
		t.Fatalf("Ollama capabilities = %#v", updated.Status)
	}
}

func TestHostAdvertisesExactLocalLlamaCppModel(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	host := &nativev1alpha1.IdleloomHost{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomHost"},
		ObjectMeta: metav1.ObjectMeta{Name: "host", Namespace: "idleloom-host-studio", Generation: 1},
		Spec:       nativev1alpha1.IdleloomHostSpec{AgentID: "studio.native"},
	}
	client := dynamicfake.NewSimpleDynamicClient(scheme, host)
	model := devruntime.LlamaCppModel{
		Name: "llama-3.2-3b.gguf", ManifestDigest: "sha256:" + strings.Repeat("c", 64),
		Family: nativev1alpha1.ModelFamilyGGUF, Format: nativev1alpha1.ArtifactFormatGGUFV1, SizeBytes: 2048,
	}
	agent := &DevAgent{config: DevAgentConfig{
		Dynamic: client, AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()), Platform: fakeAgentPlatform{},
		ResolveLlamaCpp: func() (devruntime.LlamaCppRuntime, []devruntime.LlamaCppModel, error) {
			return devruntime.LlamaCppRuntime{Version: "9960-a935fbffe", Device: "MTL0"}, []devruntime.LlamaCppModel{model}, nil
		},
	}}
	if err := agent.updateHostStatus(context.Background(), host, false, ""); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.HostsGVR).Namespace(host.Namespace).Get(context.Background(), host.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(updated.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileLlamaCppMetalV1) || len(updated.Status.AvailableModels) != 1 || updated.Status.AvailableModels[0].Name != model.Name {
		t.Fatalf("llama.cpp capabilities = %#v", updated.Status)
	}
}

func TestHostCapsCombinedLocalModelAdvertisement(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	host := &nativev1alpha1.IdleloomHost{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomHost"},
		ObjectMeta: metav1.ObjectMeta{Name: "host", Namespace: "idleloom-host-studio", Generation: 1},
		Spec:       nativev1alpha1.IdleloomHostSpec{AgentID: "studio.native"},
	}
	client := dynamicfake.NewSimpleDynamicClient(scheme, host)
	ollamaModels := make([]devruntime.OllamaModel, 40)
	llamaModels := make([]devruntime.LlamaCppModel, 40)
	for index := range ollamaModels {
		ollamaModels[index] = devruntime.OllamaModel{
			Name: fmt.Sprintf("model-%02d:v1", index), ManifestDigest: "sha256:" + strings.Repeat("a", 64),
			Family: nativev1alpha1.ModelFamilyOllamaGGUF, Format: nativev1alpha1.ArtifactFormatGGUFV1, SizeBytes: 1024,
		}
		llamaModels[index] = devruntime.LlamaCppModel{
			Name: fmt.Sprintf("model-%02d.gguf", index), ManifestDigest: "sha256:" + strings.Repeat("b", 64),
			Family: nativev1alpha1.ModelFamilyGGUF, Format: nativev1alpha1.ArtifactFormatGGUFV1, SizeBytes: 1024,
		}
	}
	agent := &DevAgent{config: DevAgentConfig{
		Dynamic: client, AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()), Platform: fakeAgentPlatform{},
		ResolveOllama: func() (devruntime.OllamaRuntime, []devruntime.OllamaModel, error) {
			return devruntime.OllamaRuntime{Version: "0.21.2"}, ollamaModels, nil
		},
		ResolveLlamaCpp: func() (devruntime.LlamaCppRuntime, []devruntime.LlamaCppModel, error) {
			return devruntime.LlamaCppRuntime{Version: "9960-a935fbffe", Device: "MTL0"}, llamaModels, nil
		},
	}}
	if err := agent.updateHostStatus(context.Background(), host, false, ""); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.HostsGVR).Namespace(host.Namespace).Get(context.Background(), host.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Status.AvailableModels) != 64 {
		t.Fatalf("available model count = %d, want 64", len(updated.Status.AvailableModels))
	}
}

func TestEnsureProcessStartsOwnedOllamaRuntime(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	model := devruntime.OllamaModel{
		Name: "qwen3.5:9b", ManifestDigest: "sha256:" + strings.Repeat("b", 64),
		Family: nativev1alpha1.ModelFamilyOllamaGGUF, Format: nativev1alpha1.ArtifactFormatGGUFV1, SizeBytes: 1024,
	}
	startCalls := 0
	underlying := &fakeBatchRunner{alive: true, waitForCancellation: true, pid: 123}
	agent := &DevAgent{
		store: store, logs: kubeletbridge.NewLogBuffer(1 << 20),
		config: DevAgentConfig{
			AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()), StateDirectory: t.TempDir(), Platform: fakeAgentPlatform{},
			ResolveOllama: func() (devruntime.OllamaRuntime, []devruntime.OllamaModel, error) {
				return devruntime.OllamaRuntime{Executable: "/usr/local/bin/ollama", Version: "0.21.2"}, []devruntime.OllamaModel{model}, nil
			},
			StartOllama: func(_ context.Context, config devruntime.OllamaProcessConfig) (Process, error) {
				startCalls++
				if config.Model != model || config.ContextLength != 2048 || !strings.Contains(config.WorkDirectory, "ollama-assignment") {
					t.Fatalf("Ollama process config = %#v", config)
				}
				if err := config.OnSpawn(123); err != nil {
					return nil, err
				}
				return underlying, nil
			},
		},
	}
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("ollama-assignment"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef:  nativev1alpha1.WorkloadObjectReference{Namespace: "default", Name: "infer", UID: types.UID("workload-uid"), Generation: 1},
			Model: &nativev1alpha1.ResolvedModel{
				CatalogRef: nativev1alpha1.ObjectReference{Name: "qwen-ollama", UID: types.UID("model-uid")},
				Family:     model.Family, RuntimeProfile: nativev1alpha1.RuntimeProfileOllamaGGUFV1,
				Artifact: nativev1alpha1.ModelArtifact{
					OllamaModel: model.Name, ManifestDigest: model.ManifestDigest,
					Format: model.Format, SizeBytes: model.SizeBytes,
				},
				UnifiedMemoryRequest: resource.MustParse("8Gi"), MaxContextLength: 2048, MaxConcurrentRequests: 1,
				Batch: &nativev1alpha1.WorkloadBatchInference{Prompt: "hello", MaxTokens: 8, TimeoutSeconds: 30},
			},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 1, LeaseDurationSeconds: 30,
		},
	}
	if err := agent.ensureProcess(context.Background(), assignment); err != nil {
		t.Fatal(err)
	}
	if startCalls != 1 || agent.process == nil || !agent.process.Alive() {
		t.Fatalf("startCalls=%d process=%#v", startCalls, agent.process)
	}
	if current := store.Current(); current == nil || current.Executable != "/usr/local/bin/ollama" || current.RuntimeVersion != "ollama-0.21.2" {
		t.Fatalf("execution journal = %#v", current)
	}
	if got := agent.assignmentRuntimeVersion(assignment); got != "ollama-0.21.2" {
		t.Fatalf("assignment runtime version = %q", got)
	}
	if err := agent.stopProcess(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureProcessStartsOwnedLlamaCppRuntime(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	model := devruntime.LlamaCppModel{
		Name: "llama-3.2-3b.gguf", ManifestDigest: "sha256:" + strings.Repeat("d", 64),
		Family: nativev1alpha1.ModelFamilyGGUF, Format: nativev1alpha1.ArtifactFormatGGUFV1, SizeBytes: 2048,
	}
	startCalls := 0
	underlying := &fakeBatchRunner{alive: true, waitForCancellation: true, pid: 124}
	agent := &DevAgent{
		store: store, logs: kubeletbridge.NewLogBuffer(1 << 20),
		config: DevAgentConfig{
			AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()), StateDirectory: t.TempDir(), Platform: fakeAgentPlatform{},
			ResolveLlamaCpp: func() (devruntime.LlamaCppRuntime, []devruntime.LlamaCppModel, error) {
				return devruntime.LlamaCppRuntime{Executable: "/opt/homebrew/bin/llama-server", Version: "9960-a935fbffe", Device: "MTL0"}, []devruntime.LlamaCppModel{model}, nil
			},
			StartLlamaCpp: func(_ context.Context, config devruntime.LlamaCppProcessConfig) (Process, error) {
				startCalls++
				if config.Model != model || config.ContextLength != 2048 || !strings.Contains(config.WorkDirectory, "llama-assignment") || config.Runtime.Device != "MTL0" {
					t.Fatalf("llama.cpp process config = %#v", config)
				}
				if err := config.OnSpawn(124); err != nil {
					return nil, err
				}
				return underlying, nil
			},
		},
	}
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("llama-assignment"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef:  nativev1alpha1.WorkloadObjectReference{Namespace: "default", Name: "infer", UID: types.UID("workload-uid"), Generation: 1},
			Model: &nativev1alpha1.ResolvedModel{
				CatalogRef: nativev1alpha1.ObjectReference{Name: "local-gguf", UID: types.UID("model-uid")},
				Family:     model.Family, RuntimeProfile: nativev1alpha1.RuntimeProfileLlamaCppMetalV1,
				Artifact: nativev1alpha1.ModelArtifact{
					GGUFFile: model.Name, ManifestDigest: model.ManifestDigest,
					Format: model.Format, SizeBytes: model.SizeBytes,
				},
				UnifiedMemoryRequest: resource.MustParse("8Gi"), MaxContextLength: 2048, MaxConcurrentRequests: 1,
				Batch: &nativev1alpha1.WorkloadBatchInference{Prompt: "hello", MaxTokens: 8, TimeoutSeconds: 30},
			},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 1, LeaseDurationSeconds: 30,
		},
	}
	if err := agent.ensureProcess(context.Background(), assignment); err != nil {
		t.Fatal(err)
	}
	if startCalls != 1 || agent.process == nil || !agent.process.Alive() {
		t.Fatalf("startCalls=%d process=%#v", startCalls, agent.process)
	}
	if current := store.Current(); current == nil || current.Executable != "/opt/homebrew/bin/llama-server" || current.RuntimeVersion != "llama.cpp-9960-a935fbffe" {
		t.Fatalf("execution journal = %#v", current)
	}
	if got := agent.assignmentRuntimeVersion(assignment); got != "llama.cpp-9960-a935fbffe" {
		t.Fatalf("assignment runtime version = %q", got)
	}
	if err := agent.stopProcess(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(llamaCppWorkDirectory(agent.config.Layout, assignment.UID)); !os.IsNotExist(err) {
		t.Fatalf("llama.cpp work directory still exists: %v", err)
	}
}

func TestAssignmentStatusUsesDetectedOllamaRuntimeVersion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{
			Name: nativev1alpha1.AssignmentMailboxName, Namespace: "host", UID: types.UID("assignment"), Generation: 1,
		},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{UID: types.UID("workload"), Generation: 1},
			Model: &nativev1alpha1.ResolvedModel{
				RuntimeProfile: nativev1alpha1.RuntimeProfileOllamaGGUFV1,
				Batch:          &nativev1alpha1.WorkloadBatchInference{},
			},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 3,
		},
	}
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	record := execution.Record{
		SchemaVersion: execution.SchemaVersionV1, WorkloadUID: "workload", WorkloadGeneration: 1,
		AssignmentUID: "assignment", ExecutionID: assignment.Spec.ExecutionID, FencingEpoch: 3,
		Executable: "/usr/local/bin/ollama", RuntimeVersion: "ollama-0.21.2", Nonce: strings.Repeat("a", 64),
	}
	if err := store.Begin(record); err != nil {
		t.Fatal(err)
	}
	client := dynamicfake.NewSimpleDynamicClient(scheme, assignment)
	agent := &DevAgent{store: store, config: DevAgentConfig{Dynamic: client, AgentID: "studio.native"}}
	if err := agent.updateAssignmentStatus(context.Background(), assignment.DeepCopy(), nativev1alpha1.PhaseRunning, nil); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.AssignmentsGVR).Namespace("host").Get(context.Background(), nativev1alpha1.AssignmentMailboxName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.RuntimeVersion != "ollama-0.21.2" {
		t.Fatalf("status runtime version = %q", updated.Status.RuntimeVersion)
	}
}

func TestEnsureProcessPreparesAndStartsTrainingRun(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	source := "print('training')\n"
	digest := sha256.Sum256([]byte(source))
	prepareCalls := 0
	startCalls := 0
	agent := &DevAgent{
		store: store,
		logs:  kubeletbridge.NewLogBuffer(1 << 20),
		config: DevAgentConfig{
			AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()), Platform: fakeAgentPlatform{},
			PrepareTrainingRuntime: func(context.Context, func(string)) (devruntime.RuntimeReceipt, error) {
				prepareCalls++
				return devruntime.RuntimeReceipt{RuntimeVersion: devruntime.RuntimeVersion}, nil
			},
			StartTraining: func(_ context.Context, config devruntime.TrainingConfig) (Process, error) {
				startCalls++
				if config.Source != source || config.Experiment != "smoke" || config.Parameters["EPOCHS"] != "5" {
					t.Fatalf("training config = %#v", config)
				}
				if err := config.OnSpawn(123); err != nil {
					return nil, err
				}
				return &fakeProcess{alive: true}, nil
			},
		},
	}
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("training-assignment"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef:  nativev1alpha1.WorkloadObjectReference{Namespace: "default", Name: "train", UID: types.UID("workload-uid"), Generation: 1},
			Training: &nativev1alpha1.ResolvedTraining{
				RuntimeProfile: nativev1alpha1.RuntimeProfileMLXTrainV1,
				Source:         source, SourceDigest: "sha256:" + hex.EncodeToString(digest[:]),
				Network: nativev1alpha1.ShellNetworkNone, TimeoutSeconds: 30,
			},
			Run:         &nativev1alpha1.WorkloadRunSpec{Task: "train", Experiment: "smoke", Attempt: 1, Parameters: map[string]nativev1alpha1.WorkloadRunParameter{"EPOCHS": "5"}},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 1, LeaseDurationSeconds: 30,
		},
	}
	if err := agent.ensureProcess(context.Background(), assignment); err != nil {
		t.Fatal(err)
	}
	if prepareCalls != 1 || startCalls != 1 || agent.runStatus == nil || agent.runStatus.Experiment != "smoke" {
		t.Fatalf("prepare=%d start=%d run=%#v", prepareCalls, startCalls, agent.runStatus)
	}
	if err := agent.stopProcess(); err != nil {
		t.Fatal(err)
	}
}

func TestHostAdvertisesTrainingCapabilityWhenRuntimeIsPreparable(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	host := &nativev1alpha1.IdleloomHost{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomHost"},
		ObjectMeta: metav1.ObjectMeta{Name: "host", Namespace: "idleloom-host-studio", Generation: 1},
		Spec:       nativev1alpha1.IdleloomHostSpec{AgentID: "studio.native"},
	}
	client := dynamicfake.NewSimpleDynamicClient(scheme, host)
	agent := &DevAgent{config: DevAgentConfig{
		Dynamic: client, AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()), Platform: fakeAgentPlatform{},
		PrepareTrainingRuntime: func(context.Context, func(string)) (devruntime.RuntimeReceipt, error) {
			return devruntime.RuntimeReceipt{}, nil
		},
	}}
	if err := agent.updateHostStatus(context.Background(), host, false, ""); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.HostsGVR).Namespace(host.Namespace).Get(context.Background(), host.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(updated.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileMLXTrainV1) || !slices.Contains(updated.Status.Capabilities, nativev1alpha1.CapabilityNativeTrainingV1) {
		t.Fatalf("training capabilities = %#v", updated.Status)
	}
}

func TestAssignmentStatusPersistsRunSummary(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "host", UID: types.UID("assignment"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			Training:    &nativev1alpha1.ResolvedTraining{},
			Run:         &nativev1alpha1.WorkloadRunSpec{Task: "train", Experiment: "experiment", Attempt: 2},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 3,
		},
	}
	client := dynamicfake.NewSimpleDynamicClient(scheme, assignment)
	agent := &DevAgent{
		config: DevAgentConfig{Dynamic: client, AgentID: "studio.native", Now: func() time.Time { return now }},
		runStatus: &nativev1alpha1.WorkloadRunStatus{
			Metrics:   []nativev1alpha1.RunMetricSummary{{Name: "loss", Value: "0.25", Step: 20, ObservedAt: metav1.NewMicroTime(now)}},
			Artifacts: []nativev1alpha1.RunArtifactReference{{Name: "checkpoint", URI: "s3://bucket/checkpoint", Digest: "sha256:" + strings.Repeat("a", 64)}},
		},
	}
	if err := agent.updateAssignmentStatus(context.Background(), assignment.DeepCopy(), nativev1alpha1.PhaseRunning, nil); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.AssignmentsGVR).Namespace("host").Get(context.Background(), "active", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Run == nil || updated.Status.Run.Experiment != "experiment" || updated.Status.Run.Attempt != 2 || len(updated.Status.Run.Metrics) != 1 || updated.Status.Run.StartedAt == nil {
		t.Fatalf("run status = %#v", updated.Status.Run)
	}
}

func TestAssignmentStatusPersistsRunTimingForShell(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "host", UID: types.UID("assignment"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			Shell:       &nativev1alpha1.ResolvedShell{},
			Run:         &nativev1alpha1.WorkloadRunSpec{Task: "shell", Experiment: "smoke", Attempt: 1},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 3,
		},
	}
	client := dynamicfake.NewSimpleDynamicClient(scheme, assignment)
	agent := &DevAgent{
		config:    DevAgentConfig{Dynamic: client, AgentID: "studio.native", Now: func() time.Time { return now }},
		runStatus: &nativev1alpha1.WorkloadRunStatus{},
	}
	if err := agent.updateAssignmentStatus(context.Background(), assignment.DeepCopy(), nativev1alpha1.PhaseSucceeded, nil); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.AssignmentsGVR).Namespace("host").Get(context.Background(), "active", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Run == nil || updated.Status.Run.Task != "shell" || updated.Status.Run.Experiment != "smoke" || updated.Status.Run.StartedAt == nil || updated.Status.Run.FinishedAt == nil {
		t.Fatalf("run status = %#v", updated.Status.Run)
	}
}

func TestServerFailureDoesNotFinishRestartableRun(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "host", UID: types.UID("assignment"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			Model: &nativev1alpha1.ResolvedModel{Server: &nativev1alpha1.ResolvedServer{
				ServiceName: "qwen-chat", ModelAlias: "qwen3-5-0-8b",
			}},
			Run:         &nativev1alpha1.WorkloadRunSpec{Task: "serve", Experiment: "qwen-chat", Attempt: 1},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 3,
		},
	}
	client := dynamicfake.NewSimpleDynamicClient(scheme, assignment)
	agent := &DevAgent{
		config:    DevAgentConfig{Dynamic: client, AgentID: "studio.native", Now: func() time.Time { return now }},
		runStatus: &nativev1alpha1.WorkloadRunStatus{},
	}
	if err := agent.updateAssignmentStatus(context.Background(), assignment.DeepCopy(), nativev1alpha1.PhaseFailed, nil); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.AssignmentsGVR).Namespace("host").Get(context.Background(), "active", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var failed nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(object, &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Status.Run == nil || failed.Status.Run.StartedAt == nil || failed.Status.Run.FinishedAt != nil {
		t.Fatalf("restartable server failure finished the run: %#v", failed.Status.Run)
	}
	now = now.Add(time.Minute)
	if err := agent.updateAssignmentStatus(context.Background(), failed.DeepCopy(), nativev1alpha1.PhaseRunning, nil); err != nil {
		t.Fatal(err)
	}
	object, err = client.Resource(nativekube.AssignmentsGVR).Namespace("host").Get(context.Background(), "active", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var running nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(object, &running); err != nil {
		t.Fatal(err)
	}
	if running.Status.Run == nil || running.Status.Run.FinishedAt != nil || !running.Status.Run.StartedAt.Time.Equal(failed.Status.Run.StartedAt.Time) {
		t.Fatalf("recovered server run timing = %#v", running.Status.Run)
	}
}

func TestAssignmentStatusDoesNotReusePreviousRunTiming(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	previous := time.Unix(1_700_000_000, 0).UTC()
	now := time.Unix(1_800_000_000, 0).UTC()
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "host", UID: types.UID("new-assignment"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			Shell:       &nativev1alpha1.ResolvedShell{},
			Run:         &nativev1alpha1.WorkloadRunSpec{Task: "shell", Experiment: "sequence", Attempt: 2},
			ExecutionID: "223e4567-e89b-42d3-a456-426614174000", FencingEpoch: 4,
		},
	}
	client := dynamicfake.NewSimpleDynamicClient(scheme, assignment)
	previousStarted := metav1.NewMicroTime(previous)
	previousFinished := metav1.NewMicroTime(previous.Add(time.Second))
	agent := &DevAgent{
		config: DevAgentConfig{Dynamic: client, AgentID: "studio.native", Now: func() time.Time { return now }},
		runStatus: &nativev1alpha1.WorkloadRunStatus{
			ID: "123e4567-e89b-42d3-a456-426614174000", Task: "shell", Experiment: "sequence", Attempt: 1,
			StartedAt: &previousStarted, FinishedAt: &previousFinished,
		},
	}
	if err := agent.updateAssignmentStatus(context.Background(), assignment.DeepCopy(), nativev1alpha1.PhaseStarting, nil); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.AssignmentsGVR).Namespace("host").Get(context.Background(), "active", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Run == nil || updated.Status.Run.ID != assignment.Spec.ExecutionID || updated.Status.Run.StartedAt == nil || !updated.Status.Run.StartedAt.Time.Equal(now) || updated.Status.Run.FinishedAt != nil {
		t.Fatalf("new run inherited previous timing: %#v", updated.Status.Run)
	}
}

func TestBatchInferenceCompletesDurablyAndRetainsResultLog(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	descriptor, err := devruntime.LockedModel()
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeBatchRunner{
		alive: true, pid: 123,
		result: devruntime.GenerateResponse{Text: "batch-answer", ElapsedMillis: 7},
	}
	agent := &DevAgent{
		store: store,
		logs:  kubeletbridge.NewLogBuffer(1 << 20),
		config: DevAgentConfig{
			AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()), Platform: fakeAgentPlatform{},
			PrepareRuntime: func(context.Context, func(string)) (devruntime.Receipt, error) {
				return devruntime.Receipt{
					ArtifactIdentity: descriptor.ArtifactIdentity, ManifestDigest: descriptor.ManifestDigest,
					RuntimeVersion: devruntime.RuntimeVersion,
				}, nil
			},
			StartProcess: func(_ context.Context, config devruntime.ProcessConfig) (Process, error) {
				if err := config.OnSpawn(runner.pid); err != nil {
					return nil, err
				}
				return runner, nil
			},
		},
	}
	assignment := batchAssignment(t, descriptor)
	if err := agent.ensureProcess(context.Background(), assignment); err != nil {
		t.Fatal(err)
	}
	waitForBatch(t, agent.process)
	if err := agent.ensureProcess(context.Background(), assignment); !errors.Is(err, ErrProcessCompleted) {
		t.Fatalf("completion error = %v", err)
	}
	current := store.Current()
	if current == nil || !current.Completed || current.ExitError != "" {
		t.Fatalf("execution journal = %#v", current)
	}
	entries := agent.logs.Snapshot(string(assignment.UID), time.Time{}, -1)
	found := false
	for _, entry := range entries {
		if strings.Contains(entry.Message, `"text":"batch-answer"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("batch result log = %#v", entries)
	}
}

func batchAssignment(t *testing.T, descriptor devruntime.LockedModelDescriptor) *nativev1alpha1.IdleloomWorkloadAssignment {
	t.Helper()
	return &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("assignment-uid"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{
				Namespace: "default", Name: "batch", UID: types.UID("workload-uid"), Generation: 1,
			},
			Model: &nativev1alpha1.ResolvedModel{
				Family: nativev1alpha1.ModelFamilyQwen35, RuntimeProfile: nativev1alpha1.RuntimeProfileMLXLMV1,
				Artifact: nativev1alpha1.ModelArtifact{OCIReference: descriptor.ArtifactIdentity, ManifestDigest: descriptor.ManifestDigest},
				Batch:    &nativev1alpha1.WorkloadBatchInference{Prompt: "hello", MaxTokens: 8, TimeoutSeconds: 30},
			},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 1, LeaseDurationSeconds: 30,
		},
	}
}

func TestAgentLogWriterPreservesPartialWhitespaceAndBlankLines(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	agent := &DevAgent{logs: kubeletbridge.NewLogBuffer(1024), config: DevAgentConfig{Now: func() time.Time { return now }}}
	agent.resetLog("assignment", now, "started")
	writer := &agentLogWriter{agent: agent}
	_, _ = writer.Write([]byte("  partial"))
	_, _ = writer.Write([]byte(" line  \n\nlast"))
	writer.Flush()
	entries := agent.logs.Snapshot("assignment", time.Time{}, -1)
	if len(entries) != 4 || entries[1].Message != "  partial line  " || entries[2].Message != "" || entries[3].Message != "last" {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestStopProcessRemovesOnlyCurrentAssignmentWorkDirectory(t *testing.T) {
	layout := devruntime.NewLayout(t.TempDir())
	current := types.UID("current-assignment")
	other := types.UID("other-assignment")
	for _, uid := range []types.UID{current, other} {
		if err := os.MkdirAll(shellWorkDirectory(layout, uid), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	agent := &DevAgent{
		config: DevAgentConfig{Layout: layout},
		assignment: &nativev1alpha1.IdleloomWorkloadAssignment{
			ObjectMeta: metav1.ObjectMeta{UID: current},
			Spec:       nativev1alpha1.IdleloomWorkloadAssignmentSpec{Shell: &nativev1alpha1.ResolvedShell{}},
		},
	}
	if err := agent.stopProcess(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(shellWorkDirectory(layout, current)); !os.IsNotExist(err) {
		t.Fatalf("current assignment work directory remains: %v", err)
	}
	if _, err := os.Stat(shellWorkDirectory(layout, other)); err != nil {
		t.Fatalf("unrelated assignment work directory was removed: %v", err)
	}
}

func TestTrainingWorkDirectoryRetentionIsBounded(t *testing.T) {
	layout := devruntime.NewLayout(t.TempDir())
	root := filepath.Join(layout.Work, "assignments")
	for index := 0; index < 12; index++ {
		path := filepath.Join(root, fmt.Sprintf("run-%02d", index))
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		when := time.Unix(int64(index+1), 0)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneTrainingWorkDirectories(layout, 9); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 9 || entries[0].Name() != "run-03" {
		t.Fatalf("retained entries = %#v", entries)
	}
}

type fakeProcess struct {
	alive     bool
	stopErr   error
	stopCalls int
}

func (p *fakeProcess) Alive() bool { return p.alive }

func (p *fakeProcess) Stop() error {
	p.stopCalls++
	return p.stopErr
}

func (p *fakeProcess) Generate(context.Context, devruntime.GenerateRequest) (devruntime.GenerateResponse, error) {
	return devruntime.GenerateResponse{}, nil
}

func (p *fakeProcess) Stderr() string { return "" }

func (p *fakeProcess) WaitError() error { return nil }

type fakeAgentPlatform struct{}

func (fakeAgentPlatform) KrunkitRunning(context.Context) (bool, error) { return false, nil }
func (fakeAgentPlatform) AllocatableMemory(context.Context) (resource.Quantity, error) {
	return resource.MustParse("16Gi"), nil
}
func (fakeAgentPlatform) ProcessStartToken(int) (string, error) { return "start", nil }
func (fakeAgentPlatform) ProcessAlive(int) (bool, error)        { return false, nil }
func (fakeAgentPlatform) FindRunnerPIDs(context.Context, string, string) ([]int, error) {
	return nil, nil
}
func (fakeAgentPlatform) KillProcessGroupAndWait(context.Context, int) error { return nil }
