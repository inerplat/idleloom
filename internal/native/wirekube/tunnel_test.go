package wirekube

import (
	"strings"
	"testing"
	"time"
)

func TestTunnelPrivateIPCConfigUsesWireGuardHexKey(t *testing.T) {
	state := testTunnelState(t)
	privateConfig, err := privateIPCConfig(state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(privateConfig, "private_key=") || strings.Contains(privateConfig, state.PrivateKey) {
		t.Fatalf("private config is not UAPI hex: %q", privateConfig)
	}
}

func TestValidateTunnelStateAcceptsWkpeerAndRejectsBroaderRoutes(t *testing.T) {
	state := testTunnelState(t)
	if err := validateTunnelState(state); err != nil {
		t.Fatal(err)
	}
	state.AllowedDestinations = []string{state.MeshCIDR, "10.0.0.0/8"}
	if err := validateTunnelState(state); err == nil {
		t.Fatal("validateTunnelState accepted routes outside the mesh CIDR")
	}
}

func TestParseTunnelSnapshotAggregatesSynchronizedPeers(t *testing.T) {
	_, firstPublic, err := generateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	_, secondPublic, err := generateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	_, ignoredPublic, err := generateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	firstHex, _ := keyHex(firstPublic)
	secondHex, _ := keyHex(secondPublic)
	ignoredHex, _ := keyHex(ignoredPublic)
	output := strings.Join([]string{
		"public_key=" + firstHex,
		"last_handshake_time_sec=1700000000",
		"last_handshake_time_nsec=123",
		"rx_bytes=100",
		"tx_bytes=200",
		"public_key=" + ignoredHex,
		"last_handshake_time_sec=1800000000",
		"rx_bytes=999",
		"tx_bytes=999",
		"public_key=" + secondHex,
		"last_handshake_time_sec=1700000010",
		"last_handshake_time_nsec=456",
		"rx_bytes=300",
		"tx_bytes=400",
	}, "\n")
	snapshot, err := parseTunnelSnapshotForPeers(output, "utun9", map[string]struct{}{firstPublic: {}, secondPublic: {}})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LastHandshake != time.Unix(1700000010, 456) || snapshot.BytesReceived != 400 || snapshot.BytesSent != 600 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func testTunnelState(t *testing.T) State {
	t.Helper()
	privateKey, publicKey, err := generateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return State{
		Version: stateVersion, PeerMode: peerModeWireKube,
		PeerName: "idleloom-mac-one", DisplayName: "idleloom-mac-one",
		EnrollmentID: "enrollment-one", PeerUID: "peer-uid", PrivateKey: privateKey, PublicKey: publicKey,
		MeshCIDR: "172.31.240.0/20", AssignedMeshIP: "172.31.241.10/32",
		RelayTransport: "tcp", RelayEndpoint: "relay.example.test:3478",
		AllowedDestinations: []string{"172.31.240.0/20"}, MTU: 1248,
		KubernetesAPIEndpoint: "https://203.0.113.10:6443",
		MeshIPClaimName:       "idleloom-wirekube-172-31-241-10", MeshIPClaimUID: "claim-uid",
		PeerNamespace: "idleloom-host-mac-one", PeerServiceAccount: "wirekube-relay-peer-idleloom-mac-one",
		LinkKubeconfig: "/private/wirekube-peer.kubeconfig",
	}
}
