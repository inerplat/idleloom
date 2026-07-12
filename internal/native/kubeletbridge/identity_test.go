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
	"math/big"
	"net"
	"os"
	"testing"
	"time"
)

func TestEnsureServingIdentityPinsMeshIPAndUsesClusterSigner(t *testing.T) {
	directory := t.TempDir()
	now := time.Now().UTC()
	_, signer := testServingSigner(t, now)
	requests := 0
	requester := func(ctx context.Context, request []byte) ([]byte, error) {
		requests++
		return signer(ctx, request)
	}
	identity, err := EnsureServingIdentity(context.Background(), directory, "198.18.18.52/32", "idleloom-test", now, requester)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(identity.CertificateFile, identity.PrivateKeyFile); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(identity.CertificateFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(certificate.IPAddresses) != 1 || !certificate.IPAddresses[0].Equal(net.ParseIP("198.18.18.52")) {
		t.Fatalf("IP SANs = %v", certificate.IPAddresses)
	}
	if certificate.Issuer.CommonName != "test-kubernetes-ca" || requests != 1 {
		t.Fatalf("issuer=%s requests=%d", certificate.Issuer.CommonName, requests)
	}
	if _, err := EnsureServingIdentity(context.Background(), directory, "198.18.18.52/32", "idleloom-test", now, requester); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatal("valid serving identity was unnecessarily rotated")
	}
	info, err := os.Stat(identity.PrivateKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %o", info.Mode().Perm())
	}
}

func TestEnsureClientCAValidatesAndPersistsPEM(t *testing.T) {
	directory := t.TempDir()
	clientCA, _ := testServingSigner(t, time.Now().UTC())
	path, err := EnsureClientCA(directory, clientCA)
	if err != nil {
		t.Fatal(err)
	}
	if path != ClientCAPath(directory) {
		t.Fatalf("client CA path = %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("client CA mode = %o", info.Mode().Perm())
	}
	if _, err := EnsureClientCA(directory, []byte("not a certificate")); err == nil {
		t.Fatal("EnsureClientCA accepted invalid PEM")
	}
}

func testServingSigner(t *testing.T, now time.Time) ([]byte, ServingCertificateRequester) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-kubernetes-ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	signer := func(_ context.Context, requestPEM []byte) ([]byte, error) {
		block, _ := pem.Decode(requestPEM)
		request, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			return nil, err
		}
		if err := request.CheckSignature(); err != nil {
			return nil, err
		}
		certificateDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
			SerialNumber: big.NewInt(2), Subject: request.Subject,
			NotBefore: now.Add(-time.Minute), NotAfter: now.Add(30 * 24 * time.Hour),
			KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			IPAddresses: request.IPAddresses,
		}, ca, request.PublicKey, key)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}), nil
	}
	return caPEM, signer
}
