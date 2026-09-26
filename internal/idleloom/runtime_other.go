//go:build !darwin && !linux && !windows

package idleloom

import (
	"context"
	"fmt"
	"io"
)

// defaultRuntime refuses on platforms with no worker backend. Returning a
// runtime that fails its preflight keeps the failure on the same path as
// every other unmet prerequisite.
func defaultRuntime(_ ExecRunner, _, _ io.Writer, _ WorkerOptions) WorkerRuntime {
	return unsupportedRuntime{}
}

type unsupportedRuntime struct{ InPlaceRuntime }

func (unsupportedRuntime) Preflight(context.Context) error {
	return fmt.Errorf("Idleloom workers run on macOS, Linux, and Windows hosts")
}
