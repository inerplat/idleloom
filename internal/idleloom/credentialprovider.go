package idleloom

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

const (
	elfClass64        = 2      // EI_CLASS value for a 64-bit ELF binary.
	elfMachineAArch64 = 0x00B7 // e_machine value for EM_AARCH64 (arm64).
)

// validateCredentialProviderBinary asserts that path is a linux/arm64 ELF
// binary without executing it. Kubelet execs credential providers inside the
// worker VM, so a macOS/darwin build would fail there at runtime.
func validateCredentialProviderBinary(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read credential provider binary %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, 20)
	if _, err := io.ReadFull(file, header); err != nil {
		return fmt.Errorf("credential provider binary %s is too small to be a linux/arm64 build; a macOS/darwin build will not run in the worker VM", path)
	}
	if header[0] != 0x7f || header[1] != 'E' || header[2] != 'L' || header[3] != 'F' {
		return fmt.Errorf("credential provider binary %s is not an ELF binary; it must be a linux/arm64 build; a macOS/darwin build will not run in the worker VM", path)
	}
	if header[4] != elfClass64 {
		return fmt.Errorf("credential provider binary %s is not 64-bit; it must be a linux/arm64 build; a macOS/darwin build will not run in the worker VM", path)
	}
	if machine := binary.LittleEndian.Uint16(header[18:20]); machine != elfMachineAArch64 {
		return fmt.Errorf("credential provider binary %s is not built for arm64 (aarch64); it must be a linux/arm64 build; a macOS/darwin build will not run in the worker VM", path)
	}
	return nil
}

type credentialProviderConfigDoc struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Providers  []struct {
		Name string `json:"name"`
	} `json:"providers"`
}

// validateCredentialProviderConfig parses path as a kubelet
// CredentialProviderConfig and asserts that every referenced provider has a
// matching supplied binary basename.
func validateCredentialProviderConfig(path string, binBasenames map[string]bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read credential provider config %s: %w", path, err)
	}
	var doc credentialProviderConfigDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse credential provider config %s: %w", path, err)
	}
	if doc.APIVersion != "kubelet.config.k8s.io/v1" {
		return fmt.Errorf("credential provider config %s has apiVersion %q; expected kubelet.config.k8s.io/v1", path, doc.APIVersion)
	}
	if doc.Kind != "CredentialProviderConfig" {
		return fmt.Errorf("credential provider config %s has kind %q; expected CredentialProviderConfig", path, doc.Kind)
	}
	for _, provider := range doc.Providers {
		if provider.Name == "" {
			return fmt.Errorf("credential provider config %s has a provider with an empty name", path)
		}
		if !binBasenames[provider.Name] {
			return fmt.Errorf("credential provider config %s references provider %q but no matching --credential-provider-bin was supplied", path, provider.Name)
		}
	}
	return nil
}

// validateCredentialProviders performs fail-fast, pre-side-effect host
// validation of the credential provider inputs. It returns nil when none are
// configured.
func validateCredentialProviders(bins []string, configPath, envPath string) error {
	if configPath == "" && len(bins) == 0 && envPath == "" {
		return nil
	}
	if configPath == "" {
		return fmt.Errorf("a credential provider config is required (--credential-provider-config) when credential providers are configured")
	}
	if len(bins) == 0 {
		return fmt.Errorf("at least one credential provider binary is required (--credential-provider-bin) when credential providers are configured")
	}
	basenames := make(map[string]bool, len(bins))
	for _, bin := range bins {
		if err := validateCredentialProviderBinary(bin); err != nil {
			return err
		}
		base := filepath.Base(bin)
		if basenames[base] {
			return fmt.Errorf("duplicate credential provider binary basename %q; each --credential-provider-bin must have a unique filename", base)
		}
		basenames[base] = true
	}
	if err := validateCredentialProviderConfig(configPath, basenames); err != nil {
		return err
	}
	if envPath != "" {
		file, err := os.Open(envPath)
		if err != nil {
			return fmt.Errorf("read credential provider env file %s: %w", envPath, err)
		}
		_ = file.Close()
	}
	return nil
}
