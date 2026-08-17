# Worker Vulkan

The experimental Vulkan backend exposes the Apple GPU to the Linux Worker
through krunkit and Kubernetes DRA. It is not Metal and is not a reviewed
multi-tenant isolation boundary.

## Budget memory for two copies of the model

Apple Silicon has unified memory, so the GPU's "VRAM" is the Mac's RAM. A model
served with every layer offloaded is therefore charged to the Mac twice over if
the guest also holds it:

- Vulkan device memory, allocated on the host by krunkit on the guest's behalf.
  A 12.5 GB GGUF shows up as roughly 12 GB of `IOAccelerator (graphics)` in
  `footprint -p "$(pgrep -f krunkit)"`. This copy is unavoidable.
- Guest RAM, if the loader keeps the file resident after uploading it. This copy
  is avoidable and the recipes pass `--no-mmap` to avoid it.

Size the serving Pod's `memory` for what the server keeps resident, which is a
couple of GiB, and never for the model. The limit also caps the Pod's page
cache, so a generous value lets the whole GGUF linger in cache long after the
upload and the Mac pays for the model twice. Measure with:

```sh
kubectl -n "${IDLELOOM_NAMESPACE}" exec deploy/RUN -c server -- \
  grep -E '^(anon|file) ' /sys/fs/cgroup/memory.stat
```

`anon` is the real requirement. `file` is reclaimable cache that will grow to
whatever the limit allows.

Worker VM memory is not a second budget to pad: krunkit returns freed guest
pages to macOS, so the host pays for what the guest actually holds, not for
`--memory`. Padding the VM only raises the ceiling the page cache can grow into.

## Install the published driver

Each `v*` release publishes a `linux/arm64` driver image to
`docker.io/inerplat/idleloom-vulkan-dra`. Installing from it needs no Docker on
the Mac and no build:

```sh
kubectl apply -k "${IDLELOOM_REPO}/deploy/published"
kubectl -n kube-system rollout status daemonset/apple-vulkan-dra-node \
  --timeout=10m
kubectl apply -f "${IDLELOOM_REPO}/deploy/examples/deviceclass.yaml"

kubectl get resourceslices -o wide
kubectl get deviceclass/apple-vulkan
```

The overlay tracks `latest`, which follows the most recent non-prerelease tag.
Pin a release before depending on it:

```sh
kustomize edit set image \
  idleloom-vulkan-dra:dev=docker.io/inerplat/idleloom-vulkan-dra:vX.Y.Z
```

See [When the DaemonSet does not schedule](#when-the-daemonset-does-not-schedule)
if no Pod appears. Build the image yourself only when working on the driver, or
when the Worker cannot reach Docker Hub.

## Build the DRA image

Build locally to run an unreleased driver. It must be `linux/arm64` because it
runs inside the Worker VM:

```sh
brew install --cask docker
open -a Docker
docker info

docker build --platform linux/arm64 \
  -t idleloom-vulkan-dra:dev \
  "${IDLELOOM_REPO}"
```

### Load it into the Worker

`idlectl load image` copies a locally built image straight into the Worker's
containerd. This is the shorter path for a personal Worker and needs no
registry, no credentials, and no image override, because `deploy/base` already
names `idleloom-vulkan-dra:dev`:

```sh
idlectl load image idleloom-vulkan-dra:dev
```

Load the image before applying the DaemonSet. A Pod scheduled against an image
the Worker does not have yet fails with `ErrImageNeverPull` or
`ImagePullBackOff` and stays in `CrashLoopBackOff` afterwards; delete the Pod to
retry once the image is in place. Reload after every rebuild, and after any
`idlectl delete worker`, because a fresh VM starts with an empty containerd.

### Or push to a registry

Use a registry instead when several Workers share the image:

```sh
export DRA_IMAGE=registry.example.com/your-project/idleloom-vulkan-dra:dev
docker login registry.example.com
docker buildx build \
  --platform linux/arm64 \
  --push \
  -t "${DRA_IMAGE}" \
  "${IDLELOOM_REPO}"
```

Private registries require an image-pull Secret on the DRA ServiceAccount.

## Install a locally built driver

`deploy/base` names the local `idleloom-vulkan-dra:dev`, so apply it after
loading a locally built image:

```sh
kubectl apply -k "${IDLELOOM_REPO}/deploy/base"
```

Override the image only when it came from another registry:

```sh
kubectl -n kube-system set image daemonset/apple-vulkan-dra-node \
  dra-node="${DRA_IMAGE}"
```

Then wait for the rollout and publish the DeviceClass:

```sh
kubectl -n kube-system rollout status daemonset/apple-vulkan-dra-node \
  --timeout=10m
kubectl apply -f "${IDLELOOM_REPO}/deploy/examples/deviceclass.yaml"

kubectl get resourceslices -o wide
kubectl get deviceclass/apple-vulkan
```

## When the DaemonSet does not schedule

`deploy/base` selects `idleloom-accelerator=apple-vulkan` and tolerates
`idleloom-dedicated`, which are the label and taint `idlectl create worker`
applies, so a Worker joined with the defaults is picked up without further
labelling. A DaemonSet with `DESIRED 0` means the Node carries neither, which
happens when the manifests have been edited locally or the Worker was created
with a different `--taint`. Compare the two before editing anything else:

```sh
kubectl get node WORKER -o jsonpath='{.metadata.labels}{"\n"}{.spec.taints}{"\n"}'
kubectl -n kube-system get daemonset apple-vulkan-dra-node \
  -o jsonpath='{.spec.template.spec.nodeSelector}{"\n"}{.spec.template.spec.tolerations}{"\n"}'
```

The driver name in `deploy/base/daemonset.yaml` must also match the one the
DeviceClass selects on. Both ship as `gpu.apple-vulkan.example`; if you rename
one, rename the other, or `ResourceSlice` objects appear but no claim ever binds.

## Allocation smoke test

```sh
kubectl -n default apply -f "${IDLELOOM_REPO}/deploy/examples/resourceclaim.yaml"
kubectl -n default apply -f "${IDLELOOM_REPO}/deploy/examples/pod.yaml"
kubectl -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/apple-vulkan-smoke --timeout=10m
kubectl -n default logs pod/apple-vulkan-smoke
```

Remove the smoke resources before running a recipe:

```sh
kubectl -n default delete -f "${IDLELOOM_REPO}/deploy/examples/pod.yaml"
kubectl -n default delete -f "${IDLELOOM_REPO}/deploy/examples/resourceclaim.yaml"
```

## Batch inference

```sh
idlectl recipe render infer/llama-vulkan@v1 \
  --name worker-infer \
  -o yaml > worker-infer.yaml

kubectl -n "${IDLELOOM_NAMESPACE}" apply -f worker-infer.yaml
kubectl -n "${IDLELOOM_NAMESPACE}" wait --for=condition=complete \
  job/worker-infer --timeout=30m
kubectl -n "${IDLELOOM_NAMESPACE}" logs job/worker-infer
```

The log must list a `Virtio-GPU Venus` Vulkan device before generation.

## Serving

Create an API key Secret and render the serving recipe:

```sh
openssl rand -hex 32 | \
  kubectl -n "${IDLELOOM_NAMESPACE}" create secret generic worker-serve-auth \
    --from-file=api-key=/dev/stdin

cat > worker-serve-values.yaml <<EOF
namespace: ${IDLELOOM_NAMESPACE}
apiKeySecret: worker-serve-auth
EOF

idlectl recipe render serve/llama-vulkan@v1 \
  --name worker-serve \
  --values worker-serve-values.yaml \
  -o yaml > worker-serve.yaml

kubectl -n "${IDLELOOM_NAMESPACE}" apply -f worker-serve.yaml
kubectl -n "${IDLELOOM_NAMESPACE}" rollout status \
  deployment/worker-serve --timeout=30m
```

Use standard `kubectl port-forward` to reach the Worker Service. First assert
HTTP 401 on `/v1/chat/completions` without the key, then send an authenticated
OpenAI-compatible request. Only the generation endpoints are protected; upstream
llama.cpp leaves `/v1/models` readable, so it answers 200 without a key.
The complete values and request contract are in the
[recipe reference](../recipes.md#linux-worker-vulkan-serving).

## Cleanup

```sh
kubectl -n "${IDLELOOM_NAMESPACE}" delete -f worker-serve.yaml --ignore-not-found
kubectl -n "${IDLELOOM_NAMESPACE}" delete -f worker-infer.yaml --ignore-not-found
kubectl -n "${IDLELOOM_NAMESPACE}" delete secret worker-serve-auth --ignore-not-found
kubectl delete deviceclass apple-vulkan --ignore-not-found
kubectl delete -k "${IDLELOOM_REPO}/deploy/base" --ignore-not-found
rm -f worker-serve.yaml worker-serve-values.yaml worker-infer.yaml
```

Both overlays create the same named objects, so `deploy/base` removes the driver
whichever one installed it.
