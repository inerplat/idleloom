package health

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/inerplat/idleloom/internal/discovery"
)

type Result struct {
	Healthy  bool
	Output   string
	Duration time.Duration
	Err      error
}

type Prober interface {
	Probe(context.Context, discovery.Device) Result
}

type CommandProbe struct {
	Command  string
	Args     []string
	Contains string
	Timeout  time.Duration
}

func (p CommandProbe) Probe(ctx context.Context, _ discovery.Device) Result {
	started := time.Now()
	probeCtx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	output, err := exec.CommandContext(probeCtx, p.Command, p.Args...).CombinedOutput()
	result := Result{
		Output:   strings.TrimSpace(string(output)),
		Duration: time.Since(started),
		Err:      err,
	}
	if probeCtx.Err() != nil {
		result.Err = fmt.Errorf("probe timed out after %s: %w", p.Timeout, probeCtx.Err())
		return result
	}
	if err != nil {
		result.Err = fmt.Errorf("run probe: %w", err)
		return result
	}
	if p.Contains != "" && !strings.Contains(strings.ToLower(result.Output), strings.ToLower(p.Contains)) {
		result.Err = fmt.Errorf("probe output does not contain %q", p.Contains)
		return result
	}
	result.Healthy = true
	return result
}
