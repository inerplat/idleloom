package wirekube

import (
	"fmt"
	"net"
)

var safeMeshRanges = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"100.64.0.0/10",
	"198.18.0.0/15",
}

func isSafeMeshNetwork(network *net.IPNet) bool {
	for _, value := range safeMeshRanges {
		_, allowed, _ := net.ParseCIDR(value)
		if cidrContainsNetwork(allowed, network) {
			return true
		}
	}
	return false
}

func validateLocalNetworkOverlap(meshCIDR string, localNetworks []*net.IPNet) error {
	_, mesh, err := net.ParseCIDR(meshCIDR)
	if err != nil {
		return err
	}
	for _, local := range localNetworks {
		if local == nil || local.IP.To4() == nil {
			continue
		}
		if cidrsOverlap(mesh, local) {
			return fmt.Errorf("the WireKube mesh CIDR %s overlaps local interface network %s", meshCIDR, local.String())
		}
	}
	return nil
}

func localInterfaceNetworks() ([]*net.IPNet, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list local interfaces: %w", err)
	}
	var networks []*net.IPNet
	for _, iface := range interfaces {
		addresses, addressErr := iface.Addrs()
		if addressErr != nil {
			return nil, fmt.Errorf("list addresses for %s: %w", iface.Name, addressErr)
		}
		for _, address := range addresses {
			_, network, parseErr := net.ParseCIDR(address.String())
			if parseErr == nil && network.IP.To4() != nil {
				networks = append(networks, network)
			}
		}
	}
	return networks, nil
}

func validateEndpointOutsideMesh(meshCIDR string, addresses []net.IP, label string) error {
	_, mesh, err := net.ParseCIDR(meshCIDR)
	if err != nil {
		return err
	}
	for _, address := range addresses {
		if mesh.Contains(address) {
			return fmt.Errorf("%s address %s is inside routed WireKube mesh CIDR %s", label, address, meshCIDR)
		}
	}
	return nil
}

func cidrsOverlap(left, right *net.IPNet) bool {
	return left.Contains(right.IP) || right.Contains(left.IP)
}

func cidrContainsNetwork(parent, child *net.IPNet) bool {
	if parent == nil || child == nil || parent.IP.To4() == nil || child.IP.To4() == nil {
		return false
	}
	ones, bits := child.Mask.Size()
	if bits != 32 {
		return false
	}
	last := append(net.IP(nil), child.IP.To4()...)
	for bit := ones; bit < bits; bit++ {
		last[bit/8] |= 1 << uint(7-bit%8)
	}
	return parent.Contains(child.IP) && parent.Contains(last)
}
