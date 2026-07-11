package idleloom

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRuntimeNetworkIsStableAndNodeSpecific(t *testing.T) {
	firstIndex := runtimeNetworkIndex("studio-idle", 0)
	first := runtimeNetworkFromIndex("studio-idle", firstIndex)
	second := runtimeNetworkFromIndex("studio-idle", firstIndex)
	other := runtimeNetworkFromIndex("render-idle", runtimeNetworkIndex("render-idle", 0))
	if first != second {
		t.Fatalf("runtime network is not stable: %#v != %#v", first, second)
	}
	if first.Subnet == other.Subnet || first.GuestIP == other.GuestIP || first.MAC == other.MAC {
		t.Fatalf("different nodes received overlapping identities: %#v and %#v", first, other)
	}
	if !strings.HasPrefix(first.MAC, "02:") {
		t.Fatalf("MAC %q is not locally administered unicast", first.MAC)
	}
}

func TestRuntimeNetworkCanProbePastCollision(t *testing.T) {
	first := runtimeNetworkIndex("studio-idle", 0)
	second := runtimeNetworkIndex("studio-idle", 1)
	if first == second {
		t.Fatalf("network probing reused index %d", first)
	}
	if runtimeNetworkFromIndex("studio-idle", first).GuestIP == runtimeNetworkFromIndex("studio-idle", second).GuestIP {
		t.Fatal("network probing did not change the guest IP")
	}
}

func TestKrunkitArgsUseDirectRuntimeDevices(t *testing.T) {
	state := RuntimeState{
		NodeName: "test-node", RuntimeDir: "/tmp/idleloom/runtime", RootDisk: "/tmp/idleloom/runtime/root.qcow2",
		DataDisk: "/tmp/idleloom/runtime/data.raw", SeedISO: "/tmp/idleloom/runtime/seed.iso",
		MACAddress: "02:00:00:00:00:01", CPUs: 4, MemoryMB: 8192,
	}
	joined := strings.Join(krunkitArgs(state), " ")
	for _, expected := range []string{
		"virtio-net,type=unixgram", "offloading=on", "vfkitMagic=on",
		"virtio-blk,path=/tmp/idleloom/runtime/root.qcow2,format=qcow2",
		"virtio-blk,path=/tmp/idleloom/runtime/data.raw,format=raw",
		"virtio-blk,path=/tmp/idleloom/runtime/seed.iso,format=raw",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("krunkit arguments are missing %q: %s", expected, joined)
		}
	}
}

func TestCloudInitPreparesContainerStorageAndISCSI(t *testing.T) {
	data := renderCloudInit("studio-idle", "ssh-ed25519 AAAA test")
	for _, expected := range []string{"/dev/vdb", "/var/lib/idleloom/$name", "containerd:/var/lib/containerd", "apt-cache:/var/cache/apt", "open-iscsi"} {
		if !strings.Contains(data, expected) {
			t.Errorf("cloud-init is missing %q", expected)
		}
	}
}

func TestGVProxyConfigUsesStaticGuestIdentity(t *testing.T) {
	runtimeDir := t.TempDir()
	state := RuntimeState{
		RuntimeDir: runtimeDir,
		Subnet:     "172.20.10.0/29",
		GatewayIP:  "172.20.10.1",
		GuestIP:    "172.20.10.2",
		HostIP:     "172.20.10.6",
		MACAddress: "02:00:00:00:00:01",
		SSHPort:    22022,
	}
	if err := writeGVProxyConfig(state); err != nil {
		t.Fatalf("writeGVProxyConfig: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(runtimeDir, "gvproxy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	for _, expected := range []string{
		`subnet: "172.20.10.0/29"`,
		`"127.0.0.1:22022": "172.20.10.2:22"`,
		`"172.20.10.2": "02:00:00:00:00:01"`,
		`vfkit: "unixgram://`,
	} {
		if !strings.Contains(config, expected) {
			t.Errorf("gvproxy configuration is missing %q:\n%s", expected, config)
		}
	}
}

func TestRuntimeProcessesStartInTheirOwnSession(t *testing.T) {
	command := detachedCommand("true")
	if command.SysProcAttr == nil || !command.SysProcAttr.Setsid {
		t.Fatal("runtime process is not detached into its own session")
	}
}

func TestRuntimeLockSerializesLifecycleOperations(t *testing.T) {
	runtimeDir := t.TempDir()
	state := RuntimeState{NodeName: "worker-a", RuntimeDir: runtimeDir}
	if err := writeRuntimeMarker(state); err != nil {
		t.Fatal(err)
	}
	first, err := acquireRuntimeLock(context.Background(), state, true)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := acquireRuntimeLock(ctx, state, false); err == nil {
		t.Fatal("second lifecycle operation acquired an already-held runtime lock")
	}
}

func TestRuntimeMarkerRejectsDifferentNode(t *testing.T) {
	runtimeDir := t.TempDir()
	owner := RuntimeState{NodeName: "worker-a", RuntimeDir: runtimeDir}
	if err := writeRuntimeMarker(owner); err != nil {
		t.Fatal(err)
	}
	other := owner
	other.NodeName = "worker-b"
	if err := validateRuntimeOwnership(other); err == nil {
		t.Fatal("runtime marker accepted a different node owner")
	}
}

func TestRuntimeStatusRecoversDurableSSHPort(t *testing.T) {
	runtimeDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := RuntimeState{
		NodeName: "worker-a", RuntimeDir: runtimeDir,
		RootDisk: filepath.Join(runtimeDir, "root.qcow2"), DataDisk: filepath.Join(runtimeDir, "data.raw"),
		SeedISO: filepath.Join(runtimeDir, "seed.iso"), SSHPrivateKey: filepath.Join(runtimeDir, "id_ed25519"),
		SSHPort: 22022,
	}
	if err := writeRuntimeMarker(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, runtimeLockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	metadataState := state
	metadataState.SSHPort = 23023
	if err := writeRuntimeMetadata(metadataState); err != nil {
		t.Fatal(err)
	}
	if _, err := (KrunkitRuntime{}).Status(context.Background(), &state); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if state.SSHPort != metadataState.SSHPort {
		t.Fatalf("SSH port = %d, want recovered port %d", state.SSHPort, metadataState.SSHPort)
	}
}

func TestRuntimePlanJournalsCanonicalPathsBeforeCreation(t *testing.T) {
	parent := t.TempDir()
	planned, err := (KrunkitRuntime{}).Plan(context.Background(), RuntimeConfig{
		NodeName: "worker-a", CPUs: 4, MemoryMB: 8192, DiskMB: 40960,
		RuntimeDir: filepath.Join(parent, "nested", "runtime"),
		Network: RuntimeNetwork{
			Subnet: "172.20.10.0/29", GatewayIP: "172.20.10.1", GuestIP: "172.20.10.2",
			HostIP: "172.20.10.6", MAC: "02:00:00:00:00:01",
		},
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if planned.RuntimeDir == "" || planned.RootDisk != filepath.Join(planned.RuntimeDir, "root.qcow2") || planned.DiskMB != 40960 {
		t.Fatalf("incomplete planned runtime: %+v", planned)
	}
	if err := (KrunkitRuntime{}).Validate(context.Background(), planned); err != nil {
		t.Fatalf("absent but planned runtime should be safe to clean up: %v", err)
	}
}

func TestDeleteRecoversEmptyUnmarkedPlannedDirectory(t *testing.T) {
	planned, err := (KrunkitRuntime{}).Plan(context.Background(), RuntimeConfig{
		NodeName: "worker-a", CPUs: 4, MemoryMB: 8192, DiskMB: 40960,
		RuntimeDir: filepath.Join(t.TempDir(), "runtime"),
		Network: RuntimeNetwork{
			Subnet: "172.20.10.0/29", GatewayIP: "172.20.10.1", GuestIP: "172.20.10.2",
			HostIP: "172.20.10.6", MAC: "02:00:00:00:00:01",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(planned.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (KrunkitRuntime{}).Validate(context.Background(), planned); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := (KrunkitRuntime{}).Delete(context.Background(), planned); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(planned.RuntimeDir); !os.IsNotExist(err) {
		t.Fatalf("planned directory remains: %v", err)
	}
}

func TestValidateRejectsNonEmptyUnmarkedPlannedDirectory(t *testing.T) {
	planned, err := (KrunkitRuntime{}).Plan(context.Background(), RuntimeConfig{
		NodeName: "worker-a", CPUs: 4, MemoryMB: 8192, DiskMB: 40960,
		RuntimeDir: filepath.Join(t.TempDir(), "runtime"),
		Network: RuntimeNetwork{
			Subnet: "172.20.10.0/29", GatewayIP: "172.20.10.1", GuestIP: "172.20.10.2",
			HostIP: "172.20.10.6", MAC: "02:00:00:00:00:01",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(planned.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(planned.RuntimeDir, "unexpected"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (KrunkitRuntime{}).Validate(context.Background(), planned); err == nil {
		t.Fatal("non-empty unmarked directory was accepted")
	}
}

func TestTerminatePIDWaitsForProcessExit(t *testing.T) {
	command := detachedCommand("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	if err := command.Process.Release(); err != nil {
		t.Fatal(err)
	}
	if err := terminatePID(pid, "sleep", 2*time.Second); err != nil {
		t.Fatalf("terminatePID: %v", err)
	}
	process, _ := os.FindProcess(pid)
	if process != nil && process.Signal(syscall.Signal(0)) == nil && !processIsZombie(pid) {
		t.Fatalf("process %d is still running after terminatePID", pid)
	}
}
