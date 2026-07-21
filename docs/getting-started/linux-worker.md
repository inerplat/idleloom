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
idlectl create worker evening-worker \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --cpus 4 \
  --memory 8g \
  --disk 40g \
  --dry-run

idlectl create worker evening-worker \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --cpus 4 \
  --memory 8g \
  --disk 40g

idlectl status
kube get node evening-worker -o wide
```

The Node must report `Ready` and carry labels `idleloom-worker=true` and
`idleloom-accelerator=apple-vulkan`.

## Managed-cluster deferred readiness

Some managed clusters require an operator to publish CNI images or place a
WireKube gateway after kubelet registration. Defer only the final readiness
waits in that case:

```sh
idlectl create worker evening-worker \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --wait=false

idlectl status
kube get node evening-worker -o wide
```

`--wait=false` still completes TLS bootstrap, serving certificate approval,
bootstrap identity removal, and Node registration. It records phase
`registered` and leaves the Node cordoned. After cluster-side networking is
ready, finish the strict readiness checks and uncordon the Node:

```sh
idlectl start worker --timeout 10m
idlectl status
```

Do not schedule workloads until the Node reports `Ready`.

## Private registries and credential providers

The worker VM pulls the cluster's system images (CNI, CSI, DNS) the moment it
registers. When those images live behind a registry the VM cannot reach or must
authenticate to, configure the pull path at `create worker` time. These flags
are advanced and optional.

### Redirect pulls to a reachable mirror

`--registry-mirror HOST=URL` (repeatable) writes a containerd `certs.d`
`hosts.toml` that redirects pulls for `HOST` to the mirror `URL`. Use it when a
registry resolves to a VPC-private address the worker cannot route to but a
public endpoint serves the same content. For example, an NKS cluster references
`nks.kr.private-ncr.ntruss.com` (private) whose public twin
`nks.kr.ncr.ntruss.com` allows anonymous pulls:

```sh
idlectl create worker evening-worker \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --registry-mirror nks.kr.private-ncr.ntruss.com=https://nks.kr.ncr.ntruss.com
```

The mirror URL must be `https` (plain `http` is accepted with a warning). `HOST`
must be a bare registry host, optionally with a port — no scheme or path.

### Authenticate with a kubelet credential provider

For registries that require dynamic credentials (for example an EKS-owned ECR
whose token rotates every few hours), inject a kubelet image credential
provider:

```sh
idlectl create worker evening-worker \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --credential-provider-bin ./ecr-credential-provider \
  --credential-provider-config ./credential-providers.yaml \
  --credential-provider-env-file ./aws.env
```

- `--credential-provider-bin` (repeatable) is the provider binary. It must be a
  **linux/arm64** build — the binary runs inside the worker VM, so a macOS build
  is rejected up front. `idlectl` validates the ELF architecture on your Mac
  without executing the binary.
- `--credential-provider-config` is a kubelet `CredentialProviderConfig`
  (`apiVersion: kubelet.config.k8s.io/v1`). Every provider `name` it lists must
  match a supplied binary's filename.
- `--credential-provider-env-file` is an optional `KEY=VALUE` file (for example
  `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`) exported to the provider. It is
  treated as a secret: installed `0600` inside the VM, never logged, and only its
  path — never its contents — is recorded in the worker state file.

An example `credential-providers.yaml` for ECR:

```yaml
apiVersion: kubelet.config.k8s.io/v1
kind: CredentialProviderConfig
providers:
  - name: ecr-credential-provider
    matchImages: ["*.dkr.ecr.*.amazonaws.com"]
    defaultCacheDuration: 12h
    apiVersion: credentialprovider.kubelet.k8s.io/v1
```

All of these settings are validated before any host or cluster change, so
`--dry-run` reports a bad binary or config immediately. They are also persisted
and reapplied automatically when a deferred worker is finished with
`idlectl start worker`.

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
