package fencing

import (
	"context"
	"fmt"
	"math"
	"strconv"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	coordinationclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/util/retry"
)

const (
	LeaseName         = "idleloom-fencing"
	EpochAnnotation   = "ai.idleloom.io/fencing-epoch"
	HostUIDAnnotation = "ai.idleloom.io/host-uid"
	ManagedByLabel    = "app.kubernetes.io/managed-by"
	ManagedByValue    = "idleloom-enrollment"
)

// Allocate increments the host-scoped fencing epoch using Kubernetes
// resourceVersion conflict detection. Enrollment must create the Lease first.
func Allocate(ctx context.Context, leases coordinationclient.LeaseInterface, hostUID types.UID) (int64, error) {
	if hostUID == "" {
		return 0, fmt.Errorf("host UID is required")
	}
	var allocated int64
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		lease, err := leases.Get(ctx, LeaseName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("fencing Lease %s is missing; rerun host enrollment", LeaseName)
		}
		if err != nil {
			return fmt.Errorf("get fencing Lease %s: %w", LeaseName, err)
		}
		current, err := validateLease(lease, hostUID)
		if err != nil {
			return err
		}
		if current == math.MaxInt64 {
			return fmt.Errorf("fencing epoch is exhausted")
		}
		next := current + 1
		updated := lease.DeepCopy()
		updated.Annotations[EpochAnnotation] = strconv.FormatInt(next, 10)
		if _, err := leases.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return err
		}
		allocated = next
		return nil
	})
	if err != nil {
		return 0, err
	}
	return allocated, nil
}

func validateLease(lease *coordinationv1.Lease, hostUID types.UID) (int64, error) {
	if lease.Labels[ManagedByLabel] != ManagedByValue {
		return 0, fmt.Errorf("fencing Lease %s is not owned by Idleloom enrollment", lease.Name)
	}
	if lease.Annotations[HostUIDAnnotation] != string(hostUID) {
		return 0, fmt.Errorf("fencing Lease %s belongs to a different host UID", lease.Name)
	}
	rawEpoch, found := lease.Annotations[EpochAnnotation]
	if !found {
		return 0, fmt.Errorf("fencing Lease %s has no epoch", lease.Name)
	}
	epoch, err := strconv.ParseInt(rawEpoch, 10, 64)
	if err != nil || epoch < 0 {
		return 0, fmt.Errorf("fencing Lease %s has invalid epoch %q", lease.Name, rawEpoch)
	}
	return epoch, nil
}
