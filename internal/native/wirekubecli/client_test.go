package wirekubecli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestClientPlanAndInstallUseConnectedHostContract(t *testing.T) {
	var calls [][]string
	runner := func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if args[0] == "install" && contains(args, "--dry-run") {
			return []byte(`{"schemaVersion":"v1alpha1","wireKubeVersion":"v0.0.15","relay":"load-balancer","relayUDP":false,"meshCIDR":"100.96.0.0/11","nodeAddresses":"internal-ip","image":"example.test/wirekube@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`), nil, nil
		}
		return []byte(`{"schemaVersion":"v1alpha1","operation":"install","installationID":"install-1","ready":true}`), nil, nil
	}
	client := Client{Binary: "/wirekubectl", Kubeconfig: "/kube/config", Context: "cluster", Run: runner}
	plan, err := client.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Install(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls=%v", calls)
	}
	for _, required := range []string{"--relay-udp=false", "--node-addresses", "internal-ip", "--kubeconfig", "/kube/config", "--context", "cluster"} {
		if !contains(calls[0], required) {
			t.Fatalf("plan args %v do not contain %q", calls[0], required)
		}
	}
	if contains(calls[0], "--relay-udp") || contains(calls[1], "--relay-udp") {
		t.Fatalf("wkpeer install unexpectedly requested a public UDP relay: %v", calls)
	}
	if !containsPair(calls[1], "--mesh-cidr", "100.96.0.0/11") || !contains(calls[1], "--yes") {
		t.Fatalf("install args=%v", calls[1])
	}
}

func TestClientRejectsIncompatiblePlanMetadata(t *testing.T) {
	tests := []struct {
		name string
		plan string
	}{
		{name: "schema", plan: `{"schemaVersion":"v2","wireKubeVersion":"v0.0.15","relay":"load-balancer","relayUDP":false,"meshCIDR":"100.96.0.0/11","nodeAddresses":"internal-ip","image":"example.test/wirekube@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`},
		{name: "version", plan: `{"schemaVersion":"v1alpha1","wireKubeVersion":"v9.9.9","relay":"load-balancer","relayUDP":false,"meshCIDR":"100.96.0.0/11","nodeAddresses":"internal-ip","image":"example.test/wirekube@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`},
		{name: "image digest", plan: `{"schemaVersion":"v1alpha1","wireKubeVersion":"v0.0.15","relay":"load-balancer","relayUDP":false,"meshCIDR":"100.96.0.0/11","nodeAddresses":"internal-ip","image":"example.test/wirekube:latest"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := Client{Binary: "/wirekubectl", Run: func(context.Context, string, ...string) ([]byte, []byte, error) {
				return []byte(test.plan), nil, nil
			}}
			if _, err := client.Plan(context.Background()); err == nil {
				t.Fatal("incompatible plan was accepted")
			}
		})
	}
}

func TestResolverDownloadsAndCachesVerifiedRelease(t *testing.T) {
	binary := []byte("wirekubectl-test-binary")
	digest := sha256.Sum256(binary)
	digestHex := hex.EncodeToString(digest[:])
	asset := "wirekubectl-darwin-arm64"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		switch filepath.Base(request.URL.Path) {
		case "wirekubectl-checksums.txt":
			_, _ = fmt.Fprintf(writer, "%s  %s\n", digestHex, asset)
		case asset:
			_, _ = writer.Write(binary)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	cache := t.TempDir()
	resolver := Resolver{
		Version: "v1.2.3", CacheRoot: cache, ReleaseBaseURL: server.URL,
		GOOS: "darwin", GOARCH: "arm64", HTTPClient: server.Client(),
	}
	originalVerify := verifyVersionHook
	verifyVersionHook = func(context.Context, string, string) error { return nil }
	t.Cleanup(func() { verifyVersionHook = originalVerify })

	first, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != second || requests != 2 {
		t.Fatalf("paths=%q/%q requests=%d", first, second, requests)
	}
	got, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, binary) {
		t.Fatalf("binary=%q", got)
	}
}

func TestChecksumForAssetRejectsMissingOrInvalidEntry(t *testing.T) {
	if _, err := checksumForAsset("bad  wirekubectl-darwin-arm64\n", "wirekubectl-darwin-arm64"); err == nil {
		t.Fatal("invalid checksum was accepted")
	}
	if _, err := checksumForAsset(strings.Repeat("a", 64)+"  other\n", "wirekubectl-darwin-arm64"); err == nil {
		t.Fatal("missing asset was accepted")
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsPair(values []string, first, second string) bool {
	for index := 0; index+1 < len(values); index++ {
		if values[index] == first && values[index+1] == second {
			return true
		}
	}
	return false
}
