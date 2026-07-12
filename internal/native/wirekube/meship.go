package wirekube

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

const ipClaimNamespace = "idleloom-system"

func meshIPForName(name, meshCIDR string) (string, error) {
	_, network, err := net.ParseCIDR(meshCIDR)
	if err != nil {
		return "", fmt.Errorf("invalid mesh CIDR %q: %w", meshCIDR, err)
	}
	base := network.IP.To4()
	if base == nil {
		return "", fmt.Errorf("mesh CIDR must be IPv4")
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones > 30 {
		return "", fmt.Errorf("mesh CIDR is too small")
	}
	size := uint32(1) << uint(bits-ones)
	const (
		fnvOffset = uint32(2166136261)
		fnvPrime  = uint32(16777619)
	)
	hash := fnvOffset
	for index := 0; index < len(name); index++ {
		hash ^= uint32(name[index])
		hash *= fnvPrime
	}
	offset := hash%(size-2) + 1
	baseValue := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	value := baseValue + offset
	ip := net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
	return ip.String() + "/32", nil
}

func validateMeshIPAvailability(ctx context.Context, client dynamic.Interface, peerName, displayName, meshCIDR string) (string, error) {
	expected, err := meshIPForName(displayName, meshCIDR)
	if err != nil {
		return "", err
	}
	expectedIP, _, _ := net.ParseCIDR(expected)

	externalPeers, err := client.Resource(ExternalPeersGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list WireKubeExternalPeer resources for address collision check: %w", err)
	}
	for index := range externalPeers.Items {
		peer := &externalPeers.Items[index]
		if peer.GetName() == peerName {
			continue
		}
		assigned, _, _ := unstructured.NestedString(peer.Object, "status", "assignedMeshIP")
		if assigned != "" && sameIP(assigned, expectedIP) {
			return "", fmt.Errorf("deterministic mesh address %s is already assigned to WireKubeExternalPeer/%s", expected, peer.GetName())
		}
		otherDisplayName, _, _ := unstructured.NestedString(peer.Object, "spec", "displayName")
		if otherDisplayName == "" {
			continue
		}
		candidate, candidateErr := meshIPForName(otherDisplayName, meshCIDR)
		if candidateErr != nil {
			return "", fmt.Errorf("derive address for WireKubeExternalPeer/%s: %w", peer.GetName(), candidateErr)
		}
		if candidate == expected {
			return "", fmt.Errorf("deterministic mesh address %s collides with pending WireKubeExternalPeer/%s", expected, peer.GetName())
		}
	}

	peers, err := client.Resource(PeersGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list WireKubePeer resources for address collision check: %w", err)
	}
	for index := range peers.Items {
		peer := &peers.Items[index]
		allowedIPs, _, _ := unstructured.NestedStringSlice(peer.Object, "spec", "allowedIPs")
		for _, allowed := range allowedIPs {
			if routeContainsIP(allowed, expectedIP) {
				return "", fmt.Errorf("deterministic mesh address %s overlaps WireKubePeer/%s allowed IP %s", expected, peer.GetName(), allowed)
			}
		}
	}
	return expected, nil
}

func sameIP(value string, expected net.IP) bool {
	ip, _, err := net.ParseCIDR(value)
	return err == nil && ip.Equal(expected)
}

func routeContainsIP(value string, expected net.IP) bool {
	if ip := net.ParseIP(value); ip != nil {
		return ip.Equal(expected)
	}
	_, network, err := net.ParseCIDR(value)
	return err == nil && network.Contains(expected)
}

func ensureMeshIPClaim(ctx context.Context, client dynamic.Interface, state State, address string) (*unstructured.Unstructured, bool, error) {
	name := "idleloom-wirekube-" + strings.ReplaceAll(strings.TrimSuffix(address, "/32"), ".", "-")
	desired := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "coordination.k8s.io/v1",
		"kind":       "Lease",
		"metadata": map[string]any{
			"name": name,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": managedBy,
				"app.kubernetes.io/part-of":    "idleloom",
			},
			"annotations": map[string]any{
				"ai.idleloom.io/enrollment-id": state.EnrollmentID,
				"ai.idleloom.io/peer-name":     state.PeerName,
				"ai.idleloom.io/mesh-address":  address,
				"ai.idleloom.io/public-key":    state.PublicKey,
			},
		},
		"spec": map[string]any{"holderIdentity": state.PeerName},
	}}
	claims := client.Resource(IPClaimsGVR).Namespace(ipClaimNamespace)
	existing, err := claims.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		created, createErr := claims.Create(ctx, desired, metav1.CreateOptions{})
		if createErr != nil {
			if apierrors.IsAlreadyExists(createErr) {
				existing, err = claims.Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return nil, false, fmt.Errorf("get mesh IP claim Lease/%s after create race: %w", name, err)
				}
				if err := validateMeshIPClaim(existing, desired, state); err != nil {
					return nil, false, err
				}
				return existing, false, nil
			}
			return nil, false, fmt.Errorf("create mesh IP claim Lease/%s: %w", name, createErr)
		}
		if created.GetUID() == "" {
			return nil, true, fmt.Errorf("mesh IP claim Lease/%s has no UID", name)
		}
		return created, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get mesh IP claim Lease/%s: %w", name, err)
	}
	if err := validateMeshIPClaim(existing, desired, state); err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

func validateMeshIPClaim(existing, desired *unstructured.Unstructured, state State) error {
	if existing.GetLabels()["app.kubernetes.io/managed-by"] != managedBy ||
		existing.GetAnnotations()["ai.idleloom.io/enrollment-id"] != state.EnrollmentID ||
		existing.GetAnnotations()["ai.idleloom.io/peer-name"] != state.PeerName ||
		existing.GetAnnotations()["ai.idleloom.io/mesh-address"] != desired.GetAnnotations()["ai.idleloom.io/mesh-address"] ||
		existing.GetAnnotations()["ai.idleloom.io/public-key"] != state.PublicKey {
		return fmt.Errorf("mesh IP claim Lease/%s is owned by another enrollment", existing.GetName())
	}
	holder, _, _ := unstructured.NestedString(existing.Object, "spec", "holderIdentity")
	if holder != state.PeerName {
		return fmt.Errorf("mesh IP claim Lease/%s has a different holder", existing.GetName())
	}
	if state.MeshIPClaimUID != "" && existing.GetUID() != state.MeshIPClaimUID {
		return fmt.Errorf("mesh IP claim Lease/%s identity changed", existing.GetName())
	}
	return nil
}

func deleteMeshIPClaim(ctx context.Context, client dynamic.Interface, state State, timeout time.Duration) error {
	if state.MeshIPClaimName == "" {
		return nil
	}
	claims := client.Resource(IPClaimsGVR).Namespace(ipClaimNamespace)
	claim, err := claims.Get(ctx, state.MeshIPClaimName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get mesh IP claim Lease/%s: %w", state.MeshIPClaimName, err)
	}
	expectedAddress, err := meshIPForName(state.DisplayName, state.MeshCIDR)
	if err != nil {
		return err
	}
	desired := &unstructured.Unstructured{}
	desired.SetAnnotations(map[string]string{"ai.idleloom.io/mesh-address": expectedAddress})
	if err := validateMeshIPClaim(claim, desired, state); err != nil {
		return err
	}
	uid := claim.GetUID()
	if state.MeshIPClaimUID != "" && uid != state.MeshIPClaimUID {
		return fmt.Errorf("mesh IP claim Lease/%s identity changed", state.MeshIPClaimName)
	}
	if uid == "" {
		return fmt.Errorf("mesh IP claim Lease/%s has no UID", state.MeshIPClaimName)
	}
	if err := claims.Delete(ctx, state.MeshIPClaimName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete mesh IP claim Lease/%s: %w", state.MeshIPClaimName, err)
	}
	err = wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		_, getErr := claims.Get(ctx, state.MeshIPClaimName, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			return true, nil
		}
		return false, getErr
	})
	if err != nil {
		return fmt.Errorf("wait for mesh IP claim Lease/%s deletion: %w", state.MeshIPClaimName, err)
	}
	return nil
}
