package wirekube

import (
	"strings"
	"testing"
	"time"
)

func TestTunnelIPCConfigUsesStandardWireGuardContract(t *testing.T) {
	state := testTunnelState(t)
	privateConfig, err := privateIPCConfig(state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(privateConfig, "private_key=") || strings.Contains(privateConfig, state.PrivateKey) {
		t.Fatalf("private config is not UAPI hex: %q", privateConfig)
	}
	peerConfig, err := peerIPCConfig(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"endpoint=relay.example.test:3478",
		"allowed_ip=172.31.240.0/20",
		"persistent_keepalive_interval=25",
	} {
		if !strings.Contains(peerConfig, expected) {
			t.Fatalf("peer config missing %q:\n%s", expected, peerConfig)
		}
	}
	if strings.Contains(peerConfig, state.PrivateKey) {
		t.Fatal("peer config exposed the private key")
	}
}

func TestValidateTunnelStateRejectsBroaderRoutes(t *testing.T) {
	state := testTunnelState(t)
	state.AllowedDestinations = []string{state.MeshCIDR, "10.0.0.0/8"}
	if err := validateTunnelState(state); err == nil {
		t.Fatal("validateTunnelState accepted routes outside the mesh CIDR")
	}
}

func TestParseTunnelSnapshotSelectsIngressPeer(t *testing.T) {
	state := testTunnelState(t)
	key, err := keyHex(state.IngressPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	otherPrivate, otherPublic, err := generateKeyPair()
	_ = otherPrivate
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _ := keyHex(otherPublic)
	output := strings.Join([]string{
		"public_key=" + otherKey,
		"last_handshake_time_sec=1",
		"rx_bytes=2",
		"tx_bytes=3",
		"public_key=" + key,
		"last_handshake_time_sec=1700000000",
		"last_handshake_time_nsec=123",
		"rx_bytes=456",
		"tx_bytes=789",
	}, "\n")
	snapshot, err := parseTunnelSnapshot(output, state.IngressPublicKey, "utun9")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LastHandshake != time.Unix(1700000000, 123) || snapshot.BytesReceived != 456 || snapshot.BytesSent != 789 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func testTunnelState(t *testing.T) State {
	t.Helper()
	privateKey, publicKey, err := generateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	_, ingressPublicKey, err := generateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return State{
		Version: stateVersion, PeerName: "idleloom-mac-one", DisplayName: "idleloom-mac-one",
		EnrollmentID: "enrollment-one", PeerUID: "peer-uid", PrivateKey: privateKey, PublicKey: publicKey,
		MeshCIDR: "172.31.240.0/20", AssignedMeshIP: "172.31.241.10/32",
		RelayEndpoint: "relay.example.test:3478", IngressPublicKey: ingressPublicKey,
		AllowedDestinations: []string{"172.31.240.0/20"}, MTU: 1248,
		KubernetesAPIEndpoint: "https://203.0.113.10:6443",
		MeshIPClaimName:       "idleloom-wirekube-172-31-241-10", MeshIPClaimUID: "claim-uid",
	}
}
