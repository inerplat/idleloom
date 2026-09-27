package idleloom

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// InPlaceRuntime enrolls a Linux environment that already exists instead of
// provisioning a virtual machine.
//
// On a Linux host that environment is the host itself. On a Windows host it is
// a WSL2 distribution, which is itself a Hyper-V virtual machine that Windows
// manages. Either way there is no disk image to build, no cloud-init seed, and
// no SSH hop: the guest executor reaches the environment directly.
type InPlaceRuntime struct {
	Exec guestExec
	Kind RuntimeKind
	Arch string
	Out  io.Writer
	Err  io.Writer
}

func (r InPlaceRuntime) Backend() RuntimeKind { return r.Kind }

func (r InPlaceRuntime) GuestArch() string { return r.Arch }

const (
	// nodeAddressScript assigns the worker's node address to a dummy link.
	//
	// kubelet refuses a --node-ip that is not present on a local interface,
	// but the address it registers with is the mesh address, which the
	// WireKube agent only configures once the node exists and its agent Pod
	// is scheduled. Holding the address on a dummy link breaks that cycle.
	// The agent reconciles addresses only on its own tunnel device, so the
	// two coexist: the dummy owns the address for local binding while the
	// tunnel carries traffic from remote peers.
	nodeAddressScript = "/usr/local/sbin/idleloom-node-address"
	nodeAddressUnit   = "idleloom-node-address.service"
	nodeAddressLink   = "idleloom0"

	// stagingDir holds files on their way into the worker. It is deliberately
	// not /tmp: the bundle carries a bootstrap token, and a fixed name in a
	// world-writable directory lets any local user pre-create it — as a
	// symlink, to redirect a root-owned write, or just to read the contents.
	stagingDir = "/var/lib/idleloom/staging"
)

// ensureStagingDir creates the private directory transfers land in.
func (r InPlaceRuntime) ensureStagingDir(ctx context.Context) error {
	if err := r.Exec.Run(ctx, io.Discard, io.Discard, "install", "-d", "-m", "0700", stagingDir); err != nil {
		return fmt.Errorf("create the staging directory on %s: %w", r.Exec.Describe(), err)
	}
	return nil
}

func (r InPlaceRuntime) Preflight(ctx context.Context) error {
	var out bytes.Buffer
	if err := r.Exec.Run(ctx, &out, &out, "uname", "-s"); err != nil {
		return fmt.Errorf("reach %s: %w; %s", r.Exec.Describe(), err, strings.TrimSpace(out.String()))
	}
	if system := strings.TrimSpace(out.String()); system != "Linux" {
		return fmt.Errorf("%s reports %q, but an Idleloom worker needs Linux", r.Exec.Describe(), system)
	}
	if err := r.checkSystemdIsRunning(ctx); err != nil {
		return err
	}
	if err := r.checkDebianFamily(ctx); err != nil {
		return err
	}
	if err := r.checkCgroupV2(ctx); err != nil {
		return err
	}
	return r.checkDummyInterface(ctx)
}

// checkDebianFamily refuses a distribution the prepare script cannot build on.
//
// That script installs containerd and the CNI plugins with apt-get, under
// Debian's package names. On anything else it would run some way in and then
// fail on a missing command, after having already changed the host.
func (r InPlaceRuntime) checkDebianFamily(ctx context.Context) error {
	var out bytes.Buffer
	if err := r.Exec.Run(ctx, &out, io.Discard, "cat", "/etc/os-release"); err != nil {
		return fmt.Errorf("read the distribution of %s: %w", r.Exec.Describe(), err)
	}
	release := out.String()
	for _, line := range strings.Split(release, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		value = strings.Trim(value, `"`)
		if key == "ID" && (value == "ubuntu" || value == "debian") {
			return nil
		}
		if key == "ID_LIKE" && slices.Contains(strings.Fields(value), "debian") {
			return nil
		}
	}
	return fmt.Errorf("%s does not run Ubuntu or a Debian derivative; Idleloom installs the worker's containerd and CNI plugins with apt-get", r.Exec.Describe())
}

// checkCgroupV2 refuses a host still exposing the cgroup v1 hierarchy.
//
// kubelet's systemd cgroup driver needs a unified hierarchy, and a hybrid one
// breaks in a way that surfaces far from the cause: on a Cilium cluster the
// socket load balancer cannot attach, and every ClusterIP call from a Pod
// times out long after enrollment reported success.
func (r InPlaceRuntime) checkCgroupV2(ctx context.Context) error {
	if err := r.Exec.Run(ctx, io.Discard, io.Discard, "test", "-e", "/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return fmt.Errorf("%s does not expose the unified cgroup v2 hierarchy, which kubelet's systemd cgroup driver requires; %s", r.Exec.Describe(), r.cgroupHint())
	}
	return nil
}

func (r InPlaceRuntime) cgroupHint() string {
	if r.Kind == RuntimeWSL2 {
		return `add "kernelCommandLine=cgroup_no_v1=all" under [wsl2] in %UserProfile%\.wslconfig and run "wsl --shutdown"`
	}
	return `boot the host with "systemd.unified_cgroup_hierarchy=1 cgroup_no_v1=all"`
}

// checkDummyInterface confirms the worker can hold its node address.
//
// The mesh address is assigned to a dummy link before kubelet starts, so a
// kernel without that driver cannot enrol. Checking here turns what would be
// a mid-enrollment failure into a preflight one. Loopback is deliberately not
// used as a fallback: Cilium treats addresses on lo as NodePort-capable and
// would SNAT service traffic to an address no peer can route back to.
func (r InPlaceRuntime) checkDummyInterface(ctx context.Context) error {
	const probe = "idleloom-probe0"
	script := "set -e\nip link add " + probe + " type dummy\nip link delete " + probe + "\n"
	var out bytes.Buffer
	if err := r.Exec.Script(ctx, &out, &out, script); err != nil {
		return fmt.Errorf("%s cannot create a dummy network interface, which the worker node address needs: %w; %s. Load the kernel's dummy module and retry", r.Exec.Describe(), err, strings.TrimSpace(out.String()))
	}
	return nil
}

// runningSystemdStates are the is-system-running answers that mean systemd is
// the service manager. "degraded" belongs here: a unit failed somewhere on the
// host, which is common and is not this worker's problem.
var runningSystemdStates = map[string]struct{}{
	"running":      {},
	"degraded":     {},
	"starting":     {},
	"stopping":     {},
	"maintenance":  {},
	"initializing": {},
}

// checkSystemdIsRunning refuses a host where systemd is not PID 1.
//
// The exit status alone cannot answer this, because is-system-running also
// exits non-zero when it is merely degraded. Nor can "systemctl --version",
// which prints the installed client's version and succeeds whether or not
// anything is managing the system — a WSL2 distribution with the systemd
// package installed but not enabled in /etc/wsl.conf passes that check and
// then fails partway through enrollment, after the host has been changed.
//
// So read what it says instead. systemd answers with a state word; without a
// manager to ask, systemctl says so on stderr and prints "offline" or nothing.
func (r InPlaceRuntime) checkSystemdIsRunning(ctx context.Context) error {
	var out bytes.Buffer
	runErr := r.Exec.Run(ctx, &out, &out, "systemctl", "is-system-running")
	for _, line := range strings.Split(out.String(), "\n") {
		if _, ok := runningSystemdStates[strings.TrimSpace(line)]; ok {
			return nil
		}
	}
	detail := strings.TrimSpace(out.String())
	if detail == "" && runErr != nil {
		detail = runErr.Error()
	}
	return fmt.Errorf("%s has no running systemd, which kubelet requires: %s; %s",
		r.Exec.Describe(), detail, r.systemdHint())
}

func (r InPlaceRuntime) systemdHint() string {
	if r.Kind == RuntimeWSL2 {
		return "set \"[boot]\\nsystemd=true\" in the distribution's /etc/wsl.conf and run \"wsl --shutdown\""
	}
	return "start the host with systemd as PID 1"
}

func (r InPlaceRuntime) Plan(_ context.Context, cfg RuntimeConfig) (RuntimeState, error) {
	if cfg.Network.GuestIP == "" {
		return RuntimeState{}, fmt.Errorf("the worker node address is not assigned")
	}
	runtimeDir := cfg.RuntimeDir
	if runtimeDir == "" {
		var err error
		if runtimeDir, err = defaultRuntimeDir(cfg.NodeName); err != nil {
			return RuntimeState{}, err
		}
	}
	runtimeDir, err := canonicalPlannedPath(runtimeDir)
	if err != nil {
		return RuntimeState{}, fmt.Errorf("resolve runtime directory: %w", err)
	}
	// Delete removes this directory recursively once its marker validates, so
	// adopting one that already exists would put a directory Idleloom did not
	// create on a path to being erased.
	if _, err := os.Lstat(runtimeDir); err == nil {
		return RuntimeState{}, fmt.Errorf("runtime directory already exists: %s, likely from an interrupted worker; if no worker is using it, remove the directory and rerun, or pass --runtime-dir", runtimeDir)
	} else if !os.IsNotExist(err) {
		return RuntimeState{}, fmt.Errorf("inspect runtime directory: %w", err)
	}
	return RuntimeState{
		NodeName:   cfg.NodeName,
		RuntimeDir: runtimeDir,
		GuestIP:    cfg.Network.GuestIP,
		Subnet:     cfg.Network.Subnet,
		Planned:    true,
	}, nil
}

func (r InPlaceRuntime) Create(ctx context.Context, state *RuntimeState) error {
	if state == nil {
		return fmt.Errorf("runtime state is nil")
	}
	if err := os.MkdirAll(filepath.Dir(state.RuntimeDir), 0o700); err != nil {
		return fmt.Errorf("create runtime parent directory: %w", err)
	}
	// Mkdir rather than MkdirAll: the directory must not exist yet, because
	// Delete removes it recursively.
	if err := os.Mkdir(state.RuntimeDir, 0o700); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("runtime directory already exists: %s, likely from an interrupted worker; if no worker is using it, remove the directory and rerun, or pass --runtime-dir", state.RuntimeDir)
		}
		return fmt.Errorf("create runtime directory: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(state.RuntimeDir)
	if err != nil {
		_ = os.Remove(state.RuntimeDir)
		return fmt.Errorf("resolve canonical runtime directory: %w", err)
	}
	if canonical != state.RuntimeDir {
		_ = os.Remove(state.RuntimeDir)
		return fmt.Errorf("planned runtime directory changed after creation: planned=%q canonical=%q", state.RuntimeDir, canonical)
	}
	if err := writeRuntimeMarker(*state); err != nil {
		_ = os.Remove(state.RuntimeDir)
		return err
	}
	state.Planned = false
	if err := r.Exec.Script(ctx, r.Out, r.Err, renderPrepareScript()); err != nil {
		return fmt.Errorf("prepare %s as a Kubernetes worker: %w", r.Exec.Describe(), err)
	}
	if err := r.Exec.Script(ctx, r.Out, r.Err, renderNodeAddressInstall(state.GuestIP)); err != nil {
		return fmt.Errorf("assign the worker node address on %s: %w", r.Exec.Describe(), err)
	}
	return nil
}

func (r InPlaceRuntime) Validate(ctx context.Context, state RuntimeState) error {
	if state.RuntimeDir == "" {
		return nil
	}
	if _, err := os.Stat(state.RuntimeDir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect runtime directory %s: %w", state.RuntimeDir, err)
	}
	_, err := validateRuntimeMarker(state)
	return err
}

func (r InPlaceRuntime) Start(ctx context.Context, state *RuntimeState) error {
	if state == nil {
		return fmt.Errorf("runtime state is nil")
	}
	script := "set -eu\nsystemctl start " + nodeAddressUnit + "\nsystemctl start containerd.service\n" +
		// "systemctl cat" fails when the unit does not exist; list-unit-files
		// exits 0 even with no match, so it cannot be used as a guard. The
		// unit is absent when enrollment was interrupted before the bundle
		// installed it, which is exactly when a resume runs this.
		"if systemctl cat kubelet.service >/dev/null 2>&1; then systemctl start kubelet.service; fi\n"
	if err := r.Exec.Script(ctx, r.Out, r.Err, script); err != nil {
		return fmt.Errorf("start the worker on %s: %w", r.Exec.Describe(), err)
	}
	return nil
}

// WaitReady waits for the guest to be usable, not for kubelet to be running.
//
// It is called during enrollment before the bundle is installed, and again
// when resuming an interrupted enrollment — in both cases kubelet may not
// exist yet. The krunkit backend draws the same line: it waits for SSH to
// answer, not for a workload to be up.
func (r InPlaceRuntime) WaitReady(ctx context.Context, _ RuntimeState, timeout time.Duration) error {
	return waitUntilFor(ctx, timeout, r.Exec.Describe()+" to become reachable", func() bool {
		return r.Exec.Run(ctx, io.Discard, io.Discard, "true") == nil
	})
}

func (r InPlaceRuntime) Stop(ctx context.Context, _ RuntimeState) error {
	script := "systemctl stop kubelet.service 2>/dev/null || true\n"
	if err := r.Exec.Script(ctx, r.Out, r.Err, script); err != nil {
		return fmt.Errorf("stop the worker kubelet on %s: %w", r.Exec.Describe(), err)
	}
	return nil
}

func (r InPlaceRuntime) Delete(ctx context.Context, state RuntimeState) error {
	// kubelet leaves each Pod's projected volumes mounted under
	// /var/lib/kubelet. Removing the tree without unmounting them first fails
	// on every one of those paths, and on an in-place worker there is no VM
	// teardown afterwards to clean up what was left behind. The worker state
	// directory is covered too, since an operator may have put either on its
	// own volume. Unmount deepest first so nested mounts release before their
	// parents.
	script := `set -u
systemctl disable --now kubelet.service 2>/dev/null || true
systemctl disable --now ` + nodeAddressUnit + ` 2>/dev/null || true
rm -f /etc/systemd/system/kubelet.service /etc/systemd/system/` + nodeAddressUnit + `
rm -f ` + nodeAddressScript + `
systemctl daemon-reload 2>/dev/null || true
awk '$2 ~ /^\/var\/lib\/(kubelet|idleloom)/ { print $2 }' /proc/self/mounts | sort -r | while read -r mount; do
	umount "$mount" 2>/dev/null || umount -l "$mount" 2>/dev/null || true
done
rm -rf /var/lib/idleloom /var/lib/kubelet /etc/kubernetes
ip link delete ` + nodeAddressLink + ` 2>/dev/null || true
`
	if err := r.Exec.Script(ctx, r.Out, r.Err, script); err != nil {
		return fmt.Errorf("remove the worker from %s: %w", r.Exec.Describe(), err)
	}
	if state.RuntimeDir != "" {
		if _, err := validateRuntimeMarker(state); err == nil {
			if err := os.RemoveAll(state.RuntimeDir); err != nil {
				return fmt.Errorf("remove runtime directory: %w", err)
			}
		}
	}
	return nil
}

func (r InPlaceRuntime) InstallBundle(ctx context.Context, state RuntimeState, bundlePath string) error {
	const destination = stagingDir + "/bundle.tar"
	if err := r.ensureStagingDir(ctx); err != nil {
		return err
	}
	if err := r.Exec.Send(ctx, bundlePath, destination); err != nil {
		return fmt.Errorf("copy worker bundle into %s: %w", r.Exec.Describe(), err)
	}
	// The bundle carries a bootstrap token and may carry credential-provider
	// secrets, so it is removed whether or not the install succeeds.
	script := `status=0
chmod 600 ` + destination + `
rm -rf /var/lib/idleloom/config
install -d -m 0700 /var/lib/idleloom/config
tar -xf ` + destination + ` -C /var/lib/idleloom/config && /var/lib/idleloom/config/install.sh || status=$?
rm -f ` + destination + `
exit $status
`
	if err := r.Exec.Script(ctx, r.Out, r.Err, script); err != nil {
		return fmt.Errorf("install worker configuration on %s: %w", r.Exec.Describe(), err)
	}
	return nil
}

func (r InPlaceRuntime) LoadImage(ctx context.Context, _ RuntimeState, localTarPath string) error {
	const destination = stagingDir + "/image.tar"
	if err := r.ensureStagingDir(ctx); err != nil {
		return err
	}
	if err := r.Exec.Send(ctx, localTarPath, destination); err != nil {
		return fmt.Errorf("copy image archive into %s: %w", r.Exec.Describe(), err)
	}
	// Import into the k8s.io namespace so kubelet's CRI view resolves the
	// image; the default namespace would be invisible to it.
	script := `status=0
ctr -n k8s.io images import ` + destination + ` || status=$?
rm -f ` + destination + `
exit $status
`
	if err := r.Exec.Script(ctx, r.Out, r.Err, script); err != nil {
		return fmt.Errorf("import image into worker containerd on %s: %w", r.Exec.Describe(), err)
	}
	return nil
}

func (r InPlaceRuntime) RemoveBootstrapIdentity(ctx context.Context, _ RuntimeState) error {
	if err := r.Exec.Run(ctx, r.Out, r.Err, "rm", "-f", "/var/lib/idleloom/config/bootstrap-kubelet.conf"); err != nil {
		return fmt.Errorf("remove bootstrap identity from %s: %w", r.Exec.Describe(), err)
	}
	return nil
}

func (r InPlaceRuntime) Status(ctx context.Context, state *RuntimeState) (WorkerStatus, error) {
	if state == nil {
		return WorkerStatus{}, fmt.Errorf("runtime state is nil")
	}
	status := WorkerStatus{VM: "stopped", Network: "stopped"}
	var out bytes.Buffer
	// is-active exits non-zero for an inactive unit, which is a normal answer
	// rather than a failure, so the exit status is ignored and the printed
	// state is read instead.
	_ = r.Exec.Run(ctx, &out, io.Discard, "systemctl", "is-active", "kubelet.service")
	if strings.TrimSpace(out.String()) == "active" {
		status.VM = "running"
	}
	if state.GuestIP != "" {
		out.Reset()
		_ = r.Exec.Run(ctx, &out, io.Discard, "ip", "-o", "addr", "show", "dev", nodeAddressLink)
		if strings.Contains(out.String(), state.GuestIP) {
			status.Network = "running"
		}
	}
	return status, nil
}

// renderNodeAddressInstall writes and enables the unit that holds the worker's
// node address before kubelet starts.
func renderNodeAddressInstall(address string) string {
	return `set -eu
install -d -m 0755 /usr/local/sbin
cat > ` + nodeAddressScript + ` <<'IDLELOOM_ADDRESS'
#!/bin/sh
set -eu
link=` + nodeAddressLink + `
address=` + shellQuote(address) + `
case "${1:-up}" in
up)
	ip link show "$link" >/dev/null 2>&1 || ip link add "$link" type dummy
	ip link set "$link" up
	ip addr replace "$address/32" dev "$link"
	;;
down)
	ip link delete "$link" 2>/dev/null || true
	;;
esac
IDLELOOM_ADDRESS
chmod 0755 ` + nodeAddressScript + `

cat > /etc/systemd/system/` + nodeAddressUnit + ` <<'IDLELOOM_UNIT'
[Unit]
Description=Idleloom worker node address
Documentation=https://inerplat.github.io/idleloom/
Before=kubelet.service
After=network-pre.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=` + nodeAddressScript + ` up
ExecStop=` + nodeAddressScript + ` down

[Install]
WantedBy=multi-user.target
IDLELOOM_UNIT

systemctl daemon-reload
systemctl enable --now ` + nodeAddressUnit + `
`
}
