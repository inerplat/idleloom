package wirekube

import (
	"context"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestInspectValidatesConnectedLeafPrerequisites(t *testing.T) {
	client := newTestClient(testMesh(2))
	report, err := Inspect(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if report.MeshCIDR != "172.31.240.0/20" || report.RelayProvider != "managed" || report.ReadyPeers != 2 {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("warnings = %v", report.Warnings)
	}
}

func TestInspectRejectsRelayDisabled(t *testing.T) {
	mesh := testMesh(1)
	_ = unstructured.SetNestedField(mesh.Object, "never", "spec", "relay", "mode")
	_, err := Inspect(context.Background(), newTestClient(mesh))
	if err == nil {
		t.Fatal("Inspect accepted a mesh with relay disabled")
	}
}

func TestMeshIPForNameMatchesWireKubeContractVector(t *testing.T) {
	address, err := meshIPForName("idleloom-connectivity-e2e", "198.18.18.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if address != "198.18.18.52/32" {
		t.Fatalf("address = %q, want WireKube contract vector", address)
	}
}

func TestEnrollCreatesStableOwnedExternalPeer(t *testing.T) {
	client := newTestClient(testMesh(1))
	expectedAddress, err := meshIPForName("idleloom-mac-one", "172.31.240.0/20")
	if err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("create", ExternalPeersGVR.Resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
		object := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID(types.UID("peer-uid"))
		object.Object["status"] = map[string]any{
			"phase":               "Active",
			"assignedMeshIP":      expectedAddress,
			"relayEndpoint":       "relay.example.test:3478",
			"ingressPublicKey":    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			"allowedDestinations": []any{"172.31.240.0/20"},
			"mtu":                 int64(1248),
		}
		return false, nil, nil
	})
	directory := t.TempDir()
	config := EnrollConfig{
		Dynamic: client, HostID: "mac-one", EnrollmentID: "enrollment-one",
		StateDirectory: directory, APIEndpoint: "https://203.0.113.10:6443", WaitTimeout: time.Second,
	}
	first, err := Enroll(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if first.PeerName != "idleloom-mac-one" || first.PeerUID != "peer-uid" || first.AssignedMeshIP != expectedAddress {
		t.Fatalf("state = %#v", first)
	}
	peer, err := client.Resource(ExternalPeersGVR).Get(context.Background(), first.PeerName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	allowed, _, _ := unstructured.NestedStringSlice(peer.Object, "spec", "allowedDestinations")
	if len(allowed) != 1 || allowed[0] != "172.31.240.0/20" {
		t.Fatalf("allowed destinations = %v", allowed)
	}
	second, err := Enroll(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if second.PrivateKey != first.PrivateKey || second.PublicKey != first.PublicKey || second.PeerUID != first.PeerUID {
		t.Fatal("idempotent enrollment changed the leaf identity")
	}
	stored, err := ReadState(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored, second) {
		t.Fatalf("stored state = %#v, want %#v", stored, second)
	}
	refreshed, err := RefreshState(context.Background(), client, directory)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(refreshed, second) {
		t.Fatalf("refreshed state = %#v, want %#v", refreshed, second)
	}
}

func TestRecreateForStaleIngressPreservesExternalIdentity(t *testing.T) {
	state := State{
		PeerName: "idleloom-mac-one", DisplayName: "idleloom-mac-one", EnrollmentID: "enrollment-one",
		PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", MeshCIDR: "172.31.240.0/20",
		PeerUID: types.UID("old-peer-uid"),
	}
	desired := desiredExternalPeer(state, "mac-one", state.MeshCIDR)
	existing := desired.DeepCopy()
	existing.SetUID(state.PeerUID)
	existing.Object["status"] = map[string]any{"phase": "Active", "ingressPeerName": "stale-node"}
	ingress := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wirekube.io/v1alpha1", "kind": "WireKubePeer",
		"metadata": map[string]any{"name": "stale-node"},
		"status":   map[string]any{"connected": true, "lastHandshake": "2020-01-01T00:00:00Z"},
	}}
	client := newTestClient(existing, ingress)
	client.PrependReactor("create", ExternalPeersGVR.Resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
		object := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID(types.UID("new-peer-uid"))
		return false, nil, nil
	})
	recreated, created, err := recreateForStaleIngress(context.Background(), client, desired, state, existing, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !created || recreated.GetUID() != types.UID("new-peer-uid") {
		t.Fatalf("recreated peer = %#v created=%t", recreated, created)
	}
	if publicKey, _, _ := unstructured.NestedString(recreated.Object, "spec", "publicKey"); publicKey != state.PublicKey {
		t.Fatal("stale ingress repair changed the external WireGuard identity")
	}
}

func TestEnrollRejectsPeerOwnedByAnotherEnrollment(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateState(directory, "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	peer := desiredExternalPeer(state, "mac-one", "172.31.240.0/20")
	peer.SetUID(types.UID("peer-uid"))
	annotations := peer.GetAnnotations()
	annotations["ai.idleloom.io/enrollment-id"] = "someone-else"
	peer.SetAnnotations(annotations)
	client := newTestClient(testMesh(1), peer)
	_, err = Enroll(context.Background(), EnrollConfig{
		Dynamic: client, HostID: "mac-one", EnrollmentID: "enrollment-one",
		StateDirectory: directory, APIEndpoint: "https://203.0.113.10:6443", WaitTimeout: time.Second,
	})
	if err == nil {
		t.Fatal("Enroll adopted a peer owned by another enrollment")
	}
}

func TestEnrollRollsBackNewClaimWhenPeerNameIsForeign(t *testing.T) {
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wirekube.io/v1alpha1", "kind": "WireKubeExternalPeer",
		"metadata": map[string]any{
			"name": "idleloom-mac-one", "labels": map[string]any{"app.kubernetes.io/managed-by": "another-tool"},
		},
		"spec": map[string]any{"displayName": "idleloom-mac-one", "publicKey": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="},
	}}
	directory := t.TempDir()
	client := newTestClient(testMesh(1), foreign)
	_, err := Enroll(context.Background(), EnrollConfig{
		Dynamic: client, HostID: "mac-one", EnrollmentID: "enrollment-one",
		StateDirectory: directory, APIEndpoint: "https://203.0.113.10:6443", WaitTimeout: time.Second,
	})
	if err == nil {
		t.Fatal("Enroll adopted a foreign peer")
	}
	state, readErr := ReadState(directory)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if _, getErr := client.Resource(IPClaimsGVR).Namespace(ipClaimNamespace).Get(context.Background(), state.MeshIPClaimName, metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Fatalf("mesh IP claim leaked after foreign peer conflict: %v", getErr)
	}
}

func TestEnrollRejectsDeterministicExternalPeerCollision(t *testing.T) {
	other := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wirekube.io/v1alpha1", "kind": "WireKubeExternalPeer",
		"metadata": map[string]any{"name": "other-peer"},
		"spec":     map[string]any{"displayName": "idleloom-mac-one"},
	}}
	_, err := Enroll(context.Background(), EnrollConfig{
		Dynamic: newTestClient(testMesh(1), other), HostID: "mac-one", EnrollmentID: "enrollment-one",
		StateDirectory: t.TempDir(), APIEndpoint: "https://203.0.113.10:6443", WaitTimeout: time.Second,
	})
	if err == nil {
		t.Fatal("Enroll accepted a deterministic ExternalPeer mesh IP collision")
	}
}

func TestEnrollRejectsWireKubePeerAllowedIPCollision(t *testing.T) {
	expectedAddress, err := meshIPForName("idleloom-mac-one", "172.31.240.0/20")
	if err != nil {
		t.Fatal(err)
	}
	peer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wirekube.io/v1alpha1", "kind": "WireKubePeer",
		"metadata": map[string]any{"name": "worker-one"},
		"spec":     map[string]any{"allowedIPs": []any{expectedAddress}},
	}}
	_, err = Enroll(context.Background(), EnrollConfig{
		Dynamic: newTestClient(testMesh(1), peer), HostID: "mac-one", EnrollmentID: "enrollment-one",
		StateDirectory: t.TempDir(), APIEndpoint: "https://203.0.113.10:6443", WaitTimeout: time.Second,
	})
	if err == nil {
		t.Fatal("Enroll accepted a WireKubePeer allowed IP collision")
	}
}

func TestMeshIPClaimAtomicallyRejectsAnotherEnrollment(t *testing.T) {
	client := newTestClient()
	first, err := loadOrCreateState(t.TempDir(), "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateState(t.TempDir(), "idleloom-mac-two", "enrollment-two")
	if err != nil {
		t.Fatal(err)
	}
	address := "172.31.241.10/32"
	if _, _, err := ensureMeshIPClaim(context.Background(), client, first, address); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureMeshIPClaim(context.Background(), client, second, address); err == nil {
		t.Fatal("a second enrollment acquired the same mesh IP claim")
	}
}

func TestEnrollPersistsPeerUIDBeforeAllocationCompletes(t *testing.T) {
	client := newTestClient(testMesh(1))
	client.PrependReactor("create", ExternalPeersGVR.Resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
		object := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID(types.UID("pending-peer-uid"))
		object.Object["status"] = map[string]any{"phase": "Pending"}
		return false, nil, nil
	})
	directory := t.TempDir()
	_, err := Enroll(context.Background(), EnrollConfig{
		Dynamic: client, HostID: "mac-one", EnrollmentID: "enrollment-one",
		StateDirectory: directory, APIEndpoint: "https://203.0.113.10:6443", WaitTimeout: 10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Enroll unexpectedly completed while allocation was pending")
	}
	state, readErr := ReadState(directory)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if state.PeerUID != "pending-peer-uid" {
		t.Fatalf("persisted peer UID = %q", state.PeerUID)
	}
}

func TestEnrollRollsBackNewCredentialWhenPostAllocationCollisionAppears(t *testing.T) {
	client := newTestClient(testMesh(1))
	expectedAddress, err := meshIPForName("idleloom-mac-one", "172.31.240.0/20")
	if err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("create", ExternalPeersGVR.Resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
		object := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID("peer-uid")
		object.Object["status"] = map[string]any{
			"phase": "Active", "assignedMeshIP": expectedAddress, "relayEndpoint": "relay.example.test:3478",
			"ingressPublicKey":    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			"allowedDestinations": []any{"172.31.240.0/20"}, "mtu": int64(1248),
		}
		return false, nil, nil
	})
	listCalls := 0
	client.PrependReactor("list", ExternalPeersGVR.Resource, func(clienttesting.Action) (bool, runtime.Object, error) {
		listCalls++
		if listCalls < 3 {
			return false, nil, nil
		}
		collision := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "wirekube.io/v1alpha1", "kind": "WireKubeExternalPeer",
			"metadata": map[string]any{"name": "racing-peer"},
			"spec":     map[string]any{"displayName": "idleloom-mac-one"},
			"status":   map[string]any{"assignedMeshIP": expectedAddress},
		}}
		return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*collision}}, nil
	})
	directory := t.TempDir()
	_, err = Enroll(context.Background(), EnrollConfig{
		Dynamic: client, HostID: "mac-one", EnrollmentID: "enrollment-one",
		StateDirectory: directory, APIEndpoint: "https://203.0.113.10:6443", WaitTimeout: time.Second,
	})
	if err == nil {
		t.Fatal("Enroll accepted a collision introduced during allocation")
	}
	if _, getErr := client.Resource(ExternalPeersGVR).Get(context.Background(), "idleloom-mac-one", metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Fatalf("new ExternalPeer was not rolled back: %v", getErr)
	}
	state, readErr := ReadState(directory)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if _, getErr := client.Resource(IPClaimsGVR).Namespace(ipClaimNamespace).Get(context.Background(), state.MeshIPClaimName, metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Fatalf("new mesh IP claim was not rolled back: %v", getErr)
	}
}

func TestRevokeDeletesOwnedPeerBeforePrivateState(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateState(directory, "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	state.MeshCIDR = "172.31.240.0/20"
	state.PeerUID = "peer-uid"
	state.KubernetesAPIEndpoint = "https://203.0.113.10:6443"
	expectedAddress, err := meshIPForName(state.DisplayName, state.MeshCIDR)
	if err != nil {
		t.Fatal(err)
	}
	claimClient := newTestClient()
	claim, _, err := ensureMeshIPClaim(context.Background(), claimClient, state, expectedAddress)
	if err != nil {
		t.Fatal(err)
	}
	state.MeshIPClaimName = claim.GetName()
	state.MeshIPClaimUID = claim.GetUID()
	if err := writeState(directory, state); err != nil {
		t.Fatal(err)
	}
	peer := desiredExternalPeer(state, "mac-one", state.MeshCIDR)
	peer.SetUID(state.PeerUID)
	client := newTestClient(testMesh(1), peer, claim.DeepCopy())
	if err := Revoke(context.Background(), RevokeConfig{
		Dynamic: client, StateDirectory: directory, RuntimeDirectory: t.TempDir(), WaitTimeout: time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	if exists, err := HasState(directory); err != nil || exists {
		t.Fatalf("private state remains after revoke: exists=%v err=%v", exists, err)
	}
}

func TestRevokeRecoversPeerUIDAfterCreateStateWriteWindow(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateState(directory, "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	state.MeshCIDR = "172.31.240.0/20"
	state.KubernetesAPIEndpoint = "https://203.0.113.10:6443"
	expectedAddress, err := meshIPForName(state.DisplayName, state.MeshCIDR)
	if err != nil {
		t.Fatal(err)
	}
	claim, _, err := ensureMeshIPClaim(context.Background(), newTestClient(), state, expectedAddress)
	if err != nil {
		t.Fatal(err)
	}
	state.MeshIPClaimName = claim.GetName()
	state.MeshIPClaimUID = claim.GetUID()
	if err := writeState(directory, state); err != nil {
		t.Fatal(err)
	}
	peer := desiredExternalPeer(state, "mac-one", state.MeshCIDR)
	peer.SetUID("recovered-peer-uid")
	client := newTestClient(testMesh(1), peer, claim.DeepCopy())
	if err := Revoke(context.Background(), RevokeConfig{
		Dynamic: client, StateDirectory: directory, RuntimeDirectory: t.TempDir(), WaitTimeout: time.Second,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeLeavesForeignPeerAndRecoversOwnedClaim(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateState(directory, "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	state.MeshCIDR = "172.31.240.0/20"
	expectedAddress, err := meshIPForName(state.DisplayName, state.MeshCIDR)
	if err != nil {
		t.Fatal(err)
	}
	claim, _, err := ensureMeshIPClaim(context.Background(), newTestClient(), state, expectedAddress)
	if err != nil {
		t.Fatal(err)
	}
	state.MeshIPClaimName = claim.GetName()
	state.MeshIPClaimUID = claim.GetUID()
	if err := writeState(directory, state); err != nil {
		t.Fatal(err)
	}
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wirekube.io/v1alpha1", "kind": "WireKubeExternalPeer",
		"metadata": map[string]any{"name": state.PeerName, "uid": "foreign-uid"},
		"spec":     map[string]any{"displayName": state.DisplayName},
	}}
	client := newTestClient(foreign, claim.DeepCopy())
	if err := Revoke(context.Background(), RevokeConfig{
		Dynamic: client, StateDirectory: directory, RuntimeDirectory: t.TempDir(), WaitTimeout: time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resource(ExternalPeersGVR).Get(context.Background(), state.PeerName, metav1.GetOptions{}); err != nil {
		t.Fatalf("foreign peer was deleted: %v", err)
	}
	if exists, err := HasState(directory); err != nil || exists {
		t.Fatalf("owned local state remains: exists=%v err=%v", exists, err)
	}
}

func TestReadStateRejectsMismatchedKeyPair(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateState(directory, "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	state.PublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	if err := writeState(directory, state); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(directory); err == nil {
		t.Fatal("ReadState accepted a mismatched public key")
	}
}

func newTestClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		MeshesGVR: "WireKubeMeshList", PeersGVR: "WireKubePeerList", ExternalPeersGVR: "WireKubeExternalPeerList",
		IPClaimsGVR: "LeaseList",
	})
	client.PrependReactor("create", IPClaimsGVR.Resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
		object := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID(types.UID("claim-" + object.GetName()))
		return false, nil, nil
	})
	for _, object := range objects {
		unstructuredObject := object.(*unstructured.Unstructured)
		resource := ExternalPeersGVR
		namespace := ""
		if unstructuredObject.GetKind() == "WireKubeMesh" {
			resource = MeshesGVR
		} else if unstructuredObject.GetKind() == "WireKubePeer" {
			resource = PeersGVR
		} else if unstructuredObject.GetKind() == "Lease" {
			resource = IPClaimsGVR
			namespace = ipClaimNamespace
		}
		if err := client.Tracker().Create(resource, object, namespace); err != nil {
			panic(err)
		}
	}
	return client
}

func testMesh(readyPeers int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wirekube.io/v1alpha1", "kind": "WireKubeMesh",
		"metadata": map[string]any{"name": defaultMeshName},
		"spec": map[string]any{
			"meshCIDR": "172.31.240.0/20",
			"relay":    map[string]any{"mode": "auto", "provider": "managed"},
		},
		"status": map[string]any{"readyPeers": readyPeers},
	}}
}
