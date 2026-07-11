package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverVirtioRenderNode(t *testing.T) {
	root := t.TempDir()
	driDir := filepath.Join(root, "dev", "dri")
	driverDir := filepath.Join(root, "sys", "bus", "virtio", "drivers", "virtio_gpu")
	deviceDir := filepath.Join(root, "sys", "class", "drm", "renderD128", "device")
	virtioDir := filepath.Join(deviceDir, "virtio3")

	for _, dir := range []string{driDir, driverDir, virtioDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(driDir, "renderD128"), nil, 0o660); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(driverDir, filepath.Join(virtioDir, "driver")); err != nil {
		t.Fatal(err)
	}

	devices, err := (Discoverer{
		DRIDir:         driDir,
		SysfsRoot:      filepath.Join(root, "sys"),
		ExpectedDriver: "virtio_gpu",
	}).Discover()
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("len(devices) = %d, want 1", len(devices))
	}
	if devices[0].Name != "renderd128" {
		t.Fatalf("device name = %q, want renderd128", devices[0].Name)
	}
}

func TestDiscoverSkipsUnexpectedDriver(t *testing.T) {
	root := t.TempDir()
	driDir := filepath.Join(root, "dev", "dri")
	driverDir := filepath.Join(root, "sys", "drivers", "amdgpu")
	deviceDir := filepath.Join(root, "sys", "class", "drm", "renderD128", "device")
	for _, dir := range []string{driDir, driverDir, deviceDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(driDir, "renderD128"), nil, 0o660); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(driverDir, filepath.Join(deviceDir, "driver")); err != nil {
		t.Fatal(err)
	}

	devices, err := (Discoverer{DRIDir: driDir, SysfsRoot: filepath.Join(root, "sys"), ExpectedDriver: "virtio_gpu"}).Discover()
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if len(devices) != 0 {
		t.Fatalf("len(devices) = %d, want 0", len(devices))
	}
}
