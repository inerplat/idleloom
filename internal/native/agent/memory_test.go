package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestParseVMStat(t *testing.T) {
	output := []byte(`Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                   110082.
Pages active:                                 651125.
Pages inactive:                               632717.
Pages speculative:                             19640.
Pages throttled:                                   0.
Pages wired down:                             119685.
Pages purgeable:                               19977.
"Translation faults":                       16468471.
`)
	pageSize, pages, err := parseVMStat(output)
	if err != nil {
		t.Fatalf("parseVMStat: %v", err)
	}
	if pageSize != 16384 {
		t.Fatalf("page size = %d, want 16384", pageSize)
	}
	for key, want := range map[string]int64{
		"Pages free": 110082, "Pages inactive": 632717,
		"Pages purgeable": 19977, "Pages speculative": 19640,
	} {
		if pages[key] != want {
			t.Fatalf("%s = %d, want %d", key, pages[key], want)
		}
	}
}

func TestParseVMStatRejectsMalformedOutput(t *testing.T) {
	for name, output := range map[string]string{
		"empty":         "",
		"no page size":  "Mach Virtual Memory Statistics:\nPages free: 5.\n",
		"no free pages": "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages active: 5.\n",
	} {
		if _, _, err := parseVMStat([]byte(output)); err == nil {
			t.Fatalf("%s vm_stat output was accepted", name)
		}
	}
}

func TestParseSwapUsage(t *testing.T) {
	for output, want := range map[string]int64{
		"total = 2048.00M  used = 1198.75M  free = 849.25M  (encrypted)": int64(1198.75 * float64(1<<20)),
		"total = 0.00M  used = 0.00M  free = 0.00M  (encrypted)":         0,
		"total = 24.00G  used = 15.50G  free = 8.50G  (encrypted)":       int64(15.5 * float64(1<<30)),
	} {
		got, err := parseSwapUsage(output)
		if err != nil {
			t.Fatalf("parseSwapUsage(%q): %v", output, err)
		}
		if got != want {
			t.Fatalf("parseSwapUsage(%q) = %d, want %d", output, got, want)
		}
	}
	if _, err := parseSwapUsage("total = 0.00M  free = 0.00M"); err == nil {
		t.Fatal("swapusage output without a used figure was accepted")
	}
}

func TestAvailableBytesClampsUnderPressure(t *testing.T) {
	pages := map[string]int64{
		"Pages free": 1000, "Pages inactive": 2000,
		"Pages purgeable": 500, "Pages speculative": 250,
	}
	const pageSize = int64(16384)
	normal := availableBytes(pageSize, pages, memoryPressureNormal, 0)
	if normal != 3750*pageSize {
		t.Fatalf("normal available = %d, want all reclaimable pages", normal)
	}
	warning := availableBytes(pageSize, pages, memoryPressureWarning, 0)
	if warning != 1500*pageSize {
		t.Fatalf("warning available = %d, want free plus purgeable only", warning)
	}
	if critical := availableBytes(pageSize, pages, memoryPressureCritical, 0); critical != 0 {
		t.Fatalf("critical available = %d, want 0", critical)
	}
	swapped := availableBytes(pageSize, pages, memoryPressureNormal, swapUsageGraceBytes+(1<<20))
	if swapped != 3750*pageSize-(1<<20) {
		t.Fatalf("swap penalty not applied: %d", swapped)
	}
	if floor := availableBytes(pageSize, pages, memoryPressureNormal, 1<<40); floor != 0 {
		t.Fatalf("heavy swap should floor available at 0, got %d", floor)
	}
}

func TestMemoryTrackerPublishesWindowMinimum(t *testing.T) {
	tracker := memoryTracker{}
	now := time.Unix(1_800_000_000, 0)
	const allocatable = int64(16 << 30)
	for i, sample := range []int64{10 << 30, 8 << 30, 12 << 30} {
		published, degraded := tracker.observe(now.Add(time.Duration(i)*2*time.Second), allocatable, sample, nil)
		if degraded {
			t.Fatalf("sample %d unexpectedly degraded", i)
		}
		if i == 2 && published != 8<<30 {
			t.Fatalf("published = %d, want the window minimum", published)
		}
	}
	// A fourth sample evicts the oldest; the minimum moves with the window.
	published, _ := tracker.observe(now.Add(6*time.Second), allocatable, 11<<30, nil)
	if published != 8<<30 {
		t.Fatalf("published = %d, want 8GiB still inside the window", published)
	}
	published, _ = tracker.observe(now.Add(8*time.Second), allocatable, 11<<30, nil)
	if published != 11<<30 {
		t.Fatalf("published = %d, want 11GiB after the 8GiB sample aged out", published)
	}
}

func TestMemoryTrackerClampsAndCoasts(t *testing.T) {
	tracker := memoryTracker{}
	now := time.Unix(1_800_000_000, 0)
	const allocatable = int64(16 << 30)
	published, degraded := tracker.observe(now, allocatable, 20<<30, nil)
	if degraded || published != allocatable {
		t.Fatalf("measured above allocatable published %d, want clamp to allocatable", published)
	}
	failure := fmt.Errorf("vm_stat unavailable")
	published, degraded = tracker.observe(now.Add(10*time.Second), allocatable, 0, failure)
	if degraded || published != allocatable {
		t.Fatalf("transient failure should coast on the window, got %d degraded=%v", published, degraded)
	}
	published, degraded = tracker.observe(now.Add(memoryStaleTolerance+time.Second), allocatable, 0, failure)
	if !degraded || published != allocatable/2 {
		t.Fatalf("stale measurement should degrade to allocatable/2, got %d degraded=%v", published, degraded)
	}
}

func TestMemoryTrackerDegradesWithoutAnySample(t *testing.T) {
	tracker := memoryTracker{}
	published, degraded := tracker.observe(time.Unix(1_800_000_000, 0), 16<<30, 0, fmt.Errorf("boom"))
	if !degraded || published != 8<<30 {
		t.Fatalf("first failed sample should degrade immediately, got %d degraded=%v", published, degraded)
	}
}

func TestPreStartMemoryGate(t *testing.T) {
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			Model: &nativev1alpha1.ResolvedModel{UnifiedMemoryRequest: resource.MustParse("14Gi")},
		},
	}
	starved := &DevAgent{config: DevAgentConfig{Platform: fakeAgentPlatform{availableBytes: 6 << 30}}}
	if err := starved.preStartMemoryGate(context.Background(), assignment); !errors.Is(err, ErrInsufficientMemoryAtStart) {
		t.Fatalf("starved host accepted the start: %v", err)
	}
	healthy := &DevAgent{config: DevAgentConfig{Platform: fakeAgentPlatform{availableBytes: 15 << 30}}}
	if err := healthy.preStartMemoryGate(context.Background(), assignment); err != nil {
		t.Fatalf("healthy host refused the start: %v", err)
	}
	// A measurement failure waves the start through; the runtime's own
	// placement verification is the final arbiter.
	blind := &DevAgent{config: DevAgentConfig{Platform: fakeAgentPlatform{snapshotErr: fmt.Errorf("boom")}}}
	if err := blind.preStartMemoryGate(context.Background(), assignment); err != nil {
		t.Fatalf("failed measurement refused the start: %v", err)
	}
	shell := &DevAgent{config: DevAgentConfig{Platform: fakeAgentPlatform{availableBytes: 1 << 20}}}
	if err := shell.preStartMemoryGate(context.Background(), &nativev1alpha1.IdleloomWorkloadAssignment{}); err != nil {
		t.Fatalf("non-model assignment was gated: %v", err)
	}
}
