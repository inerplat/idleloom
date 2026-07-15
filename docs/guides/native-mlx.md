# MLX Workloads

Use the Native backend for direct Apple Metal execution through MLX. Complete
the [Native Metal quick start](../getting-started/native-metal.md) first.

## Training

```sh
idlectl recipe render train/mlx-linear-regression@v1 \
  --name native-train \
  -o yaml > native-train.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply -f native-train.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait \
  --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/native-train \
  --timeout=15m

idlectl logs workload/native-train \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

The result includes device information, bounded metrics, converged parameters,
and a checkpoint artifact. See the [Native training contract](../native-training.md)
for custom source, metrics, artifacts, retry, and resume behavior.

## Batch inference

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
```

The first execution may download and verify the locked runtime and model. A
successful log ends with the structured result envelope.

## Serving

Serving requires a WireKube-connected host and a real Linux kubelet node on the
same mesh for the client Pod:

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
```

Read [Native Serving](native-serving.md) for the authentication, Service,
EndpointSlice, and connectivity contract.

## Cleanup

```sh
kube -n default delete pod/native-mlx-serve-client --ignore-not-found
kube -n "${IDLELOOM_NAMESPACE}" delete -f native-mlx-serve.yaml --ignore-not-found
kube -n "${IDLELOOM_NAMESPACE}" delete -f native-mlx-infer.yaml --ignore-not-found
kube -n "${IDLELOOM_NAMESPACE}" delete -f native-train.yaml --ignore-not-found
rm -f native-mlx-serve.yaml native-mlx-infer.yaml native-train.yaml
```
