# Reproducible training and inference recipes

Idleloom recipes produce ordinary Kubernetes YAML for one of two execution
contracts. A recipe is a versioned manifest bundle in this repository, not a
cluster API or another custom resource.

| Backend | Rendered resource | Execution contract | Best fit |
| --- | --- | --- | --- |
| Native Metal | `IdleloomWorkload` | A restricted macOS process with direct Metal access | MLX and trusted host tools |
| Linux worker | Standard resources such as `Job` and `ResourceClaim` | A normal OCI container managed by kubelet and containerd | Portable training code, Pod networking, volumes, DRA, and standard Kubernetes tooling |

Both backends use the same recipe discovery and rendering commands. The
rendered YAML is the durable interface: it can be reviewed, stored, changed
under source control, and applied with `kubectl` without a separate Idleloom
run service.

## Discover recipes

List the immutable recipe versions included in the current `idlectl` binary:

```sh
idlectl recipe list
```

Inspect prerequisites, defaults, and accepted parameters before rendering:

```sh
idlectl recipe show train/mlx-linear-regression@v1
idlectl recipe show train/container-linear-regression@v1
idlectl recipe show infer/mlx-batch@v1
idlectl recipe show infer/llama-vulkan@v1
```

The version suffix is required. A manifest always records the exact recipe ID
and separate SHA-256 digests for the embedded recipe content and normalized
render inputs. `recipe show` includes a complete example values object.

Rendering without `--values` uses the embedded defaults. To override them,
pass a user-owned YAML file or pipe YAML through `--values -`; no source
checkout is required.

## Native Metal training

This recipe trains a two-parameter model with MLX on a joined Native Metal
host. It expects MLX 0.32.0 at `/var/tmp/idleloom-mlx/bin/python`; the detailed
host preparation and link-mode behavior are covered in
[`native-training.md`](native-training.md).

Review the example values:

```yaml
namespace: default
timeoutSeconds: 120
unifiedMemory: 512Mi
```

Render and inspect the Kubernetes manifest:

```sh
idlectl recipe render train/mlx-linear-regression@v1 \
  --name native-train \
  -o yaml > native-train.yaml

kubectl apply --dry-run=client -f native-train.yaml
kubectl apply -f native-train.yaml
```

The output is an `ai.idleloom.io/v1alpha1` `IdleloomWorkload`, so it follows
the same scheduler, assignment, sandbox, fencing, and log path as a manifest
written by hand.

Observe and remove the run with the Native resource commands:

```sh
idlectl get workload/native-train -n default
idlectl logs -f workload/native-train -n default
idlectl delete workload/native-train -n default
```

For an API-only host, use `idlectl logs --local` on the joined Mac after the
workload has completed.

## Linux worker training

This recipe creates a real Kubernetes Job on a joined Idleloom worker. The
Job uses a multi-architecture Python image pinned by OCI digest and selects
Nodes labeled `idleloom-worker=true`.

Review the example values:

```yaml
namespace: default
epochs: 120
learningRate: 0.08
cpu: 250m
memory: 128Mi
```

Render and apply the Job:

```sh
idlectl recipe render train/container-linear-regression@v1 \
  --name worker-train \
  -o yaml > worker-train.yaml

kubectl apply --dry-run=client -f worker-train.yaml
kubectl apply -f worker-train.yaml
```

Because this backend is a normal Job, standard Kubernetes operations retain
their usual meaning:

```sh
kubectl wait --for=condition=complete job/worker-train --timeout=2m
kubectl logs job/worker-train
kubectl describe job/worker-train
kubectl delete job/worker-train
```

The same contract supports Pod networking, `ConfigMap`, `Secret`, PVC and CSI
volumes, `exec`, and port forwarding when a workload shape needs them. Those
are Worker capabilities; the Native backend does not emulate them.

## Native Metal batch inference

The Native batch recipe creates an explicit `Batch` `IdleloomWorkload`; it
does not hide inference inside a shell command. `idlectl join` installs the
locked `qwen3-5-0-8b-mlx` catalog entry. A compatible Native host advertises
that it can prepare this model, and the agent downloads and verifies the
locked MLX runtime and model files on first use.

```sh
idlectl recipe render infer/mlx-batch@v1 \
  --name native-infer \
  -o yaml > native-infer.yaml

kubectl apply -f native-infer.yaml
idlectl get workload/native-infer -n default
idlectl logs -f workload/native-infer -n default
```

The first execution requires outbound HTTPS access from the Mac and downloads
approximately 650 MB of model files plus the locked Python runtime. Later runs
reuse the verified local copy. Runtime preparation renews the assignment lease
and reports progress through the normal workload log.

Prompts are stored in the Kubernetes API as part of the immutable workload.
Do not put credentials or other secrets in a prompt.

Native API and catalog installation currently happens during `idlectl join`.
A host joined by an older binary must be deleted and joined again with the
current binary before applying a `Batch` workload; in-place join upgrades are
not yet implemented.

## Linux worker Vulkan batch inference

The Worker inference recipe renders two standard Kubernetes objects: a
`resource.k8s.io/v1` `ResourceClaim` and a `batch/v1` `Job`. The Job downloads
a Qwen3 0.6B GGUF file from a commit-pinned URL, verifies its SHA-256, and runs
a digest-pinned llama.cpp Vulkan image.

```sh
idlectl recipe render infer/llama-vulkan@v1 \
  --name worker-infer \
  -o yaml > worker-infer.yaml

kubectl apply --dry-run=client -f worker-infer.yaml
kubectl apply -f worker-infer.yaml
kubectl wait --for=condition=complete job/worker-infer --timeout=30m
kubectl logs job/worker-infer
kubectl delete -f worker-infer.yaml
```

The target cluster must already have the Idleloom Apple Vulkan DRA driver and
an `apple-vulkan` `DeviceClass`. Override `modelURL`, `modelSHA256`,
`modelStorage`, and `memory` together when using a larger operator-approved
model; `deviceClass` selects another DRA configuration. The inference image
currently runs as its default root user because the Worker CDI path does not
yet remap the render device node to a Pod security-context UID. The container
drops all capabilities, forbids privilege escalation, and uses a read-only
root filesystem, but this is not a multi-tenant isolation boundary.

## Manifest contract

Every rendered object carries the same queryable metadata:

```yaml
metadata:
  labels:
    app.kubernetes.io/managed-by: idleloom
    ai.idleloom.io/run: RUN
    ai.idleloom.io/task: train-or-infer
    ai.idleloom.io/backend: native-or-worker
    ai.idleloom.io/runtime: RUNTIME
  annotations:
    ai.idleloom.io/recipe: TASK/RECIPE@v1
    ai.idleloom.io/recipe-digest: sha256:...
    ai.idleloom.io/input-digest: sha256:...
```

The recipe annotation documents provenance; it is not a hidden controller
trigger. A hand-written `IdleloomWorkload` or Job uses the same execution path
as a rendered object. Recipe rendering adds strict input validation,
deterministic defaults, pinned assets, and consistent metadata.

Query runs across both backends with the shared labels:

```sh
kubectl get idleloomworkloads,jobs \
  -l app.kubernetes.io/managed-by=idleloom
```

## Current result boundary

The current recipes report metrics and inference results through logs. The
Native training checkpoint is created in the assignment work directory and
removed with that ephemeral execution state. The Worker training smoke Job
does not write a checkpoint. Durable result and checkpoint destinations will
be added as a separate contract rather than embedding object-store credentials
in scripts or manifests.
