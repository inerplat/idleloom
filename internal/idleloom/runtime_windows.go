//go:build windows

package idleloom

import (
	"io"
	"runtime"
)

// defaultRuntime enrols a WSL2 distribution. WSL2 is itself a Hyper-V virtual
// machine, but Windows owns its lifecycle, so Idleloom treats it as an
// environment that already exists rather than one to provision.
func defaultRuntime(runner ExecRunner, out, errOut io.Writer, opts WorkerOptions) WorkerRuntime {
	return InPlaceRuntime{
		Exec: wslExec{Runner: runner, Distribution: opts.Distribution},
		Kind: RuntimeWSL2,
		Arch: runtime.GOARCH,
		Out:  out,
		Err:  errOut,
	}
}
