# Choose a Mode

Idleloom exposes two intentionally different Kubernetes execution contracts.

| Requirement | Native Metal | Linux Worker |
| --- | --- | --- |
| Direct MLX or Apple Metal | Yes | No |
| Ollama or llama.cpp on macOS | Yes | No |
| Ordinary OCI containers | No | Yes |
| Standard Pod networking and volumes | No | Yes |
| Kubernetes logs | Connected mode | Yes |
| Kubernetes `exec`, attach, and port-forward | No | Yes |
| hostPath, NFS, and CSI storage | No | Yes |
| Apple GPU interface | Metal | Vulkan through krunkit and DRA |
| Kubernetes Node type | Observability projection | Real kubelet Node |

## Native Metal

Choose Native when the process must run on macOS and use Metal directly. An
Idleloom controller assigns `IdleloomWorkload` objects to an enrolled host. The
projection creates short-lived Node and Pod objects for status and logs, but it
is not a kubelet and does not accept arbitrary Pods.

Connected mode uses WireKube for projected logs and cluster-private serving.
API-only mode supports batch execution when inbound connectivity cannot be
provided, but logs are local and serving is unavailable.

## Linux Worker

Choose Worker when compatibility with Kubernetes Pods matters more than direct
Metal access. Idleloom starts an ARM64 Linux VM with krunkit, performs TLS
bootstrap, and joins it as a dedicated kubelet node. The Node supports normal
container runtimes, networking, volumes, logs, `exec`, and port-forward.

The Apple GPU is exposed through an experimental Vulkan DRA path rather than
Metal. Treat that device boundary as single-tenant alpha functionality.

## Using both

The modes are independent. A cluster may have a Native host for Metal workloads
and a Linux Worker for containers at the same time. Select a recipe by its
`BACKEND` column:

```sh
idlectl recipe list
```
