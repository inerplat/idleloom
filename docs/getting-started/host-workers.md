# Linux and Windows Hosts

A Linux Worker is a real kubelet Node running Ubuntu. Idleloom can produce one three ways, and the difference is only how that Linux environment comes to exist.

| Host | Backend | Linux environment | Node architecture |
| --- | --- | --- | --- |
| macOS on Apple Silicon | `krunkit` | A VM Idleloom builds and owns | linux/arm64 |
| Linux | `linux` | The host itself | the host's |
| Windows | `wsl2` | A WSL2 distribution | linux/amd64 |

`idlectl` picks the backend from the host it runs on; there is no flag. The node's `idleloom-runtime` label records which one enrolled it.

The macOS path is covered by the [Linux Worker quick start](linux-worker.md). This page covers the other two, which Idleloom calls *in-place* backends because the Linux environment already exists.

## What in-place enrollment changes

There is no disk image to download, no cloud-init seed, and no SSH hop. Idleloom installs containerd and kubelet into the environment directly and registers it. `--cpus`, `--memory`, and `--disk` are ignored: the environment is sized by the host, or by `.wslconfig` on Windows.

Deleting an in-place worker removes kubelet, its configuration, and the node address. It does not remove the host.

!!! warning "The host becomes a Kubernetes node"
    `idlectl create worker` installs system services and writes to `/var/lib/kubelet` and `/etc/kubernetes`. Do not point it at a machine that is already a node in another cluster.

## Node addressing

An in-place worker shares a network stack Idleloom does not own, so it cannot be given a private subnet the way a krunkit VM is. Its node IP comes from the WireKube mesh instead, and a systemd unit holds that address on a dummy link so kubelet can take it before the WireKube agent exists.

Two node names can reduce to the same mesh address. Whether enrollment refuses or picks another address depends on the mesh, and `--dry-run` tells you which you are about to get. [Mesh Addressing](../mesh-addressing.md) covers all of it.

## Linux host

```sh
sudo idlectl create worker evening-linux \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --dry-run

sudo idlectl create worker evening-linux \
  --kubeconfig ~/.kube/config \
  --context my-cluster
```

Enrollment installs system services, so run it as root or through `sudo`.

`--dry-run` checks the host before anything is changed:

- Linux running systemd
- Ubuntu, or a distribution declaring `ID_LIKE=debian` — the worker's containerd and CNI plugins are installed with `apt-get`
- the unified cgroup v2 hierarchy at `/sys/fs/cgroup/cgroup.controllers`
- a kernel that can create a `dummy` link
- the mesh address the worker would take

It also reads the cluster, so it fails there if the Kubernetes API is unreachable. It does not check that `dl.k8s.io` is reachable; the kubelet download happens after the dry run.

## Windows host

The worker runs inside a WSL2 distribution. WSL2 is itself a Hyper-V virtual machine, but Windows owns its lifecycle, so Idleloom enrolls it rather than building it.

Configure `%UserProfile%\.wslconfig` **before** installing the distribution:

```ini
[wsl2]
kernelCommandLine=cgroup_no_v1=all
dnsTunneling=false
networkingMode=mirrored
```

!!! danger "`.wslconfig` applies to every WSL distribution on the machine"
    These settings are machine-wide, and applying them requires `wsl --shutdown`, which stops every running distribution, including any used by Docker Desktop. Review the file before changing it.

`cgroup_no_v1=all` is required. Without it the hierarchy stays hybrid, Cilium's socket load balancer cannot attach, and every Pod-to-ClusterIP call times out with nothing pointing at the cause.

`dnsTunneling=false` avoids a related trap. Tunnelled DNS binds `10.255.255.254/32` to loopback, which Cilium then picks as a NodePort address and uses to SNAT service traffic to somewhere unroutable.

`networkingMode=mirrored` is optional. It gives WSL2 the host's address, so WireKube's STUN sees the router's mapping instead of a Windows NAT layer on top of it. Without it the worker still connects, because WireKube keeps its relay warm and falls back to it, but a direct peer-to-peer path is less likely.

Then install the distribution and enable systemd inside it:

```powershell
wsl --install -d Ubuntu-24.04
```

```sh
sudo tee /etc/wsl.conf <<'WSLCONF'
[boot]
systemd=true
command="mount --make-shared / && mkdir -p /var/run/netns && mount --bind /var/run/netns /var/run/netns && mount --make-shared /var/run/netns"
WSLCONF
```

The shared mount propagation is not optional: without it CNI Pods fail with `path "/var/run/netns" is mounted on "/" but it is not a shared or slave mount`.

Run `wsl --shutdown`, reopen the distribution, then enroll from Windows:

```powershell
idlectl create worker evening-windows --kubeconfig $HOME\.kube\config --context my-cluster --distribution Ubuntu-24.04 --dry-run
```

`--distribution` defaults to the machine's default distribution and is recorded in the worker state, so later lifecycle commands act on the same one.

### GPUs

A WSL2 worker sees an NVIDIA GPU through `/dev/dxg` rather than `/dev/nvidia*`, because WSL2 uses the Windows host driver rather than one installed in the guest. Install `nvidia-container-toolkit` inside the distribution, generate a CDI spec, and deploy the NVIDIA GPU Operator with `driver.enabled=false` and `toolkit.enabled=false`. WSL2 has no PCI bus, so Node Feature Discovery cannot detect the GPU; label the node manually with `feature.node.kubernetes.io/pci-10de.present=true`.

Idleloom claims no accelerator on an in-place worker. Devices belong to the cluster's own device plugins. Only the krunkit backend carries the `idleloom-accelerator` label, for the Apple Vulkan DRA driver.
