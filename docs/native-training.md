# Native MLX training

For the shorter manifest-first workflow shared with Linux workers, start with
[`recipes.md`](recipes.md). This guide remains the detailed Native-only
walkthrough for preparing MLX and testing both host link modes.

Idleloom Native currently runs training code as a `Shell` workload. There is
no separate training CRD, distributed trainer, dataset controller, or durable
checkpoint exporter yet. The example in this guide verifies that a Kubernetes
workload can execute MLX gradient updates on the joined Mac's Metal GPU.

The same computation works with both link modes. The difference is how logs
return to the operator:

| Link mode | Cluster-to-host path | Log command |
| --- | --- | --- |
| `api-only` | None; the agent polls the Kubernetes API | `./bin/idlectl logs --local` on the joined Mac |
| `wirekube` | WireKube relay link to the kubelet log bridge | Standard `./bin/idlectl logs`, including `--follow` |

## Prerequisites

- An Apple Silicon Mac.
- A kubeconfig allowed to install the Idleloom Native CRDs, RBAC, and
  connected-mode WireKube resources.
- Python 3.12 and MLX 0.32.0 in a host-readable virtual environment.

Build the complete Native bundle:

```sh
make build-idlectl
```

Create the demo environment outside the sandbox. The workload can read this
environment but cannot modify it:

```sh
python3 -m venv /var/tmp/idleloom-mlx
/var/tmp/idleloom-mlx/bin/python -m pip install \
  --disable-pip-version-check \
  'mlx==0.32.0'
```

The reusable workload script is
`examples/native/mlx-linear-regression.sh`. It trains two scalar parameters on
synthetic data, explicitly selects `mx.gpu`, saves a temporary checkpoint, and
prints its SHA-256 digest.

The script uses `python -c` rather than a shell heredoc. macOS `sandbox-exec`
may deny the temporary file that zsh creates for a heredoc even when the
Python process itself is allowed.

## API-only mode

Join without an inbound host link. Projection is unnecessary because logs are
read from local agent state:

```sh
./bin/idlectl join training-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --state-dir /var/tmp/idleloom-training-api-state \
  --root /var/tmp/idleloom-training-api-root \
  --link api-only \
  --shell-access sandboxed \
  --projection=false
```

If the kubeconfig disables TLS verification, add `--allow-tofu` after
verifying the observed API identity as described in the main README. Repeat
`--allow-tofu` on `delete host/...` when using that kubeconfig.

Create the training workload:

```sh
./bin/idlectl run mlx-train-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default \
  --shell "$(<examples/native/mlx-linear-regression.sh)" \
  --isolation sandbox \
  --network none \
  --memory 512Mi \
  --timeout 2m
```

Check until the phase is `Succeeded` or `Failed`:

```sh
./bin/idlectl get workload/mlx-train-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default
```

Read the completed snapshot on the joined Mac:

```sh
./bin/idlectl logs workload/mlx-train-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default \
  --local \
  --state-dir /var/tmp/idleloom-training-api-state
```

`--local` intentionally does not support `--follow`; it reads a consistent
snapshot from the local agent log. Use it after the workload has an assignment.

Clean up in workload-first order:

```sh
./bin/idlectl delete workload/mlx-train-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default

./bin/idlectl delete host/training-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --state-dir /var/tmp/idleloom-training-api-state
```

## WireKube mode

Join with the default relay link and Kubernetes log projection:

```sh
./bin/idlectl join training-wire \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --state-dir /var/tmp/idleloom-training-wire-state \
  --root /var/tmp/idleloom-training-wire-root \
  --link wirekube \
  --shell-access sandboxed \
  --projection=true
```

The join requests macOS administrator authorization to install the root link
service. The agent and training process continue to run as the regular login
user. If WireKube is missing, the same command downloads a compatible
checksum-verified `wirekubectl`, displays its installation plan, installs the
cluster dependency, and resumes enrollment. Add `--install-dependencies
--yes` when running this flow non-interactively.

Create the same workload:

```sh
./bin/idlectl run mlx-train-wire \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default \
  --shell "$(<examples/native/mlx-linear-regression.sh)" \
  --isolation sandbox \
  --network none \
  --memory 512Mi \
  --timeout 2m
```

Stream or retrieve logs through the Kubernetes API:

```sh
./bin/idlectl logs workload/mlx-train-wire \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default \
  --follow
```

After completion, the projected Pod remains addressable while the workload
exists, so the same command without `--follow` returns the retained log.

Clean up:

```sh
./bin/idlectl delete workload/mlx-train-wire \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  -n default

./bin/idlectl delete host/training-wire \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --state-dir /var/tmp/idleloom-training-wire-state
```

## Expected result

Both modes were exercised with the same script. The significant output is:

```text
step=000 loss=16.00586319
step=020 loss=0.00455355
step=040 loss=0.00000356
step=100 loss=0.00000000
device=Device(gpu, 0)
weight=3.000000 bias=2.000000
checkpoint=checkpoint.npz
```

`device=Device(gpu, 0)` confirms that MLX selected the Metal GPU rather than a
CPU device.

## Current checkpoint limitation

The assignment work directory is intentionally ephemeral and is removed after
the shell process completes. The example prints a checkpoint digest to prove
that the file was produced, but the checkpoint itself is not exported.

Real fine-tuning needs a future artifact-output contract with a destination,
credentials that are not embedded in the workload script, digest verification,
and cleanup semantics. Host-isolated shell commands can write to normal user
paths, but that bypasses the sandbox boundary and should not be treated as the
durable training API.
