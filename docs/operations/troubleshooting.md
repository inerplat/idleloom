# Troubleshooting

Start with the owner-facing CLI before inspecting generated Kubernetes or
launchd objects.

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

Check host readiness, free unified memory, exact model catalog capability, and
connected serving requirements. An Ollama digest mismatch or missing GGUF file
intentionally prevents scheduling.

Only one Native workload runs on a host at a time. Remove a completed or stale
workload before expecting another assignment.

## Native service process is unhealthy

```sh
launchctl list | grep -i idleloom
tail -n 200 "${HOME}/Library/Application Support/idleloom/native/logs/controller.log"
tail -n 200 "${HOME}/Library/Application Support/idleloom/native/logs/agent.log"
tail -n 200 "${HOME}/Library/Application Support/idleloom/native/logs/projection.log"
```

The root link log is stored under
`/Library/Application Support/Idleloom/Native/io.idleloom.link.<host>/link.log`
and requires administrator access. Do not delete launchd files manually; use
`idlectl delete host/...` so peer and receipt ownership checks run.

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

Wait for the projected Node InternalIP and WireKube peer. Do not use `exec` or
port-forward as a fallback; Native projection implements logs only. API-only
workloads use `idlectl logs --local` after completion.

## Native serving is unreachable

```sh
kubectl -n "${IDLELOOM_NAMESPACE}" get idleloomworkload/WORKLOAD -o wide
kubectl -n "${IDLELOOM_NAMESPACE}" get service,endpointSlice
wirekubectl doctor \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"
```

The EndpointSlice must contain the connected Native host's WireKube address.
Run clients on a real Linux node with a ready WireKube agent. The API Service
proxy and `kubectl port-forward` do not support the Native endpoint.

## Worker Pod remains Pending

```sh
kubectl describe pod POD -n "${IDLELOOM_NAMESPACE}"
kubectl get node -l idleloom-worker=true -o wide
kubectl get resourceslices -o wide
kubectl -n kube-system get daemonset apple-vulkan-dra-node
```

Check the dedicated taint toleration, image-pull access, CNI readiness, DRA API
version, DeviceClass, and ResourceSlice publication.

## Worker remains registered but NotReady

An intentional `create worker --wait=false` leaves the Node cordoned in phase
`registered`. Complete the cluster-side CNI or WireKube work, then run:

```sh
idlectl start worker --timeout 10m
idlectl status
kubectl get node -l idleloom-worker=true -o wide
```

Do not use deferred readiness to declare a broken Node healthy.

## Inventory before cleanup

```sh
kubectl get idleloomworkloads,idleloomworkloadassignments -A
kubectl get idleloomhosts -A
kubectl get idleloommodels
kubectl get wirekubepeers
kubectl get nodes -l idleloom-worker=true
```

Do not delete shared WireKube resources as part of ordinary Idleloom cleanup.
