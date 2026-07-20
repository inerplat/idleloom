package idleloom

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// elfHeader crafts a minimal 20-byte ELF identification header with the given
// EI_CLASS and e_machine so tests do not need real binaries.
func elfHeader(class byte, machine uint16) []byte {
	header := make([]byte, 20)
	header[0], header[1], header[2], header[3] = 0x7f, 'E', 'L', 'F'
	header[4] = class
	header[18] = byte(machine)
	header[19] = byte(machine >> 8)
	return header
}

func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateCredentialProviderBinary(t *testing.T) {
	if err := validateCredentialProviderBinary(writeTempFile(t, "arm64", elfHeader(elfClass64, elfMachineAArch64))); err != nil {
		t.Fatalf("valid linux/arm64 ELF rejected: %v", err)
	}

	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"x86-64", elfHeader(elfClass64, 0x3E), "not built for arm64"},
		{"32-bit arm", elfHeader(1, elfMachineAArch64), "not 64-bit"},
		{"mach-o darwin", []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xb7, 0x00}, "not an ELF binary"},
		{"too short", []byte{0x7f, 'E', 'L'}, "too small"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCredentialProviderBinary(writeTempFile(t, tc.name, tc.data))
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

const goodCredentialProviderConfig = `apiVersion: kubelet.config.k8s.io/v1
kind: CredentialProviderConfig
providers:
  - name: ecr-credential-provider
    matchImages:
      - "*.dkr.ecr.*.amazonaws.com"
    defaultCacheDuration: "12h"
    apiVersion: credentialprovider.kubelet.k8s.io/v1
`

func TestValidateCredentialProviderConfig(t *testing.T) {
	bins := map[string]bool{"ecr-credential-provider": true}
	if err := validateCredentialProviderConfig(writeTempFile(t, "config.yaml", []byte(goodCredentialProviderConfig)), bins); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := []struct {
		name   string
		config string
		bins   map[string]bool
		want   string
	}{
		{
			name:   "wrong kind",
			config: strings.Replace(goodCredentialProviderConfig, "CredentialProviderConfig", "KubeletConfiguration", 1),
			bins:   bins,
			want:   "expected CredentialProviderConfig",
		},
		{
			name:   "wrong apiVersion",
			config: strings.Replace(goodCredentialProviderConfig, "kubelet.config.k8s.io/v1\n", "kubelet.config.k8s.io/v1beta1\n", 1),
			bins:   bins,
			want:   "expected kubelet.config.k8s.io/v1",
		},
		{
			name:   "provider missing bin",
			config: goodCredentialProviderConfig,
			bins:   map[string]bool{"other-provider": true},
			want:   `references provider "ecr-credential-provider"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCredentialProviderConfig(writeTempFile(t, "config.yaml", []byte(tc.config)), tc.bins)
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateCredentialProvidersNoneConfigured(t *testing.T) {
	if err := validateCredentialProviders(nil, "", ""); err != nil {
		t.Fatalf("no providers should validate cleanly: %v", err)
	}
}

func TestValidateCredentialProvidersRequiresConfigAndBin(t *testing.T) {
	bin := writeTempFile(t, "ecr-credential-provider", elfHeader(elfClass64, elfMachineAArch64))
	if err := validateCredentialProviders([]string{bin}, "", ""); err == nil || !strings.Contains(err.Error(), "config is required") {
		t.Fatalf("expected a missing-config error, got %v", err)
	}
	config := writeTempFile(t, "config.yaml", []byte(goodCredentialProviderConfig))
	if err := validateCredentialProviders(nil, config, ""); err == nil || !strings.Contains(err.Error(), "binary is required") {
		t.Fatalf("expected a missing-bin error, got %v", err)
	}
}

func TestValidateCredentialProvidersEndToEnd(t *testing.T) {
	bin := writeTempFile(t, "ecr-credential-provider", elfHeader(elfClass64, elfMachineAArch64))
	config := writeTempFile(t, "config.yaml", []byte(goodCredentialProviderConfig))
	env := writeTempFile(t, "aws.env", []byte("AWS_ACCESS_KEY_ID=example\n"))
	if err := validateCredentialProviders([]string{bin}, config, env); err != nil {
		t.Fatalf("valid credential providers rejected: %v", err)
	}
	if err := validateCredentialProviders([]string{bin}, config, filepath.Join(t.TempDir(), "missing.env")); err == nil {
		t.Fatal("expected an error for a missing env file")
	}
}

func TestValidateCredentialProvidersRejectsDuplicateBinaryBasename(t *testing.T) {
	header := elfHeader(elfClass64, elfMachineAArch64)
	binA := filepath.Join(t.TempDir(), "ecr-credential-provider")
	binB := filepath.Join(t.TempDir(), "ecr-credential-provider")
	for _, path := range []string{binA, binB} {
		if err := os.WriteFile(path, header, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	config := writeTempFile(t, "config.yaml", []byte(goodCredentialProviderConfig))
	err := validateCredentialProviders([]string{binA, binB}, config, "")
	if err == nil || !strings.Contains(err.Error(), "duplicate credential provider binary basename") {
		t.Fatalf("expected a duplicate-basename error, got %v", err)
	}
}
