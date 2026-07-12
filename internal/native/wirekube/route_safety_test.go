package wirekube

import (
	"net"
	"testing"
)

func TestValidateMeshCIDRAcceptsOnlyReservedOverlayRanges(t *testing.T) {
	for _, value := range []string{"10.42.0.0/16", "172.31.240.0/20", "192.168.252.0/22", "100.64.0.0/10", "198.18.0.0/15"} {
		if err := validateMeshCIDR(value); err != nil {
			t.Errorf("validateMeshCIDR(%q): %v", value, err)
		}
	}
	for _, value := range []string{"0.0.0.0/0", "127.0.0.0/8", "169.254.0.0/16", "203.0.113.0/24", "224.0.0.0/4", "10.0.0.0/7"} {
		if err := validateMeshCIDR(value); err == nil {
			t.Errorf("validateMeshCIDR(%q) succeeded", value)
		}
	}
}

func TestValidateLocalNetworkOverlap(t *testing.T) {
	_, overlapping, _ := net.ParseCIDR("172.31.248.0/24")
	if err := validateLocalNetworkOverlap("172.31.240.0/20", []*net.IPNet{overlapping}); err == nil {
		t.Fatal("local network overlap was accepted")
	}
	_, separate, _ := net.ParseCIDR("192.168.1.0/24")
	if err := validateLocalNetworkOverlap("172.31.240.0/20", []*net.IPNet{separate}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateEndpointOutsideMesh(t *testing.T) {
	if err := validateEndpointOutsideMesh("172.31.240.0/20", []net.IP{net.ParseIP("172.31.241.1")}, "API"); err == nil {
		t.Fatal("endpoint inside mesh was accepted")
	}
}
