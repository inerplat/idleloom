# Worker bootstrap

Idleloom enrolls a remote worker with Kubernetes TLS bootstrap. It does not
copy an administrator certificate or the source kubeconfig into the worker VM.

## Enrollment sequence

1. Read the selected cluster version, CA, DNS settings, and WireKube status.
2. Reserve the worker network and create the krunkit VM.
3. Create a short-lived bootstrap token and install the matching kubelet.
4. Wait for the kubelet client certificate and Node registration.
5. Label the Node, wait for the WireKube peer and Node readiness, then delete
   the bootstrap token and guest bootstrap identity.

## Cluster permissions

The enrollment kubeconfig needs permission to:

- read the selected cluster version, DNS Service, and root CA ConfigMap;
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
`LoadBalancer` is the simplest ingress path. Clusters that cannot provision one
need a preconfigured WSS, NodePort, or external relay topology from the
[WireKube relay guide](https://inerplat.github.io/wirekube/guides/relay-entrypoints/).

WireKube is a cluster-wide shared dependency. `idleloom delete` removes the
worker Node and its Idleloom Lease, but never uninstalls WireKube. Use
`wirekubectl uninstall` only after confirming that no other node or Idleloom
host depends on the installation.
