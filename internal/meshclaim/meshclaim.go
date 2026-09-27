// Package meshclaim claims a WireKube mesh address for a peer that does not
// exist yet.
//
// A worker's node address is its mesh address, and kubelet needs it as
// --node-ip before the node — and therefore the WireKubePeer — exists. The
// address is derived from the node name, which is reproducible but not
// collision-free: two names can reduce into the same /32, and until WireKube
// grew an allocator the only honest answer was to refuse the enrolment and ask
// for a different name.
//
// WireKube now arbitrates addresses through Lease claims, and this is the
// client half of the same protocol. It has to agree with
// wirekube/pkg/meshalloc exactly — the same namespace, the same Lease name,
// the same holder — because the arbitration *is* the name collision in etcd:
// two claimants that compute different names do not contend, they both win.
//
// It is a separate implementation rather than an import because idlectl is a
// CLI built on client-go and WireKube's allocator is built on
// controller-runtime. The wire format is the contract; the code is not shared.
package meshclaim

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/inerplat/idleloom/internal/meship"
)

// The claim wire format, fixed by wirekube/pkg/meshalloc.
const (
	// DefaultNamespace is where WireKube keeps its claims.
	DefaultNamespace = "wirekube-system"

	meshLabel  = "wirekube.io/mesh"
	peerLabel  = "wirekube.io/peer-name"
	claimLabel = "wirekube.io/claim"
	claimValue = "address"

	addressAnnotation = "wirekube.io/address"
	attemptAnnotation = "wirekube.io/attempt"

	// probeLimit matches WireKube's: after this many candidates are found
	// taken, listing the claims once is both cheaper and exact.
	probeLimit = 32
)

// ErrExhausted reports that every address in the mesh CIDR is claimed. It is
// terminal — nothing frees up on its own — so the mesh CIDR has to be widened.
var ErrExhausted = errors.New("every address in the WireKube mesh CIDR is already claimed")

// Allocator claims mesh addresses through the Kubernetes API.
type Allocator struct {
	// Store reads and creates the claim Leases. Use Typed or Dynamic.
	Store Store
	// MeshName is the WireKubeMesh the address comes from.
	MeshName string
	// MeshCIDR is the range to allocate from.
	MeshCIDR string
	// Grace is how long the claim stays valid with no peer answering for it.
	// It has to outlast enrolment: the claim is made first and the peer only
	// appears once the worker has booted and its agent has connected, and
	// WireKube's reaper would otherwise hand the address to somebody else
	// mid-enrolment. Zero leaves the reaper's own default in force.
	Grace time.Duration
}

// Request describes the claim to make.
type Request struct {
	// Holder identifies who holds the claim. It is the name WireKube's reaper
	// looks up to decide whether the claim is still answered for, so it has to
	// be the WireKubePeer or WireKubeExternalPeer name.
	Holder string
	// Name is the name the address derives from. It defaults to Holder, and
	// differs only where WireKube itself derives from something else: an
	// external peer's address comes from its display name while the peer
	// resource carries another. Deriving from the wrong one would move a peer
	// that is already up.
	Name string
	// Preferred is the address the caller already advertises, if any. It is
	// taken when it is usable and free, so adopting the allocator renumbers
	// nobody.
	Preferred string
}

func (r Request) hashName() string {
	if r.Name != "" {
		return r.Name
	}
	return r.Holder
}

// Result is a claimed address and how it was reached.
type Result struct {
	// Address is the claimed /32 in CIDR notation.
	Address string
	// ClaimName and ClaimUID identify the Lease, for a caller that has to roll
	// the claim back as part of a larger failed enrollment.
	ClaimName string
	ClaimUID  types.UID
	// Attempt is the candidate index that won; 0 is the hashed address.
	Attempt int
	// Adopted is true when the claim already existed and was this peer's.
	Adopted bool
	// Moved is true when the peer did not get its hashed address, which is
	// worth telling the operator: the node name collided with something.
	Moved bool
}

// ErrInUse reports that a specific address is claimed by somebody else. Only
// Reserve returns it; Allocate moves to another address instead.
var ErrInUse = errors.New("the mesh address is already claimed")

// Reserve claims one specific address for holder, or fails.
//
// It is for the caller that cannot move — a mesh that does not arbitrate
// addresses gives every peer exactly the address its name hashes to, so there
// is no second choice to fall back on and a contested address has to be
// reported. Reserving the address the caller was going to take anyway still
// keeps two enrolments from racing onto it.
//
// Claiming an address this holder already holds succeeds and reports Adopted.
func (a *Allocator) Reserve(ctx context.Context, holder, address string) (Result, error) {
	if err := a.validate(); err != nil {
		return Result{}, err
	}
	if holder == "" {
		return Result{}, fmt.Errorf("a mesh address claim needs a holder")
	}
	if !meship.Contains(address, a.MeshCIDR) {
		return Result{}, fmt.Errorf("the mesh address %s is not one %s can hand out", address, a.MeshCIDR)
	}
	result, claimed, err := a.claim(ctx, holder, address, -1)
	if err != nil {
		return Result{}, err
	}
	if !claimed {
		return Result{}, fmt.Errorf("%w: %s", ErrInUse, address)
	}
	return result, nil
}

// Allocate returns the address peerName holds, claiming one if it holds none.
//
// It prefers, in order, a claim peerName already holds, then preferred if it
// is usable and free, then the walk from the hashed address. Holding the
// existing claim first is what makes a re-run of an interrupted enrolment
// land on the same address instead of consuming a second one.
func (a *Allocator) Allocate(ctx context.Context, request Request) (Result, error) {
	if err := a.validate(); err != nil {
		return Result{}, err
	}
	if request.Holder == "" {
		return Result{}, fmt.Errorf("a mesh address claim needs a holder")
	}
	capacity, err := meship.Capacity(a.MeshCIDR)
	if err != nil {
		return Result{}, err
	}
	hashed, err := meship.IPForName(request.hashName(), a.MeshCIDR)
	if err != nil {
		return Result{}, err
	}

	if held, found, err := a.heldClaim(ctx, request); err != nil {
		return Result{}, err
	} else if found {
		held.Moved = held.Address != hashed
		return held, nil
	}

	if request.Preferred != "" && meship.Contains(request.Preferred, a.MeshCIDR) {
		result, claimed, err := a.claim(ctx, request.Holder, request.Preferred, -1)
		if err != nil {
			return Result{}, err
		}
		if claimed {
			result.Moved = result.Address != hashed
			return result, nil
		}
	}

	probed := min(probeLimit, capacity)
	for attempt := range probed {
		address, err := meship.IPForNameAttempt(request.hashName(), a.MeshCIDR, attempt)
		if err != nil {
			return Result{}, err
		}
		result, claimed, err := a.claim(ctx, request.Holder, address, attempt)
		if err != nil {
			return Result{}, err
		}
		if claimed {
			result.Moved = attempt != 0
			return result, nil
		}
	}
	if probed == capacity {
		// The walk is a permutation, so every address has been tried.
		return Result{}, a.exhausted(capacity)
	}
	return a.allocateFromFreeList(ctx, request, capacity)
}

// allocateFromFreeList takes over once enough candidates have come back taken
// to suggest the pool is dense. One list turns the rest of the search exact,
// so the last free address costs a bounded number of calls rather than one per
// occupied address.
func (a *Allocator) allocateFromFreeList(ctx context.Context, request Request, capacity int) (Result, error) {
	claims, err := a.list(ctx, "")
	if err != nil {
		return Result{}, err
	}
	taken := make(map[string]struct{}, len(claims))
	for i := range claims {
		if address := claims[i].Annotations[addressAnnotation]; address != "" {
			taken[address] = struct{}{}
		}
	}
	for attempt := range capacity {
		address, err := meship.IPForNameAttempt(request.hashName(), a.MeshCIDR, attempt)
		if err != nil {
			return Result{}, err
		}
		if _, occupied := taken[address]; occupied {
			continue
		}
		result, claimed, err := a.claim(ctx, request.Holder, address, attempt)
		if err != nil {
			return Result{}, err
		}
		if claimed {
			result.Moved = attempt != 0
			return result, nil
		}
		taken[address] = struct{}{}
	}
	return Result{}, a.exhausted(capacity)
}

// claim attempts to take address. It reports claimed=false only when somebody
// else holds it.
func (a *Allocator) claim(ctx context.Context, peerName, address string, attempt int) (Result, bool, error) {
	created, err := a.Store.Create(ctx, a.leaseFor(peerName, address, attempt))
	if err == nil {
		return Result{Address: address, ClaimName: created.Name, ClaimUID: created.UID, Attempt: attempt}, true, nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return Result{}, false, fmt.Errorf("claim the mesh address %s for %q: %w", address, peerName, err)
	}

	existing, err := a.Store.Get(ctx, ClaimName(a.MeshName, address))
	if apierrors.IsNotFound(err) {
		// Released between the create and the read. Report it as taken
		// rather than retrying: claiming an address we do not hold would be
		// worse than walking past a free one, and the walk comes back around.
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, fmt.Errorf("read the claim on the mesh address %s: %w", address, err)
	}
	if holderOf(existing) != peerName {
		return Result{}, false, nil
	}
	return Result{Address: address, ClaimName: existing.Name, ClaimUID: existing.UID, Attempt: attempt, Adopted: true}, true, nil
}

// heldClaim returns the claim peerName already holds, releasing any duplicate.
func (a *Allocator) heldClaim(ctx context.Context, request Request) (Result, bool, error) {
	claims, err := a.list(ctx, peerLabel+"="+labelSafe(request.Holder))
	if err != nil {
		return Result{}, false, err
	}
	mine := make([]coordinationv1.Lease, 0, 1)
	for i := range claims {
		// The label is truncated and so ambiguous; the holder is authoritative.
		if holderOf(&claims[i]) == request.Holder && claims[i].Annotations[addressAnnotation] != "" {
			mine = append(mine, claims[i])
		}
	}
	if len(mine) == 0 {
		return Result{}, false, nil
	}
	sort.Slice(mine, func(i, j int) bool { return betterClaim(&mine[i], &mine[j], request.Preferred) })
	keep := &mine[0]
	for i := 1; i < len(mine); i++ {
		if err := a.deleteClaim(ctx, &mine[i]); err != nil {
			return Result{}, false, err
		}
	}
	return Result{
		Address:   keep.Annotations[addressAnnotation],
		ClaimName: keep.Name,
		ClaimUID:  keep.UID,
		Attempt:   attemptOf(keep),
		Adopted:   true,
	}, true, nil
}

// Release drops peerName's claims so the addresses go straight back into the
// pool instead of waiting out the reaper's grace period. It never touches a
// claim somebody else holds: an enrolment torn down after its address was
// reassigned must not take the new holder's claim with it.
func (a *Allocator) Release(ctx context.Context, holder string) error {
	if err := a.validate(); err != nil {
		return err
	}
	claims, err := a.list(ctx, peerLabel+"="+labelSafe(holder))
	if err != nil {
		return err
	}
	for i := range claims {
		if holderOf(&claims[i]) != holder {
			continue
		}
		if err := a.deleteClaim(ctx, &claims[i]); err != nil {
			return err
		}
	}
	return nil
}

func (a *Allocator) deleteClaim(ctx context.Context, claim *coordinationv1.Lease) error {
	err := a.Store.Delete(ctx, claim.Name, claim.UID)
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("release the claim on the mesh address %s: %w", claim.Annotations[addressAnnotation], err)
	}
	return nil
}

func (a *Allocator) list(ctx context.Context, extra string) ([]coordinationv1.Lease, error) {
	selector := claimLabel + "=" + claimValue + "," + meshLabel + "=" + a.MeshName
	if extra != "" {
		selector += "," + extra
	}
	claims, err := a.Store.List(ctx, selector)
	if err != nil {
		return nil, fmt.Errorf("list WireKube mesh address claims in %s: %w", a.Store.Namespace(), err)
	}
	return claims, nil
}

func (a *Allocator) leaseFor(peerName, address string, attempt int) *coordinationv1.Lease {
	holder := peerName
	attemptValue := strconv.Itoa(attempt)
	if attempt < 0 {
		attemptValue = "preferred"
	}
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ClaimName(a.MeshName, address),
			Namespace: a.Store.Namespace(),
			Labels: map[string]string{
				claimLabel: claimValue,
				meshLabel:  a.MeshName,
				peerLabel:  labelSafe(peerName),
			},
			Annotations: map[string]string{
				addressAnnotation: address,
				attemptAnnotation: attemptValue,
			},
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
	if a.Grace > 0 {
		seconds := int32(a.Grace.Seconds())
		lease.Spec.LeaseDurationSeconds = &seconds
	}
	return lease
}

func (a *Allocator) exhausted(capacity int) error {
	return fmt.Errorf("%w: %s holds %d addresses and all of them are taken; widen the WireKubeMesh spec.meshCIDR",
		ErrExhausted, a.MeshCIDR, capacity)
}

func (a *Allocator) validate() error {
	if a.Store == nil {
		return fmt.Errorf("no Lease store configured for mesh address claims")
	}
	if a.MeshName == "" {
		return fmt.Errorf("no WireKubeMesh name configured for mesh address claims")
	}
	if a.MeshCIDR == "" {
		return fmt.Errorf("the WireKubeMesh does not publish spec.meshCIDR, so the worker node address cannot be derived")
	}
	return nil
}

// ClaimName is the Lease name that arbitrates address within mesh. It must
// match wirekube/pkg/meshalloc.ClaimName exactly or the two never contend.
func ClaimName(mesh, address string) string {
	host := strings.TrimSuffix(address, "/32")
	return "wirekube-" + mesh + "-" + strings.ReplaceAll(host, ".", "-")
}

// betterClaim orders two claims held by one peer: the address it already
// advertises, then the earliest candidate, then the lower address so the
// choice does not depend on list order.
func betterClaim(candidate, incumbent *coordinationv1.Lease, preferred string) bool {
	if preferred != "" {
		a := candidate.Annotations[addressAnnotation] == preferred
		b := incumbent.Annotations[addressAnnotation] == preferred
		if a != b {
			return a
		}
	}
	if a, b := attemptOf(candidate), attemptOf(incumbent); a != b {
		return a < b
	}
	return candidate.Annotations[addressAnnotation] < incumbent.Annotations[addressAnnotation]
}

func attemptOf(lease *coordinationv1.Lease) int {
	attempt, err := strconv.Atoi(lease.Annotations[attemptAnnotation])
	if err != nil {
		return -1
	}
	return attempt
}

func holderOf(lease *coordinationv1.Lease) string {
	if lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *lease.Spec.HolderIdentity
}

// labelSafe renders a peer name as a label value. Node names can exceed the
// 63-character label limit; the label is only a selector shortcut, and the
// holder is authoritative, so truncating beats failing the create.
func labelSafe(name string) string {
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.Trim(name, "-_.")
}
