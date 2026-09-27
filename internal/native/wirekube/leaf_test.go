package wirekube

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

	"github.com/inerplat/idleloom/internal/meshclaim"
	"github.com/inerplat/idleloom/internal/meship"
)

func TestInspectValidatesWireKubePeerRelayPrerequisites(t *testing.T) {
	report, err := Inspect(context.Background(), newTestClient(testMesh(2)))
	if err != nil {
		t.Fatal(err)
	}
	if report.MeshCIDR != "172.31.240.0/20" || report.RelayProvider != "managed" || report.RelayTransport != "wss" || report.RelayEndpoint != "wss://relay.example.test/relay" || report.ReadyPeers != 2 {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("warnings = %v", report.Warnings)
	}
}

func TestInspectDiscoversManagedTCPRelayLoadBalancer(t *testing.T) {
	mesh := testMesh(1)
	_ = unstructured.SetNestedField(mesh.Object, "tcp", "spec", "relay", "managed", "transport")
	unstructured.RemoveNestedField(mesh.Object, "spec", "relay", "managed", "controlEndpoint")
	service := relayService("relay.example.test")
	report, err := Inspect(context.Background(), newTestClient(mesh, service))
	if err != nil {
		t.Fatal(err)
	}
	if report.RelayTransport != "tcp" || report.RelayEndpoint != "relay.example.test:3478" {
		t.Fatalf("report = %#v", report)
	}
}

func TestInspectRejectsRelayDisabled(t *testing.T) {
	mesh := testMesh(1)
	_ = unstructured.SetNestedField(mesh.Object, "never", "spec", "relay", "mode")
	if _, err := Inspect(context.Background(), newTestClient(mesh)); err == nil {
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

func TestEnrollCreatesStableOwnedWireKubePeerAndRestrictedIdentity(t *testing.T) {
	dynamicClient := newTestClient(testMesh(1))
	dynamicClient.PrependReactor("create", PeersGVR.Resource, assignUID("peer-uid"))
	kubernetesClient := newKubernetesTestClient()
	directory := t.TempDir()
	config := testEnrollConfig(directory, dynamicClient, kubernetesClient)

	first, err := Enroll(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	expectedAddress, _ := meshIPForName("idleloom-mac-one", "172.31.240.0/20")
	if first.PeerMode != peerModeWireKube || first.PeerUID != "peer-uid" || first.AssignedMeshIP != expectedAddress || first.RelayTransport != "wss" {
		t.Fatalf("state = %#v", first)
	}
	peer, err := dynamicClient.Resource(PeersGVR).Get(context.Background(), first.PeerName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	allowed, _, _ := unstructured.NestedStringSlice(peer.Object, "spec", "allowedIPs")
	if !reflect.DeepEqual(allowed, []string{expectedAddress}) {
		t.Fatalf("allowed IPs = %v", allowed)
	}
	if _, err := kubernetesClient.CoreV1().ServiceAccounts(first.PeerNamespace).Get(context.Background(), first.PeerServiceAccount, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	clusterRole, err := kubernetesClient.RbacV1().ClusterRoles().Get(context.Background(), peerRBACName(first.PeerName), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range clusterRole.Rules {
		for _, verb := range rule.Verbs {
			if (verb == "patch" || verb == "update" || verb == "delete" || verb == "create") &&
				(!reflect.DeepEqual(rule.Resources, []string{"wirekubepeers/status"}) || !reflect.DeepEqual(rule.ResourceNames, []string{first.PeerName})) {
				t.Fatalf("peer identity has an unexpected write rule: %#v", rule)
			}
		}
	}
	if first.LinkKubeconfig == "" {
		t.Fatal("link kubeconfig was not persisted")
	}

	second, err := Enroll(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if second.PrivateKey != first.PrivateKey || second.PublicKey != first.PublicKey || second.PeerUID != first.PeerUID {
		t.Fatal("idempotent enrollment changed the peer identity")
	}
	refreshed, err := RefreshState(context.Background(), dynamicClient, directory)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(refreshed, second) {
		t.Fatalf("refreshed state = %#v, want %#v", refreshed, second)
	}
}

func TestRelayTokenUsesWireKubeAudience(t *testing.T) {
	client := fake.NewClientset()
	var audiences []string
	client.PrependReactor("create", "serviceaccounts", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		request := action.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenRequest)
		audiences = append([]string(nil), request.Spec.Audiences...)
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{
			Token: "relay-token", ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Hour)),
		}}, nil
	})
	state := State{
		RelayTransport: "wss", RelayTokenAudience: peerRelayAudience,
		PeerNamespace: "idleloom-host-mac-one", PeerServiceAccount: peerServiceAccountName("idleloom-mac-one"),
	}
	expires, err := WriteRelayToken(context.Background(), client, state, t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(audiences, []string{peerRelayAudience}) || expires.IsZero() {
		t.Fatalf("audiences=%v expires=%v", audiences, expires)
	}
}

func TestEnrollRejectsPeerOwnedByAnotherEnrollmentAndRollsBackClaim(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateState(directory, "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	address, _ := meshIPForName(state.PeerName, "172.31.240.0/20")
	peer := desiredWireKubePeer(state, "mac-one", address)
	peer.SetUID("foreign-peer")
	annotations := peer.GetAnnotations()
	annotations["ai.idleloom.io/enrollment-id"] = "someone-else"
	peer.SetAnnotations(annotations)
	dynamicClient := newTestClient(testMesh(1), peer)
	_, err = Enroll(context.Background(), testEnrollConfig(directory, dynamicClient, newKubernetesTestClient()))
	if err == nil {
		t.Fatal("Enroll adopted a foreign WireKubePeer")
	}
	stored, readErr := ReadState(directory)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if _, getErr := dynamicClient.Resource(meshclaim.LeasesGVR).Namespace(stored.MeshIPClaimNamespaceOrDefault()).Get(context.Background(), stored.MeshIPClaimName, metav1.GetOptions{}); getErr == nil {
		t.Fatal("mesh IP claim leaked after foreign peer conflict")
	}
}

func TestEnrollRejectsWireKubePeerAllowedIPCollision(t *testing.T) {
	expectedAddress, _ := meshIPForName("idleloom-mac-one", "172.31.240.0/20")
	peer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wirekube.io/v1alpha1", "kind": "WireKubePeer",
		"metadata": map[string]any{"name": "worker-one"},
		"spec":     map[string]any{"allowedIPs": []any{expectedAddress}},
	}}
	directory := t.TempDir()
	_, err := Enroll(context.Background(), testEnrollConfig(directory, newTestClient(testMesh(1), peer), newKubernetesTestClient()))
	if err == nil {
		t.Fatal("Enroll accepted a WireKubePeer mesh IP collision")
	}
}

func TestEnrollRejectsPeerNameThatMatchesKubernetesNode(t *testing.T) {
	kubernetesClient := newKubernetesTestClient()
	if _, err := kubernetesClient.CoreV1().Nodes().Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "idleloom-mac-one"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := Enroll(context.Background(), testEnrollConfig(t.TempDir(), newTestClient(testMesh(1)), kubernetesClient))
	if err == nil {
		t.Fatal("Enroll accepted a WireKube peer name that matches a Kubernetes Node")
	}
}

// TestMeshIPClaimAtomicallyRejectsAnotherEnrollment: on a mesh that does not
// arbitrate addresses there is no second choice, so two hosts whose names
// produce one address cannot both enrol. The create is what settles it.
func TestMeshIPClaimAtomicallyRejectsAnotherEnrollment(t *testing.T) {
	client := newTestClient()
	report := DoctorReport{MeshName: defaultMeshName, MeshCIDR: "172.31.240.0/20"}
	first, second := collidingStates(t)

	if _, _, _, err := claimMeshIP(context.Background(), client, first, report); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := claimMeshIP(context.Background(), client, second, report)
	if err == nil {
		t.Fatal("a second enrollment acquired the same mesh IP claim")
	}
	if !strings.Contains(err.Error(), "allocator") {
		t.Errorf("error %q does not say which setting makes the collision recoverable", err)
	}
}

// TestMeshIPClaimMovesOnAnArbitratingMesh is the same collision on a mesh that
// resolves them: the second host takes another address instead of failing.
func TestMeshIPClaimMovesOnAnArbitratingMesh(t *testing.T) {
	client := newTestClient()
	report := DoctorReport{MeshName: defaultMeshName, MeshCIDR: "172.31.240.0/20", AddressAllocation: "allocator"}
	first, second := collidingStates(t)

	held, _, _, err := claimMeshIP(context.Background(), client, first, report)
	if err != nil {
		t.Fatal(err)
	}
	moved, claim, created, err := claimMeshIP(context.Background(), client, second, report)
	if err != nil {
		t.Fatalf("the second enrollment was refused instead of moved: %v", err)
	}
	if moved == held {
		t.Fatalf("both hosts took %s", held)
	}
	if !created {
		t.Error("the claim was reported as adopted, but it was made here")
	}
	if claim.Namespace != meshclaim.DefaultNamespace {
		t.Errorf("claim namespace = %q, want WireKube's %q; a claim nowhere near WireKube's arbitrates nothing",
			claim.Namespace, meshclaim.DefaultNamespace)
	}
	if claim.Name != meshclaim.ClaimName(defaultMeshName, moved) {
		t.Errorf("claim name = %q, want %q", claim.Name, meshclaim.ClaimName(defaultMeshName, moved))
	}
	if !meship.Contains(moved, report.MeshCIDR) {
		t.Errorf("%s is not an address the mesh can hand out", moved)
	}
}

// TestMeshIPClaimIsIdempotent: a re-run of an interrupted join keeps the
// address rather than consuming a second one.
func TestMeshIPClaimIsIdempotent(t *testing.T) {
	client := newTestClient()
	report := DoctorReport{MeshName: defaultMeshName, MeshCIDR: "172.31.240.0/20", AddressAllocation: "allocator"}
	state, err := loadOrCreateState(t.TempDir(), "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	first, _, created, err := claimMeshIP(context.Background(), client, state, report)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("the first claim was reported as adopted")
	}
	state.AssignedMeshIP = first
	again, _, createdAgain, err := claimMeshIP(context.Background(), client, state, report)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Errorf("the second run moved from %s to %s", first, again)
	}
	if createdAgain {
		t.Error("the second run reported a new claim rather than adopting its own")
	}
}

// collidingStates returns two enrollments whose display names produce the same
// mesh address, which is the situation the allocator exists for.
func collidingStates(t *testing.T) (State, State) {
	t.Helper()
	first, err := loadOrCreateState(t.TempDir(), "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateState(t.TempDir(), "idleloom-mac-two", "enrollment-two")
	if err != nil {
		t.Fatal(err)
	}
	// Forcing the display names to match is the shortest way to a guaranteed
	// collision; searching for a natural pair would test the hash, not this.
	second.DisplayName = first.DisplayName
	return first, second
}

func TestRevokeDeletesOwnedWireKubePeerClaimAndPrivateState(t *testing.T) {
	directory := t.TempDir()
	dynamicClient := newTestClient(testMesh(1))
	dynamicClient.PrependReactor("create", PeersGVR.Resource, assignUID("peer-uid"))
	state, err := Enroll(context.Background(), testEnrollConfig(directory, dynamicClient, newKubernetesTestClient()))
	if err != nil {
		t.Fatal(err)
	}
	if err := Revoke(context.Background(), RevokeConfig{Dynamic: dynamicClient, StateDirectory: directory, RuntimeDirectory: t.TempDir(), WaitTimeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err := dynamicClient.Resource(PeersGVR).Get(context.Background(), state.PeerName, metav1.GetOptions{}); err == nil {
		t.Fatal("WireKubePeer remains after revoke")
	}
	if exists, err := HasState(directory); err != nil || exists {
		t.Fatalf("private state remains after revoke: exists=%v err=%v", exists, err)
	}
}

func TestRevokeCleansLegacyExternalPeerEnrollment(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateState(directory, "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	state.MeshCIDR = "172.31.240.0/20"
	state.PeerUID = "legacy-peer-uid"
	peer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wirekube.io/v1alpha1", "kind": "WireKubeExternalPeer",
		"metadata": map[string]any{
			"name": state.PeerName, "uid": string(state.PeerUID),
			"labels":      map[string]any{"app.kubernetes.io/managed-by": managedBy},
			"annotations": map[string]any{"ai.idleloom.io/enrollment-id": state.EnrollmentID},
		},
		"spec": map[string]any{
			"displayName": state.DisplayName, "publicKey": state.PublicKey,
			"allowedDestinations": []any{state.MeshCIDR},
		},
	}}
	dynamicClient := newTestClient(peer)
	address, claim, _, err := claimMeshIP(context.Background(), dynamicClient, state,
		DoctorReport{MeshName: defaultMeshName, MeshCIDR: state.MeshCIDR})
	if err != nil {
		t.Fatal(err)
	}
	state.AssignedMeshIP = address
	state.MeshIPClaimName, state.MeshIPClaimUID, state.MeshIPClaimNamespace = claim.Name, claim.UID, claim.Namespace
	if err := writeState(directory, state); err != nil {
		t.Fatal(err)
	}

	if err := Revoke(context.Background(), RevokeConfig{Dynamic: dynamicClient, StateDirectory: directory, RuntimeDirectory: t.TempDir(), WaitTimeout: time.Second}); err != nil {
		t.Fatal(err)
	}
	if exists, err := HasState(directory); err != nil || exists {
		t.Fatalf("legacy state remains after revoke: exists=%v err=%v", exists, err)
	}
	if _, err := dynamicClient.Resource(meshclaim.LeasesGVR).Namespace(claim.Namespace).
		Get(context.Background(), claim.Name, metav1.GetOptions{}); err == nil {
		t.Error("the mesh address claim survived revoke, so the address stays out of the pool")
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

func testEnrollConfig(directory string, dynamicClient *dynamicfake.FakeDynamicClient, kubernetesClient *fake.Clientset) EnrollConfig {
	return EnrollConfig{
		Dynamic: dynamicClient, Kubernetes: kubernetesClient,
		REST: &rest.Config{
			Host:            "https://203.0.113.10:6443",
			TLSClientConfig: rest.TLSClientConfig{CAData: []byte("test-ca")},
		},
		HostID: "mac-one", EnrollmentID: "enrollment-one", Namespace: "idleloom-host-mac-one",
		StateDirectory: directory, APIEndpoint: "https://203.0.113.10:6443", WaitTimeout: time.Second,
	}
}

func newKubernetesTestClient() *fake.Clientset {
	client := fake.NewClientset()
	client.PrependReactor("create", "serviceaccounts", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{
			Token:               strings.Join([]string{"header", "eyJleHAiOjQxMDI0NDQ4MDB9", "signature"}, "."),
			ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Hour)),
		}}, nil
	})
	return client
}

func assignUID(uid types.UID) clienttesting.ReactionFunc {
	return func(action clienttesting.Action) (bool, runtime.Object, error) {
		object := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID(uid)
		return false, nil, nil
	}
}

func newTestClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		MeshesGVR: "WireKubeMeshList", PeersGVR: "WireKubePeerList", ExternalPeersGVR: "WireKubeExternalPeerList",
		meshclaim.LeasesGVR: "LeaseList", ServicesGVR: "ServiceList", DaemonSetsGVR: "DaemonSetList",
	})
	client.PrependReactor("create", meshclaim.LeasesGVR.Resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
		object := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured)
		object.SetUID(types.UID("claim-" + object.GetName()))
		return false, nil, nil
	})
	for _, object := range objects {
		unstructuredObject := object.(*unstructured.Unstructured)
		resource := ExternalPeersGVR
		namespace := ""
		switch unstructuredObject.GetKind() {
		case "WireKubeMesh":
			resource = MeshesGVR
		case "WireKubePeer":
			resource = PeersGVR
		case "Lease":
			resource = meshclaim.LeasesGVR
			namespace = unstructuredObject.GetNamespace()
			if namespace == "" {
				namespace = meshclaim.DefaultNamespace
			}
		case "Service":
			resource = ServicesGVR
			namespace = unstructuredObject.GetNamespace()
		case "DaemonSet":
			resource = DaemonSetsGVR
			namespace = unstructuredObject.GetNamespace()
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
			"meshCIDR": "172.31.240.0/20", "mtu": int64(1248),
			"relay": map[string]any{
				"mode": "auto", "provider": "managed",
				"managed": map[string]any{"transport": "wss", "controlEndpoint": "wss://relay.example.test/relay"},
			},
		},
		"status": map[string]any{"readyPeers": readyPeers},
	}}
}

func relayService(host string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{
			"name": "wirekube-relay", "namespace": "wirekube-system",
			"labels": map[string]any{"app.kubernetes.io/part-of": "wirekube"},
		},
		"spec": map[string]any{
			"type":  "LoadBalancer",
			"ports": []any{map[string]any{"name": "relay-tcp", "protocol": "TCP", "port": int64(3478)}},
		},
		"status": map[string]any{"loadBalancer": map[string]any{"ingress": []any{map[string]any{"hostname": host}}}},
	}}
}

// TestClaimMeshIPFollowsTheAgentNamespace. A claim arbitrates by name inside
// one namespace, so claiming in the chart default while WireKube runs
// somewhere else would give Idleloom a private pool that contends with
// nothing — the exact failure the allocator exists to remove, made silent.
func TestClaimMeshIPFollowsTheAgentNamespace(t *testing.T) {
	const namespace = "wirekube-prod"
	client := newTestClient(testMesh(1), wirekubeAgentDaemonSet(namespace))
	report, err := Inspect(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if report.AgentNamespace != namespace {
		t.Fatalf("AgentNamespace = %q, want %q", report.AgentNamespace, namespace)
	}
	state, err := loadOrCreateState(t.TempDir(), "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	_, claim, _, err := claimMeshIP(context.Background(), client, state, report)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Namespace != namespace {
		t.Errorf("claim namespace = %q, want the agent's %q", claim.Namespace, namespace)
	}
	if _, err := client.Resource(meshclaim.LeasesGVR).Namespace(namespace).
		Get(context.Background(), claim.Name, metav1.GetOptions{}); err != nil {
		t.Errorf("the claim is not in %s: %v", namespace, err)
	}
}

// TestClaimMeshIPFallsBackToTheChartDefault when no agent DaemonSet is
// visible, which is right for every default install and is all that can be
// done otherwise.
func TestClaimMeshIPFallsBackToTheChartDefault(t *testing.T) {
	client := newTestClient(testMesh(1))
	report, err := Inspect(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if report.AgentNamespace != "" {
		t.Fatalf("AgentNamespace = %q with no DaemonSet present", report.AgentNamespace)
	}
	state, err := loadOrCreateState(t.TempDir(), "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	_, claim, _, err := claimMeshIP(context.Background(), client, state, report)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Namespace != meshclaim.DefaultNamespace {
		t.Errorf("claim namespace = %q, want %q", claim.Namespace, meshclaim.DefaultNamespace)
	}
}

func wirekubeAgentDaemonSet(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "DaemonSet",
		"metadata": map[string]any{
			"name": "wirekube-agent", "namespace": namespace,
			"labels": map[string]any{
				"app.kubernetes.io/name":      "wirekube",
				"app.kubernetes.io/component": "agent",
			},
		},
	}}
}

// TestConfirmMeshIPReadsTheClaimWhereItWasWritten. Recomputing the namespace
// during the late re-check would disagree with the create on any installation
// that does not run WireKube in the chart's default namespace, and the
// enrollment would roll itself back over a claim that never went missing.
func TestConfirmMeshIPReadsTheClaimWhereItWasWritten(t *testing.T) {
	const namespace = "wirekube-prod"
	client := newTestClient(testMesh(1), wirekubeAgentDaemonSet(namespace))
	report, err := Inspect(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	state, err := loadOrCreateState(t.TempDir(), "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	address, claim, _, err := claimMeshIP(context.Background(), client, state, report)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Namespace == meshclaim.DefaultNamespace {
		t.Fatalf("test vector is useless: the claim landed in %s anyway", claim.Namespace)
	}
	if err := confirmMeshIPStillHeld(context.Background(), client, state, report, claim, address); err != nil {
		t.Errorf("the re-check failed against a claim that is right there: %v", err)
	}
}

// TestDiscoverAgentNamespaceRefusesToGuess. Two candidates is a coin toss
// whose losing side is silent, so it reports none and lets the caller warn.
func TestDiscoverAgentNamespaceRefusesToGuess(t *testing.T) {
	client := newTestClient(testMesh(1),
		wirekubeAgentDaemonSet("wirekube-system"),
		wirekubeAgentDaemonSet("wirekube-staging"))
	report, err := Inspect(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if report.AgentNamespace != "" {
		t.Errorf("AgentNamespace = %q, want none when two installations are visible", report.AgentNamespace)
	}
}

// TestDiscoverAgentNamespaceIgnoresTheRelay. A substring match on "wirekube"
// would take the relay's namespace just as happily.
func TestDiscoverAgentNamespaceIgnoresTheRelay(t *testing.T) {
	relay := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "DaemonSet",
		"metadata": map[string]any{
			"name": "wirekube-relay", "namespace": "wirekube-relay-ns",
			"labels": map[string]any{"app.kubernetes.io/name": "wirekube-relay"},
		},
	}}
	client := newTestClient(testMesh(1), relay, wirekubeAgentDaemonSet("wirekube-system"))
	report, err := Inspect(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if report.AgentNamespace != "wirekube-system" {
		t.Errorf("AgentNamespace = %q, want the agent's", report.AgentNamespace)
	}
}

// TestEnrollReportsAnAddressMove. Re-enrolling onto a different address is
// legitimate — the old one was taken while this host was away — but it changes
// what the mesh routes here, so the operator has to be told. The warning used
// to be appended to a DoctorReport that Enroll discards.
func TestEnrollReportsAnAddressMove(t *testing.T) {
	directory := t.TempDir()
	state, err := loadOrCreateState(directory, "idleloom-mac-one", "enrollment-one")
	if err != nil {
		t.Fatal(err)
	}
	state.MeshCIDR = "172.31.240.0/20"
	// The address this host last held, which somebody else has since taken.
	held, err := meshIPForName(state.DisplayName, state.MeshCIDR)
	if err != nil {
		t.Fatal(err)
	}
	state.AssignedMeshIP = held
	if err := writeState(directory, state); err != nil {
		t.Fatal(err)
	}

	mesh := testMesh(1)
	if err := unstructured.SetNestedField(mesh.Object, "allocator", "spec", "addressAllocation"); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(mesh, wirekubeAgentDaemonSet("wirekube-system"))
	client.PrependReactor("create", PeersGVR.Resource, assignUID("peer-uid"))
	squatter := &meshclaim.Allocator{
		Store: meshclaim.Dynamic(client, "wirekube-system"), MeshName: defaultMeshName, MeshCIDR: state.MeshCIDR,
	}
	if _, err := squatter.Allocate(context.Background(), meshclaim.Request{Holder: "somebody-else", Preferred: held}); err != nil {
		t.Fatal(err)
	}

	var warnings []string
	config := testEnrollConfig(directory, client, newKubernetesTestClient())
	config.Warn = func(message string) { warnings = append(warnings, message) }
	got, err := Enroll(context.Background(), config)
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if got.AssignedMeshIP == held {
		t.Fatalf("took %s, which somebody-else holds", held)
	}
	if len(warnings) == 0 {
		t.Fatal("the address moved and nothing said so")
	}
	if !strings.Contains(warnings[0], held) || !strings.Contains(warnings[0], got.AssignedMeshIP) {
		t.Errorf("the warning does not name both addresses: %q", warnings[0])
	}
}
