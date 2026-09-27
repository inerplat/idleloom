//go:build windows

package idleloom

import (
	"context"
	"io"
	"runtime"
)

// defaultRuntime enrols a WSL2 distribution. WSL2 is itself a Hyper-V virtual
// machine, but Windows owns its lifecycle, so Idleloom treats it as an
// environment that already exists rather than one to provision.
func defaultRuntime(runner ExecRunner, out, errOut io.Writer, opts WorkerOptions) WorkerRuntime {
	distribution := opts.Distribution
	if distribution == "" {
		// Bind to whichever distribution is default now, by name, rather than
		// following the default wherever it moves to later. An empty value
		// here would leave the resolution to preflight, which reports it.
		distribution = resolveWSLDistribution(context.Background(), runner)
	}
	return InPlaceRuntime{
		Exec: wslExec{Runner: runner, Distribution: distribution},
		Kind: RuntimeWSL2,
		Arch: runtime.GOARCH,
		Out:  out,
		Err:  errOut,
	}
}
