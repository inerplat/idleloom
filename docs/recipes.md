# Reproducible ML workload recipes

Idleloom recipes produce ordinary Kubernetes YAML for one of two execution
contracts. A recipe is a versioned manifest bundle in this repository, not a
cluster API or another custom resource.

| Backend | Rendered resource | Execution contract | Best fit |
| --- | --- | --- | --- |
| Native Metal | `IdleloomWorkload` | A restricted macOS process with direct Metal access | MLX, local Ollama or llama.cpp GGUF, and trusted host tools |
| Linux worker | Standard resources such as `Job` and `ResourceClaim` | A normal OCI container managed by kubelet and containerd | Portable training code, Pod networking, volumes, DRA, and standard Kubernetes tooling |

Both backends use the same recipe discovery and rendering commands. The
rendered YAML is the durable interface: it can be reviewed, stored, changed
under source control, and applied with `kubectl` without a separate Idleloom
run service.

For a zero-context installation, start with the
[documentation entry point](getting-started/index.md) and select either the
[Native Metal](getting-started/native-metal.md) or
[Linux Worker](getting-started/linux-worker.md) path. This document is the
deeper recipe reference.

Commands that reference `examples/...` assume the current directory is the
repository root. Recipe discovery and rendering themselves remain embedded in
`idlectl` and do not require a checkout.

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
idlectl recipe show infer/ollama-gguf@v1
idlectl recipe show infer/llama-cpp-metal@v1
idlectl recipe show infer/llama-vulkan@v1
idlectl recipe show serve/mlx-qwen@v1
idlectl recipe show serve/ollama-gguf@v1
idlectl recipe show serve/llama-cpp-metal@v1
idlectl recipe show serve/llama-vulkan@v1
```

The version suffix is required. A manifest always records the exact recipe ID
and separate SHA-256 digests for the embedded recipe content and normalized
render inputs. `recipe show` includes a complete example values object.

Rendering without `--values` uses embedded defaults when every parameter has a
default. Recipes with required operator-owned inputs, such as the Worker
serving API-key Secret, require a values file. Pass a user-owned YAML file or
pipe YAML through `--values -`; no source checkout is required for rendering.

## Native Metal training

This recipe creates an explicit `Train` run on a joined Native Metal host.
Idleloom creates and seals its own MLX environment on first use. Python 3.12
must be installed on the host, but users do not manage a separate MLX virtual
environment. The detailed run model, metric and artifact protocol, retry
semantics, and link-mode behavior are covered in
[`native-training.md`](native-training.md).

Review the example values:

```yaml
namespace: default
experiment: linear-regression
attempt: 1
learningRate: 0.08
steps: 100
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

The output is an immutable `ai.idleloom.io/v1alpha1` `IdleloomWorkload`. Its
status records start and finish times, the latest bounded metric summaries,
and checkpoint references. Logs retain the complete metric stream only while
that assignment remains in the host mailbox; export them before another run
reclaims the mailbox when durable history is required.

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

The runnable toolbox, hostPath, and Longhorn manifests under
[`examples/worker`](https://github.com/inerplat/idleloom/tree/main/examples/worker)
exercise these standard Pod semantics. The exact `logs`, `exec`, and
port-forward path starts in the
[Linux Worker quick start](getting-started/linux-worker.md).

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

## Native Metal Ollama GGUF batch inference

The Ollama recipe uses the same `Batch` workload contract and result envelope
as MLX, but the agent runs a digest-pinned GGUF model through an
Idleloom-owned Ollama daemon. The user's existing Ollama daemon on port 11434
is not reused, stopped, or exposed to the cluster.

Install Ollama 0.17.1 or later and the exact local model before starting the
workload:

```sh
brew install --cask ollama
brew install jq
open -a Ollama
ollama pull qwen3.5:9b
ollama list
```

The current v1 catalog pins `qwen3.5:9b` to manifest digest
`sha256:6488c96fa5faab64bb65cbd30d4289e20e6130ef535a93ef9a49f42eda893ea7`.
The agent advertises the runtime only when that model is present and the
scheduler matches its name, digest, format, and size. Before every execution,
the agent hashes the manifest and all referenced blobs. Idleloom never invokes
`ollama pull` automatically. Startup also requires Ollama to report the whole
loaded model in Metal memory; partial CPU/GPU placement fails the assignment.

Other local GGUF models use the same runtime. Read the exact name, digest, and
byte size from the local Ollama API, then create an immutable cluster catalog
entry and recipe values file:

```sh
export OLLAMA_MODEL=qwen3.5:9b
export OLLAMA_CATALOG=example-local-gguf
export OLLAMA_INFO="$(
  curl --fail --silent http://127.0.0.1:11434/api/tags | \
    jq -c --arg model "${OLLAMA_MODEL}" '.models[] | select(.name == $model)'
)"
export OLLAMA_DIGEST="$(printf '%s' "${OLLAMA_INFO}" | jq -r '.digest')"
export OLLAMA_SIZE="$(printf '%s' "${OLLAMA_INFO}" | jq -r '.size')"

test -n "${OLLAMA_DIGEST}"
test "${OLLAMA_DIGEST}" != null
test "${OLLAMA_SIZE}" -gt 0

cat > custom-ollama-model.yaml <<EOF
apiVersion: ai.idleloom.io/v1alpha1
kind: IdleloomModel
metadata:
  name: ${OLLAMA_CATALOG}
spec:
  family: ollama-gguf
  runtimeProfile: ollama-gguf-v1
  artifact:
    ollamaModel: ${OLLAMA_MODEL}
    manifestDigest: ${OLLAMA_DIGEST}
    format: gguf-v1
    sizeBytes: ${OLLAMA_SIZE}
  minimumUnifiedMemory: 16Gi
  maxContextLength: 2048
  maxConcurrentRequests: 1
EOF

cat > custom-ollama-values.yaml <<EOF
namespace: ${IDLELOOM_NAMESPACE}
model: ${OLLAMA_CATALOG}
prompt: Explain why idle compute is useful in one sentence.
maxTokens: 128
timeoutSeconds: 600
unifiedMemory: 16Gi
EOF

kube apply --dry-run=server -f custom-ollama-model.yaml
kube apply -f custom-ollama-model.yaml
```

Pass `--values custom-ollama-values.yaml` to `recipe render`. The admission
policy rejects malformed sources, and scheduling remains blocked until a host
advertises the exact local model. `minimumUnifiedMemory` must cover the model,
runtime overhead, and context; use a conservative value above the local model
size rather than treating it as an enforced memory limit.

Render, apply, and read the result:

```sh
idlectl recipe render infer/ollama-gguf@v1 \
  --name native-ollama-infer \
  -o yaml > native-ollama-infer.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply -f native-ollama-infer.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/native-ollama-infer --timeout=20m
idlectl logs workload/native-ollama-infer \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

For the custom catalog above, render with:

```sh
idlectl recipe render infer/ollama-gguf@v1 \
  --name native-ollama-infer \
  --values custom-ollama-values.yaml \
  -o yaml > native-ollama-infer.yaml
```

Delete the workload after reading the result:

```sh
idlectl delete workload/native-ollama-infer \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
rm -f native-ollama-infer.yaml
```

## Native Metal llama.cpp GGUF batch inference

Use the llama.cpp runtime when the operator already has a standalone GGUF and
wants direct control over the native Metal backend without an Ollama model
store. Idleloom starts a private `llama-server` for each assignment, binds it
only to loopback inside the macOS sandbox, and requires every model layer to be
offloaded to the selected Apple Metal device.

Install llama.cpp and copy an operator-approved GGUF into the managed directory
under the same `--root` used during `idlectl join`. The default root is shown
here:

```sh
brew install llama.cpp

export IDLELOOM_ROOT=/var/tmp/idleloom
export GGUF_SOURCE=/absolute/path/to/model.gguf
export GGUF_FILE="$(basename "${GGUF_SOURCE}")"

mkdir -p "${IDLELOOM_ROOT}/models/gguf"
chmod 700 "${IDLELOOM_ROOT}/models/gguf"
cp "${GGUF_SOURCE}" "${IDLELOOM_ROOT}/models/gguf/${GGUF_FILE}"
chmod 600 "${IDLELOOM_ROOT}/models/gguf/${GGUF_FILE}"

export GGUF_SHA256="$(shasum -a 256 "${IDLELOOM_ROOT}/models/gguf/${GGUF_FILE}" | awk '{print $1}')"
export GGUF_SIZE="$(stat -f %z "${IDLELOOM_ROOT}/models/gguf/${GGUF_FILE}")"
printf 'file=%s\nsha256=%s\nsizeBytes=%s\n' "${GGUF_FILE}" "${GGUF_SHA256}" "${GGUF_SIZE}"
```

The filename must end in `.gguf` and contain only letters, digits, dots,
underscores, and hyphens. Idleloom never downloads this file, follows a model
path outside the managed directory, or treats an Ollama tag as the same
artifact. Confirm that the installed llama.cpp build supports the chosen GGUF;
model metadata support can differ between llama.cpp and Ollama releases.

Create an immutable cluster catalog entry from the measured values:

```sh
cat > llama-cpp-model.yaml <<EOF
apiVersion: ai.idleloom.io/v1alpha1
kind: IdleloomModel
metadata:
  name: local-gguf
spec:
  family: gguf
  runtimeProfile: llama-cpp-metal-v1
  artifact:
    ggufFile: ${GGUF_FILE}
    manifestDigest: sha256:${GGUF_SHA256}
    format: gguf-v1
    sizeBytes: ${GGUF_SIZE}
  minimumUnifiedMemory: 16Gi
  maxContextLength: 2048
  maxConcurrentRequests: 1
EOF

kube apply --dry-run=server -f llama-cpp-model.yaml
kube apply -f llama-cpp-model.yaml
```

Increase `minimumUnifiedMemory` for larger files or context windows. It is a
conservative scheduling reservation rather than an enforced macOS memory
limit. The agent advertises the model only after hashing the complete file,
and it repeats that strong verification immediately before every execution.

Render and run the batch recipe:

```sh
idlectl recipe render infer/llama-cpp-metal@v1 \
  --name native-llama-infer \
  -o yaml > native-llama-infer.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply -f native-llama-infer.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/native-llama-infer --timeout=20m
idlectl logs workload/native-llama-infer \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

The result uses the same structured batch envelope as MLX and Ollama. A
startup fails closed if llama.cpp selects no Metal device, loads only part of
the model on Metal, or the file no longer matches the catalog digest.

Clean up the run and catalog entry without deleting the host-local GGUF:

```sh
idlectl delete workload/native-llama-infer \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
kube delete -f llama-cpp-model.yaml
rm -f native-llama-infer.yaml llama-cpp-model.yaml
```

## Linux worker Vulkan batch inference

The Worker inference recipe renders two standard Kubernetes objects: a
`resource.k8s.io/v1` `ResourceClaim` and a `batch/v1` `Job`. The Job downloads
a Qwen3 0.6B GGUF file from a commit-pinned URL, verifies its SHA-256, and runs
the llama.cpp CLI from a digest-pinned Ramalama image whose Mesa userspace is
compatible with the krunkit Venus device. Before loading the model, the
container requires `llama-cli --list-devices` to report a `Virtio-GPU Venus`
Vulkan device; the run fails instead of silently falling back to CPU.

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
runs as nonroot UID and GID `65532`. The DRA CDI specification publishes the
render device with group-writable permissions, while the Pod uses `fsGroup:
65532`, drops all capabilities, forbids privilege escalation, and keeps the
image root filesystem read-only. This remains an experimental device boundary,
not a reviewed multi-tenant isolation boundary.

## Native Metal serving

The Native serving recipe renders an explicit `Server` `IdleloomWorkload` and
a selectorless ClusterIP `Service`. It can schedule only to a Native host with
a healthy WireKube relay session and synchronized peer routes. The controller creates an `EndpointSlice` that
points at the host's WireKube mesh address after the assignment reaches
`Running`; API-only hosts are rejected by capability scheduling and continue
to use batch inference.

```sh
idlectl recipe render serve/mlx-qwen@v1 \
  --name native-mlx-serve \
  -o yaml > native-mlx-serve.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply -f native-mlx-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=condition=Ready \
  idleloomworkload/native-mlx-serve --timeout=15m
kube -n "${IDLELOOM_NAMESPACE}" get endpointslice/native-mlx-serve
```

The controller generates `Secret/native-mlx-serve-auth` in the workload namespace
and copies the same API key into a fixed, agent-readable Secret in the selected
host namespace. Secret values never enter the Workload or Assignment CRs.
The client must run on a real schedulable Linux kubelet node that participates
in the WireKube mesh. The example clients tolerate Idleloom's default Worker
taint and require Pod affinity to a `wirekube-agent` or
`wirekube-agent-proxy` Pod on the same node. The Native projection Node is
deliberately unschedulable and cannot run these Pods.

The repository provides one complete, digest-pinned client manifest for each
Native runtime. Each client asserts an unauthenticated HTTP 401 before making
an authenticated request. Apply the MLX client, wait for completion, and read
the response:

```sh
kube apply -f examples/native/serve-mlx-client.yaml
kube -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/native-mlx-serve-client \
  --timeout=5m
kube -n default logs pod/native-mlx-serve-client
```

These example clients intentionally use namespace `default`. Copy and edit the
manifest when the serving recipe uses another namespace.

The Kubernetes API Service proxy accepts only Pod-backed endpoints, while this
alpha path publishes the Mac through a WireKube `EndpointSlice`; `kubectl
proxy` therefore cannot expose this Service. The logs-only projection also
does not implement `kubectl port-forward` yet.

The current adapter implements
non-streaming chat completions, `GET /v1/models`, and a 512-token response
limit. One generation may run at a time; concurrent requests receive HTTP 429
with `Retry-After: 1` instead of waiting in an unbounded queue. A generation
already accepted by the model finishes even if its client disconnects, so the
shared model process remains available for later requests. Traffic to the Mac
is encrypted by WireGuard and authenticated with the generated API key; this
slice does not add application-layer TLS or expose an Ingress. The first MLX
request path may require the same approximately 650 MB locked runtime and
model preparation as Native batch inference.

To serve the pinned local GGUF model instead, render
`serve/ollama-gguf@v1`. The Service, generated Secret, authentication,
concurrency limit, and OpenAI-compatible API are identical; only the private
model process changes:

```sh
idlectl recipe render serve/ollama-gguf@v1 \
  --name native-ollama-serve \
  -o yaml > native-ollama-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" apply -f native-ollama-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=condition=Ready \
  idleloomworkload/native-ollama-serve --timeout=15m
kube apply -f examples/native/serve-ollama-client.yaml
kube -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/native-ollama-serve-client --timeout=5m
kube -n default logs pod/native-ollama-serve-client
```

The client uses model alias `qwen3-5-9b`, Service
`native-ollama-serve.default.svc`, and generated Secret
`native-ollama-serve-auth`. For a custom catalog, create a serving values file
with `model`, `modelAlias`, `servicePort`, `namespace`, and `unifiedMemory`, then
pass it through `--values` when rendering.

The direct GGUF runtime uses the same serving contract. Keep the
`IdleloomModel` from the llama.cpp batch section, then render the serving
recipe:

```sh
idlectl recipe render serve/llama-cpp-metal@v1 \
  --name native-llama-serve \
  -o yaml > native-llama-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" apply -f native-llama-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=condition=Ready \
  idleloomworkload/native-llama-serve --timeout=15m
kube apply -f examples/native/serve-llama-cpp-client.yaml
kube -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/native-llama-serve-client --timeout=5m
kube -n default logs pod/native-llama-serve-client
```

The client calls model alias `local-gguf` through Service
`native-llama-serve.default.svc` with generated Secret
`native-llama-serve-auth`. Authentication, the one-request concurrency limit,
restart behavior, and cluster-private WireKube endpoint are identical to MLX
and Ollama serving. The private llama.cpp server itself remains bound to
loopback; only Idleloom's authenticated adapter listens on the host mesh
address.

Delete the manifest to stop the process and remove its EndpointSlice and
managed Secrets:

```sh
kube -n default delete pod/native-mlx-serve-client \
  pod/native-ollama-serve-client \
  pod/native-llama-serve-client \
  --ignore-not-found
for manifest in \
  native-mlx-serve.yaml \
  native-ollama-serve.yaml \
  native-llama-serve.yaml
do
  if test -f "${manifest}"; then
    kube -n "${IDLELOOM_NAMESPACE}" delete -f "${manifest}"
  fi
done
rm -f native-mlx-serve.yaml native-ollama-serve.yaml native-llama-serve.yaml
```

## Linux worker Vulkan serving

The serving recipe renders a `resource.k8s.io/v1` `ResourceClaimTemplate`, an
`apps/v1` `Deployment`, and a ClusterIP `Service`. It uses one replica with a
`Recreate` strategy so two Pods cannot contend for the single DRA device during
a rollout. Complete the same registry-published DRA driver and `apple-vulkan`
`DeviceClass` setup required by Worker batch inference first. The llama.cpp
OpenAI-compatible API requires a key from an existing Kubernetes Secret; the
key is never placed in recipe values or generated YAML.
The key protects generation endpoints such as `/v1/chat/completions`.
Upstream llama.cpp leaves `/v1/models` readable to clients that can already
reach the cluster-private Service, so model identifiers are not treated as
secret metadata.

Create the Secret and a values file:

```sh
openssl rand -hex 32 | kube -n "${IDLELOOM_NAMESPACE}" create secret generic worker-serve-auth \
  --from-file=api-key=/dev/stdin

cat > worker-serve-values.yaml <<EOF
namespace: ${IDLELOOM_NAMESPACE}
apiKeySecret: worker-serve-auth
EOF
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
Pod also mounts a writable runtime cache for Vulkan shader compilation while
keeping the image root filesystem read-only. The Service is cluster-private
and does not configure Ingress, external TLS, or
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

Native training recognizes bounded metric and artifact records in process
output. Host-local `file://` artifacts are digest-verified and retained only as
a small alpha cache; durable runs should upload checkpoints to an external
artifact store and emit a credential-free URI. Worker recipes keep normal
container semantics and can use volumes or an experiment tracker directly.
Neither backend puts object-store credentials into recipe values or workload
metadata.
