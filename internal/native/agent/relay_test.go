package agent

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type relayFakeRuntime struct {
	fakeBatchRunner
	endpoint string
}

func (r *relayFakeRuntime) Endpoint() string { return r.endpoint }

func TestServingRelayCarriesStreamingResponses(t *testing.T) {
	// A chunked response must reach the client incrementally, which is what
	// server-sent events depend on.
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upstream.Close() }()
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		for _, chunk := range []string{"data: one\n\n", "data: [DONE]\n\n"} {
			fmt.Fprintf(conn, "%x\r\n%s\r\n", len(chunk), chunk)
		}
		fmt.Fprint(conn, "0\r\n\r\n")
	}()
	runtime := &relayFakeRuntime{
		fakeBatchRunner: fakeBatchRunner{alive: true, waitForCancellation: true, pid: 1},
		endpoint:        "http://" + upstream.Addr().String(),
	}
	relay, err := startServingRelay(runtime, "198.18.18.104:18080", listenAnywhere)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = relay.Stop() }()
	address := relay.(*servingRelay).listener.Addr().String()
	response, err := http.Get("http://" + address + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("relayed response = %d %q", response.StatusCode, body)
	}
}

func TestServingRelayRejectsUnusableAddresses(t *testing.T) {
	for name, address := range map[string]string{
		"loopback":   "127.0.0.1:18080",
		"wrong port": "198.18.18.104:8080",
		"hostname":   "example.com:18080",
	} {
		runtime := &relayFakeRuntime{
			fakeBatchRunner: fakeBatchRunner{alive: true, pid: 1},
			endpoint:        "http://127.0.0.1:9",
		}
		if _, err := startServingRelay(runtime, address, listenAnywhere); err == nil {
			t.Fatalf("%s address %q was accepted", name, address)
		}
	}
}

func TestServingRelayRequiresAnEndpoint(t *testing.T) {
	runtime := &fakeBatchRunner{alive: true, pid: 1}
	if _, err := startServingRelay(runtime, "198.18.18.104:18080", listenAnywhere); err == nil {
		t.Fatal("a runtime without an HTTP endpoint was accepted for serving")
	}
}

func TestServingRelayStopsWithAnOpenRequest(t *testing.T) {
	// A slow generation keeps its connection open; stopping the workload must
	// still stop the runtime instead of waiting for that request to finish.
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upstream.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()
	runtime := &relayFakeRuntime{
		fakeBatchRunner: fakeBatchRunner{alive: true, pid: 4242},
		endpoint:        "http://" + upstream.Addr().String(),
	}
	relay, err := startServingRelay(runtime, "198.18.18.104:18080", listenAnywhere)
	if err != nil {
		t.Fatal(err)
	}
	if pid := processPID(relay); pid != 4242 {
		t.Fatalf("relay PID = %d, want the runtime PID 4242", pid)
	}
	client, err := net.Dial("tcp", relay.(*servingRelay).listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	fmt.Fprint(client, "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nContent-Length: 2\r\n\r\n{}")
	held := <-accepted
	defer func() { _ = held.Close() }()

	stopped := make(chan error, 1)
	go func() { stopped <- relay.Stop() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay Stop waited for an open request instead of stopping the runtime")
	}
	runtime.mu.Lock()
	calls := runtime.stopCalls
	runtime.mu.Unlock()
	if calls != 1 {
		t.Fatalf("runtime Stop calls = %d, want 1", calls)
	}
}
