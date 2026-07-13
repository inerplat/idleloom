# Idleloom

> Weave idle machines into compute.

Idleloom turns an after-hours Apple Silicon Mac into Kubernetes compute. It
supports a full Linux worker for ordinary Pods and a Native Metal mode for
running explicitly authorized MLX or shell workloads directly on macOS.

## Choose a mode

| Mode | Kubernetes view | Workloads | Host runtime | CLI |
| --- | --- | --- | --- | --- |
| Linux worker | A real kubelet-managed Node | OCI containers, hostPath, iSCSI, and DRA workloads | Ubuntu VM managed with krunkit and containerd | `idleloom` |
| Native Metal alpha | Idleloom CRDs with optional ephemeral Node and Pod projection | Sandboxed or trusted shell commands and a locked MLX model | Regular macOS processes using Metal directly | `idlectl` |

Use the Linux worker when the workload expects normal Kubernetes Pod and
container semantics. Use Native Metal when direct Metal access matters more
than OCI compatibility and the workload fits Idleloom's restricted execution
contract.

Training, batch inference, and serving recipes provide one manifest-first
workflow across both modes. They render real Kubernetes resources or an
explicit `IdleloomWorkload`; the resulting YAML is applied and operated with
the native resource semantics of that backend. See
[`docs/recipes.md`](docs/recipes.md).

## Native Metal quick start

Install the complete macOS bundle and Kubernetes CLI from Homebrew:

```sh
brew install kubectl
brew install inerplat/tap/idleloom
idlectl version
kubectl version --client
kubectl cluster-info
```

This documentation follows the current repository checkout. Before joining a
host, verify that the installed release contains the manifest recipe surface:

```sh
idlectl recipe list
```

If that command reports `unknown command "recipe"`, the Homebrew release is
older than this checkout. Build the current checkout instead and keep its
bundle first on `PATH` for the rest of the guide:

```sh
brew install go
make build-idlectl
export PATH="$PWD/bin:$PATH"
idlectl recipe list
```

Join the current Kubernetes context and run a sandboxed command:

```sh
idlectl join studio-idle
idlectl run inventory --shell 'uname -a; sysctl -n hw.memsize'
idlectl logs -f workload/inventory
```

`idlectl join` installs a missing compatible WireKube release automatically
for the default connected link. The user does not need a WireKube checkout or
a preinstalled `wirekubectl`.

Native Metal has two link modes. They use the same scheduler and execute the
same workloads; the link changes how logs and control-plane-to-host traffic
return from the Mac.

| Link mode | Inbound path to the Mac | Administrator authorization | Logs |
| --- | --- | --- | --- |
| `wirekube` (default) | Restricted WireKube peer and root-owned route service | Required during join and deletion | Standard Kubernetes logs, including follow |
| `api-only` | None; the agent makes outbound Kubernetes API requests | Not required | Completed local snapshots with `idlectl logs --local` |

For API-only hosts, `--projection=false` is recommended because projected
Nodes and Pods cannot provide a reachable kubelet log endpoint without the
WireKube link.

Connected mode does not require a WireKube checkout or a preinstalled
`wirekubectl`. When the cluster has no compatible WireKube installation,
`idlectl join` downloads the pinned CLI release into the user cache, verifies
its checksum, shows the combined infrastructure plan, installs WireKube, and
continues the same host enrollment transaction.

Each execution mode starts with one command:

```sh
# Full Linux worker
idleloom init --kubeconfig ~/.kube/config

# Native Metal host; WireKube is the default link
idlectl join HOST --kubeconfig ~/.kube/config
```

The default connected path needs either a public TCP `LoadBalancer` that the
cluster can provision or an existing WireKube installation with a reachable
`wss://` control endpoint. `join` shows the infrastructure plan before making
changes. Use `--link api-only --projection=false` when the cluster cannot
provide an inbound relay path; workloads still run, but logs are read locally
after completion.

The administrator kubeconfig remains on the Mac. The Linux worker receives
only the cluster CA, API endpoint, and a short-lived bootstrap token. Native
Metal services receive restricted, short-lived service account credentials.

## Current milestone

The repository provides:

- `idleloom init`, `status`, `start`, `stop`, and `delete`;
- `idlectl join`, `run`, `recipe`, `get`, `logs`, and `delete`;
- direct krunkit and gvproxy lifecycle management without a VM orchestrator;
- an Ubuntu 24.04 ARM64 worker with persistent containerd and kubelet data;
- kubelet version matching against the target Kubernetes API server;
- checksum-verified Ubuntu image and kubelet downloads;
- short-lived bootstrap tokens and dedicated CSR approval RBAC;
- a host-side certificate maintainer for kubelet serving certificate rotation;
- checksum-verified WireKube dependency installation, compatibility checks, and node enrollment;
- hostPath and iSCSI support in the worker base system;
- an Apple Vulkan DRA node driver and example ResourceClaims;
- direct Native Metal execution with API-only and WireKube link modes;
- version-pinned Native and Worker training, batch inference, and serving recipes that render Kubernetes YAML.

## How the Linux worker works

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

## Linux worker requirements

- Apple Silicon Mac running macOS 14 or later;
- at least 4 GiB VM memory; 8 GiB or more is recommended for AI workloads;
- `krunkit` 1.3 or later and its `gvproxy` dependency in `PATH`;
- a reachable Kubernetes cluster with bootstrap token authentication enabled;
- a kubeconfig allowed to create bootstrap token Secrets and RBAC, approve kubelet serving CSRs, manage Idleloom network Leases, and update Nodes;
- WireKube installed in the target cluster.

Install the host runtime with Homebrew:

```sh
brew install go
brew tap libkrun/krun
brew install krunkit
```

The current Idleloom Homebrew formula installs the Native Metal bundle only.
Build `idleloom` from this checkout for the Linux worker path. WireKube must be
installed separately with its released easy installer before enrollment; see
[`docs/worker-bootstrap.md`](docs/worker-bootstrap.md).

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

## Build from source

```sh
make build
export PATH="$PWD/bin:$PATH"
idleloom version
idlectl version
```

For development:

```sh
go test ./...
go vet ./...
```

## Join a Linux worker

Interactive setup:

```sh
idleloom init --kubeconfig ~/.kube/config --context my-cluster
```

Non-interactive setup:

```sh
idleloom init \
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
idleloom init --yes --dry-run --kubeconfig ~/.kube/config
```

The advanced `--runtime-dir` flag changes where VM disks, SSH keys, sockets,
and local logs are stored. The default is
`~/.idleloom/runtimes/<node-name>`.

## Linux worker lifecycle

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

## Linux worker capabilities

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

The Vulkan recipes require Kubernetes 1.35 or later with the stable
`resource.k8s.io/v1` API enabled on the API server, scheduler, and kubelet.
Verify the API before building or deploying the driver:

```sh
kubectl version
kubectl api-resources --api-group=resource.k8s.io
```

The second command must list `DeviceClass`, `ResourceClaim`,
`ResourceClaimTemplate`, and `ResourceSlice`. A basic Linux worker can be used
for ordinary Pods on a cluster without DRA, but the Vulkan recipes cannot.

The development driver is not published as a ready-to-pull image. Install
Docker Desktop, choose a container registry reachable by the worker, and push
an ARM64 image before applying the DaemonSet:

```sh
brew install --cask docker
open -a Docker
docker info

export DRA_IMAGE=registry.example.com/your-project/idleloom-vulkan-dra:dev
docker login registry.example.com
docker buildx build --platform linux/arm64 --push -t "${DRA_IMAGE}" .

kubectl apply -k deploy/base
kubectl -n kube-system set image \
  daemonset/apple-vulkan-dra-node \
  dra-node="${DRA_IMAGE}"
kubectl -n kube-system rollout status \
  daemonset/apple-vulkan-dra-node \
  --timeout=5m
kubectl apply -f deploy/examples/deviceclass.yaml
```

Replace the example registry with one the cluster can authenticate to. If the
registry is private, configure the worker's image-pull credentials before the
rollout. Verify publication before using a Vulkan recipe:

```sh
kubectl get resourceslices -o wide
kubectl get deviceclass/apple-vulkan
```

The public manifests use the development driver name
`gpu.apple-vulkan.example`. Operators must replace it with a DNS name they
control before production deployment.

## Native Metal mode (alpha)

The Native Metal backend runs an explicitly authorized shell workload or a
locked MLX model directly on macOS. It does not present the Mac as a
general-purpose container host. Kubernetes execution state remains in
`IdleloomHost`, `IdleloomWorkload`, and `IdleloomWorkloadAssignment` resources.
The public `run` command currently creates shell workloads; curated model
workloads remain declarative Kubernetes resources in this alpha.

For a tested MLX training walkthrough in both link modes, see
[`docs/native-training.md`](docs/native-training.md).
For the shared manifest-first Native and Worker workflow, see
[`docs/recipes.md`](docs/recipes.md).

Homebrew installs the complete macOS bundle used by `join`:

```sh
brew install inerplat/tap/idleloom
```

Contributors can build the same bundle from source:

```sh
make build-idlectl
export PATH="$PWD/bin:$PATH"
```

The public CLI contains only Kubernetes-style resource operations:

```text
idlectl join HOST
idlectl run NAME
idlectl recipe (list | show | render)
idlectl get (hosts|workloads) [NAME]
idlectl logs workload/NAME
idlectl delete (host|workload)/NAME
idlectl version
```

Controller, agent, link, and projection processes are implementation
details. `join` installs and starts them with launchd; users do not run service
subcommands or keep terminal sessions open.

The agent runs workloads and reports host state as the regular login user. The
link is the smaller root service that maintains the WireKube tunnel and routes.
API-only mode needs the agent but does not install the link service.

The CLI accepts the CRD singular, plural, short, and API-qualified resource
names. For example, `workload`, `idleloomworkloads`, `ilw`, and
`idleloomworkloads.ai.idleloom.io` all address `IdleloomWorkload` resources.
The same forms are available for `IdleloomHost` through `host`,
`idleloomhosts`, and `ilh`. Hosts are exposed as logical cluster-wide
resources, so host `get` and `delete` reject namespace flags.

### Join a Mac

Native Metal requires an Apple Silicon Mac and a kubeconfig allowed to install
the Native CRDs, RBAC, and connected-mode cluster dependencies. It does not
require krunkit, the Ubuntu worker VM, a WireKube source checkout, or a
preinstalled `wirekubectl`.

Join installs the Native API and restricted RBAC, enrolls the host, creates
short-lived service credentials, and installs user LaunchAgents. In the
default WireKube mode it also installs the root link LaunchDaemon and
waits until the host is ready and connected:

```sh
idlectl join studio-idle \
  --kubeconfig ~/.kube/config \
  --context my-cluster
```

The default WireKube join asks for macOS administrator authorization once when
it installs the root-owned link helper. API-only mode does not install
that helper and does not require administrator authorization. Neither mode
runs the agent or shell workloads as root. The link is registered as a normal
`WireKubePeer`, not as an external peer. Its peer-specific ServiceAccount can
read mesh peers, refresh only its own short-lived credentials, and update only
its own peer status. The WireKube private key and restricted kubeconfig used by
the helper are copied into a root-owned state directory; the long-running
privileged service does not trust the user-writable enrollment directory for
tunnel configuration.

Hosts enrolled by an earlier connected-mode build as external peers must be
deleted and joined once with the new bundle. The delete path retains legacy
cleanup support, but the link service does not continue running the old peer
contract.

If WireKube is missing, interactive join first displays the detected cluster,
compatible WireKube version and image digest, selected mesh CIDR, privileged
DaemonSet requirement, and the public TCP relay LoadBalancer needed for the
connected host. Idleloom does not request the optional public UDP relay used by
external peers. When the mesh exposes a `wss://` control endpoint, the same
peer client uses WSS with an audience-scoped, short-lived token instead; WSS is
recommended for internet-facing relays. One confirmation approves both
dependency installation and the host join. The successful WireKube
installation remains available for other hosts if a later Idleloom enrollment
step fails.

If the plan selects a public TCP relay, confirm that the cluster has a working
`LoadBalancer` controller and that the assigned address is reachable from the
Mac. If the cluster cannot provide one, cancel the plan and use API-only mode,
or have the cluster operator configure WireKube WSS first. A WSS endpoint,
certificate, Gateway, or Ingress is not created by Idleloom.

For non-interactive onboarding, explicitly authorize dependency installation:

```sh
idlectl join studio-idle \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --install-dependencies \
  --yes
```

If the source kubeconfig disables TLS verification, explicitly pin the API
certificate observed during the first connection:

```sh
idlectl join studio-idle \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --allow-tofu
```

The pin is stored as an SPKI SHA-256 value in `cluster-trust.json` under the
selected state directory. Verify it through a trusted channel before relying
on the connection. A later mismatch is rejected; after separately verifying a
legitimate API certificate rotation, repeat the command with both
`--allow-tofu` and `--reset-trust`.

Use `--link api-only` when inbound cluster-to-host access is not required.
API-only hosts can execute work but do not provide Kubernetes log streaming.
Run `idlectl logs --local workload/NAME` on the joined Mac to read a
completed log snapshot from agent state. Use `--projection=false` to disable
ephemeral Node and Pod projection.

`--shell-access` is an immutable enrollment boundary. The default,
`sandboxed`, accepts Seatbelt-restricted shell workloads. `disabled` exposes no
shell runtime. `host` executes trusted commands with the full access of the
regular macOS login user. Changing this boundary requires deleting and joining
the host again.

Sandbox mode denies the user home, `/Users`, `/Volumes`, `/Network`, the
Idleloom state directory, and the agent kubeconfig, and permits writes only in
the assignment work directory. It is a constrained execution boundary, not a
complete macOS confidentiality boundary; other system-readable paths may
remain visible.

Clusters using a non-default API server kubelet client certificate subject can
set exact comma-separated allowlists with `--kubelet-client-cn` and
`--kubelet-client-organization` during `join`.

### Run and observe work

Commands use the namespace from the selected kubeconfig context unless `-n`
is supplied:

```sh
idlectl get host/studio-idle \
  --kubeconfig ~/.kube/config \
  --context my-cluster

idlectl run inventory \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default \
  --shell 'uname -a; sysctl -n hw.memsize' \
  --isolation sandbox \
  --network none

idlectl get workloads -A \
  --kubeconfig ~/.kube/config \
  --context my-cluster

idlectl logs -f workload/inventory \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default
```

Check the installed binary without contacting a cluster:

```sh
idlectl version
```

After `join`, `idlectl get host/studio-idle` must show `READY=True`. The
default WireKube mode must also show `CONNECTED=True`; API-only mode reports
`CONNECTED=False` or `Unknown` by design. While a projected workload is
running, inspect its observability-only Node and Pod with:

```sh
kubectl get nodes,pods -A -l native.idleloom.io/projection=true
```

Projected Nodes are unschedulable, carry dedicated `NoSchedule` and
`NoExecute` taints, and expose one synthetic execution slot. The projection
publishes a reachable kubelet log endpoint only after an API-server-mediated
probe succeeds. It does not support arbitrary Pod submission, OCI container
execution, Pod networking, Services, volumes, probes, `exec`, attach, or port
forwarding.

### Delete resources

Delete a workload with Kubernetes resource syntax:

```sh
idlectl delete workload/inventory \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default
```

Delete every workload that references the host, wait for its assignment to be
removed, and then delete the joined Mac:

```sh
idlectl delete host/studio-idle \
  --kubeconfig ~/.kube/config \
  --context my-cluster
```

Host deletion stops and removes the launchd services, revokes the WireKube
peer with UID and key ownership checks, deletes the host namespace, and then
removes local credentials and private state. Removing the root link
helper requires macOS administrator authorization. Deletion fails closed while
a workload or assignment still references the host. Before any local or root
cleanup, the selected cluster namespace must match the enrollment nonce stored
in local state, which prevents deleting a same-named host from another context.

If host deletion says an assignment is still being cleaned up, inspect it and
retry after it disappears:

```sh
kubectl get idleloomworkloadassignments -A
```

Deleting an Idleloom host never uninstalls the shared WireKube deployment,
relay Services, mesh, or agents. Those resources may serve other nodes and are
owned by the cluster operator. A dependency installation that succeeded before
a later host-enrollment failure is intentionally retained; remove it only with
WireKube's own ownership-aware lifecycle command after confirming that no
other peer depends on it.

Joining over an existing local installation is rejected. Delete the current
host first, then join it again when changing immutable enrollment settings.
Service installation keeps a private receipt so a failed fresh installation
can roll back its partial services. There is no offline root-helper removal
command in this alpha; if the cluster is unavailable, restore access and run
`delete host/...` rather than deleting launchd files by hand.

## Known limitations

- The worker backend currently supports Apple Silicon and krunkit only.
- Linux workers still require a compatible WireKube deployment before `idleloom init`; Native Metal `idlectl join` installs a missing compatible release automatically.
- The Ubuntu image is downloaded once and retained in the user cache.
- A newly joined Node needs a registry-pullable DRA image.
- Serving certificate rotation depends on the host-side maintainer and the operator kubeconfig remaining valid while the worker runs.
- Physical Apple GPU exclusivity still requires a future macOS host lease agent.
- Native Metal projection creates Kubernetes API objects but is not a kubelet or a general-purpose Fargate implementation.
- Native Metal connected leaf is relay-only; its kubelet bridge supports logs but not exec, attach, or port forwarding.

Idleloom is experimental software. Do not enroll untrusted hosts or run
untrusted GPU workloads until the security boundaries have been reviewed for
your environment.

## License

Idleloom is licensed under the Apache License 2.0. See [`LICENSE`](LICENSE).
