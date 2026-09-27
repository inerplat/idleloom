//go:build linux

package idleloom

import (
	"io"
	"runtime"
)

// defaultRuntime enrols the Linux host itself. There is nothing to virtualise:
// the host already runs the kernel kubelet needs, so Idleloom installs the
// kubelet alongside the host's own services.
func defaultRuntime(runner ExecRunner, out, errOut io.Writer, _ WorkerOptions) WorkerRuntime {
	return InPlaceRuntime{
		Exec: newLinuxExec(runner),
		Kind: RuntimeLinux,
		Arch: runtime.GOARCH,
		Out:  out,
		Err:  errOut,
	}
}
