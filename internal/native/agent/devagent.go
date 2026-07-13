package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
)

type DevAgentConfig struct {
	Dynamic            dynamic.Interface
	Namespace          string
	AgentID            string
	Layout             devruntime.Layout
	StateDirectory     string
	KubeconfigPath     string
	ListenAddress      string
	PollInterval       time.Duration
	Now                func() time.Time
	Logf               func(string, ...any)
	ConnectivityStatus func() (nativev1alpha1.HostConnectivityStatus, error)
	Platform           Platform
	StartProcess       func(context.Context, devruntime.ProcessConfig) (Process, error)
	StartShell         func(context.Context, devruntime.ShellConfig) (Process, error)
	PrepareRuntime     func(context.Context, func(string)) (devruntime.Receipt, error)
	KubeletBridge      *KubeletBridgeConfig
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
}

type agentLogWriter struct {
	agent   *DevAgent
	mu      sync.Mutex
	pending []byte
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
		writer.agent.appendLogMessage(writer.agent.now(), string(writer.pending[:newline]))
		writer.pending = writer.pending[newline+1:]
	}
	for len(writer.pending) > 64<<10 {
		writer.agent.appendLogMessage(writer.agent.now(), string(writer.pending[:64<<10]))
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
	writer.agent.appendLogMessage(writer.agent.now(), string(writer.pending))
	writer.pending = nil
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
	if config.StartShell == nil {
		config.StartShell = func(ctx context.Context, shellConfig devruntime.ShellConfig) (Process, error) {
			return devruntime.StartShell(ctx, shellConfig)
		}
	}
	if config.PrepareRuntime == nil && runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		config.PrepareRuntime = func(ctx context.Context, progress func(string)) (devruntime.Receipt, error) {
			return (devruntime.Preparer{Root: config.Layout.Root, Progress: progress}).Prepare(ctx)
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
		store.Close()
		return nil, err
	}
	logs, err := kubeletbridge.OpenLogBuffer(filepath.Join(config.StateDirectory, "container-logs.jsonl"), 1<<20)
	if err != nil {
		store.Close()
		return nil, err
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
	if err := a.updateAssignmentStatus(ctx, &assignment, nativev1alpha1.PhaseStarting, nil); err != nil {
		return err
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
	a.mu.RLock()
	process := a.process
	running := process != nil && process.Alive() && a.assignment != nil && a.assignment.UID == assignment.UID && a.assignment.Spec.ExecutionID == assignment.Spec.ExecutionID
	a.mu.RUnlock()
	if running {
		return nil
	}
	if process != nil {
		if finiteAssignment(assignment) && !process.Alive() {
			kind := "shell process"
			if assignment.Spec.Model != nil {
				kind = "batch inference"
			}
			waitErr := process.WaitError()
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
	a.mu.Unlock()
	a.resetLog(string(assignment.UID), a.now(), "assignment accepted: execution="+assignment.Spec.ExecutionID)
	var receipt devruntime.Receipt
	var err error
	if assignment.Spec.Model != nil {
		receipt, err = devruntime.VerifyFast(a.config.Layout)
		if err != nil && a.config.PrepareRuntime != nil {
			a.appendLog(a.now(), "preparing locked MLX runtime and model")
			receipt, err = a.prepareRuntimeWithLease(ctx, assignment)
		}
		if err != nil {
			return fmt.Errorf("prepare locked MLX runtime: %w", err)
		}
		if assignment.Spec.Model.Artifact.OCIReference != receipt.ArtifactIdentity || assignment.Spec.Model.Artifact.ManifestDigest != receipt.ManifestDigest || assignment.Spec.Model.RuntimeProfile != nativev1alpha1.RuntimeProfileMLXLMV1 || assignment.Spec.Model.Family != nativev1alpha1.ModelFamilyQwen35 {
			return fmt.Errorf("assignment is not the exact locked development model")
		}
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
		Executable:         filepath.Join(a.config.Layout.Venv, "bin", "python"),
		RuntimeVersion:     receipt.RuntimeVersion,
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
		a.appendLog(a.now(), "verified locked MLX runtime and model artifact")
		if assignment.Spec.Model.Batch != nil {
			a.appendLog(a.now(), "starting sandboxed MLX batch inference")
		} else {
			a.appendLog(a.now(), "starting sandboxed MLX server process")
		}
	} else {
		a.appendLog(a.now(), "starting shell process: isolation=%s network=%s", assignment.Spec.Shell.Isolation, assignment.Spec.Shell.Network)
	}
	process, err = a.startProcessWithLease(ctx, assignment, planned, nonce)
	if err != nil {
		a.appendLog(a.now(), "native process failed to start: %v", err)
		if current := a.store.Current(); current != nil {
			if terminateErr := a.terminateRecorded(ctx, *current); terminateErr == nil {
				_ = a.store.Clear(*current)
			}
		}
		if assignment.Spec.Shell != nil {
			_ = os.RemoveAll(shellWorkDirectory(a.config.Layout, assignment.UID))
		}
		return err
	}
	a.mu.Lock()
	a.process = process
	a.mu.Unlock()
	a.appendLog(a.now(), "native process started: pid=%d", processPID(process))
	return nil
}

func (a *DevAgent) startProcessWithLease(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment, planned execution.Record, nonce string) (Process, error) {
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
		} else {
			process, err = a.config.StartProcess(startupCtx, devruntime.ProcessConfig{
				Layout: a.config.Layout, DeniedPaths: []string{a.config.StateDirectory, a.config.KubeconfigPath},
				ReadyTimeout: 5 * time.Minute, Nonce: nonce, OnSpawn: onSpawn,
			})
			if err == nil && assignment.Spec.Model.Batch != nil {
				batch := assignment.Spec.Model.Batch
				process = startBatchProcess(process, devruntime.GenerateRequest{
					Prompt: batch.Prompt, MaxTokens: int(batch.MaxTokens),
				}, time.Duration(batch.TimeoutSeconds)*time.Second, &agentLogWriter{agent: a})
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

func finiteAssignment(assignment *nativev1alpha1.IdleloomWorkloadAssignment) bool {
	return assignment.Spec.Shell != nil || assignment.Spec.Model != nil && assignment.Spec.Model.Batch != nil
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
	copy.Status.RuntimeVersion = devruntime.RuntimeVersion
	copy.Status.ResolvedArtifactDigest = ""
	if assignment.Spec.Model != nil {
		copy.Status.ResolvedArtifactDigest = assignment.Spec.Model.Artifact.ManifestDigest
	} else {
		copy.Status.RuntimeVersion = nativev1alpha1.RuntimeProfileShellV1
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
		readyReason = "DevelopmentRuntimeReady"
		readyMessage = "locked development MLX runtime is available"
	} else if a.config.PrepareRuntime != nil {
		copy.Status.RuntimeProfiles = append(copy.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileMLXLMV1)
		copy.Status.ModelFamilies = append(copy.Status.ModelFamilies, nativev1alpha1.ModelFamilyQwen35)
		readyStatus = metav1.ConditionTrue
		readyReason = "DevelopmentRuntimePreparable"
		readyMessage = "locked development MLX runtime will be prepared on first use"
	}
	if len(copy.Status.ModelFamilies) > 0 {
		copy.Status.Capabilities = append(copy.Status.Capabilities, nativev1alpha1.CapabilityBatchInferenceV1)
	}
	if host.Spec.ShellAccess == nativev1alpha1.ShellAccessSandboxed || host.Spec.ShellAccess == nativev1alpha1.ShellAccessHost {
		copy.Status.RuntimeProfiles = append(copy.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileShellV1)
		readyStatus = metav1.ConditionTrue
		readyReason = "NativeRuntimeReady"
		readyMessage = "Native MLX and/or shell runtime capability is available"
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
	if connectivity.LastHandshakeTime == nil || connectivity.LastHandshakeTime.IsZero() {
		condition.Reason = "WireKubeHandshakePending"
		condition.Message = "WireGuard handshake has not completed"
		return connectivity, condition
	}
	handshakeAge := now.Sub(connectivity.LastHandshakeTime.Time)
	if handshakeAge < -nativev1alpha1.HeartbeatClockSkewAllowance {
		condition.Reason = "WireKubeClockSkew"
		condition.Message = "WireGuard handshake timestamp is in the future"
		return connectivity, condition
	}
	if handshakeAge > 3*time.Minute {
		condition.Reason = "WireKubeHandshakeStale"
		condition.Message = "WireGuard handshake is stale"
		return connectivity, condition
	}
	condition.Status = metav1.ConditionTrue
	condition.Reason = "WireKubeRelaySessionReady"
	condition.Message = "WireKube relay session has a fresh handshake; reverse reachability is not yet verified"
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
	return nil
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

func (a *DevAgent) recoverOrphan() error {
	current := a.store.Current()
	if current == nil {
		return nil
	}
	if current.Completed {
		return os.RemoveAll(filepath.Join(a.config.Layout.Work, "assignments", current.AssignmentUID))
	}
	if err := a.terminateRecorded(context.Background(), *current); err != nil {
		return err
	}
	if err := a.store.Clear(*current); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(a.config.Layout.Work, "assignments", current.AssignmentUID))
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
