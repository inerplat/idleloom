# Native MLX training

This guide runs the same MLX training recipe through both Native Metal link
modes. The computation and scheduler are identical; only the return path for
logs changes.

| Link mode | Cluster-to-host path | Log command |
| --- | --- | --- |
| `api-only` | None; the agent polls the Kubernetes API | `idlectl logs --local` on the joined Mac after completion |
| `wirekube` | Restricted WireKube peer to the kubelet log bridge | Standard `idlectl logs`, including `--follow` |

Idleloom Native currently represents training as a `Shell`
`IdleloomWorkload`. There is no distributed trainer, dataset controller, or
durable checkpoint exporter yet. The recipe verifies that Kubernetes can
schedule MLX gradient updates onto the Mac's Metal GPU.

## Prerequisites

- An Apple Silicon Mac running macOS 14 or later.
- `kubectl` and a kubeconfig allowed to install the Idleloom Native CRDs and
  RBAC. Connected mode also needs permission to inspect or install WireKube.
- An `idlectl` build that supports `recipe list`. If the Homebrew binary says
  that `recipe` is unknown, follow the current-checkout build instructions in
  the main README before continuing.
- Python 3.12 and MLX 0.32.0 at the recipe's fixed host path.

Install and verify the local prerequisites:

```sh
brew install kubectl python@3.12
python312="$(brew --prefix python@3.12)/bin/python3.12"
"${python312}" -m venv /var/tmp/idleloom-mlx
/var/tmp/idleloom-mlx/bin/python -m pip install \
  --disable-pip-version-check \
  'mlx==0.32.0'

idlectl recipe list
kubectl --kubeconfig ~/.kube/config --context my-cluster cluster-info
```

The environment is outside the assignment work directory. A sandboxed
workload can read it but cannot modify it. The versioned recipe embeds the
training script, so the commands below do not require an
`examples/native/...` file from a source checkout.

If the kubeconfig disables TLS verification, add `--allow-tofu` to `join` only
after verifying the observed API identity as described in the main README.
Use the same flag when deleting that host.

## API-only mode

Join without an inbound host link. Projection is disabled because this mode
cannot expose a reachable kubelet log endpoint:

```sh
idlectl join training-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --state-dir /var/tmp/idleloom-training-api-state \
  --root /var/tmp/idleloom-training-api-root \
  --link api-only \
  --shell-access sandboxed \
  --projection=false
```

`idlectl get` must show `READY=True`. `CONNECTED=False` or `Unknown` is normal
for this mode:

```sh
idlectl get host/training-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster
```

Render and apply the embedded training workload:

```sh
idlectl recipe render train/mlx-linear-regression@v1 \
  --name mlx-train-api \
  -o yaml > mlx-train-api.yaml

kubectl --kubeconfig ~/.kube/config --context my-cluster \
  -n default apply --dry-run=client -f mlx-train-api.yaml
kubectl --kubeconfig ~/.kube/config --context my-cluster \
  -n default apply -f mlx-train-api.yaml
kubectl --kubeconfig ~/.kube/config --context my-cluster \
  -n default wait \
  --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/mlx-train-api \
  --timeout=3m
```

Read the completed snapshot from local agent state on the joined Mac:

```sh
idlectl logs workload/mlx-train-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default \
  --local \
  --state-dir /var/tmp/idleloom-training-api-state
```

`--local` intentionally does not support `--follow`; it reads a consistent
snapshot after the workload has an assignment.

Clean up in workload-first order:

```sh
idlectl delete workload/mlx-train-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default

idlectl delete host/training-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --state-dir /var/tmp/idleloom-training-api-state

rm -f mlx-train-api.yaml
```

If this host was joined with `--allow-tofu`, repeat the host command with that
flag so deletion uses the persisted certificate pin:

```sh
idlectl delete host/training-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --state-dir /var/tmp/idleloom-training-api-state \
  --allow-tofu
```

## WireKube mode

The default mode installs a root-owned route service and publishes Kubernetes
log projection:

```sh
idlectl join training-wire \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --state-dir /var/tmp/idleloom-training-wire-state \
  --root /var/tmp/idleloom-training-wire-root \
  --link wirekube \
  --shell-access sandboxed \
  --projection=true
```

The join requests macOS administrator authorization for the root link service.
The agent and training process remain regular-user processes. If WireKube is
missing, interactive join downloads the compatible, checksum-verified
`wirekubectl`, shows the cluster impact, installs the dependency, and resumes
enrollment. For non-interactive use, add `--install-dependencies --yes`.

The default installer uses a public TCP relay and does not request the optional
external-peer UDP listener. The cluster therefore needs a working public
`LoadBalancer`. An existing mesh with a reachable `wss://` control endpoint is
also supported. If neither path is available, use API-only mode instead.

Verify both host conditions before creating work:

```sh
idlectl get host/training-wire \
  --kubeconfig ~/.kube/config \
  --context my-cluster
```

The result must show both `READY=True` and `CONNECTED=True`.

Run the same embedded recipe and stream its projected logs:

```sh
idlectl recipe render train/mlx-linear-regression@v1 \
  --name mlx-train-wire \
  -o yaml > mlx-train-wire.yaml

kubectl --kubeconfig ~/.kube/config --context my-cluster \
  -n default apply --dry-run=client -f mlx-train-wire.yaml
kubectl --kubeconfig ~/.kube/config --context my-cluster \
  -n default apply -f mlx-train-wire.yaml

idlectl logs workload/mlx-train-wire \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default \
  --follow
```

After completion, the projected Pod remains addressable while the workload
exists, so the same log command without `--follow` returns the retained log.

Clean up:

```sh
idlectl delete workload/mlx-train-wire \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default

idlectl delete host/training-wire \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --state-dir /var/tmp/idleloom-training-wire-state

rm -f mlx-train-wire.yaml
```

If this host was joined with `--allow-tofu`, repeat the host command with that
flag:

```sh
idlectl delete host/training-wire \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --state-dir /var/tmp/idleloom-training-wire-state \
  --allow-tofu
```

Host deletion removes only the Idleloom host's restricted peer and local
services. It does not uninstall the shared WireKube deployment, mesh, relay,
or agents.

## Expected result

Both modes should report output containing:

```text
step=000 loss=16.00586319
step=020 loss=0.00455355
step=040 loss=0.00000356
step=100 loss=0.00000000
device=Device(gpu, 0)
weight=3.000000 bias=2.000000
checkpoint=checkpoint.npz
```

`device=Device(gpu, 0)` confirms that MLX selected the Metal GPU. The final
line printed by the recipe is the checkpoint SHA-256 digest.

## Current checkpoint limitation

The assignment work directory is intentionally ephemeral and is removed after
the shell process completes. The recipe prints a checkpoint digest to prove
that the file was produced, but it does not export the file.

Durable training needs an artifact-output contract with a destination,
credentials outside the workload script, digest verification, and explicit
cleanup semantics. A host-isolated shell can write to normal user paths, but
that bypasses the sandbox boundary and is not the durable training API.
