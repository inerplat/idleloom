//go:build !darwin

package wirekube

import (
	"context"
	"fmt"
)

func startPlatformTunnel(context.Context, State, TunnelConfig, func(string, ...any)) (Tunnel, error) {
	return nil, fmt.Errorf("WireKube connected leaf currently requires macOS")
}
