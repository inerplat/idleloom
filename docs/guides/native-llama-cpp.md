# llama.cpp Metal

Use this path for a standalone GGUF file managed outside Ollama. Idleloom pins
the filename, byte size, and SHA-256 digest and rejects CPU fallback or partial
Metal layer placement.

## Install the runtime and model

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
```

Idleloom does not download this file or follow paths outside the managed GGUF
directory.

## Create the immutable catalog entry

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

Increase the memory reservation for larger models or context windows.

## Batch inference

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
```

The log must confirm full Metal offload before generation.

## Serving

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
```

Read [Native Serving](native-serving.md) for the shared API and connectivity
contract.

## Cleanup

```sh
kube -n default delete pod/native-llama-serve-client --ignore-not-found
kube -n "${IDLELOOM_NAMESPACE}" delete -f native-llama-serve.yaml --ignore-not-found
kube -n "${IDLELOOM_NAMESPACE}" delete -f native-llama-infer.yaml --ignore-not-found
kube delete idleloommodel local-gguf --ignore-not-found
rm -f native-llama-serve.yaml native-llama-infer.yaml llama-cpp-model.yaml
```

Deleting the catalog object does not delete the GGUF file. Remove the file only
when no remaining workload or operator needs it.
