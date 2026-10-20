# Native Serving

Native serving exposes a macOS model process through a selectorless Kubernetes
Service. It is available only when the Native host is connected through
WireKube.

## Requirements

- a Native host with `READY=True` and `CONNECTED=True`
- one real schedulable Linux kubelet node on the WireKube mesh
- the runtime and exact model required by the selected recipe

The Linux node runs the client Pod. The projected Native Node is deliberately
unschedulable.

## Kubernetes objects

Each serving recipe renders:

- an `IdleloomWorkload` in `Server` mode
- a selectorless ClusterIP Service
- an EndpointSlice that points at the host's WireKube address after assignment
  readiness

Inspect the endpoint:

```sh
kubectl -n "${IDLELOOM_NAMESPACE}" get service,endpointSlice
```

## Client test

Each client manifest sends one OpenAI-compatible chat request to the Service
and fails if it does not succeed. Available examples are:

```text
examples/native/serve-mlx-client.yaml
examples/native/serve-ollama-client.yaml
examples/native/serve-llama-cpp-client.yaml
```

The client Pod requires affinity to a node that already runs a WireKube agent.
It tolerates Idleloom's default Worker taint.

## Supported API

The host agent relays the WireKube address byte for byte to the runtime's own
OpenAI-compatible server: `mlx_lm.server`, `llama-server`, or Ollama. Idleloom
does not parse or rewrite requests, so the routes, streaming, and a
request's own `max_tokens` are handled by that runtime.

Idleloom starts `llama-server` with a single slot (`--parallel 1`) and Ollama
with `OLLAMA_NUM_PARALLEL=1` and `OLLAMA_MAX_QUEUE=1`. It starts `mlx_lm.server`
with `--decode-concurrency 1`, `--prompt-concurrency 1`, and
`--prompt-cache-size 1`, so it processes one request and keeps one KV cache,
which is the memory that admission charges for. It also sets
`--max-tokens` to the model's `maxContextLength`, so a request that
sends no `max_tokens` is not cut off at the server's built-in default of 512
tokens. That flag limits only the output: unlike the other two runtimes,
`mlx_lm.server` does not bound prompt plus output by the context length, so a
client sending long prompts should set its own `max_tokens`. A request beyond
what the runtime accepts gets that runtime's
own response, because Idleloom neither queues requests nor answers with HTTP
429.

Traffic is encrypted by WireGuard but not authenticated. The endpoint is
cluster-private by design, like an ordinary ClusterIP Service, and Idleloom does
not add application-layer TLS or publish an Ingress. Front the Service with your
own gateway when clients need authentication.

## Kubernetes limitations

The EndpointSlice targets a Mac rather than a Pod. The Kubernetes Service proxy
and `kubectl port-forward` do not expose this endpoint. Run a client Pod on a
real WireKube-connected Linux node.

Native projection implements logs only. `kubectl exec`, attach, and
port-forward intentionally return unsupported responses.

## Observability

```sh
idlectl logs workload/WORKLOAD \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

Confirm runtime verification, Metal acceleration or full offload, and process
startup in the log. Requests are relayed without being logged.
