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
)

type Tunnel interface {
	InterfaceName() string
	Validate(context.Context) error
	Snapshot() (TunnelSnapshot, error)
	Close() error
}

type TunnelSnapshot struct {
	InterfaceName string
	PeerPublicKey string
	LastHandshake time.Time
	BytesReceived int64
	BytesSent     int64
}

func StartTunnel(ctx context.Context, state State, logf func(string, ...any)) (Tunnel, error) {
	if err := validateTunnelState(state); err != nil {
		return nil, err
	}
	return startPlatformTunnel(ctx, state, logf)
}

func validateTunnelState(state State) error {
	if state.Version != stateVersion || state.PeerUID == "" || state.MeshIPClaimName == "" || state.MeshIPClaimUID == "" || state.MeshCIDR == "" {
		return fmt.Errorf("WireKube leaf enrollment is incomplete")
	}
	if err := validateKeyPair(state.PrivateKey, state.PublicKey); err != nil {
		return err
	}
	if err := validateKey(state.IngressPublicKey); err != nil {
		return fmt.Errorf("invalid ingress public key: %w", err)
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
	if _, _, err := net.SplitHostPort(state.RelayEndpoint); err != nil {
		return fmt.Errorf("invalid relay endpoint %q: %w", state.RelayEndpoint, err)
	}
	if len(state.AllowedDestinations) != 1 || state.AllowedDestinations[0] != state.MeshCIDR {
		return fmt.Errorf("connected leaf routes must contain only the WireKube mesh CIDR")
	}
	if state.MTU < 576 || state.MTU > 1420 {
		return fmt.Errorf("WireKube leaf MTU must be between 576 and 1420")
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
		return nil, fmt.Errorf("Kubernetes API endpoint resolved to no addresses")
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

func peerIPCConfig(state State) (string, error) {
	key, err := keyHex(state.IngressPublicKey)
	if err != nil {
		return "", err
	}
	var config strings.Builder
	fmt.Fprintf(&config, "public_key=%s\n", key)
	fmt.Fprintf(&config, "endpoint=%s\n", state.RelayEndpoint)
	config.WriteString("replace_allowed_ips=true\n")
	for _, destination := range state.AllowedDestinations {
		fmt.Fprintf(&config, "allowed_ip=%s\n", destination)
	}
	config.WriteString("persistent_keepalive_interval=25\n")
	return config.String(), nil
}

func keyHex(value string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return "", fmt.Errorf("invalid WireGuard key")
	}
	return hex.EncodeToString(decoded), nil
}

func parseTunnelSnapshot(output, peerPublicKey, interfaceName string) (TunnelSnapshot, error) {
	target, err := keyHex(peerPublicKey)
	if err != nil {
		return TunnelSnapshot{}, err
	}
	snapshot := TunnelSnapshot{InterfaceName: interfaceName, PeerPublicKey: peerPublicKey}
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
		case "last_handshake_time_sec":
			if currentPeer == target {
				handshakeSeconds, _ = strconv.ParseInt(value, 10, 64)
			}
		case "last_handshake_time_nsec":
			if currentPeer == target {
				handshakeNanos, _ = strconv.ParseInt(value, 10, 64)
			}
		case "rx_bytes":
			if currentPeer == target {
				snapshot.BytesReceived, _ = strconv.ParseInt(value, 10, 64)
			}
		case "tx_bytes":
			if currentPeer == target {
				snapshot.BytesSent, _ = strconv.ParseInt(value, 10, 64)
			}
		}
	}
	if handshakeSeconds > 0 {
		snapshot.LastHandshake = time.Unix(handshakeSeconds, handshakeNanos)
	}
	return snapshot, nil
}
