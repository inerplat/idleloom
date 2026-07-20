# Idleloom

Use idle Apple Silicon Macs as Kubernetes compute.

Idleloom provides two execution modes with different compatibility and security
boundaries:

| Mode | Best for | Kubernetes contract |
| --- | --- | --- |
| Native Metal | MLX, Ollama, llama.cpp, macOS tools | Restricted macOS workloads with observability projection |
| Linux Worker | OCI containers, Pods, volumes, `exec`, port-forward | A real ARM64 kubelet Node in a krunkit VM |

**[Documentation](https://inerplat.github.io/idleloom/)**

## Choose a mode

Choose Native Metal when direct Metal access is the priority. Choose Linux
Worker when the workload expects ordinary Kubernetes Pods and storage.

| Capability | Native Metal | Linux Worker |
| --- | --- | --- |
| Direct Apple Metal | Yes | No |
| MLX, local Ollama, llama.cpp on macOS | Yes | No |
| Standard OCI containers | No | Yes |
| Standard Pod networking and volumes | No | Yes |
| Kubernetes logs | Connected mode | Yes |
| `exec`, attach, port-forward | No | Yes |
| Apple GPU API | Metal | Vulkan through krunkit and DRA |

Native projection is observability-only. It publishes ephemeral Node and Pod
objects for status and logs; it is not a kubelet and cannot run arbitrary Pods.

In the CLI, Native Metal manages `host` and `workload` resources, the Linux
Worker path manages the `worker` resource, and the verbs `get`, `delete`, and
`status` are shared across both.

## Native Metal quick start

Install the CLI and join the current Mac:

```sh
brew install kubectl python@3.12
brew install inerplat/tap/idleloom

idlectl join evening-mac \
  --kubeconfig ~/.kube/config \
  --context my-cluster

idlectl get host/evening-mac \
  --kubeconfig ~/.kube/config \
  --context my-cluster
```

Connected mode installs or reuses WireKube, supports projected logs, and
enables cluster-private Native serving. When the cluster needs a non-default
relay topology, install WireKube independently first:

```sh
brew install inerplat/tap/wirekube
wirekubectl install --dry-run \
  --kubeconfig ~/.kube/config \
  --context my-cluster
```

Run a sandboxed shell workload:

```sh
idlectl run native-shell \
  --kubeconfig ~/.kube/config \
  --context my-cluster \
  --shell 'uname -m; sw_vers -productVersion' \
  --isolation sandbox \
  --network none

idlectl logs -f workload/native-shell \
  --kubeconfig ~/.kube/config \
  --context my-cluster
```

Continue with the [Native Metal guide](docs/getting-started/native-metal.md).

## Linux Worker quick start

The worker commands ship with the brew-installed `idlectl`:

```sh
brew install kubectl
brew tap libkrun/krun
brew install krunkit
brew install inerplat/tap/wirekube
brew install inerplat/tap/idleloom
```

The development Vulkan driver (`bin/idleloom-vulkan-dra`) still builds from
source with `make build`, which also produces a from-source `bin/idlectl`:

```sh
brew install go
git clone https://github.com/inerplat/idleloom.git
cd idleloom
make build
export PATH="${PWD}/bin:${PATH}"
```

Install or reuse WireKube, then preview and enroll the Worker:

```sh
wirekubectl install --dry-run \
  --node-addresses internal-ip \
  --kubeconfig ~/.kube/config \
  --context my-cluster

wirekubectl install \
  --node-addresses internal-ip \
  --kubeconfig ~/.kube/config \
  --context my-cluster

idlectl create worker evening-worker --dry-run \
  --kubeconfig ~/.kube/config \
  --context my-cluster

idlectl create worker evening-worker \
  --kubeconfig ~/.kube/config \
  --context my-cluster
```

Managed clusters that need operator-side CNI or WireKube work after secure
registration can use `idlectl create worker NAME --wait=false`. The Node
remains cordoned in phase `registered`; run `idlectl start worker` only after
the network path is ready.

Continue with the [Linux Worker guide](docs/getting-started/linux-worker.md).

## Recipes

Recipes render reviewable Kubernetes YAML rather than creating another run API:

```sh
idlectl recipe list
idlectl recipe show train/mlx-linear-regression@v1
idlectl recipe render train/mlx-linear-regression@v1 \
  --name example-run \
  -o yaml
```

The current catalog covers:

- Native MLX training, batch inference, and serving
- Native Ollama GGUF batch inference and serving
- Native llama.cpp GGUF batch inference and serving
- Worker container training
- Worker llama.cpp Vulkan batch inference and serving

See the [recipe reference](docs/recipes.md) for provenance, digest, manifest,
metrics, artifact, and result contracts.

## Documentation

- [Start here](docs/getting-started/index.md)
- [Choose a mode](docs/getting-started/choose-mode.md)
- [Native Metal](docs/getting-started/native-metal.md)
- [Linux Worker](docs/getting-started/linux-worker.md)
- [Native shell boundaries](docs/guides/native-shell.md)
- [MLX](docs/guides/native-mlx.md)
- [Ollama](docs/guides/native-ollama.md)
- [llama.cpp Metal](docs/guides/native-llama-cpp.md)
- [Native serving](docs/guides/native-serving.md)
- [Worker storage](docs/guides/worker-storage.md)
- [Worker Vulkan](docs/guides/worker-vulkan.md)
- [Lifecycle and recovery](docs/operations/lifecycle.md)
- [Safe cleanup](docs/operations/cleanup.md)
- [Troubleshooting](docs/operations/troubleshooting.md)

## Build and test

```sh
make build
make test
make vet
```

Native Metal requires Apple Silicon. The Linux Worker requires macOS 14 or
later and krunkit. MLX recipes currently require macOS 26 or later and Python
3.12. Vulkan DRA requires Kubernetes 1.35 or later with `resource.k8s.io/v1`.

## Current limitations

- Native Metal is a restricted process runtime, not a container runtime.
- Native projection supports logs but not `exec`, attach, or port-forward.
- Native serving requires a connected WireKube host and a real Linux client
  node on the mesh.
- Worker Vulkan is experimental and should be treated as single-tenant.
- Windows Hyper-V enrollment is not part of the current release.

## License

Idleloom is licensed under the [Apache License 2.0](LICENSE).
