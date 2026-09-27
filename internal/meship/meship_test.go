package meship

import "testing"

// TestIPForNameMatchesWireKube pins the allocator against addresses WireKube
// assigned in a live mesh. If these drift, idlectl would hand kubelet a
// --node-ip that the mesh never routes.
func TestIPForNameMatchesWireKube(t *testing.T) {
	const meshCIDR = "198.18.18.0/24"
	for name, want := range map[string]string{
		"master":  "198.18.18.74/32",
		"worker1": "198.18.18.83/32",
		"worker2": "198.18.18.180/32",
		"worker3": "198.18.18.23/32",
		"worker4": "198.18.18.120/32",
		"worker5": "198.18.18.217/32",
		"worker6": "198.18.18.60/32",
		"worker7": "198.18.18.157/32",
		"worker8": "198.18.18.226/32",
	} {
		got, err := IPForName(name, meshCIDR)
		if err != nil {
			t.Fatalf("IPForName(%q): %v", name, err)
		}
		if got != want {
			t.Errorf("IPForName(%q) = %s, want %s", name, got, want)
		}
	}
}

func TestAddressForNameDropsThePrefixLength(t *testing.T) {
	address, err := AddressForName("worker1", "198.18.18.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if address != "198.18.18.83" {
		t.Errorf("AddressForName = %s, want 198.18.18.83", address)
	}
}

func TestIPForNameRejectsUnusableCIDRs(t *testing.T) {
	for _, meshCIDR := range []string{"", "not-a-cidr", "2001:db8::/32", "10.0.0.0/31"} {
		if _, err := IPForName("worker1", meshCIDR); err == nil {
			t.Errorf("IPForName accepted mesh CIDR %q", meshCIDR)
		}
	}
}
