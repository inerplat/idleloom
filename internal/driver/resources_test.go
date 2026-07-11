package driver

import (
	"testing"

	"github.com/inerplat/idleloom/internal/discovery"
)

func TestDesiredResourcesPublishesOnlyHealthyDevice(t *testing.T) {
	device := &discovery.Device{Name: "renderd128", Path: "/dev/dri/renderD128", SysfsDriver: "virtio_gpu"}
	healthy := DesiredResources("gpu.apple-vulkan.example", "worker-1", device, true)
	got := healthy.Pools["worker-1"].Slices[0].Devices
	if len(got) != 1 || got[0].Name != "renderd128" {
		t.Fatalf("healthy devices = %#v", got)
	}

	unhealthy := DesiredResources("gpu.apple-vulkan.example", "worker-1", device, false)
	if got := unhealthy.Pools["worker-1"].Slices[0].Devices; len(got) != 0 {
		t.Fatalf("unhealthy devices = %#v, want none", got)
	}
}
