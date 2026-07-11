package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

type Device struct {
	Name        string
	Path        string
	SysfsDriver string
}

type Discoverer struct {
	DRIDir         string
	SysfsRoot      string
	ExpectedDriver string
}

func (d Discoverer) Discover() ([]Device, error) {
	entries, err := os.ReadDir(d.DRIDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read DRI directory: %w", err)
	}

	var devices []Device
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "renderD") {
			continue
		}

		name := strings.ToLower(entry.Name())
		if problems := validation.IsDNS1123Label(name); len(problems) > 0 {
			return nil, fmt.Errorf("render node %q cannot be converted to a DRA device name: %s", entry.Name(), strings.Join(problems, ", "))
		}

		driver, err := d.readDriver(entry.Name())
		if err != nil {
			return nil, err
		}
		if d.ExpectedDriver != "" && driver != d.ExpectedDriver {
			continue
		}

		devices = append(devices, Device{
			Name:        name,
			Path:        filepath.Join(d.DRIDir, entry.Name()),
			SysfsDriver: driver,
		})
	}

	sort.Slice(devices, func(i, j int) bool { return devices[i].Path < devices[j].Path })
	return devices, nil
}

func (d Discoverer) readDriver(renderNode string) (string, error) {
	devicePath := filepath.Join(d.SysfsRoot, "class", "drm", renderNode, "device")
	entries, err := os.ReadDir(devicePath)
	if err != nil {
		return "", fmt.Errorf("read sysfs device for %s: %w", renderNode, err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "virtio") {
			continue
		}
		driver, err := resolveDriver(filepath.Join(devicePath, entry.Name(), "driver"))
		if err == nil {
			return driver, nil
		}
	}

	return resolveDriver(filepath.Join(devicePath, "driver"))
}

func resolveDriver(link string) (string, error) {
	target, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", fmt.Errorf("resolve sysfs driver %s: %w", link, err)
	}
	return filepath.Base(target), nil
}
