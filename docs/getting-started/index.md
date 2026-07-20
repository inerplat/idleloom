# Start Here

The guides assume an Apple Silicon Mac and a kubeconfig for an existing
Kubernetes cluster. Start by preparing one shell, then choose a mode.

## Prerequisites

- Homebrew
- an Apple Silicon Mac
- a reachable Kubernetes API
- permission to install Idleloom CRDs and RBAC
- permission to install or reuse WireKube when connected execution is required

Native MLX requires macOS 26 or later and Python 3.12. Linux Worker requires
macOS 14 or later, krunkit, and Kubernetes bootstrap-token support.

## Prepare the CLI and repository

The Homebrew formula provides `idlectl`, including the Native bundle and the
Linux Worker commands. A repository checkout provides reviewed examples
and is required only to build the development Vulkan driver.

```sh
brew install git kubectl
brew install inerplat/tap/idleloom

export IDLELOOM_REPO="${HOME}/src/idleloom"
test -d "${IDLELOOM_REPO}/.git" || \
  git clone https://github.com/inerplat/idleloom.git "${IDLELOOM_REPO}"

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

Replace `my-cluster` before continuing. Keep the exports and `kube` helper in
every terminal that follows a guide.

## Select the next guide

- Use [Native Metal](native-metal.md) for MLX, Ollama, llama.cpp, or macOS shell
  workloads.
- Use [Linux Worker](linux-worker.md) for normal Pods, containers, volumes,
  `exec`, or port-forward.
- Read [Choose a Mode](choose-mode.md) when the boundary is unclear.

In the CLI, Native Metal manages `host` and `workload` resources, the Linux
Worker path manages the `worker` resource, and the verbs `get`, `delete`, and
`status` are shared across both.

Do not execute every guide as one script. Ollama, standalone GGUF, Longhorn,
NFS, and Vulkan are optional paths with separate prerequisites.
