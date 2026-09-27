# Troubleshooting

Start with the owner-facing CLI before inspecting generated Kubernetes or launchd objects.

## Native workload remains Scheduling

```sh
idlectl get hosts \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"
idlectl get workload/WORKLOAD -o yaml \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
kubectl get idleloomworkloadassignments -A -o wide
```

Check host readiness, free unified memory, exact model catalog capability, and connected serving requirements. An Ollama digest mismatch or missing GGUF file intentionally prevents scheduling.

Only one Native workload runs on a host at a time. Remove a completed or stale workload before expecting another assignment.

## Native service process is unhealthy

```sh
launchctl list | grep -i idleloom
tail -n 200 "${HOME}/Library/Application Support/idleloom/native/logs/controller.log"
tail -n 200 "${HOME}/Library/Application Support/idleloom/native/logs/agent.log"
tail -n 200 "${HOME}/Library/Application Support/idleloom/native/logs/projection.log"
```

The root link log is stored under `/Library/Application Support/Idleloom/Native/io.idleloom.link.<host>/link.log` and requires administrator access. Do not delete launchd files manually; use `idlectl delete host/...` so peer and receipt ownership checks run.

## Connected Native logs are unavailable

Projection readiness and address publication converge independently:

```sh
kubectl get nodes,pods -A -l native.idleloom.io/projection=true -o wide
kubectl get wirekubepeers -o wide
idlectl logs -f workload/WORKLOAD \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"
```

Wait for the projected Node InternalIP and WireKube peer. Do not use `exec` or port-forward as a fallback; Native projection implements logs only. API-only workloads use `idlectl logs --local` after completion.

## Native serving is unreachable

```sh
kubectl -n "${IDLELOOM_NAMESPACE}" get idleloomworkload/WORKLOAD -o wide
kubectl -n "${IDLELOOM_NAMESPACE}" get service,endpointSlice
wirekubectl doctor \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"
```

The EndpointSlice must contain the connected Native host's WireKube address. Run clients on a real Linux node with a ready WireKube agent. The API Service proxy and `kubectl port-forward` do not support the Native endpoint.

## Worker Pod remains Pending

```sh
kubectl describe pod POD -n "${IDLELOOM_NAMESPACE}"
kubectl get node -l idleloom-worker=true -o wide
kubectl get resourceslices -o wide
kubectl -n kube-system get daemonset apple-vulkan-dra-node
```

Check the dedicated taint toleration, image-pull access, CNI readiness, DRA API version, DeviceClass, and ResourceSlice publication.

## Worker remains registered but NotReady

An intentional `create worker --wait=false` leaves the Node cordoned in phase `registered`. Complete the cluster-side CNI or WireKube work, then run:

```sh
idlectl start worker --timeout 10m
idlectl status
kubectl get node -l idleloom-worker=true -o wide
```

Do not use deferred readiness to declare a broken Node healthy.

## In-place worker preflight fails

```sh
sudo idlectl create worker NAME --dry-run --kubeconfig "${IDLELOOM_KUBECONFIG}"
```

Preflight refuses a host that cannot carry a kubelet, and names the remedy. The two it catches most often, both of which a WSL2 host gets wrong by default:

- **No cgroup v2.** kubelet's systemd cgroup driver needs the unified hierarchy. Left hybrid, Cilium's socket load balancer cannot attach and every Pod-to-ClusterIP call times out with nothing pointing at the cause. On Windows add `kernelCommandLine=cgroup_no_v1=all` under `[wsl2]` in `%UserProfile%\.wslconfig` and run `wsl --shutdown`.
- **No `dummy` driver.** The worker's mesh address lives on a dummy link before kubelet starts. Loopback is not a substitute: Cilium treats addresses on `lo` as NodePort-capable and would SNAT service traffic to an address no peer can route back to.

## In-place worker registers with the wrong address

```sh
kubectl get node NAME -o jsonpath='{.status.addresses}'
kubectl get wirekubepeer NAME -o jsonpath='{.spec.allowedIPs}'
```

The node's `InternalIP` must equal the first entry of the peer's `allowedIPs`. A host address there instead (`10.0.2.2`, `192.168.x.x`) means the worker installed a bundle that let kubelet detect its own address. Confirm the address is held locally, then re-enrol:

```sh
systemctl status idleloom-node-address.service
ip -o addr show dev idleloom0
cat /etc/default/idleloom-kubelet
```

Do not assume the address is the one the node name hashes to. On a mesh with `spec.addressAllocation: allocator` it is whatever the claim settled on, which differs whenever the hashed address was already taken:

```sh
kubectl -n wirekube-system get leases -l wirekube.io/claim=address \
  -o custom-columns='ADDRESS:.metadata.annotations.wirekube\.io/address,HOLDER:.spec.holderIdentity'
```

See [Mesh Addressing](../mesh-addressing.md).

## Enrollment refuses the mesh address

Two node names can reduce to the same mesh address. On a mesh that does not arbitrate addresses there is no second choice, so enrollment stops rather than leave two peers advertising one `/32`. Either enrol under a different node name, or switch the mesh to `spec.addressAllocation: allocator` and let WireKube assign a free address. The switch has an ordering requirement; see [Mesh Addressing](../mesh-addressing.md).

An `every address in the WireKube mesh CIDR is already claimed` failure is different: the pool is full, nothing frees up on its own, and `spec.meshCIDR` has to be widened.

## Worker enrols but the WireKube peer never connects

```sh
kubectl get wirekubepeer NAME -o jsonpath='{.status.natType}{"\n"}'
kubectl logs -n wirekube-system -l app.kubernetes.io/name=wirekube --tail=50
```

A worker behind symmetric NAT cannot establish a direct path, so the relay is the only route and an unreachable relay means no connectivity at all. `no relay connected` in the agent log with `dial tcp <relay>: i/o timeout` is egress filtering rather than a WireKube fault. Check the relay port specifically, since a host that reaches everything else may still have that one port blocked:

```sh
nc -vz RELAY_HOST 3478
```

Double NAT makes this likely: a Windows host adds its own NAT layer unless WSL2 runs with `networkingMode=mirrored`. With the relay reachable the worker still connects, because WireKube keeps it warm and falls back to it, but without the relay a symmetric-NAT worker has no path.

## Inventory before cleanup

```sh
kubectl get idleloomworkloads,idleloomworkloadassignments -A
kubectl get idleloomhosts -A
kubectl get idleloommodels
kubectl get wirekubepeers
kubectl get nodes -l idleloom-worker=true
kubectl -n wirekube-system get leases -l wirekube.io/claim=address
```

Do not delete shared WireKube resources as part of ordinary Idleloom cleanup. A worker's address claim is released by `idlectl delete worker`; a claim left behind by a worker that is already gone is collected by WireKube.
