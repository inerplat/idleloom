# Linux Worker Quick Start

This path starts a krunkit VM and joins it as a real Kubernetes kubelet Node.
Complete [Start Here](index.md) first.

## Install host dependencies

```sh
brew tap libkrun/krun
brew install krunkit
brew install inerplat/tap/wirekube
brew install inerplat/tap/idleloom

idlectl version
wirekubectl version
```

The worker commands ship with the brew-installed `idlectl`. Only the
development Vulkan driver (`bin/idleloom-vulkan-dra`) still builds from source
with `make build` in `${IDLELOOM_REPO}`; that build also produces a
from-source `bin/idlectl`.

## Install or verify WireKube

Worker nodes behind NAT need the cluster-wide WireKube mesh before enrollment.
Preview the infrastructure plan first:

```sh
wirekubectl install \
  --node-addresses internal-ip \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --dry-run
```

Then approve the selected topology interactively, or reuse an existing
installation:

```sh
wirekubectl install \
  --node-addresses internal-ip \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"

wirekubectl doctor \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"
```

The mesh must advertise Node InternalIPs:

```sh
kube get wirekubemesh default \
  -o jsonpath='{.spec.autoAllowedIPs.includeNodeInternalIP}{"\n"}'
```

The result must be `true`. An existing installation that reports `false` was
installed with the default `mesh-only` exposure; fix it in place with
`wirekubectl upgrade --node-addresses internal-ip` against the same context.

## Preview and join the Worker

```sh
idlectl worker init \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --name evening-worker \
  --cpus 4 \
  --memory 8g \
  --disk 40g \
  --dry-run

idlectl worker init \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --name evening-worker \
  --cpus 4 \
  --memory 8g \
  --disk 40g

idlectl worker status
kube get node evening-worker -o wide
```

The Node must report `Ready` and carry labels `idleloom-worker=true` and
`idleloom-accelerator=apple-vulkan`.

## Managed-cluster deferred readiness

Some managed clusters require an operator to publish CNI images or place a
WireKube gateway after kubelet registration. Defer only the final readiness
waits in that case:

```sh
idlectl worker init \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --name evening-worker \
  --wait=false

idlectl worker status
kube get node evening-worker -o wide
```

`--wait=false` still completes TLS bootstrap, serving certificate approval,
bootstrap identity removal, and Node registration. It records phase
`registered` and leaves the Node cordoned. After cluster-side networking is
ready, finish the strict readiness checks and uncordon the Node:

```sh
idlectl worker start --timeout 10m
idlectl worker status
```

Do not schedule workloads until the Node reports `Ready`.

## Run an ordinary Pod

```sh
kube apply -f "${IDLELOOM_REPO}/examples/worker/toolbox-pod.yaml"
kube wait --for=condition=Ready pod/idleloom-worker-toolbox --timeout=5m
kube logs pod/idleloom-worker-toolbox
kube exec pod/idleloom-worker-toolbox -- uname -m
kube delete pod/idleloom-worker-toolbox
```

## Next steps

- [Worker storage](../guides/worker-storage.md)
- [Worker Vulkan](../guides/worker-vulkan.md)
- [Lifecycle and recovery](../operations/lifecycle.md)
- [Worker bootstrap contract](../worker-bootstrap.md)
