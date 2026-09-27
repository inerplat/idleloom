package idleloom

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/inerplat/idleloom/internal/meshclaim"
	"github.com/inerplat/idleloom/internal/meship"
)

type WireKubeStatus struct {
	Installed             bool
	IncludeNodeInternalIP bool
	AgentNamespace        string
	AgentName             string
	ReadyPeers            int64
	// MeshCIDR is the overlay range peers draw their addresses from. The
	// in-place backends derive the worker's node address from it before the
	// node exists, so enrollment needs it up front.
	MeshCIDR string
	// MeshName is the WireKubeMesh the address comes from. Claims are named
	// after it, so it has to travel with the CIDR.
	MeshName string
	// AddressAllocation is the mesh's spec.addressAllocation: "hash" when
	// every peer computes its own address and a collision is nobody's to
	// resolve, "allocator" when WireKube arbitrates them through claims.
	AddressAllocation string
}

// ArbitratesAddresses reports whether the mesh resolves address collisions
// rather than leaving both peers advertising the same /32. Enrollment can only
// take a different address when it does; on a hash mesh the address is
// whatever the node name produces, and a collision has to be refused.
func (s WireKubeStatus) ArbitratesAddresses() bool {
	return s.AddressAllocation == "allocator"
}

// CheckWireKube validates the WireKube installation structurally: the mesh
// exists, an agent DaemonSet runs, and Node InternalIPs are advertised. It
// deliberately does NOT require a ready ingress peer — on a bootstrapping mesh
// this worker becomes the first one, and waitForWireKube already waits for the
// worker's own peer to connect within the command timeout.
// defaultMeshName is the only WireKubeMesh Idleloom reads; CheckWireKube asks
// for it by name.
const defaultMeshName = "default"

func CheckWireKube(ctx context.Context, client kubernetes.Interface) (WireKubeStatus, error) {
	var status WireKubeStatus
	raw, err := client.Discovery().RESTClient().Get().AbsPath("/apis/wirekube.io/v1alpha1/wirekubemeshes/default").Do(ctx).Raw()
	if err != nil {
		return status, fmt.Errorf("the WireKube mesh is not available: %w", err)
	}
	var mesh struct {
		Spec struct {
			MeshCIDR          string `json:"meshCIDR"`
			AddressAllocation string `json:"addressAllocation"`
			AutoAllowedIPs    struct {
				IncludeNodeInternalIP bool `json:"includeNodeInternalIP"`
			} `json:"autoAllowedIPs"`
		} `json:"spec"`
		Status struct {
			ReadyPeers int64 `json:"readyPeers"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &mesh); err != nil {
		return status, fmt.Errorf("decode WireKubeMesh: %w", err)
	}
	status.Installed = true
	status.IncludeNodeInternalIP = mesh.Spec.AutoAllowedIPs.IncludeNodeInternalIP
	status.ReadyPeers = mesh.Status.ReadyPeers
	status.MeshCIDR = mesh.Spec.MeshCIDR
	status.MeshName = defaultMeshName
	status.AddressAllocation = mesh.Spec.AddressAllocation

	daemonSets, err := client.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return status, fmt.Errorf("list DaemonSets while checking WireKube: %w", err)
	}
	for _, daemonSet := range daemonSets.Items {
		if strings.Contains(strings.ToLower(daemonSet.Name), "wirekube") {
			status.AgentNamespace = daemonSet.Namespace
			status.AgentName = daemonSet.Name
			break
		}
	}
	if err := validateWireKubeStatus(status); err != nil {
		return status, err
	}
	return status, nil
}

func validateWireKubeStatus(status WireKubeStatus) error {
	if status.AgentName == "" {
		return fmt.Errorf("the WireKubeMesh exists but no WireKube agent DaemonSet was found")
	}
	if !status.IncludeNodeInternalIP {
		return fmt.Errorf("the WireKubeMesh default must set spec.autoAllowedIPs.includeNodeInternalIP=true")
	}
	return nil
}

func WireKubePeerConnected(ctx context.Context, client kubernetes.Interface, nodeName string) (bool, error) {
	path := "/apis/wirekube.io/v1alpha1/wirekubepeers/" + nodeName
	raw, err := client.Discovery().RESTClient().Get().AbsPath(path).Do(ctx).Raw()
	if err != nil {
		return false, err
	}
	var peer struct {
		Status struct {
			Connected bool `json:"connected"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &peer); err != nil {
		return false, fmt.Errorf("decode WireKubePeer %s: %w", nodeName, err)
	}
	return peer.Status.Connected, nil
}

// MeshAddress is the overlay address a worker will advertise.
type MeshAddress struct {
	// Address is the bare IPv4 address, the form kubelet takes as --node-ip.
	Address string
	// Claimed is true when a cluster-wide claim backs the address, which is
	// what "idlectl delete worker" has to release.
	Claimed bool
	// Moved is true when the node name's own address was taken and the worker
	// was given another. Worth saying out loud: the operator picked a name
	// expecting one address and got a different one.
	Moved bool
}

// ReserveMeshAddress settles the worker's node address against the WireKube
// mesh.
//
// WireKube derives a peer's address from a hash of its name, which is what
// lets idlectl know the address before the node exists. The hash is reduced
// into the mesh CIDR though, so it is not collision-free, and what happens
// next depends on the mesh:
//
//   - spec.addressAllocation=allocator: WireKube arbitrates addresses through
//     claims. Enrollment claims one too, under the node name, and WireKube's
//     agent adopts that claim when the worker joins. A contested address means
//     this worker takes another, not that enrollment fails.
//   - otherwise: nothing arbitrates, so an address another peer already holds
//     has to be reported rather than silently duplicated.
func ReserveMeshAddress(ctx context.Context, client kubernetes.Interface, nodeName string, wireKube WireKubeStatus) (MeshAddress, error) {
	if wireKube.MeshCIDR == "" {
		return MeshAddress{}, fmt.Errorf("the WireKubeMesh does not publish spec.meshCIDR, so the worker node address cannot be derived")
	}
	if wireKube.ArbitratesAddresses() {
		return claimMeshAddress(ctx, client, nodeName, wireKube)
	}
	address, err := meship.AddressForName(nodeName, wireKube.MeshCIDR)
	if err != nil {
		return MeshAddress{}, err
	}
	if err := checkExternalPeerClaims(ctx, client, address, wireKube.MeshCIDR); err != nil {
		return MeshAddress{}, err
	}
	if err := checkPeerAllowedIPs(ctx, client, nodeName, address); err != nil {
		return MeshAddress{}, err
	}
	return MeshAddress{Address: address}, nil
}

// claimMeshAddress takes the address through WireKube's allocator.
func claimMeshAddress(ctx context.Context, client kubernetes.Interface, nodeName string, wireKube WireKubeStatus) (MeshAddress, error) {
	result, err := meshAllocator(client, wireKube).Allocate(ctx, nodeName, "")
	if err != nil {
		return MeshAddress{}, err
	}
	address, _, err := net.ParseCIDR(result.Address)
	if err != nil {
		return MeshAddress{}, fmt.Errorf("parse the claimed mesh address %q: %w", result.Address, err)
	}
	return MeshAddress{Address: address.String(), Claimed: true, Moved: result.Moved}, nil
}

// ReleaseMeshAddress hands the worker's address back so the next enrollment
// can use it immediately. WireKube's reaper would collect it anyway once the
// node is gone, so a failure here is worth reporting but is not worth failing
// a teardown over.
func ReleaseMeshAddress(ctx context.Context, client kubernetes.Interface, nodeName string, wireKube WireKubeStatus) error {
	if !wireKube.ArbitratesAddresses() || wireKube.MeshCIDR == "" {
		return nil
	}
	return meshAllocator(client, wireKube).Release(ctx, nodeName)
}

// meshEnrollmentGrace is how long a claim survives with no peer answering for
// it. It has to outlast the worker's whole enrollment — download, boot, TLS
// bootstrap, agent connect — because until the WireKubePeer exists WireKube's
// reaper sees nothing but an unheld claim. It is deliberately generous: the
// cost of overshooting is that an abandoned address takes longer to come back,
// and the cost of undershooting is an address reassigned mid-enrollment.
const meshEnrollmentGrace = time.Hour

func meshAllocator(client kubernetes.Interface, wireKube WireKubeStatus) *meshclaim.Allocator {
	return &meshclaim.Allocator{
		Client:   client,
		MeshName: wireKube.MeshName,
		MeshCIDR: wireKube.MeshCIDR,
		Grace:    meshEnrollmentGrace,
	}
}

// checkPeerAllowedIPs refuses an address a WireKubePeer already advertises.
func checkPeerAllowedIPs(ctx context.Context, client kubernetes.Interface, nodeName, address string) error {
	raw, err := client.Discovery().RESTClient().Get().AbsPath("/apis/wirekube.io/v1alpha1/wirekubepeers").Do(ctx).Raw()
	if err != nil {
		return fmt.Errorf("list WireKube peers to check the worker address: %w", err)
	}
	var peers struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				AllowedIPs []string `json:"allowedIPs"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &peers); err != nil {
		return fmt.Errorf("decode WireKube peers: %w", err)
	}
	for _, peer := range peers.Items {
		if peer.Metadata.Name == nodeName {
			continue
		}
		for _, allowed := range peer.Spec.AllowedIPs {
			if strings.TrimSuffix(allowed, "/32") == address {
				return fmt.Errorf("the mesh address %s derived from node name %q is already held by WireKubePeer/%s; enrol this worker under a different name, or set the WireKubeMesh spec.addressAllocation to \"allocator\" so WireKube assigns a free address instead", address, nodeName, peer.Metadata.Name)
			}
		}
	}
	return nil
}

// checkExternalPeerClaims refuses an address a Native Metal host has taken.
//
// A WireKubeExternalPeer reserves its address before the WireKubePeer that
// carries it exists, so scanning peers alone would let a worker take an
// address "idlectl join" had already claimed and leave both advertising it.
// The resource is absent on installations that never enrolled one, which is
// not an error.
func checkExternalPeerClaims(ctx context.Context, client kubernetes.Interface, address, meshCIDR string) error {
	raw, err := client.Discovery().RESTClient().Get().AbsPath("/apis/wirekube.io/v1alpha1/wirekubeexternalpeers").Do(ctx).Raw()
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("list WireKube external peers to check the worker address: %w", err)
	}
	var peers struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				DisplayName string `json:"displayName"`
			} `json:"spec"`
			Status struct {
				AssignedMeshIP string `json:"assignedMeshIP"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &peers); err != nil {
		return fmt.Errorf("decode WireKube external peers: %w", err)
	}
	claims := make([]externalPeerClaim, 0, len(peers.Items))
	for _, peer := range peers.Items {
		claims = append(claims, externalPeerClaim{
			Name:           peer.Metadata.Name,
			DisplayName:    peer.Spec.DisplayName,
			AssignedMeshIP: peer.Status.AssignedMeshIP,
		})
	}
	return conflictingExternalPeer(claims, address, meshCIDR)
}

// externalPeerClaim is the part of a WireKubeExternalPeer that can collide
// with a worker's address.
type externalPeerClaim struct {
	Name           string
	DisplayName    string
	AssignedMeshIP string
}

// conflictingExternalPeer reports the external peer, if any, that already
// holds address or would take it.
func conflictingExternalPeer(claims []externalPeerClaim, address, meshCIDR string) error {
	for _, claim := range claims {
		if assigned := strings.TrimSuffix(claim.AssignedMeshIP, "/32"); assigned == address {
			return fmt.Errorf("the mesh address %s is already assigned to WireKubeExternalPeer/%s; enrol this worker under a different name", address, claim.Name)
		}
		if claim.DisplayName == "" {
			continue
		}
		// A peer that has not been assigned an address yet will take the one
		// its display name hashes to, so a pending claim collides too.
		pending, err := meship.AddressForName(claim.DisplayName, meshCIDR)
		if err != nil {
			return fmt.Errorf("derive the address of WireKubeExternalPeer/%s: %w", claim.Name, err)
		}
		if pending == address {
			return fmt.Errorf("the mesh address %s collides with pending WireKubeExternalPeer/%s; enrol this worker under a different name", address, claim.Name)
		}
	}
	return nil
}

// PreviewMeshAddress reports the address a worker named nodeName would take,
// without claiming it. A dry run has to be able to say "this name collides"
// without consuming an address, because the run that follows it needs the same
// address still free.
func PreviewMeshAddress(ctx context.Context, client kubernetes.Interface, nodeName string, wireKube WireKubeStatus) (MeshAddress, error) {
	if wireKube.MeshCIDR == "" {
		return MeshAddress{}, fmt.Errorf("the WireKubeMesh does not publish spec.meshCIDR, so the worker node address cannot be derived")
	}
	address, err := meship.AddressForName(nodeName, wireKube.MeshCIDR)
	if err != nil {
		return MeshAddress{}, err
	}
	if !wireKube.ArbitratesAddresses() {
		if err := checkExternalPeerClaims(ctx, client, address, wireKube.MeshCIDR); err != nil {
			return MeshAddress{}, err
		}
		if err := checkPeerAllowedIPs(ctx, client, nodeName, address); err != nil {
			return MeshAddress{}, err
		}
		return MeshAddress{Address: address}, nil
	}
	// On an arbitrating mesh a taken address is not a failure, so the preview
	// reports the move rather than refusing. It cannot say which address the
	// real run would land on — that depends on what is free at the time — so
	// it says only that this one is spoken for.
	held, err := meshAddressIsClaimed(ctx, client, address, wireKube)
	if err != nil {
		return MeshAddress{}, err
	}
	return MeshAddress{Address: address, Moved: held}, nil
}

// meshAddressIsClaimed reports whether a claim already covers address.
func meshAddressIsClaimed(ctx context.Context, client kubernetes.Interface, address string, wireKube WireKubeStatus) (bool, error) {
	name := meshclaim.ClaimName(wireKube.MeshName, address)
	_, err := client.CoordinationV1().Leases(meshclaim.DefaultNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the claim on the mesh address %s: %w", address, err)
	}
	return true, nil
}
