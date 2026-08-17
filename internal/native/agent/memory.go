package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
)

// ErrInsufficientMemoryAtStart marks a start refused because the host lost the
// memory it was scheduled with. The refusal is not terminal; the next
// reconcile retries once memory frees up.
var ErrInsufficientMemoryAtStart = errors.New("insufficient available memory to start the model process")

// preStartMarginBytes is the hysteresis between the scheduling comparison and
// the pre-start re-check, so the two cannot flap against each other.
const preStartMarginBytes = int64(512 << 20)

const (
	// memoryWindowSize is how many recent measurements feed the published
	// figure. Publishing the window minimum keeps best-fit scheduling from
	// flapping on short-lived dips.
	memoryWindowSize = 3
	// memoryStaleTolerance is how long a failed measurement may coast on the
	// last good window before the agent stops pretending to know.
	memoryStaleTolerance = 30 * time.Second
)

// memoryTracker turns per-heartbeat memory measurements into the available
// figure the host publishes. It exists because a single sample is too noisy to
// schedule against and a transient measurement failure must not swing the
// advertised capacity.
type memoryTracker struct {
	window   []int64
	lastGood time.Time
}

// observe folds one measurement attempt into the tracker and returns the
// available bytes to publish. degraded is true when no recent measurement
// exists, in which case the returned figure is a deliberately modest
// allocatable/2 so the host stays eligible for small work while being honest
// about not knowing.
func (t *memoryTracker) observe(now time.Time, allocatableBytes, measuredBytes int64, measurementErr error) (int64, bool) {
	if measurementErr == nil {
		if measuredBytes > allocatableBytes {
			measuredBytes = allocatableBytes
		}
		if measuredBytes < 0 {
			measuredBytes = 0
		}
		t.window = append(t.window, measuredBytes)
		if len(t.window) > memoryWindowSize {
			t.window = t.window[len(t.window)-memoryWindowSize:]
		}
		t.lastGood = now
	} else if len(t.window) == 0 || now.Sub(t.lastGood) > memoryStaleTolerance {
		t.window = nil
		return allocatableBytes / 2, true
	}
	published := t.window[0]
	for _, sample := range t.window[1:] {
		if sample < published {
			published = sample
		}
	}
	return published, false
}

// preStartMemoryGate re-checks host memory immediately before a model process
// starts. Scheduling compared against a heartbeat-aged figure and anything on
// the host may have claimed memory since; refusing here is cheaper than
// loading gigabytes of weights into a host that started swapping. The check
// only narrows the race. The runtime's own placement verification after start
// remains the final arbiter, which is also why a failed measurement waves the
// start through.
func (a *DevAgent) preStartMemoryGate(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment) error {
	if assignment.Spec.Model == nil || a.config.Platform == nil {
		return nil
	}
	snapshot, err := a.config.Platform.MemorySnapshot(ctx)
	if err != nil {
		return nil
	}
	required := assignment.Spec.Model.UnifiedMemoryRequest.Value() - preStartMarginBytes
	if required <= 0 {
		return nil
	}
	if snapshot.Available.Value() < required {
		return fmt.Errorf("%w: %s available for a %s request",
			ErrInsufficientMemoryAtStart, snapshot.Available.String(), assignment.Spec.Model.UnifiedMemoryRequest.String())
	}
	return nil
}
