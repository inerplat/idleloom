# Reproducible ML workload recipes

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
idlectl recipe show serve/mlx-qwen@v1
idlectl recipe show serve/llama-vulkan@v1
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
current binary before applying a `Batch` or serving workload; in-place join
upgrades are not yet implemented.

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

## Native Metal serving

The Native serving recipe renders an explicit `Server` `IdleloomWorkload` and
a selectorless ClusterIP `Service`. It can schedule only to a Native host with
a fresh WireKube connection. The controller creates an `EndpointSlice` that
points at the host's WireKube mesh address after the assignment reaches
`Running`; API-only hosts are rejected by capability scheduling and continue
to use batch inference.

```sh
idlectl recipe render serve/mlx-qwen@v1 \
  --name native-serve \
  -o yaml > native-serve.yaml

kubectl apply -f native-serve.yaml
kubectl wait --for=condition=Ready \
  idleloomworkload/native-serve --timeout=15m
kubectl get endpointslice/native-serve
```

The controller generates `Secret/native-serve-auth` in the workload namespace
and copies the same API key into a fixed, agent-readable Secret in the selected
host namespace. Secret values never enter the Workload or Assignment CRs.
Start a local Kubernetes API proxy:

```sh
kubectl proxy --port=8001
```

Read the client key and send a smoke request from another terminal through the
standard Service proxy:

```sh
API_KEY="$(kubectl get secret native-serve-auth \
  -o jsonpath='{.data.api-key}' | openssl base64 -d -A)"

curl --fail-with-body \
  http://127.0.0.1:8001/api/v1/namespaces/default/services/native-serve:http/proxy/v1/chat/completions \
  -H "X-Idleloom-API-Key: ${API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen3-5-0-8b","messages":[{"role":"user","content":"Why is idle compute useful?"}],"max_tokens":64}'
```

In-cluster clients use `http://native-serve.default.svc:8000` with the normal
`Authorization: Bearer API_KEY` header. The current adapter implements
non-streaming chat completions, `GET /v1/models`, and a 512-token response
limit. Traffic to the Mac is encrypted by WireGuard and authenticated with the
generated API key; this slice does not add application-layer TLS or expose an
Ingress. The first request path may require the same approximately 650 MB
locked runtime and model preparation as Native batch inference.

Delete the manifest to stop the process and remove its EndpointSlice and
managed Secrets:

```sh
kubectl delete -f native-serve.yaml
```

## Linux worker Vulkan serving

The serving recipe renders a `resource.k8s.io/v1` `ResourceClaimTemplate`, an
`apps/v1` `Deployment`, and a ClusterIP `Service`. It uses one replica with a
`Recreate` strategy so two Pods cannot contend for the single DRA device during
a rollout. The llama.cpp OpenAI-compatible API requires a key from an existing
Kubernetes Secret; the key is never placed in recipe values or generated YAML.

Create the Secret and a values file:

```sh
openssl rand -hex 32 | kubectl create secret generic worker-serve-auth \
  --from-file=api-key=/dev/stdin
```

```yaml
apiKeySecret: worker-serve-auth
```

Render, inspect, and apply the manifest:

```sh
idlectl recipe render serve/llama-vulkan@v1 \
  --name worker-serve \
  --values worker-serve-values.yaml \
  -o yaml > worker-serve.yaml

kubectl apply --dry-run=client -f worker-serve.yaml
kubectl apply -f worker-serve.yaml
kubectl rollout status deployment/worker-serve --timeout=30m
```

Forward the cluster-private Service in one terminal:

```sh
kubectl port-forward service/worker-serve 8080:8080
```

Read the key and call the endpoint from another terminal:

```sh
API_KEY="$(kubectl get secret worker-serve-auth \
  -o jsonpath='{.data.api-key}' | openssl base64 -d -A)"

curl --fail-with-body http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer ${API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen3-0.6b","messages":[{"role":"user","content":"Why is idle compute useful?"}],"max_tokens":64}'
```

The default model is downloaded and checksum-verified into Pod-local
`emptyDir` storage on each replacement Pod. Use an operator-managed persistent
model cache in a future recipe before treating restarts as a fast path. The
Service is cluster-private and does not configure Ingress, external TLS, or
tenant authorization. Remove the generated resources and Secret explicitly:

```sh
kubectl delete -f worker-serve.yaml
kubectl delete secret worker-serve-auth
```

## Manifest contract

Every rendered object carries the same queryable metadata:

```yaml
metadata:
  labels:
    app.kubernetes.io/managed-by: idleloom
    ai.idleloom.io/run: RUN
    ai.idleloom.io/task: train-infer-or-serve
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
kubectl get idleloomworkloads,jobs,deployments \
  -l app.kubernetes.io/managed-by=idleloom
```

## Current result boundary

The current recipes report metrics and inference results through logs. The
Native training checkpoint is created in the assignment work directory and
removed with that ephemeral execution state. The Worker training smoke Job
does not write a checkpoint. Durable result and checkpoint destinations will
be added as a separate contract rather than embedding object-store credentials
in scripts or manifests.
