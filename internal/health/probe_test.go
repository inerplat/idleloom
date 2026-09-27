package health

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/inerplat/idleloom/internal/discovery"
)

// printCommand runs a shell that echoes text, on whichever platform the test
// runs. The probe executes it for real, so it cannot assume /bin/sh.
func printCommand(text string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", "echo " + text}
	}
	return "/bin/sh", []string{"-c", "printf " + text}
}

func TestCommandProbeChecksOutput(t *testing.T) {
	command, args := printCommand("Venus")
	result := (CommandProbe{
		Command:  command,
		Args:     args,
		Contains: "venus",
		Timeout:  time.Second,
	}).Probe(context.Background(), discovery.Device{})
	if !result.Healthy || result.Err != nil {
		t.Fatalf("Probe() = healthy %v, err %v", result.Healthy, result.Err)
	}
}

func TestCommandProbeRejectsMissingMarker(t *testing.T) {
	command, args := printCommand("llvmpipe")
	result := (CommandProbe{
		Command:  command,
		Args:     args,
		Contains: "venus",
		Timeout:  time.Second,
	}).Probe(context.Background(), discovery.Device{})
	if result.Healthy || result.Err == nil {
		t.Fatalf("Probe() = healthy %v, err %v; want unhealthy", result.Healthy, result.Err)
	}
}
