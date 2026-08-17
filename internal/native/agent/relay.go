package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/devruntime"
)

// endpointProcess is a runtime process that answers its own HTTP API on a
// loopback address.
type endpointProcess interface {
	Endpoint() string
}

// servingRelay publishes a runtime's HTTP API on the host's mesh address by
// copying bytes, never by interpreting them. The runtime keeps its loopback
// binding for two reasons: a host cannot open a TCP connection to its own
// WireKube address, so the supervisor could not health check a runtime bound
// there, and loopback keeps the model server off every other interface.
// Copying at the transport layer leaves streaming, sampling parameters,
// response fields, and future API additions untouched.
type servingRelay struct {
	process  Process
	listener net.Listener
	upstream string
	done     chan struct{}
	mu       sync.Mutex
	waitErr  error
	stopped  bool
	conns    sync.WaitGroup
}

func startServingRelay(process Process, listenAddress string, listen func(string, string) (net.Listener, error)) (Process, error) {
	endpoint, ok := process.(endpointProcess)
	if !ok {
		_ = process.Stop()
		return nil, fmt.Errorf("the serving runtime does not expose an HTTP endpoint")
	}
	upstream, err := hostPortFromURL(endpoint.Endpoint())
	if err != nil {
		_ = process.Stop()
		return nil, err
	}
	host, port, err := net.SplitHostPort(listenAddress)
	if err != nil || port != fmt.Sprint(nativev1alpha1.NativeServingPort) {
		_ = process.Stop()
		return nil, fmt.Errorf("the Native serving address must use port %d", nativev1alpha1.NativeServingPort)
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.To4() == nil {
		_ = process.Stop()
		return nil, fmt.Errorf("the Native serving address must use the WireKube IPv4 address")
	}
	listener, err := listen("tcp4", listenAddress)
	if err != nil {
		_ = process.Stop()
		return nil, fmt.Errorf("listen for Native serving: %w", err)
	}
	relay := &servingRelay{process: process, listener: listener, upstream: upstream, done: make(chan struct{})}
	go relay.accept()
	return relay, nil
}

func (r *servingRelay) accept() {
	defer close(r.done)
	for {
		client, err := r.listener.Accept()
		if err != nil {
			r.mu.Lock()
			if !r.stopped && !errors.Is(err, net.ErrClosed) {
				r.waitErr = fmt.Errorf("the Native serving endpoint stopped: %w", err)
			}
			r.mu.Unlock()
			return
		}
		r.conns.Add(1)
		go func() {
			defer r.conns.Done()
			r.proxy(client)
		}()
	}
}

func (r *servingRelay) proxy(client net.Conn) {
	defer func() { _ = client.Close() }()
	upstream, err := net.DialTimeout("tcp", r.upstream, 10*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()
	// Half-close each direction as it drains so a client that stops sending
	// still receives the rest of a long streaming response.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, client)
		if half, ok := upstream.(*net.TCPConn); ok {
			_ = half.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		if half, ok := client.(*net.TCPConn); ok {
			_ = half.CloseWrite()
		}
	}()
	wg.Wait()
}

func (r *servingRelay) Alive() bool {
	if r == nil || r.process == nil {
		return false
	}
	select {
	case <-r.done:
		return false
	default:
		return r.process.Alive()
	}
}

func (r *servingRelay) Stop() error {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	closeErr := r.listener.Close()
	<-r.done
	r.conns.Wait()
	return errors.Join(closeErr, r.process.Stop())
}

// Generate is unsupported: a serving relay carries HTTP for clients, while
// batch inference keeps using the runtime's own generate path.
func (r *servingRelay) Generate(context.Context, devruntime.GenerateRequest) (devruntime.GenerateResponse, error) {
	return devruntime.GenerateResponse{}, fmt.Errorf("the Native serving endpoint answers HTTP clients rather than in-process generation")
}

func (r *servingRelay) Stderr() string { return r.process.Stderr() }

func (r *servingRelay) WaitError() error {
	r.mu.Lock()
	relayErr := r.waitErr
	r.mu.Unlock()
	return errors.Join(relayErr, r.process.WaitError())
}

func hostPortFromURL(endpoint string) (string, error) {
	if endpoint == "" {
		return "", fmt.Errorf("the serving runtime reported no endpoint")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("the serving runtime reported an unusable endpoint")
	}
	return parsed.Host, nil
}

func (a *DevAgent) listenServing() func(string, string) (net.Listener, error) {
	if a.config.ListenServing != nil {
		return a.config.ListenServing
	}
	return net.Listen
}
