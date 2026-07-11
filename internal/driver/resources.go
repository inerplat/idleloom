package driver

import (
	"runtime"

	"github.com/inerplat/idleloom/internal/discovery"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
)

func DesiredResources(driverName, nodeName string, device *discovery.Device, healthy bool) resourceslice.DriverResources {
	devices := []resourceapi.Device{}
	if device != nil && healthy {
		devices = append(devices, resourceapi.Device{
			Name: device.Name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				qualified(driverName, "backend"):        stringAttribute("venus"),
				qualified(driverName, "guestAPI"):       stringAttribute("vulkan"),
				qualified(driverName, "virtualization"): stringAttribute("krunkit"),
				qualified(driverName, "architecture"):   stringAttribute(runtime.GOARCH),
				qualified(driverName, "drmDriver"):      stringAttribute(device.SysfsDriver),
				qualified(driverName, "computeHealthy"): boolAttribute(true),
			},
		})
	}
	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {
				Slices: []resourceslice.Slice{{Devices: devices}},
			},
		},
	}
}

func qualified(driverName, name string) resourceapi.QualifiedName {
	return resourceapi.QualifiedName(driverName + "/" + name)
}

func stringAttribute(value string) resourceapi.DeviceAttribute {
	return resourceapi.DeviceAttribute{StringValue: &value}
}

func boolAttribute(value bool) resourceapi.DeviceAttribute {
	return resourceapi.DeviceAttribute{BoolValue: &value}
}
