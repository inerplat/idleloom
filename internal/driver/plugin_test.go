package driver

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/inerplat/idleloom/internal/discovery"
	"github.com/inerplat/idleloom/internal/health"
	"github.com/inerplat/idleloom/internal/state"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

type healthyProbe struct{}

func (healthyProbe) Probe(context.Context, discovery.Device) health.Result {
	return health.Result{Healthy: true}
}

func TestPluginEnforcesExclusiveClaimLease(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	device := &discovery.Device{Name: "renderd128", Path: "/dev/dri/renderD128"}
	plugin := NewPlugin(
		"gpu.apple-vulkan.example",
		"worker-1",
		device,
		"gpu.apple-vulkan.example/render=renderd128",
		healthyProbe{},
		store,
	)

	first := allocatedClaim("claim-1")
	prepared, err := plugin.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{first})
	if err != nil || prepared[first.UID].Err != nil {
		t.Fatalf("prepare first claim: result=%#v err=%v", prepared, err)
	}
	prepared, err = plugin.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{first})
	if err != nil || prepared[first.UID].Err != nil {
		t.Fatalf("idempotent prepare: result=%#v err=%v", prepared, err)
	}

	second := allocatedClaim("claim-2")
	prepared, err = plugin.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{second})
	if err != nil {
		t.Fatalf("prepare second overall error = %v", err)
	}
	if prepared[second.UID].Err == nil {
		t.Fatal("second claim unexpectedly prepared the exclusive device")
	}

	unprepared, err := plugin.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{{UID: first.UID}})
	if err != nil || unprepared[first.UID] != nil {
		t.Fatalf("unprepare first claim: result=%#v err=%v", unprepared, err)
	}
	prepared, err = plugin.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{second})
	if err != nil || prepared[second.UID].Err != nil {
		t.Fatalf("prepare second after release: result=%#v err=%v", prepared, err)
	}
}

func allocatedClaim(uid string) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      uid,
			UID:       types.UID(uid),
		},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{
							Request: "accelerator",
							Driver:  "gpu.apple-vulkan.example",
							Pool:    "worker-1",
							Device:  "renderd128",
						},
					},
				},
			},
		},
	}
}
