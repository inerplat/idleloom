package fencing

import (
	"context"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestAllocateIncrementsHostScopedEpoch(t *testing.T) {
	hostUID := types.UID("host-uid")
	client := fake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "idleloom-host-studio",
			Name:        LeaseName,
			Labels:      map[string]string{ManagedByLabel: ManagedByValue},
			Annotations: map[string]string{HostUIDAnnotation: string(hostUID), EpochAnnotation: "7"},
		},
	})
	leases := client.CoordinationV1().Leases("idleloom-host-studio")
	epoch, err := Allocate(context.Background(), leases, hostUID)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if epoch != 8 {
		t.Fatalf("Allocate = %d, want 8", epoch)
	}
	stored, err := leases.Get(context.Background(), LeaseName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Annotations[EpochAnnotation] != "8" {
		t.Fatalf("stored epoch = %q, want 8", stored.Annotations[EpochAnnotation])
	}
}

func TestAllocateRejectsWrongHostOwnership(t *testing.T) {
	client := fake.NewClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "idleloom-host-studio",
			Name:        LeaseName,
			Labels:      map[string]string{ManagedByLabel: ManagedByValue},
			Annotations: map[string]string{HostUIDAnnotation: "other-host", EpochAnnotation: "1"},
		},
	})
	if _, err := Allocate(context.Background(), client.CoordinationV1().Leases("idleloom-host-studio"), types.UID("host-uid")); err == nil {
		t.Fatal("Allocate accepted a fencing Lease owned by another host")
	}
}
