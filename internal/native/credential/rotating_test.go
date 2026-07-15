package credential

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestConfigureRefreshesRequestAndPersistsToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	oldToken := testJWT(t, now.Add(5*time.Minute))
	newExpiry := now.Add(8 * time.Hour)
	newToken := testJWT(t, newExpiry)
	var refreshes atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/namespaces/host/serviceaccounts/agent/token":
			refreshes.Add(1)
			if got := request.Header.Get("Authorization"); got != "Bearer "+oldToken {
				t.Errorf("refresh authorization = %q", got)
			}
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{
				Token: newToken, ExpirationTimestamp: metav1.NewTime(newExpiry),
			}})
		case "/probe":
			if got := request.Header.Get("Authorization"); got != "Bearer "+newToken {
				t.Errorf("probe authorization = %q", got)
			}
			response.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	kubeconfigPath := filepath.Join(t.TempDir(), "agent.kubeconfig")
	writeTestKubeconfig(t, kubeconfigPath, server.URL, oldToken)
	config, err := Configure(&rest.Config{Host: server.URL, BearerToken: oldToken}, Options{
		Namespace: "host", ServiceAccount: "agent", KubeconfigPath: kubeconfigPath, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := rest.TransportFor(config)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1", refreshes.Load())
	}
	persisted, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.AuthInfos["agent"].Token; got != newToken {
		t.Fatalf("persisted token = %q, want refreshed token", got)
	}
	if info, err := os.Stat(kubeconfigPath); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("kubeconfig permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestConfigureDoesNotSendExpiredTokenWhenRefreshFails(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	oldToken := testJWT(t, now.Add(-time.Minute))
	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/probe" {
			probes.Add(1)
		}
		http.Error(response, "denied", http.StatusForbidden)
	}))
	defer server.Close()

	config, err := Configure(&rest.Config{Host: server.URL, BearerToken: oldToken}, Options{
		Namespace: "host", ServiceAccount: "agent", Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := rest.TransportFor(config)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("request succeeded after token refresh failure")
	}
	if probes.Load() != 0 {
		t.Fatalf("protected endpoint received %d requests with an expired token", probes.Load())
	}
}

func TestTokenExpiryRejectsMalformedTokens(t *testing.T) {
	for _, token := range []string{"opaque", "a.!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".c"} {
		if _, err := tokenExpiry(token); err == nil {
			t.Fatalf("tokenExpiry(%q) succeeded", token)
		}
	}
}

func testJWT(t *testing.T, expiresAt time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]int64{"exp": expiresAt.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("header.%s.signature", base64.RawURLEncoding.EncodeToString(payload))
}

func writeTestKubeconfig(t *testing.T, path, server, token string) {
	t.Helper()
	config := clientcmdapi.Config{
		CurrentContext: "test",
		Clusters:       map[string]*clientcmdapi.Cluster{"cluster": {Server: server}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{"agent": {Token: token}},
		Contexts:       map[string]*clientcmdapi.Context{"test": {Cluster: "cluster", AuthInfo: "agent"}},
	}
	data, err := clientcmd.Write(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
