//go:build darwin

package idleloom

import "io"

// defaultRuntime is krunkit on macOS: a real ARM64 Linux VM with Apple GPU
// access through Vulkan.
func defaultRuntime(runner ExecRunner, out, errOut io.Writer, _ WorkerOptions) WorkerRuntime {
	return KrunkitRuntime{Runner: runner, Out: out, Err: errOut}
}
