//go:build darwin

package wirekube

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

	agentrelay "github.com/inerplat/wirekube/pkg/agent/relay"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

type darwinTunnel struct {
	name    string
	state   State
	device  *device.Device
	tun     tun.Device
	network *darwinNetwork
	relay   *agentrelay.Pool

	mu       sync.RWMutex
	peerKeys map[string]string
	once     sync.Once
	err      error
}

func startPlatformTunnel(ctx context.Context, state State, config TunnelConfig, logf func(string, ...any)) (Tunnel, error) {
	if err := validateEndpointOutsideMeshForRelay(ctx, state); err != nil {
		return nil, err
	}
	apiAddresses, err := resolveAPIEndpoint(ctx, state.KubernetesAPIEndpoint)
	if err != nil {
		return nil, err
	}
	if err := validateEndpointOutsideMesh(state.MeshCIDR, apiAddresses, "Kubernetes API endpoint"); err != nil {
		return nil, err
	}
	localNetworks, err := localInterfaceNetworks()
	if err != nil {
		return nil, err
	}
	if err := validateLocalNetworkOverlap(state.MeshCIDR, localNetworks); err != nil {
		return nil, err
	}
	network := newDarwinNetwork(execCommandRunner{})
	if err := network.Preflight(ctx, state.MeshCIDR, state.AssignedMeshIP); err != nil {
		return nil, err
	}
	tunDevice, err := tun.CreateTUN("utun", int(state.MTU))
	if err != nil {
		return nil, fmt.Errorf("create macOS utun: %w", err)
	}
	name, err := tunDevice.Name()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("read macOS utun name: %w", err), tunDevice.Close())
	}
	logger := &device.Logger{Verbosef: device.DiscardLogf, Errorf: device.DiscardLogf}
	if logf != nil {
		logger.Errorf = func(format string, values ...any) { logf("wireguard: "+format, values...) }
	}
	wireGuardDevice := device.NewDevice(tunDevice, conn.NewDefaultBind(), logger)
	tunnel := &darwinTunnel{
		name: name, state: state, device: wireGuardDevice, tun: tunDevice, network: network,
		peerKeys: make(map[string]string),
	}
	fail := func(cause error) (Tunnel, error) {
		return nil, errors.Join(cause, tunnel.Close())
	}
	privateConfig, err := privateIPCConfig(state)
	if err != nil {
		return fail(err)
	}
	if err := wireGuardDevice.IpcSet(privateConfig); err != nil {
		return fail(fmt.Errorf("configure WireGuard private key: %w", err))
	}
	if err := wireGuardDevice.Up(); err != nil {
		return fail(fmt.Errorf("start WireGuard device: %w", err))
	}
	if err := wireGuardDevice.IpcSet("listen_port=0\n"); err != nil {
		return fail(fmt.Errorf("allocate WireGuard listen port: %w", err))
	}
	listenPort, err := wireGuardListenPort(wireGuardDevice)
	if err != nil {
		return fail(err)
	}
	if err := tunnel.network.Configure(ctx, name, state.AssignedMeshIP, state.AllowedDestinations); err != nil {
		return fail(err)
	}
	publicKey, err := relayPublicKey(state.PublicKey)
	if err != nil {
		return fail(err)
	}
	tunnel.relay = agentrelay.NewPool(state.RelayEndpoint, publicKey, listenPort)
	if state.RelayTransport == "wss" {
		tunnel.relay.SetTokenFile(config.RelayTokenFile)
	}
	if err := tunnel.relay.Connect(ctx); err != nil && logf != nil {
		logf("relay initial connection failed; retrying in background: %v", err)
	}
	go func() {
		<-ctx.Done()
		_ = tunnel.Close()
	}()
	return tunnel, nil
}

func (t *darwinTunnel) InterfaceName() string { return t.name }

func (t *darwinTunnel) SyncPeers(ctx context.Context, client dynamic.Interface) error {
	if client == nil {
		return fmt.Errorf("the WireKube peer client is required")
	}
	list, err := client.Resource(PeersGVR).List(ctx, v1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list WireKube peers: %w", err)
	}
	var config strings.Builder
	config.WriteString("replace_peers=true\n")
	next := make(map[string]string)
	for index := range list.Items {
		peer := &list.Items[index]
		if peer.GetName() == t.state.PeerName {
			continue
		}
		publicKey, _, _ := unstructured.NestedString(peer.Object, "spec", "publicKey")
		if publicKey == "" {
			continue
		}
		key, err := relayPublicKey(publicKey)
		if err != nil {
			continue
		}
		allowed, _, _ := unstructured.NestedStringSlice(peer.Object, "spec", "allowedIPs")
		meshRoutes, err := peerMeshRoutes(allowed, t.state.MeshCIDR)
		if err != nil {
			continue
		}
		proxy, err := t.relay.GetOrCreateProxy(key)
		if err != nil {
			return fmt.Errorf("create relay path for WireKubePeer/%s: %w", peer.GetName(), err)
		}
		hexKey, _ := keyHex(publicKey)
		fmt.Fprintf(&config, "public_key=%s\n", hexKey)
		fmt.Fprintf(&config, "endpoint=%s\n", proxy.ListenAddr())
		config.WriteString("replace_allowed_ips=true\n")
		for _, route := range meshRoutes {
			fmt.Fprintf(&config, "allowed_ip=%s\n", route)
		}
		keepalive, _, _ := unstructured.NestedInt64(peer.Object, "spec", "persistentKeepalive")
		if keepalive <= 0 || keepalive > 65535 {
			keepalive = 25
		}
		fmt.Fprintf(&config, "persistent_keepalive_interval=%d\n", keepalive)
		next[peer.GetName()] = publicKey
	}
	if len(next) == 0 {
		t.mu.Lock()
		if err := t.device.IpcSet(config.String()); err != nil {
			t.mu.Unlock()
			return fmt.Errorf("clear unusable WireKube peers: %w", err)
		}
		previous := t.peerKeys
		t.peerKeys = next
		t.mu.Unlock()
		for _, publicKey := range stalePeerPublicKeys(previous, next) {
			key, err := relayPublicKey(publicKey)
			if err == nil {
				t.relay.RemoveProxy(key)
			}
		}
		return fmt.Errorf("the WireKube mesh has no remote peers with usable mesh routes")
	}
	t.mu.Lock()
	if err := t.device.IpcSet(config.String()); err != nil {
		t.mu.Unlock()
		return fmt.Errorf("synchronize WireKube peers: %w", err)
	}
	previous := t.peerKeys
	t.peerKeys = next
	t.mu.Unlock()
	for _, publicKey := range stalePeerPublicKeys(previous, next) {
		key, err := relayPublicKey(publicKey)
		if err == nil {
			t.relay.RemoveProxy(key)
		}
	}
	return nil
}

func (t *darwinTunnel) Validate(ctx context.Context) error {
	if err := t.network.Validate(ctx); err != nil {
		return err
	}
	if !t.relay.IsConnected() {
		return fmt.Errorf("the WireKube relay is not connected")
	}
	t.mu.RLock()
	peerCount := len(t.peerKeys)
	t.mu.RUnlock()
	if peerCount == 0 {
		return fmt.Errorf("the WireKube peers are not synchronized")
	}
	return nil
}

func (t *darwinTunnel) Snapshot() (TunnelSnapshot, error) {
	t.mu.RLock()
	output, err := t.device.IpcGet()
	keys := make(map[string]struct{}, len(t.peerKeys))
	for _, key := range t.peerKeys {
		keys[key] = struct{}{}
	}
	t.mu.RUnlock()
	if err != nil {
		return TunnelSnapshot{}, fmt.Errorf("read WireGuard state: %w", err)
	}
	return parseTunnelSnapshotForPeers(output, t.name, keys)
}

func (t *darwinTunnel) Close() error {
	t.once.Do(func() {
		var values []error
		if t.network != nil {
			var cleanupErr error
			for range 3 {
				cleanupErr = t.network.Cleanup(context.Background())
				if cleanupErr == nil {
					break
				}
			}
			values = append(values, cleanupErr)
		}
		if t.relay != nil {
			t.relay.Close()
		}
		if t.device != nil {
			t.device.Close()
		}
		if t.tun != nil {
			values = append(values, t.tun.Close())
		}
		t.err = errors.Join(values...)
	})
	return t.err
}

func wireGuardListenPort(wireGuardDevice *device.Device) (int, error) {
	output, err := wireGuardDevice.IpcGet()
	if err != nil {
		return 0, fmt.Errorf("read WireGuard listen port: %w", err)
	}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if key != "listen_port" || !ok {
			continue
		}
		port, err := strconv.Atoi(value)
		if err == nil && port > 0 && port <= 65535 {
			return port, nil
		}
	}
	return 0, fmt.Errorf("the WireGuard implementation did not allocate a listen port")
}

func relayPublicKey(value string) ([32]byte, error) {
	var result [32]byte
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(decoded) != len(result) {
		return result, fmt.Errorf("expected a base64-encoded 32-byte key")
	}
	copy(result[:], decoded)
	return result, nil
}

func peerMeshRoutes(values []string, meshCIDR string) ([]string, error) {
	_, mesh, err := net.ParseCIDR(meshCIDR)
	if err != nil {
		return nil, err
	}
	var routes []string
	for _, value := range values {
		ip, network, err := net.ParseCIDR(value)
		if err != nil || ip.To4() == nil {
			continue
		}
		ones, bits := network.Mask.Size()
		if bits == 32 && ones == 32 && mesh.Contains(ip) {
			routes = append(routes, value)
		}
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("has no /32 route inside WireKube mesh %s", meshCIDR)
	}
	return routes, nil
}

func validateEndpointOutsideMeshForRelay(ctx context.Context, state State) error {
	host := ""
	if state.RelayTransport == "wss" {
		parsed, err := url.Parse(state.RelayEndpoint)
		if err != nil {
			return err
		}
		host = parsed.Hostname()
	} else {
		var err error
		host, _, err = net.SplitHostPort(state.RelayEndpoint)
		if err != nil {
			return err
		}
	}
	addresses, err := resolveHost(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve WireKube relay endpoint: %w", err)
	}
	return validateEndpointOutsideMesh(state.MeshCIDR, addresses, "WireKube relay endpoint")
}

func resolveHost(ctx context.Context, host string) ([]net.IP, error) {
	if address := net.ParseIP(host); address != nil {
		return []net.IP{address}, nil
	}
	resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	addresses := make([]net.IP, 0, len(resolved))
	for _, address := range resolved {
		addresses = append(addresses, address.IP)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("resolved to no addresses")
	}
	return addresses, nil
}

func resolveRelayEndpoint(ctx context.Context, state State) (State, error) {
	if state.RelayTransport != "" && state.RelayTransport != "tcp" {
		return State{}, fmt.Errorf("numeric relay endpoint resolution applies only to TCP transport")
	}
	host, port, err := net.SplitHostPort(state.RelayEndpoint)
	if err != nil {
		return State{}, err
	}
	addresses, err := resolveHost(ctx, host)
	if err != nil {
		return State{}, fmt.Errorf("resolve WireKube relay endpoint: %w", err)
	}
	_, mesh, _ := net.ParseCIDR(state.MeshCIDR)
	var selected net.IP
	for _, address := range addresses {
		if mesh != nil && mesh.Contains(address) {
			return State{}, fmt.Errorf("the WireKube relay endpoint %s is inside the routed mesh CIDR %s", address, state.MeshCIDR)
		}
		if selected == nil || selected.To4() == nil && address.To4() != nil {
			selected = address
		}
	}
	if selected == nil {
		return State{}, fmt.Errorf("the WireKube relay endpoint resolved to no addresses")
	}
	state.RelayEndpoint = net.JoinHostPort(selected.String(), port)
	return state, nil
}
