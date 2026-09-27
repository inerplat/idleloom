package wirekube

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"

	"github.com/inerplat/idleloom/internal/meshclaim"
	"github.com/inerplat/idleloom/internal/meship"
)

// legacyClaimNamespace is where Idleloom kept its own mesh address claims
// before WireKube grew an allocator.
//
// Claims arbitrate by name, so a claim nothing else looks at arbitrates
// nothing: a Native host claiming here while WireKube claims in its own
// namespace means both can take the same /32 and neither notices. Enrollment
// now claims through meshclaim, and this is kept only long enough to clean up
// what an older idlectl left behind.
const legacyClaimNamespace = "idleloom-system"

// meshIPClaim identifies the Lease holding this host's address, so a failed
// enrollment can roll it back.
type meshIPClaim struct {
	Name      string
	UID       types.UID
	Namespace string
}

// nativeEnrollmentGrace is how long the claim survives with no peer answering
// for it. The WireKubePeer is created moments after the claim, so this only
// has to cover the rest of enrollment, but overshooting costs nothing beyond
// an abandoned address taking longer to come back.
const nativeEnrollmentGrace = time.Hour

// meshIPForName derives the deterministic mesh address for a name. Workers
// and Native hosts have to agree on it, so both go through one allocator.
func meshIPForName(name, meshCIDR string) (string, error) {
	return meship.IPForName(name, meshCIDR)
}

// claimMeshIP settles this host's mesh address and takes a cluster-wide claim
// on it.
//
// The address derives from the display name, which is what WireKube's external
// peer reconciler derives from too, while the claim is held under the peer
// name, which is what WireKube's reaper looks up to decide the claim is still
// answered for. Getting either wrong would be silent: the wrong hash name
// renumbers a host that is already up, and the wrong holder makes the reaper
// treat a live host's claim as abandoned.
//
// On a mesh that arbitrates addresses a contested address means this host
// takes another. On one that does not, there is no second choice — every peer
// takes the address its name hashes to — so a contested address is reported.
func claimMeshIP(ctx context.Context, client dynamic.Interface, state State, report DoctorReport) (string, meshIPClaim, bool, error) {
	namespace := claimNamespace(report.AgentNamespace)
	allocator := &meshclaim.Allocator{
		Store:    meshclaim.Dynamic(client, namespace),
		MeshName: report.MeshName,
		MeshCIDR: report.MeshCIDR,
		Grace:    nativeEnrollmentGrace,
	}

	if report.ArbitratesAddresses() {
		result, err := allocator.Allocate(ctx, meshclaim.Request{
			Holder:    state.PeerName,
			Name:      state.DisplayName,
			Preferred: state.AssignedMeshIP,
		})
		if err != nil {
			return "", meshIPClaim{}, false, err
		}
		return result.Address, meshIPClaim{
			Name:      result.ClaimName,
			UID:       result.ClaimUID,
			Namespace: namespace,
		}, !result.Adopted, nil
	}

	expected, err := validateMeshIPAvailability(ctx, client, state.PeerName, state.DisplayName, report.MeshCIDR)
	if err != nil {
		return "", meshIPClaim{}, false, err
	}
	result, err := allocator.Reserve(ctx, state.PeerName, expected)
	if err != nil {
		if errors.Is(err, meshclaim.ErrInUse) {
			return "", meshIPClaim{}, false, fmt.Errorf(
				"the mesh address %s derived from display name %q is already claimed by another enrollment; "+
					"join under a different display name, or set the WireKubeMesh spec.addressAllocation to \"allocator\" "+
					"so WireKube assigns a free address instead", expected, state.DisplayName)
		}
		return "", meshIPClaim{}, false, err
	}
	return result.Address, meshIPClaim{
		Name:      result.ClaimName,
		UID:       result.ClaimUID,
		Namespace: namespace,
	}, !result.Adopted, nil
}

// claimNamespace is where WireKube keeps its address claims: the namespace its
// agent runs in. Falling back to the chart default is the best that can be
// done when the agent cannot be found, and is right for every default install.
func claimNamespace(agentNamespace string) string {
	if agentNamespace != "" {
		return agentNamespace
	}
	return meshclaim.DefaultNamespace
}

// discoverAgentNamespace finds where the WireKube agent runs. It goes by the
// DaemonSet rather than by a constant so that an installation in a namespace
// other than the chart default still arbitrates against WireKube's own claims
// instead of quietly keeping a private pool.
//
// An ambiguous answer is reported as none. Picking one of two candidates would
// be a coin toss whose losing side is silent, and the caller warns and falls
// back to the chart default, which is at least a namespace an operator can
// check.
func discoverAgentNamespace(ctx context.Context, client dynamic.Interface) string {
	daemonSets, err := client.Resource(DaemonSetsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}
	found := ""
	for index := range daemonSets.Items {
		item := &daemonSets.Items[index]
		if !meshclaim.IsAgentDaemonSet(item.GetName(), item.GetLabels()) {
			continue
		}
		if found != "" && found != item.GetNamespace() {
			return ""
		}
		found = item.GetNamespace()
	}
	return found
}

// validateMeshIPAvailability reports the address displayName takes and refuses
// it when another peer already holds it. It is only reachable on a mesh that
// does not arbitrate addresses, where there is nothing to move to.
func validateMeshIPAvailability(ctx context.Context, client dynamic.Interface, peerName, displayName, meshCIDR string) (string, error) {
	expected, err := meshIPForName(displayName, meshCIDR)
	if err != nil {
		return "", err
	}
	expectedIP, _, _ := net.ParseCIDR(expected)

	externalPeers, err := client.Resource(ExternalPeersGVR).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("list WireKubeExternalPeer resources for address collision check: %w", err)
	}
	if err == nil {
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
	}

	peers, err := client.Resource(PeersGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list WireKubePeer resources for address collision check: %w", err)
	}
	for index := range peers.Items {
		peer := &peers.Items[index]
		if peer.GetName() == peerName {
			continue
		}
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

// deleteMeshIPClaim hands this host's address back.
//
// It goes by holder rather than by the name recorded in state, so a claim made
// at a different address — an earlier enrollment that was reassigned, or one
// whose state write never landed — is collected too. The legacy namespace is
// swept as well, because an installation upgraded from an older idlectl has a
// claim there that nothing else will ever look at.
func deleteMeshIPClaim(ctx context.Context, client dynamic.Interface, state State, timeout time.Duration) error {
	// Teardown uses the namespace recorded at enrollment rather than
	// rediscovering it: the WireKube installation may already be gone, and the
	// claim is wherever it was made.
	allocator := &meshclaim.Allocator{
		Store:    meshclaim.Dynamic(client, state.MeshIPClaimNamespaceOrDefault()),
		MeshName: state.MeshNameOrDefault(),
		MeshCIDR: state.MeshCIDR,
	}
	if state.MeshCIDR != "" {
		if err := allocator.Release(ctx, state.PeerName); err != nil {
			return err
		}
	}
	if err := deleteLegacyMeshIPClaim(ctx, client, state); err != nil {
		return err
	}
	if state.MeshIPClaimName == "" {
		return nil
	}
	return waitForClaimGone(ctx, client, state.MeshIPClaimNamespaceOrDefault(), state.MeshIPClaimName, state.MeshIPClaimUID, timeout)
}

// deleteLegacyMeshIPClaim removes a claim an older idlectl left in
// idleloom-system. It is guarded by the recorded holder so it cannot delete a
// Lease that merely shares the namespace.
func deleteLegacyMeshIPClaim(ctx context.Context, client dynamic.Interface, state State) error {
	if state.PeerName == "" {
		return nil
	}
	claims := client.Resource(meshclaim.LeasesGVR).Namespace(legacyClaimNamespace)
	name := legacyClaimName(state)
	if name == "" {
		return nil
	}
	claim, err := claims.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		if apierrors.IsForbidden(err) {
			// The namespace may not exist on an installation that never ran an
			// older idlectl, and the peer's RBAC need not cover it.
			return nil
		}
		return fmt.Errorf("get legacy mesh IP claim Lease/%s: %w", name, err)
	}
	holder, _, _ := unstructured.NestedString(claim.Object, "spec", "holderIdentity")
	if holder != state.PeerName {
		return nil
	}
	uid := claim.GetUID()
	err = claims.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("delete legacy mesh IP claim Lease/%s: %w", name, err)
	}
	return nil
}

// legacyClaimName reproduces the name an older idlectl used.
func legacyClaimName(state State) string {
	address := state.AssignedMeshIP
	if address == "" {
		if state.DisplayName == "" || state.MeshCIDR == "" {
			return ""
		}
		derived, err := meshIPForName(state.DisplayName, state.MeshCIDR)
		if err != nil {
			return ""
		}
		address = derived
	}
	return "idleloom-wirekube-" + dashedAddress(address)
}

func dashedAddress(address string) string {
	ip, _, err := net.ParseCIDR(address)
	if err != nil {
		if parsed := net.ParseIP(address); parsed != nil {
			ip = parsed
		} else {
			return ""
		}
	}
	out := []byte(ip.String())
	for index := range out {
		if out[index] == '.' {
			out[index] = '-'
		}
	}
	return string(out)
}

// waitForClaimGone waits for this enrollment's claim to disappear.
//
// It matches on the recorded UID rather than the name. Release deliberately
// leaves a claim held by somebody else alone, so if this host's claim was
// reclaimed and another peer has since taken the same address, a name-only
// wait would sit there until it timed out and leave local state behind — over
// a claim that is already gone.
func waitForClaimGone(ctx context.Context, client dynamic.Interface, namespace, name string, uid types.UID, timeout time.Duration) error {
	if timeout <= 0 {
		return nil
	}
	claims := client.Resource(meshclaim.LeasesGVR).Namespace(namespace)
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		claim, getErr := claims.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			return true, nil
		}
		if getErr != nil {
			return false, getErr
		}
		return uid != "" && claim.GetUID() != uid, nil
	})
	if err != nil {
		return fmt.Errorf("wait for mesh IP claim Lease/%s deletion: %w", name, err)
	}
	return nil
}

// MeshNameOrDefault is the mesh this host claimed its address in. Claims are
// labelled with it, so a release that guessed would find nothing. State written
// before the field existed predates any mesh but the default.
func (s State) MeshNameOrDefault() string {
	if s.MeshName != "" {
		return s.MeshName
	}
	return defaultMeshName
}

// MeshIPClaimNamespaceOrDefault is where this host's claim lives. State
// written before claims moved to WireKube's namespace does not record one.
func (s State) MeshIPClaimNamespaceOrDefault() string {
	if s.MeshIPClaimNamespace != "" {
		return s.MeshIPClaimNamespace
	}
	return meshclaim.DefaultNamespace
}

// rollbackMeshIPClaim undoes a claim made as part of an enrollment that then
// failed. A claim that was adopted rather than created is not rolled back:
// this host already held it before the attempt.
func rollbackMeshIPClaim(ctx context.Context, client dynamic.Interface, claim meshIPClaim) error {
	if claim.Name == "" || claim.UID == "" {
		return nil
	}
	namespace := claim.Namespace
	if namespace == "" {
		namespace = meshclaim.DefaultNamespace
	}
	uid := claim.UID
	err := client.Resource(meshclaim.LeasesGVR).Namespace(namespace).Delete(ctx, claim.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("rollback mesh IP claim Lease/%s: %w", claim.Name, err)
	}
	return nil
}

// confirmMeshIPStillHeld re-checks the address late in enrollment, after the
// WireKubePeer exists.
//
// What it is guarding against differs by mesh. Where addresses are arbitrated,
// the claim is the guarantee, so the check is that this host still holds it —
// a claim that vanished means something reclaimed the address and the peer
// would be advertising one the mesh has given away. Where they are not, there
// is no claim to lean on and the check is the same collision scan as before.
func confirmMeshIPStillHeld(ctx context.Context, client dynamic.Interface, state State, report DoctorReport, claim meshIPClaim, address string) error {
	if !report.ArbitratesAddresses() {
		current, err := validateMeshIPAvailability(ctx, client, state.PeerName, state.DisplayName, report.MeshCIDR)
		if err != nil {
			return err
		}
		if current != address {
			return fmt.Errorf("deterministic WireKube mesh address changed during enrollment")
		}
		return nil
	}
	// Read it back where it was written. Recomputing the namespace here would
	// disagree with the create on any installation that does not run WireKube
	// in the chart's default namespace, and the enrollment would roll itself
	// back reporting a claim that never went missing.
	lease, err := client.Resource(meshclaim.LeasesGVR).Namespace(claim.Namespace).
		Get(ctx, claim.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("the claim on mesh address %s disappeared during enrollment", address)
	}
	if err != nil {
		return fmt.Errorf("confirm the claim on mesh address %s: %w", address, err)
	}
	holder, _, _ := unstructured.NestedString(lease.Object, "spec", "holderIdentity")
	if holder != state.PeerName {
		return fmt.Errorf("the mesh address %s was claimed by %q during enrollment", address, holder)
	}
	return nil
}
