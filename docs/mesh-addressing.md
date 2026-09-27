# Mesh Addressing

An in-place worker takes its Kubernetes node IP from the WireKube mesh.

This page applies to the `linux` and `wsl2` backends. A krunkit worker gets a private subnet Idleloom owns and reserves cluster-wide, and none of this applies to it.

## Why the address comes from the mesh

An in-place worker shares a network stack Idleloom does not own. Its own address is whatever the host already has: a LAN address behind a home router, or a WSL2 address behind Windows NAT. Neither is unique across machines, and `autoAllowedIPs` publishes the node IP to every peer in the mesh, so two workers on two different `192.168.0.x` networks would advertise addresses that collide.

The mesh overlay range has neither problem. WireKube gives every peer a `/32` inside `spec.meshCIDR`, and that address is routable from every other peer by construction. Using it as the node IP means kubelet, the WireKube tunnel, and the rest of the cluster all agree on one address for the node.

## How the address is chosen

WireKube derives a peer's address from a 32-bit FNV-1a hash of its name, reduced into the mesh CIDR. Nothing coordinates: every participant computes the same answer for the same name. That is what lets `idlectl` know the address before the node exists, which it has to, because kubelet needs `--node-ip` at startup.

The hash is not injective. Two names can reduce to the same `/32`, and the chance is higher than it looks: the birthday bound is roughly the square root of the usable range, so a `/24` starts producing collisions somewhere around sixteen names.

What happens at that point is set by `WireKubeMesh.spec.addressAllocation`.

| Value | On a collision |
| --- | --- |
| `hash` (default) | Nothing arbitrates. `idlectl` enrollment refuses rather than let two peers advertise one address, but WireKube itself does not detect the collision, so two peers configured by other means will both hold the address. |
| `allocator` | WireKube arbitrates through claims. Enrollment takes the next free address and says so. |

## The bootstrap cycle

The address has to be on the host before kubelet starts, and the WireKube agent that would normally put it there is a DaemonSet Pod. That Pod needs the node registered, the node needs kubelet running, and kubelet needs the address. Nothing in that loop can go first.

Idleloom breaks it with a systemd unit, `idleloom-node-address.service`, which holds the address on a dummy link and is ordered before kubelet. The WireKube agent later assigns the same address to its own tunnel device and reconciles nothing outside it, so the two coexist.

Loopback will not do instead. Cilium treats an address on `lo` as NodePort-capable and will SNAT service traffic to it, which sends replies to an address no peer can route back to.

## Claims

On a mesh set to `allocator`, an address is backed by a `Lease` named after it, in the namespace the WireKube agent runs in. That is `wirekube-system` by default, and `idlectl` finds it from the agent's DaemonSet rather than assuming it:

```sh
kubectl -n wirekube-system get leases -l wirekube.io/claim=address \
  -o custom-columns='ADDRESS:.metadata.annotations.wirekube\.io/address,HOLDER:.spec.holderIdentity'
```

Creating that Lease is the arbitration. The API server admits one creator per name, so the second claimant learns it lost and walks to another candidate. There is no allocator process to run and nothing to keep in sync.

`idlectl` claims under the node name before the worker boots, and the WireKube agent adopts the same claim when the worker joins. Until the `WireKubePeer` exists, WireKube's reaper sees nothing but an unheld claim, and its own floor for that is 15 minutes — not enough to cover a download, a boot and a TLS bootstrap. `idlectl` therefore writes `spec.leaseDurationSeconds: 3600` into the claim, which the reaper honours in place of its default.

The claim also records which candidate won, in the `wirekube.io/attempt` annotation. A fleet where that is routinely non-zero is close enough to full that the CIDR wants widening.

`idlectl delete worker` releases the claim. If the release fails the command still finishes and prints a warning; WireKube's reaper collects the claim once the peer is gone.

## Reading the address back

On a mesh set to `allocator` the node name no longer predicts the address. Read it from the node:

```sh
kubectl get node NAME -o jsonpath='{.status.addresses}'
kubectl get wirekubepeer NAME -o jsonpath='{.spec.allowedIPs}'
```

The node's `InternalIP` and the first entry of the peer's `allowedIPs` are the same address. If they differ, see [Troubleshooting](operations/troubleshooting.md).

`--dry-run` reports the address a worker would try for, and whether somebody else already holds it, without claiming anything. It cannot report the address the real run would settle on, because that depends on what is free at the time.

## Switching a mesh to the allocator

```sh
kubectl get wirekubepeers -o custom-columns=NAME:.metadata.name,MESHIP:.status.meshIP
kubectl patch wirekubemesh default --type=merge \
  -p '{"spec":{"addressAllocation":"allocator"}}'
```

Every row of the first command has to be populated before you run the second. `status.meshIP` is how an agent remembers which address it was given; an agent too old to write it recomputes the hash on every upsert and would drag an allocated peer straight back onto the contested address.

Switching moves nobody. The first candidate the allocator tries is the hashed address, and a peer already advertising something else has that honoured ahead of it.

Switching back to `hash` is equally safe, and does not put anybody back on a hashed address: agents keep honouring `status.meshIP`. The claims stop being consulted and are collected as their holders go away.

## Exhaustion

When every address in the mesh CIDR is claimed, enrollment fails and says so. Nothing frees up on its own, so the only fix is to widen `spec.meshCIDR`. Collisions, and so renumbering, start long before that, so watch the pool rather than waiting for the failure:

| Metric | Meaning |
| --- | --- |
| `wirekube_mesh_addresses_capacity` | Usable host addresses in the mesh CIDR |
| `wirekube_mesh_addresses_allocated` | Addresses currently claimed |
| `wirekube_mesh_addresses_free` | Addresses still available |

The series are published by WireKube's leader-elected sweep, so exactly one agent Pod exposes them, and only for a mesh set to `allocator`.

## Permissions

Claiming needs `create`, `get`, `list` and `delete` on `coordination.k8s.io/leases` in the namespace the WireKube agent runs in. That is a different namespace from the `kube-system` Leases a krunkit worker uses to reserve its private subnet, so a kubeconfig scoped narrowly enough to enrol a krunkit worker will not enrol an in-place one on an arbitrating mesh.

## When the state file and the cluster disagree

`idlectl` records `meshAddressClaimed` in the worker state file. If `delete worker` warns that it could not release the claim, the address is still held and the state file is gone, so nothing local can release it. Either delete the Lease by hand, or leave it: WireKube reclaims it once no peer answers for the holder.
