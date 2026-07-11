package health

import (
	"context"
	"testing"
	"time"

	"github.com/inerplat/idleloom/internal/discovery"
)

func TestCommandProbeChecksOutput(t *testing.T) {
	result := (CommandProbe{
		Command:  "/bin/sh",
		Args:     []string{"-c", "printf Venus"},
		Contains: "venus",
		Timeout:  time.Second,
	}).Probe(context.Background(), discovery.Device{})
	if !result.Healthy || result.Err != nil {
		t.Fatalf("Probe() = healthy %v, err %v", result.Healthy, result.Err)
	}
}

func TestCommandProbeRejectsMissingMarker(t *testing.T) {
	result := (CommandProbe{
		Command:  "/bin/sh",
		Args:     []string{"-c", "printf llvmpipe"},
		Contains: "venus",
		Timeout:  time.Second,
	}).Probe(context.Background(), discovery.Device{})
	if result.Healthy || result.Err == nil {
		t.Fatalf("Probe() = healthy %v, err %v; want unhealthy", result.Healthy, result.Err)
	}
}
