package idleloom

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/inerplat/idleloom/internal/meshclaim"
	"github.com/inerplat/idleloom/internal/meship"
)

const testMeshCIDR = "198.18.18.0/24"

func hashMesh() WireKubeStatus {
	return WireKubeStatus{MeshCIDR: testMeshCIDR, MeshName: "default"}
}

func allocatorMesh() WireKubeStatus {
	return WireKubeStatus{MeshCIDR: testMeshCIDR, MeshName: "default", AddressAllocation: "allocator"}
}

func TestArbitratesAddresses(t *testing.T) {
	for allocation, want := range map[string]bool{"": false, "hash": false, "allocator": true, "Allocator": false} {
		if got := (WireKubeStatus{AddressAllocation: allocation}).ArbitratesAddresses(); got != want {
			t.Errorf("addressAllocation=%q → %v, want %v", allocation, got, want)
		}
	}
}

func TestReserveMeshAddressNeedsAMeshCIDR(t *testing.T) {
	for _, wireKube := range []WireKubeStatus{hashMesh(), allocatorMesh()} {
		wireKube.MeshCIDR = ""
		_, err := ReserveMeshAddress(context.Background(), fake.NewSimpleClientset(), "worker1", wireKube)
		if err == nil || !strings.Contains(err.Error(), "meshCIDR") {
			t.Errorf("error = %v, want it to name spec.meshCIDR", err)
		}
	}
}

// TestReserveMeshAddressClaimsOnAnArbitratingMesh: the address is taken out of
// the pool, and teardown is told there is something to hand back.
func TestReserveMeshAddressClaimsOnAnArbitratingMesh(t *testing.T) {
	client := fake.NewSimpleClientset()
	want, err := meship.AddressForName("worker1", testMeshCIDR)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReserveMeshAddress(context.Background(), client, "worker1", allocatorMesh())
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != want {
		t.Errorf("address = %s, want the hashed %s", got.Address, want)
	}
	if strings.Contains(got.Address, "/") {
		t.Errorf("address %q carries a prefix length; kubelet --node-ip takes a bare address", got.Address)
	}
	if !got.Claimed {
		t.Error("did not report the claim, so teardown would never release it")
	}
	if got.Moved {
		t.Error("reported a move but took its own address")
	}
	if n := len(listLeases(t, client)); n != 1 {
		t.Errorf("%d claims, want one", n)
	}
}

// TestReserveMeshAddressMovesOnAnArbitratingMesh is the behaviour change:
// enrollment used to fail outright when a node name's address was taken.
func TestReserveMeshAddressMovesOnAnArbitratingMesh(t *testing.T) {
	client := fake.NewSimpleClientset()
	contested, err := meship.IPForName("worker1", testMeshCIDR)
	if err != nil {
		t.Fatal(err)
	}
	squatter := &meshclaim.Allocator{Store: meshclaim.Typed(client, ""), MeshName: "default", MeshCIDR: testMeshCIDR}
	if _, err := squatter.Allocate(context.Background(), meshclaim.Request{Holder: "squatter", Preferred: contested}); err != nil {
		t.Fatal(err)
	}

	got, err := ReserveMeshAddress(context.Background(), client, "worker1", allocatorMesh())
	if err != nil {
		t.Fatalf("enrollment refused instead of moving: %v", err)
	}
	if got.Address+"/32" == contested {
		t.Fatalf("took %s, which squatter holds", contested)
	}
	if !got.Moved {
		t.Error("did not report the move, so the operator would not know the name collided")
	}
	if !meship.Contains(got.Address+"/32", testMeshCIDR) {
		t.Errorf("%s is not an address the mesh can hand out", got.Address)
	}
}

// TestReserveMeshAddressRefusesOnAHashMesh: with nothing arbitrating, taking a
// second worker onto an address a peer already advertises would leave both
// advertising it, so refusing is the honest answer.
func TestReserveMeshAddressRefusesOnAHashMesh(t *testing.T) {
	contested, err := meship.AddressForName("worker1", testMeshCIDR)
	if err != nil {
		t.Fatal(err)
	}
	client := clientServingPeers(t, peerFixture{name: "other", allowedIPs: []string{contested + "/32"}})
	_, err = ReserveMeshAddress(context.Background(), client, "worker1", hashMesh())
	if err == nil {
		t.Fatal("a duplicate address was accepted")
	}
	// The message has to point at the way out, not just at the problem.
	for _, want := range []string{contested, "WireKubePeer/other", "allocator"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestReserveMeshAddressAcceptsAFreeAddressOnAHashMesh(t *testing.T) {
	client := clientServingPeers(t, peerFixture{name: "other", allowedIPs: []string{"198.18.18.222/32"}})
	got, err := ReserveMeshAddress(context.Background(), client, "worker1", hashMesh())
	if err != nil {
		t.Fatal(err)
	}
	want, err := meship.AddressForName("worker1", testMeshCIDR)
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != want {
		t.Errorf("address = %s, want %s", got.Address, want)
	}
	if got.Claimed {
		t.Error("a hash mesh has no claim to release")
	}
}

// TestPreviewMeshAddressClaimsNothing: a dry run must leave the address free
// for the real run that follows it.
func TestPreviewMeshAddressClaimsNothing(t *testing.T) {
	client := fake.NewSimpleClientset()
	got, err := PreviewMeshAddress(context.Background(), client, "worker1", allocatorMesh())
	if err != nil {
		t.Fatal(err)
	}
	want, err := meship.AddressForName("worker1", testMeshCIDR)
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != want {
		t.Errorf("address = %s, want %s", got.Address, want)
	}
	if got.Moved {
		t.Error("reported a move against an empty mesh")
	}
	if n := len(listLeases(t, client)); n != 0 {
		t.Errorf("the preview created %d claims, want none", n)
	}
}

func TestPreviewMeshAddressReportsAContestedAddress(t *testing.T) {
	client := fake.NewSimpleClientset()
	contested, err := meship.IPForName("worker1", testMeshCIDR)
	if err != nil {
		t.Fatal(err)
	}
	squatter := &meshclaim.Allocator{Store: meshclaim.Typed(client, ""), MeshName: "default", MeshCIDR: testMeshCIDR}
	if _, err := squatter.Allocate(context.Background(), meshclaim.Request{Holder: "squatter", Preferred: contested}); err != nil {
		t.Fatal(err)
	}
	got, err := PreviewMeshAddress(context.Background(), client, "worker1", allocatorMesh())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Moved {
		t.Error("the preview did not report that the name's address is taken")
	}
	if n := len(listLeases(t, client)); n != 1 {
		t.Errorf("%d claims, want the preview to have added none", n)
	}
}

func TestReleaseMeshAddress(t *testing.T) {
	client := fake.NewSimpleClientset()
	if _, err := ReserveMeshAddress(context.Background(), client, "worker1", allocatorMesh()); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseMeshAddress(context.Background(), client, "worker1", allocatorMesh()); err != nil {
		t.Fatal(err)
	}
	if n := len(listLeases(t, client)); n != 0 {
		t.Errorf("%d claims after release, want none", n)
	}
}

// TestReleaseMeshAddressIsANoOpWithoutAnArbitratingMesh covers teardown after
// the mesh was switched back, or read back as empty because it is gone.
func TestReleaseMeshAddressIsANoOpWithoutAnArbitratingMesh(t *testing.T) {
	client := fake.NewSimpleClientset()
	for _, wireKube := range []WireKubeStatus{hashMesh(), {}, {AddressAllocation: "allocator"}} {
		if err := ReleaseMeshAddress(context.Background(), client, "worker1", wireKube); err != nil {
			t.Errorf("Release against %+v: %v", wireKube, err)
		}
	}
}

func listLeases(t *testing.T, client kubernetes.Interface) []string {
	t.Helper()
	leases, err := client.CoordinationV1().Leases(meshclaim.DefaultNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(leases.Items))
	for i := range leases.Items {
		names = append(names, leases.Items[i].Name)
	}
	return names
}

type peerFixture struct {
	name       string
	allowedIPs []string
}

// clientServingPeers builds a real clientset over an httptest server, because
// the WireKube CRDs are read through the discovery REST client and the fake
// clientset does not serve arbitrary API paths.
func clientServingPeers(t *testing.T, peers ...peerFixture) kubernetes.Interface {
	t.Helper()
	items := make([]map[string]any, 0, len(peers))
	for _, peer := range peers {
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": peer.name},
			"spec":     map[string]any{"allowedIPs": peer.allowedIPs},
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/apis/wirekube.io/v1alpha1/wirekubepeers":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case "/apis/wirekube.io/v1alpha1/wirekubeexternalpeers":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		default:
			http.Error(w, fmt.Sprintf("unexpected path %s", r.URL.Path), http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestReserveMeshAddressClaimsInTheAgentNamespace. The claim only arbitrates
// against WireKube's own if it lands in the same namespace; writing to the
// chart default while WireKube runs elsewhere would give idlectl a private
// pool that contends with nothing. Reserve, preview and release all have to
// agree on it, or a dry run reports a free address that is taken and teardown
// leaks the claim.
func TestReserveMeshAddressClaimsInTheAgentNamespace(t *testing.T) {
	const namespace = "wirekube-prod"
	client := fake.NewSimpleClientset()
	wireKube := allocatorMesh()
	wireKube.AgentNamespace = namespace

	// Somebody else holds the address worker1's name produces, in the agent's
	// namespace. A preview that looked anywhere else would miss it.
	contested, err := meship.IPForName("worker1", testMeshCIDR)
	if err != nil {
		t.Fatal(err)
	}
	squatter := &meshclaim.Allocator{Store: meshclaim.Typed(client, namespace), MeshName: "default", MeshCIDR: testMeshCIDR}
	if _, err := squatter.Allocate(context.Background(), meshclaim.Request{Holder: "squatter", Preferred: contested}); err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewMeshAddress(context.Background(), client, "worker1", wireKube)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Moved {
		t.Error("the preview did not read the agent namespace, so it missed a claim that is there")
	}

	got, err := ReserveMeshAddress(context.Background(), client, "worker1", wireKube)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Moved {
		t.Error("worker1 did not report the move off the contested address")
	}
	if n := len(claimsIn(t, client, namespace)); n != 2 {
		t.Fatalf("%d claims in %s, want the squatter's and worker1's", n, namespace)
	}
	if n := len(listLeases(t, client)); n != 0 {
		t.Errorf("%d claims landed in %s as well", n, meshclaim.DefaultNamespace)
	}

	if err := ReleaseMeshAddress(context.Background(), client, "worker1", wireKube); err != nil {
		t.Fatal(err)
	}
	remaining := claimsIn(t, client, namespace)
	if len(remaining) != 1 {
		t.Fatalf("%d claims after release, want the squatter's alone", len(remaining))
	}
}

func claimsIn(t *testing.T, client kubernetes.Interface, namespace string) []string {
	t.Helper()
	leases, err := client.CoordinationV1().Leases(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(leases.Items))
	for i := range leases.Items {
		names = append(names, leases.Items[i].Name)
	}
	return names
}

// TestPreviewMeshAddressIgnoresTheWorkersOwnClaim. A dry run re-run against a
// worker that is already enrolled would otherwise report that its address had
// been taken, by itself.
func TestPreviewMeshAddressIgnoresTheWorkersOwnClaim(t *testing.T) {
	client := fake.NewSimpleClientset()
	held, err := ReserveMeshAddress(context.Background(), client, "worker1", allocatorMesh())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewMeshAddress(context.Background(), client, "worker1", allocatorMesh())
	if err != nil {
		t.Fatal(err)
	}
	if preview.Moved {
		t.Error("the preview reported the worker's own claim as somebody else's")
	}
	if preview.Address != held.Address {
		t.Errorf("preview = %s, want the held %s", preview.Address, held.Address)
	}
}

// TestCheckWireKubeRefusesAnAmbiguousAgentNamespace. Two agent DaemonSets mean
// there is no telling which namespace holds the claims. The fallback to the
// chart default is right when the agent simply is not visible, and wrong here:
// it would put the claim somewhere WireKube never reads, where it arbitrates
// against nothing and two workers can take one address.
func TestCheckWireKubeRefusesAnAmbiguousAgentNamespace(t *testing.T) {
	for _, c := range []struct {
		allocation string
		refuse     bool
	}{
		{"allocator", true},
		// On a hash mesh no claim is made, so the namespace does not matter.
		{"hash", false},
	} {
		status := WireKubeStatus{
			Installed: true, IncludeNodeInternalIP: true,
			AgentName: "wirekube-agent", AgentNamespace: "",
			AddressAllocation: c.allocation,
		}
		err := validateWireKubeStatus(status)
		if c.refuse && err == nil {
			t.Errorf("addressAllocation=%q: accepted an ambiguous agent namespace", c.allocation)
		}
		if !c.refuse && err != nil {
			t.Errorf("addressAllocation=%q: refused: %v", c.allocation, err)
		}
		if c.refuse && err != nil && !strings.Contains(err.Error(), "more than one namespace") {
			t.Errorf("the error does not say what is wrong: %v", err)
		}
	}
}

// TestHolderIdentitiesMatchWireKube. These prefixes are WireKube's, and they
// are what keeps a node and an external peer of the same name from adopting
// each other's claim. Reproducing them wrong would not fail a build or a
// request; it would hand two peers one address.
func TestHolderIdentitiesMatchWireKube(t *testing.T) {
	if got := meshclaim.HolderForPeer("alice"); got != "wirekubepeer/alice" {
		t.Errorf("HolderForPeer = %q", got)
	}
	if got := meshclaim.HolderForExternalPeer("alice"); got != "wirekubeexternalpeer/alice" {
		t.Errorf("HolderForExternalPeer = %q", got)
	}
}

// TestReserveMeshAddressClaimsUnderThePeerHolder. A worker is a WireKubePeer,
// and WireKube's agent adopts the claim by matching exactly this identity when
// the worker joins. Using the bare node name would leave the agent unable to
// find it, and it would claim a second address.
func TestReserveMeshAddressClaimsUnderThePeerHolder(t *testing.T) {
	client := fake.NewSimpleClientset()
	if _, err := ReserveMeshAddress(context.Background(), client, "worker1", allocatorMesh()); err != nil {
		t.Fatal(err)
	}
	claims, err := client.CoordinationV1().Leases(meshclaim.DefaultNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(claims.Items) != 1 {
		t.Fatalf("%d claims, want one", len(claims.Items))
	}
	want := meshclaim.HolderForPeer("worker1")
	if got := *claims.Items[0].Spec.HolderIdentity; got != want {
		t.Errorf("holderIdentity = %q, want %q", got, want)
	}
}

func TestWaitForPeerGone(t *testing.T) {
	// A cluster that does not serve the CRD has no peer to wait for.
	absent := clientServingPeers(t)
	if err := waitForPeerGone(context.Background(), absent, "worker1", time.Second); err != nil {
		t.Errorf("waited on a cluster with no WireKube peers: %v", err)
	}
}
