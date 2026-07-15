# Ollama

Idleloom runs a private Ollama process for each assignment while reusing the
operator-managed local model store. It never invokes `ollama pull` implicitly.

## Install and pin the model

```sh
brew install --cask ollama
brew install jq
open -a Ollama

export OLLAMA_MODEL=qwen3.5:9b
ollama pull "${OLLAMA_MODEL}"

export OLLAMA_INFO="$(
  curl --fail --silent http://127.0.0.1:11434/api/tags | \
    jq -c --arg model "${OLLAMA_MODEL}" '.models[] | select(.name == $model)'
)"
export OLLAMA_DIGEST="$(printf '%s' "${OLLAMA_INFO}" | jq -r '.digest')"
export OLLAMA_SIZE="$(printf '%s' "${OLLAMA_INFO}" | jq -r '.size')"

printf 'model=%s\ndigest=%s\nsizeBytes=%s\n' \
  "${OLLAMA_MODEL}" "${OLLAMA_DIGEST}" "${OLLAMA_SIZE}"
```

The built-in `qwen3-5-9b-ollama` catalog expects:

```text
sha256:6488c96fa5faab64bb65cbd30d4289e20e6130ef535a93ef9a49f42eda893ea7
```

Use a separate immutable `IdleloomModel` and recipe values when the local digest
differs. Do not weaken digest verification. The complete custom catalog schema
is in the [recipe reference](../recipes.md#native-metal-ollama-gguf-batch-inference).

## Batch inference

```sh
idlectl recipe render infer/ollama-gguf@v1 \
  --name native-ollama-infer \
  -o yaml > native-ollama-infer.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply -f native-ollama-infer.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait \
  --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/native-ollama-infer \
  --timeout=20m

idlectl logs workload/native-ollama-infer \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

For thinking models, a small `maxTokens` value can be consumed before visible
text is produced. Use a larger bounded value, such as 512, when validating that
the response body is non-empty.

## Serving

```sh
idlectl recipe render serve/ollama-gguf@v1 \
  --name native-ollama-serve \
  -o yaml > native-ollama-serve.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply -f native-ollama-serve.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=condition=Ready \
  idleloomworkload/native-ollama-serve --timeout=20m

kube apply -f "${IDLELOOM_REPO}/examples/native/serve-ollama-client.yaml"
kube -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/native-ollama-serve-client --timeout=10m
kube -n default logs pod/native-ollama-serve-client
```

The client first asserts HTTP 401 without credentials, then mounts the
controller-generated Secret and makes an authenticated request. See
[Native Serving](native-serving.md).

## Cleanup

```sh
kube -n default delete pod/native-ollama-serve-client --ignore-not-found
kube -n "${IDLELOOM_NAMESPACE}" delete -f native-ollama-serve.yaml --ignore-not-found
kube -n "${IDLELOOM_NAMESPACE}" delete -f native-ollama-infer.yaml --ignore-not-found
rm -f native-ollama-serve.yaml native-ollama-infer.yaml
```

Do not delete the built-in catalog object. Remove a custom catalog only after
all dependent workloads are gone.
