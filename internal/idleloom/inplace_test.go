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
)

// recordingExec captures what a runtime would run instead of running it.
type recordingExec struct {
	scripts  []string
	runs     [][]string
	sent     [][2]string
	replies  map[string]string
	failures map[string]error
}

func (r *recordingExec) Describe() string { return "the test guest" }

func (r *recordingExec) Run(_ context.Context, stdout, _ io.Writer, argv ...string) error {
	r.runs = append(r.runs, argv)
	command := strings.Join(argv, " ")
	if reply, ok := r.replies[command]; ok && stdout != nil {
		_, _ = io.WriteString(stdout, reply)
	}
	return r.failures[command]
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
	// Create makes the directory itself and refuses one that already exists.
	// The parent has to be canonical, because Create re-resolves the path and
	// refuses one that moved under it: t.TempDir hands back /var/... on macOS,
	// where /var is a symlink to /private/var, and an 8.3 short path on
	// Windows. Plan does this for the real caller.
	state := RuntimeState{NodeName: "worker-a", RuntimeDir: filepath.Join(canonicalTempDir(t), "runtime"), GuestIP: "198.18.18.42"}
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
	// The bundle carries a bootstrap token, so it must not be staged in a
	// world-writable directory where any local user can pre-create the name.
	if destination := exec.sent[0][1]; !strings.HasPrefix(destination, stagingDir+"/") {
		t.Errorf("bundle staged at %s, outside the private staging directory", destination)
	}
	staged := false
	for _, argv := range exec.runs {
		if strings.Join(argv, " ") == "install -d -m 0700 "+stagingDir {
			staged = true
		}
	}
	if !staged {
		t.Error("the staging directory is not created with a private mode first")
	}
	script := exec.allScripts()
	// The bundle carries a bootstrap token, so it must not survive a failed
	// install. An "&& rm" chain would leave it behind.
	if !strings.Contains(script, "|| status=$?") || !strings.Contains(script, "rm -f "+stagingDir+"/bundle.tar\nexit $status") {
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

// TestInPlaceDeleteReleasesKubeletMountsBeforeRemoving covers a failure mode
// the krunkit backend never had: there, deleting the VM discards everything
// at once. An in-place worker's directory tree survives the command, so
// kubelet's projected Pod volumes have to be unmounted or the removal fails
// on every one of them and leaves them mounted.
func TestInPlaceDeleteReleasesKubeletMountsBeforeRemoving(t *testing.T) {
	exec := &recordingExec{}
	runtime := InPlaceRuntime{Exec: exec, Kind: RuntimeLinux, Out: io.Discard, Err: io.Discard}
	if err := runtime.Delete(context.Background(), RuntimeState{NodeName: "worker-a"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	script := exec.allScripts()
	unmount := strings.Index(script, "umount")
	remove := strings.Index(script, "rm -rf /var/lib/idleloom /var/lib/kubelet")
	if unmount < 0 {
		t.Fatal("delete never unmounts kubelet's Pod volumes")
	}
	if remove < 0 {
		t.Fatal("delete never removes the worker state directories")
	}
	if unmount > remove {
		t.Error("delete removes /var/lib/kubelet before unmounting its Pod volumes")
	}
	if !strings.Contains(script, "sort -r") {
		t.Error("delete does not unmount nested mounts deepest-first")
	}
	// Either directory may sit on its own volume on a host Idleloom does not
	// otherwise control.
	if !strings.Contains(script, "kubelet|idleloom") {
		t.Error("delete only unmounts under /var/lib/kubelet, not the worker state directory")
	}
}

// failingExec fails the commands whose joined form contains a marker, so a
// preflight check can be observed rejecting the condition it guards.
type failingExec struct {
	recordingExec
	failScriptsContaining string
	failRunsContaining    string
}

func (f *failingExec) Run(ctx context.Context, stdout, stderr io.Writer, argv ...string) error {
	if f.failRunsContaining != "" && strings.Contains(strings.Join(argv, " "), f.failRunsContaining) {
		return errProbeRefused
	}
	return f.recordingExec.Run(ctx, stdout, stderr, argv...)
}

func (f *failingExec) Script(ctx context.Context, stdout, stderr io.Writer, script string) error {
	if f.failScriptsContaining != "" && strings.Contains(script, f.failScriptsContaining) {
		return errProbeRefused
	}
	return f.recordingExec.Script(ctx, stdout, stderr, script)
}

var errProbeRefused = errors.New("refused")

func TestPreflightRejectsAHostWithoutCgroupV2(t *testing.T) {
	exec := &failingExec{
		recordingExec: recordingExec{replies: map[string]string{
			"uname -s":                    "Linux\n",
			"systemctl is-system-running": "running\n",
			"cat /etc/os-release":         "ID=ubuntu\n",
		}},
		failRunsContaining: "/sys/fs/cgroup/cgroup.controllers",
	}
	runtime := InPlaceRuntime{Exec: exec, Kind: RuntimeWSL2}
	err := runtime.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected a host without cgroup v2 to be rejected")
	}
	// The remedy is the whole point of checking here: a hybrid hierarchy
	// surfaces later as ClusterIP timeouts with no obvious cause.
	if !strings.Contains(err.Error(), "cgroup_no_v1=all") {
		t.Errorf("error does not say how to fix it: %v", err)
	}
}

func TestPreflightRejectsAHostWithoutTheDummyDriver(t *testing.T) {
	exec := &failingExec{
		recordingExec: recordingExec{replies: map[string]string{
			"uname -s":                    "Linux\n",
			"systemctl is-system-running": "running\n",
			"cat /etc/os-release":         "ID=ubuntu\n",
		}},
		failScriptsContaining: "type dummy",
	}
	runtime := InPlaceRuntime{Exec: exec, Kind: RuntimeLinux}
	err := runtime.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected a host that cannot create a dummy link to be rejected")
	}
	if !strings.Contains(err.Error(), "dummy") {
		t.Errorf("error does not name the missing driver: %v", err)
	}
}

func TestPreflightRejectsANonLinuxGuest(t *testing.T) {
	exec := &recordingExec{replies: map[string]string{"uname -s": "Darwin\n"}}
	runtime := InPlaceRuntime{Exec: exec, Kind: RuntimeLinux}
	if err := runtime.Preflight(context.Background()); err == nil {
		t.Fatal("expected a non-Linux guest to be rejected")
	}
}

// TestWaitReadyDoesNotRequireKubelet covers resuming an interrupted
// enrollment: WaitReady runs before the bundle installs kubelet, so requiring
// kubelet there would deadlock every resume.
func TestWaitReadyDoesNotRequireKubelet(t *testing.T) {
	// No replies configured, so "systemctl is-active kubelet.service" answers
	// empty — kubelet is not running.
	exec := &recordingExec{}
	runtime := InPlaceRuntime{Exec: exec, Kind: RuntimeLinux}
	if err := runtime.WaitReady(context.Background(), RuntimeState{NodeName: "worker-a"}, time.Second); err != nil {
		t.Fatalf("WaitReady required kubelet to already be running: %v", err)
	}
}

func TestPreflightAcceptsUbuntuAndDebianDerivatives(t *testing.T) {
	for _, release := range []string{
		"NAME=\"Ubuntu\"\nID=ubuntu\nID_LIKE=debian\n",
		// Debian itself sets ID=debian and no ID_LIKE at all.
		"PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nNAME=\"Debian GNU/Linux\"\nID=debian\n",
		"ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n",
	} {
		exec := &recordingExec{replies: map[string]string{
			"uname -s":                    "Linux\n",
			"systemctl is-system-running": "running\n",
			"cat /etc/os-release":         release,
		}}
		runtime := InPlaceRuntime{Exec: exec, Kind: RuntimeLinux}
		if err := runtime.Preflight(context.Background()); err != nil {
			t.Errorf("rejected a Debian-family host (%q): %v", release, err)
		}
	}
}

// TestPreflightRejectsANonDebianDistribution matters because the prepare
// script installs containerd with apt-get: without this check it would run
// part-way and fail on a missing command, after changing the host.
func TestPreflightRejectsANonDebianDistribution(t *testing.T) {
	exec := &recordingExec{replies: map[string]string{
		"uname -s":                    "Linux\n",
		"systemctl is-system-running": "running\n",
		"cat /etc/os-release":         "NAME=\"Fedora Linux\"\nID=fedora\n",
	}}
	runtime := InPlaceRuntime{Exec: exec, Kind: RuntimeLinux}
	err := runtime.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected a non-Debian distribution to be rejected")
	}
	if !strings.Contains(err.Error(), "apt-get") {
		t.Errorf("error does not explain why: %v", err)
	}
}

// TestInPlaceRefusesARuntimeDirectoryItDidNotCreate guards against erasing a
// user's data: Delete removes the runtime directory recursively once its
// marker validates, so Create must never adopt one that already exists.
func TestInPlaceRefusesARuntimeDirectoryItDidNotCreate(t *testing.T) {
	existing := t.TempDir()
	keep := filepath.Join(existing, "important.txt")
	if err := os.WriteFile(keep, []byte("not idleloom's"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := InPlaceRuntime{Exec: &recordingExec{}, Kind: RuntimeLinux, Out: io.Discard, Err: io.Discard}

	_, err := runtime.Plan(context.Background(), RuntimeConfig{
		NodeName:   "worker-a",
		RuntimeDir: existing,
		Network:    RuntimeNetwork{GuestIP: "198.18.18.42"},
	})
	if err == nil {
		t.Fatal("Plan adopted a directory that already exists")
	}

	state := RuntimeState{NodeName: "worker-a", RuntimeDir: existing, GuestIP: "198.18.18.42"}
	if err := runtime.Create(context.Background(), &state); err == nil {
		t.Fatal("Create adopted a directory that already exists")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the pre-existing file was disturbed: %v", err)
	}
}

// canonicalTempDir is t.TempDir with the platform's aliases resolved, which is
// what Plan hands Create in production.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve the temporary directory: %v", err)
	}
	return resolved
}

// TestCheckSystemdIsRunningReadsTheState. The exit status cannot answer this:
// is-system-running also exits non-zero when merely degraded. Nor can
// "systemctl --version", which reports the installed client and succeeds with
// nothing managing the system — which is exactly a WSL2 distribution that has
// the package but no "systemd=true" in /etc/wsl.conf, and it would pass
// preflight and then fail partway through enrollment.
func TestCheckSystemdIsRunningReadsTheState(t *testing.T) {
	for _, c := range []struct {
		what   string
		output string
		failed bool
		accept bool
	}{
		{"healthy", "running\n", false, true},
		{"a unit failed somewhere on the host", "degraded\n", true, true},
		{"still booting", "starting\n", true, true},
		{"shutting down", "stopping\n", true, true},
		{"rescue mode", "maintenance\n", true, true},
		{"no systemd as PID 1", "System has not been booted with systemd as init system (PID 1). Can't operate.\nFailed to connect to bus: Host is down\noffline\n", true, false},
		{"systemctl absent", "", true, false},
		{"unknown", "unknown\n", true, false},
	} {
		exec := &recordingExec{replies: map[string]string{"systemctl is-system-running": c.output}}
		if c.failed {
			exec.failures = map[string]error{"systemctl is-system-running": errors.New("exit status 1")}
		}
		runtime := InPlaceRuntime{Exec: exec, Kind: RuntimeWSL2, Arch: "amd64"}
		err := runtime.checkSystemdIsRunning(context.Background())
		if c.accept && err != nil {
			t.Errorf("%s: rejected: %v", c.what, err)
		}
		if !c.accept && err == nil {
			t.Errorf("%s: accepted", c.what)
		}
		if !c.accept && err != nil && !strings.Contains(err.Error(), "wsl.conf") {
			t.Errorf("%s: the error does not name the remedy: %v", c.what, err)
		}
	}
}
