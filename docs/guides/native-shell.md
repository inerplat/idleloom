# Native Shells

Native shell workloads run macOS processes under the enrollment's maximum shell
access policy. The script is stored in the Kubernetes API; never include
credentials or tokens.

## Sandboxed shell

The default enrollment permits sandboxed shell workloads:

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
```
Use `--network outbound` only when the process must reach external services.
The sandbox is a macOS process boundary, not a container or multi-tenant VM.

## Trusted host shell

Host isolation is an immutable enrollment capability. Delete and rejoin the
host with explicit maximum access:

```sh
idlectl delete host/"${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"

idlectl join "${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --shell-access host
```

Then request host isolation per workload:

```sh
idlectl run trusted-host-shell \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}" \
  --shell 'pwd; command -v python3; uname -a' \
  --isolation host \
  --network outbound
```

A host enrolled with `host` access can still run sandboxed workloads. A host
enrolled with `sandboxed` access cannot escalate through a workload manifest.

## Cleanup

```sh
idlectl delete workload/native-shell \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"

idlectl delete workload/trusted-host-shell \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```
