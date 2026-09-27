package idleloom

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

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
}

// CheckWireKube validates the WireKube installation structurally: the mesh
// exists, an agent DaemonSet runs, and Node InternalIPs are advertised. It
// deliberately does NOT require a ready ingress peer — on a bootstrapping mesh
// this worker becomes the first one, and waitForWireKube already waits for the
// worker's own peer to connect within the command timeout.
func CheckWireKube(ctx context.Context, client kubernetes.Interface) (WireKubeStatus, error) {
	var status WireKubeStatus
	raw, err := client.Discovery().RESTClient().Get().AbsPath("/apis/wirekube.io/v1alpha1/wirekubemeshes/default").Do(ctx).Raw()
	if err != nil {
		return status, fmt.Errorf("the WireKube mesh is not available: %w", err)
	}
	var mesh struct {
		Spec struct {
			MeshCIDR       string `json:"meshCIDR"`
			AutoAllowedIPs struct {
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

// ReserveMeshAddress derives the worker's node address from the WireKube mesh
// and confirms no other peer already holds it.
//
// WireKube's allocator is a hash of the node name, so the address needs no
// cluster-wide lease: it is reproducible from the name alone. It is not
// collision-free though — the hash is reduced into the mesh CIDR — so an
// existing peer holding the same address has to be reported rather than
// silently overwritten.
func ReserveMeshAddress(ctx context.Context, client kubernetes.Interface, nodeName, meshCIDR string) (string, error) {
	if meshCIDR == "" {
		return "", fmt.Errorf("the WireKubeMesh does not publish spec.meshCIDR, so the worker node address cannot be derived")
	}
	address, err := meship.AddressForName(nodeName, meshCIDR)
	if err != nil {
		return "", err
	}
	if err := checkExternalPeerClaims(ctx, client, address, meshCIDR); err != nil {
		return "", err
	}
	raw, err := client.Discovery().RESTClient().Get().AbsPath("/apis/wirekube.io/v1alpha1/wirekubepeers").Do(ctx).Raw()
	if err != nil {
		return "", fmt.Errorf("list WireKube peers to check the worker address: %w", err)
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
		return "", fmt.Errorf("decode WireKube peers: %w", err)
	}
	for _, peer := range peers.Items {
		if peer.Metadata.Name == nodeName {
			continue
		}
		for _, allowed := range peer.Spec.AllowedIPs {
			if routeCovers(allowed, address) {
				return "", fmt.Errorf("the mesh address %s derived from node name %q is already routed to WireKubePeer/%s as %s; enrol this worker under a different name", address, nodeName, peer.Metadata.Name, allowed)
			}
		}
	}
	return address, nil
}

// routeCovers reports whether an entry in a peer's allowedIPs would capture
// traffic for address.
//
// Comparing for equality is not enough: a gateway peer advertises a CIDR, not
// a /32, so a peer carrying the whole mesh range would look like no conflict
// at all while in fact absorbing every address in it. WireGuard resolves
// overlapping allowedIPs by longest prefix, so the worker would come up and
// the route would simply belong to somebody else.
func routeCovers(route, address string) bool {
	ip := net.ParseIP(address)
	if ip == nil {
		return false
	}
	if bare := net.ParseIP(strings.TrimSpace(route)); bare != nil {
		return bare.Equal(ip)
	}
	_, network, err := net.ParseCIDR(strings.TrimSpace(route))
	return err == nil && network.Contains(ip)
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
