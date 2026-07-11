# Idleloom

> Weave idle machines into compute.

Idleloom turns an after-hours Apple Silicon Mac into a Kubernetes worker for
AI compute. It directly manages a Linux VM with krunkit, enrolls kubelet with
Kubernetes TLS bootstrap, and exposes the Apple GPU through Vulkan, Venus,
MoltenVK, and Metal.

The intended onboarding experience is one command:

```sh
idleloom init --kubeconfig ~/.kube/config
```

The administrator kubeconfig never enters the VM. The worker receives only
the cluster CA, API endpoint, and a short-lived bootstrap token. Idleloom
deletes both the token and the guest copy of its bootstrap identity after the
kubelet receives a client certificate.

## Current milestone

The repository provides:

- `idleloom init`, `status`, `start`, `stop`, and `delete`;
- direct krunkit and gvproxy lifecycle management without a VM orchestrator;
- an Ubuntu 24.04 ARM64 worker with persistent containerd and kubelet data;
- kubelet version matching against the target Kubernetes API server;
- checksum-verified Ubuntu image and kubelet downloads;
- short-lived bootstrap tokens and dedicated CSR approval RBAC;
- a host-side certificate maintainer for kubelet serving certificate rotation;
- WireKube compatibility checks and node enrollment;
- hostPath and iSCSI support in the worker base system;
- an Apple Vulkan DRA node driver and example ResourceClaims.

## How it works

The CLI runs on macOS and uses the operator kubeconfig only for enrollment and
lifecycle operations. It manages krunkit and gvproxy directly. Inside the
Ubuntu ARM64 VM, kubelet and containerd use a persistent data disk, WireKube
provides node connectivity, and the Apple Vulkan DRA driver publishes the
guest render device.

Each worker atomically reserves a private gvproxy subnet with a Kubernetes
Lease. Existing Node addresses, WireKube AllowedIPs, and other Idleloom
reservations are excluded before the VM starts. WireKube then advertises the
reserved guest InternalIP.

The Ubuntu root image is checksum-verified and copied per worker. Container
images, kubelet certificates, hostPath data, and runtime state live on a
separate sparse data disk. This keeps the root image simple and avoids a
`qemu-img` dependency.

## Requirements

- Apple Silicon Mac running macOS 14 or later;
- at least 4 GiB VM memory; 8 GiB or more is recommended for AI workloads;
- `krunkit` 1.3 or later and its `gvproxy` dependency in `PATH`;
- a reachable Kubernetes cluster with bootstrap token authentication enabled;
- a kubeconfig allowed to create bootstrap token Secrets and RBAC, approve kubelet serving CSRs, manage Idleloom network Leases, and update Nodes;
- WireKube installed in the target cluster.

Install the host runtime with Homebrew:

```sh
brew tap libkrun/krun
brew install krunkit
```

`WireKubeMesh/default` must advertise Node InternalIPs:

```yaml
spec:
  autoAllowedIPs:
    includeNodeInternalIP: true
```

Idleloom checks the mesh, agent DaemonSet, and ready ingress peers before it
creates a VM. The gvproxy backend intentionally supports WireKube networking
only: its guest address is private to the host and is not directly routable
from other cluster nodes.

## Build

```sh
make build
./bin/idleloom version
```

For development:

```sh
go test ./...
go vet ./...
```

## Join a worker

Interactive setup:

```sh
./bin/idleloom init --kubeconfig ~/.kube/config --context my-cluster
```

Non-interactive setup:

```sh
./bin/idleloom init \
  --yes \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --name studio-idle \
  --cpus 4 \
  --memory 8g \
  --disk 40g
```

Run read-only preflight checks without downloading an image, creating a token,
or starting a VM:

```sh
./bin/idleloom init --yes --dry-run --kubeconfig ~/.kube/config
```

The advanced `--runtime-dir` flag changes where VM disks, SSH keys, sockets,
and local logs are stored. The default is
`~/.idleloom/runtimes/<node-name>`.

## Worker lifecycle

```sh
idleloom status
idleloom stop
idleloom start
idleloom delete
```

`stop` refuses to shut down while non-DaemonSet workload Pods are active and
cordons the Node before its final workload check and VM shutdown. `start`
boots the existing disks, reuses the issued kubelet certificate, waits for a
fresh heartbeat, and then uncordons the Node. While the VM is running, a small
detached host process validates and approves serving certificate rotation
requests using the worker state and its Kubernetes network Lease.

If the control plane or kubeconfig is unavailable, local resources can still
be reclaimed explicitly:

```sh
idleloom stop --local-only
idleloom delete --local-only
```

Local-only deletion retains the small state file so a later normal `delete`
can remove the stale Kubernetes Node and network Lease after the cluster
recovers.

Enrollment intent, the network reservation identity, and the planned canonical
runtime path are written before VM creation with phase `enrolling`. If the CLI
or host stops during creation, `idleloom delete` can recover the Lease and find
the partially created runtime. Inspect later bootstrap failures with
`idleloom status` and the local logs under the runtime directory. `idleloom
delete` removes the Node, VM processes, disks, keys, runtime logs, and the
certificate maintainer. It refuses active workloads unless `--force` is
supplied.

## Worker capabilities

The Ubuntu base installs and configures:

- containerd with systemd cgroups;
- standard CNI plugins and Kubernetes networking sysctls;
- `open-iscsi` and `iscsid` for Longhorn-style volumes;
- NFS client utilities;
- a persistent filesystem backing `/var/lib/containerd` and `/var/lib/kubelet`;
- a krunkit Venus render device for GPU workload images with Mesa Vulkan userspace.

Ordinary Pod `hostPath` volumes use the persistent VM filesystem. Use paths
under `/var/lib/idleloom/volumes` when the data should live on the large worker
disk. These paths are inside the worker VM, not on macOS. Sharing a macOS
directory through virtio-fs is a separate future feature.

## Apple Vulkan backend

The DRA backend discovers a krunkit/Venus render node and publishes it through
Kubernetes Dynamic Resource Allocation:

- `/dev/dri/renderD*` discovery backed by `virtio_gpu`;
- Vulkan compute-shader health probes;
- node-local `ResourceSlice` publication;
- kubelet DRA v1 prepare/unprepare service;
- exclusive Claim leases and stable CDI device injection.

Build the development image and deploy it after choosing a driver domain you
control:

```sh
docker build -t idleloom-vulkan-dra:dev .
kubectl apply -k deploy/base
kubectl apply -f deploy/examples/deviceclass.yaml
```

The public manifests use the development driver name
`gpu.apple-vulkan.example`. Operators must replace it with a DNS name they
control before production deployment.

## Known limitations

- The worker backend currently supports Apple Silicon and krunkit only.
- WireKube installation is not embedded; Idleloom integrates with an existing deployment.
- The Ubuntu image is downloaded once and retained in the user cache.
- A newly joined Node needs a registry-pullable DRA image.
- Serving certificate rotation depends on the host-side maintainer and the operator kubeconfig remaining valid while the worker runs.
- Physical Apple GPU exclusivity still requires a future macOS host lease agent.

Idleloom is experimental software. Do not enroll untrusted hosts or run
untrusted GPU workloads until the security boundaries have been reviewed for
your environment.
