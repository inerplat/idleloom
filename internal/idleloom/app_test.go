package idleloom

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func TestValidateInitOptions(t *testing.T) {
	valid := InitOptions{
		NodeName: "mac-mini-idle",
		CPUs:     4,
		MemoryMB: 8192,
		DiskMB:   40960,
		Taint:    "example.com/dedicated=gpu:NoSchedule",
		Network:  NetworkWireKube,
		Timeout:  time.Minute,
		TokenTTL: time.Minute,
	}
	if err := validateInitOptions(valid); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	tooSmall := valid
	tooSmall.MemoryMB = 2048
	if err := validateInitOptions(tooSmall); err == nil {
		t.Fatal("expected 2 GiB VM to be rejected")
	}
	badTaint := valid
	badTaint.Taint = "dedicated"
	if err := validateInitOptions(badTaint); err == nil {
		t.Fatal("expected malformed taint to be rejected")
	}
	unsafeTaint := valid
	unsafeTaint.Taint = "example.com/dedicated=gpu:NoSchedule;rm -rf /"
	if err := validateInitOptions(unsafeTaint); err == nil {
		t.Fatal("expected shell metacharacters in taint to be rejected")
	}
}

func TestDeleteValidatesRuntimeBeforeLoadingCluster(t *testing.T) {
	sentinel := errors.New("runtime ownership rejected")
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := State{
		NodeName: "worker-a", KubeconfigPath: filepath.Join(t.TempDir(), "missing-kubeconfig"),
		Phase: PhaseReady, Runtime: RuntimeState{NodeName: "worker-a", RuntimeDir: "/tmp/idleloom-worker-a"},
	}
	if err := SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	app := &App{Out: io.Discard, Err: io.Discard, Now: time.Now, Runtime: rejectingRuntime{err: sentinel}}
	err := app.Delete(context.Background(), statePath, false, false)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Delete error = %v, want runtime validation error", err)
	}
}

func TestLocalDeleteDoesNotAdvancePhaseWhenValidationFails(t *testing.T) {
	sentinel := errors.New("runtime ownership rejected")
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := State{NodeName: "worker-a", Phase: PhaseReady, Runtime: RuntimeState{NodeName: "worker-a", RuntimeDir: "/tmp/idleloom-worker-a"}}
	if err := SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	app := &App{Out: io.Discard, Err: io.Discard, Now: time.Now, Runtime: rejectingRuntime{err: sentinel}}
	if err := app.Delete(context.Background(), statePath, false, true); !errors.Is(err, sentinel) {
		t.Fatalf("Delete error = %v, want validation error", err)
	}
	got, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != PhaseReady {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseReady)
	}
}

func TestLocalDeleteFailureLeavesPendingPhase(t *testing.T) {
	sentinel := errors.New("runtime delete failed")
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := State{NodeName: "worker-a", Phase: PhaseReady, Runtime: RuntimeState{NodeName: "worker-a"}}
	if err := SaveState(statePath, state); err != nil {
		t.Fatal(err)
	}
	app := &App{Out: io.Discard, Err: io.Discard, Now: time.Now, Runtime: deletingRuntime{err: sentinel}}
	if err := app.Delete(context.Background(), statePath, false, true); !errors.Is(err, sentinel) {
		t.Fatalf("Delete error = %v, want delete error", err)
	}
	got, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != PhaseLocalDeleting {
		t.Fatalf("phase = %q, want %q", got.Phase, PhaseLocalDeleting)
	}
}

func TestPollToleratesTransientCredentialRotation(t *testing.T) {
	attempts := 0
	err := pollWithInterval(context.Background(), time.Second, time.Millisecond, func() (bool, error) {
		attempts++
		if attempts == 1 {
			return false, apierrors.NewUnauthorized("expired exec credential")
		}
		return true, nil
	}, "credential refresh")
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("poll attempts = %d, want 2", attempts)
	}
}

func TestPollRejectsPersistentUnauthorizedCredentials(t *testing.T) {
	attempts := 0
	err := pollWithInterval(context.Background(), time.Second, time.Millisecond, func() (bool, error) {
		attempts++
		return false, apierrors.NewUnauthorized("invalid credential")
	}, "credential refresh")
	if err == nil || attempts != 16 {
		t.Fatalf("poll attempts = %d error = %v", attempts, err)
	}
}

func TestResumeEnrollmentRejectsRuntimeThatWasOnlyPlanned(t *testing.T) {
	runtime := &resumeRuntime{}
	app := &App{Out: io.Discard, Err: io.Discard, Now: time.Now, Runtime: runtime}
	state := &State{
		NodeName: "worker-a", Phase: PhaseEnrolling, TaintConfigured: true,
		Runtime: RuntimeState{NodeName: "worker-a", Planned: true},
	}
	cluster := &Cluster{Client: kubernetesfake.NewClientset()}
	err := app.resumeEnrollment(context.Background(), filepath.Join(t.TempDir(), "state.json"), state, cluster, time.Second)
	if err == nil || !strings.Contains(err.Error(), "before the VM was created") {
		t.Fatalf("planned resume error = %v", err)
	}
	if runtime.startCalls != 0 {
		t.Fatalf("runtime start calls = %d", runtime.startCalls)
	}
}

func TestResumeEnrollmentReinstallsFreshBootstrapBundle(t *testing.T) {
	removeErr := errors.New("stop after bootstrap cleanup")
	runtime := &resumeRuntime{removeBootstrapErr: removeErr}
	kubelet := filepath.Join(t.TempDir(), "kubelet")
	if err := os.WriteFile(kubelet, []byte("kubelet-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-a"},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue,
		}}},
	}
	client := kubernetesfake.NewClientset(
		node,
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "system:node-bootstrapper"}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "system:certificates.k8s.io:certificatesigningrequests:nodeclient"}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "system:certificates.k8s.io:certificatesigningrequests:selfnodeclient"}},
	)
	cluster := &Cluster{
		Client: client, Server: "https://api.example.test", TLSServerName: "api.example.test",
		CAData: []byte("test-ca"), KubeletVersion: "v1.35.3", ClusterDNS: "10.96.0.10", ClusterDomain: "cluster.local",
	}
	state := &State{
		NodeName: "worker-a", Phase: PhaseEnrolling, Taint: "idleloom-dedicated=compute:NoSchedule",
		TaintConfigured: true, TokenTTLSeconds: 60, CreatedAt: time.Now().Add(-time.Minute),
		Runtime: RuntimeState{NodeName: "worker-a", GuestIP: "192.0.2.20", SSHPort: 22022},
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	var servingNotBefore time.Time
	app := &App{
		Out: io.Discard, Err: io.Discard, Now: time.Now, Runtime: runtime,
		DownloadKubelet: func(context.Context, string) (string, error) { return kubelet, nil },
		ApproveKubeletServingCSR: func(_ context.Context, _ *Cluster, _, _ string, notBefore time.Time, _ bool, _ time.Duration) error {
			servingNotBefore = notBefore
			return nil
		},
	}
	err := app.resumeEnrollment(context.Background(), statePath, state, cluster, time.Second)
	if !errors.Is(err, removeErr) {
		t.Fatalf("resume error = %v, want bootstrap cleanup sentinel", err)
	}
	if runtime.startCalls != 1 || runtime.waitReadyCalls != 1 || runtime.installCalls != 1 || len(runtime.bundle) == 0 {
		t.Fatalf("resume runtime calls: start=%d wait=%d install=%d bundle=%d", runtime.startCalls, runtime.waitReadyCalls, runtime.installCalls, len(runtime.bundle))
	}
	if !servingNotBefore.Equal(state.CreatedAt) {
		t.Fatalf("serving CSR cutoff = %s, want existing enrollment time %s", servingNotBefore, state.CreatedAt)
	}
	bundleCopy := filepath.Join(t.TempDir(), "bundle.tar")
	if err := os.WriteFile(bundleCopy, runtime.bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	entries := readBundle(t, bundleCopy)
	if !strings.Contains(string(entries["install.sh"]), "idleloom-dedicated=compute:NoSchedule") {
		t.Fatalf("resumed bundle lost taint: %s", entries["install.sh"])
	}
	secrets, err := client.CoreV1().Secrets("kube-system").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets.Items {
		if secret.Type == corev1.SecretTypeBootstrapToken {
			t.Fatalf("bootstrap token remained after interrupted resume: %s", secret.Name)
		}
	}
}

func TestRegisterWorkerWithoutWaitingCleansBootstrapAndCordonsNode(t *testing.T) {
	secretUID := types.UID("bootstrap-token-uid")
	client := kubernetesfake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-test", Namespace: "kube-system", UID: secretUID}},
	)
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := &State{
		NodeName: "worker-a", Phase: PhaseEnrolling,
		Runtime: RuntimeState{NodeName: "worker-a"},
	}
	runtime := &resumeRuntime{}
	var output bytes.Buffer
	maintainerCalls := 0
	app := &App{
		Out: &output, Err: io.Discard, Now: time.Now, Runtime: runtime,
		StartMaintainer: func(context.Context, string, io.Writer) error {
			maintainerCalls++
			return nil
		},
	}
	token := &BootstrapToken{SecretName: "bootstrap-token-test", UID: secretUID, client: client}
	cluster := &Cluster{Client: client}

	if err := app.registerWorkerWithoutWaiting(context.Background(), statePath, state, cluster, token); err != nil {
		t.Fatal(err)
	}
	stored, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != PhaseRegistered {
		t.Fatalf("phase = %q, want %q", stored.Phase, PhaseRegistered)
	}
	node, err := client.CoreV1().Nodes().Get(context.Background(), "worker-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !node.Spec.Unschedulable {
		t.Fatal("registered node was not cordoned")
	}
	if _, err := client.CoreV1().Secrets("kube-system").Get(context.Background(), token.SecretName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("bootstrap token still exists: %v", err)
	}
	if token.client != nil {
		t.Fatal("deleted bootstrap token retained its client")
	}
	if runtime.removeBootstrapCalls != 1 || runtime.waitReadyCalls != 0 {
		t.Fatalf("runtime cleanup calls = %d, wait calls = %d", runtime.removeBootstrapCalls, runtime.waitReadyCalls)
	}
	if maintainerCalls != 1 {
		t.Fatalf("maintainer calls = %d, want 1", maintainerCalls)
	}
	if !strings.Contains(output.String(), "readiness is pending") || !strings.Contains(output.String(), "remains cordoned") {
		t.Fatalf("unexpected output: %s", output.String())
	}
}

func TestCompleteRegisteredEnrollmentWaitsAndUncordons(t *testing.T) {
	now := time.Now().UTC()
	client := kubernetesfake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-a"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: metav1.NewTime(now),
		}}},
	})
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := &State{
		NodeName: "worker-a", Phase: PhaseRegistered, CreatedAt: now.Add(-time.Minute),
		Runtime: RuntimeState{NodeName: "worker-a", GuestIP: "192.0.2.20"},
	}
	if err := SaveState(statePath, *state); err != nil {
		t.Fatal(err)
	}
	runtime := &resumeRuntime{status: WorkerStatus{VM: "running", Network: "running"}}
	maintainerCalls := 0
	servingChecks := 0
	app := &App{
		Out: io.Discard, Err: io.Discard, Now: time.Now, Runtime: runtime,
		ApproveKubeletServingCSR: func(context.Context, *Cluster, string, string, time.Time, bool, time.Duration) error {
			servingChecks++
			return nil
		},
		StartMaintainer: func(context.Context, string, io.Writer) error {
			maintainerCalls++
			return nil
		},
	}
	cluster := &Cluster{Client: client}

	if err := app.completeRegisteredEnrollment(context.Background(), statePath, state, cluster, time.Second); err != nil {
		t.Fatal(err)
	}
	stored, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != PhaseReady {
		t.Fatalf("phase = %q, want %q", stored.Phase, PhaseReady)
	}
	node, err := client.CoreV1().Nodes().Get(context.Background(), "worker-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if node.Spec.Unschedulable {
		t.Fatal("ready node remained cordoned")
	}
	if runtime.startCalls != 1 || runtime.waitReadyCalls != 1 {
		t.Fatalf("runtime start=%d wait=%d, want 1/1", runtime.startCalls, runtime.waitReadyCalls)
	}
	if servingChecks != 1 || maintainerCalls != 1 {
		t.Fatalf("serving checks=%d maintainer calls=%d", servingChecks, maintainerCalls)
	}
}

func TestCompleteRegisteredEnrollmentFailureStaysRegisteredAndCordoned(t *testing.T) {
	client := kubernetesfake.NewClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-a"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionFalse,
		}}},
	})
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := &State{
		NodeName: "worker-a", Phase: PhaseRegistered, CreatedAt: time.Now().Add(-time.Minute),
		Runtime: RuntimeState{NodeName: "worker-a", GuestIP: "192.0.2.20"},
	}
	if err := SaveState(statePath, *state); err != nil {
		t.Fatal(err)
	}
	app := &App{
		Out: io.Discard, Err: io.Discard, Now: time.Now,
		Runtime: &resumeRuntime{status: WorkerStatus{VM: "running", Network: "running"}},
		ApproveKubeletServingCSR: func(context.Context, *Cluster, string, string, time.Time, bool, time.Duration) error {
			return nil
		},
		StartMaintainer: func(context.Context, string, io.Writer) error { return nil },
	}
	cluster := &Cluster{Client: client}

	err := app.completeRegisteredEnrollment(context.Background(), statePath, state, cluster, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "node readiness") {
		t.Fatalf("completion error = %v", err)
	}
	stored, loadErr := LoadState(statePath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if stored.Phase != PhaseRegistered {
		t.Fatalf("phase = %q, want %q", stored.Phase, PhaseRegistered)
	}
	node, getErr := client.CoreV1().Nodes().Get(context.Background(), "worker-a", metav1.GetOptions{})
	if getErr != nil {
		t.Fatal(getErr)
	}
	if !node.Spec.Unschedulable {
		t.Fatal("failed registered completion uncordoned the node")
	}
}

func writeReadyWorkerState(t *testing.T, nodeName string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	if err := SaveState(path, State{
		NodeName: nodeName,
		Phase:    PhaseReady,
		Runtime:  RuntimeState{NodeName: nodeName},
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

// loadImageRuntime records LoadImage calls and reports a configurable VM
// status, standing in for a live worker without a real krunkit VM.
type loadImageRuntime struct {
	rejectingRuntime
	status         WorkerStatus
	loadImageCalls int
	loadImagePath  string
	loadImageState RuntimeState
	loadImageErr   error
}

func (r *loadImageRuntime) Status(context.Context, *RuntimeState) (WorkerStatus, error) {
	return r.status, nil
}

func (r *loadImageRuntime) LoadImage(_ context.Context, state RuntimeState, path string) error {
	r.loadImageCalls++
	r.loadImageState = state
	r.loadImagePath = path
	return r.loadImageErr
}

func TestLoadImageExportsThenImportsExportedTar(t *testing.T) {
	statePath := writeReadyWorkerState(t, "worker-a")
	runtime := &loadImageRuntime{status: WorkerStatus{VM: "running", Network: "running"}}
	app := &App{Out: io.Discard, Err: io.Discard, Now: time.Now, Runtime: runtime}
	var savedRefs []string
	var savedDest string
	app.SaveImage = func(_ context.Context, _ string, refs []string, dest string) error {
		savedRefs = refs
		savedDest = dest
		return os.WriteFile(dest, []byte("dummy-image-tar"), 0o600)
	}
	if err := app.LoadImage(context.Background(), statePath, []string{"nginx:local", "app:poc"}, "", ""); err != nil {
		t.Fatalf("LoadImage: %v", err)
	}
	if runtime.loadImageCalls != 1 {
		t.Fatalf("Runtime.LoadImage calls = %d, want 1", runtime.loadImageCalls)
	}
	if runtime.loadImagePath != savedDest {
		t.Fatalf("Runtime.LoadImage tar = %q, exported tar = %q", runtime.loadImagePath, savedDest)
	}
	if len(savedRefs) != 2 || savedRefs[0] != "nginx:local" || savedRefs[1] != "app:poc" {
		t.Fatalf("SaveImage refs = %v", savedRefs)
	}
}

func TestLoadImageRefusesStoppedWorker(t *testing.T) {
	statePath := writeReadyWorkerState(t, "worker-a")
	runtime := &loadImageRuntime{status: WorkerStatus{VM: "stopped", Network: "stopped"}}
	app := &App{Out: io.Discard, Err: io.Discard, Now: time.Now, Runtime: runtime}
	app.SaveImage = func(context.Context, string, []string, string) error {
		t.Fatal("SaveImage must not run for a stopped worker")
		return nil
	}
	err := app.LoadImage(context.Background(), statePath, []string{"nginx:local"}, "", "")
	if err == nil || !strings.Contains(err.Error(), "is not running") {
		t.Fatalf("LoadImage error = %v, want not-running error", err)
	}
	if runtime.loadImageCalls != 0 {
		t.Fatalf("Runtime.LoadImage calls = %d, want 0", runtime.loadImageCalls)
	}
}

func TestLoadImageArchiveBypassesEngineExport(t *testing.T) {
	statePath := writeReadyWorkerState(t, "worker-a")
	archive := filepath.Join(t.TempDir(), "preexported.tar")
	if err := os.WriteFile(archive, []byte("archive-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &loadImageRuntime{status: WorkerStatus{VM: "running", Network: "running"}}
	app := &App{Out: io.Discard, Err: io.Discard, Now: time.Now, Runtime: runtime}
	app.SaveImage = func(context.Context, string, []string, string) error {
		t.Fatal("SaveImage must not run when --archive is set")
		return nil
	}
	if err := app.LoadImage(context.Background(), statePath, nil, archive, ""); err != nil {
		t.Fatalf("LoadImage: %v", err)
	}
	if runtime.loadImagePath != archive {
		t.Fatalf("Runtime.LoadImage tar = %q, want archive %q", runtime.loadImagePath, archive)
	}
}

func TestLoadImageRejectsMissingArchiveFile(t *testing.T) {
	statePath := writeReadyWorkerState(t, "worker-a")
	runtime := &loadImageRuntime{status: WorkerStatus{VM: "running", Network: "running"}}
	app := &App{Out: io.Discard, Err: io.Discard, Now: time.Now, Runtime: runtime}
	err := app.LoadImage(context.Background(), statePath, nil, filepath.Join(t.TempDir(), "absent.tar"), "")
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("LoadImage error = %v, want missing-archive error", err)
	}
	if runtime.loadImageCalls != 0 {
		t.Fatalf("Runtime.LoadImage calls = %d, want 0", runtime.loadImageCalls)
	}
}

type rejectingRuntime struct {
	err error
}

type deletingRuntime struct {
	rejectingRuntime
	err error
}

type resumeRuntime struct {
	startCalls           int
	waitReadyCalls       int
	installCalls         int
	removeBootstrapCalls int
	bundle               []byte
	removeBootstrapErr   error
	status               WorkerStatus
}

func (r *resumeRuntime) Preflight(context.Context) error { return nil }
func (r *resumeRuntime) Plan(context.Context, RuntimeConfig) (RuntimeState, error) {
	return RuntimeState{}, nil
}
func (r *resumeRuntime) Create(context.Context, *RuntimeState) error  { return nil }
func (r *resumeRuntime) Validate(context.Context, RuntimeState) error { return nil }
func (r *resumeRuntime) Start(_ context.Context, _ *RuntimeState) error {
	r.startCalls++
	return nil
}
func (r *resumeRuntime) WaitReady(context.Context, RuntimeState, time.Duration) error {
	r.waitReadyCalls++
	return nil
}
func (r *resumeRuntime) Stop(context.Context, RuntimeState) error   { return nil }
func (r *resumeRuntime) Delete(context.Context, RuntimeState) error { return nil }
func (r *resumeRuntime) InstallBundle(_ context.Context, _ RuntimeState, path string) error {
	r.installCalls++
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	r.bundle = data
	return nil
}
func (r *resumeRuntime) LoadImage(context.Context, RuntimeState, string) error { return nil }
func (r *resumeRuntime) RemoveBootstrapIdentity(context.Context, RuntimeState) error {
	r.removeBootstrapCalls++
	return r.removeBootstrapErr
}
func (r *resumeRuntime) Status(context.Context, *RuntimeState) (WorkerStatus, error) {
	return r.status, nil
}

func (r deletingRuntime) Validate(context.Context, RuntimeState) error { return nil }
func (r deletingRuntime) Delete(context.Context, RuntimeState) error   { return r.err }

func (r rejectingRuntime) Preflight(context.Context) error { return nil }
func (r rejectingRuntime) Plan(context.Context, RuntimeConfig) (RuntimeState, error) {
	return RuntimeState{}, nil
}
func (r rejectingRuntime) Create(context.Context, *RuntimeState) error  { return nil }
func (r rejectingRuntime) Validate(context.Context, RuntimeState) error { return r.err }
func (r rejectingRuntime) Start(context.Context, *RuntimeState) error   { return nil }
func (r rejectingRuntime) WaitReady(context.Context, RuntimeState, time.Duration) error {
	return nil
}
func (r rejectingRuntime) Stop(context.Context, RuntimeState) error   { return nil }
func (r rejectingRuntime) Delete(context.Context, RuntimeState) error { return nil }
func (r rejectingRuntime) InstallBundle(context.Context, RuntimeState, string) error {
	return nil
}
func (r rejectingRuntime) LoadImage(context.Context, RuntimeState, string) error       { return nil }
func (r rejectingRuntime) RemoveBootstrapIdentity(context.Context, RuntimeState) error { return nil }
func (r rejectingRuntime) Status(context.Context, *RuntimeState) (WorkerStatus, error) {
	return WorkerStatus{}, nil
}
