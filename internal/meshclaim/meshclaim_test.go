package meshclaim

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/inerplat/idleloom/internal/meship"
)

const testMesh = "198.18.18.0/24"

func newAllocator() *Allocator {
	return &Allocator{
		Client:   fake.NewSimpleClientset(),
		MeshName: "default",
		MeshCIDR: testMesh,
	}
}

// TestClaimWireFormatMatchesWireKube pins the half of this package that is a
// protocol rather than an implementation. The Lease name is the arbitration:
// two claimants that compute different names for one address do not contend,
// they both succeed, and the mesh ends up with a duplicate. These literals are
// wirekube/pkg/meshalloc's, and they are not free to change on one side.
func TestClaimWireFormatMatchesWireKube(t *testing.T) {
	if got := DefaultNamespace; got != "wirekube-system" {
		t.Errorf("namespace = %q, want wirekube-system", got)
	}
	for _, c := range []struct{ mesh, address, want string }{
		{"default", "198.18.18.74/32", "wirekube-default-198-18-18-74"},
		{"default", "198.18.18.74", "wirekube-default-198-18-18-74"},
		{"prod", "10.0.0.1/32", "wirekube-prod-10-0-0-1"},
	} {
		if got := claimName(c.mesh, c.address); got != c.want {
			t.Errorf("claimName(%q, %q) = %q, want %q", c.mesh, c.address, got, c.want)
		}
	}

	a := newAllocator()
	a.Grace = 45 * time.Minute
	lease := a.leaseFor("worker1", "198.18.18.74/32", 3)
	if lease.Name != "wirekube-default-198-18-18-74" {
		t.Errorf("lease name = %q", lease.Name)
	}
	for key, want := range map[string]string{
		"wirekube.io/claim":     "address",
		"wirekube.io/mesh":      "default",
		"wirekube.io/peer-name": "worker1",
	} {
		if got := lease.Labels[key]; got != want {
			t.Errorf("label %s = %q, want %q", key, got, want)
		}
	}
	for key, want := range map[string]string{
		"wirekube.io/address": "198.18.18.74/32",
		"wirekube.io/attempt": "3",
	} {
		if got := lease.Annotations[key]; got != want {
			t.Errorf("annotation %s = %q, want %q", key, got, want)
		}
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "worker1" {
		t.Errorf("holderIdentity = %v, want worker1", lease.Spec.HolderIdentity)
	}
	if lease.Spec.LeaseDurationSeconds == nil || *lease.Spec.LeaseDurationSeconds != 2700 {
		t.Errorf("leaseDurationSeconds = %v, want 2700", lease.Spec.LeaseDurationSeconds)
	}
}

// TestGraceIsWhatKeepsAnEnrolmentAlive: the claim is made before the node
// exists, so without a duration long enough to cover the boot WireKube's
// reaper would hand the address to somebody else mid-enrolment.
func TestGraceIsWhatKeepsAnEnrolmentAlive(t *testing.T) {
	a := newAllocator()
	if seconds := a.leaseFor("worker1", "198.18.18.74/32", 0).Spec.LeaseDurationSeconds; seconds != nil {
		t.Errorf("leaseDurationSeconds = %v with no grace configured, want the reaper's default left in force", *seconds)
	}
}

func TestAllocateTakesTheHashedAddress(t *testing.T) {
	a := newAllocator()
	want, err := meship.IPForName("worker1", testMesh)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Allocate(context.Background(), "worker1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != want {
		t.Errorf("Allocate = %s, want the hashed %s", got.Address, want)
	}
	if got.Moved {
		t.Error("reported as moved, but it took its hashed address")
	}
}

// TestAllocateMovesRatherThanRefusing is the behaviour change. Enrolment used
// to fail with "enrol this worker under a different name" the moment a node
// name hashed onto a taken address; it now takes another address.
func TestAllocateMovesRatherThanRefusing(t *testing.T) {
	a := newAllocator()
	contested, err := meship.IPForName("worker1", testMesh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Allocate(context.Background(), "squatter", contested); err != nil {
		t.Fatal(err)
	}
	got, err := a.Allocate(context.Background(), "worker1", "")
	if err != nil {
		t.Fatalf("Allocate refused instead of moving: %v", err)
	}
	if got.Address == contested {
		t.Fatalf("worker1 took %s, which squatter holds", contested)
	}
	if !got.Moved {
		t.Error("did not report the move")
	}
	if !meship.Contains(got.Address, testMesh) {
		t.Errorf("%s is not an address the mesh can hand out", got.Address)
	}
}

// TestAllocateIsIdempotent covers a re-run of an interrupted enrolment: it
// must land on the same address rather than consume a second one.
func TestAllocateIsIdempotent(t *testing.T) {
	a := newAllocator()
	first, err := a.Allocate(context.Background(), "worker1", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		got, err := a.Allocate(context.Background(), "worker1", "")
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if got.Address != first.Address {
			t.Fatalf("run %d took %s, want the held %s", i, got.Address, first.Address)
		}
		if !got.Adopted {
			t.Errorf("run %d did not report the claim as adopted", i)
		}
	}
	if n := countClaims(t, a); n != 1 {
		t.Errorf("%d claims after four runs, want one", n)
	}
}

func TestAllocatePrefersTheRecordedAddress(t *testing.T) {
	a := newAllocator()
	const preferred = "198.18.18.222/32"
	hashed, err := meship.IPForName("worker1", testMesh)
	if err != nil {
		t.Fatal(err)
	}
	if hashed == preferred {
		t.Fatal("test vector is useless")
	}
	got, err := a.Allocate(context.Background(), "worker1", preferred)
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != preferred {
		t.Errorf("Allocate = %s, want the preferred %s", got.Address, preferred)
	}
	if !got.Moved {
		t.Error("a preferred address that is not the hash is a move")
	}
}

func TestAllocateIgnoresAnUnusablePreferredAddress(t *testing.T) {
	want, err := meship.IPForName("worker1", testMesh)
	if err != nil {
		t.Fatal(err)
	}
	for _, preferred := range []string{"10.9.9.9/32", "198.18.18.0/32", "198.18.18.255/32", "garbage", "198.18.18.9"} {
		a := newAllocator()
		got, err := a.Allocate(context.Background(), "worker1", preferred)
		if err != nil {
			t.Fatalf("preferred %q: %v", preferred, err)
		}
		if got.Address != want {
			t.Errorf("preferred %q: got %s, want the re-derived %s", preferred, got.Address, want)
		}
	}
}

// TestAllocateExhaustsExactly: a full mesh has to say so, and say what to do
// about it. Nothing frees up on its own.
func TestAllocateExhaustsExactly(t *testing.T) {
	const cidr = "198.18.18.0/29"
	a := newAllocator()
	a.MeshCIDR = cidr
	capacity, err := meship.Capacity(cidr)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]string, capacity)
	for i := range capacity {
		name := fmt.Sprintf("peer-%d", i)
		got, err := a.Allocate(context.Background(), name, "")
		if err != nil {
			t.Fatalf("filling slot %d: %v", i, err)
		}
		if prev, ok := seen[got.Address]; ok {
			t.Fatalf("%s handed to both %q and %q", got.Address, prev, name)
		}
		seen[got.Address] = name
	}
	_, err = a.Allocate(context.Background(), "one-too-many", "")
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("error = %v, want ErrExhausted", err)
	}
	if got := err.Error(); !strings.Contains(got, "widen") {
		t.Errorf("error %q does not say what to do about it", got)
	}
}

// TestAllocateFindsTheLastFreeAddress exercises the switch from probing to the
// exact free list.
func TestAllocateFindsTheLastFreeAddress(t *testing.T) {
	a := newAllocator()
	capacity, err := meship.Capacity(testMesh)
	if err != nil {
		t.Fatal(err)
	}
	var free string
	for i := range capacity {
		name := fmt.Sprintf("filler-%d", i)
		got, err := a.Allocate(context.Background(), name, "")
		if err != nil {
			t.Fatalf("filling slot %d: %v", i, err)
		}
		if i == capacity-1 {
			free = got.Address
			if err := a.Release(context.Background(), name); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := a.Allocate(context.Background(), "latecomer", "")
	if err != nil {
		t.Fatalf("Allocate with one address free: %v", err)
	}
	if got.Address != free {
		t.Errorf("got %s, want the only free address %s", got.Address, free)
	}
}

func TestReleaseReturnsTheAddressToThePool(t *testing.T) {
	a := newAllocator()
	held, err := a.Allocate(context.Background(), "worker1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Release(context.Background(), "worker1"); err != nil {
		t.Fatal(err)
	}
	if n := countClaims(t, a); n != 0 {
		t.Fatalf("%d claims after Release, want none", n)
	}
	// Releasing twice is not an error; a delete that already happened is the
	// normal case when an enrolment is torn down after a partial failure.
	if err := a.Release(context.Background(), "worker1"); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	got, err := a.Allocate(context.Background(), "successor", held.Address)
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != held.Address {
		t.Errorf("successor got %s, want the freed %s", got.Address, held.Address)
	}
}

func TestReleaseLeavesAnotherHoldersClaimAlone(t *testing.T) {
	a := newAllocator()
	if _, err := a.Allocate(context.Background(), "keeper", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(context.Background(), "stranger"); err != nil {
		t.Fatal(err)
	}
	if n := countClaims(t, a); n != 1 {
		t.Errorf("%d claims, want the keeper's to survive", n)
	}
}

func TestAllocateRejectsAnIncompleteAllocator(t *testing.T) {
	for name, a := range map[string]*Allocator{
		"no client":    {MeshName: "default", MeshCIDR: testMesh},
		"no mesh name": {Client: fake.NewSimpleClientset(), MeshCIDR: testMesh},
		"no cidr":      {Client: fake.NewSimpleClientset(), MeshName: "default"},
		"bad cidr":     {Client: fake.NewSimpleClientset(), MeshName: "default", MeshCIDR: "198.18.18.0/31"},
	} {
		if _, err := a.Allocate(context.Background(), "worker1", ""); err == nil {
			t.Errorf("%s: Allocate succeeded", name)
		}
		if err := a.Release(context.Background(), "worker1"); err == nil && name != "bad cidr" {
			t.Errorf("%s: Release succeeded", name)
		}
	}
}

func countClaims(t *testing.T, a *Allocator) int {
	t.Helper()
	claims, err := a.Client.CoordinationV1().Leases(a.namespace()).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(claims.Items)
}
