# Cleanup

Ordinary cleanup removes resources owned by one host or workload. It must not
remove shared WireKube, cluster APIs, models, or package installations.

## Remove Native workloads and host

```sh
idlectl get workloads \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"

idlectl delete workload/WORKLOAD \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  -n "${IDLELOOM_NAMESPACE}"

idlectl delete host/"${IDLELOOM_HOST}" \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}"
```

Remove operator-created model catalog entries only after their workloads are
gone. Keep built-in MLX and Ollama catalog entries.

## Remove Worker resources

Delete user Pods and optional DRA resources before the Worker:

```sh
kube delete deviceclass apple-vulkan --ignore-not-found
kube delete -k "${IDLELOOM_REPO}/deploy/base" --ignore-not-found
idlectl worker delete
```

Idleloom does not uninstall WireKube.

## Full lab uninstall

Use this section only for a dedicated evaluation cluster and Mac. First prove
that no managed execution remains:

```sh
if kube api-resources | grep -q idleloomworkloads; then
  test -z "$(kube get idleloomworkloads -A -o name)"
  test -z "$(kube get idleloomhosts -A -o name)"
  test -z "$(kube get idleloomworkloadassignments -A -o name)"
fi
test -z "$(kube get nodes -l idleloom-worker=true -o name)"
test -z "$(kube -n kube-system get leases \
  -l app.kubernetes.io/managed-by=idleloom -o name)"
```

Stop if any command prints a resource. After the ownership checks pass:

```sh
kube delete -k "${IDLELOOM_REPO}/deploy/native/projection" --ignore-not-found
kube delete -k "${IDLELOOM_REPO}/deploy/native" --ignore-not-found
kube delete clusterrolebinding \
  idleloom:node-autoapprove-bootstrap \
  idleloom:node-autoapprove-rotation \
  idleloom:node-bootstrapper \
  --ignore-not-found
kube delete deviceclass apple-vulkan --ignore-not-found
kube delete -k "${IDLELOOM_REPO}/deploy/base" --ignore-not-found
```

Deleting Native CRDs also deletes all `IdleloomModel` catalog objects. Never do
this while another Native host uses the cluster.

## Optional WireKube uninstall

WireKube is a separate shared product. Inspect every peer first:

```sh
kube get wirekubepeers -o wide
```

Only a dedicated lab should purge it:

```sh
wirekubectl uninstall \
  --kubeconfig "${IDLELOOM_KUBECONFIG}" \
  --context "${IDLELOOM_CONTEXT}" \
  --purge \
  --confirm-purge \
  --yes
```

Omit this command whenever another node, Native host, or external peer depends
on the mesh.

## Verify final inventory

```sh
for resource in \
  pods services deployments daemonsets jobs secrets endpointslices \
  persistentvolumeclaims resourceclaims resourceclaimtemplates
do
  kube get "${resource}" -A \
    -l app.kubernetes.io/managed-by=idleloom \
    -o name 2>/dev/null
done

kube get idleloomhosts,idleloomworkloads,idleloomworkloadassignments -A 2>/dev/null
kube get nodes -l idleloom-worker=true
launchctl list | grep -i idleloom || true
```

WireKube resources should remain when the optional purge was skipped.
