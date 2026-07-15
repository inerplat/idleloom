package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/devruntime"
	"github.com/inerplat/idleloom/internal/native/execution"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	"github.com/inerplat/idleloom/internal/native/kubeletbridge"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

type DevAgentConfig struct {
	Dynamic                dynamic.Interface
	Kubernetes             kubernetes.Interface
	Namespace              string
	AgentID                string
	Layout                 devruntime.Layout
	StateDirectory         string
	KubeconfigPath         string
	ListenAddress          string
	ServeListenAddress     string
	PollInterval           time.Duration
	Now                    func() time.Time
	Logf                   func(string, ...any)
	ConnectivityStatus     func() (nativev1alpha1.HostConnectivityStatus, error)
	Platform               Platform
	StartProcess           func(context.Context, devruntime.ProcessConfig) (Process, error)
	StartOllama            func(context.Context, devruntime.OllamaProcessConfig) (Process, error)
	StartLlamaCpp          func(context.Context, devruntime.LlamaCppProcessConfig) (Process, error)
	StartShell             func(context.Context, devruntime.ShellConfig) (Process, error)
	StartTraining          func(context.Context, devruntime.TrainingConfig) (Process, error)
	ResolveOllama          func() (devruntime.OllamaRuntime, []devruntime.OllamaModel, error)
	ResolveLlamaCpp        func() (devruntime.LlamaCppRuntime, []devruntime.LlamaCppModel, error)
	PrepareRuntime         func(context.Context, func(string)) (devruntime.Receipt, error)
	PrepareTrainingRuntime func(context.Context, func(string)) (devruntime.RuntimeReceipt, error)
	KubeletBridge          *KubeletBridgeConfig
}

type KubeletBridgeConfig struct {
	ListenAddress              string
	Identity                   kubeletbridge.Identity
	ClientCA                   []byte
	AllowedClientCommonNames   []string
	AllowedClientOrganizations []string
}

type Process interface {
	Alive() bool
	Stop() error
	Generate(context.Context, devruntime.GenerateRequest) (devruntime.GenerateResponse, error)
	Stderr() string
	WaitError() error
}

type DevAgent struct {
	config         DevAgentConfig
	store          *execution.Store
	mu             sync.RWMutex
	process        Process
	assignment     *nativev1alpha1.IdleloomWorkloadAssignment
	lastAPISuccess time.Time
	server         *http.Server
	serverErrors   chan error
	endpointToken  string
	logs           *kubeletbridge.LogBuffer
	bridgeErrors   chan error
	runStatus      *nativev1alpha1.WorkloadRunStatus
	runProtocolErr error
}

type agentLogWriter struct {
	agent   *DevAgent
	mu      sync.Mutex
	pending []byte
	onLine  func(time.Time, string)
}

func (writer *agentLogWriter) Write(data []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.pending = append(writer.pending, data...)
	for {
		newline := bytes.IndexByte(writer.pending, '\n')
		if newline < 0 {
			break
		}
		writer.writeLine(string(writer.pending[:newline]))
		writer.pending = writer.pending[newline+1:]
	}
	for len(writer.pending) > 64<<10 {
		writer.writeLine(string(writer.pending[:64<<10]))
		writer.pending = writer.pending[64<<10:]
	}
	return len(data), nil
}

func (writer *agentLogWriter) Flush() {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if len(writer.pending) == 0 {
		return
	}
	writer.writeLine(string(writer.pending))
	writer.pending = nil
}

func (writer *agentLogWriter) writeLine(message string) {
	now := writer.agent.now()
	writer.agent.appendLogMessage(now, message)
	if writer.onLine != nil {
		writer.onLine(now, message)
	}
}

var ErrProcessExited = errors.New("native process exited unexpectedly")
var ErrProcessCompleted = errors.New("native process completed")

func NewDevAgent(config DevAgentConfig) (*DevAgent, error) {
	if config.Dynamic == nil || config.Namespace == "" || config.AgentID == "" || config.StateDirectory == "" {
		return nil, fmt.Errorf("dynamic client, namespace, agent ID, and state directory are required")
	}
	if config.Layout.Root == "" {
		config.Layout = devruntime.NewLayout(devruntime.DefaultRoot())
	}
	if config.ListenAddress == "" {
		config.ListenAddress = "127.0.0.1:0"
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 2 * time.Second
	}
	if config.Platform == nil {
		config.Platform = DarwinPlatform{}
	}
	if config.StartProcess == nil {
		config.StartProcess = func(ctx context.Context, processConfig devruntime.ProcessConfig) (Process, error) {
			return devruntime.Start(ctx, processConfig)
		}
	}
	if config.StartOllama == nil {
		config.StartOllama = func(ctx context.Context, processConfig devruntime.OllamaProcessConfig) (Process, error) {
			return devruntime.StartOllama(ctx, processConfig)
		}
	}
	if config.StartLlamaCpp == nil {
		config.StartLlamaCpp = func(ctx context.Context, processConfig devruntime.LlamaCppProcessConfig) (Process, error) {
			return devruntime.StartLlamaCpp(ctx, processConfig)
		}
	}
	if config.StartShell == nil {
		config.StartShell = func(ctx context.Context, shellConfig devruntime.ShellConfig) (Process, error) {
			return devruntime.StartShell(ctx, shellConfig)
		}
	}
	if config.StartTraining == nil {
		config.StartTraining = func(ctx context.Context, trainingConfig devruntime.TrainingConfig) (Process, error) {
			return devruntime.StartTraining(ctx, trainingConfig)
		}
	}
	if config.ResolveOllama == nil {
		var ollamaMu sync.Mutex
		var ollamaRuntime devruntime.OllamaRuntime
		var lastOllamaAttempt time.Time
		var lastOllamaErr error
		config.ResolveOllama = func() (devruntime.OllamaRuntime, []devruntime.OllamaModel, error) {
			ollamaMu.Lock()
			defer ollamaMu.Unlock()
			if ollamaRuntime.Executable == "" {
				if lastOllamaErr != nil && time.Since(lastOllamaAttempt) < 30*time.Second {
					return devruntime.OllamaRuntime{}, nil, lastOllamaErr
				}
				lastOllamaAttempt = time.Now()
				resolved, err := devruntime.FindOllama("", "")
				if err != nil {
					lastOllamaErr = err
					return devruntime.OllamaRuntime{}, nil, err
				}
				ollamaRuntime = resolved
				lastOllamaErr = nil
			}
			models, err := devruntime.DiscoverOllamaModels(ollamaRuntime)
			return ollamaRuntime, models, err
		}
	}
	if config.ResolveLlamaCpp == nil {
		var llamaMu sync.Mutex
		var llamaRuntime devruntime.LlamaCppRuntime
		var lastLlamaAttempt time.Time
		var lastLlamaErr error
		discovery := &devruntime.LlamaCppDiscovery{}
		config.ResolveLlamaCpp = func() (devruntime.LlamaCppRuntime, []devruntime.LlamaCppModel, error) {
			llamaMu.Lock()
			defer llamaMu.Unlock()
			if llamaRuntime.Executable == "" {
				if lastLlamaErr != nil && time.Since(lastLlamaAttempt) < 30*time.Second {
					return devruntime.LlamaCppRuntime{}, nil, lastLlamaErr
				}
				lastLlamaAttempt = time.Now()
				resolved, err := devruntime.FindLlamaCpp(context.Background(), "", filepath.Join(config.Layout.Root, "models", "gguf"))
				if err != nil {
					lastLlamaErr = err
					return devruntime.LlamaCppRuntime{}, nil, err
				}
				llamaRuntime = resolved
				lastLlamaErr = nil
			}
			models, err := discovery.Discover(context.Background(), llamaRuntime)
			return llamaRuntime, models, err
		}
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" && (config.PrepareRuntime == nil || config.PrepareTrainingRuntime == nil) {
		python, pythonErr := devruntime.FindPython312("")
		if platformErr := devruntime.CheckMLXPlatform(); platformErr == nil && pythonErr == nil {
			if config.PrepareRuntime == nil {
				config.PrepareRuntime = func(ctx context.Context, progress func(string)) (devruntime.Receipt, error) {
					return (devruntime.Preparer{Root: config.Layout.Root, Python: python, Progress: progress}).Prepare(ctx)
				}
			}
			if config.PrepareTrainingRuntime == nil {
				config.PrepareTrainingRuntime = func(ctx context.Context, progress func(string)) (devruntime.RuntimeReceipt, error) {
					return (devruntime.Preparer{Root: config.Layout.Root, Python: python, Progress: progress}).PrepareRuntime(ctx)
				}
			}
		}
	}
	if err := os.MkdirAll(config.StateDirectory, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(config.StateDirectory, 0o700); err != nil {
		return nil, err
	}
	store, err := execution.Open(filepath.Join(config.StateDirectory, "execution.json"))
	if err != nil {
		return nil, err
	}
	token, err := secureToken()
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	logs, err := kubeletbridge.OpenLogBuffer(filepath.Join(config.StateDirectory, "container-logs.jsonl"), 1<<20)
	if err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return &DevAgent{config: config, store: store, endpointToken: token, logs: logs}, nil
}

func (a *DevAgent) Run(ctx context.Context) (runErr error) {
	defer func() {
		runErr = errors.Join(runErr, a.Close())
	}()
	if err := a.recoverOrphan(); err != nil {
		return err
	}
	if err := a.startHTTP(); err != nil {
		return err
	}
	if err := a.startKubeletBridge(ctx); err != nil {
		return err
	}
	if a.config.Logf != nil {
		a.config.Logf("native agent started")
	}
	ticker := time.NewTicker(a.config.PollInterval)
	defer ticker.Stop()
	watchdogCtx, cancelWatchdog := context.WithCancel(ctx)
	var watchdog sync.WaitGroup
	watchdog.Add(1)
	go func() {
		defer watchdog.Done()
		a.runWatchdog(watchdogCtx)
	}()
	defer func() {
		cancelWatchdog()
		watchdog.Wait()
	}()
	for {
		if err := a.reconcile(ctx); err != nil {
			if a.config.Logf != nil {
				a.config.Logf("reconcile: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-a.serverErrors:
			return fmt.Errorf("native inference endpoint stopped: %w", err)
		case err, open := <-a.bridgeErrors:
			if !open || err == nil {
				return nil
			}
			return fmt.Errorf("kubelet logs bridge stopped: %w", err)
		case <-ticker.C:
		}
	}
}

func (a *DevAgent) Close() error {
	var errs []error
	if err := a.stopProcess(); err != nil {
		errs = append(errs, err)
	}
	if a.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := a.server.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if err := os.Remove(filepath.Join(a.config.StateDirectory, "endpoint.json")); err != nil && !os.IsNotExist(err) {
		errs = append(errs, err)
	}
	if a.store != nil {
		errs = append(errs, a.store.Close())
	}
	return errors.Join(errs...)
}

func (a *DevAgent) reconcile(ctx context.Context) error {
	hostObject, err := a.config.Dynamic.Resource(nativekube.HostsGVR).Namespace(a.config.Namespace).Get(ctx, "host", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get host mailbox: %w", err)
	}
	var host nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(hostObject, &host); err != nil {
		return err
	}
	if host.Spec.AgentID != a.config.AgentID {
		return fmt.Errorf("host mailbox belongs to agent %q", host.Spec.AgentID)
	}
	krunkitRunning, err := a.config.Platform.KrunkitRunning(ctx)
	if err != nil {
		return err
	}
	if krunkitRunning {
		if err := a.stopProcess(); err != nil {
			return err
		}
	}
	assignmentObject, err := a.config.Dynamic.Resource(nativekube.AssignmentsGVR).Namespace(a.config.Namespace).Get(ctx, nativev1alpha1.AssignmentMailboxName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		a.markAPISuccess()
		if err := a.stopProcess(); err != nil {
			return err
		}
		if current := a.store.Current(); current != nil {
			if err := a.terminateRecorded(ctx, *current); err != nil {
				return err
			}
			if err := a.store.Clear(*current); err != nil {
				return err
			}
		}
		return a.updateHostStatus(ctx, &host, krunkitRunning, "")
	}
	if err != nil {
		return fmt.Errorf("get assignment mailbox: %w", err)
	}
	var assignment nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(assignmentObject, &assignment); err != nil {
		return err
	}
	if err := ValidateMailbox(&assignment, &host, a.config.AgentID, a.store.Current()); err != nil {
		return err
	}
	if err := a.fenceSupersededExecution(ctx, &assignment); err != nil {
		return err
	}
	a.markAPISuccess()
	if assignment.Spec.DesiredState == nativev1alpha1.AssignmentDesiredStopped {
		if err := a.stopAssignment(ctx, &assignment); err != nil {
			return err
		}
		return a.updateHostStatus(ctx, &host, krunkitRunning, "")
	}
	if krunkitRunning {
		if err := a.updateAssignmentStatus(ctx, &assignment, nativev1alpha1.PhaseBlocked, nil); err != nil {
			return err
		}
		return a.updateHostStatus(ctx, &host, true, "")
	}
	if phase, terminal := a.completedAssignmentPhase(&assignment); terminal {
		a.mu.Lock()
		a.assignment = assignment.DeepCopy()
		a.mu.Unlock()
		if err := a.updateAssignmentStatus(ctx, &assignment, phase, nil); err != nil {
			return err
		}
		return a.updateHostStatus(ctx, &host, false, "")
	}
	if !a.processRunningFor(&assignment) {
		if err := a.updateAssignmentStatus(ctx, &assignment, nativev1alpha1.PhaseStarting, nil); err != nil {
			return err
		}
	}
	if err := a.ensureProcess(ctx, &assignment); err != nil {
		if errors.Is(err, ErrProcessCompleted) {
			if statusErr := a.updateAssignmentStatus(ctx, &assignment, nativev1alpha1.PhaseSucceeded, nil); statusErr != nil {
				return statusErr
			}
			return a.updateHostStatus(ctx, &host, false, "")
		}
		statusErr := a.updateAssignmentStatus(ctx, &assignment, nativev1alpha1.PhaseFailed, nil)
		hostErr := a.updateHostStatus(ctx, &host, false, "")
		return errors.Join(err, statusErr, hostErr)
	}
	if err := a.updateAssignmentStatus(ctx, &assignment, nativev1alpha1.PhaseRunning, nil); err != nil {
		return err
	}
	return a.updateHostStatus(ctx, &host, false, assignment.UID)
}

func (a *DevAgent) completedAssignmentPhase(assignment *nativev1alpha1.IdleloomWorkloadAssignment) (string, bool) {
	if !finiteAssignment(assignment) || assignment.Status.ObservedGeneration != assignment.Generation || assignment.Status.AgentID != a.config.AgentID || assignment.Status.ExecutionID != assignment.Spec.ExecutionID || assignment.Status.FencingEpoch != assignment.Spec.FencingEpoch {
		return "", false
	}
	if assignment.Status.Phase != nativev1alpha1.PhaseSucceeded && assignment.Status.Phase != nativev1alpha1.PhaseFailed {
		return "", false
	}
	return assignment.Status.Phase, true
}

func (a *DevAgent) ensureProcess(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment) error {
	if a.processRunningFor(assignment) {
		return nil
	}
	a.mu.RLock()
	process := a.process
	a.mu.RUnlock()
	if process == nil && !finiteAssignment(assignment) {
		if err := a.clearRestartableExecution(ctx, assignment); err != nil {
			return err
		}
	}
	if process != nil {
		if finiteAssignment(assignment) && !process.Alive() {
			kind := "shell process"
			if assignment.Spec.Model != nil {
				kind = "batch inference"
			} else if assignment.Spec.Training != nil {
				kind = "training process"
			}
			waitErr := process.WaitError()
			if assignment.Spec.Training != nil {
				a.mu.RLock()
				protocolErr := a.runProtocolErr
				a.mu.RUnlock()
				waitErr = errors.Join(waitErr, protocolErr)
			}
			current := a.store.Current()
			if current == nil || !recordMatchesAssignment(*current, assignment) {
				return fmt.Errorf("finite workload completion does not match the durable execution journal")
			}
			if err := a.store.Complete(*current, waitErr); err != nil {
				return fmt.Errorf("persist %s completion: %w", kind, err)
			}
			if err := a.stopProcess(); err != nil {
				return err
			}
			if waitErr == nil {
				a.appendLog(a.now(), "%s completed successfully", kind)
				return ErrProcessCompleted
			}
			a.appendLog(a.now(), "%s exited with error: %v", kind, waitErr)
			return fmt.Errorf("%s exited: %w", kind, waitErr)
		}
		if err := a.stopProcess(); err != nil {
			return err
		}
		if !finiteAssignment(assignment) {
			if err := a.clearRestartableExecution(ctx, assignment); err != nil {
				return err
			}
		}
		return ErrProcessExited
	}
	if finiteAssignment(assignment) {
		if current := a.store.Current(); current != nil && current.Completed && recordMatchesAssignment(*current, assignment) {
			if current.ExitError == "" {
				return ErrProcessCompleted
			}
			return fmt.Errorf("finite native workload exited: %s", current.ExitError)
		}
	}
	a.mu.Lock()
	a.assignment = assignment.DeepCopy()
	a.runStatus = nil
	a.runProtocolErr = nil
	if assignment.Spec.Run != nil {
		a.runStatus = cloneRunStatus(assignment.Status.Run)
		if a.runStatus == nil {
			a.runStatus = &nativev1alpha1.WorkloadRunStatus{
				ID: assignment.Spec.ExecutionID, Task: assignment.Spec.Run.Task,
				Experiment: assignment.Spec.Run.Experiment, Attempt: assignment.Spec.Run.Attempt,
			}
		}
	}
	a.mu.Unlock()
	a.beginAssignmentLog(assignment)
	var receipt devruntime.Receipt
	var runtimeReceipt devruntime.RuntimeReceipt
	var ollamaRuntime devruntime.OllamaRuntime
	var ollamaModel devruntime.OllamaModel
	var llamaRuntime devruntime.LlamaCppRuntime
	var llamaModel devruntime.LlamaCppModel
	runtimeVersion := ""
	executable := filepath.Join(a.config.Layout.Venv, "bin", "python")
	var err error
	if assignment.Spec.Model != nil {
		switch assignment.Spec.Model.RuntimeProfile {
		case nativev1alpha1.RuntimeProfileMLXLMV1:
			receipt, err = devruntime.VerifyFast(a.config.Layout)
			if err != nil && a.config.PrepareRuntime != nil {
				a.appendLog(a.now(), "preparing locked MLX runtime and model")
				receipt, err = a.prepareRuntimeWithLease(ctx, assignment)
			}
			if err != nil {
				return fmt.Errorf("prepare locked MLX runtime: %w", err)
			}
			if assignment.Spec.Model.Artifact.OCIReference != receipt.ArtifactIdentity || assignment.Spec.Model.Artifact.ManifestDigest != receipt.ManifestDigest || assignment.Spec.Model.Family != nativev1alpha1.ModelFamilyQwen35 {
				return fmt.Errorf("assignment is not the exact locked MLX model")
			}
			runtimeVersion = receipt.RuntimeVersion
		case nativev1alpha1.RuntimeProfileOllamaGGUFV1:
			var models []devruntime.OllamaModel
			ollamaRuntime, models, err = a.config.ResolveOllama()
			if err != nil {
				return fmt.Errorf("resolve local Ollama runtime: %w", err)
			}
			for _, candidate := range models {
				if candidate.Name == assignment.Spec.Model.Artifact.OllamaModel && candidate.ManifestDigest == assignment.Spec.Model.Artifact.ManifestDigest {
					ollamaModel = candidate
					break
				}
			}
			if ollamaModel.Name == "" || ollamaModel.Family != assignment.Spec.Model.Family || ollamaModel.Format != assignment.Spec.Model.Artifact.Format || ollamaModel.SizeBytes != assignment.Spec.Model.Artifact.SizeBytes {
				return fmt.Errorf("the exact pinned Ollama GGUF model is not installed on this host")
			}
			runtimeVersion = "ollama-" + ollamaRuntime.Version
			executable = ollamaRuntime.Executable
		case nativev1alpha1.RuntimeProfileLlamaCppMetalV1:
			var models []devruntime.LlamaCppModel
			llamaRuntime, models, err = a.config.ResolveLlamaCpp()
			if err != nil {
				return fmt.Errorf("resolve local llama.cpp runtime: %w", err)
			}
			for _, candidate := range models {
				if candidate.Name == assignment.Spec.Model.Artifact.GGUFFile && candidate.ManifestDigest == assignment.Spec.Model.Artifact.ManifestDigest {
					llamaModel = candidate
					break
				}
			}
			if llamaModel.Name == "" || llamaModel.Family != assignment.Spec.Model.Family || llamaModel.Format != assignment.Spec.Model.Artifact.Format || llamaModel.SizeBytes != assignment.Spec.Model.Artifact.SizeBytes {
				return fmt.Errorf("the exact pinned llama.cpp GGUF model is not installed on this host")
			}
			runtimeVersion = "llama.cpp-" + llamaRuntime.Version
			executable = llamaRuntime.Executable
		default:
			return fmt.Errorf("assignment requests an unsupported model runtime %q", assignment.Spec.Model.RuntimeProfile)
		}
	} else if assignment.Spec.Training != nil {
		runtimeReceipt, err = devruntime.VerifyRuntimeFast(a.config.Layout)
		if err != nil && a.config.PrepareTrainingRuntime != nil {
			a.appendLog(a.now(), "preparing locked MLX training runtime")
			runtimeReceipt, err = a.prepareTrainingRuntimeWithLease(ctx, assignment)
		}
		if err != nil {
			return fmt.Errorf("prepare locked MLX training runtime: %w", err)
		}
		if assignment.Spec.Training.RuntimeProfile != nativev1alpha1.RuntimeProfileMLXTrainV1 {
			return fmt.Errorf("assignment requests an unsupported training runtime")
		}
		digest := sha256.Sum256([]byte(assignment.Spec.Training.Source))
		if assignment.Spec.Training.SourceDigest != "sha256:"+hex.EncodeToString(digest[:]) {
			return fmt.Errorf("training source digest does not match the resolved assignment")
		}
		runtimeVersion = runtimeReceipt.RuntimeVersion
	}
	nonce, err := secureToken()
	if err != nil {
		return err
	}
	planned := execution.Record{
		SchemaVersion:      execution.SchemaVersionV1,
		WorkloadUID:        string(assignment.Spec.WorkloadRef.UID),
		WorkloadGeneration: assignment.Spec.WorkloadRef.Generation,
		AssignmentUID:      string(assignment.UID),
		ExecutionID:        assignment.Spec.ExecutionID,
		FencingEpoch:       assignment.Spec.FencingEpoch,
		Executable:         executable,
		RuntimeVersion:     runtimeVersion,
		Nonce:              nonce,
	}
	if assignment.Spec.Shell != nil {
		planned.Executable = "/bin/zsh"
		planned.RuntimeVersion = nativev1alpha1.RuntimeProfileShellV1
	}
	if err := a.store.Begin(planned); err != nil {
		return err
	}
	if assignment.Spec.Model != nil {
		a.appendLog(a.now(), "verified pinned %s runtime and model artifact", assignment.Spec.Model.RuntimeProfile)
		if assignment.Spec.Model.Batch != nil {
			a.appendLog(a.now(), "starting Native batch inference")
		} else {
			a.appendLog(a.now(), "starting Native model server process")
		}
	} else if assignment.Spec.Training != nil {
		a.appendLog(a.now(), "verified locked MLX training runtime")
		a.appendLog(a.now(), "starting MLX training run: experiment=%s attempt=%d network=%s", assignment.Spec.Run.Experiment, assignment.Spec.Run.Attempt, assignment.Spec.Training.Network)
	} else {
		a.appendLog(a.now(), "starting shell process: isolation=%s network=%s", assignment.Spec.Shell.Isolation, assignment.Spec.Shell.Network)
	}
	process, err = a.startProcessWithLease(ctx, assignment, planned, nonce, ollamaRuntime, ollamaModel, llamaRuntime, llamaModel)
	if err != nil {
		a.appendLog(a.now(), "native process failed to start: %v", err)
		if current := a.store.Current(); current != nil {
			if terminateErr := a.terminateRecorded(ctx, *current); terminateErr == nil {
				_ = a.store.Clear(*current)
			}
		}
		if assignment.Spec.Shell != nil || assignment.Spec.Training != nil {
			_ = os.RemoveAll(shellWorkDirectory(a.config.Layout, assignment.UID))
		}
		if assignment.Spec.Model != nil && assignment.Spec.Model.RuntimeProfile == nativev1alpha1.RuntimeProfileOllamaGGUFV1 {
			_ = os.RemoveAll(ollamaWorkDirectory(a.config.Layout, assignment.UID))
		}
		if assignment.Spec.Model != nil && assignment.Spec.Model.RuntimeProfile == nativev1alpha1.RuntimeProfileLlamaCppMetalV1 {
			_ = os.RemoveAll(llamaCppWorkDirectory(a.config.Layout, assignment.UID))
		}
		return err
	}
	a.mu.Lock()
	a.process = process
	a.mu.Unlock()
	if assignment.Spec.Model != nil && assignment.Spec.Model.RuntimeProfile == nativev1alpha1.RuntimeProfileOllamaGGUFV1 {
		a.appendLog(a.now(), "verified Ollama Metal acceleration")
	}
	if assignment.Spec.Model != nil && assignment.Spec.Model.RuntimeProfile == nativev1alpha1.RuntimeProfileLlamaCppMetalV1 {
		a.appendLog(a.now(), "verified llama.cpp full Metal offload")
	}
	a.appendLog(a.now(), "native process started: pid=%d", processPID(process))
	return nil
}

func (a *DevAgent) processRunningFor(assignment *nativev1alpha1.IdleloomWorkloadAssignment) bool {
	if assignment == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.process != nil && a.process.Alive() && a.assignment != nil &&
		a.assignment.UID == assignment.UID && a.assignment.Spec.ExecutionID == assignment.Spec.ExecutionID
}

func (a *DevAgent) clearRestartableExecution(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment) error {
	if a.store == nil {
		return nil
	}
	current := a.store.Current()
	if current == nil {
		return nil
	}
	if !recordMatchesAssignment(*current, assignment) {
		return fmt.Errorf("refusing to clear a journal for a different server assignment")
	}
	if err := a.terminateRecorded(ctx, *current); err != nil {
		return fmt.Errorf("terminate previous server execution: %w", err)
	}
	if err := a.store.Clear(*current); err != nil {
		return fmt.Errorf("clear previous server execution: %w", err)
	}
	return nil
}

func (a *DevAgent) fenceSupersededExecution(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment) error {
	if a.store == nil {
		return nil
	}
	current := a.store.Current()
	if current == nil || recordMatchesAssignment(*current, assignment) {
		return nil
	}
	if assignment.Spec.FencingEpoch <= current.FencingEpoch {
		return fmt.Errorf("refusing assignment epoch %d while execution epoch %d is recorded", assignment.Spec.FencingEpoch, current.FencingEpoch)
	}
	stopErr := a.stopProcess()
	terminateErr := a.terminateRecorded(ctx, *current)
	if err := errors.Join(stopErr, terminateErr); err != nil {
		return fmt.Errorf("fence superseded native execution: %w", err)
	}
	if err := a.store.Clear(*current); err != nil {
		return fmt.Errorf("clear superseded native execution: %w", err)
	}
	a.mu.Lock()
	a.assignment = nil
	a.runStatus = nil
	a.runProtocolErr = nil
	a.mu.Unlock()
	return nil
}

func (a *DevAgent) startProcessWithLease(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment, planned execution.Record, nonce string, ollamaRuntime devruntime.OllamaRuntime, ollamaModel devruntime.OllamaModel, llamaRuntime devruntime.LlamaCppRuntime, llamaModel devruntime.LlamaCppModel) (Process, error) {
	type result struct {
		process Process
		err     error
	}
	startupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	completed := make(chan result, 1)
	go func() {
		onSpawn := func(pid int) error {
			startToken, err := a.config.Platform.ProcessStartToken(pid)
			if err != nil {
				return err
			}
			started := planned
			started.PID = pid
			started.ProcessStartToken = startToken
			return a.store.UpdateProcess(planned, started)
		}
		var process Process
		var err error
		if assignment.Spec.Shell != nil {
			workDirectory := shellWorkDirectory(a.config.Layout, assignment.UID)
			if err := os.RemoveAll(workDirectory); err != nil {
				completed <- result{err: fmt.Errorf("clear shell work directory: %w", err)}
				return
			}
			process, err = a.config.StartShell(startupCtx, devruntime.ShellConfig{
				Layout: a.config.Layout, WorkDirectory: workDirectory, Script: assignment.Spec.Shell.Script,
				Isolation: assignment.Spec.Shell.Isolation, Network: assignment.Spec.Shell.Network,
				Timeout:     time.Duration(assignment.Spec.Shell.TimeoutSeconds) * time.Second,
				DeniedPaths: []string{a.config.StateDirectory, a.config.KubeconfigPath},
				Output:      &agentLogWriter{agent: a}, OnSpawn: onSpawn,
			})
		} else if assignment.Spec.Training != nil {
			workDirectory := shellWorkDirectory(a.config.Layout, assignment.UID)
			if err := pruneTrainingWorkDirectories(a.config.Layout, 9); err != nil {
				completed <- result{err: fmt.Errorf("prune training work directories: %w", err)}
				return
			}
			if err := os.RemoveAll(workDirectory); err != nil {
				completed <- result{err: fmt.Errorf("clear training work directory: %w", err)}
				return
			}
			process, err = a.config.StartTraining(startupCtx, devruntime.TrainingConfig{
				Layout: a.config.Layout, WorkDirectory: workDirectory,
				Source: assignment.Spec.Training.Source, Network: assignment.Spec.Training.Network,
				Timeout:     time.Duration(assignment.Spec.Training.TimeoutSeconds) * time.Second,
				DeniedPaths: []string{a.config.StateDirectory, a.config.KubeconfigPath},
				Parameters:  trainingEnvironment(assignment.Spec.Run.Parameters),
				RunID:       assignment.Spec.ExecutionID, Experiment: assignment.Spec.Run.Experiment,
				Attempt: assignment.Spec.Run.Attempt,
				Output:  &agentLogWriter{agent: a, onLine: a.observeRunProtocol}, OnSpawn: onSpawn,
			})
		} else {
			switch assignment.Spec.Model.RuntimeProfile {
			case nativev1alpha1.RuntimeProfileOllamaGGUFV1:
				process, err = a.config.StartOllama(startupCtx, devruntime.OllamaProcessConfig{
					Runtime: ollamaRuntime, Model: ollamaModel,
					ContextLength: int(assignment.Spec.Model.MaxContextLength),
					WorkDirectory: ollamaWorkDirectory(a.config.Layout, assignment.UID),
					DeniedPaths:   []string{a.config.StateDirectory, a.config.KubeconfigPath},
					ReadyTimeout:  2 * time.Minute, OnSpawn: onSpawn,
				})
			case nativev1alpha1.RuntimeProfileLlamaCppMetalV1:
				process, err = a.config.StartLlamaCpp(startupCtx, devruntime.LlamaCppProcessConfig{
					Runtime: llamaRuntime, Model: llamaModel,
					ContextLength: int(assignment.Spec.Model.MaxContextLength),
					WorkDirectory: llamaCppWorkDirectory(a.config.Layout, assignment.UID),
					DeniedPaths:   []string{a.config.StateDirectory, a.config.KubeconfigPath},
					ReadyTimeout:  5 * time.Minute, OnSpawn: onSpawn,
				})
			default:
				process, err = a.config.StartProcess(startupCtx, devruntime.ProcessConfig{
					Layout: a.config.Layout, DeniedPaths: []string{a.config.StateDirectory, a.config.KubeconfigPath},
					ReadyTimeout: 5 * time.Minute, Nonce: nonce, OnSpawn: onSpawn,
				})
			}
			if err == nil {
				switch {
				case assignment.Spec.Model.Batch != nil:
					batch := assignment.Spec.Model.Batch
					process = startBatchProcess(process, devruntime.GenerateRequest{
						Prompt: batch.Prompt, MaxTokens: int(batch.MaxTokens),
					}, time.Duration(batch.TimeoutSeconds)*time.Second, &agentLogWriter{agent: a})
				case assignment.Spec.Model.Server != nil:
					key, keyErr := a.resolveServingKey(startupCtx, assignment)
					if keyErr != nil {
						_ = process.Stop()
						err = keyErr
						break
					}
					process, err = startServeProcess(process, a.config.ServeListenAddress, assignment.Spec.Model.Server.ModelAlias, key, a)
				}
			}
		}
		completed <- result{process: process, err: err}
	}()
	interval := time.Duration(assignment.Spec.LeaseDurationSeconds) * time.Second / 3
	if interval < time.Second {
		interval = time.Second
	}
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case result := <-completed:
			return result.process, result.err
		case <-ticker.C:
			if err := a.refreshStartupLease(startupCtx, assignment); err != nil {
				cancel()
				result := <-completed
				if result.process != nil {
					_ = result.process.Stop()
				}
				return nil, fmt.Errorf("refresh startup assignment lease: %w", err)
			}
		case <-ctx.Done():
			cancel()
			result := <-completed
			if result.process != nil {
				_ = result.process.Stop()
			}
			return nil, ctx.Err()
		}
	}
}

func (a *DevAgent) refreshStartupLease(ctx context.Context, expected *nativev1alpha1.IdleloomWorkloadAssignment) error {
	object, err := a.config.Dynamic.Resource(nativekube.AssignmentsGVR).Namespace(expected.Namespace).Get(ctx, expected.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var current nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(object, &current); err != nil {
		return err
	}
	if current.UID != expected.UID || current.Spec.ExecutionID != expected.Spec.ExecutionID || current.Spec.FencingEpoch != expected.Spec.FencingEpoch || current.Generation != expected.Generation || current.Spec.DesiredState != nativev1alpha1.AssignmentDesiredRunning {
		return fmt.Errorf("assignment changed while the native process was starting")
	}
	if err := a.updateAssignmentStatus(ctx, &current, nativev1alpha1.PhaseStarting, nil); err != nil {
		return err
	}
	a.markAPISuccess()
	return nil
}

func (a *DevAgent) prepareRuntimeWithLease(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment) (devruntime.Receipt, error) {
	type result struct {
		receipt devruntime.Receipt
		err     error
	}
	completed := make(chan result, 1)
	prepareCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		receipt, err := a.config.PrepareRuntime(prepareCtx, func(message string) {
			a.appendLog(a.now(), "prepare: %s", message)
		})
		completed <- result{receipt: receipt, err: err}
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case result := <-completed:
			return result.receipt, result.err
		case <-ticker.C:
			if err := a.refreshStartupLease(prepareCtx, assignment); err != nil {
				cancel()
				result := <-completed
				return result.receipt, errors.Join(fmt.Errorf("refresh preparation assignment lease: %w", err), result.err)
			}
		case <-ctx.Done():
			cancel()
			result := <-completed
			return result.receipt, errors.Join(ctx.Err(), result.err)
		}
	}
}

func (a *DevAgent) prepareTrainingRuntimeWithLease(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment) (devruntime.RuntimeReceipt, error) {
	type result struct {
		receipt devruntime.RuntimeReceipt
		err     error
	}
	completed := make(chan result, 1)
	prepareCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		receipt, err := a.config.PrepareTrainingRuntime(prepareCtx, func(message string) {
			a.appendLog(a.now(), "prepare: %s", message)
		})
		completed <- result{receipt: receipt, err: err}
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case result := <-completed:
			return result.receipt, result.err
		case <-ticker.C:
			if err := a.refreshStartupLease(prepareCtx, assignment); err != nil {
				cancel()
				result := <-completed
				return result.receipt, errors.Join(fmt.Errorf("refresh training runtime preparation lease: %w", err), result.err)
			}
		case <-ctx.Done():
			cancel()
			result := <-completed
			return result.receipt, errors.Join(ctx.Err(), result.err)
		}
	}
}

func finiteAssignment(assignment *nativev1alpha1.IdleloomWorkloadAssignment) bool {
	return assignment.Spec.Shell != nil || assignment.Spec.Training != nil || assignment.Spec.Model != nil && assignment.Spec.Model.Batch != nil
}

func trainingEnvironment(values map[string]nativev1alpha1.WorkloadRunParameter) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for name, value := range values {
		result[name] = string(value)
	}
	return result
}

func (a *DevAgent) stopAssignment(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment) error {
	if err := a.stopProcess(); err != nil {
		return err
	}
	if current := a.store.Current(); current != nil {
		if current.AssignmentUID != string(assignment.UID) || current.ExecutionID != assignment.Spec.ExecutionID || current.FencingEpoch != assignment.Spec.FencingEpoch {
			return fmt.Errorf("refusing to clear a journal for a different assignment")
		}
		if err := a.terminateRecorded(ctx, *current); err != nil {
			return err
		}
		if err := a.store.Clear(*current); err != nil {
			return err
		}
	}
	if assignment.Spec.Run != nil {
		if err := os.RemoveAll(shellWorkDirectory(a.config.Layout, assignment.UID)); err != nil {
			return fmt.Errorf("remove assignment work directory: %w", err)
		}
	}
	now := metav1.NewMicroTime(a.now())
	ack := &nativev1alpha1.StopAcknowledgement{
		AssignmentUID:      assignment.UID,
		ObservedGeneration: assignment.Generation,
		ExecutionID:        assignment.Spec.ExecutionID,
		FencingEpoch:       assignment.Spec.FencingEpoch,
		StoppedAt:          now,
	}
	return a.updateAssignmentStatus(ctx, assignment, nativev1alpha1.PhaseStopped, ack)
}

func (a *DevAgent) updateAssignmentStatus(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment, phase string, ack *nativev1alpha1.StopAcknowledgement) error {
	copy := assignment.DeepCopy()
	now := metav1.NewMicroTime(a.now())
	copy.Status.ObservedGeneration = assignment.Generation
	copy.Status.Phase = phase
	copy.Status.AgentID = a.config.AgentID
	copy.Status.ExecutionID = assignment.Spec.ExecutionID
	copy.Status.FencingEpoch = assignment.Spec.FencingEpoch
	copy.Status.RuntimeVersion = a.assignmentRuntimeVersion(assignment)
	copy.Status.ResolvedArtifactDigest = ""
	if assignment.Spec.Model != nil {
		copy.Status.ResolvedArtifactDigest = assignment.Spec.Model.Artifact.ManifestDigest
	} else if assignment.Spec.Shell != nil {
		copy.Status.RuntimeVersion = nativev1alpha1.RuntimeProfileShellV1
	}
	if assignment.Spec.Run != nil {
		a.mu.Lock()
		run := cloneRunStatus(a.runStatus)
		if run == nil || run.ID != "" && run.ID != assignment.Spec.ExecutionID {
			run = cloneRunStatus(assignment.Status.Run)
		}
		if run != nil && run.ID != "" && run.ID != assignment.Spec.ExecutionID {
			run = nil
		}
		if run == nil {
			run = &nativev1alpha1.WorkloadRunStatus{}
		}
		run.ID = assignment.Spec.ExecutionID
		run.Task = assignment.Spec.Run.Task
		run.Experiment = assignment.Spec.Run.Experiment
		run.Attempt = assignment.Spec.Run.Attempt
		if run.StartedAt == nil && phase != nativev1alpha1.PhaseAssigned {
			started := now
			run.StartedAt = &started
		}
		terminalRun := phase == nativev1alpha1.PhaseStopped || finiteAssignment(assignment) && (phase == nativev1alpha1.PhaseSucceeded || phase == nativev1alpha1.PhaseFailed)
		if terminalRun && run.FinishedAt == nil {
			finished := now
			run.FinishedAt = &finished
		} else if !terminalRun {
			run.FinishedAt = nil
		}
		a.runStatus = cloneRunStatus(run)
		copy.Status.Run = cloneRunStatus(run)
		a.mu.Unlock()
	}
	copy.Status.LastHeartbeatTime = &now
	copy.Status.StopAcknowledgement = ack
	object, err := nativekube.ToUnstructured(copy)
	if err != nil {
		return err
	}
	updated, err := a.config.Dynamic.Resource(nativekube.AssignmentsGVR).Namespace(assignment.Namespace).UpdateStatus(ctx, object, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nativekube.FromUnstructured(updated, assignment)
}

func (a *DevAgent) assignmentRuntimeVersion(assignment *nativev1alpha1.IdleloomWorkloadAssignment) string {
	if a.store != nil {
		if current := a.store.Current(); current != nil && recordMatchesAssignment(*current, assignment) {
			return current.RuntimeVersion
		}
	}
	switch {
	case assignment.Spec.Shell != nil:
		return nativev1alpha1.RuntimeProfileShellV1
	case assignment.Spec.Training != nil:
		return assignment.Spec.Training.RuntimeProfile
	case assignment.Spec.Model != nil:
		return assignment.Spec.Model.RuntimeProfile
	default:
		return ""
	}
}

func (a *DevAgent) updateHostStatus(ctx context.Context, host *nativev1alpha1.IdleloomHost, krunkit bool, activeUID types.UID) error {
	copy := host.DeepCopy()
	now := metav1.NewMicroTime(a.now())
	memory, err := a.config.Platform.AllocatableMemory(ctx)
	if err != nil {
		return err
	}
	copy.Status.ObservedGeneration = host.Generation
	copy.Status.ProtocolVersion = nativev1alpha1.AgentProtocolV1Alpha1
	copy.Status.RuntimeProfiles = nil
	copy.Status.ModelFamilies = nil
	copy.Status.AvailableModels = nil
	copy.Status.Capabilities = nil
	copy.Status.AllocatableUnifiedMemory = memory
	copy.Status.AvailableUnifiedMemory = memory
	copy.Status.KrunkitState = nativev1alpha1.KrunkitStateStopped
	if krunkit {
		copy.Status.KrunkitState = nativev1alpha1.KrunkitStateRunning
	}
	copy.Status.VulkanLeaseActive = false
	copy.Status.ActiveAssignmentUID = activeUID
	copy.Status.LastHeartbeatTime = &now
	connectivity, connectedCondition := evaluateConnectivity(a.now(), a.config.ConnectivityStatus)
	copy.Status.Connectivity = &connectivity
	readyStatus := metav1.ConditionFalse
	readyReason := "DevelopmentRuntimeUnavailable"
	readyMessage := "no Native runtime capability is available"
	if _, err := devruntime.VerifyFast(a.config.Layout); err == nil {
		copy.Status.RuntimeProfiles = append(copy.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileMLXLMV1)
		copy.Status.ModelFamilies = append(copy.Status.ModelFamilies, nativev1alpha1.ModelFamilyQwen35)
		readyStatus = metav1.ConditionTrue
		readyReason = "NativeRuntimeReady"
		readyMessage = "locked MLX inference runtime and model are available"
	} else if a.config.PrepareRuntime != nil {
		copy.Status.RuntimeProfiles = append(copy.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileMLXLMV1)
		copy.Status.ModelFamilies = append(copy.Status.ModelFamilies, nativev1alpha1.ModelFamilyQwen35)
		readyStatus = metav1.ConditionTrue
		readyReason = "NativeRuntimePreparable"
		readyMessage = "locked MLX inference runtime and model will be prepared on first use"
	}
	if a.config.ResolveOllama != nil {
		_, models, err := a.config.ResolveOllama()
		if err == nil && len(models) > 0 {
			copy.Status.RuntimeProfiles = appendUnique(copy.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileOllamaGGUFV1)
			for _, model := range models {
				if model.Family != nativev1alpha1.ModelFamilyOllamaGGUF || model.Format != nativev1alpha1.ArtifactFormatGGUFV1 {
					continue
				}
				copy.Status.ModelFamilies = appendUnique(copy.Status.ModelFamilies, model.Family)
				copy.Status.AvailableModels = appendAvailableModel(copy.Status.AvailableModels, nativev1alpha1.HostModelStatus{
					RuntimeProfile: nativev1alpha1.RuntimeProfileOllamaGGUFV1,
					Name:           model.Name, ManifestDigest: model.ManifestDigest,
					Family: model.Family, Format: model.Format, SizeBytes: model.SizeBytes,
				})
			}
			if len(copy.Status.AvailableModels) > 0 {
				readyStatus = metav1.ConditionTrue
				readyReason = "NativeRuntimeReady"
				readyMessage = "Native MLX and/or local Ollama GGUF runtime is available"
			}
		}
	}
	if a.config.ResolveLlamaCpp != nil {
		_, models, err := a.config.ResolveLlamaCpp()
		if err == nil && len(models) > 0 {
			copy.Status.RuntimeProfiles = appendUnique(copy.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileLlamaCppMetalV1)
			for _, model := range models {
				if model.Family != nativev1alpha1.ModelFamilyGGUF || model.Format != nativev1alpha1.ArtifactFormatGGUFV1 {
					continue
				}
				copy.Status.ModelFamilies = appendUnique(copy.Status.ModelFamilies, model.Family)
				copy.Status.AvailableModels = appendAvailableModel(copy.Status.AvailableModels, nativev1alpha1.HostModelStatus{
					RuntimeProfile: nativev1alpha1.RuntimeProfileLlamaCppMetalV1,
					Name:           model.Name, ManifestDigest: model.ManifestDigest,
					Family: model.Family, Format: model.Format, SizeBytes: model.SizeBytes,
				})
			}
			if len(copy.Status.AvailableModels) > 0 {
				readyStatus = metav1.ConditionTrue
				readyReason = "NativeRuntimeReady"
				readyMessage = "Native MLX, Ollama, and/or llama.cpp Metal runtime is available"
			}
		}
	}
	if _, err := devruntime.VerifyRuntimeFast(a.config.Layout); err == nil {
		copy.Status.RuntimeProfiles = appendUnique(copy.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileMLXTrainV1)
		copy.Status.Capabilities = appendUnique(copy.Status.Capabilities, nativev1alpha1.CapabilityNativeTrainingV1)
		readyStatus = metav1.ConditionTrue
		readyReason = "NativeRuntimeReady"
		readyMessage = "locked MLX training runtime is available"
	} else if a.config.PrepareTrainingRuntime != nil {
		copy.Status.RuntimeProfiles = appendUnique(copy.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileMLXTrainV1)
		copy.Status.Capabilities = appendUnique(copy.Status.Capabilities, nativev1alpha1.CapabilityNativeTrainingV1)
		readyStatus = metav1.ConditionTrue
		if readyReason == "DevelopmentRuntimeUnavailable" {
			readyReason = "NativeRuntimePreparable"
			readyMessage = "locked MLX training runtime will be prepared on first use"
		}
	}
	if len(copy.Status.ModelFamilies) > 0 {
		copy.Status.Capabilities = appendUnique(copy.Status.Capabilities, nativev1alpha1.CapabilityBatchInferenceV1)
		if connectedCondition.Status == metav1.ConditionTrue && a.config.ServeListenAddress != "" {
			copy.Status.Capabilities = appendUnique(copy.Status.Capabilities, nativev1alpha1.CapabilityNativeServiceV1)
		}
	}
	if host.Spec.ShellAccess == nativev1alpha1.ShellAccessSandboxed || host.Spec.ShellAccess == nativev1alpha1.ShellAccessHost {
		copy.Status.RuntimeProfiles = appendUnique(copy.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileShellV1)
		readyStatus = metav1.ConditionTrue
		readyReason = "NativeRuntimeReady"
		readyMessage = "Native shell execution is available"
	}
	if readyStatus == metav1.ConditionTrue && len(copy.Status.RuntimeProfiles) > 0 {
		readyMessage = "Native host advertises runtime profiles: " + strings.Join(copy.Status.RuntimeProfiles, ", ")
	}
	apiMeta.SetStatusCondition(&copy.Status.Conditions, metav1.Condition{
		Type:               nativev1alpha1.HostConditionReady,
		Status:             readyStatus,
		ObservedGeneration: host.Generation,
		LastTransitionTime: metav1.NewTime(a.now()),
		Reason:             readyReason,
		Message:            readyMessage,
	})
	apiMeta.SetStatusCondition(&copy.Status.Conditions, metav1.Condition{
		Type:               nativev1alpha1.HostConditionConnected,
		Status:             connectedCondition.Status,
		ObservedGeneration: host.Generation,
		LastTransitionTime: metav1.NewTime(a.now()),
		Reason:             connectedCondition.Reason,
		Message:            connectedCondition.Message,
	})
	apiMeta.SetStatusCondition(&copy.Status.Conditions, metav1.Condition{
		Type:               nativev1alpha1.HostConditionDevelopmentOnly,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: host.Generation,
		LastTransitionTime: metav1.NewTime(a.now()),
		Reason:             "NoHostResourceArbiter",
		Message:            "Vulkan lease state is not authoritative; this host is eligible only for the single-host development MVP",
	})
	object, err := nativekube.ToUnstructured(copy)
	if err != nil {
		return err
	}
	_, err = a.config.Dynamic.Resource(nativekube.HostsGVR).Namespace(host.Namespace).UpdateStatus(ctx, object, metav1.UpdateOptions{})
	return err
}

func evaluateConnectivity(now time.Time, status func() (nativev1alpha1.HostConnectivityStatus, error)) (nativev1alpha1.HostConnectivityStatus, metav1.Condition) {
	connectivity := nativev1alpha1.HostConnectivityStatus{Mode: nativev1alpha1.ConnectivityModeAPIOnly}
	condition := metav1.Condition{
		Type: nativev1alpha1.HostConditionConnected, Status: metav1.ConditionFalse,
		Reason: "APIOnly", Message: "host connectivity is limited to outbound Kubernetes API access",
	}
	if status == nil {
		return connectivity, condition
	}
	observed, err := status()
	connectivity = observed
	condition.Reason = "WireKubeUnavailable"
	condition.Message = "WireKube connected leaf is not ready"
	if err != nil {
		condition.Message = err.Error()
		return connectivity, condition
	}
	condition.Status = metav1.ConditionTrue
	condition.Reason = "WireKubeRelaySessionReady"
	condition.Message = "WireKube relay session and peer routing are active"
	if connectivity.LastHandshakeTime == nil || connectivity.LastHandshakeTime.IsZero() {
		condition.Message += "; waiting for the first WireGuard handshake"
		return connectivity, condition
	}
	handshakeAge := now.Sub(connectivity.LastHandshakeTime.Time)
	if handshakeAge < -nativev1alpha1.HeartbeatClockSkewAllowance {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "WireKubeClockSkew"
		condition.Message = "WireGuard handshake timestamp is in the future"
		return connectivity, condition
	}
	condition.Message += fmt.Sprintf("; last handshake was %s ago", handshakeAge.Round(time.Second))
	return connectivity, condition
}

func (a *DevAgent) selfFenceIfExpired() {
	a.mu.RLock()
	assignment := a.assignment
	lastAPISuccess := a.lastAPISuccess
	a.mu.RUnlock()
	if assignment == nil {
		return
	}
	deadline := time.Duration(assignment.Spec.LeaseDurationSeconds) * time.Second
	if lastAPISuccess.IsZero() || a.now().Sub(lastAPISuccess) > deadline {
		if err := a.stopProcess(); err != nil && a.config.Logf != nil {
			a.config.Logf("self-fence process: %v", err)
		}
		if current := a.store.Current(); current != nil {
			if err := a.terminateRecorded(context.Background(), *current); err != nil && a.config.Logf != nil {
				a.config.Logf("self-fence journal process: %v", err)
			}
		}
	}
}

func (a *DevAgent) stopProcess() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.process != nil {
		stderr := strings.TrimSpace(a.process.Stderr())
		if stderr != "" && (a.assignment == nil || a.assignment.Spec.Shell == nil) {
			a.appendLog(a.now(), "native process stderr: %s", stderr)
		}
		if err := a.process.Stop(); err != nil {
			a.appendLog(a.now(), "failed to stop native process: %v", err)
			return err
		}
		a.appendLog(a.now(), "native process stopped")
	}
	a.process = nil
	if a.assignment != nil && a.assignment.Spec.Shell != nil {
		if err := os.RemoveAll(shellWorkDirectory(a.config.Layout, a.assignment.UID)); err != nil {
			return fmt.Errorf("remove shell work directory: %w", err)
		}
	}
	if a.assignment != nil && a.assignment.Spec.Model != nil && a.assignment.Spec.Model.RuntimeProfile == nativev1alpha1.RuntimeProfileOllamaGGUFV1 {
		if err := os.RemoveAll(ollamaWorkDirectory(a.config.Layout, a.assignment.UID)); err != nil {
			return fmt.Errorf("remove Ollama work directory: %w", err)
		}
	}
	if a.assignment != nil && a.assignment.Spec.Model != nil && a.assignment.Spec.Model.RuntimeProfile == nativev1alpha1.RuntimeProfileLlamaCppMetalV1 {
		if err := os.RemoveAll(llamaCppWorkDirectory(a.config.Layout, a.assignment.UID)); err != nil {
			return fmt.Errorf("remove llama.cpp work directory: %w", err)
		}
	}
	return nil
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func appendAvailableModel(values []nativev1alpha1.HostModelStatus, value nativev1alpha1.HostModelStatus) []nativev1alpha1.HostModelStatus {
	if len(values) >= 64 {
		return values
	}
	return append(values, value)
}

func (a *DevAgent) startKubeletBridge(ctx context.Context) error {
	if a.config.KubeletBridge == nil {
		a.bridgeErrors = make(chan error)
		return nil
	}
	bridge, err := kubeletbridge.NewServer(kubeletbridge.ServerConfig{
		ListenAddress:              a.config.KubeletBridge.ListenAddress,
		Identity:                   a.config.KubeletBridge.Identity,
		ClientCA:                   a.config.KubeletBridge.ClientCA,
		AllowedClientCommonNames:   a.config.KubeletBridge.AllowedClientCommonNames,
		AllowedClientOrganizations: a.config.KubeletBridge.AllowedClientOrganizations,
		Logs:                       a.logs,
		ResolveTarget:              a.resolveLogTarget,
	})
	if err != nil {
		return err
	}
	a.bridgeErrors = make(chan error, 1)
	go func() {
		a.bridgeErrors <- bridge.Run(ctx)
		close(a.bridgeErrors)
	}()
	return nil
}

func (a *DevAgent) resolveLogTarget() (kubeletbridge.Target, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.assignment == nil || a.assignment.Spec.DesiredState != nativev1alpha1.AssignmentDesiredRunning {
		return kubeletbridge.Target{}, false
	}
	return kubeletbridge.Target{
		AssignmentUID: string(a.assignment.UID),
		Namespace:     a.assignment.Spec.WorkloadRef.Namespace,
		PodName:       "idleloom-" + compactAssignmentUID(a.assignment.UID),
		ContainerName: "native-metal",
	}, true
}

func compactAssignmentUID(uid types.UID) string {
	value := strings.ReplaceAll(strings.ToLower(string(uid)), "-", "")
	if len(value) > 20 {
		value = value[:20]
	}
	return value
}

func shellWorkDirectory(layout devruntime.Layout, uid types.UID) string {
	return filepath.Join(layout.Work, "assignments", string(uid))
}

func ollamaWorkDirectory(layout devruntime.Layout, uid types.UID) string {
	return filepath.Join(layout.Work, "ollama", string(uid))
}

func llamaCppWorkDirectory(layout devruntime.Layout, uid types.UID) string {
	return filepath.Join(layout.Work, "llama-cpp", string(uid))
}

func pruneTrainingWorkDirectories(layout devruntime.Layout, retain int) error {
	root := filepath.Join(layout.Work, "assignments")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	type candidate struct {
		name    string
		modTime time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate{name: entry.Name(), modTime: info.ModTime()})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].modTime.Equal(candidates[j].modTime) {
			return candidates[i].name < candidates[j].name
		}
		return candidates[i].modTime.Before(candidates[j].modTime)
	})
	if retain < 0 {
		retain = 0
	}
	for _, item := range candidates[:max(0, len(candidates)-retain)] {
		if err := os.RemoveAll(filepath.Join(root, item.name)); err != nil {
			return err
		}
	}
	return nil
}

func processPID(process Process) int {
	if typed, ok := process.(interface{ PID() int }); ok {
		return typed.PID()
	}
	return 0
}

func (a *DevAgent) appendLog(now time.Time, format string, values ...any) {
	if a.logs != nil {
		if err := a.logs.Append(now, format, values...); err != nil && a.config.Logf != nil {
			a.config.Logf("persist native log: %v", err)
		}
	}
}

func (a *DevAgent) appendLogMessage(now time.Time, message string) {
	if a.logs != nil {
		if err := a.logs.AppendMessage(now, message); err != nil && a.config.Logf != nil {
			a.config.Logf("persist native log: %v", err)
		}
	}
}

func (a *DevAgent) resetLog(assignment string, now time.Time, message string) {
	if a.logs != nil {
		if err := a.logs.Reset(assignment, now, message); err != nil && a.config.Logf != nil {
			a.config.Logf("reset native log: %v", err)
		}
	}
}

func (a *DevAgent) beginAssignmentLog(assignment *nativev1alpha1.IdleloomWorkloadAssignment) {
	uid := string(assignment.UID)
	if a.logs != nil && a.logs.MatchesAssignment(uid) {
		a.appendLog(a.now(), "assignment retry accepted: execution=%s", assignment.Spec.ExecutionID)
		return
	}
	a.resetLog(uid, a.now(), "assignment accepted: execution="+assignment.Spec.ExecutionID)
}

func (a *DevAgent) recoverOrphan() error {
	current := a.store.Current()
	if current == nil {
		return nil
	}
	if current.Completed {
		return nil
	}
	if err := a.terminateRecorded(context.Background(), *current); err != nil {
		return err
	}
	if err := a.store.Clear(*current); err != nil {
		return err
	}
	return errors.Join(
		os.RemoveAll(filepath.Join(a.config.Layout.Work, "assignments", current.AssignmentUID)),
		os.RemoveAll(filepath.Join(a.config.Layout.Work, "ollama", current.AssignmentUID)),
		os.RemoveAll(filepath.Join(a.config.Layout.Work, "llama-cpp", current.AssignmentUID)),
	)
}

func recordMatchesAssignment(record execution.Record, assignment *nativev1alpha1.IdleloomWorkloadAssignment) bool {
	return record.WorkloadUID == string(assignment.Spec.WorkloadRef.UID) &&
		record.WorkloadGeneration == assignment.Spec.WorkloadRef.Generation &&
		record.AssignmentUID == string(assignment.UID) &&
		record.ExecutionID == assignment.Spec.ExecutionID &&
		record.FencingEpoch == assignment.Spec.FencingEpoch
}

func (a *DevAgent) runWatchdog(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.selfFenceIfExpired()
		}
	}
}

func (a *DevAgent) markAPISuccess() {
	a.mu.Lock()
	a.lastAPISuccess = a.now()
	a.mu.Unlock()
}

func (a *DevAgent) terminateRecorded(ctx context.Context, record execution.Record) error {
	if record.Completed {
		return nil
	}
	if record.PID > 0 {
		observed, err := a.config.Platform.ProcessStartToken(record.PID)
		if err != nil {
			alive, aliveErr := a.config.Platform.ProcessAlive(record.PID)
			if aliveErr == nil && !alive {
				return nil
			}
			if aliveErr != nil {
				return errors.Join(err, aliveErr)
			}
			return err
		}
		if observed != record.ProcessStartToken {
			return fmt.Errorf("recorded process PID %d has been reused", record.PID)
		}
		return a.config.Platform.KillProcessGroupAndWait(ctx, record.PID)
	}
	pids, err := a.config.Platform.FindRunnerPIDs(ctx, a.config.Layout.Runner, record.Nonce)
	if err != nil {
		return err
	}
	for _, pid := range pids {
		if err := a.config.Platform.KillProcessGroupAndWait(ctx, pid); err != nil {
			return err
		}
	}
	return nil
}

func (a *DevAgent) now() time.Time {
	if a.config.Now != nil {
		return a.config.Now()
	}
	return time.Now()
}

func secureToken() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
