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

Install `kubectl` and define the kubeconfig, context, and namespace used for
enrollment. The helper keeps selection explicit without changing the
kubeconfig's persistent `current-context`:

```sh
brew install kubectl
export IDLELOOM_KUBECONFIG="${HOME}/.kube/config"
export IDLELOOM_CONTEXT=my-cluster
export IDLELOOM_NAMESPACE=default

kube() {
  kubectl \
    --kubeconfig "${IDLELOOM_KUBECONFIG}" \
    --context "${IDLELOOM_CONTEXT}" \
    "$@"
}

idlectl version
idlectl recipe list
kube cluster-info
```

The examples render `namespace: default`, matching `IDLELOOM_NAMESPACE` above.
To use another namespace, change the variable and pass the same `namespace`
value through each recipe's `--values` file; edit the standalone client Pod
manifest to match.

If `idlectl recipe list` reports `unknown command "recipe"`, the installed
release predates this guide. Build the current repository checkout and put its
`bin` directory first on `PATH`, as described in the main README.

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

kube -n "${IDLELOOM_NAMESPACE}" apply --dry-run=client -f native-train.yaml
kube -n "${IDLELOOM_NAMESPACE}" apply -f native-train.yaml
```

The output is an `ai.idleloom.io/v1alpha1` `IdleloomWorkload`, so it follows
the same scheduler, assignment, sandbox, fencing, and log path as a manifest
written by hand.

Observe and remove the run with the Native resource commands:

```sh
idlectl get workload/native-train \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
idlectl logs -f workload/native-train \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
idlectl delete workload/native-train \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
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

kube -n "${IDLELOOM_NAMESPACE}" apply --dry-run=client -f worker-train.yaml
kube -n "${IDLELOOM_NAMESPACE}" apply -f worker-train.yaml
```

Because this backend is a normal Job, standard Kubernetes operations retain
their usual meaning:

```sh
kube -n "${IDLELOOM_NAMESPACE}" wait --for=condition=complete job/worker-train --timeout=2m
kube -n "${IDLELOOM_NAMESPACE}" logs job/worker-train
kube -n "${IDLELOOM_NAMESPACE}" describe job/worker-train
kube -n "${IDLELOOM_NAMESPACE}" delete job/worker-train
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

kube -n "${IDLELOOM_NAMESPACE}" apply -f native-infer.yaml
idlectl get workload/native-infer \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
idlectl logs -f workload/native-infer \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

Wait for a terminal success state when running this in automation:

```sh
kube -n "${IDLELOOM_NAMESPACE}" wait --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/native-infer \
  --timeout=20m
```

The first execution requires outbound HTTPS access from the Mac and downloads
approximately 650 MB of model files plus the locked Python runtime. Later runs
reuse the verified local copy. Runtime preparation renews the assignment lease
and reports progress through the normal workload log.

Prompts are stored in the Kubernetes API as part of the immutable workload.
Do not put credentials or other secrets in a prompt.

A successful log ends with one structured result record. The generated text
varies, but the envelope is stable:

```json
{"type":"result","text":"...","elapsedMillis":1234}
```

Remove the workload after inspecting the result:

```sh
idlectl delete workload/native-infer \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
rm -f native-infer.yaml
```

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

kube -n "${IDLELOOM_NAMESPACE}" apply --dry-run=client -f worker-infer.yaml
kube -n "${IDLELOOM_NAMESPACE}" apply -f worker-infer.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=condition=complete job/worker-infer --timeout=30m
kube -n "${IDLELOOM_NAMESPACE}" logs job/worker-infer
kube -n "${IDLELOOM_NAMESPACE}" delete -f worker-infer.yaml
```

The target cluster must already have the Idleloom Apple Vulkan DRA driver and
an `apple-vulkan` `DeviceClass`. Follow the registry push, DaemonSet rollout,
and `ResourceSlice` verification steps in the main README before this recipe.
Override `modelURL`, `modelSHA256`,
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

kube -n "${IDLELOOM_NAMESPACE}" apply -f native-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=condition=Ready \
  idleloomworkload/native-serve --timeout=15m
kube -n "${IDLELOOM_NAMESPACE}" get endpointslice/native-serve
```

The controller generates `Secret/native-serve-auth` in the workload namespace
and copies the same API key into a fixed, agent-readable Secret in the selected
host namespace. Secret values never enter the Workload or Assignment CRs.
The client must run on a normal schedulable Linux node that participates in the
WireKube mesh. The Native projection Node is deliberately unschedulable and
cannot run this Pod.

Create a one-shot in-cluster client that mounts the generated Secret and calls
the normal ClusterIP Service:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: native-serve-client
  namespace: default
spec:
  restartPolicy: Never
  containers:
    - name: curl
      image: curlimages/curl@sha256:94e9e444bcba979c2ea12e27ae39bee4cd10bc7041a472c4727a558e213744e6
      command: ["/bin/sh", "-ec"]
      args:
        - |
          API_KEY="$(cat /var/run/secrets/idleloom/api-key)"
          curl --fail-with-body \
            http://native-serve.default.svc:8000/v1/chat/completions \
            -H "Authorization: Bearer ${API_KEY}" \
            -H 'Content-Type: application/json' \
            -d '{"model":"qwen3-5-0-8b","messages":[{"role":"user","content":"Why is idle compute useful?"}],"max_tokens":64}'
      volumeMounts:
        - name: auth
          mountPath: /var/run/secrets/idleloom
          readOnly: true
  volumes:
    - name: auth
      secret:
        secretName: native-serve-auth
```

Apply the client manifest, wait for completion, and read the response:

```sh
kube -n "${IDLELOOM_NAMESPACE}" apply -f native-serve-client.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/native-serve-client \
  --timeout=5m
kube -n "${IDLELOOM_NAMESPACE}" logs pod/native-serve-client
```

The Kubernetes API Service proxy accepts only Pod-backed endpoints, while this
alpha path publishes the Mac through a WireKube `EndpointSlice`; `kubectl
proxy` therefore cannot expose this Service. The logs-only projection also
does not implement `kubectl port-forward` yet.

The current adapter implements
non-streaming chat completions, `GET /v1/models`, and a 512-token response
limit. Traffic to the Mac is encrypted by WireGuard and authenticated with the
generated API key; this slice does not add application-layer TLS or expose an
Ingress. The first request path may require the same approximately 650 MB
locked runtime and model preparation as Native batch inference.

Delete the manifest to stop the process and remove its EndpointSlice and
managed Secrets:

```sh
kube -n "${IDLELOOM_NAMESPACE}" delete pod/native-serve-client --ignore-not-found
kube -n "${IDLELOOM_NAMESPACE}" delete -f native-serve.yaml
rm -f native-serve.yaml native-serve-client.yaml
```

## Linux worker Vulkan serving

The serving recipe renders a `resource.k8s.io/v1` `ResourceClaimTemplate`, an
`apps/v1` `Deployment`, and a ClusterIP `Service`. It uses one replica with a
`Recreate` strategy so two Pods cannot contend for the single DRA device during
a rollout. Complete the same registry-published DRA driver and `apple-vulkan`
`DeviceClass` setup required by Worker batch inference first. The llama.cpp
OpenAI-compatible API requires a key from an existing Kubernetes Secret; the
key is never placed in recipe values or generated YAML.

Create the Secret and a values file:

```sh
openssl rand -hex 32 | kube -n "${IDLELOOM_NAMESPACE}" create secret generic worker-serve-auth \
  --from-file=api-key=/dev/stdin
```

```yaml
namespace: default
apiKeySecret: worker-serve-auth
```

Render, inspect, and apply the manifest:

```sh
idlectl recipe render serve/llama-vulkan@v1 \
  --name worker-serve \
  --values worker-serve-values.yaml \
  -o yaml > worker-serve.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply --dry-run=client -f worker-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" apply -f worker-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" rollout status deployment/worker-serve --timeout=30m
```

Forward the cluster-private Service in one terminal:

```sh
kube -n "${IDLELOOM_NAMESPACE}" port-forward service/worker-serve 8080:8080
```

Read the key and call the endpoint from another terminal:

```sh
API_KEY="$(kube -n "${IDLELOOM_NAMESPACE}" get secret worker-serve-auth \
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
kube -n "${IDLELOOM_NAMESPACE}" delete -f worker-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" delete secret worker-serve-auth
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
kube -A get idleloomworkloads,jobs,deployments \
  -l app.kubernetes.io/managed-by=idleloom
```

## Current result boundary

The current recipes report metrics and inference results through logs. The
Native training checkpoint is created in the assignment work directory and
removed with that ephemeral execution state. The Worker training smoke Job
does not write a checkpoint. Durable result and checkpoint destinations will
be added as a separate contract rather than embedding object-store credentials
in scripts or manifests.
