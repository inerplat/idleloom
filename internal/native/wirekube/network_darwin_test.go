//go:build darwin

package wirekube

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
)

type recordedCommand struct {
	name string
	args []string
}

func TestResolveRelayEndpointProducesNumericWireGuardEndpoint(t *testing.T) {
	state := testTunnelState(t)
	state.RelayEndpoint = "localhost:3478"
	resolved, err := resolveRelayEndpoint(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(resolved.RelayEndpoint)
	if err != nil || net.ParseIP(host) == nil || port != "3478" {
		t.Fatalf("resolved endpoint = %q", resolved.RelayEndpoint)
	}
}

type recordingRunner struct {
	commands []recordedCommand
	failAt   int
	outputs  [][]byte
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.commands = append(r.commands, recordedCommand{name: name, args: append([]string(nil), args...)})
	if r.failAt > 0 && len(r.commands) == r.failAt {
		return []byte("injected failure"), errors.New("run failed")
	}
	if len(r.outputs) >= len(r.commands) {
		return r.outputs[len(r.commands)-1], nil
	}
	return nil, nil
}

func TestDarwinNetworkPreflightAcceptsOnlyDefaultFallback(t *testing.T) {
	runner := &recordingRunner{outputs: [][]byte{
		[]byte("route to: 172.31.241.10\ndestination: default\ngateway: 192.0.2.1\n"),
		[]byte("Routing tables\nInternet:\nDestination Gateway Flags Netif Expire\ndefault 192.0.2.1 UGScg en0\n192.0.2/24 link#4 UCS en0\n"),
	}}
	network := newDarwinNetwork(runner)
	if err := network.Preflight(context.Background(), "172.31.240.0/20", "172.31.241.10/32"); err != nil {
		t.Fatal(err)
	}
}

func TestDarwinNetworkPreflightRejectsMoreSpecificRoute(t *testing.T) {
	runner := &recordingRunner{outputs: [][]byte{
		[]byte("route to: 172.31.241.10\ndestination: 172.31.240.0\nmask: 255.255.240.0\n"),
	}}
	network := newDarwinNetwork(runner)
	if err := network.Preflight(context.Background(), "172.31.240.0/20", "172.31.241.10/32"); err == nil {
		t.Fatal("Preflight accepted an existing more-specific route")
	}
}

func TestDarwinNetworkPreflightRejectsOverlappingRouteTableEntry(t *testing.T) {
	runner := &recordingRunner{outputs: [][]byte{
		[]byte("destination: default\n"),
		[]byte("Destination Gateway Flags Netif Expire\ndefault 192.0.2.1 UGScg en0\n172.31.240/21 link#20 UCS utun7\n"),
	}}
	network := newDarwinNetwork(runner)
	if err := network.Preflight(context.Background(), "172.31.240.0/20", "172.31.241.10/32"); err == nil {
		t.Fatal("Preflight accepted an overlapping VPN route")
	}
}

func TestDarwinNetworkConfiguresAndCleansOnlyOwnedRoute(t *testing.T) {
	runner := &recordingRunner{}
	network := newDarwinNetwork(runner)
	if err := network.Configure(context.Background(), "utun9", "172.31.241.10/32", []string{"172.31.240.0/20"}); err != nil {
		t.Fatal(err)
	}
	if err := network.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []recordedCommand{
		{name: "/sbin/ifconfig", args: []string{"utun9", "inet", "172.31.241.10/32", "172.31.241.10", "alias"}},
		{name: "/sbin/ifconfig", args: []string{"utun9", "up"}},
		{name: "/sbin/route", args: []string{"-q", "-n", "add", "-inet", "172.31.240.0/20", "-interface", "utun9"}},
		{name: "/sbin/route", args: []string{"-q", "-n", "delete", "-inet", "172.31.240.0/20", "-interface", "utun9"}},
	}
	if !reflect.DeepEqual(runner.commands, want) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, want)
	}
}

func TestDarwinNetworkDoesNotDeleteRouteThatFailedToAdd(t *testing.T) {
	runner := &recordingRunner{failAt: 3}
	network := newDarwinNetwork(runner)
	if err := network.Configure(context.Background(), "utun9", "172.31.241.10/32", []string{"172.31.240.0/20"}); err == nil {
		t.Fatal("Configure succeeded after route failure")
	}
	if len(runner.commands) != 3 {
		t.Fatalf("unexpected cleanup command after failed route add: %#v", runner.commands)
	}
}

func TestDarwinNetworkRetriesOwnedRouteCleanup(t *testing.T) {
	runner := &recordingRunner{failAt: 4}
	network := newDarwinNetwork(runner)
	if err := network.Configure(context.Background(), "utun9", "172.31.241.10/32", []string{"172.31.240.0/20"}); err != nil {
		t.Fatal(err)
	}
	if err := network.Cleanup(context.Background()); err == nil {
		t.Fatal("first cleanup unexpectedly succeeded")
	}
	if len(network.routes) != 1 {
		t.Fatal("failed route deletion lost ownership state")
	}
	runner.failAt = 0
	if err := network.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(network.routes) != 0 {
		t.Fatal("successful retry retained route ownership state")
	}
}

func TestDarwinNetworkValidateDetectsRouteReplacement(t *testing.T) {
	runner := &recordingRunner{outputs: [][]byte{
		nil, nil, nil,
		[]byte("inet 172.31.241.10 --> 172.31.241.10 netmask 0xffffffff\n"),
		[]byte("destination: 172.31.240.0\ninterface: utun7\n"),
	}}
	network := newDarwinNetwork(runner)
	if err := network.Configure(context.Background(), "utun9", "172.31.241.10/32", []string{"172.31.240.0/20"}); err != nil {
		t.Fatal(err)
	}
	if err := network.Validate(context.Background()); err == nil {
		t.Fatal("Validate accepted a route moved to another interface")
	}
}
