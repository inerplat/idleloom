# Worker Storage

Linux Worker is a real kubelet node and supports standard Kubernetes volume
semantics. Complete the [Linux Worker quick start](../getting-started/linux-worker.md)
before these tests.

## hostPath persistence

The reviewed example writes, reads, and removes data under the Worker VM's
`/var/lib/idleloom/volumes` directory:

```sh
kube apply -f "${IDLELOOM_REPO}/examples/worker/hostpath-writer.yaml"
kube -n default wait --for=condition=complete \
  job/idleloom-hostpath-writer --timeout=5m
kube -n default logs job/idleloom-hostpath-writer

kube -n default delete job/idleloom-hostpath-writer
kube apply -f "${IDLELOOM_REPO}/examples/worker/hostpath-reader.yaml"
kube -n default wait --for=condition=complete \
  job/idleloom-hostpath-reader --timeout=5m
kube -n default logs job/idleloom-hostpath-reader

kube -n default delete job/idleloom-hostpath-reader
kube apply -f "${IDLELOOM_REPO}/examples/worker/hostpath-cleanup.yaml"
```

In a multi-Worker cluster, add an explicit hostname selector so the writer and
reader use the same node-local disk. This path is inside the Linux VM, not a
macOS host directory.

## Longhorn and iSCSI

The Worker image includes `open-iscsi` and starts `iscsid`. When Longhorn is
already installed and tolerates the Worker taint:

```sh
kube get storageclass longhorn
kube apply -f "${IDLELOOM_REPO}/examples/worker/longhorn-pvc-smoke.yaml"
kube -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/idleloom-longhorn-smoke --timeout=10m
kube -n default logs pod/idleloom-longhorn-smoke
kube -n default delete -f \
  "${IDLELOOM_REPO}/examples/worker/longhorn-pvc-smoke.yaml"
```

This verifies CSI attachment, the host iSCSI session, filesystem mount, and Pod
access. Do not apply the manifest to a cluster without Longhorn.

## NFSv4.1

Start from the reviewed template and replace both reserved placeholders:

```sh
export NFS_SERVER=REPLACE_WITH_NFS_SERVER
export NFS_PATH=REPLACE_WITH_EXPORT_PATH

sed \
  -e "s/203\\.0\\.113\\.20/${NFS_SERVER}/" \
  -e "s#/exports/idleloom#${NFS_PATH}#" \
  "${IDLELOOM_REPO}/examples/worker/nfs-pv-smoke.example.yaml" \
  > nfs-pv-smoke.yaml

kube apply -f nfs-pv-smoke.yaml
kube -n default wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/idleloom-nfs-smoke --timeout=10m
kube -n default logs pod/idleloom-nfs-smoke
kube delete -f nfs-pv-smoke.yaml
rm -f nfs-pv-smoke.yaml
```

The export must be reachable from the Worker VM, not only from the macOS host.

## Managed-cluster CSI images

Confirm every CSI DaemonSet image supports `linux/arm64`. An amd64-only node
plugin fails with `exec format error` before Idleloom can test the storage
backend. Treat that as a provider image limitation rather than an iSCSI or NFS
failure.
