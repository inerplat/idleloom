# Reproducible training recipes

Idleloom recipes produce ordinary Kubernetes YAML for one of two execution
contracts. A recipe is a versioned manifest bundle in this repository, not a
cluster API or another custom resource.

| Backend | Rendered resource | Execution contract | Best fit |
| --- | --- | --- | --- |
| Native Metal | `IdleloomWorkload` | A restricted macOS process with direct Metal access | MLX and trusted host tools |
| Linux worker | `batch/v1 Job` | A normal OCI container managed by kubelet and containerd | Portable training code, Pod networking, volumes, and standard Kubernetes tooling |

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

## Manifest contract

Every rendered object carries the same queryable metadata:

```yaml
metadata:
  labels:
    app.kubernetes.io/managed-by: idleloom
    ai.idleloom.io/run: RUN
    ai.idleloom.io/task: train
    ai.idleloom.io/backend: native-or-worker
    ai.idleloom.io/runtime: RUNTIME
  annotations:
    ai.idleloom.io/recipe: train/RECIPE@v1
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
  -l app.kubernetes.io/managed-by=idleloom,ai.idleloom.io/task=train
```

## Current result boundary

The first recipes prove execution and report metrics through logs. The Native
checkpoint is currently created in the assignment work directory and removed
with that ephemeral execution state. The Worker smoke Job does not write a
checkpoint. Durable result and checkpoint destinations will be added as a
separate contract rather than embedding object-store credentials in scripts
or manifests.
