//go:build darwin

package wirekube

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

type darwinTunnel struct {
	name    string
	peerKey string
	device  *device.Device
	tun     tun.Device
	network *darwinNetwork
	once    sync.Once
	err     error
}

func startPlatformTunnel(ctx context.Context, state State, logf func(string, ...any)) (Tunnel, error) {
	resolvedState, err := resolveRelayEndpoint(ctx, state)
	if err != nil {
		return nil, err
	}
	state = resolvedState
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
		tunDevice.Close()
		return nil, fmt.Errorf("read macOS utun name: %w", err)
	}
	logger := &device.Logger{Verbosef: device.DiscardLogf, Errorf: device.DiscardLogf}
	if logf != nil {
		logger.Errorf = func(format string, values ...any) { logf("wireguard: "+format, values...) }
	}
	wireGuardDevice := device.NewDevice(tunDevice, conn.NewDefaultBind(), logger)
	tunnel := &darwinTunnel{
		name: name, peerKey: state.IngressPublicKey, device: wireGuardDevice,
		tun: tunDevice, network: network,
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
	if err := tunnel.network.Configure(ctx, name, state.AssignedMeshIP, state.AllowedDestinations); err != nil {
		return fail(err)
	}
	peerConfig, err := peerIPCConfig(state)
	if err != nil {
		return fail(err)
	}
	if err := wireGuardDevice.IpcSet(peerConfig); err != nil {
		return fail(fmt.Errorf("configure WireGuard ingress peer: %w", err))
	}
	go func() {
		<-ctx.Done()
		_ = tunnel.Close()
	}()
	return tunnel, nil
}

func (t *darwinTunnel) InterfaceName() string { return t.name }

func (t *darwinTunnel) Validate(ctx context.Context) error {
	return t.network.Validate(ctx)
}

func (t *darwinTunnel) Snapshot() (TunnelSnapshot, error) {
	output, err := t.device.IpcGet()
	if err != nil {
		return TunnelSnapshot{}, fmt.Errorf("read WireGuard state: %w", err)
	}
	return parseTunnelSnapshot(output, t.peerKey, t.name)
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

func resolveRelayEndpoint(ctx context.Context, state State) (State, error) {
	host, port, err := net.SplitHostPort(state.RelayEndpoint)
	if err != nil {
		return State{}, err
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return State{}, fmt.Errorf("resolve WireKube relay endpoint: %w", err)
	}
	_, mesh, _ := net.ParseCIDR(state.MeshCIDR)
	var selected net.IP
	for _, address := range addresses {
		if mesh.Contains(address.IP) {
			return State{}, fmt.Errorf("WireKube relay endpoint %s is inside the routed mesh CIDR %s", address.IP, state.MeshCIDR)
		}
		if selected == nil || selected.To4() == nil && address.IP.To4() != nil {
			selected = address.IP
		}
	}
	if selected == nil {
		return State{}, fmt.Errorf("WireKube relay endpoint resolved to no addresses")
	}
	state.RelayEndpoint = net.JoinHostPort(selected.String(), port)
	return state, nil
}
