# Lifecycle

Native Metal and Linux Worker have separate lifecycle owners. Use their CLI
commands rather than deleting generated cluster or launchd objects manually.

## Native host

Inspect and delete a connected host:

```sh
idlectl get host/"${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"

idlectl delete host/"${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"
```

Delete workloads before deleting their host. Connected deletion asks for macOS
administrator authorization to remove the root-owned link helper. It removes
the host peer and local credentials but preserves the shared WireKube
installation.

## Native API-only mode

API-only is a separate immutable enrollment. It supports batch workloads when
the cluster cannot reach the Mac, but projected logs and serving are not
available:

```sh
export IDLELOOM_HOST=evening-api

idlectl join "${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --link api-only \
  --projection=false

idlectl recipe render train/mlx-linear-regression@v1 \
  --name linear-api \
  -o yaml > linear-api.yaml
kube -n "${IDLELOOM_NAMESPACE}" apply -f linear-api.yaml
kube -n "${IDLELOOM_NAMESPACE}" wait \
  --for=jsonpath='{.status.phase}'=Succeeded \
  idleloomworkload/linear-api \
  --timeout=15m

idlectl logs --local workload/linear-api \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

Local API-only logs are completed snapshots and do not support follow.

## Worker start and stop

The default state file is `~/.idleloom/state.json` and the default runtime root
is `~/.idleloom/runtimes/<node-name>`:

```sh
idlectl worker status
idlectl worker stop
idlectl worker start
idlectl worker status
```

Worker stop and delete refuse active non-DaemonSet Pods unless explicitly
forced. Remove user workloads before stopping a production Worker.

## Resume interrupted enrollment

```sh
test -f "${HOME}/.idleloom/state.json"
idlectl worker status
idlectl worker start --timeout 10m
```

An intentional `init --wait=false` records phase `registered`, not
`enrolling`. It completes TLS bootstrap and leaves the Node cordoned. After the
cluster-side CNI and WireKube path are ready, `idlectl worker start` performs
strict readiness checks, records phase `ready`, and uncordons the Node.

Pass a custom state path to every lifecycle command:

```sh
idlectl worker status --state /absolute/path/state.json
idlectl worker start --state /absolute/path/state.json --timeout 10m
idlectl worker delete --state /absolute/path/state.json
```

## Worker logs

```text
~/.idleloom/runtimes/<node-name>/serial.log
~/.idleloom/runtimes/<node-name>/krunkit.log
~/.idleloom/runtimes/<node-name>/krunkit-launch.log
~/.idleloom/runtimes/<node-name>/gvproxy-launch.log
~/.idleloom/state.json.maintainer.log
```

Use `idlectl worker delete` when enrollment cannot be resumed. `--local-only`
is an emergency path for an unavailable cluster; retained state allows a later
normal delete to remove stale Node and network Lease objects.
