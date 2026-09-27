// Package meship derives a WireKube mesh address from a stable name.
//
// It mirrors WireKube's own allocator (wirekube/pkg/meship) bit for bit: a
// 32-bit FNV-1a hash of the name, reduced into the usable host range of the
// mesh CIDR, and for later attempts a walk over that range by a stride hashed
// from the name and forced coprime to the range size. Every participant
// computes the same sequence for the same name, which is what lets idlectl
// know a worker's mesh address before the node exists, and why the address can
// be handed to kubelet as --node-ip rather than waiting for the WireKube agent
// to assign one.
//
// It is a copy rather than an import so that idlectl does not carry WireKube's
// controller-runtime dependency tree into a CLI. The cost is that the two have
// to stay in step, which the golden vectors in the test enforce.
package meship

import (
	"fmt"
	"net"
)

// IPForName returns the /32 mesh address for name inside meshCIDR. It is
// IPForNameAttempt(name, meshCIDR, 0).
func IPForName(name, meshCIDR string) (string, error) {
	return IPForNameAttempt(name, meshCIDR, 0)
}

// IPForNameAttempt returns the attempt'th candidate address for name.
//
// Attempt 0 is the address the name hashes to, which is what WireKube assigns
// when nothing contests it. Later attempts walk the usable range as a
// permutation, visiting every address exactly once before repeating, so a
// caller probing for a free address never re-tries one it has ruled out and
// finds a free one whenever one exists.
func IPForNameAttempt(name, meshCIDR string, attempt int) (string, error) {
	if attempt < 0 {
		return "", fmt.Errorf("attempt must not be negative, got %d", attempt)
	}
	base, size, err := parseMesh(meshCIDR)
	if err != nil {
		return "", err
	}
	usable := size - 2
	start := fnv32a(name) % usable
	index := start
	if attempt > 0 {
		step := stepForName(name, usable)
		// Reducing the attempt first keeps the multiplication inside 64 bits
		// for any attempt a caller can pass, not just ones below the capacity.
		index = uint32((uint64(start) + uint64(step)*(uint64(attempt)%uint64(usable))) % uint64(usable))
	}
	// Skip the network and broadcast addresses: the offset lands in [1, size-2].
	value := base + index + 1
	address := net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
	return address.String() + "/32", nil
}

// AddressForName is IPForName without the /32 suffix, for callers that need a
// bare address such as a kubelet --node-ip flag.
func AddressForName(name, meshCIDR string) (string, error) {
	return AddressForNameAttempt(name, meshCIDR, 0)
}

// AddressForNameAttempt is IPForNameAttempt without the /32 suffix.
func AddressForNameAttempt(name, meshCIDR string, attempt int) (string, error) {
	value, err := IPForNameAttempt(name, meshCIDR, attempt)
	if err != nil {
		return "", err
	}
	address, _, err := net.ParseCIDR(value)
	if err != nil {
		return "", fmt.Errorf("derive mesh address for %q: %w", name, err)
	}
	return address.String(), nil
}

// Capacity is how many addresses meshCIDR can hand out, which is also how many
// distinct attempts there are before the walk repeats.
func Capacity(meshCIDR string) (int, error) {
	_, size, err := parseMesh(meshCIDR)
	if err != nil {
		return 0, err
	}
	return int(size - 2), nil
}

// Contains reports whether address is a host address meshCIDR can hand out.
// It is stricter than net.IPNet.Contains: the network and broadcast addresses
// are inside the CIDR but are not assignable, and the walk never returns them.
func Contains(address, meshCIDR string) bool {
	ip, network, err := net.ParseCIDR(address)
	if err != nil {
		return false
	}
	if ones, bits := network.Mask.Size(); ones != 32 || bits != 32 {
		return false
	}
	base, size, err := parseMesh(meshCIDR)
	if err != nil {
		return false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	value := uint32(ip4[0])<<24 | uint32(ip4[1])<<16 | uint32(ip4[2])<<8 | uint32(ip4[3])
	return value > base && value < base+size-1
}

// parseMesh validates meshCIDR and returns its network address as a 32-bit
// integer along with the size of its address space.
func parseMesh(meshCIDR string) (base, size uint32, err error) {
	_, network, err := net.ParseCIDR(meshCIDR)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid mesh CIDR %q: %w", meshCIDR, err)
	}
	ip := network.IP.To4()
	if ip == nil {
		return 0, 0, fmt.Errorf("mesh CIDR must be IPv4")
	}
	ones, bits := network.Mask.Size()
	// A /0 shifts out of range and lands on zero, which the size check below
	// rejects along with the genuinely too-small prefixes.
	size = uint32(1) << uint(bits-ones)
	if size < 4 {
		return 0, 0, fmt.Errorf("mesh CIDR is too small")
	}
	base = uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	return base, size, nil
}

// stepForName picks the stride of the permutation walk: a value in
// [1, usable-1] coprime to usable, so repeatedly adding it modulo usable
// cycles through every offset before repeating. The candidate comes from a
// second hash so two names that share a starting offset still diverge on their
// first retry.
func stepForName(name string, usable uint32) uint32 {
	if usable < 3 {
		return 1
	}
	step := fnv32a("step\x00"+name)%(usable-1) + 1
	for gcd(step, usable) != 1 {
		step++
		if step >= usable {
			step = 1
		}
	}
	return step
}

func gcd(a, b uint32) uint32 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
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
