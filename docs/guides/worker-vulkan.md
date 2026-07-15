# Worker Vulkan

The experimental Vulkan backend exposes the Apple GPU to the Linux Worker
through krunkit and Kubernetes DRA. It is not Metal and is not a reviewed
multi-tenant isolation boundary.

## Build and publish the DRA image

The development image is not published. Push an ARM64 image to a registry the
Worker can pull from:

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
kube -n kube-system set image daemonset/apple-vulkan-dra-node \
  dra-node="${DRA_IMAGE}"
kube -n kube-system rollout status daemonset/apple-vulkan-dra-node \
  --timeout=10m
kube apply -f "${IDLELOOM_REPO}/deploy/examples/deviceclass.yaml"

kube get resourceslices -o wide
kube get deviceclass/apple-vulkan
```

Private registries require an image-pull Secret on the DRA ServiceAccount.

## Allocation smoke test

```sh
kube -n default apply -f "${IDLELOOM_REPO}/deploy/examples/resourceclaim.yaml"
kube -n default apply -f "${IDLELOOM_REPO}/deploy/examples/pod.yaml"
kube -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/apple-vulkan-smoke --timeout=10m
kube -n default logs pod/apple-vulkan-smoke
```

Remove the smoke resources before running a recipe:

```sh
kube -n default delete -f "${IDLELOOM_REPO}/deploy/examples/pod.yaml"
kube -n default delete -f "${IDLELOOM_REPO}/deploy/examples/resourceclaim.yaml"
```

## Batch inference

```sh
idlectl recipe render infer/llama-vulkan@v1 \
  --name worker-infer \
  -o yaml > worker-infer.yaml

kube -n "${IDLELOOM_NAMESPACE}" apply -f worker-infer.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait --for=condition=complete \
  job/worker-infer --timeout=30m
kube -n "${IDLELOOM_NAMESPACE}" logs job/worker-infer
```

The log must list a `Virtio-GPU Venus` Vulkan device before generation.

## Serving

Create an API key Secret and render the serving recipe:

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

Use standard `kubectl port-forward` to reach the Worker Service. First assert
HTTP 401 without the key, then send an authenticated OpenAI-compatible request.
The complete values and request contract are in the
[recipe reference](../recipes.md#linux-worker-vulkan-serving).

## Cleanup

```sh
kube -n "${IDLELOOM_NAMESPACE}" delete -f worker-serve.yaml --ignore-not-found
kube -n "${IDLELOOM_NAMESPACE}" delete -f worker-infer.yaml --ignore-not-found
kube -n "${IDLELOOM_NAMESPACE}" delete secret worker-serve-auth --ignore-not-found
kube delete deviceclass apple-vulkan --ignore-not-found
kube delete -k "${IDLELOOM_REPO}/deploy/base" --ignore-not-found
rm -f worker-serve.yaml worker-serve-values.yaml worker-infer.yaml
```
