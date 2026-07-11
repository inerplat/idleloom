package idleloom

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var ErrRuntimeNetworkReservationNotFound = errors.New("worker network reservation not found")

const (
	networkLeaseNamespace        = "kube-system"
	networkLeasePrefix           = "idleloom-network-"
	networkReservationAnnotation = "idleloom.io/reservation-id"
)

func NewNetworkReservationID() (string, error) {
	data := make([]byte, 16)
	if _, err := cryptorand.Read(data); err != nil {
		return "", fmt.Errorf("generate network reservation identity: %w", err)
	}
	return hex.EncodeToString(data), nil
}

func ReserveRuntimeNetwork(ctx context.Context, client kubernetes.Interface, nodeName, reservationID string) (RuntimeNetwork, string, string, error) {
	if nodeName == "" || reservationID == "" {
		return RuntimeNetwork{}, "", "", fmt.Errorf("worker network reservation identity is incomplete")
	}
	if network, leaseName, leaseUID, found, err := FindRuntimeNetworkReservation(ctx, client, nodeName, reservationID); err != nil {
		return RuntimeNetwork{}, "", "", err
	} else if found {
		return network, leaseName, leaseUID, nil
	}
	used, err := usedClusterIPs(ctx, client)
	if err != nil {
		return RuntimeNetwork{}, "", "", err
	}
	for attempt := 0; attempt < 1024; attempt++ {
		index := runtimeNetworkIndex(nodeName, attempt)
		network := runtimeNetworkFromIndex(nodeName, index)
		if used.Overlaps(network.Subnet) {
			continue
		}
		leaseName := networkLeasePrefix + fmt.Sprintf("%05x", index)
		lease := newRuntimeNetworkLease(leaseName, nodeName, reservationID)
		if created, err := client.CoordinationV1().Leases(networkLeaseNamespace).Create(ctx, lease, metav1.CreateOptions{}); err == nil {
			if created.UID == "" {
				_ = client.CoordinationV1().Leases(networkLeaseNamespace).Delete(context.Background(), leaseName, metav1.DeleteOptions{})
				return RuntimeNetwork{}, "", "", fmt.Errorf("worker network reservation %s has no Kubernetes UID", leaseName)
			}
			return network, leaseName, string(created.UID), nil
		} else if !apierrors.IsAlreadyExists(err) {
			return RuntimeNetwork{}, "", "", fmt.Errorf("reserve worker network %s: %w", network.Subnet, err)
		}
	}
	return RuntimeNetwork{}, "", "", fmt.Errorf("could not reserve a collision-free worker network after 1024 attempts")
}

func newRuntimeNetworkLease(leaseName, nodeName, reservationID string) *coordinationv1.Lease {
	holder := nodeName
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:        leaseName,
			Namespace:   networkLeaseNamespace,
			Labels:      map[string]string{"app.kubernetes.io/managed-by": "idleloom"},
			Annotations: map[string]string{networkReservationAnnotation: reservationID},
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}
}

func FindRuntimeNetworkReservation(ctx context.Context, client kubernetes.Interface, nodeName, reservationID string) (RuntimeNetwork, string, string, bool, error) {
	if nodeName == "" || reservationID == "" {
		return RuntimeNetwork{}, "", "", false, nil
	}
	leases, err := client.CoordinationV1().Leases(networkLeaseNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=idleloom"})
	if err != nil {
		return RuntimeNetwork{}, "", "", false, fmt.Errorf("find worker network reservation: %w", err)
	}
	var match *coordinationv1.Lease
	for i := range leases.Items {
		lease := &leases.Items[i]
		if lease.Annotations[networkReservationAnnotation] != reservationID {
			continue
		}
		if match != nil {
			return RuntimeNetwork{}, "", "", false, fmt.Errorf("multiple worker network reservations use identity %q", reservationID)
		}
		match = lease
	}
	if match == nil {
		return RuntimeNetwork{}, "", "", false, nil
	}
	if match.UID == "" {
		return RuntimeNetwork{}, "", "", false, fmt.Errorf("worker network reservation %s has no Kubernetes UID", match.Name)
	}
	if err := validateNetworkLeaseIdentity(match, string(match.UID), nodeName, reservationID); err != nil {
		return RuntimeNetwork{}, "", "", false, err
	}
	index, err := networkIndexFromLeaseName(match.Name)
	if err != nil {
		return RuntimeNetwork{}, "", "", false, err
	}
	if !isRuntimeNetworkCandidate(nodeName, index) {
		return RuntimeNetwork{}, "", "", false, fmt.Errorf("worker network reservation %s is not a network allocated for node %q", match.Name, nodeName)
	}
	return runtimeNetworkFromIndex(nodeName, index), match.Name, string(match.UID), true, nil
}

func ValidateRuntimeNetworkReservation(ctx context.Context, client kubernetes.Interface, leaseName, leaseUID, nodeName, reservationID string, runtime RuntimeState) error {
	if leaseName == "" || leaseUID == "" || reservationID == "" {
		return fmt.Errorf("worker network reservation identity is incomplete")
	}
	lease, err := client.CoordinationV1().Leases(networkLeaseNamespace).Get(ctx, leaseName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %s", ErrRuntimeNetworkReservationNotFound, leaseName)
	}
	if err != nil {
		return fmt.Errorf("get worker network reservation %s: %w", leaseName, err)
	}
	if err := validateNetworkLeaseIdentity(lease, leaseUID, nodeName, reservationID); err != nil {
		return err
	}
	index, err := networkIndexFromLeaseName(leaseName)
	if err != nil {
		return err
	}
	if !isRuntimeNetworkCandidate(nodeName, index) {
		return fmt.Errorf("worker network reservation %s is not a network allocated for node %q", leaseName, nodeName)
	}
	expected := runtimeNetworkFromIndex(nodeName, index)
	actual := RuntimeNetwork{
		Subnet: runtime.Subnet, GatewayIP: runtime.GatewayIP, GuestIP: runtime.GuestIP,
		HostIP: runtime.HostIP, MAC: runtime.MACAddress,
	}
	if actual != expected {
		return fmt.Errorf("worker network reservation %s does not match runtime network: state=%+v expected=%+v", leaseName, actual, expected)
	}
	return nil
}

func ReleaseRuntimeNetwork(ctx context.Context, client kubernetes.Interface, leaseName, leaseUID, nodeName, reservationID string) error {
	if leaseName == "" {
		return nil
	}
	if leaseUID == "" {
		return fmt.Errorf("worker network reservation %s has no recorded UID", leaseName)
	}
	lease, err := client.CoordinationV1().Leases(networkLeaseNamespace).Get(ctx, leaseName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get worker network reservation %s: %w", leaseName, err)
	}
	if err := validateNetworkLeaseIdentity(lease, leaseUID, nodeName, reservationID); err != nil {
		return err
	}
	uid := lease.UID
	if err := client.CoordinationV1().Leases(networkLeaseNamespace).Delete(ctx, leaseName, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("release worker network reservation %s: %w", leaseName, err)
	}
	return nil
}

func validateNetworkLeaseIdentity(lease *coordinationv1.Lease, leaseUID, nodeName, reservationID string) error {
	if lease.Labels["app.kubernetes.io/managed-by"] != "idleloom" {
		return fmt.Errorf("worker network reservation %s is not managed by Idleloom", lease.Name)
	}
	if string(lease.UID) != leaseUID {
		return fmt.Errorf("worker network reservation %s UID changed: cluster=%q state=%q", lease.Name, lease.UID, leaseUID)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != nodeName {
		return fmt.Errorf("worker network reservation %s belongs to %q, not %q", lease.Name, pointerValue(lease.Spec.HolderIdentity), nodeName)
	}
	if lease.Annotations[networkReservationAnnotation] != reservationID {
		return fmt.Errorf("worker network reservation %s has a different reservation identity", lease.Name)
	}
	return nil
}

func networkIndexFromLeaseName(leaseName string) (uint32, error) {
	raw := strings.TrimPrefix(leaseName, networkLeasePrefix)
	if raw == leaseName || len(raw) != 5 {
		return 0, fmt.Errorf("invalid worker network reservation name %q", leaseName)
	}
	value, err := strconv.ParseUint(raw, 16, 32)
	if err != nil || value > 0x1ffff {
		return 0, fmt.Errorf("invalid worker network reservation name %q", leaseName)
	}
	return uint32(value), nil
}

func isRuntimeNetworkCandidate(nodeName string, index uint32) bool {
	for attempt := 0; attempt < 1024; attempt++ {
		if runtimeNetworkIndex(nodeName, attempt) == index {
			return true
		}
	}
	return false
}

type usedIPRanges struct {
	exact    map[string]bool
	networks []*net.IPNet
}

func (u usedIPRanges) Contains(value string) bool {
	ip := net.ParseIP(value)
	if ip == nil {
		return false
	}
	if u.exact[ip.String()] {
		return true
	}
	for _, network := range u.networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (u usedIPRanges) Overlaps(cidr string) bool {
	_, candidate, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	for value := range u.exact {
		if candidate.Contains(net.ParseIP(value)) {
			return true
		}
	}
	for _, network := range u.networks {
		if candidate.Contains(network.IP) || network.Contains(candidate.IP) {
			return true
		}
	}
	return false
}

func usedClusterIPs(ctx context.Context, client kubernetes.Interface) (usedIPRanges, error) {
	used := usedIPRanges{exact: make(map[string]bool)}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return used, fmt.Errorf("list Nodes while reserving worker network: %w", err)
	}
	for _, node := range nodes.Items {
		for _, address := range node.Status.Addresses {
			if ip := net.ParseIP(address.Address); ip != nil {
				used.exact[ip.String()] = true
			}
		}
	}
	raw, err := client.Discovery().RESTClient().Get().AbsPath("/apis/wirekube.io/v1alpha1/wirekubepeers").Do(ctx).Raw()
	if err != nil {
		return used, fmt.Errorf("list WireKube peers while reserving worker network: %w", err)
	}
	var peers struct {
		Items []struct {
			Spec struct {
				AllowedIPs []string `json:"allowedIPs"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &peers); err != nil {
		return used, fmt.Errorf("decode WireKube peers while reserving worker network: %w", err)
	}
	for _, peer := range peers.Items {
		for _, allowed := range peer.Spec.AllowedIPs {
			_, network, err := net.ParseCIDR(allowed)
			if err == nil {
				used.networks = append(used.networks, network)
			}
		}
	}
	return used, nil
}

func runtimeNetworkIndex(nodeName string, attempt int) uint32 {
	hash := sha256.Sum256([]byte(nodeName + ":" + strconv.Itoa(attempt)))
	return (uint32(hash[0])<<9 | uint32(hash[1])<<1 | uint32(hash[2])>>7) & 0x1ffff
}

func runtimeNetworkFromIndex(nodeName string, index uint32) RuntimeNetwork {
	base := uint32(172)<<24 | uint32(16)<<16
	address := base + index*8
	addressOf := func(offset uint32) string {
		value := address + offset
		return fmt.Sprintf("%d.%d.%d.%d", byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
	}
	macHash := sha256.Sum256([]byte(nodeName + ":" + strconv.FormatUint(uint64(index), 10)))
	return RuntimeNetwork{
		Subnet:    addressOf(0) + "/29",
		GatewayIP: addressOf(1),
		GuestIP:   addressOf(2),
		HostIP:    addressOf(6),
		MAC:       fmt.Sprintf("02:%02x:%02x:%02x:%02x:%02x", macHash[0], macHash[1], macHash[2], macHash[3], macHash[4]),
	}
}

func pointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}
