# Mesh Addressing

An in-place worker takes its Kubernetes node IP from the WireKube mesh. This page explains where that address comes from, what happens when two workers want the same one, and what you have to do about it.

It applies to the `linux` and `wsl2` backends. A krunkit worker gets a private subnet Idleloom owns and reserves cluster-wide, and none of this applies to it.

## Why the address comes from the mesh

An in-place worker shares a network stack Idleloom does not own. Its own address is whatever the host already has: a LAN address behind a home router, or a WSL2 address behind Windows NAT. Neither is unique across machines, and `autoAllowedIPs` publishes the node IP to every peer in the mesh, so two workers on two different `192.168.0.x` networks would advertise addresses that collide.

The mesh overlay range has neither problem. WireKube gives every peer a `/32` inside `spec.meshCIDR`, and that address is routable from every other peer by construction. Using it as the node IP means kubelet, the WireKube tunnel, and the rest of the cluster all agree on one address for the node.

## How the address is chosen

WireKube derives a peer's address from a 32-bit FNV-1a hash of its name, reduced into the mesh CIDR. Nothing coordinates: every participant computes the same answer for the same name. That is what lets `idlectl` know the address before the node exists, which it has to, because kubelet needs `--node-ip` at startup.

The hash is not injective. Two names can reduce to the same `/32`, and the chance is higher than it looks: the birthday bound is roughly the square root of the usable range, so a `/24` starts producing collisions somewhere around sixteen names.

What happens at that point is set by `WireKubeMesh.spec.addressAllocation`.

| Value | On a collision |
| --- | --- |
| `hash` (default) | Nothing arbitrates. Enrollment refuses rather than let two peers advertise one address. |
| `allocator` | WireKube arbitrates through claims. Enrollment takes the next free address and says so. |

## The bootstrap cycle

The address has to be on the host before kubelet starts, and the WireKube agent that would normally put it there is a DaemonSet Pod. That Pod needs the node registered, the node needs kubelet running, and kubelet needs the address. Nothing in that loop can go first.

Idleloom breaks it with a systemd unit, `idleloom-node-address.service`, which holds the address on a dummy link and is ordered before kubelet. The WireKube agent later assigns the same address to its own tunnel device and reconciles nothing outside it, so the two coexist. This is the same arrangement a WireKube control-plane node already uses.

Loopback will not do instead. Cilium treats an address on `lo` as NodePort-capable and will SNAT service traffic to it, which sends replies to an address no peer can route back to.

## Claims

On a mesh set to `allocator`, an address is backed by a `Lease` named after it, in the namespace the WireKube agent runs in:

```sh
kubectl -n wirekube-system get leases -l wirekube.io/claim=address \
  -o custom-columns='ADDRESS:.metadata.annotations.wirekube\.io/address,HOLDER:.spec.holderIdentity'
```

Creating that Lease is the arbitration. The API server admits one creator per name, so the second claimant learns it lost and walks to another candidate. There is no allocator process to run and nothing to keep in sync.

`idlectl` claims under the node name before the worker boots, and the WireKube agent adopts the same claim when the worker joins. The claim carries an hour of grace for that gap: until the `WireKubePeer` exists, WireKube's reaper sees only an unheld claim, and without the grace it would hand the address to somebody else mid-enrollment.

`idlectl delete worker` releases the claim. If the release fails the command still finishes and prints a warning; WireKube's reaper collects the claim once the peer is gone.

## Reading the address back

On a mesh set to `allocator` the node name no longer predicts the address. Read it from the node:

```sh
kubectl get node NAME -o jsonpath='{.status.addresses}'
kubectl get wirekubepeer NAME -o jsonpath='{.spec.allowedIPs}'
```

The node's `InternalIP` and the first entry of the peer's `allowedIPs` are the same address. If they differ, see [Troubleshooting](operations/troubleshooting.md).

`--dry-run` reports the address a worker would take, and whether it is already claimed, without claiming it.

## Switching a mesh to the allocator

```sh
kubectl get wirekubepeers -o custom-columns=NAME:.metadata.name,MESHIP:.status.meshIP
kubectl patch wirekubemesh default --type=merge \
  -p '{"spec":{"addressAllocation":"allocator"}}'
```

Every row of the first command has to be populated before you run the second. `status.meshIP` is how an agent remembers which address it was given; an agent too old to write it recomputes the hash on every upsert and would drag an allocated peer straight back onto the contested address.

Switching moves nobody. The first candidate the allocator tries is the hashed address, and a peer already advertising something else has that honoured ahead of it.

## Exhaustion

When every address in the mesh CIDR is claimed, enrollment fails and says so. Nothing frees up on its own, so the only fix is to widen `spec.meshCIDR`. Watch for it well before then:

| Metric | Meaning |
| --- | --- |
| `wirekube_mesh_addresses_capacity` | Usable host addresses in the mesh CIDR |
| `wirekube_mesh_addresses_allocated` | Addresses currently claimed |
| `wirekube_mesh_addresses_free` | Addresses still available |

Collisions, and so renumbering, begin long before the pool is full.
