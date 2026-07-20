package idleloom

import (
	"archive/tar"
	"io"
	"os"
	"strings"
	"testing"
)

func TestCreateWorkerBundle(t *testing.T) {
	kubelet := t.TempDir() + "/kubelet"
	if err := os.WriteFile(kubelet, []byte("kubelet-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, cleanup, err := CreateWorkerBundle(BundleConfig{
		NodeName:      "mac-mini-idle",
		Taint:         "example.com/dedicated=gpu:NoSchedule",
		Server:        "https://cluster.example.com:6443",
		TLSServerName: "api.internal.example.com",
		CAData:        []byte("test-ca"),
		Token:         "abcdef.0123456789abcdef",
		ClusterDNS:    "10.96.0.10",
		ClusterDomain: "cluster.local",
		KubeletPath:   kubelet,
	})
	if err != nil {
		t.Fatalf("CreateWorkerBundle: %v", err)
	}
	defer cleanup()

	entries := readBundle(t, bundle)
	for _, required := range []string{"bin/kubelet", "bootstrap-kubelet.conf", "config.yaml", "install.sh", "kubelet.service"} {
		if _, ok := entries[required]; !ok {
			t.Errorf("bundle is missing %s", required)
		}
	}
	bootstrap := string(entries["bootstrap-kubelet.conf"])
	if !strings.Contains(bootstrap, "abcdef.0123456789abcdef") || !strings.Contains(bootstrap, "cluster.example.com") {
		t.Fatalf("bootstrap kubeconfig does not contain expected enrollment data:\n%s", bootstrap)
	}
	if !strings.Contains(bootstrap, "tls-server-name: api.internal.example.com") {
		t.Fatalf("bootstrap kubeconfig lost tls-server-name:\n%s", bootstrap)
	}
	install := string(entries["install.sh"])
	if !strings.Contains(install, "--hostname-override=mac-mini-idle") {
		t.Fatalf("install script is missing stable node name:\n%s", install)
	}
	if !strings.Contains(install, "--register-with-taints=example.com/dedicated=gpu:NoSchedule") {
		t.Fatalf("install script is missing dedicated taint:\n%s", install)
	}
	service := string(entries["kubelet.service"])
	if !strings.Contains(service, "--kubeconfig=/var/lib/kubelet/kubeconfig") {
		t.Fatalf("kubelet kubeconfig is not on persistent storage:\n%s", service)
	}
	if !strings.Contains(service, "ExecStart=/var/lib/idleloom/bin/kubelet") {
		t.Fatal("kubelet binary is not stored on the persistent worker disk")
	}
	if !strings.Contains(string(entries["config.yaml"]), "serverTLSBootstrap: true") {
		t.Fatal("kubelet serving certificate bootstrap is disabled")
	}
}

func TestCreateWorkerBundleWithMirrorsAndCredentials(t *testing.T) {
	dir := t.TempDir()
	kubelet := dir + "/kubelet"
	if err := os.WriteFile(kubelet, []byte("kubelet-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	provider := dir + "/ecr-credential-provider"
	if err := os.WriteFile(provider, []byte("provider-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := dir + "/credential-providers.yaml"
	if err := os.WriteFile(config, []byte(goodCredentialProviderConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	env := dir + "/aws.env"
	if err := os.WriteFile(env, []byte("AWS_ACCESS_KEY_ID=example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, cleanup, err := CreateWorkerBundle(BundleConfig{
		NodeName:    "mac-mini-idle",
		Server:      "https://cluster.example.com:6443",
		CAData:      []byte("test-ca"),
		Token:       "abcdef.0123456789abcdef",
		ClusterDNS:  "10.96.0.10",
		KubeletPath: kubelet,
		RegistryMirrors: []RegistryMirror{
			{Host: "nks.kr.private-ncr.ntruss.com", URL: "https://nks.kr.ncr.ntruss.com"},
		},
		CredentialProviderBins:   []string{provider},
		CredentialProviderConfig: config,
		CredentialProviderEnv:    env,
	})
	if err != nil {
		t.Fatalf("CreateWorkerBundle: %v", err)
	}
	defer cleanup()

	entries := readBundle(t, bundle)
	hosts, ok := entries["certs.d/nks.kr.private-ncr.ntruss.com/hosts.toml"]
	if !ok {
		t.Fatal("bundle is missing the registry mirror hosts.toml")
	}
	wantHosts := `server = "https://nks.kr.private-ncr.ntruss.com"

[host."https://nks.kr.ncr.ntruss.com"]
  capabilities = ["pull", "resolve"]
`
	if string(hosts) != wantHosts {
		t.Fatalf("hosts.toml =\n%s\nwant\n%s", hosts, wantHosts)
	}
	if _, ok := entries["credential-providers/ecr-credential-provider"]; !ok {
		t.Fatal("bundle is missing the credential provider binary")
	}
	if _, ok := entries["credential-providers.yaml"]; !ok {
		t.Fatal("bundle is missing the credential provider config")
	}
	if _, ok := entries["credential-providers.env"]; !ok {
		t.Fatal("bundle is missing the credential provider env file")
	}
	install := string(entries["install.sh"])
	if !strings.Contains(install, "--image-credential-provider-config=/var/lib/idleloom/config/credential-providers.yaml") {
		t.Fatalf("install script is missing the credential provider kubelet flags:\n%s", install)
	}
	if !strings.Contains(install, "config_path = '/etc/containerd/certs.d'") {
		t.Fatalf("install script does not set the containerd certs.d config_path:\n%s", install)
	}
	if !strings.Contains(string(entries["kubelet.service"]), "EnvironmentFile=-/etc/default/idleloom-credential-providers") {
		t.Fatal("kubelet unit is missing the credential provider EnvironmentFile")
	}
}

func TestCreateWorkerBundleOmitsExtrasWhenUnset(t *testing.T) {
	kubelet := t.TempDir() + "/kubelet"
	if err := os.WriteFile(kubelet, []byte("kubelet-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	bundle, cleanup, err := CreateWorkerBundle(BundleConfig{
		NodeName:    "mac-mini-idle",
		Server:      "https://cluster.example.com:6443",
		CAData:      []byte("test-ca"),
		Token:       "abcdef.0123456789abcdef",
		ClusterDNS:  "10.96.0.10",
		KubeletPath: kubelet,
	})
	if err != nil {
		t.Fatalf("CreateWorkerBundle: %v", err)
	}
	defer cleanup()

	entries := readBundle(t, bundle)
	for name := range entries {
		if strings.HasPrefix(name, "certs.d/") || strings.HasPrefix(name, "credential-providers") {
			t.Fatalf("bundle unexpectedly contains %s when no extras are configured", name)
		}
	}
	install := string(entries["install.sh"])
	if strings.Contains(install, "--image-credential-provider-config") {
		t.Fatalf("install script should not reference credential providers when unset:\n%s", install)
	}
}

func readBundle(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	entries := make(map[string][]byte)
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = data
	}
	return entries
}
