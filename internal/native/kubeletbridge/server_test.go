package kubeletbridge

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLogsHandlerServesOnlyCurrentProjectedContainer(t *testing.T) {
	buffer := NewLogBuffer(1024)
	now := time.Unix(1_800_000_000, 0).UTC()
	buffer.Reset("assignment-one", now, "process started")
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
