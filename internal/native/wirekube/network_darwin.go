//go:build darwin

package wirekube

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

type commandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type darwinNetwork struct {
	runner  commandRunner
	mu      sync.Mutex
	iface   string
	address string
	routes  []string
}

func newDarwinNetwork(runner commandRunner) *darwinNetwork {
	return &darwinNetwork{runner: runner}
}

func (n *darwinNetwork) Preflight(ctx context.Context, meshCIDR, assignedAddress string) error {
	assignedIP, _, err := net.ParseCIDR(assignedAddress)
	if err != nil {
		return err
	}
	output, err := n.runner.Run(ctx, "/sbin/route", "-n", "get", "-inet", assignedIP.String())
	if err != nil {
		return commandError("inspect existing route", output, err)
	}
	destination := routeGetDestination(string(output))
	if destination == "" {
		return fmt.Errorf("inspect existing route for %s: route output has no destination", assignedIP)
	}
	if destination != "default" {
		return fmt.Errorf("WireKube mesh address %s already uses a more-specific route (%s)", assignedIP, destination)
	}

	output, err = n.runner.Run(ctx, "/usr/sbin/netstat", "-rn", "-f", "inet")
	if err != nil {
		return commandError("inspect IPv4 route table", output, err)
	}
	_, mesh, _ := net.ParseCIDR(meshCIDR)
	for _, route := range darwinRouteNetworks(string(output)) {
		if cidrsOverlap(mesh, route) {
			return fmt.Errorf("WireKube mesh CIDR %s overlaps existing route %s", meshCIDR, route)
		}
	}
	return nil
}

func (n *darwinNetwork) Configure(ctx context.Context, interfaceName, address string, routes []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.iface != "" {
		return fmt.Errorf("Darwin network is already configured")
	}
	ip, _, err := net.ParseCIDR(address)
	if err != nil {
		return err
	}
	if err := n.run(ctx, "/sbin/ifconfig", interfaceName, "inet", address, ip.String(), "alias"); err != nil {
		return fmt.Errorf("assign %s to %s: %w", address, interfaceName, err)
	}
	if err := n.run(ctx, "/sbin/ifconfig", interfaceName, "up"); err != nil {
		return fmt.Errorf("bring up %s: %w", interfaceName, err)
	}
	n.iface = interfaceName
	n.address = address
	for _, route := range routes {
		if err := n.run(ctx, "/sbin/route", "-q", "-n", "add", "-inet", route, "-interface", interfaceName); err != nil {
			cleanupErr := n.cleanupLocked(context.Background())
			return errors.Join(fmt.Errorf("add route %s through %s: %w", route, interfaceName, err), cleanupErr)
		}
		n.routes = append(n.routes, route)
	}
	return nil
}

func (n *darwinNetwork) Validate(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.iface == "" || n.address == "" || len(n.routes) == 0 {
		return fmt.Errorf("Darwin network is not configured")
	}
	assignedIP, _, err := net.ParseCIDR(n.address)
	if err != nil {
		return err
	}
	output, err := n.runner.Run(ctx, "/sbin/ifconfig", n.iface, "inet")
	if err != nil {
		return commandError("validate WireKube interface", output, err)
	}
	if !strings.Contains(string(output), assignedIP.String()) {
		return fmt.Errorf("WireKube interface %s no longer has address %s", n.iface, assignedIP)
	}
	output, err = n.runner.Run(ctx, "/sbin/route", "-n", "get", "-inet", assignedIP.String())
	if err != nil {
		return commandError("validate WireKube route", output, err)
	}
	if routeGetInterface(string(output)) != n.iface {
		return fmt.Errorf("WireKube route for %s is no longer owned by %s", assignedIP, n.iface)
	}
	return nil
}

func (n *darwinNetwork) Cleanup(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cleanupLocked(ctx)
}

func (n *darwinNetwork) cleanupLocked(ctx context.Context) error {
	var values []error
	var failed []string
	for index := len(n.routes) - 1; index >= 0; index-- {
		route := n.routes[index]
		if err := n.run(ctx, "/sbin/route", "-q", "-n", "delete", "-inet", route, "-interface", n.iface); err != nil {
			values = append(values, fmt.Errorf("delete route %s: %w", route, err))
			failed = append([]string{route}, failed...)
		}
	}
	n.routes = failed
	if len(failed) == 0 {
		n.iface = ""
		n.address = ""
	}
	return errors.Join(values...)
}

func (n *darwinNetwork) run(ctx context.Context, command string, args ...string) error {
	output, err := n.runner.Run(ctx, command, args...)
	if err == nil {
		return nil
	}
	return commandError(command, output, err)
}

func commandError(action string, output []byte, err error) error {
	message := strings.TrimSpace(string(output))
	if message == "" {
		return err
	}
	return fmt.Errorf("%s: %w: %s", action, err, message)
}

func routeGetDestination(output string) string {
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && key == "destination" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func routeGetInterface(output string) string {
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && key == "interface" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func darwinRouteNetworks(output string) []*net.IPNet {
	var networks []*net.IPNet
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] == "Destination" || fields[0] == "Routing" || fields[0] == "Internet:" || fields[0] == "default" {
			continue
		}
		if network := parseDarwinRoute(fields[0]); network != nil {
			networks = append(networks, network)
		}
	}
	return networks
}

func parseDarwinRoute(value string) *net.IPNet {
	address, prefixValue, hasPrefix := strings.Cut(value, "/")
	parts := strings.Split(address, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return nil
	}
	for len(parts) < 4 {
		parts = append(parts, "0")
	}
	address = strings.Join(parts, ".")
	prefix := len(strings.Split(strings.Split(value, "/")[0], ".")) * 8
	if hasPrefix {
		parsed, err := strconv.Atoi(prefixValue)
		if err != nil || parsed < 0 || parsed > 32 {
			return nil
		}
		prefix = parsed
	}
	_, network, err := net.ParseCIDR(fmt.Sprintf("%s/%d", address, prefix))
	if err != nil {
		return nil
	}
	return network
}
