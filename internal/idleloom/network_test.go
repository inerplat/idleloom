package idleloom

import (
	"context"
	"errors"
	"net"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestUsedIPRangesMatchesWholeAllowedCIDR(t *testing.T) {
	_, network, err := net.ParseCIDR("172.20.10.0/24")
	if err != nil {
		t.Fatal(err)
	}
	used := usedIPRanges{exact: map[string]bool{"172.21.1.2": true}, networks: []*net.IPNet{network}}
	for _, address := range []string{"172.20.10.2", "172.21.1.2"} {
		if !used.Contains(address) {
			t.Errorf("expected %s to be reserved", address)
		}
	}
	if used.Contains("172.22.1.2") {
		t.Fatal("unrelated IP was marked as reserved")
	}
	if !used.Overlaps("172.20.10.0/29") {
		t.Fatal("allowed CIDR overlap was not detected")
	}
	used.exact["172.22.1.5"] = true
	if !used.Overlaps("172.22.1.0/29") {
		t.Fatal("node IP inside a candidate subnet was not detected")
	}
	if used.Overlaps("172.23.1.0/29") {
		t.Fatal("unrelated candidate subnet was marked as reserved")
	}
}

func TestReleaseRuntimeNetworkChecksHolder(t *testing.T) {
	holder := "worker-a"
	reservationID := "reservation-a"
	uid := types.UID("lease-uid")
	lease := newRuntimeNetworkLease("idleloom-network-00001", holder, reservationID)
	lease.UID = uid
	client := fake.NewClientset(lease)
	if err := ReleaseRuntimeNetwork(context.Background(), client, lease.Name, string(uid), "worker-b", reservationID); err == nil {
		t.Fatal("network lease accepted a different holder")
	}
	if err := ReleaseRuntimeNetwork(context.Background(), client, lease.Name, "different-uid", holder, reservationID); err == nil {
		t.Fatal("network lease accepted a different UID")
	}
	if err := ReleaseRuntimeNetwork(context.Background(), client, lease.Name, string(uid), holder, reservationID); err != nil {
		t.Fatalf("ReleaseRuntimeNetwork: %v", err)
	}
	if _, err := client.CoordinationV1().Leases(networkLeaseNamespace).Get(context.Background(), lease.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("network lease still exists after release")
	}
}

func TestRuntimeNetworkLeaseIsPersistent(t *testing.T) {
	lease := newRuntimeNetworkLease("idleloom-network-00001", "worker-a", "reservation-a")
	if lease.Spec.LeaseDurationSeconds != nil || lease.Spec.AcquireTime != nil || lease.Spec.RenewTime != nil {
		t.Fatalf("persistent network reservation unexpectedly expires: %#v", lease.Spec)
	}
}

func TestValidateRuntimeNetworkReservation(t *testing.T) {
	nodeName := "worker-a"
	reservationID := "reservation-a"
	index := runtimeNetworkIndex(nodeName, 0)
	leaseName := networkLeasePrefix + formatNetworkIndex(index)
	uid := types.UID("lease-uid")
	lease := newRuntimeNetworkLease(leaseName, nodeName, reservationID)
	lease.UID = uid
	network := runtimeNetworkFromIndex(nodeName, index)
	runtime := RuntimeState{
		NodeName: nodeName, Subnet: network.Subnet, GatewayIP: network.GatewayIP,
		GuestIP: network.GuestIP, HostIP: network.HostIP, MACAddress: network.MAC,
	}
	client := fake.NewClientset(lease)
	if err := ValidateRuntimeNetworkReservation(context.Background(), client, leaseName, string(uid), nodeName, reservationID, runtime); err != nil {
		t.Fatalf("ValidateRuntimeNetworkReservation: %v", err)
	}
	badRuntime := runtime
	badRuntime.GuestIP = "172.31.255.2"
	if err := ValidateRuntimeNetworkReservation(context.Background(), client, leaseName, string(uid), nodeName, reservationID, badRuntime); err == nil {
		t.Fatal("network reservation accepted a mismatched guest IP")
	}
	if err := ValidateRuntimeNetworkReservation(context.Background(), client, leaseName, "different-uid", nodeName, reservationID, runtime); err == nil {
		t.Fatal("network reservation accepted a mismatched UID")
	}
}

func TestFindRuntimeNetworkReservationAdoptsMatchingIntent(t *testing.T) {
	nodeName := "worker-a"
	reservationID := "reservation-a"
	index := runtimeNetworkIndex(nodeName, 0)
	lease := newRuntimeNetworkLease(networkLeasePrefix+formatNetworkIndex(index), nodeName, reservationID)
	lease.UID = types.UID("lease-uid")
	client := fake.NewClientset(lease)
	network, name, uid, found, err := FindRuntimeNetworkReservation(context.Background(), client, nodeName, reservationID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || name != lease.Name || uid != string(lease.UID) || network != runtimeNetworkFromIndex(nodeName, index) {
		t.Fatalf("unexpected recovered reservation: found=%v name=%q uid=%q network=%+v", found, name, uid, network)
	}
}

func TestValidateRuntimeNetworkReservationReportsMissingLease(t *testing.T) {
	err := ValidateRuntimeNetworkReservation(context.Background(), fake.NewClientset(), "idleloom-network-00001", "lease-uid", "worker-a", "reservation-a", RuntimeState{})
	if !errors.Is(err, ErrRuntimeNetworkReservationNotFound) {
		t.Fatalf("missing Lease error = %v", err)
	}
}

func formatNetworkIndex(index uint32) string {
	const digits = "0123456789abcdef"
	value := make([]byte, 5)
	for i := len(value) - 1; i >= 0; i-- {
		value[i] = digits[index&0xf]
		index >>= 4
	}
	return string(value)
}

// TestValidateNetworkReservationIfHeldSkipsAnInPlaceWorker. An in-place worker
// records no lease, so the unguarded check reports "reservation identity is
// incomplete" for one every single time. Three lifecycle paths carried that
// guard separately and the certificate maintainer did not, which meant every
// maintenance pass returned before approving a CSR — invisibly, until the
// serving certificate it was meant to be rotating expired.
func TestValidateNetworkReservationIfHeldSkipsAnInPlaceWorker(t *testing.T) {
	client := fake.NewSimpleClientset()
	inPlace := State{NodeName: "worker-a", Runtime: RuntimeState{GuestIP: "198.18.18.42"}}
	if inPlace.HoldsNetworkReservation() {
		t.Fatal("test vector is useless: this state holds a reservation")
	}
	if err := ValidateNetworkReservationIfHeld(context.Background(), client, inPlace); err != nil {
		t.Errorf("an in-place worker was asked for a reservation it never takes: %v", err)
	}
	// And the unguarded call is exactly what the guard exists to avoid.
	if err := ValidateRuntimeNetworkReservation(context.Background(), client,
		inPlace.NetworkLease, inPlace.NetworkLeaseUID, inPlace.NodeName, inPlace.NetworkReservationID, inPlace.Runtime); err == nil {
		t.Error("the unguarded check accepted a state with no reservation, so the guard is pointless")
	}
}

// TestValidateNetworkReservationIfHeldStillChecksAVMWorker keeps the guard
// from becoming a way to skip the check altogether.
func TestValidateNetworkReservationIfHeldStillChecksAVMWorker(t *testing.T) {
	client := fake.NewSimpleClientset()
	vm := State{
		NodeName:             "worker-a",
		NetworkLease:         "idleloom-network-00abc",
		NetworkLeaseUID:      "lease-uid",
		NetworkReservationID: "reservation-id",
	}
	if !vm.HoldsNetworkReservation() {
		t.Fatal("test vector is useless: this state holds no reservation")
	}
	if err := ValidateNetworkReservationIfHeld(context.Background(), client, vm); err == nil {
		t.Error("a VM worker's missing reservation went unreported")
	}
}
