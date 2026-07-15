package wirekube

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
)

type Tunnel interface {
	InterfaceName() string
	SyncPeers(context.Context, dynamic.Interface) error
	Validate(context.Context) error
	Snapshot() (TunnelSnapshot, error)
	Close() error
}

type TunnelConfig struct {
	RelayTokenFile string
}

type TunnelSnapshot struct {
	InterfaceName string
	LastHandshake time.Time
	BytesReceived int64
	BytesSent     int64
}

func StartTunnel(ctx context.Context, state State, config TunnelConfig, logf func(string, ...any)) (Tunnel, error) {
	if err := validateTunnelState(state); err != nil {
		return nil, err
	}
	if state.RelayTransport == "wss" && config.RelayTokenFile == "" {
		return nil, fmt.Errorf("the WireKube WSS relay token file is required")
	}
	return startPlatformTunnel(ctx, state, config, logf)
}

func validateTunnelState(state State) error {
	if state.Version != stateVersion || state.PeerMode != peerModeWireKube || state.PeerUID == "" || state.MeshIPClaimName == "" || state.MeshIPClaimUID == "" || state.MeshCIDR == "" {
		return fmt.Errorf("the WireKube leaf enrollment is incomplete")
	}
	if state.PeerNamespace == "" || state.PeerServiceAccount != peerServiceAccountName(state.PeerName) || state.LinkKubeconfig == "" {
		return fmt.Errorf("the WireKube peer identity is incomplete")
	}
	if err := validateKeyPair(state.PrivateKey, state.PublicKey); err != nil {
		return err
	}
	if err := validateAssignedAddress(state.AssignedMeshIP, state.MeshCIDR); err != nil {
		return err
	}
	if err := validateMeshCIDR(state.MeshCIDR); err != nil {
		return err
	}
	if err := validateAPIEndpoint(state.KubernetesAPIEndpoint); err != nil {
		return err
	}
	switch state.RelayTransport {
	case "wss":
		parsed, err := url.Parse(state.RelayEndpoint)
		if err != nil || parsed.Scheme != "wss" || parsed.Host == "" {
			return fmt.Errorf("invalid WireKube WSS relay endpoint %q", state.RelayEndpoint)
		}
		if state.RelayTokenAudience != peerRelayAudience {
			return fmt.Errorf("the WireKube WSS relay audience is invalid")
		}
	case "tcp":
		if _, _, err := net.SplitHostPort(state.RelayEndpoint); err != nil {
			return fmt.Errorf("invalid WireKube TCP relay endpoint %q: %w", state.RelayEndpoint, err)
		}
	default:
		return fmt.Errorf("unsupported WireKube relay transport %q", state.RelayTransport)
	}
	if len(state.AllowedDestinations) != 1 || state.AllowedDestinations[0] != state.MeshCIDR {
		return fmt.Errorf("connected leaf routes must contain only the WireKube mesh CIDR")
	}
	if state.MTU < 576 || state.MTU > 1420 {
		return fmt.Errorf("the WireKube leaf MTU must be between 576 and 1420")
	}
	return nil
}

func resolveAPIEndpoint(ctx context.Context, endpoint string) ([]net.IP, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("parse Kubernetes API endpoint %q", endpoint)
	}
	if address := net.ParseIP(parsed.Hostname()); address != nil {
		return []net.IP{address}, nil
	}
	resolved, err := net.DefaultResolver.LookupIPAddr(ctx, parsed.Hostname())
	if err != nil {
		return nil, fmt.Errorf("resolve Kubernetes API endpoint: %w", err)
	}
	addresses := make([]net.IP, 0, len(resolved))
	for _, address := range resolved {
		addresses = append(addresses, address.IP)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("kubernetes API endpoint resolved to no addresses")
	}
	return addresses, nil
}

func privateIPCConfig(state State) (string, error) {
	key, err := keyHex(state.PrivateKey)
	if err != nil {
		return "", err
	}
	return "private_key=" + key + "\n", nil
}

func keyHex(value string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return "", fmt.Errorf("invalid WireGuard key")
	}
	return hex.EncodeToString(decoded), nil
}

func parseTunnelSnapshotForPeers(output, interfaceName string, peerPublicKeys map[string]struct{}) (TunnelSnapshot, error) {
	targets := make(map[string]struct{}, len(peerPublicKeys))
	for key := range peerPublicKeys {
		hexKey, err := keyHex(key)
		if err != nil {
			return TunnelSnapshot{}, err
		}
		targets[hexKey] = struct{}{}
	}
	snapshot := TunnelSnapshot{InterfaceName: interfaceName}
	currentPeer := ""
	var handshakeSeconds, handshakeNanos int64
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "public_key":
			currentPeer = value
			handshakeSeconds = 0
		case "last_handshake_time_sec":
			if _, ok := targets[currentPeer]; ok {
				handshakeSeconds, _ = strconv.ParseInt(value, 10, 64)
			}
		case "last_handshake_time_nsec":
			if _, ok := targets[currentPeer]; ok {
				handshakeNanos, _ = strconv.ParseInt(value, 10, 64)
				if handshakeSeconds > 0 {
					handshake := time.Unix(handshakeSeconds, handshakeNanos)
					if handshake.After(snapshot.LastHandshake) {
						snapshot.LastHandshake = handshake
					}
				}
			}
		case "rx_bytes":
			if _, ok := targets[currentPeer]; ok {
				parsed, _ := strconv.ParseInt(value, 10, 64)
				snapshot.BytesReceived += parsed
			}
		case "tx_bytes":
			if _, ok := targets[currentPeer]; ok {
				parsed, _ := strconv.ParseInt(value, 10, 64)
				snapshot.BytesSent += parsed
			}
		}
	}
	return snapshot, nil
}

func stalePeerPublicKeys(previous, next map[string]string) []string {
	active := make(map[string]struct{}, len(next))
	for _, publicKey := range next {
		active[publicKey] = struct{}{}
	}
	stale := make(map[string]struct{})
	for _, publicKey := range previous {
		if _, ok := active[publicKey]; !ok {
			stale[publicKey] = struct{}{}
		}
	}
	result := make([]string, 0, len(stale))
	for publicKey := range stale {
		result = append(result, publicKey)
	}
	return result
}
