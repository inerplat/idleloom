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
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"golang.org/x/crypto/curve25519"
)

const (
	ConnectivityAPIOnly  = "api-only"
	ConnectivityWireKube = "wirekube"

	managedBy       = "idleloom"
	stateVersion    = 1
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
	MeshName       string
	MeshCIDR       string
	MTU            int32
	RelayMode      string
	RelayProvider  string
	RelayTransport string
	RelayEndpoint  string
	ReadyPeers     int64
	Warnings       []string
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
	PeerMode              string    `json:"peerMode,omitempty"`
	RelayTransport        string    `json:"relayTransport,omitempty"`
	RelayTokenAudience    string    `json:"relayTokenAudience,omitempty"`
	PeerNamespace         string    `json:"peerNamespace,omitempty"`
	PeerServiceAccount    string    `json:"peerServiceAccount,omitempty"`
	LinkKubeconfig        string    `json:"linkKubeconfig,omitempty"`
}

type EnrollConfig struct {
	Dynamic        dynamic.Interface
	Kubernetes     kubernetes.Interface
	REST           *rest.Config
	HostID         string
	EnrollmentID   string
	Namespace      string
	StateDirectory string
	APIEndpoint    string
	TokenDuration  time.Duration
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
			return DoctorReport{}, fmt.Errorf("the WireKubeMesh/%s is not installed", defaultMeshName)
		}
		return DoctorReport{}, fmt.Errorf("get WireKubeMesh/%s: %w", defaultMeshName, err)
	}
	report := DoctorReport{MeshName: mesh.GetName()}
	report.MeshCIDR, _, _ = unstructured.NestedString(mesh.Object, "spec", "meshCIDR")
	if err := validateMeshCIDR(report.MeshCIDR); err != nil {
		return DoctorReport{}, err
	}
	mtu, _, _ := unstructured.NestedInt64(mesh.Object, "spec", "mtu")
	if mtu == 0 {
		mtu = 1420
	}
	if mtu < 576 || mtu > 1420 {
		return DoctorReport{}, fmt.Errorf("the WireKube mesh MTU %d is unsupported", mtu)
	}
	report.MTU = int32(mtu)
	report.RelayMode, _, _ = unstructured.NestedString(mesh.Object, "spec", "relay", "mode")
	report.RelayProvider, _, _ = unstructured.NestedString(mesh.Object, "spec", "relay", "provider")
	if report.RelayMode == "" {
		report.RelayMode = "auto"
	}
	if report.RelayMode == "never" {
		return DoctorReport{}, fmt.Errorf("the WireKube relay mode is never; connected leaf requires a relay")
	}
	if report.RelayProvider != "managed" && report.RelayProvider != "external" {
		return DoctorReport{}, fmt.Errorf("the WireKube relay provider %q is unsupported", report.RelayProvider)
	}
	report.ReadyPeers, _, _ = unstructured.NestedInt64(mesh.Object, "status", "readyPeers")
	if report.ReadyPeers < 1 {
		report.Warnings = append(report.Warnings, "WireKube reports no ready mesh peers")
	}
	if _, err := client.Resource(PeersGVR).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		if apierrors.IsNotFound(err) {
			return DoctorReport{}, fmt.Errorf("the WireKubePeer API is not installed")
		}
		return DoctorReport{}, fmt.Errorf("list WireKubePeer resources: %w", err)
	}
	report.RelayTransport, report.RelayEndpoint, err = relayDialTarget(ctx, client, mesh)
	if err != nil {
		return DoctorReport{}, err
	}
	return report, nil
}

func Enroll(ctx context.Context, config EnrollConfig) (State, error) {
	if config.Dynamic == nil || config.Kubernetes == nil || config.REST == nil || config.HostID == "" || config.EnrollmentID == "" || config.Namespace == "" || config.StateDirectory == "" || config.APIEndpoint == "" {
		return State{}, fmt.Errorf("dynamic and Kubernetes clients, REST config, host ID, enrollment ID, namespace, state directory, and API endpoint are required")
	}
	if err := validateAPIEndpoint(config.APIEndpoint); err != nil {
		return State{}, err
	}
	report, err := Inspect(ctx, config.Dynamic)
	if err != nil {
		return State{}, err
	}
	peerName := "idleloom-" + config.HostID
	if _, err := config.Kubernetes.CoreV1().Nodes().Get(ctx, peerName, metav1.GetOptions{}); err == nil {
		return State{}, fmt.Errorf("the WireKube peer name %s conflicts with an existing Kubernetes Node", peerName)
	} else if !apierrors.IsNotFound(err) {
		return State{}, fmt.Errorf("check WireKube peer name against Kubernetes Nodes: %w", err)
	}
	state, err := loadOrCreateState(config.StateDirectory, peerName, config.EnrollmentID)
	if err != nil {
		return State{}, err
	}
	if state.MeshCIDR != "" && state.MeshCIDR != report.MeshCIDR {
		return State{}, fmt.Errorf("the WireKube mesh CIDR changed from %s to %s; repair connectivity before reenrolling", state.MeshCIDR, report.MeshCIDR)
	}
	state.MeshCIDR = report.MeshCIDR
	state.KubernetesAPIEndpoint = config.APIEndpoint
	state.PeerMode = peerModeWireKube
	state.RelayTransport = report.RelayTransport
	state.RelayEndpoint = report.RelayEndpoint
	state.RelayTokenAudience = relayTokenAudience(report.RelayTransport)
	state.PeerNamespace = config.Namespace
	state.PeerServiceAccount = peerServiceAccountName(state.PeerName)
	state.MTU = report.MTU
	state.AllowedDestinations = []string{report.MeshCIDR}
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
	state.AssignedMeshIP = expectedAddress
	state.IngressPublicKey = ""
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
	desired := desiredWireKubePeer(state, config.HostID, expectedAddress)
	peer, peerCreated, err := ensureWireKubePeer(ctx, config.Dynamic, desired, state)
	if err != nil {
		return State{}, errors.Join(err, rollbackNewWireKubeEnrollment(ctx, config.Dynamic, nil, claim, state, false, claimCreated))
	}
	state.PeerUID = peer.GetUID()
	if state.PeerUID == "" {
		err := fmt.Errorf("the WireKubePeer/%s has no UID", peer.GetName())
		return State{}, errors.Join(err, rollbackNewWireKubeEnrollment(ctx, config.Dynamic, peer, claim, state, peerCreated, claimCreated))
	}
	if err := writeState(config.StateDirectory, state); err != nil {
		return State{}, errors.Join(err, rollbackNewWireKubeEnrollment(ctx, config.Dynamic, peer, claim, state, peerCreated, claimCreated))
	}
	currentExpectedAddress, err := validateMeshIPAvailability(ctx, config.Dynamic, state.PeerName, state.DisplayName, report.MeshCIDR)
	if err != nil {
		return State{}, errors.Join(err, rollbackNewWireKubeEnrollment(ctx, config.Dynamic, peer, claim, state, peerCreated, claimCreated))
	}
	if currentExpectedAddress != expectedAddress {
		err := fmt.Errorf("deterministic WireKube mesh address changed during enrollment")
		return State{}, errors.Join(err, rollbackNewWireKubeEnrollment(ctx, config.Dynamic, peer, claim, state, peerCreated, claimCreated))
	}
	if err := ensurePeerIdentity(ctx, config.Kubernetes, state, config.HostID); err != nil {
		return State{}, errors.Join(err, rollbackNewWireKubeEnrollment(ctx, config.Dynamic, peer, claim, state, peerCreated, claimCreated))
	}
	duration := config.TokenDuration
	if duration <= 0 || duration > 24*time.Hour {
		duration = 8 * time.Hour
	}
	linkKubeconfig := filepath.Join(config.StateDirectory, linkKubeconfigName)
	if err := writePeerKubeconfig(ctx, config.Kubernetes, config.REST, state, linkKubeconfig, duration); err != nil {
		return State{}, errors.Join(err, rollbackNewWireKubeEnrollment(ctx, config.Dynamic, peer, claim, state, peerCreated, claimCreated))
	}
	state.LinkKubeconfig = linkKubeconfig
	if err := writeState(config.StateDirectory, state); err != nil {
		_ = os.Remove(linkKubeconfig)
		return State{}, errors.Join(err, rollbackNewWireKubeEnrollment(ctx, config.Dynamic, peer, claim, state, peerCreated, claimCreated))
	}
	return state, nil
}

func ReadState(directory string) (State, error) {
	path := StatePath(directory)
	info, err := os.Lstat(path)
	if err != nil {
		return State{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return State{}, fmt.Errorf("the WireKube leaf state must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return State{}, err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return State{}, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return State{}, fmt.Errorf("the WireKube leaf state changed while opening")
	}
	if openedInfo.Mode().Perm()&0o077 != 0 {
		return State{}, fmt.Errorf("the WireKube leaf state permissions must be 0600 or stricter")
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
	if state.PeerMode == peerModeWireKube {
		return refreshWireKubePeerState(ctx, client, directory, state)
	}
	return State{}, fmt.Errorf("legacy WireKubeExternalPeer state can only be revoked; delete and rejoin the host")
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

func hasOwnedPeerMetadata(peer *unstructured.Unstructured, state State) bool {
	return peer.GetLabels()["app.kubernetes.io/managed-by"] == managedBy &&
		peer.GetAnnotations()["ai.idleloom.io/enrollment-id"] == state.EnrollmentID
}

func validateLegacyExternalPeer(peer *unstructured.Unstructured, state State) error {
	if !hasOwnedPeerMetadata(peer, state) {
		return fmt.Errorf("the WireKubeExternalPeer/%s is not owned by this enrollment", peer.GetName())
	}
	if state.PeerUID != "" && peer.GetUID() != state.PeerUID {
		return fmt.Errorf("the WireKubeExternalPeer/%s identity changed", peer.GetName())
	}
	displayName, _, _ := unstructured.NestedString(peer.Object, "spec", "displayName")
	publicKey, _, _ := unstructured.NestedString(peer.Object, "spec", "publicKey")
	allowed, _, _ := unstructured.NestedStringSlice(peer.Object, "spec", "allowedDestinations")
	if displayName != state.DisplayName || publicKey != state.PublicKey || len(allowed) != 1 || allowed[0] != state.MeshCIDR {
		return fmt.Errorf("the WireKubeExternalPeer/%s does not match the legacy enrollment contract", peer.GetName())
	}
	return nil
}

func validateMeshCIDR(value string) error {
	ip, network, err := net.ParseCIDR(value)
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("the WireKube meshCIDR %q must be a valid IPv4 CIDR", value)
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones > 30 {
		return fmt.Errorf("the WireKube meshCIDR %q is too small", value)
	}
	if !isSafeMeshNetwork(network) {
		return fmt.Errorf("the WireKube meshCIDR %q must be contained in RFC1918, CGNAT, or benchmark address space", value)
	}
	return nil
}

func validateAPIEndpoint(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return fmt.Errorf("kubernetes API endpoint %q must be an HTTPS URL", value)
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
			return fmt.Errorf("the WireKube connectivity service is still running; stop it or rerun with --force")
		}
	}
	if state.PeerMode == peerModeWireKube {
		return revokeWireKubePeer(ctx, config, state)
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
			return fmt.Errorf("the WireKubeExternalPeer/%s identity changed", state.PeerName)
		}
		if err := validateLegacyExternalPeer(peer, state); err != nil {
			return err
		}
		uid := peer.GetUID()
		if uid == "" {
			return fmt.Errorf("the WireKubeExternalPeer/%s has no UID", state.PeerName)
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
		return State{}, fmt.Errorf("the WireKube leaf state has an invalid identity")
	}
	if state.PeerMode != "" && state.PeerMode != peerModeWireKube {
		return State{}, fmt.Errorf("the WireKube leaf state has an unsupported peer mode %q", state.PeerMode)
	}
	if err := validateKeyPair(state.PrivateKey, state.PublicKey); err != nil {
		return State{}, err
	}
	return state, nil
}

func validateKeyPair(privateValue, publicValue string) error {
	privateBytes, err := base64.StdEncoding.DecodeString(privateValue)
	if err != nil || len(privateBytes) != 32 {
		return fmt.Errorf("the WireKube private key is invalid")
	}
	if err := validateKey(publicValue); err != nil {
		return fmt.Errorf("the WireKube public key is invalid: %w", err)
	}
	var private, expectedPublic [32]byte
	copy(private[:], privateBytes)
	curve25519.ScalarBaseMult(&expectedPublic, &private)
	if base64.StdEncoding.EncodeToString(expectedPublic[:]) != publicValue {
		return fmt.Errorf("the WireKube public key does not match the private key")
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
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if _, err := temporary.Write(data); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, name)
}

func IsNotInstalled(err error) bool {
	return err != nil && (apierrors.IsNotFound(err) || strings.Contains(err.Error(), "is not installed"))
}
