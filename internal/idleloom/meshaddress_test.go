package idleloom

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	squatter := &meshclaim.Allocator{Client: client, MeshName: "default", MeshCIDR: testMeshCIDR}
	if _, err := squatter.Allocate(context.Background(), "squatter", contested); err != nil {
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
	squatter := &meshclaim.Allocator{Client: client, MeshName: "default", MeshCIDR: testMeshCIDR}
	if _, err := squatter.Allocate(context.Background(), "squatter", contested); err != nil {
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
