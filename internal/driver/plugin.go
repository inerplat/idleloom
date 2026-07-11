package driver

import (
	"context"
	"fmt"
	"sync"

	"github.com/inerplat/idleloom/internal/discovery"
	"github.com/inerplat/idleloom/internal/health"
	"github.com/inerplat/idleloom/internal/state"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

type Plugin struct {
	mu         sync.RWMutex
	driverName string
	poolName   string
	device     *discovery.Device
	cdiID      string
	prober     health.Prober
	state      *state.Store
}

func NewPlugin(driverName, poolName string, device *discovery.Device, cdiID string, prober health.Prober, store *state.Store) *Plugin {
	return &Plugin{
		driverName: driverName,
		poolName:   poolName,
		device:     device,
		cdiID:      cdiID,
		prober:     prober,
		state:      store,
	}
}

func (p *Plugin) SetDevice(device *discovery.Device, cdiID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.device = device
	p.cdiID = cdiID
}

func (p *Plugin) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	result := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		result[claim.UID] = p.prepare(ctx, claim)
	}
	return result, nil
}

func (p *Plugin) prepare(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	p.mu.RLock()
	device := p.device
	cdiID := p.cdiID
	p.mu.RUnlock()
	if device == nil {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("no healthy Apple Vulkan device is discovered")}
	}
	if claim.Status.Allocation == nil {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("ResourceClaim %s/%s has no allocation", claim.Namespace, claim.Name)}
	}

	var devices []kubeletplugin.Device
	seen := false
	for _, allocation := range claim.Status.Allocation.Devices.Results {
		if allocation.Driver != p.driverName {
			continue
		}
		if allocation.Pool != p.poolName || allocation.Device != device.Name {
			return kubeletplugin.PrepareResult{Err: fmt.Errorf(
				"claim allocation references unexpected device %s/%s; expected %s/%s",
				allocation.Pool, allocation.Device, p.poolName, device.Name,
			)}
		}
		if seen {
			return kubeletplugin.PrepareResult{Err: fmt.Errorf("claim allocates the exclusive device more than once")}
		}
		seen = true
		devices = append(devices, kubeletplugin.Device{
			Requests:     []string{allocation.Request},
			PoolName:     allocation.Pool,
			DeviceName:   allocation.Device,
			CDIDeviceIDs: []string{cdiID},
		})
	}
	if !seen {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("claim has no allocation for driver %s", p.driverName)}
	}

	probe := p.prober.Probe(ctx, *device)
	if !probe.Healthy {
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("device health probe failed: %w", probe.Err)}
	}
	if err := p.state.Reserve(state.PreparedClaim{
		UID:       string(claim.UID),
		Namespace: claim.Namespace,
		Name:      claim.Name,
		Pool:      p.poolName,
		Device:    device.Name,
	}); err != nil {
		return kubeletplugin.PrepareResult{Err: err}
	}
	return kubeletplugin.PrepareResult{Devices: devices}
}

func (p *Plugin) UnprepareResourceClaims(_ context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	result := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		result[claim.UID] = p.state.Release(string(claim.UID))
	}
	return result, nil
}

func (p *Plugin) HandleError(ctx context.Context, err error, msg string) {
	utilruntime.HandleErrorWithContext(ctx, err, msg)
}
