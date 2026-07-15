package idleloom

import (
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

type rejectingRuntime struct {
	err error
}

type deletingRuntime struct {
	rejectingRuntime
	err error
}

type resumeRuntime struct {
	startCalls         int
	waitReadyCalls     int
	installCalls       int
	bundle             []byte
	removeBootstrapErr error
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
func (r *resumeRuntime) RemoveBootstrapIdentity(context.Context, RuntimeState) error {
	return r.removeBootstrapErr
}
func (r *resumeRuntime) Status(context.Context, *RuntimeState) (WorkerStatus, error) {
	return WorkerStatus{}, nil
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
func (r rejectingRuntime) RemoveBootstrapIdentity(context.Context, RuntimeState) error { return nil }
func (r rejectingRuntime) Status(context.Context, *RuntimeState) (WorkerStatus, error) {
	return WorkerStatus{}, nil
}
