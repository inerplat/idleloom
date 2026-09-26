package idleloom

import (
	"context"
	"io"
	"strings"
	"testing"
)

// recordingExec captures what a runtime would run instead of running it.
type recordingExec struct {
	scripts []string
	runs    [][]string
	sent    [][2]string
	replies map[string]string
}

func (r *recordingExec) Describe() string { return "the test guest" }

func (r *recordingExec) Run(_ context.Context, stdout, _ io.Writer, argv ...string) error {
	r.runs = append(r.runs, argv)
	if reply, ok := r.replies[strings.Join(argv, " ")]; ok && stdout != nil {
		_, _ = io.WriteString(stdout, reply)
	}
	return nil
}

func (r *recordingExec) Script(_ context.Context, _, _ io.Writer, script string) error {
	r.scripts = append(r.scripts, script)
	return nil
}

func (r *recordingExec) Send(_ context.Context, localPath, guestPath string) error {
	r.sent = append(r.sent, [2]string{localPath, guestPath})
	return nil
}

func (r *recordingExec) allScripts() string { return strings.Join(r.scripts, "\n") }

func TestInPlacePlanRequiresANodeAddress(t *testing.T) {
	runtime := InPlaceRuntime{Exec: &recordingExec{}, Kind: RuntimeLinux, Arch: "amd64"}
	if _, err := runtime.Plan(context.Background(), RuntimeConfig{NodeName: "worker-a"}); err == nil {
		t.Fatal("expected a worker without a node address to be rejected")
	}
}

// TestInPlaceCreateHoldsTheNodeAddressBeforeKubelet covers the ordering that
// makes a mesh address usable as --node-ip: the address has to exist locally
// before kubelet registers, but the WireKube agent that would normally own it
// cannot run until the node exists.
func TestInPlaceCreateHoldsTheNodeAddressBeforeKubelet(t *testing.T) {
	exec := &recordingExec{}
	runtime := InPlaceRuntime{Exec: exec, Kind: RuntimeLinux, Arch: "amd64", Out: io.Discard, Err: io.Discard}
	state := RuntimeState{NodeName: "worker-a", RuntimeDir: t.TempDir(), GuestIP: "198.18.18.42"}
	if err := runtime.Create(context.Background(), &state); err != nil {
		t.Fatalf("Create: %v", err)
	}
	scripts := exec.allScripts()
	for _, expected := range []string{
		"ip addr replace \"$address/32\" dev \"$link\"",
		"address='198.18.18.42'",
		"Before=kubelet.service",
		"systemctl enable --now " + nodeAddressUnit,
	} {
		if !strings.Contains(scripts, expected) {
			t.Errorf("node address setup is missing %q", expected)
		}
	}
	if state.Planned {
		t.Error("runtime state is still marked as planned after Create")
	}
}

func TestInPlaceInstallBundleRemovesTheBundleEvenWhenInstallFails(t *testing.T) {
	exec := &recordingExec{}
	runtime := InPlaceRuntime{Exec: exec, Kind: RuntimeLinux, Out: io.Discard, Err: io.Discard}
	if err := runtime.InstallBundle(context.Background(), RuntimeState{}, "/tmp/bundle.tar"); err != nil {
		t.Fatalf("InstallBundle: %v", err)
	}
	if len(exec.sent) != 1 || exec.sent[0][0] != "/tmp/bundle.tar" {
		t.Fatalf("bundle was not copied into the guest: %v", exec.sent)
	}
	script := exec.allScripts()
	// The bundle carries a bootstrap token, so it must not survive a failed
	// install. An "&& rm" chain would leave it behind.
	if !strings.Contains(script, "|| status=$?") || !strings.Contains(script, "rm -f /tmp/idleloom-bundle.tar\nexit $status") {
		t.Errorf("bundle is not removed unconditionally:\n%s", script)
	}
}

func TestInPlaceStatusReadsKubeletAndTheNodeAddress(t *testing.T) {
	exec := &recordingExec{replies: map[string]string{
		"systemctl is-active kubelet.service":    "active\n",
		"ip -o addr show dev " + nodeAddressLink: "5: idleloom0    inet 198.18.18.42/32 scope global idleloom0\n",
	}}
	runtime := InPlaceRuntime{Exec: exec, Kind: RuntimeLinux}
	state := RuntimeState{NodeName: "worker-a", GuestIP: "198.18.18.42"}
	status, err := runtime.Status(context.Background(), &state)
	if err != nil {
		t.Fatal(err)
	}
	if status.VM != "running" || status.Network != "running" {
		t.Fatalf("status = %+v, want both running", status)
	}
}

func TestBundlePinsTheNodeIPForInPlaceWorkers(t *testing.T) {
	pinned := renderInstallScript(BundleConfig{NodeName: "worker-a", NodeIP: "198.18.18.42"})
	if !strings.Contains(pinned, "node_ip='198.18.18.42'") {
		t.Error("in-place bundle does not pin the node IP")
	}
	if strings.Contains(pinned, "route get 1.1.1.1") {
		t.Error("in-place bundle still detects the node IP from the host route")
	}
	detected := renderInstallScript(BundleConfig{NodeName: "worker-a"})
	if !strings.Contains(detected, "route get 1.1.1.1") {
		t.Error("krunkit bundle no longer detects its own node IP")
	}
}
