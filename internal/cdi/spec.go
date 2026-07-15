package cdi

import (
	"fmt"
	"os"

	"github.com/inerplat/idleloom/internal/discovery"
	cdilib "tags.cncf.io/container-device-interface/pkg/cdi"
	specs "tags.cncf.io/container-device-interface/specs-go"
)

const specVersion = "0.8.0"

type Writer struct {
	cache      *cdilib.Cache
	driverName string
}

func NewWriter(directory, driverName string) (*Writer, error) {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create CDI directory: %w", err)
	}
	cache, err := cdilib.NewCache(cdilib.WithSpecDirs(directory))
	if err != nil {
		return nil, fmt.Errorf("create CDI cache: %w", err)
	}
	return &Writer{cache: cache, driverName: driverName}, nil
}

func (w *Writer) Ensure(device discovery.Device) (string, error) {
	kind := w.driverName + "/render"
	deviceGroup := uint32(65532)
	deviceMode := os.FileMode(0o660)
	raw := &specs.Spec{
		Version: specVersion,
		Kind:    kind,
		Devices: []specs.Device{
			{
				Name: device.Name,
				ContainerEdits: specs.ContainerEdits{
					AdditionalGIDs: []uint32{deviceGroup},
					DeviceNodes: []*specs.DeviceNode{
						{
							Path:        device.Path,
							HostPath:    device.Path,
							Permissions: "rw",
							GID:         &deviceGroup,
							FileMode:    &deviceMode,
						},
					},
				},
			},
		},
	}
	name, err := cdilib.GenerateNameForSpec(raw)
	if err != nil {
		return "", fmt.Errorf("generate CDI spec name: %w", err)
	}
	if err := w.cache.WriteSpec(raw, name); err != nil {
		return "", fmt.Errorf("write CDI spec: %w", err)
	}
	return kind + "=" + device.Name, nil
}
