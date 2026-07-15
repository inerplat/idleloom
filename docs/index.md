# Idleloom

Idleloom turns an idle Apple Silicon Mac into Kubernetes-accessible compute.
Choose the execution mode per workload instead of forcing every job through the
same runtime.

## Choose an execution mode

| Use case | Mode |
| --- | --- |
| MLX, Ollama, llama.cpp, or direct Apple Metal | [Native Metal](getting-started/native-metal.md) |
| OCI containers, standard Pods, volumes, `exec`, or port-forward | [Linux Worker](getting-started/linux-worker.md) |

Native Metal runs restricted macOS processes and projects observability objects
into Kubernetes. Linux Worker starts a krunkit VM and joins it as a real kubelet
node. Both modes use WireKube when the cluster must reach compute behind NAT.

## Start here

1. [Prepare the common shell](getting-started/index.md).
2. [Compare the two modes](getting-started/choose-mode.md).
3. Follow either the [Native Metal](getting-started/native-metal.md) or
   [Linux Worker](getting-started/linux-worker.md) quick start.
4. Open only the guide for the workload you want to run.

## Workload guides

- [Native shells](guides/native-shell.md)
- [MLX training, inference, and serving](guides/native-mlx.md)
- [Ollama GGUF inference and serving](guides/native-ollama.md)
- [Standalone llama.cpp GGUF inference and serving](guides/native-llama-cpp.md)
- [Native serving connectivity and authentication](guides/native-serving.md)
- [Worker storage](guides/worker-storage.md)
- [Worker Vulkan inference and serving](guides/worker-vulkan.md)

## Operations and reference

- [Lifecycle and recovery](operations/lifecycle.md)
- [Safe cleanup](operations/cleanup.md)
- [Troubleshooting](operations/troubleshooting.md)
- [Recipe catalog and manifest contract](recipes.md)
- [Worker bootstrap security contract](worker-bootstrap.md)

Idleloom is alpha software. Review every cluster-scoped RBAC, privileged
workload, public relay entry point, and host shell boundary before approval.
