# Native MLX training runs

Idleloom treats every Native training workload as an immutable run. The
`IdleloomWorkload` name is the run identity, `spec.run.experiment` groups
related runs, and `spec.run.attempt` records retries without mutating history.
The controller does not create a separate experiment or run CR.

This follows two useful conventions from workflow and experiment systems:

- definitions and executions are separate: a versioned recipe is a template,
  while each rendered `IdleloomWorkload` is one execution;
- parameters, timestamps, latest metrics, and artifact references remain on
  the run so completed executions can be searched and compared.

The Native backend is intentionally smaller than a distributed training
platform. It runs one Python program with the locked MLX runtime on one joined
Apple Silicon host.

For installation, connected-mode setup, and the complete Native and Worker
smoke-test sequence, start with [`getting-started.md`](getting-started.md).

## Link modes

Training and scheduling are identical in both Native link modes. The link
changes only how logs return from the Mac.

| Link mode | Cluster-to-host path | Logs |
| --- | --- | --- |
| `wirekube` | Restricted WireKube peer to the kubelet log bridge | `idlectl logs`, including `--follow` |
| `api-only` | No inbound path; the agent polls the Kubernetes API | `idlectl logs --local` on the joined Mac |

Use `--projection=false` with API-only mode because a projected kubelet log
endpoint cannot be reached without an inbound link.

## Prerequisites

- Apple Silicon Mac running macOS 26 or later for the locked MLX 0.32 wheels;
- `kubectl` and a kubeconfig allowed to install the Native API and RBAC;
- current `idlectl` binary with the `Train` workload schema;
- Python 3.12 for creating the sealed MLX environment on first use;
- outbound HTTPS from the Mac for first-use download of the locked MLX wheels.

MLX does not need to be installed at a separate host path. The agent uses
Python 3.12 to create and verify its sealed runtime under the selected `--root`
directory on first use. Training preparation does not download the locked
inference model.

```sh
brew install kubectl python@3.12
idlectl version
idlectl recipe list
kubectl --kubeconfig ~/.kube/config --context my-cluster cluster-info
```

## Run the tracked example

Join a connected host:

```sh
idlectl join training-host \
  --kubeconfig ~/.kube/config \
  --context my-cluster
```

Render ordinary Kubernetes YAML, review it, then apply it:

```sh
idlectl recipe render train/mlx-linear-regression@v1 \
  --name linear-a1 \
  -o yaml > linear-a1.yaml

kubectl --kubeconfig ~/.kube/config --context my-cluster \
  apply --dry-run=client -f linear-a1.yaml
kubectl --kubeconfig ~/.kube/config --context my-cluster \
  apply -f linear-a1.yaml
```

The first run may spend several minutes in `Starting` while the runtime is
downloaded and sealed. Preparation progress is part of the workload log and
renews the assignment lease.

Watch the run:

```sh
idlectl get workload/linear-a1 \
  --kubeconfig ~/.kube/config \
  --context my-cluster

idlectl logs -f workload/linear-a1 \
  --kubeconfig ~/.kube/config \
  --context my-cluster

kubectl --kubeconfig ~/.kube/config --context my-cluster \
  wait --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/linear-a1 \
  --timeout=10m
```

The table output includes task, experiment, attempt, phase, metric count,
artifact count, and duration. The complete run record is available as YAML:

```sh
idlectl get workload/linear-a1 -o yaml \
  --kubeconfig ~/.kube/config \
  --context my-cluster
```

A successful example reports the Metal device, converged parameters, a latest
`loss` metric, and a `checkpoint` artifact reference.

## Recipe parameters

Inspect defaults before rendering:

```sh
idlectl recipe show train/mlx-linear-regression@v1
```

Override them with a values file:

```yaml
namespace: default
experiment: linear-regression
attempt: 1
learningRate: 0.05
steps: 500
timeoutSeconds: 600
unifiedMemory: 1Gi
```

```sh
idlectl recipe render train/mlx-linear-regression@v1 \
  --name linear-long-a1 \
  --values values.yaml \
  -o yaml > linear-long-a1.yaml
```

Recipe parameters are validated before a manifest is rendered. The normalized
input and recipe contents are recorded as separate SHA-256 annotations.

## Write a training manifest

The manifest-first API is not limited to the bundled example. A custom run can
be written directly:

```yaml
apiVersion: ai.idleloom.io/v1alpha1
kind: IdleloomWorkload
metadata:
  name: custom-train-a1
  namespace: default
  labels:
    app.kubernetes.io/managed-by: idleloom
    ai.idleloom.io/run: custom-train-a1
    ai.idleloom.io/task: train
    ai.idleloom.io/experiment: custom-train
    ai.idleloom.io/attempt: "1"
spec:
  mode: Train
  run:
    task: train
    experiment: custom-train
    attempt: 1
    parameters:
      LEARNING_RATE: "0.0001"
      EPOCHS: "3"
  train:
    runtimeProfile: mlx-train-v1
    network: Outbound
    timeoutSeconds: 3600
    source:
      inline: |
        import json
        import os
        import mlx.core as mx

        epochs = int(os.environ["EPOCHS"])
        learning_rate = float(os.environ["LEARNING_RATE"])
        print(f"device={mx.default_device()}")
        for epoch in range(epochs):
            loss = 1.0 / (epoch + 1)
            print("::idleloom-metric::" + json.dumps({
                "name": "loss",
                "value": loss,
                "step": epoch,
            }))
        print(f"learning_rate={learning_rate}")
  resources:
    unifiedMemoryRequest: 2Gi
```

`spec.run.parameters` becomes environment variables. Parameter names must use
uppercase letters, digits, and underscores, and cannot use the reserved
`IDLELOOM_` prefix. Values are recorded in Kubernetes and must never contain
passwords, API tokens, private keys, or other secrets.

`network: None` denies network access inside the macOS sandbox. `Outbound`
allows the program to fetch public datasets or upload results. Neither mode
provides inbound sockets.

## Metrics protocol

A training program reports a metric by writing one JSON record on a line:

```text
::idleloom-metric::{"name":"loss","value":0.0125,"step":40}
```

Rules:

- metric names are Kubernetes DNS labels;
- values must be finite JSON numbers;
- steps are non-negative integers;
- status keeps the latest value for at most 32 distinct metric names;
- logs retain the full metric stream while the assignment owns the host
  mailbox rather than growing CR status without a bound.

Malformed protocol records fail the run. This prevents a successful training
phase from hiding an invalid metric or artifact declaration.

## Artifact protocol

An artifact reference uses a separate line:

```text
::idleloom-artifact::{"name":"checkpoint","uri":"s3://models/run/checkpoint.safetensors","digest":"sha256:...","sizeBytes":12345}
```

The status stores at most 16 artifacts by name. Each reference requires an
absolute URI and a lowercase SHA-256 digest. URIs cannot contain embedded
credentials, query strings, or fragments; presigned URLs therefore must not be
reported as artifact references.

For a host-local file, emit a `file://` URI. The agent accepts it only when the
resolved regular file is inside the current assignment work directory and its
size and digest match. Local files are an alpha cache for inspection and quick
same-host experiments, not a durable artifact store. The host retains at most
the nine previous training work directories before starting the next run.
Finish writing the file and atomically rename it into place before emitting the
artifact record; files that change while the agent hashes them are rejected.

For durable checkpoints, upload to an external artifact store in the training
program and emit the resulting credential-free URI. Keep credentials outside
the workload object; Native secret injection is not implemented yet.

## Retry and resume

A retry is a new immutable workload, not a mutation of the failed object.
Keep the experiment label, increment `attempt`, and choose a new Kubernetes
name:

```yaml
metadata:
  name: custom-train-a2
spec:
  run:
    task: train
    experiment: custom-train
    attempt: 2
    parameters:
      RESUME_URI: s3://models/custom-train/a1/checkpoint.safetensors
```

The program decides how to consume `RESUME_URI` and must verify the expected
checkpoint digest. Idleloom records the run relationship but does not silently
repeat failed work or choose a checkpoint on the user's behalf.

Search an experiment with native Kubernetes labels:

```sh
kubectl get idleloomworkloads.ai.idleloom.io \
  -l ai.idleloom.io/experiment=custom-train \
  -o custom-columns='NAME:.metadata.name,ATTEMPT:.spec.run.attempt,PHASE:.status.phase,LOSS:.status.run.metrics[0].value'
```

## API-only logging

API-only is a separate enrollment scenario. Delete the connected test host or
use a cluster with no other eligible Native host so the workload cannot be
assigned elsewhere. Then join without an inbound path:

```sh
idlectl join training-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --link api-only \
  --projection=false
```

Render and apply a workload with the same name used by the local log command:

```sh
idlectl recipe render train/mlx-linear-regression@v1 \
  --name linear-api \
  -o yaml > linear-api.yaml

kubectl --kubeconfig ~/.kube/config --context my-cluster \
  apply -f linear-api.yaml
kubectl --kubeconfig ~/.kube/config --context my-cluster \
  wait --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/linear-api \
  --timeout=10m
```

After completion, read the local snapshot on the joined Mac:

```sh
idlectl logs workload/linear-api \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --local
```

Local snapshots do not support `--follow`.

```sh
kubectl --kubeconfig ~/.kube/config --context my-cluster \
  delete -f linear-api.yaml
rm -f linear-api.yaml
```

## Cleanup

Delete workloads before deleting the host:

```sh
idlectl delete workload/linear-a1 \
  --kubeconfig ~/.kube/config \
  --context my-cluster

idlectl delete host/training-host \
  --kubeconfig ~/.kube/config \
  --context my-cluster
```

A completed assignment remains available for logs until another run needs the
same host mailbox. At that point the controller archives the completed status,
reclaims only the verified terminal assignment, and schedules the queued run.

## Current limits

- one Native run executes at a time per host;
- training source is one inline Python program, limited to 64 KiB;
- there is no distributed trainer, automatic retry policy, dataset CR,
  credential injection, or built-in object-store uploader;
- CR status contains bounded summaries, not full metric history;
- projected Pods support logs only; `exec`, attach, and port forwarding return
  an explicit unsupported response.
