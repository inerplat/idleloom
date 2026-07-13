package agent

import (
	"context"
	"errors"
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

func TestEvaluateConnectivityRequiresFreshWireGuardHandshake(t *testing.T) {
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
	if condition.Status != metav1.ConditionFalse || condition.Reason != "WireKubeHandshakeStale" {
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

func TestEnsureProcessDoesNotReplayDurablyCompletedShell(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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

func TestEnsureProcessPreparesLockedModelOnFirstUse(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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

func TestBatchInferenceCompletesDurablyAndRetainsResultLog(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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
