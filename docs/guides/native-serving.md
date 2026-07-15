# Native Serving

Native serving exposes a macOS model process through a selectorless Kubernetes
Service. It is available only when the Native host is connected through
WireKube.

## Requirements

- a Native host with `READY=True` and `CONNECTED=True`
- one real schedulable Linux kubelet node on the WireKube mesh
- the runtime and exact model required by the selected recipe
- permission to read the controller-generated client Secret

The Linux node runs the client Pod. The projected Native Node is deliberately
unschedulable.

## Kubernetes objects

Each serving recipe renders:

- an `IdleloomWorkload` in `Server` mode
- a selectorless ClusterIP Service
- a generated `<service-name>-auth` Secret
- an EndpointSlice that points at the host's WireKube address after assignment
  readiness

Inspect the endpoint without printing the API key:

```sh
kube -n "${IDLELOOM_NAMESPACE}" get service,endpointSlice
kube -n "${IDLELOOM_NAMESPACE}" get secret SERVICE-auth \
  -o jsonpath='{.metadata.name}{"\n"}'
```

## Authentication test

The reviewed client manifests perform two requests:

1. an unauthenticated request that must return HTTP 401
2. an authenticated OpenAI-compatible request using the mounted Secret

Available examples are:

```text
examples/native/serve-mlx-client.yaml
examples/native/serve-ollama-client.yaml
examples/native/serve-llama-cpp-client.yaml
```

The client Pod requires affinity to a node that already runs a WireKube agent.
It tolerates Idleloom's default Worker taint.

## Supported API

The current alpha adapter supports:

- non-streaming `POST /v1/chat/completions`
- `GET /v1/models`
- one active generation per host
- up to 512 response tokens

Concurrent requests receive HTTP 429 with `Retry-After: 1`. Traffic is
encrypted by WireGuard and authenticated by the generated API key. Idleloom
does not add application-layer TLS or publish an Ingress.

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

Confirm runtime verification, Metal acceleration or full offload, process
startup, request start, and request completion in the log.
