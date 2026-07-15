package kubeletbridge

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLogsHandlerServesOnlyCurrentProjectedContainer(t *testing.T) {
	buffer := NewLogBuffer(1024)
	now := time.Unix(1_800_000_000, 0).UTC()
	if err := buffer.Reset("assignment-one", now, "process started"); err != nil {
		t.Fatal(err)
	}
	server := &Server{config: ServerConfig{
		Logs: buffer,
		ResolveTarget: func() (Target, bool) {
			return Target{AssignmentUID: "assignment-one", Namespace: "tenant", PodName: "idleloom-one", ContainerName: "native-metal"}, true
		},
	}}
	request := httptest.NewRequest(http.MethodGet, "https://example.test/containerLogs/tenant/idleloom-one/native-metal?timestamps=true", nil)
	request.TLS.PeerCertificates = []*x509.Certificate{{Subject: pkix.Name{CommonName: "kube-apiserver-kubelet-client"}}}
	response := httptest.NewRecorder()
	server.authorize(server.handleLogs)(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "process started") || !strings.Contains(response.Body.String(), now.Format(time.RFC3339)) {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "https://example.test/containerLogs/other/idleloom-one/native-metal", nil)
	request.TLS.PeerCertificates = []*x509.Certificate{{Subject: pkix.Name{CommonName: "kube-apiserver-kubelet-client"}}}
	response = httptest.NewRecorder()
	server.authorize(server.handleLogs)(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("wrong namespace status = %d", response.Code)
	}
}

func TestLogsHandlerRejectsUntrustedClient(t *testing.T) {
	server := &Server{config: ServerConfig{Logs: NewLogBuffer(1024), ResolveTarget: func() (Target, bool) { return Target{}, false }}}
	request := httptest.NewRequest(http.MethodGet, "https://example.test/healthz", nil)
	request.TLS.PeerCertificates = []*x509.Certificate{{Subject: pkix.Name{CommonName: "system:node:other"}}}
	response := httptest.NewRecorder()
	server.authorize(server.handleHealth)(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestAuthorizeAcceptsConfiguredKubeletClientSubject(t *testing.T) {
	server := &Server{config: ServerConfig{
		Logs: NewLogBuffer(1024), ResolveTarget: func() (Target, bool) { return Target{}, false },
		AllowedClientCommonNames: []string{"control-plane-log-proxy"},
	}}
	request := httptest.NewRequest(http.MethodGet, "https://example.test/healthz", nil)
	request.TLS.PeerCertificates = []*x509.Certificate{{Subject: pkix.Name{CommonName: "control-plane-log-proxy"}}}
	response := httptest.NewRecorder()
	server.authorize(server.handleHealth)(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestUnsupportedStreamingEndpointsReturnExplicitError(t *testing.T) {
	server := &Server{config: ServerConfig{}}
	for _, path := range []string{
		"/exec/default/projected/native-metal",
		"/attach/default/projected/native-metal",
		"/portForward/default/projected",
	} {
		request := httptest.NewRequest(http.MethodPost, "https://example.test"+path, nil)
		response := httptest.NewRecorder()
		server.handleUnsupportedStreaming(response, request)
		if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "not supported") {
			t.Fatalf("%s response = %d %q", path, response.Code, response.Body.String())
		}
	}
}

func TestParseLogOptionsRejectsSinceSecondsDurationOverflow(t *testing.T) {
	values := make(url.Values)
	values["sinceSeconds"] = []string{fmt.Sprintf("%d", int64(^uint64(0)>>1)/int64(time.Second)+1)}
	if _, err := parseLogOptions(values, time.Now()); err == nil || !strings.Contains(err.Error(), "invalid sinceSeconds") {
		t.Fatalf("overflow error = %v", err)
	}
}
