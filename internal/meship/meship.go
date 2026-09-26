// Package meship derives a WireKube mesh address from a stable name.
//
// It mirrors WireKube's own allocator: a 32-bit FNV-1a hash of the name,
// reduced into the usable host range of the mesh CIDR. There is no central
// allocator, so every participant computes the same address for the same
// name. That is what lets idlectl know a worker's mesh address before the
// node exists, and it is why the address can be handed to kubelet as
// --node-ip rather than waiting for the WireKube agent to assign one.
package meship

import (
	"fmt"
	"net"
)

// IPForName returns the /32 mesh address for name inside meshCIDR.
func IPForName(name, meshCIDR string) (string, error) {
	_, network, err := net.ParseCIDR(meshCIDR)
	if err != nil {
		return "", fmt.Errorf("invalid mesh CIDR %q: %w", meshCIDR, err)
	}
	base := network.IP.To4()
	if base == nil {
		return "", fmt.Errorf("mesh CIDR must be IPv4")
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones > 30 {
		return "", fmt.Errorf("mesh CIDR is too small")
	}
	size := uint32(1) << uint(bits-ones)
	// Skip the network and broadcast addresses: offset lands in [1, size-2].
	offset := fnv32a(name)%(size-2) + 1
	value := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	value += offset
	address := net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
	return address.String() + "/32", nil
}

// AddressForName is IPForName without the /32 suffix, for callers that need a
// bare address such as a kubelet --node-ip flag.
func AddressForName(name, meshCIDR string) (string, error) {
	value, err := IPForName(name, meshCIDR)
	if err != nil {
		return "", err
	}
	address, _, err := net.ParseCIDR(value)
	if err != nil {
		return "", fmt.Errorf("derive mesh address for %q: %w", name, err)
	}
	return address.String(), nil
}

func fnv32a(value string) uint32 {
	const (
		offset32 = uint32(2166136261)
		prime32  = uint32(16777619)
	)
	hash := offset32
	for index := 0; index < len(value); index++ {
		hash ^= uint32(value[index])
		hash *= prime32
	}
	return hash
}
