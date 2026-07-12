package kubeletbridge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	certificateFileName = "kubelet-serving.crt"
	privateKeyFileName  = "kubelet-serving.key"
	clientCAFileName    = "kubelet-client-ca.crt"
)

type Identity struct {
	CertificateFile string
	PrivateKeyFile  string
}

func IdentityPaths(directory string) Identity {
	return Identity{
		CertificateFile: filepath.Join(directory, certificateFileName),
		PrivateKeyFile:  filepath.Join(directory, privateKeyFileName),
	}
}

func ClientCAPath(directory string) string {
	return filepath.Join(directory, clientCAFileName)
}

func EnsureClientCA(directory string, certificate []byte) (string, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certificate) {
		return "", fmt.Errorf("Kubernetes client CA is not valid PEM")
	}
	path := ClientCAPath(directory)
	if err := atomicWrite(path, certificate, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

type ServingCertificateRequester func(context.Context, []byte) ([]byte, error)

func EnsureServingIdentity(ctx context.Context, directory, meshAddress, nodeName string, now time.Time, requester ServingCertificateRequester) (Identity, error) {
	ip, _, err := net.ParseCIDR(meshAddress)
	if err != nil || ip.To4() == nil {
		return Identity{}, fmt.Errorf("kubelet bridge requires an IPv4 mesh /32")
	}
	if nodeName == "" || requester == nil {
		return Identity{}, fmt.Errorf("kubelet bridge node name and certificate requester are required")
	}
	identity := IdentityPaths(directory)
	if certificate, err := os.ReadFile(identity.CertificateFile); err == nil {
		block, _ := pem.Decode(certificate)
		if block != nil {
			parsed, parseErr := x509.ParseCertificate(block.Bytes)
			if parseErr == nil && validateServingCertificate(parsed, ip, nodeName, now) == nil {
				if parsed.NotAfter.After(now.Add(7 * 24 * time.Hour)) {
					if _, keyErr := tls.LoadX509KeyPair(identity.CertificateFile, identity.PrivateKeyFile); keyErr == nil {
						return identity, nil
					}
				}
			}
		}
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Identity{}, err
	}
	requestDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: "system:node:" + nodeName, Organization: []string{"system:nodes"}},
		IPAddresses: []net.IP{ip},
	}, privateKey)
	if err != nil {
		return Identity{}, err
	}
	certificatePEM, err := requester(ctx, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: requestDER}))
	if err != nil {
		return Identity{}, err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return Identity{}, err
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	certificateBlock, _ := pem.Decode(certificatePEM)
	if certificateBlock == nil {
		return Identity{}, fmt.Errorf("Kubernetes signer returned an invalid serving certificate")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return Identity{}, err
	}
	if err := validateServingCertificate(certificate, ip, nodeName, now); err != nil {
		return Identity{}, fmt.Errorf("validate Kubernetes-signed serving certificate: %w", err)
	}
	if _, err := tls.X509KeyPair(certificatePEM, privatePEM); err != nil {
		return Identity{}, fmt.Errorf("Kubernetes-signed serving certificate does not match the generated key: %w", err)
	}
	if err := atomicWrite(identity.CertificateFile, certificatePEM, 0o644); err != nil {
		return Identity{}, err
	}
	if err := atomicWrite(identity.PrivateKeyFile, privatePEM, 0o600); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

func validateServingCertificate(certificate *x509.Certificate, ip net.IP, nodeName string, now time.Time) error {
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return fmt.Errorf("certificate is not currently valid")
	}
	if certificate.Subject.CommonName != "system:node:"+nodeName || !containsString(certificate.Subject.Organization, "system:nodes") {
		return fmt.Errorf("certificate subject is not the requested Kubernetes node identity")
	}
	if !containsIP(certificate.IPAddresses, ip) {
		return fmt.Errorf("certificate does not contain mesh IP %s", ip)
	}
	if !containsExtendedKeyUsage(certificate.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		return fmt.Errorf("certificate does not permit server authentication")
	}
	return nil
}

func containsIP(values []net.IP, expected net.IP) bool {
	for _, value := range values {
		if value.Equal(expected) {
			return true
		}
	}
	return false
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func containsExtendedKeyUsage(values []x509.ExtKeyUsage, expected x509.ExtKeyUsage) bool {
	for _, value := range values {
		if value == expected || value == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".kubelet-identity-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}
