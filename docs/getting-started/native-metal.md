# Native Metal Quick Start

This path enrolls the current Mac as restricted Native Metal compute. Complete
[Start Here](index.md) first.

## Install Native dependencies

```sh
brew install python@3.12
brew install inerplat/tap/idleloom

idlectl version
idlectl recipe list
```

The recipe list must include the Native MLX, Ollama, llama.cpp, shell, and
serving paths documented in the [recipe reference](../recipes.md).

## Decide how WireKube is installed

The default connected join needs WireKube so the cluster can retrieve projected
logs and reach Native serving endpoints.

- An existing compatible WireKube installation is reused.
- Interactive `idlectl join` can show and execute the dependency installation
  plan when WireKube is absent.
- Install WireKube independently when the cluster requires NodePort, WSS, an
  external relay, or separate lifecycle ownership.

Independent installation uses the public Homebrew tap:

```sh
brew install inerplat/tap/wirekube

wirekubectl install \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --dry-run
```

Follow the [WireKube relay guide](https://inerplat.github.io/wirekube/guides/relay-entrypoints/)
before choosing a public LoadBalancer, NodePort, or WSS entry point.

## Join the Mac

```sh
export IDLELOOM_HOST=evening-mac

idlectl join "${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"

idlectl get host/"${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"
```

Connected mode is ready when the host reports `READY=True` and
`CONNECTED=True`. Non-interactive dependency installation requires explicit
approval:

```sh
idlectl join "${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --install-dependencies \
  --yes
```

Do not run the second command after a successful interactive join. One Mac can
have one local Native enrollment at a time.

## Run a shell smoke test

```sh
idlectl run native-shell \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}" \
  --shell 'uname -m; sw_vers -productVersion; id' \
  --isolation sandbox \
  --network none

idlectl logs -f workload/native-shell \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"

idlectl delete workload/native-shell \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

Shell source is stored in the Kubernetes API. Never place credentials in the
script or prompt.

## Next steps

- [Native shell boundaries](../guides/native-shell.md)
- [MLX workloads](../guides/native-mlx.md)
- [Ollama](../guides/native-ollama.md)
- [llama.cpp Metal](../guides/native-llama-cpp.md)
- [Native serving](../guides/native-serving.md)
- [Lifecycle and API-only mode](../operations/lifecycle.md)
