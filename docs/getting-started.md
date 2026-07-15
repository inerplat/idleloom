# End-to-end getting started

This guide starts with an ordinary Apple Silicon Mac and an existing
Kubernetes kubeconfig. It covers both Idleloom execution modes and provides a
copyable verification path for each public capability.

Use the Native Metal track when direct Apple Metal access is the priority. Use
the Linux Worker track when the workload needs normal Pods, OCI containers,
volumes, `exec`, or port forwarding. The two tracks can be used independently.

## What this guide assumes

- an Apple Silicon Mac;
- a kubeconfig for a reachable Kubernetes cluster;
- permission to install cluster-scoped RBAC and CRDs;
- Homebrew;
- one real schedulable Linux kubelet node in the cluster when testing Native serving;
- a container registry that accepts your push and allows the Worker to pull the
  development Vulkan DRA image; an anonymously readable test repository is the
  simplest path.

Native MLX requires macOS 26 or later and Python 3.12. The Linux Worker requires
macOS 14 or later, krunkit, Kubernetes bootstrap-token authentication, and a
WireKube mesh. Vulkan recipes additionally require Kubernetes 1.35 or later
with `resource.k8s.io/v1` enabled.

All examples in this guide use namespace `default`. Use a separate test cluster
or namespace when evaluating experimental software.

## Run order

1. Complete [Prepare one shell](#prepare-one-shell).
2. Read [Choose a track](#choose-a-track).
3. Follow either [Native Metal track](#native-metal-track) or
   [Linux Worker track](#linux-worker-track) from top to bottom.
4. Run only the conditional Ollama, standalone GGUF, Longhorn, NFS, or Vulkan
   sections whose external prerequisites you can provide.
5. Finish with the cleanup section for the selected track.
6. Use [Optional full lab uninstall](#optional-full-lab-uninstall) only for a
   dedicated cluster and Mac where no other user depends on shared resources.

Commands are grouped in execution order. Do not apply every code block at once;
several sections deliberately delete and rejoin the same host to demonstrate an
immutable security or connectivity mode.

## Prepare one shell

Clone the repository to obtain the reviewed example manifests. Native users may
still install the executable with Homebrew; the checkout is used for examples
and for the source fallback. The Linux Worker executable and development DRA
image are currently built from this checkout.

```sh
brew install git kubectl

export IDLELOOM_REPO="${HOME}/src/idleloom"
mkdir -p "$(dirname "${IDLELOOM_REPO}")"
git clone https://github.com/inerplat/idleloom.git "${IDLELOOM_REPO}"
cd "${IDLELOOM_REPO}"

export IDLELOOM_KUBECONFIG="${HOME}/.kube/config"
export IDLELOOM_CONTEXT=my-cluster
export IDLELOOM_NAMESPACE=default

kube() {
  kubectl \
    --kubeconfig "${IDLELOOM_KUBECONFIG}" \
    --context "${IDLELOOM_CONTEXT}" \
    "$@"
}

kube cluster-info
kube get nodes -o wide
```

Replace `my-cluster` before continuing. Keep the same shell open so the helper
function and environment variables remain available. The `kube` function is
shell-local. If a port-forward section uses a second terminal, repeat the three
exports and the `kube()` definition there before running a command that invokes
`kube`; a terminal used only for `curl` needs no helper.

## Choose a track

| Requirement | Native Metal | Linux Worker |
| --- | --- | --- |
| Direct MLX or Metal execution | Yes | No |
| Ollama or llama.cpp on macOS | Yes | No |
| Ordinary OCI containers | No | Yes |
| Standard Pod networking and volumes | No | Yes |
| Kubernetes `logs` | Connected mode | Yes |
| Kubernetes `exec` and port-forward | No | Yes |
| hostPath and Longhorn-style storage | No | Yes |
| Apple GPU interface | Metal | Vulkan through krunkit and DRA |

Native projection creates observability-only Node and Pod objects. It is not a
kubelet and does not accept arbitrary Pods.

# Native Metal track

## Install and verify the CLI

```sh
brew install python@3.12
brew install inerplat/tap/idleloom

idlectl version
idlectl recipe list
```

The recipe list must include all ten entries shown in
[`recipes.md`](recipes.md). If the installed release is older, build the current
checkout and keep it first on `PATH`:

```sh
brew install go
cd "${IDLELOOM_REPO}"
make build-idlectl
export PATH="${IDLELOOM_REPO}/bin:${PATH}"
idlectl recipe list
```

## Join a connected host

Connected mode installs or reuses WireKube, creates a restricted `WireKubePeer`,
and supports standard Kubernetes log streaming and Native serving.

```sh
export IDLELOOM_HOST=evening-mac

idlectl join "${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"

idlectl get host/"${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"
```

The host must report `READY=True` and `CONNECTED=True`. Interactive join shows
the WireKube installation plan when the dependency is missing. Automation must
authorize that installation explicitly:

```sh
idlectl join "${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --install-dependencies \
  --yes
```

Do not run the second command after a successful interactive join. One Mac can
have one local Native enrollment at a time.

The integrated dependency plan uses a public TCP LoadBalancer. If the cluster
has no LoadBalancer, install WireKube v0.0.15 first with the NodePort or WSS
path in [Install WireKube v0.0.15](#install-wirekube-v0015), verify it with
`wirekubectl doctor`, then return to the normal `idlectl join` command. Use
API-only mode when no inbound relay endpoint can be provided.

## Run a sandboxed shell

```sh
idlectl run native-shell \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}" \
  --shell 'uname -m; sw_vers -productVersion; id' \
  --isolation sandbox \
  --network none

idlectl logs -f workload/native-shell \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"

idlectl delete workload/native-shell \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

Shell source is stored in the Kubernetes API. Never place credentials in a
shell command or prompt.

## Optional trusted host shell

Full host shell access is an immutable enrollment boundary. Use it only for
trusted workloads. Delete the current host, join it again with maximum access,
then select host isolation for a workload:

```sh
idlectl delete host/"${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"

idlectl join "${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --shell-access host

idlectl run trusted-host-shell \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}" \
  --shell 'pwd; command -v python3; curl -I https://example.com' \
  --isolation host \
  --network outbound

idlectl logs -f workload/trusted-host-shell \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"

idlectl delete workload/trusted-host-shell \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

A host joined with maximum `host` access can still run sandboxed workloads.

## Run MLX training

```sh
idlectl recipe render train/mlx-linear-regression@v1 \
  --name native-train \
  -o yaml > native-train.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply --dry-run=client -f native-train.yaml
kube -n "${IDLELOOM_NAMESPACE}" apply -f native-train.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait \
  --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/native-train \
  --timeout=15m

idlectl get workload/native-train -o yaml \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
idlectl logs workload/native-train \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"

kube -n "${IDLELOOM_NAMESPACE}" delete -f native-train.yaml
rm -f native-train.yaml
```

The result must contain MLX device information, converged parameters, bounded
metric status, and a checkpoint artifact. See
[`native-training.md`](native-training.md) for custom training source, metrics,
artifacts, and retries.

## Run MLX batch inference

```sh
idlectl recipe render infer/mlx-batch@v1 \
  --name native-mlx-infer \
  -o yaml > native-mlx-infer.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply -f native-mlx-infer.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait \
  --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/native-mlx-infer \
  --timeout=20m
idlectl logs workload/native-mlx-infer \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"

kube -n "${IDLELOOM_NAMESPACE}" delete -f native-mlx-infer.yaml
rm -f native-mlx-infer.yaml
```

The first run downloads and verifies the locked runtime and model. A successful
log ends with a JSON result record.

## Prepare Ollama and verify the catalog digest

Idleloom uses a private Ollama process but reads the operator-installed Ollama
model store. Install the CLI, start the normal desktop daemon for model
management, and inspect the exact local model metadata:

```sh
brew install --cask ollama
brew install jq
open -a Ollama

for attempt in {1..30}; do
  if curl --fail --silent http://127.0.0.1:11434/api/tags >/dev/null; then
    break
  fi
  sleep 1
done
curl --fail --silent http://127.0.0.1:11434/api/tags >/dev/null

export OLLAMA_MODEL=qwen3.5:9b
ollama pull "${OLLAMA_MODEL}"
ollama list

export OLLAMA_INFO="$(
  curl --fail --silent http://127.0.0.1:11434/api/tags | \
    jq -c --arg model "${OLLAMA_MODEL}" '.models[] | select(.name == $model)'
)"
export OLLAMA_DIGEST="$(printf '%s' "${OLLAMA_INFO}" | jq -r '.digest')"
export OLLAMA_SIZE="$(printf '%s' "${OLLAMA_INFO}" | jq -r '.size')"

test -n "${OLLAMA_DIGEST}"
test "${OLLAMA_DIGEST}" != null
test "${OLLAMA_SIZE}" -gt 0
printf 'model=%s\ndigest=%s\nsizeBytes=%s\n' \
  "${OLLAMA_MODEL}" "${OLLAMA_DIGEST}" "${OLLAMA_SIZE}"
```

The built-in `qwen3-5-9b-ollama` catalog currently expects:

```text
sha256:6488c96fa5faab64bb65cbd30d4289e20e6130ef535a93ef9a49f42eda893ea7
```

If the digest matches, use the default recipes. If it differs, create a catalog
entry and values files from the measured values instead of weakening digest
verification:

```sh
export OLLAMA_CATALOG=local-qwen35-9b

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

cat > custom-ollama-infer-values.yaml <<EOF
namespace: ${IDLELOOM_NAMESPACE}
model: ${OLLAMA_CATALOG}
prompt: Explain why idle compute is useful in one sentence.
maxTokens: 128
timeoutSeconds: 600
unifiedMemory: 16Gi
EOF

cat > custom-ollama-serve-values.yaml <<EOF
namespace: ${IDLELOOM_NAMESPACE}
model: ${OLLAMA_CATALOG}
modelAlias: qwen3-5-9b
servicePort: 8000
unifiedMemory: 16Gi
EOF

kube apply --dry-run=server -f custom-ollama-model.yaml
kube apply -f custom-ollama-model.yaml
kube get idleloommodel "${OLLAMA_CATALOG}" -o yaml
```

Pass the relevant values file to the Ollama render commands below. Omit
`--values` when the built-in digest matches.

## Run Ollama batch inference

```sh
idlectl recipe render infer/ollama-gguf@v1 \
  --name native-ollama-infer \
  -o yaml > native-ollama-infer.yaml

# When using the custom catalog, render again with:
#   --values custom-ollama-infer-values.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply -f native-ollama-infer.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait \
  --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/native-ollama-infer \
  --timeout=20m
idlectl logs workload/native-ollama-infer \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"

kube -n "${IDLELOOM_NAMESPACE}" delete -f native-ollama-infer.yaml
rm -f native-ollama-infer.yaml
```

For a custom catalog, the render command is:

```sh
idlectl recipe render infer/ollama-gguf@v1 \
  --name native-ollama-infer \
  --values custom-ollama-infer-values.yaml \
  -o yaml > native-ollama-infer.yaml
```

## Prepare a standalone llama.cpp GGUF

Idleloom does not choose or download a standalone GGUF. Obtain a model whose
license and provenance you accept, then install it under the same root used by
`idlectl join`:

```sh
brew install llama.cpp
command -v llama-server

export IDLELOOM_ROOT=/var/tmp/idleloom
export GGUF_SOURCE=/absolute/path/to/model.gguf
export GGUF_FILE="$(basename "${GGUF_SOURCE}")"
printf '%s\n' "${GGUF_FILE}" | grep -Eq '^[A-Za-z0-9._-]+\.gguf$'

mkdir -p "${IDLELOOM_ROOT}/models/gguf"
chmod 700 "${IDLELOOM_ROOT}/models/gguf"
cp "${GGUF_SOURCE}" "${IDLELOOM_ROOT}/models/gguf/${GGUF_FILE}"
chmod 600 "${IDLELOOM_ROOT}/models/gguf/${GGUF_FILE}"

export GGUF_SHA256="$(
  shasum -a 256 "${IDLELOOM_ROOT}/models/gguf/${GGUF_FILE}" | awk '{print $1}'
)"
export GGUF_SIZE="$(stat -f %z "${IDLELOOM_ROOT}/models/gguf/${GGUF_FILE}")"

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
kube get idleloommodel local-gguf -o yaml
```

Increase the memory values for larger models or context windows. The host must
have enough unified memory for the catalog reservation.

## Run llama.cpp batch inference

```sh
idlectl recipe render infer/llama-cpp-metal@v1 \
  --name native-llama-infer \
  -o yaml > native-llama-infer.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply -f native-llama-infer.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait \
  --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/native-llama-infer \
  --timeout=20m
idlectl logs workload/native-llama-infer \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"

kube -n "${IDLELOOM_NAMESPACE}" delete -f native-llama-infer.yaml
rm -f native-llama-infer.yaml
```

Idleloom rejects CPU fallback or partial Metal layer placement.

## Serve MLX, Ollama, and llama.cpp

Native serving requires connected WireKube mode and at least one real
schedulable Linux kubelet node that participates in the mesh. The client
examples tolerate Idleloom's default Worker taint, so that node may be an
Idleloom Worker. Required Pod affinity places each client only on a node that
already runs a `wirekube-agent` or `wirekube-agent-proxy` Pod. Only one Native
workload runs on a host at a time, so complete and remove each serving example
before starting the next.

### MLX serving

```sh
idlectl recipe render serve/mlx-qwen@v1 \
  --name native-mlx-serve \
  -o yaml > native-mlx-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" apply -f native-mlx-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=condition=Ready \
  idleloomworkload/native-mlx-serve --timeout=20m

kube apply -f "${IDLELOOM_REPO}/examples/native/serve-mlx-client.yaml"
kube -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/native-mlx-serve-client --timeout=10m
kube -n default logs pod/native-mlx-serve-client

kube -n default delete pod/native-mlx-serve-client --ignore-not-found
kube -n "${IDLELOOM_NAMESPACE}" delete -f native-mlx-serve.yaml
rm -f native-mlx-serve.yaml
```

### Ollama serving

```sh
idlectl recipe render serve/ollama-gguf@v1 \
  --name native-ollama-serve \
  -o yaml > native-ollama-serve.yaml

# Add --values custom-ollama-serve-values.yaml when using a custom catalog.

kube -n "${IDLELOOM_NAMESPACE}" apply -f native-ollama-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=condition=Ready \
  idleloomworkload/native-ollama-serve --timeout=20m

kube apply -f "${IDLELOOM_REPO}/examples/native/serve-ollama-client.yaml"
kube -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/native-ollama-serve-client --timeout=10m
kube -n default logs pod/native-ollama-serve-client

kube -n default delete pod/native-ollama-serve-client --ignore-not-found
kube -n "${IDLELOOM_NAMESPACE}" delete -f native-ollama-serve.yaml
rm -f native-ollama-serve.yaml
```

For the custom catalog, use this render command instead of the default render:

```sh
idlectl recipe render serve/ollama-gguf@v1 \
  --name native-ollama-serve \
  --values custom-ollama-serve-values.yaml \
  -o yaml > native-ollama-serve.yaml
```

### llama.cpp serving

```sh
idlectl recipe render serve/llama-cpp-metal@v1 \
  --name native-llama-serve \
  -o yaml > native-llama-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" apply -f native-llama-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=condition=Ready \
  idleloomworkload/native-llama-serve --timeout=20m

kube apply -f "${IDLELOOM_REPO}/examples/native/serve-llama-cpp-client.yaml"
kube -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/native-llama-serve-client --timeout=10m
kube -n default logs pod/native-llama-serve-client

kube -n default delete pod/native-llama-serve-client --ignore-not-found
kube -n "${IDLELOOM_NAMESPACE}" delete -f native-llama-serve.yaml
rm -f native-llama-serve.yaml
```

The controller creates a `<service-name>-auth` Secret for each Native server.
Each example first verifies that an unauthenticated request returns HTTP 401,
then mounts that Secret and calls the matching Service and model alias.
Native projection supports logs only; `kubectl exec`, attach, and port-forward
intentionally return unsupported responses.

Remove operator-created catalog entries after all dependent workloads are
gone. Built-in MLX and Ollama catalog entries are owned by Idleloom and should
remain:

```sh
kube delete idleloommodel local-gguf --ignore-not-found
if test -n "${OLLAMA_CATALOG:-}"; then
  kube delete idleloommodel "${OLLAMA_CATALOG}" --ignore-not-found
fi
rm -f llama-cpp-model.yaml \
  custom-ollama-model.yaml \
  custom-ollama-infer-values.yaml \
  custom-ollama-serve-values.yaml
```

Deleting a catalog entry does not delete the operator-owned GGUF under
`${IDLELOOM_ROOT}/models/gguf`. Remove that file separately only when it is no
longer needed.

The API-only section below starts by deleting the connected host. If you are
stopping the Native track here, delete it now instead:

```sh
idlectl delete host/"${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"
```

Connected host deletion asks for macOS administrator authorization to remove
the root-owned link helper. It removes the host peer and local credentials but
does not uninstall the shared WireKube deployment.

## Verify API-only mode

API-only is a separate enrollment scenario. Remove all Native workloads and the
connected host first so the example cannot be assigned to another host.

```sh
idlectl delete host/"${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"

export IDLELOOM_HOST=evening-api
idlectl join "${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --link api-only \
  --projection=false

idlectl recipe render train/mlx-linear-regression@v1 \
  --name linear-api \
  -o yaml > linear-api.yaml
kube -n "${IDLELOOM_NAMESPACE}" apply -f linear-api.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait \
  --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/linear-api \
  --timeout=15m

idlectl logs --local workload/linear-api \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"

kube -n "${IDLELOOM_NAMESPACE}" delete -f linear-api.yaml
rm -f linear-api.yaml
idlectl delete host/"${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"
```

API-only logs are completed local snapshots and do not support follow. Native
serving requires connected mode and is rejected on API-only hosts.

# Linux Worker track

## Build the Worker CLI

The Homebrew formula currently installs the Native CLI only. Build `idleloom`
and the development DRA binary from the reviewed checkout:

```sh
brew install go
brew tap libkrun/krun
brew install krunkit

cd "${IDLELOOM_REPO}"
make build
export PATH="${IDLELOOM_REPO}/bin:${PATH}"

idleloom version
idlectl recipe list
krunkit --version
gvproxy -version
```

## Install WireKube v0.0.15

Download the compatible easy installer and verify its checksum:

```sh
export WIREKUBE_VERSION=v0.0.15
export WIREKUBECTL="${HOME}/.local/bin/wirekubectl"
mkdir -p "$(dirname "${WIREKUBECTL}")"

curl -fL \
  -o /tmp/wirekubectl-darwin-arm64 \
  "https://github.com/inerplat/wirekube/releases/download/${WIREKUBE_VERSION}/wirekubectl-darwin-arm64"
curl -fL \
  -o /tmp/wirekubectl-checksums.txt \
  "https://github.com/inerplat/wirekube/releases/download/${WIREKUBE_VERSION}/wirekubectl-checksums.txt"

expected="$(
  awk '$2 == "wirekubectl-darwin-arm64" { print $1 }' \
    /tmp/wirekubectl-checksums.txt
)"
actual="$(shasum -a 256 /tmp/wirekubectl-darwin-arm64 | awk '{ print $1 }')"
test -n "${expected}"
test "${actual}" = "${expected}"

install -m 0755 /tmp/wirekubectl-darwin-arm64 "${WIREKUBECTL}"
"${WIREKUBECTL}" version
```

If Native connected mode already installed a compatible WireKube release,
`wirekubectl status` and `wirekubectl doctor` should succeed. In that case,
verify that `WireKubeMesh/default` advertises Node InternalIPs and skip the
install command below.

```sh
kube get wirekubemesh default \
  -o jsonpath='{.spec.autoAllowedIPs.includeNodeInternalIP}{"\n"}'
```

The result must be `true`.

For a cluster with a public TCP LoadBalancer:

```sh
"${WIREKUBECTL}" install \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --node-addresses internal-ip \
  --relay load-balancer \
  --relay-udp=false
```

For a cluster without a LoadBalancer but with a stable public node address:

```sh
export WIREKUBE_PUBLIC_NODE_IP=203.0.113.10

"${WIREKUBECTL}" install \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --node-addresses internal-ip \
  --relay node-port \
  --relay-transport tcp \
  --relay-endpoint "${WIREKUBE_PUBLIC_NODE_IP}:30478" \
  --relay-udp=false

nc -vz "${WIREKUBE_PUBLIC_NODE_IP}" 30478
```

WSS requires an operator-managed public hostname, certificate, and HTTPS
Gateway or Ingress. Follow the
[WireKube v0.0.15 relay guide](https://github.com/inerplat/wirekube/blob/v0.0.15/docs/guides/relay-entrypoints.md)
and keep `--node-addresses internal-ip`. Idleloom does not create DNS or TLS
infrastructure.

Verify the selected installation:

```sh
"${WIREKUBECTL}" status \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"
"${WIREKUBECTL}" doctor \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"
```

## Join the Worker

```sh
idleloom init \
  --dry-run \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"

idleloom init \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --name evening-worker \
  --cpus 4 \
  --memory 8g \
  --disk 40g

idleloom status
kube get node evening-worker -o wide
```

The Node must report `Ready` and carry labels `idleloom-worker=true` and
`idleloom-accelerator=apple-vulkan`.

## Run an ordinary Pod, exec, and port-forward

```sh
kube apply -f "${IDLELOOM_REPO}/examples/worker/toolbox-pod.yaml"
kube -n default wait --for=condition=Ready \
  pod/idleloom-worker-toolbox --timeout=5m

kube -n default logs pod/idleloom-worker-toolbox
kube -n default exec pod/idleloom-worker-toolbox -- uname -m
```

In one terminal:

```sh
kube -n default port-forward pod/idleloom-worker-toolbox 8000:8000
```

In another terminal:

```sh
curl --fail http://127.0.0.1:8000/
```

Stop the port-forward with `Ctrl-C`, then remove the Pod:

```sh
kube -n default delete pod/idleloom-worker-toolbox
```

## Verify persistent hostPath storage

These manifests assume one Idleloom Worker so the writer and reader use the
same node-local filesystem. In a multi-Worker cluster, add an explicit hostname
selector to both manifests.

```sh
kube apply -f "${IDLELOOM_REPO}/examples/worker/hostpath-writer.yaml"
kube -n default wait --for=condition=complete \
  job/idleloom-hostpath-writer --timeout=5m
kube -n default logs job/idleloom-hostpath-writer

kube -n default delete job/idleloom-hostpath-writer
kube apply -f "${IDLELOOM_REPO}/examples/worker/hostpath-reader.yaml"
kube -n default wait --for=condition=complete \
  job/idleloom-hostpath-reader --timeout=5m
kube -n default logs job/idleloom-hostpath-reader

kube -n default delete job/idleloom-hostpath-reader
kube apply -f "${IDLELOOM_REPO}/examples/worker/hostpath-cleanup.yaml"
kube -n default wait --for=condition=complete \
  job/idleloom-hostpath-cleanup --timeout=5m
kube -n default delete job/idleloom-hostpath-cleanup
```

Both logs must print `persistent worker data`. The directory lives inside the
Worker VM at `/var/lib/idleloom/volumes/docs-smoke`; it is not a macOS host
directory.

## Verify Longhorn and host iSCSI support

Idleloom installs `open-iscsi`, starts `iscsid`, and persists kubelet state.
When Longhorn is already installed and its node components tolerate the
Worker's dedicated taint, use the PVC smoke manifest:

```sh
kube get storageclass longhorn
kube apply -f "${IDLELOOM_REPO}/examples/worker/longhorn-pvc-smoke.yaml"
kube -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/idleloom-longhorn-smoke --timeout=10m
kube -n default logs pod/idleloom-longhorn-smoke
kube -n default get pvc idleloom-longhorn-smoke

kube -n default delete -f \
  "${IDLELOOM_REPO}/examples/worker/longhorn-pvc-smoke.yaml"
```

The log must print `longhorn volume mounted`. This is an integration test for
the host iSCSI stack, CSI attachment, mount, and Pod access. Do not apply the
manifest when the cluster has no Longhorn installation.

## Verify NFS client support

This test requires an existing NFSv4.1 export reachable from the Worker VM.
Generate a concrete manifest from the reviewed example values:

```sh
export NFS_SERVER=REPLACE_WITH_NFS_SERVER
export NFS_PATH=REPLACE_WITH_EXPORT_PATH

test "${NFS_SERVER}" != REPLACE_WITH_NFS_SERVER
test "${NFS_PATH}" != REPLACE_WITH_EXPORT_PATH

sed \
  -e "s/203\\.0\\.113\\.20/${NFS_SERVER}/" \
  -e "s#/exports/idleloom#${NFS_PATH}#" \
  "${IDLELOOM_REPO}/examples/worker/nfs-pv-smoke.example.yaml" \
  > nfs-pv-smoke.yaml

grep -nE 'server:|path:' nfs-pv-smoke.yaml
kube apply -f nfs-pv-smoke.yaml
kube -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/idleloom-nfs-smoke --timeout=10m
kube -n default logs pod/idleloom-nfs-smoke

kube delete -f nfs-pv-smoke.yaml
rm -f nfs-pv-smoke.yaml
```

The log must print `nfs volume mounted`. The reserved example addresses in the
source manifest are documentation placeholders and must be replaced.

## Run Worker container training

```sh
idlectl recipe render train/container-linear-regression@v1 \
  --name worker-train \
  -o yaml > worker-train.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply -f worker-train.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=condition=complete \
  job/worker-train --timeout=5m
kube -n "${IDLELOOM_NAMESPACE}" logs job/worker-train
kube -n "${IDLELOOM_NAMESPACE}" delete -f worker-train.yaml
rm -f worker-train.yaml
```

## Install and verify the Vulkan DRA driver

The development image is not published. Push it to an anonymously readable
test repository that the Worker can pull from. Private registries require the
operator to add an image-pull Secret to the `apple-vulkan-dra-node` ServiceAccount
before the rollout:

```sh
brew install --cask docker
open -a Docker
docker info

export DRA_IMAGE=registry.example.com/your-project/idleloom-vulkan-dra:dev
docker login registry.example.com
docker buildx build \
  --platform linux/arm64 \
  --push \
  -t "${DRA_IMAGE}" \
  "${IDLELOOM_REPO}"

kube apply -k "${IDLELOOM_REPO}/deploy/base"
kube -n kube-system set image \
  daemonset/apple-vulkan-dra-node \
  dra-node="${DRA_IMAGE}"
kube -n kube-system rollout status \
  daemonset/apple-vulkan-dra-node \
  --timeout=10m
kube apply -f "${IDLELOOM_REPO}/deploy/examples/deviceclass.yaml"

kube get resourceslices -o wide
kube get deviceclass/apple-vulkan
```

Run the low-level allocation smoke test:

```sh
kube -n default apply -f "${IDLELOOM_REPO}/deploy/examples/resourceclaim.yaml"
kube -n default apply -f "${IDLELOOM_REPO}/deploy/examples/pod.yaml"
kube -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/apple-vulkan-smoke --timeout=10m
kube -n default logs pod/apple-vulkan-smoke

kube -n default delete -f "${IDLELOOM_REPO}/deploy/examples/pod.yaml"
kube -n default delete -f "${IDLELOOM_REPO}/deploy/examples/resourceclaim.yaml"
```

## Run Worker Vulkan batch inference

```sh
idlectl recipe render infer/llama-vulkan@v1 \
  --name worker-infer \
  -o yaml > worker-infer.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply -f worker-infer.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=condition=complete \
  job/worker-infer --timeout=30m
kube -n "${IDLELOOM_NAMESPACE}" logs job/worker-infer
kube -n "${IDLELOOM_NAMESPACE}" delete -f worker-infer.yaml
rm -f worker-infer.yaml
```

The log must list a `Virtio-GPU Venus` Vulkan device before generation starts.

## Run Worker Vulkan serving

```sh
openssl rand -hex 32 | \
  kube -n "${IDLELOOM_NAMESPACE}" create secret generic worker-serve-auth \
    --from-file=api-key=/dev/stdin

cat > worker-serve-values.yaml <<EOF
namespace: ${IDLELOOM_NAMESPACE}
apiKeySecret: worker-serve-auth
EOF

idlectl recipe render serve/llama-vulkan@v1 \
  --name worker-serve \
  --values worker-serve-values.yaml \
  -o yaml > worker-serve.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply -f worker-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" rollout status \
  deployment/worker-serve --timeout=30m
```

In one terminal:

```sh
kube -n "${IDLELOOM_NAMESPACE}" port-forward service/worker-serve 8080:8080
```

In another terminal:

```sh
API_KEY="$(
  kube -n "${IDLELOOM_NAMESPACE}" get secret worker-serve-auth \
    -o jsonpath='{.data.api-key}' | openssl base64 -d -A
)"

STATUS="$(
  curl --silent --output /dev/null --write-out '%{http_code}' \
    http://127.0.0.1:8080/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -d '{"model":"qwen3-0.6b","messages":[{"role":"user","content":"unauthenticated probe"}],"max_tokens":1}'
)"
test "${STATUS}" = 401

curl --fail-with-body http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer ${API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen3-0.6b","messages":[{"role":"user","content":"Why is idle compute useful?"}],"max_tokens":64}'
```

Stop port-forward, then clean up:

```sh
kube -n "${IDLELOOM_NAMESPACE}" delete -f worker-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" delete secret worker-serve-auth
rm -f worker-serve.yaml worker-serve-values.yaml
```

## Stop, restart, and recover the Worker

The default state file is `~/.idleloom/state.json`. The default runtime
directory is `~/.idleloom/runtimes/<node-name>`.

```sh
idleloom status
idleloom stop
idleloom start
idleloom status
```

When `init` is interrupted after VM creation, inspect the default state and
resume the saved enrollment phase:

```sh
test -f "${HOME}/.idleloom/state.json"
idleloom status
idleloom start --timeout 10m
```

If `init` used `--state /absolute/path/state.json`, pass the same path to every
lifecycle command:

```sh
idleloom status --state /absolute/path/state.json
idleloom start --state /absolute/path/state.json --timeout 10m
idleloom delete --state /absolute/path/state.json
```

Useful runtime logs are:

```text
~/.idleloom/runtimes/<node-name>/serial.log
~/.idleloom/runtimes/<node-name>/krunkit.log
~/.idleloom/runtimes/<node-name>/krunkit-launch.log
~/.idleloom/runtimes/<node-name>/gvproxy-launch.log
~/.idleloom/state.json.maintainer.log
```

Use `idleloom delete` when enrollment cannot be resumed. Use `--local-only`
only when the cluster is unavailable; the retained state allows a later normal
delete to remove the stale Node and network Lease.

## Delete the Worker and DRA resources

Delete user workloads first. Worker stop and delete refuse active non-DaemonSet
Pods unless explicitly forced.

```sh
kube delete deviceclass apple-vulkan --ignore-not-found
kube delete -k "${IDLELOOM_REPO}/deploy/base" --ignore-not-found

idleloom delete
```

Idleloom never uninstalls the shared WireKube deployment. Remove WireKube only
after confirming that no other peer or node depends on it.

# Optional full lab uninstall

Ordinary `idlectl delete` and `idleloom delete` intentionally preserve shared
cluster APIs, WireKube, downloaded model assets, and host package installations.
Use this section only when the cluster and Mac were dedicated to the evaluation
and every Idleloom host, Worker, and workload has already been deleted.

## Remove cluster-wide Idleloom APIs

Prove that no managed execution remains before deleting CRDs:

```sh
if kube api-resources | grep -q idleloomworkloads; then
  test -z "$(kube get idleloomworkloads -A -o name)"
  test -z "$(kube get idleloomhosts -A -o name)"
  test -z "$(kube get idleloomworkloadassignments -A -o name)"
fi
test -z "$(kube get nodes -l idleloom-worker=true -o name)"
test -z "$(kube -n kube-system get leases \
  -l app.kubernetes.io/managed-by=idleloom \
  -o name)"
```

If any command prints a resource or returns a failed `test`, stop and use the
owning lifecycle command. Do not bypass host or network-lease ownership checks
with a blind delete.

After the preconditions pass, remove projection policy, Native APIs, shared
Worker bootstrap bindings, and any remaining development DRA objects:

```sh
kube delete -k "${IDLELOOM_REPO}/deploy/native/projection" --ignore-not-found
kube delete -k "${IDLELOOM_REPO}/deploy/native" --ignore-not-found
kube delete clusterrolebinding \
  idleloom:node-autoapprove-bootstrap \
  idleloom:node-autoapprove-rotation \
  idleloom:node-bootstrapper \
  --ignore-not-found
kube delete deviceclass apple-vulkan --ignore-not-found
kube delete -k "${IDLELOOM_REPO}/deploy/base" --ignore-not-found
```

Deleting the Native CRDs also deletes built-in and operator-created
`IdleloomModel` catalog objects. It must never be done while another Native host
uses the cluster.

## Optionally uninstall WireKube

WireKube is a separate shared product. Keep it when any remaining node or peer
depends on the mesh:

```sh
kube get wirekubepeers -o wide
```

For a dedicated lab, download the same v0.0.15 `wirekubectl` described earlier,
verify that no required peer remains, then remove workloads and purge its CRDs
and custom resources:

```sh
"${WIREKUBECTL}" uninstall \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --purge \
  --confirm-purge \
  --yes
```

Omit this command on a shared cluster.

## Optionally remove local caches, models, and packages

Run these commands only after both host lifecycle commands have completed. If
Native join used a custom `--root`, replace the default before removal:

```sh
export IDLELOOM_ROOT="${IDLELOOM_ROOT:-/var/tmp/idleloom}"

test ! -e "${HOME}/.idleloom/state.json"
test ! -e "${HOME}/Library/Application Support/idleloom/native/services.json"

rm -rf "${IDLELOOM_ROOT}"
rm -rf "${HOME}/Library/Caches/idleloom"
rm -rf "${HOME}/Library/Application Support/idleloom"
rm -rf "${HOME}/.idleloom"
rm -f "${HOME}/.local/bin/wirekubectl"
rm -f /tmp/wirekubectl-darwin-arm64 /tmp/wirekubectl-checksums.txt

test -z "$(git -C "${IDLELOOM_REPO}" status --short)"
rm -rf "${IDLELOOM_REPO}"
```

The normal Ollama store and standalone GGUF source are operator-owned. Remove
them only when they were installed solely for this evaluation:

```sh
ollama rm qwen3.5:9b
brew uninstall inerplat/tap/idleloom

# Optional dependency cleanup; omit anything used by another project.
brew uninstall llama.cpp python@3.12 krunkit go jq kubectl git
brew uninstall --cask ollama docker
```

Delete the pushed DRA image with the registry's own lifecycle command. Registry
deletion is provider-specific and cannot be performed safely from a generic
Kubernetes manifest.

## Verify the final inventory

```sh
for resource in \
  pods services deployments daemonsets jobs secrets endpointslices \
  persistentvolumeclaims resourceclaims resourceclaimtemplates
do
  kube get "${resource}" -A \
    -l app.kubernetes.io/managed-by=idleloom \
    -o name 2>/dev/null
done

kube get persistentvolumes,deviceclasses,resourceslices \
  -l app.kubernetes.io/managed-by=idleloom \
  -o name 2>/dev/null
kube get deviceclass apple-vulkan -o name 2>/dev/null || true
kube get resourceslices -o name 2>/dev/null | grep -E 'apple-vulkan|gpu\.apple' || true
kube get crd,clusterrole,clusterrolebinding,validatingadmissionpolicy,validatingadmissionpolicybinding \
  -o name 2>/dev/null | grep -i idleloom || true
kube -n kube-system get leases \
  -l app.kubernetes.io/managed-by=idleloom \
  -o name
launchctl list | grep -i idleloom || true
```

All commands above should be empty. WireKube resources should remain when the
optional WireKube purge was intentionally skipped.

# Troubleshooting

## Native workload remains Scheduling

```sh
idlectl get hosts \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"
idlectl get workload/WORKLOAD -o yaml \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
kube get idleloomworkloadassignments -A -o wide
```

Check that a host is Ready, has enough unified memory, advertises the exact
catalog model, and satisfies connected serving requirements. Mutable Ollama
digest mismatches and missing GGUF files intentionally prevent scheduling.

## Native service process is unhealthy

```sh
launchctl list | grep -i idleloom
tail -n 200 "${HOME}/Library/Application Support/idleloom/native/logs/controller.log"
tail -n 200 "${HOME}/Library/Application Support/idleloom/native/logs/agent.log"
tail -n 200 "${HOME}/Library/Application Support/idleloom/native/logs/projection.log"
```

The root WireKube link log is under
`/Library/Application Support/Idleloom/Native/io.idleloom.link.<host>/link.log`
and requires administrator access. Do not delete launchd files manually; use
`idlectl delete host/...` so peer and receipt ownership checks run.

## Connected Native logs are temporarily unavailable

Host readiness and projection address publication converge independently. If
the first `idlectl logs` call reports that the projected Node has no preferred
address, inspect the projection and retry after its InternalIP appears:

```sh
kube get nodes,pods -A -l native.idleloom.io/projection=true -o wide
idlectl logs -f workload/WORKLOAD \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

Do not use `kubectl exec` or port-forward as a fallback; the Native projection
implements logs only.

## Worker Pod remains Pending

```sh
kube describe pod POD -n "${IDLELOOM_NAMESPACE}"
kube get node -l idleloom-worker=true -o wide
kube get resourceslices -o wide
kube -n kube-system get daemonset apple-vulkan-dra-node
```

Check the Worker taint toleration, image-pull access, CNI readiness, DRA API
version, DeviceClass, and ResourceSlice publication.

## Cleanup inventory

Before and after a test, list resources managed by the examples:

```sh
kube get idleloomworkloads,idleloomworkloadassignments -A
kube get idleloomhosts -A
kube get idleloommodels
for resource in \
  jobs deployments pods services secrets endpointslices \
  persistentvolumeclaims resourceclaims resourceclaimtemplates
do
  kube get "${resource}" -A \
    -l app.kubernetes.io/managed-by=idleloom \
    -o wide 2>/dev/null
done
kube get persistentvolumes,deviceclasses,resourceslices -o wide 2>/dev/null
kube get wirekubepeers
kube get nodes -l idleloom-worker=true
```

Do not delete shared WireKube resources as part of ordinary Idleloom workload
cleanup.
