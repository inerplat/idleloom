package cdi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inerplat/idleloom/internal/discovery"
)

func TestEnsureWritesStableSpec(t *testing.T) {
	dir := t.TempDir()
	writer, err := NewWriter(dir, "gpu.apple-vulkan.example")
	if err != nil {
		t.Fatal(err)
	}
	id, err := writer.Ensure(discovery.Device{Name: "renderd128", Path: "/dev/dri/renderD128"})
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if id != "gpu.apple-vulkan.example/render=renderd128" {
		t.Fatalf("CDI ID = %q", id)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("CDI files = %d, want 1", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "/dev/dri/renderD128") {
		t.Fatalf("CDI spec does not contain render node: %s", data)
	}
}
