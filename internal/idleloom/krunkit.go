package idleloom

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	ubuntuImageName = "ubuntu-24.04-server-cloudimg-arm64.img"
	ubuntuImageBase = "https://cloud-images.ubuntu.com/releases/24.04/release/"
	runtimeMarker   = ".idleloom-runtime"
	runtimeLockName = ".idleloom.lock"
	runtimeMetadata = "runtime.json"
)

type KrunkitRuntime struct {
	Runner CommandRunner
	Out    io.Writer
	Err    io.Writer
}

func (k KrunkitRuntime) Preflight(ctx context.Context) error {
	for _, binary := range []string{"krunkit", "gvproxy", "ssh", "scp", "ssh-keygen", "hdiutil"} {
		if _, err := exec.LookPath(binary); err != nil {
			return fmt.Errorf("required executable %q was not found in PATH", binary)
		}
	}
	if output, err := k.Runner.Output(ctx, "krunkit", "--version"); err != nil {
		return fmt.Errorf("run krunkit: %w", err)
	} else if strings.TrimSpace(string(output)) == "" {
		return fmt.Errorf("krunkit returned an empty version")
	}
	if _, err := k.Runner.Output(ctx, "gvproxy", "-version"); err != nil {
		return fmt.Errorf("run gvproxy: %w", err)
	}
	return nil
}

func (k KrunkitRuntime) Plan(_ context.Context, cfg RuntimeConfig) (RuntimeState, error) {
	runtimeDir := cfg.RuntimeDir
	var err error
	if runtimeDir == "" {
		runtimeDir, err = defaultRuntimeDir(cfg.NodeName)
		if err != nil {
			return RuntimeState{}, err
		}
	}
	runtimeDir, err = canonicalPlannedPath(runtimeDir)
	if err != nil {
		return RuntimeState{}, fmt.Errorf("resolve runtime directory: %w", err)
	}
	if strings.Contains(runtimeDir, ",") {
		return RuntimeState{}, fmt.Errorf("runtime directory cannot contain a comma: %s", runtimeDir)
	}
	if _, err := os.Lstat(runtimeDir); err == nil {
		return RuntimeState{}, fmt.Errorf("runtime directory already exists: %s", runtimeDir)
	} else if !os.IsNotExist(err) {
		return RuntimeState{}, fmt.Errorf("inspect runtime directory: %w", err)
	}
	if cfg.Network.Subnet == "" || cfg.Network.GuestIP == "" || cfg.Network.MAC == "" {
		return RuntimeState{}, fmt.Errorf("worker network reservation is incomplete")
	}
	return RuntimeState{
		NodeName:      cfg.NodeName,
		RuntimeDir:    runtimeDir,
		RootDisk:      filepath.Join(runtimeDir, "root.qcow2"),
		DataDisk:      filepath.Join(runtimeDir, "data.raw"),
		SeedISO:       filepath.Join(runtimeDir, "seed.iso"),
		SSHPrivateKey: filepath.Join(runtimeDir, "id_ed25519"),
		MACAddress:    cfg.Network.MAC,
		Subnet:        cfg.Network.Subnet,
		GatewayIP:     cfg.Network.GatewayIP,
		GuestIP:       cfg.Network.GuestIP,
		HostIP:        cfg.Network.HostIP,
		CPUs:          cfg.CPUs,
		MemoryMB:      cfg.MemoryMB,
		DiskMB:        cfg.DiskMB,
		Planned:       true,
	}, nil
}

func (k KrunkitRuntime) Create(ctx context.Context, state *RuntimeState) (err error) {
	if state == nil {
		return fmt.Errorf("runtime state is nil")
	}
	if err := validatePlannedRuntime(*state); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(state.RuntimeDir), 0o700); err != nil {
		return fmt.Errorf("create runtime parent directory: %w", err)
	}
	if err := os.Mkdir(state.RuntimeDir, 0o700); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("runtime directory already exists: %s", state.RuntimeDir)
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
	lock, err := acquireRuntimeLock(ctx, *state, true)
	if err != nil {
		return err
	}
	defer lock.Close()
	cleanup := true
	defer func() {
		if cleanup {
			if cleanupErr := k.stopUnlocked(context.Background(), *state); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()

	if err := k.generateSSHKey(ctx, *state); err != nil {
		return err
	}
	baseImage, err := downloadUbuntuImage(ctx)
	if err != nil {
		return err
	}
	if err := cloneOrCopyFile(baseImage, state.RootDisk, 0o600); err != nil {
		return fmt.Errorf("create worker root disk: %w", err)
	}
	dataDisk, err := os.OpenFile(state.DataDisk, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create worker data disk: %w", err)
	}
	if err := dataDisk.Truncate(int64(state.DiskMB) * 1024 * 1024); err != nil {
		dataDisk.Close()
		return fmt.Errorf("size worker data disk: %w", err)
	}
	if err := dataDisk.Close(); err != nil {
		return fmt.Errorf("close worker data disk: %w", err)
	}
	if err := k.createSeedISO(ctx, state.NodeName, *state); err != nil {
		return err
	}
	state.SSHPort, err = availableTCPPort()
	if err != nil {
		return err
	}
	if err := writeGVProxyConfig(*state); err != nil {
		return err
	}
	if err := k.startUnlocked(ctx, state); err != nil {
		return err
	}
	if err := k.waitForSSH(ctx, *state, 8*time.Minute); err != nil {
		return err
	}
	if err := k.ssh(ctx, *state, "sudo cloud-init status --wait && test -f /var/lib/idleloom/.prepared"); err != nil {
		return fmt.Errorf("prepare Ubuntu worker: %w; inspect %s", err, filepath.Join(state.RuntimeDir, "serial.log"))
	}
	cleanup = false
	return nil
}

func (k KrunkitRuntime) Start(ctx context.Context, state *RuntimeState) error {
	if state == nil {
		return fmt.Errorf("runtime state is nil")
	}
	if err := validateRuntimeOwnership(*state); err != nil {
		return err
	}
	lock, err := acquireRuntimeLock(ctx, *state, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := validateRuntimeOwnership(*state); err != nil {
		return err
	}
	if _, err := recoverRuntimeMetadata(state); err != nil {
		return err
	}
	return k.startUnlocked(ctx, state)
}

func (k KrunkitRuntime) Validate(ctx context.Context, state RuntimeState) error {
	if state.RuntimeDir == "" {
		return nil
	}
	if _, err := os.Stat(state.RuntimeDir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect runtime directory %s: %w", state.RuntimeDir, err)
	}
	if err := validateRuntimeOwnership(state); err != nil {
		if recoverable, recoverErr := recoverablePlannedRuntimeDirectory(state); recoverErr != nil {
			return recoverErr
		} else if recoverable {
			return nil
		}
		return err
	}
	lock, err := acquireRuntimeLock(ctx, state, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	return validateRuntimeOwnership(state)
}

func (k KrunkitRuntime) startUnlocked(ctx context.Context, state *RuntimeState) error {
	status, err := k.statusUnlocked(*state)
	if err != nil {
		return err
	}
	if status.VM == "running" && status.Network == "running" {
		return nil
	}
	if status.VM == "running" || status.Network == "running" {
		if err := k.stopUnlocked(ctx, *state); err != nil {
			return err
		}
	}
	for _, path := range []string{krunkitSocket(*state), krunkitPIDFile(*state)} {
		_ = os.Remove(path)
	}
	gvproxyPID, err := k.startGVProxy(ctx, state)
	if err != nil {
		return err
	}

	krunkitLog, err := os.OpenFile(filepath.Join(state.RuntimeDir, "krunkit-launch.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		cleanupErr := terminatePID(gvproxyPID, "gvproxy", 5*time.Second)
		return errors.Join(fmt.Errorf("open krunkit launch log: %w", err), cleanupErr)
	}
	command := detachedCommand("krunkit", krunkitArgs(*state)...)
	command.Stdout = krunkitLog
	command.Stderr = krunkitLog
	if err := command.Start(); err != nil {
		krunkitLog.Close()
		cleanupErr := terminatePID(gvproxyPID, "gvproxy", 5*time.Second)
		return errors.Join(fmt.Errorf("start krunkit: %w", err), cleanupErr)
	}
	krunkitPID := command.Process.Pid
	_ = command.Process.Release()
	_ = krunkitLog.Close()
	if err := waitForProcess(ctx, krunkitPIDFile(*state), "krunkit", 15*time.Second); err != nil {
		krunkitCleanupErr := terminatePID(krunkitPID, "krunkit", 5*time.Second)
		gvproxyCleanupErr := terminatePID(gvproxyPID, "gvproxy", 5*time.Second)
		return errors.Join(
			fmt.Errorf("wait for krunkit: %w; inspect %s", err, filepath.Join(state.RuntimeDir, "krunkit.log")),
			krunkitCleanupErr,
			gvproxyCleanupErr,
		)
	}
	return nil
}

func (k KrunkitRuntime) startGVProxy(ctx context.Context, state *RuntimeState) (int, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if state.SSHPort == 0 || !tcpPortAvailable(state.SSHPort) {
			port, err := availableTCPPort()
			if err != nil {
				return 0, err
			}
			state.SSHPort = port
		}
		if err := writeRuntimeMetadata(*state); err != nil {
			return 0, err
		}
		if err := writeGVProxyConfig(*state); err != nil {
			return 0, err
		}
		_ = os.Remove(networkSocket(*state))
		_ = os.Remove(gvproxyPIDFile(*state))
		gvproxyLog, err := os.OpenFile(filepath.Join(state.RuntimeDir, "gvproxy-launch.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, fmt.Errorf("open gvproxy launch log: %w", err)
		}
		gvproxy := detachedCommand("gvproxy", "-config", filepath.Join(state.RuntimeDir, "gvproxy.yaml"))
		gvproxy.Stdout = gvproxyLog
		gvproxy.Stderr = gvproxyLog
		if err := gvproxy.Start(); err != nil {
			gvproxyLog.Close()
			return 0, fmt.Errorf("start gvproxy: %w", err)
		}
		pid := gvproxy.Process.Pid
		_ = gvproxy.Process.Release()
		_ = gvproxyLog.Close()
		pathErr := waitForPath(ctx, networkSocket(*state), 3*time.Second)
		processErr := waitForProcess(ctx, gvproxyPIDFile(*state), "gvproxy", 3*time.Second)
		if pathErr == nil && processErr == nil {
			return pid, nil
		}
		if cleanupErr := terminatePID(pid, "gvproxy", 5*time.Second); cleanupErr != nil {
			return 0, errors.Join(pathErr, processErr, cleanupErr)
		}
		lastErr = errors.Join(pathErr, processErr)
		state.SSHPort = 0
	}
	return 0, fmt.Errorf("start gvproxy after 5 port attempts: %w; inspect %s", lastErr, filepath.Join(state.RuntimeDir, "gvproxy.log"))
}

func (k KrunkitRuntime) WaitReady(ctx context.Context, state RuntimeState, timeout time.Duration) error {
	return k.waitForSSH(ctx, state, timeout)
}

func (k KrunkitRuntime) Stop(ctx context.Context, state RuntimeState) error {
	if _, err := os.Stat(state.RuntimeDir); os.IsNotExist(err) {
		return nil
	}
	if err := validateRuntimeOwnership(state); err != nil {
		return err
	}
	lock, err := acquireRuntimeLock(ctx, state, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := validateRuntimeOwnership(state); err != nil {
		return err
	}
	return k.stopUnlocked(ctx, state)
}

func (k KrunkitRuntime) stopUnlocked(ctx context.Context, state RuntimeState) error {
	pid, running, err := processFromPIDFile(krunkitPIDFile(state), "krunkit")
	if err != nil {
		return err
	}
	if running {
		if err := requestVMStop(ctx, krunkitSocket(state)); err != nil {
			process, _ := os.FindProcess(pid)
			if process != nil {
				_ = process.Signal(syscall.SIGTERM)
			}
		}
		if err := waitForPIDExit(ctx, pid, 30*time.Second); err != nil {
			if terminateErr := terminatePID(pid, "krunkit", 10*time.Second); terminateErr != nil {
				return terminateErr
			}
		}
	}
	if err := stopPIDFile(gvproxyPIDFile(state), "gvproxy", 10*time.Second); err != nil {
		return err
	}
	return nil
}

func (k KrunkitRuntime) Delete(ctx context.Context, state RuntimeState) error {
	if _, err := os.Stat(state.RuntimeDir); os.IsNotExist(err) {
		return nil
	}
	if recoverable, err := recoverablePlannedRuntimeDirectory(state); err != nil {
		return err
	} else if recoverable {
		if err := os.Remove(state.RuntimeDir); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove empty planned runtime directory %s: %w", state.RuntimeDir, err)
		}
		return nil
	}
	if err := validateRuntimeOwnership(state); err != nil {
		return err
	}
	lock, err := acquireRuntimeLock(ctx, state, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := validateRuntimeOwnership(state); err != nil {
		return err
	}
	if err := k.stopUnlocked(ctx, state); err != nil {
		return err
	}
	if err := os.RemoveAll(state.RuntimeDir); err != nil {
		return fmt.Errorf("remove runtime directory %s: %w", state.RuntimeDir, err)
	}
	return nil
}

func (k KrunkitRuntime) InstallBundle(ctx context.Context, state RuntimeState, bundlePath string) error {
	destination := "/tmp/idleloom-bundle.tar"
	args := []string{
		"-i", state.SSHPrivateKey,
		"-P", strconv.Itoa(state.SSHPort),
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + filepath.Join(state.RuntimeDir, "known_hosts"),
		bundlePath, "idleloom@127.0.0.1:" + destination,
	}
	if err := k.Runner.Run(ctx, k.Out, k.Err, "scp", args...); err != nil {
		return fmt.Errorf("copy worker bundle into VM: %w", err)
	}
	command := "sudo rm -rf /var/lib/idleloom/config && sudo install -d -m 0700 /var/lib/idleloom/config && " +
		"sudo tar -xf " + destination + " -C /var/lib/idleloom/config && " +
		"sudo /var/lib/idleloom/config/install.sh && rm -f " + destination
	if err := k.ssh(ctx, state, command); err != nil {
		return fmt.Errorf("install worker configuration: %w", err)
	}
	return nil
}

func (k KrunkitRuntime) RemoveBootstrapIdentity(ctx context.Context, state RuntimeState) error {
	if err := k.ssh(ctx, state, "sudo rm -f /var/lib/idleloom/config/bootstrap-kubelet.conf"); err != nil {
		return fmt.Errorf("remove bootstrap identity from worker: %w", err)
	}
	return nil
}

func (k KrunkitRuntime) Status(ctx context.Context, state *RuntimeState) (WorkerStatus, error) {
	if state == nil {
		return WorkerStatus{}, fmt.Errorf("runtime state is nil")
	}
	if _, err := os.Stat(state.RuntimeDir); os.IsNotExist(err) {
		return WorkerStatus{VM: "stopped", Network: "stopped"}, nil
	}
	if err := validateRuntimeOwnership(*state); err != nil {
		return WorkerStatus{}, err
	}
	lock, err := acquireRuntimeLock(ctx, *state, false)
	if err != nil {
		return WorkerStatus{}, err
	}
	defer lock.Close()
	if err := validateRuntimeOwnership(*state); err != nil {
		return WorkerStatus{}, err
	}
	if _, err := recoverRuntimeMetadata(state); err != nil {
		return WorkerStatus{}, err
	}
	return k.statusUnlocked(*state)
}

func (k KrunkitRuntime) statusUnlocked(state RuntimeState) (WorkerStatus, error) {
	status := WorkerStatus{VM: "stopped", Network: "stopped"}
	_, running, err := processFromPIDFile(gvproxyPIDFile(state), "gvproxy")
	if err != nil {
		return status, err
	}
	if running {
		status.Network = "running"
	}
	_, running, err = processFromPIDFile(krunkitPIDFile(state), "krunkit")
	if err != nil {
		return status, err
	}
	if running {
		status.VM = "running"
	}
	return status, nil
}

func (k KrunkitRuntime) generateSSHKey(ctx context.Context, state RuntimeState) error {
	if err := k.Runner.Run(ctx, k.Out, k.Err, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "idleloom", "-f", state.SSHPrivateKey); err != nil {
		return fmt.Errorf("generate worker SSH key: %w", err)
	}
	if err := os.Chmod(state.SSHPrivateKey, 0o600); err != nil {
		return fmt.Errorf("protect worker SSH key: %w", err)
	}
	return nil
}

func (k KrunkitRuntime) createSeedISO(ctx context.Context, nodeName string, state RuntimeState) error {
	publicKey, err := os.ReadFile(state.SSHPrivateKey + ".pub")
	if err != nil {
		return fmt.Errorf("read worker SSH public key: %w", err)
	}
	seedDir := filepath.Join(state.RuntimeDir, "seed")
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		return fmt.Errorf("create cloud-init seed directory: %w", err)
	}
	files := map[string]string{
		"user-data": renderCloudInit(nodeName, strings.TrimSpace(string(publicKey))),
		"meta-data": "instance-id: idleloom-" + nodeName + "\nlocal-hostname: " + nodeName + "\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(seedDir, name), []byte(data), 0o600); err != nil {
			return fmt.Errorf("write cloud-init %s: %w", name, err)
		}
	}
	if err := k.Runner.Run(ctx, k.Out, k.Err, "hdiutil", "makehybrid", "-iso", "-joliet", "-default-volume-name", "cidata", "-o", state.SeedISO, seedDir); err != nil {
		return fmt.Errorf("create cloud-init seed ISO: %w", err)
	}
	if _, err := os.Stat(state.SeedISO); err != nil {
		return fmt.Errorf("cloud-init seed ISO was not created: %w", err)
	}
	if err := os.RemoveAll(seedDir); err != nil {
		return fmt.Errorf("remove cloud-init seed directory: %w", err)
	}
	return nil
}

func (k KrunkitRuntime) waitForSSH(ctx context.Context, state RuntimeState, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		err := k.sshQuiet(probeCtx, state, "true")
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out after %s waiting for worker SSH; inspect %s", timeout, filepath.Join(state.RuntimeDir, "serial.log"))
		case <-ticker.C:
		}
	}
}

func (k KrunkitRuntime) sshQuiet(ctx context.Context, state RuntimeState, remoteCommand string) error {
	args := append(k.sshBaseArgs(state), "idleloom@127.0.0.1", remoteCommand)
	command := exec.CommandContext(ctx, "ssh", args...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}

func (k KrunkitRuntime) ssh(ctx context.Context, state RuntimeState, command string) error {
	args := append(k.sshBaseArgs(state), "idleloom@127.0.0.1", command)
	return k.Runner.Run(ctx, k.Out, k.Err, "ssh", args...)
}

func (k KrunkitRuntime) sshBaseArgs(state RuntimeState) []string {
	return []string{
		"-i", state.SSHPrivateKey,
		"-p", strconv.Itoa(state.SSHPort),
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + filepath.Join(state.RuntimeDir, "known_hosts"),
		"-o", "ConnectTimeout=5",
	}
}

func writeGVProxyConfig(state RuntimeState) error {
	config := fmt.Sprintf(`stack:
  subnet: %q
  gatewayIP: %q
  forwards:
    %q: %q
  nat:
    %q: "127.0.0.1"
  gatewayVirtualIPs:
    - %q
  dhcpStaticLeases:
    %q: %q
interfaces:
  vfkit: %q
pid-file: %q
log-file: %q
`, state.Subnet, state.GatewayIP,
		fmt.Sprintf("127.0.0.1:%d", state.SSHPort), state.GuestIP+":22",
		state.HostIP, state.HostIP, state.GuestIP, state.MACAddress,
		"unixgram://"+networkSocket(state), gvproxyPIDFile(state), filepath.Join(state.RuntimeDir, "gvproxy.log"))
	if err := os.WriteFile(filepath.Join(state.RuntimeDir, "gvproxy.yaml"), []byte(config), 0o600); err != nil {
		return fmt.Errorf("write gvproxy configuration: %w", err)
	}
	return nil
}

func krunkitArgs(state RuntimeState) []string {
	return []string{
		"--cpus", strconv.Itoa(state.CPUs),
		"--memory", strconv.Itoa(state.MemoryMB),
		"--pidfile", krunkitPIDFile(state),
		"--restful-uri", "unix://" + krunkitSocket(state),
		"--log-file", filepath.Join(state.RuntimeDir, "krunkit.log"),
		"--device", "virtio-net,type=unixgram,path=" + networkSocket(state) + ",mac=" + state.MACAddress + ",offloading=on,vfkitMagic=on",
		"--device", "virtio-serial,logFilePath=" + filepath.Join(state.RuntimeDir, "serial.log"),
		"--device", "virtio-blk,path=" + state.RootDisk + ",format=qcow2",
		"--device", "virtio-blk,path=" + state.DataDisk + ",format=raw",
		"--device", "virtio-blk,path=" + state.SeedISO + ",format=raw",
	}
}

func detachedCommand(name string, args ...string) *exec.Cmd {
	command := exec.Command(name, args...)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return command
}

func renderCloudInit(nodeName, publicKey string) string {
	return fmt.Sprintf(`#cloud-config
hostname: %s
manage_etc_hosts: true
ssh_pwauth: false
disable_root: true
users:
  - name: idleloom
    gecos: Idleloom Worker
    groups: [adm, sudo]
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - %s
write_files:
  - path: /etc/modules-load.d/idleloom.conf
    permissions: '0644'
    content: |
      overlay
      br_netfilter
  - path: /etc/sysctl.d/99-idleloom-kubernetes.conf
    permissions: '0644'
    content: |
      net.ipv4.ip_forward = 1
      net.bridge.bridge-nf-call-iptables = 1
      net.bridge.bridge-nf-call-ip6tables = 1
  - path: /usr/local/sbin/idleloom-prepare
    permissions: '0755'
    content: |
      #!/bin/bash
      set -euo pipefail
      swapoff -a
      sed -i.bak '/[[:space:]]swap[[:space:]]/d' /etc/fstab
      systemctl stop containerd.service 2>/dev/null || true
      if ! blkid /dev/vdb >/dev/null 2>&1; then
        blocks=$(( $(blockdev --getsize64 /dev/vdb) / 4096 - 16 ))
        mkfs.ext4 -F -L idleloom-data -b 4096 /dev/vdb "$blocks"
      fi
      install -d -m 0755 /var/lib/idleloom
      if ! mountpoint -q /var/lib/idleloom; then
        mount LABEL=idleloom-data /var/lib/idleloom
      fi
      grep -q '^LABEL=idleloom-data ' /etc/fstab || echo 'LABEL=idleloom-data /var/lib/idleloom ext4 defaults,nofail 0 2' >> /etc/fstab
      for mapping in containerd:/var/lib/containerd kubelet:/var/lib/kubelet apt-cache:/var/cache/apt apt-lists:/var/lib/apt/lists; do
        name=${mapping%%:*}
        target=${mapping#*:}
        install -d -m 0755 /var/lib/idleloom/$name "$target"
        if ! mountpoint -q "$target"; then
          mount --bind /var/lib/idleloom/$name "$target"
        fi
        grep -q "^/var/lib/idleloom/$name " /etc/fstab || echo "/var/lib/idleloom/$name $target none bind 0 0" >> /etc/fstab
      done
      modprobe overlay
      modprobe br_netfilter
      sysctl --system >/dev/null
      export DEBIAN_FRONTEND=noninteractive
      apt-get update
      apt-get install -y --no-install-recommends containerd containernetworking-plugins conntrack ebtables ethtool ipset iptables nfs-common open-iscsi socat
      apt-get clean
      install -d -m 0755 /etc/containerd
      containerd config default > /etc/containerd/config.toml
      sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
      systemctl enable --now containerd.service iscsid.service
      install -d -m 0755 /var/lib/idleloom
      touch /var/lib/idleloom/.prepared
runcmd:
  - [/usr/local/sbin/idleloom-prepare]
final_message: "Idleloom base system is ready"
`, nodeName, publicKey)
}

func downloadUbuntuImage(ctx context.Context) (string, error) {
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find user cache directory: %w", err)
	}
	dir := filepath.Join(cacheRoot, "idleloom", "images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create image cache: %w", err)
	}
	expected, err := ubuntuImageChecksum(ctx)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(dir, ubuntuImageName)
	if actual, err := fileSHA256(destination); err == nil && actual == expected {
		return destination, nil
	}
	temporary, err := os.CreateTemp(dir, ".ubuntu-*.img")
	if err != nil {
		return "", fmt.Errorf("create Ubuntu image download: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ubuntuImageBase+ubuntuImageName, nil)
	if err != nil {
		temporary.Close()
		return "", fmt.Errorf("create Ubuntu image request: %w", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		temporary.Close()
		return "", fmt.Errorf("download Ubuntu image: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		temporary.Close()
		return "", fmt.Errorf("download Ubuntu image: HTTP %s", response.Status)
	}
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temporary, hash), response.Body); err != nil {
		temporary.Close()
		return "", fmt.Errorf("write Ubuntu image: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close Ubuntu image: %w", err)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return "", fmt.Errorf("Ubuntu image checksum mismatch: expected %s, got %s", expected, actual)
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return "", fmt.Errorf("protect Ubuntu image: %w", err)
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return "", fmt.Errorf("cache Ubuntu image: %w", err)
	}
	return destination, nil
}

func ubuntuImageChecksum(ctx context.Context) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ubuntuImageBase+"SHA256SUMS", nil)
	if err != nil {
		return "", fmt.Errorf("create Ubuntu checksum request: %w", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("download Ubuntu checksums: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download Ubuntu checksums: HTTP %s", response.Status)
	}
	scanner := bufio.NewScanner(io.LimitReader(response.Body, 2*1024*1024))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == ubuntuImageName {
			if len(fields[0]) != sha256.Size*2 {
				return "", fmt.Errorf("invalid Ubuntu image checksum")
			}
			if _, err := hex.DecodeString(fields[0]); err != nil {
				return "", fmt.Errorf("invalid Ubuntu image checksum: %w", err)
			}
			return strings.ToLower(fields[0]), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read Ubuntu checksums: %w", err)
	}
	return "", fmt.Errorf("Ubuntu checksums do not contain %s", ubuntuImageName)
}

func cloneOrCopyFile(source, destination string, mode os.FileMode) error {
	if err := exec.Command("cp", "-c", source, destination).Run(); err == nil {
		return os.Chmod(destination, mode)
	}
	_ = os.Remove(destination)
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func defaultRuntimeDir(nodeName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".idleloom", "runtimes", nodeName), nil
}

func canonicalPlannedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := abs
	var missing []string
	for {
		if _, err := os.Lstat(current); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for %s", abs)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
	canonical, err := filepath.EvalSymlinks(current)
	if err != nil {
		return "", err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		canonical = filepath.Join(canonical, missing[i])
	}
	return filepath.Clean(canonical), nil
}

func validatePlannedRuntime(state RuntimeState) error {
	if state.NodeName == "" || state.RuntimeDir == "" {
		return fmt.Errorf("planned runtime identity is incomplete")
	}
	if !filepath.IsAbs(state.RuntimeDir) || filepath.Clean(state.RuntimeDir) != state.RuntimeDir {
		return fmt.Errorf("planned runtime directory is not canonical: %q", state.RuntimeDir)
	}
	if state.Subnet == "" || state.GatewayIP == "" || state.GuestIP == "" || state.HostIP == "" || state.MACAddress == "" {
		return fmt.Errorf("planned runtime network is incomplete")
	}
	if state.CPUs < 1 || state.MemoryMB < 1 || state.DiskMB < 1 {
		return fmt.Errorf("planned runtime resources are incomplete")
	}
	expected := map[string]string{
		"root disk":       filepath.Join(state.RuntimeDir, "root.qcow2"),
		"data disk":       filepath.Join(state.RuntimeDir, "data.raw"),
		"seed ISO":        filepath.Join(state.RuntimeDir, "seed.iso"),
		"SSH private key": filepath.Join(state.RuntimeDir, "id_ed25519"),
	}
	actual := map[string]string{
		"root disk":       state.RootDisk,
		"data disk":       state.DataDisk,
		"seed ISO":        state.SeedISO,
		"SSH private key": state.SSHPrivateKey,
	}
	for name, path := range expected {
		if actual[name] != path {
			return fmt.Errorf("planned runtime %s path mismatch: state=%q expected=%q", name, actual[name], path)
		}
	}
	return nil
}

func recoverablePlannedRuntimeDirectory(state RuntimeState) (bool, error) {
	if !state.Planned {
		return false, nil
	}
	if err := validatePlannedRuntime(state); err != nil {
		return false, err
	}
	info, err := os.Lstat(state.RuntimeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	canonical, err := filepath.EvalSymlinks(state.RuntimeDir)
	if err != nil {
		return false, err
	}
	if canonical != state.RuntimeDir {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(state.RuntimeDir, runtimeMarker)); err == nil || !os.IsNotExist(err) {
		return false, nil
	}
	entries, err := os.ReadDir(state.RuntimeDir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

type runtimeMarkerData struct {
	NodeName   string `json:"nodeName"`
	RuntimeDir string `json:"runtimeDir"`
}

func writeRuntimeMarker(state RuntimeState) error {
	data, err := json.Marshal(runtimeMarkerData{NodeName: state.NodeName, RuntimeDir: state.RuntimeDir})
	if err != nil {
		return fmt.Errorf("encode runtime marker: %w", err)
	}
	if err := atomicWriteFile(filepath.Join(state.RuntimeDir, runtimeMarker), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write runtime marker: %w", err)
	}
	return nil
}

type runtimeMetadataData struct {
	SSHPort int `json:"sshPort"`
}

func writeRuntimeMetadata(state RuntimeState) error {
	if state.SSHPort < 1024 || state.SSHPort > 65535 {
		return fmt.Errorf("invalid worker SSH port %d", state.SSHPort)
	}
	data, err := json.Marshal(runtimeMetadataData{SSHPort: state.SSHPort})
	if err != nil {
		return fmt.Errorf("encode runtime metadata: %w", err)
	}
	if err := atomicWriteFile(filepath.Join(state.RuntimeDir, runtimeMetadata), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write runtime metadata: %w", err)
	}
	return nil
}

func recoverRuntimeMetadata(state *RuntimeState) (bool, error) {
	data, err := os.ReadFile(filepath.Join(state.RuntimeDir, runtimeMetadata))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read runtime metadata: %w", err)
	}
	var metadata runtimeMetadataData
	if err := json.Unmarshal(data, &metadata); err != nil {
		return false, fmt.Errorf("decode runtime metadata: %w", err)
	}
	if metadata.SSHPort < 1024 || metadata.SSHPort > 65535 {
		return false, fmt.Errorf("runtime metadata has invalid SSH port %d", metadata.SSHPort)
	}
	if state.SSHPort == metadata.SSHPort {
		return false, nil
	}
	state.SSHPort = metadata.SSHPort
	return true, nil
}

func validateRuntimeOwnership(state RuntimeState) error {
	if state.NodeName == "" {
		return fmt.Errorf("runtime state has no node name")
	}
	canonical, err := filepath.EvalSymlinks(state.RuntimeDir)
	if err != nil {
		return fmt.Errorf("resolve runtime directory %s: %w", state.RuntimeDir, err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return fmt.Errorf("resolve absolute runtime directory: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(canonical, runtimeMarker))
	if err != nil {
		return fmt.Errorf("refusing to use unmarked runtime directory %s: %w", canonical, err)
	}
	var marker runtimeMarkerData
	if err := json.Unmarshal(data, &marker); err != nil {
		return fmt.Errorf("decode runtime marker in %s: %w", canonical, err)
	}
	if marker.NodeName != state.NodeName {
		return fmt.Errorf("runtime directory %s belongs to node %q, not %q", canonical, marker.NodeName, state.NodeName)
	}
	if marker.RuntimeDir != canonical || state.RuntimeDir != canonical {
		return fmt.Errorf("runtime directory ownership mismatch: marker=%q state=%q canonical=%q", marker.RuntimeDir, state.RuntimeDir, canonical)
	}
	expectedPaths := map[string]string{
		"root disk":       filepath.Join(canonical, "root.qcow2"),
		"data disk":       filepath.Join(canonical, "data.raw"),
		"seed ISO":        filepath.Join(canonical, "seed.iso"),
		"SSH private key": filepath.Join(canonical, "id_ed25519"),
	}
	actualPaths := map[string]string{
		"root disk":       state.RootDisk,
		"data disk":       state.DataDisk,
		"seed ISO":        state.SeedISO,
		"SSH private key": state.SSHPrivateKey,
	}
	for name, expected := range expectedPaths {
		if actualPaths[name] != expected {
			return fmt.Errorf("runtime %s path mismatch: state=%q expected=%q", name, actualPaths[name], expected)
		}
	}
	return nil
}

type runtimeLock struct {
	file *os.File
}

func acquireRuntimeLock(ctx context.Context, state RuntimeState, create bool) (*runtimeLock, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(filepath.Join(state.RuntimeDir, runtimeLockName), flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open runtime lock: %w", err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return &runtimeLock{file: file}, nil
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			file.Close()
			return nil, fmt.Errorf("lock runtime directory: %w", err)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, fmt.Errorf("wait for runtime lock: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (l *runtimeLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func availableTCPPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate worker SSH port: %w", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func tcpPortAvailable(port int) bool {
	if port < 1024 || port > 65535 {
		return false
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func waitForPath(ctx context.Context, path string, timeout time.Duration) error {
	return waitUntil(ctx, timeout, func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
}

func waitForProcess(ctx context.Context, pidFile, executable string, timeout time.Duration) error {
	return waitUntil(ctx, timeout, func() bool {
		_, running, _ := processFromPIDFile(pidFile, executable)
		return running
	})
}

func waitUntil(ctx context.Context, timeout time.Duration, ready func() bool) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ready() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out after %s", timeout)
		case <-ticker.C:
		}
	}
}

func processFromPIDFile(path, executable string) (int, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read %s pid file: %w", executable, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false, fmt.Errorf("invalid %s pid file %s", executable, path)
	}
	process, err := os.FindProcess(pid)
	if err != nil || process.Signal(syscall.Signal(0)) != nil || processIsZombie(pid) {
		return pid, false, nil
	}
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return pid, false, nil
	}
	if filepath.Base(strings.TrimSpace(string(output))) != executable {
		return pid, false, fmt.Errorf("pid %d from %s belongs to %q, not %s", pid, path, strings.TrimSpace(string(output)), executable)
	}
	return pid, true, nil
}

func stopPIDFile(path, executable string, timeout time.Duration) error {
	pid, running, err := processFromPIDFile(path, executable)
	if err != nil {
		return err
	}
	if !running {
		_ = os.Remove(path)
		return nil
	}
	if err := terminatePID(pid, executable, timeout); err != nil {
		return err
	}
	_ = os.Remove(path)
	return nil
}

func terminatePID(pid int, executable string, timeout time.Duration) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find %s process %d: %w", executable, pid, err)
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal %s process %d: %w", executable, pid, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	err = waitForPIDExit(ctx, pid, timeout)
	cancel()
	if err == nil {
		return nil
	}
	if killErr := process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return fmt.Errorf("kill %s process %d: %w", executable, pid, killErr)
	}
	killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer killCancel()
	if err := waitForPIDExit(killCtx, pid, 5*time.Second); err != nil {
		return fmt.Errorf("%s process %d did not exit after SIGKILL: %w", executable, pid, err)
	}
	return nil
}

func waitForPIDExit(ctx context.Context, pid int, timeout time.Duration) error {
	return waitUntil(ctx, timeout, func() bool {
		process, err := os.FindProcess(pid)
		return err != nil || process.Signal(syscall.Signal(0)) != nil || processIsZombie(pid)
	})
}

func processIsZombie(pid int) bool {
	output, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "state=").Output()
	if err != nil {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(string(output)), "Z")
}

func requestVMStop(ctx context.Context, socketPath string) error {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	body, _ := json.Marshal(map[string]string{"state": "Stop"})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://krunkit/vm/state", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("krunkit stop returned HTTP %s", response.Status)
	}
	return nil
}

func networkSocket(state RuntimeState) string {
	return filepath.Join(state.RuntimeDir, "network.sock")
}

func krunkitSocket(state RuntimeState) string {
	return filepath.Join(state.RuntimeDir, "krunkit.sock")
}

func gvproxyPIDFile(state RuntimeState) string {
	return filepath.Join(state.RuntimeDir, "gvproxy.pid")
}

func krunkitPIDFile(state RuntimeState) string {
	return filepath.Join(state.RuntimeDir, "krunkit.pid")
}
