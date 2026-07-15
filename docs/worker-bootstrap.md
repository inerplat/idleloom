# Worker bootstrap

Idleloom enrolls a remote worker with Kubernetes TLS bootstrap. It does not
copy an administrator certificate or the source kubeconfig into the worker VM.

For repository acquisition, CLI build, WireKube installation choices, Worker
join, Pod and storage examples, Vulkan DRA, and cleanup, start with
[`getting-started.md`](getting-started.md).

## Enrollment sequence

1. Read the selected cluster version, CA, DNS settings, and WireKube status.
2. Inspect Linux node bootstrap DaemonSets before creating local state.
3. Reserve the worker network and create the krunkit VM.
4. Create a short-lived bootstrap token and install the matching kubelet.
5. Wait for the kubelet client certificate and Node registration, label the
   Node, then verify and approve its serving certificate.
6. Wait for the WireKube peer and Node readiness, then delete
   the bootstrap token and guest bootstrap identity.

## Cluster permissions

The enrollment kubeconfig needs permission to:

- read the selected cluster version, DNS Service, and root CA ConfigMap;
- list DaemonSets to check whether a new external Linux node can bootstrap its
  CNI and node add-ons;
- read WireKube CRDs and DaemonSets when `--network=wirekube` is used;
- create and delete `bootstrap.kubernetes.io/token` Secrets;
- create or reconcile Idleloom bootstrap ClusterRoleBindings;
- create and delete `coordination.k8s.io` Leases used for network allocation;
- list and approve the enrolled node's `kubernetes.io/kubelet-serving` CSR;
- read, label, cordon, and uncordon Nodes;
- list Pods for safe worker shutdown.

Idleloom uses the bootstrap group
`system:bootstrappers:idleloom:default-node-token`.

It binds that group to the standard Kubernetes bootstrap and node-client CSR
approval roles. The token defaults to a 30-minute TTL and is deleted as soon
as enrollment succeeds. Failed enrollment also attempts token deletion.

The serving CSR validator binds the exact node name and guest IP SAN. It
accepts algorithm-appropriate usages: ECDSA and Ed25519 use digital signature
plus server authentication, while RSA additionally uses key encipherment.
Client authentication or other extra usages are rejected.

## External-node compatibility preflight

`idleloom init --dry-run` checks the same cluster prerequisites as a real
enrollment without creating a VM or token. It does not require `--yes`.

The preflight discovers `kube-dns`, `coredns`, or a Service labeled
`k8s-app=kube-dns`; it does not require the Service object to be named
`kube-dns`.

It also identifies CNI-installing DaemonSets by their host CNI paths. If an
image registry resolves only to private, loopback, or link-local addresses
from the Mac, preflight prints a warning. Host-side DNS alone cannot prove
whether a krunkit guest using host NAT, VPN routes, or private network routing
can reach the registry. Critical CNI warnings should therefore be verified in
the VM rather than treated as a structural incompatibility.

This check deliberately reports warnings instead of rewriting managed
provider manifests or mirroring images behind the operator's back.

## Persistent guest state

Idleloom copies a checksum-verified Ubuntu cloud image for the root filesystem
and attaches a separate sparse data disk. Durable worker state and large
hostPath volumes live under `/var/lib/idleloom`.

| Path | Purpose |
| --- | --- |
| `/var/lib/idleloom/bin/kubelet` | Version-matched kubelet binary |
| `/var/lib/idleloom/config` | CA, kubelet configuration, and installer |
| `/var/lib/idleloom/containerd` | Persistent backing for `/var/lib/containerd` |
| `/var/lib/idleloom/kubelet` | Persistent backing for `/var/lib/kubelet` |
| `/var/lib/idleloom/volumes` | Recommended persistent hostPath root |

The first join uses the bootstrap token. Later starts use the kubelet client
certificate stored under `/var/lib/kubelet`; no new bootstrap token is needed.
The bootstrap kubeconfig is removed from the guest after successful enrollment.

If `init` is interrupted after the kubelet has obtained its client certificate,
the state remains in phase `enrolling`. Fix the external cause, then run
`idleloom start` to resume Node labeling, serving certificate approval,
WireKube checks, and bootstrap identity removal. The default state file is
`~/.idleloom/state.json`. If `init` used `--state /absolute/path/state.json`,
pass that same path to `status`, `start`, `stop`, and `delete`. Use
`idleloom delete` with the matching state path when the partial worker should
be discarded instead. Runtime logs are under
`~/.idleloom/runtimes/<node-name>` by default; exact filenames are listed in
[`getting-started.md`](getting-started.md).

## Why WireKube is part of onboarding

TLS bootstrap only authenticates the worker. It does not make a private VM
address routable from the control plane or other nodes.

Idleloom gives each gvproxy-backed VM a stable private `/29` and guest address
derived from its node name. WireKube publishes that Node InternalIP as an
AllowedIP and installs an encrypted route
from the existing mesh peers. This makes kubelet port 10250 and node-level CNI
traffic reachable without opening inbound ports on the Mac.

The current integration contract is deliberately small:

1. `wirekube.io/v1alpha1` CRDs are installed.
2. `WireKubeMesh/default` exists.
3. `spec.autoAllowedIPs.includeNodeInternalIP` is true.
4. A WireKube agent DaemonSet and at least one ready peer exist.
5. Idleloom adds `wirekube.io/vpn-enabled=true` after Node registration.

WireKube owns the encrypted mesh and NAT traversal. Idleloom owns host
provisioning, Kubernetes identity, and worker lifecycle. Keeping this boundary
avoids vendoring WireKube manifests and lets both projects upgrade independently.

## Install WireKube before the worker

The Linux worker command does not install cluster dependencies. Install the
WireKube release required by this checkout before running `idleloom init`.
Native Metal `idlectl join` has a separate integrated dependency path.

On Apple Silicon macOS, download and verify WireKube v0.0.15:

```sh
version=v0.0.15
curl -fLO "https://github.com/inerplat/wirekube/releases/download/${version}/wirekubectl-darwin-arm64"
curl -fLO "https://github.com/inerplat/wirekube/releases/download/${version}/wirekubectl-checksums.txt"

expected="$(awk '$2 == "wirekubectl-darwin-arm64" { print $1 }' wirekubectl-checksums.txt)"
actual="$(shasum -a 256 wirekubectl-darwin-arm64 | awk '{ print $1 }')"
test -n "${expected}" && test "${actual}" = "${expected}"

chmod +x wirekubectl-darwin-arm64
sudo install wirekubectl-darwin-arm64 /usr/local/bin/wirekubectl
```

Run the ownership-aware easy installer. `internal-ip` advertising is required
because Idleloom waits for the worker Node address to become a WireKube
AllowedIP. The UDP relay Service is unnecessary unless the operator separately
supports external WireGuard peer invites:

```sh
wirekubectl install \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --node-addresses internal-ip \
  --relay-udp=false

wirekubectl status --kubeconfig ~/.kube/config --context my-cluster
wirekubectl doctor --kubeconfig ~/.kube/config --context my-cluster
```

The interactive plan must show a non-overlapping mesh CIDR, a reachable relay,
the privileged agent DaemonSet, and
`spec.autoAllowedIPs.includeNodeInternalIP=true`. A public TCP
`LoadBalancer` is the simplest ingress path. For a cluster without one, the
same v0.0.15 easy installer can create a TCP NodePort without a source
checkout. Replace the example address with a stable, publicly reachable node:

```sh
wirekubectl install \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --node-addresses internal-ip \
  --relay node-port \
  --relay-transport tcp \
  --relay-endpoint 203.0.113.10:30478 \
  --relay-udp=false

nc -vz 203.0.113.10 30478
```

WSS requires an operator-managed public hostname, certificate, and HTTPS
Gateway or Ingress. Follow the version-matched
[WireKube v0.0.15 relay guide](https://github.com/inerplat/wirekube/blob/v0.0.15/docs/guides/relay-entrypoints.md).

WireKube is a cluster-wide shared dependency. `idleloom delete` removes the
worker Node and its Idleloom Lease, but never uninstalls WireKube. Use
`wirekubectl uninstall` only after confirming that no other node or Idleloom
host depends on the installation.
