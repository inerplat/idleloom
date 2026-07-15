package agent

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/execution"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExpiredAPIDeadlineKillsJournaledProcess(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	startToken, err := (DarwinPlatform{}).ProcessStartToken(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	store, err := execution.Open(t.TempDir() + "/execution.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	record := execution.Record{
		SchemaVersion: execution.SchemaVersionV1, WorkloadUID: "workload", WorkloadGeneration: 1,
		AssignmentUID: "assignment", ExecutionID: "11111111-1111-4111-8111-111111111111", FencingEpoch: 1,
		Executable: "/bin/sh", RuntimeVersion: "test", Nonce: "nonce",
	}
	if err := store.Begin(record); err != nil {
		t.Fatal(err)
	}
	started := record
	started.PID = cmd.Process.Pid
	started.ProcessStartToken = startToken
	if err := store.UpdateProcess(record, started); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	agent := &DevAgent{
		config: DevAgentConfig{Now: func() time.Time { return now }, Platform: DarwinPlatform{}},
		store:  store,
		assignment: &nativev1alpha1.IdleloomWorkloadAssignment{
			ObjectMeta: metav1.ObjectMeta{UID: "assignment"},
			Spec:       nativev1alpha1.IdleloomWorkloadAssignmentSpec{LeaseDurationSeconds: 10},
		},
		lastAPISuccess: now.Add(-11 * time.Second),
	}
	agent.selfFenceIfExpired()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("expired API deadline did not kill process")
	}
}
