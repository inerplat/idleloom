package wirekube

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"

	"golang.org/x/crypto/curve25519"
)

const (
	ConnectivityAPIOnly  = "api-only"
	ConnectivityWireKube = "wirekube"

	managedBy       = "idleloom"
	stateVersion    = 1
	defaultPeerMTU  = int32(1248)
	stateFileName   = "wirekube-leaf.json"
	defaultMeshName = "default"
)

var (
	MeshesGVR        = schema.GroupVersionResource{Group: "wirekube.io", Version: "v1alpha1", Resource: "wirekubemeshes"}
	PeersGVR         = schema.GroupVersionResource{Group: "wirekube.io", Version: "v1alpha1", Resource: "wirekubepeers"}
	ExternalPeersGVR = schema.GroupVersionResource{Group: "wirekube.io", Version: "v1alpha1", Resource: "wirekubeexternalpeers"}
	IPClaimsGVR      = schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}
)

type DoctorReport struct {
	MeshName      string
	MeshCIDR      string
	RelayMode     string
	RelayProvider string
	ReadyPeers    int64
	Warnings      []string
}

type State struct {
	Version               int       `json:"version"`
	PeerName              string    `json:"peerName"`
	DisplayName           string    `json:"displayName"`
	EnrollmentID          string    `json:"enrollmentID"`
	PeerUID               types.UID `json:"peerUID,omitempty"`
	PrivateKey            string    `json:"privateKey"`
	PublicKey             string    `json:"publicKey"`
	MeshCIDR              string    `json:"meshCIDR,omitempty"`
	AssignedMeshIP        string    `json:"assignedMeshIP,omitempty"`
	RelayEndpoint         string    `json:"relayEndpoint,omitempty"`
	IngressPublicKey      string    `json:"ingressPublicKey,omitempty"`
	AllowedDestinations   []string  `json:"allowedDestinations,omitempty"`
	MTU                   int32     `json:"mtu,omitempty"`
	KubernetesAPIEndpoint string    `json:"kubernetesAPIEndpoint,omitempty"`
	MeshIPClaimName       string    `json:"meshIPClaimName,omitempty"`
	MeshIPClaimUID        types.UID `json:"meshIPClaimUID,omitempty"`
}

type EnrollConfig struct {
	Dynamic        dynamic.Interface
	HostID         string
	EnrollmentID   string
	StateDirectory string
	APIEndpoint    string
	WaitTimeout    time.Duration
}

type RevokeConfig struct {
	Dynamic          dynamic.Interface
	StateDirectory   string
	RuntimeDirectory string
	WaitTimeout      time.Duration
	Force            bool
}

func Inspect(ctx context.Context, client dynamic.Interface) (DoctorReport, error) {
	if client == nil {
		return DoctorReport{}, fmt.Errorf("dynamic client is required")
	}
	mesh, err := client.Resource(MeshesGVR).Get(ctx, defaultMeshName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return DoctorReport{}, fmt.Errorf("WireKubeMesh/%s is not installed", defaultMeshName)
		}
		return DoctorReport{}, fmt.Errorf("get WireKubeMesh/%s: %w", defaultMeshName, err)
	}
	report := DoctorReport{MeshName: mesh.GetName()}
	report.MeshCIDR, _, _ = unstructured.NestedString(mesh.Object, "spec", "meshCIDR")
	if err := validateMeshCIDR(report.MeshCIDR); err != nil {
		return DoctorReport{}, err
	}
	report.RelayMode, _, _ = unstructured.NestedString(mesh.Object, "spec", "relay", "mode")
	report.RelayProvider, _, _ = unstructured.NestedString(mesh.Object, "spec", "relay", "provider")
	if report.RelayMode == "" {
		report.RelayMode = "auto"
	}
	if report.RelayMode == "never" {
		return DoctorReport{}, fmt.Errorf("WireKube relay mode is never; connected leaf requires a relay")
	}
	if report.RelayProvider != "managed" && report.RelayProvider != "external" {
		return DoctorReport{}, fmt.Errorf("WireKube relay provider %q is unsupported", report.RelayProvider)
	}
	report.ReadyPeers, _, _ = unstructured.NestedInt64(mesh.Object, "status", "readyPeers")
	if report.ReadyPeers < 1 {
		report.Warnings = append(report.Warnings, "WireKube reports no ready ingress peers")
	}
	if _, err := client.Resource(ExternalPeersGVR).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		if apierrors.IsNotFound(err) {
			return DoctorReport{}, fmt.Errorf("WireKubeExternalPeer API is not installed")
		}
		return DoctorReport{}, fmt.Errorf("list WireKubeExternalPeer resources: %w", err)
	}
	return report, nil
}

func Enroll(ctx context.Context, config EnrollConfig) (State, error) {
	if config.Dynamic == nil || config.HostID == "" || config.EnrollmentID == "" || config.StateDirectory == "" || config.APIEndpoint == "" {
		return State{}, fmt.Errorf("dynamic client, host ID, enrollment ID, state directory, and API endpoint are required")
	}
	if err := validateAPIEndpoint(config.APIEndpoint); err != nil {
		return State{}, err
	}
	report, err := Inspect(ctx, config.Dynamic)
	if err != nil {
		return State{}, err
	}
	peerName := "idleloom-" + config.HostID
	state, err := loadOrCreateState(config.StateDirectory, peerName, config.EnrollmentID)
	if err != nil {
		return State{}, err
	}
	if state.MeshCIDR != "" && state.MeshCIDR != report.MeshCIDR {
		return State{}, fmt.Errorf("WireKube mesh CIDR changed from %s to %s; repair connectivity before reenrolling", state.MeshCIDR, report.MeshCIDR)
	}
	state.MeshCIDR = report.MeshCIDR
	state.KubernetesAPIEndpoint = config.APIEndpoint
	expectedAddress, err := validateMeshIPAvailability(ctx, config.Dynamic, state.PeerName, state.DisplayName, report.MeshCIDR)
	if err != nil {
		return State{}, err
	}
	claim, claimCreated, err := ensureMeshIPClaim(ctx, config.Dynamic, state, expectedAddress)
	if err != nil {
		return State{}, err
	}
	state.MeshIPClaimName = claim.GetName()
	state.MeshIPClaimUID = claim.GetUID()
	clearAllocation(&state)
	if err := writeState(config.StateDirectory, state); err != nil {
		if claimCreated {
			uid := claim.GetUID()
			rollbackErr := config.Dynamic.Resource(IPClaimsGVR).Namespace("idleloom-system").Delete(ctx, claim.GetName(), metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &uid},
			})
			return State{}, errors.Join(err, rollbackErr)
		}
		return State{}, err
	}
	desired := desiredExternalPeer(state, config.HostID, report.MeshCIDR)
	peer, peerCreated, err := ensureExternalPeer(ctx, config.Dynamic, desired, state)
	if err != nil {
		return State{}, errors.Join(err, rollbackNewEnrollment(ctx, config.Dynamic, nil, claim, false, claimCreated))
	}
	if !peerCreated {
		peer, peerCreated, err = recreateForStaleIngress(ctx, config.Dynamic, desired, state, peer, config.WaitTimeout)
		if err != nil {
			return State{}, err
		}
	}
	state.PeerUID = peer.GetUID()
	if state.PeerUID == "" {
		return State{}, fmt.Errorf("WireKubeExternalPeer/%s has no UID", peer.GetName())
	}
	// Persist the UID before waiting so a partially completed enrollment can be revoked safely.
	if err := writeState(config.StateDirectory, state); err != nil {
		return State{}, err
	}
	waitTimeout := config.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = time.Minute
	}
	peer, err = waitForActive(ctx, config.Dynamic, peer.GetName(), peer.GetUID(), waitTimeout)
	if err != nil {
		return State{}, err
	}
	currentExpectedAddress, err := validateMeshIPAvailability(ctx, config.Dynamic, state.PeerName, state.DisplayName, report.MeshCIDR)
	if err != nil {
		return State{}, errors.Join(err, rollbackNewEnrollment(ctx, config.Dynamic, peer, claim, peerCreated, claimCreated))
	}
	if currentExpectedAddress != expectedAddress {
		err := fmt.Errorf("deterministic WireKube mesh address changed during enrollment")
		return State{}, errors.Join(err, rollbackNewEnrollment(ctx, config.Dynamic, peer, claim, peerCreated, claimCreated))
	}
	if err := applyAllocation(&state, peer, report.MeshCIDR, expectedAddress); err != nil {
		return State{}, errors.Join(err, rollbackNewEnrollment(ctx, config.Dynamic, peer, claim, peerCreated, claimCreated))
	}
	if err := writeState(config.StateDirectory, state); err != nil {
		return State{}, err
	}
	return state, nil
}

func recreateForStaleIngress(ctx context.Context, client dynamic.Interface, desired *unstructured.Unstructured, state State, peer *unstructured.Unstructured, timeout time.Duration) (*unstructured.Unstructured, bool, error) {
	ingressName, _, _ := unstructured.NestedString(peer.Object, "status", "ingressPeerName")
	if ingressName == "" {
		return peer, false, nil
	}
	ingress, err := client.Resource(PeersGVR).Get(ctx, ingressName, metav1.GetOptions{})
	stale := apierrors.IsNotFound(err)
	if err != nil && !stale {
		return nil, false, fmt.Errorf("get WireKube ingress peer %s: %w", ingressName, err)
	}
	if err == nil {
		connected, _, _ := unstructured.NestedBool(ingress.Object, "status", "connected")
		lastHandshake, _, _ := unstructured.NestedString(ingress.Object, "status", "lastHandshake")
		handshake, parseErr := time.Parse(time.RFC3339, lastHandshake)
		stale = !connected || parseErr != nil || time.Since(handshake) > 5*time.Minute
	}
	if !stale {
		return peer, false, nil
	}
	runtimeDirectory, runtimeErr := DefaultRuntimeDirectory(state)
	if runtimeErr == nil {
		active, err := RuntimeStatusIsActive(runtimeDirectory, state)
		if err != nil {
			return nil, false, err
		}
		if active {
			return nil, false, fmt.Errorf("WireKube ingress peer %s is stale; stop the connectivity service and rerun join to repair it", ingressName)
		}
	}
	uid := peer.GetUID()
	if err := client.Resource(ExternalPeersGVR).Delete(ctx, peer.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
		return nil, false, fmt.Errorf("delete ExternalPeer bound to stale ingress %s: %w", ingressName, err)
	}
	if timeout <= 0 {
		timeout = time.Minute
	}
	if err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := client.Resource(ExternalPeersGVR).Get(ctx, peer.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}); err != nil {
		return nil, false, fmt.Errorf("wait for stale ExternalPeer removal: %w", err)
	}
	created, err := client.Resource(ExternalPeersGVR).Create(ctx, desired, metav1.CreateOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("recreate ExternalPeer after stale ingress: %w", err)
	}
	return created, true, nil
}

func ReadState(directory string) (State, error) {
	path := StatePath(directory)
	info, err := os.Lstat(path)
	if err != nil {
		return State{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return State{}, fmt.Errorf("WireKube leaf state must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return State{}, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return State{}, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return State{}, fmt.Errorf("WireKube leaf state changed while opening")
	}
	if openedInfo.Mode().Perm()&0o077 != 0 {
		return State{}, fmt.Errorf("WireKube leaf state permissions must be 0600 or stricter")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return State{}, err
	}
	return decodeState(data)
}

// StatePath returns the private connected-leaf state file for an enrollment.
func StatePath(directory string) string {
	return filepath.Join(directory, stateFileName)
}

func RefreshState(ctx context.Context, client dynamic.Interface, directory string) (State, error) {
	if client == nil {
		return State{}, fmt.Errorf("dynamic client is required")
	}
	state, err := ReadState(directory)
	if err != nil {
		return State{}, err
	}
	peer, err := client.Resource(ExternalPeersGVR).Get(ctx, state.PeerName, metav1.GetOptions{})
	if err != nil {
		return State{}, fmt.Errorf("get WireKubeExternalPeer/%s: %w", state.PeerName, err)
	}
	if peer.GetUID() != state.PeerUID {
		return State{}, fmt.Errorf("WireKubeExternalPeer/%s identity changed", state.PeerName)
	}
	desired := desiredExternalPeer(state, strings.TrimPrefix(state.PeerName, "idleloom-"), state.MeshCIDR)
	if err := validateOwnedPeer(peer, desired, state); err != nil {
		return State{}, err
	}
	phase, _, _ := unstructured.NestedString(peer.Object, "status", "phase")
	if phase != "Active" {
		return State{}, fmt.Errorf("WireKubeExternalPeer/%s is %s", state.PeerName, phase)
	}
	expectedAddress, err := meshIPForName(state.DisplayName, state.MeshCIDR)
	if err != nil {
		return State{}, err
	}
	if err := applyAllocation(&state, peer, state.MeshCIDR, expectedAddress); err != nil {
		return State{}, err
	}
	if err := writeState(directory, state); err != nil {
		return State{}, err
	}
	return state, nil
}

func loadOrCreateState(directory, peerName, enrollmentID string) (State, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return State{}, err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return State{}, err
	}
	state, err := ReadState(directory)
	if err == nil {
		if state.PeerName != peerName || state.DisplayName != peerName || state.EnrollmentID != enrollmentID {
			return State{}, fmt.Errorf("existing WireKube leaf state belongs to a different enrollment")
		}
		return state, nil
	}
	if !os.IsNotExist(err) {
		return State{}, err
	}
	privateKey, publicKey, err := generateKeyPair()
	if err != nil {
		return State{}, err
	}
	state = State{
		Version: stateVersion, PeerName: peerName, DisplayName: peerName, EnrollmentID: enrollmentID,
		PrivateKey: privateKey, PublicKey: publicKey,
	}
	if err := writeState(directory, state); err != nil {
		return State{}, err
	}
	return state, nil
}

func desiredExternalPeer(state State, hostID, meshCIDR string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wirekube.io/v1alpha1",
		"kind":       "WireKubeExternalPeer",
		"metadata": map[string]any{
			"name": state.PeerName,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": managedBy,
				"app.kubernetes.io/part-of":    "idleloom",
				"ai.idleloom.io/host-id":       hostID,
			},
			"annotations": map[string]any{
				"ai.idleloom.io/enrollment-id": state.EnrollmentID,
			},
		},
		"spec": map[string]any{
			"displayName":         state.DisplayName,
			"publicKey":           state.PublicKey,
			"allowedDestinations": []any{meshCIDR},
		},
	}}
}

func ensureExternalPeer(ctx context.Context, client dynamic.Interface, desired *unstructured.Unstructured, state State) (*unstructured.Unstructured, bool, error) {
	peers := client.Resource(ExternalPeersGVR)
	existing, err := peers.Get(ctx, desired.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		created, createErr := peers.Create(ctx, desired, metav1.CreateOptions{})
		if createErr != nil {
			return nil, false, fmt.Errorf("create WireKubeExternalPeer/%s: %w", desired.GetName(), createErr)
		}
		return created, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get WireKubeExternalPeer/%s: %w", desired.GetName(), err)
	}
	if err := validateOwnedPeer(existing, desired, state); err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

func rollbackNewEnrollment(ctx context.Context, client dynamic.Interface, peer, claim *unstructured.Unstructured, peerCreated, claimCreated bool) error {
	var values []error
	if peerCreated && peer != nil && peer.GetUID() != "" {
		uid := peer.GetUID()
		if err := client.Resource(ExternalPeersGVR).Delete(ctx, peer.GetName(), metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		}); err != nil && !apierrors.IsNotFound(err) {
			values = append(values, fmt.Errorf("rollback WireKubeExternalPeer/%s: %w", peer.GetName(), err))
		}
	}
	if claimCreated && claim != nil && claim.GetUID() != "" {
		uid := claim.GetUID()
		if err := client.Resource(IPClaimsGVR).Namespace(ipClaimNamespace).Delete(ctx, claim.GetName(), metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		}); err != nil && !apierrors.IsNotFound(err) {
			values = append(values, fmt.Errorf("rollback mesh IP claim Lease/%s: %w", claim.GetName(), err))
		}
	}
	return errors.Join(values...)
}

func clearAllocation(state *State) {
	state.AssignedMeshIP = ""
	state.RelayEndpoint = ""
	state.IngressPublicKey = ""
	state.AllowedDestinations = nil
	state.MTU = 0
}

func validateOwnedPeer(existing, desired *unstructured.Unstructured, state State) error {
	if !hasOwnedPeerMetadata(existing, state) {
		return fmt.Errorf("WireKubeExternalPeer/%s is not owned by this enrollment", existing.GetName())
	}
	if state.PeerUID != "" && existing.GetUID() != state.PeerUID {
		return fmt.Errorf("WireKubeExternalPeer/%s identity changed", existing.GetName())
	}
	existingSpec, _, _ := unstructured.NestedMap(existing.Object, "spec")
	desiredSpec, _, _ := unstructured.NestedMap(desired.Object, "spec")
	if !reflect.DeepEqual(existingSpec, desiredSpec) {
		return fmt.Errorf("WireKubeExternalPeer/%s does not match the enrolled key and route contract", existing.GetName())
	}
	return nil
}

func hasOwnedPeerMetadata(peer *unstructured.Unstructured, state State) bool {
	return peer.GetLabels()["app.kubernetes.io/managed-by"] == managedBy &&
		peer.GetAnnotations()["ai.idleloom.io/enrollment-id"] == state.EnrollmentID
}

func waitForActive(ctx context.Context, client dynamic.Interface, name string, uid types.UID, timeout time.Duration) (*unstructured.Unstructured, error) {
	var active *unstructured.Unstructured
	err := wait.PollUntilContextTimeout(ctx, time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		peer, err := client.Resource(ExternalPeersGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if peer.GetUID() != uid {
			return false, fmt.Errorf("WireKubeExternalPeer/%s identity changed while waiting for allocation", name)
		}
		phase, _, _ := unstructured.NestedString(peer.Object, "status", "phase")
		switch phase {
		case "Active":
			active = peer
			return true, nil
		case "Failed", "Revoked":
			return false, fmt.Errorf("WireKubeExternalPeer/%s entered %s: %s", name, phase, lastConditionMessage(peer))
		default:
			return false, nil
		}
	})
	if err != nil {
		return nil, fmt.Errorf("wait for WireKubeExternalPeer/%s: %w", name, err)
	}
	return active, nil
}

func applyAllocation(state *State, peer *unstructured.Unstructured, meshCIDR, expectedAddress string) error {
	address, _, _ := unstructured.NestedString(peer.Object, "status", "assignedMeshIP")
	relayEndpoint, _, _ := unstructured.NestedString(peer.Object, "status", "relayEndpoint")
	ingressPublicKey, _, _ := unstructured.NestedString(peer.Object, "status", "ingressPublicKey")
	allowed, _, _ := unstructured.NestedStringSlice(peer.Object, "status", "allowedDestinations")
	mtu, _, _ := unstructured.NestedInt64(peer.Object, "status", "mtu")
	if address == "" || relayEndpoint == "" || ingressPublicKey == "" {
		return fmt.Errorf("WireKubeExternalPeer/%s is Active but allocation is incomplete", peer.GetName())
	}
	if err := validateAssignedAddress(address, meshCIDR); err != nil {
		return err
	}
	if address != expectedAddress {
		return fmt.Errorf("WireKubeExternalPeer/%s was assigned %s, expected deterministic address %s", peer.GetName(), address, expectedAddress)
	}
	if err := validateKey(ingressPublicKey); err != nil {
		return fmt.Errorf("invalid ingress public key: %w", err)
	}
	if _, _, err := net.SplitHostPort(relayEndpoint); err != nil {
		return fmt.Errorf("invalid relay endpoint %q: %w", relayEndpoint, err)
	}
	if len(allowed) == 0 {
		allowed = []string{meshCIDR}
	}
	for _, destination := range allowed {
		if _, _, err := net.ParseCIDR(destination); err != nil {
			return fmt.Errorf("invalid allowed destination %q: %w", destination, err)
		}
	}
	if len(allowed) != 1 || allowed[0] != meshCIDR {
		return fmt.Errorf("WireKubeExternalPeer/%s returned routes outside the connected leaf contract", peer.GetName())
	}
	if mtu == 0 {
		mtu = int64(defaultPeerMTU)
	}
	if mtu < 576 || mtu > 1420 {
		return fmt.Errorf("WireKubeExternalPeer/%s returned invalid MTU %d", peer.GetName(), mtu)
	}
	state.PeerUID = peer.GetUID()
	state.AssignedMeshIP = address
	state.RelayEndpoint = relayEndpoint
	state.IngressPublicKey = ingressPublicKey
	state.AllowedDestinations = allowed
	state.MTU = int32(mtu)
	return nil
}

func validateMeshCIDR(value string) error {
	ip, network, err := net.ParseCIDR(value)
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("WireKube meshCIDR %q must be a valid IPv4 CIDR", value)
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones > 30 {
		return fmt.Errorf("WireKube meshCIDR %q is too small", value)
	}
	if !isSafeMeshNetwork(network) {
		return fmt.Errorf("WireKube meshCIDR %q must be contained in RFC1918, CGNAT, or benchmark address space", value)
	}
	return nil
}

func validateAPIEndpoint(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return fmt.Errorf("Kubernetes API endpoint %q must be an HTTPS URL", value)
	}
	return nil
}

func HasState(directory string) (bool, error) {
	_, err := os.Lstat(filepath.Join(directory, stateFileName))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func Revoke(ctx context.Context, config RevokeConfig) error {
	if config.Dynamic == nil || config.StateDirectory == "" {
		return fmt.Errorf("dynamic client and state directory are required")
	}
	state, err := ReadState(config.StateDirectory)
	if err != nil {
		return err
	}
	runtimeDirectory := config.RuntimeDirectory
	if runtimeDirectory == "" && state.PeerUID != "" {
		runtimeDirectory, err = DefaultRuntimeDirectory(state)
		if err != nil {
			return err
		}
	}
	if !config.Force && runtimeDirectory != "" {
		active, err := RuntimeStatusIsActive(runtimeDirectory, state)
		if err != nil {
			return err
		}
		if active {
			return fmt.Errorf("WireKube connectivity is still running; stop connectivity service or rerun with --force")
		}
	}
	peer, err := config.Dynamic.Resource(ExternalPeersGVR).Get(ctx, state.PeerName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get WireKubeExternalPeer/%s: %w", state.PeerName, err)
	}
	waitTimeout := config.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = time.Minute
	}
	if peer != nil {
		if !hasOwnedPeerMetadata(peer, state) {
			if err := deleteMeshIPClaim(ctx, config.Dynamic, state, waitTimeout); err != nil {
				return err
			}
			return removeLocalState(config.StateDirectory)
		}
		if state.PeerUID != "" && peer.GetUID() != state.PeerUID {
			return fmt.Errorf("WireKubeExternalPeer/%s identity changed", state.PeerName)
		}
		desired := desiredExternalPeer(state, strings.TrimPrefix(state.PeerName, "idleloom-"), state.MeshCIDR)
		if err := validateOwnedPeer(peer, desired, state); err != nil {
			return err
		}
		uid := peer.GetUID()
		if uid == "" {
			return fmt.Errorf("WireKubeExternalPeer/%s has no UID", state.PeerName)
		}
		if err := config.Dynamic.Resource(ExternalPeersGVR).Delete(ctx, state.PeerName, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete WireKubeExternalPeer/%s: %w", state.PeerName, err)
		}
		err = wait.PollUntilContextTimeout(ctx, time.Second, waitTimeout, true, func(ctx context.Context) (bool, error) {
			_, getErr := config.Dynamic.Resource(ExternalPeersGVR).Get(ctx, state.PeerName, metav1.GetOptions{})
			if apierrors.IsNotFound(getErr) {
				return true, nil
			}
			return false, getErr
		})
		if err != nil {
			return fmt.Errorf("wait for WireKubeExternalPeer/%s deletion: %w", state.PeerName, err)
		}
	}
	if err := deleteMeshIPClaim(ctx, config.Dynamic, state, waitTimeout); err != nil {
		return err
	}
	return removeLocalState(config.StateDirectory)
}

func removeLocalState(directory string) error {
	if err := os.Remove(filepath.Join(directory, stateFileName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func validateAssignedAddress(address, meshCIDR string) error {
	ip, network, err := net.ParseCIDR(address)
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("assigned mesh address %q is not an IPv4 CIDR", address)
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones != 32 {
		return fmt.Errorf("assigned mesh address %q must be a /32", address)
	}
	_, mesh, _ := net.ParseCIDR(meshCIDR)
	if !mesh.Contains(ip) {
		return fmt.Errorf("assigned mesh address %q is outside %s", address, meshCIDR)
	}
	return nil
}

func generateKeyPair() (string, string, error) {
	var private, public [32]byte
	if _, err := rand.Read(private[:]); err != nil {
		return "", "", fmt.Errorf("generate WireGuard private key: %w", err)
	}
	private[0] &= 248
	private[31] &= 127
	private[31] |= 64
	curve25519.ScalarBaseMult(&public, &private)
	return base64.StdEncoding.EncodeToString(private[:]), base64.StdEncoding.EncodeToString(public[:]), nil
}

func decodeState(data []byte) (State, error) {
	var state State
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return State{}, fmt.Errorf("decode WireKube leaf state: %w", err)
	}
	if state.Version != stateVersion || state.PeerName == "" || state.PeerName != state.DisplayName || state.EnrollmentID == "" {
		return State{}, fmt.Errorf("WireKube leaf state has an invalid identity")
	}
	if err := validateKeyPair(state.PrivateKey, state.PublicKey); err != nil {
		return State{}, err
	}
	return state, nil
}

func validateKeyPair(privateValue, publicValue string) error {
	privateBytes, err := base64.StdEncoding.DecodeString(privateValue)
	if err != nil || len(privateBytes) != 32 {
		return fmt.Errorf("WireKube private key is invalid")
	}
	if err := validateKey(publicValue); err != nil {
		return fmt.Errorf("WireKube public key is invalid: %w", err)
	}
	var private, expectedPublic [32]byte
	copy(private[:], privateBytes)
	curve25519.ScalarBaseMult(&expectedPublic, &private)
	if base64.StdEncoding.EncodeToString(expectedPublic[:]) != publicValue {
		return fmt.Errorf("WireKube public key does not match the private key")
	}
	return nil
}

func validateKey(value string) error {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return fmt.Errorf("expected a base64-encoded 32-byte key")
	}
	return nil
}

func writeState(directory string, state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(directory, stateFileName), append(data, '\n'))
}

func writePrivate(name string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(name), ".wirekube-leaf-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, name)
}

func lastConditionMessage(peer *unstructured.Unstructured) string {
	conditions, _, _ := unstructured.NestedSlice(peer.Object, "status", "conditions")
	if len(conditions) == 0 {
		return "no condition details"
	}
	condition, ok := conditions[len(conditions)-1].(map[string]any)
	if !ok {
		return "invalid condition details"
	}
	reason, _ := condition["reason"].(string)
	message, _ := condition["message"].(string)
	return strings.TrimSpace(strings.TrimSpace(reason + ": " + message))
}

func IsNotInstalled(err error) bool {
	return err != nil && (apierrors.IsNotFound(err) || strings.Contains(err.Error(), "is not installed"))
}
