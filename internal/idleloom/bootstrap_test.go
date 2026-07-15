package idleloom

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"regexp"
	"testing"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCreateBootstrapToken(t *testing.T) {
	client := fake.NewClientset(
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "system:node-bootstrapper"}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "system:certificates.k8s.io:certificatesigningrequests:nodeclient"}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "system:certificates.k8s.io:certificatesigningrequests:selfnodeclient"}},
	)
	token, err := CreateBootstrapToken(context.Background(), client, 30*time.Minute)
	if err != nil {
		t.Fatalf("CreateBootstrapToken: %v", err)
	}
	if !regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`).MatchString(token.Value) {
		t.Fatalf("unexpected token format %q", token.Value)
	}
	secret, err := client.CoreV1().Secrets("kube-system").Get(context.Background(), token.SecretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get token secret: %v", err)
	}
	if secret.Type != corev1.SecretTypeBootstrapToken {
		t.Fatalf("secret type = %q", secret.Type)
	}
	if got := string(secret.Data["auth-extra-groups"]); got != bootstrapGroup {
		t.Fatalf("auth-extra-groups = %q", got)
	}
	if _, err := client.RbacV1().ClusterRoleBindings().Get(context.Background(), "idleloom:node-autoapprove-bootstrap", metav1.GetOptions{}); err != nil {
		t.Fatalf("get bootstrap approval binding: %v", err)
	}
	if err := token.Delete(context.Background()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestValidateKubeletServingCSR(t *testing.T) {
	csr := servingCSR(t, "valid", "worker-a", "172.20.10.2", []string{"system:nodes"}, servingUsages())
	if err := validateKubeletServingCSR(csr, "worker-a", "172.20.10.2"); err != nil {
		t.Fatalf("valid serving CSR rejected: %v", err)
	}
	if err := validateKubeletServingCSR(csr, "worker-a", "172.20.10.3"); err == nil {
		t.Fatal("serving CSR with an unrelated IP SAN was accepted")
	}
}

func TestValidateKubeletServingCSRAcceptsAlgorithmSpecificUsages(t *testing.T) {
	for _, test := range []struct {
		name   string
		key    any
		usages []certificatesv1.KeyUsage
	}{
		{name: "ed25519", key: newEd25519Key(t), usages: servingUsages()},
		{name: "ecdsa", key: newECDSAKey(t), usages: servingUsages()},
		{name: "rsa", key: newRSAKey(t), usages: append(servingUsages(), certificatesv1.UsageKeyEncipherment)},
	} {
		t.Run(test.name, func(t *testing.T) {
			csr := servingCSRWithKey(t, test.name, "worker-a", "172.20.10.2", []string{"system:nodes"}, test.usages, test.key)
			if err := validateKubeletServingCSR(csr, "worker-a", "172.20.10.2"); err != nil {
				t.Fatalf("valid %s serving CSR rejected: %v", test.name, err)
			}
		})
	}
}

func TestValidateKubeletServingCSRRejectsRSAUsageOnECDSAKey(t *testing.T) {
	usages := append(servingUsages(), certificatesv1.UsageKeyEncipherment)
	csr := servingCSRWithKey(t, "ecdsa-extra", "worker-a", "172.20.10.2", []string{"system:nodes"}, usages, newECDSAKey(t))
	if err := validateKubeletServingCSR(csr, "worker-a", "172.20.10.2"); err == nil {
		t.Fatal("ECDSA serving CSR with RSA key encipherment usage was accepted")
	}
}

func TestValidateKubeletServingCSRRejectsWeakKeyAndTrailingData(t *testing.T) {
	weakRSA, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	csr := servingCSRWithKey(t, "weak-rsa", "worker-a", "172.20.10.2", []string{"system:nodes"}, append(servingUsages(), certificatesv1.UsageKeyEncipherment), weakRSA)
	if err := validateKubeletServingCSR(csr, "worker-a", "172.20.10.2"); err == nil {
		t.Fatal("serving CSR with a weak RSA key was accepted")
	}
	csr = servingCSR(t, "trailing", "worker-a", "172.20.10.2", []string{"system:nodes"}, servingUsages())
	csr.Spec.Request = append(csr.Spec.Request, []byte("unexpected")...)
	if err := validateKubeletServingCSR(csr, "worker-a", "172.20.10.2"); err == nil {
		t.Fatal("serving CSR with trailing data was accepted")
	}
}

func TestValidateKubeletServingCSRRejectsExtraOrganizationAndUsage(t *testing.T) {
	extraOrganization := servingCSR(t, "extra-org", "worker-a", "172.20.10.2", []string{"system:nodes", "system:masters"}, servingUsages())
	if err := validateKubeletServingCSR(extraOrganization, "worker-a", "172.20.10.2"); err == nil {
		t.Fatal("serving CSR with an extra organization was accepted")
	}
	extraUsage := append(servingUsages(), certificatesv1.UsageClientAuth)
	csr := servingCSR(t, "extra-usage", "worker-a", "172.20.10.2", []string{"system:nodes"}, extraUsage)
	if err := validateKubeletServingCSR(csr, "worker-a", "172.20.10.2"); err == nil {
		t.Fatal("serving CSR with an extra usage was accepted")
	}
}

func TestApproveKubeletServingCSRDoesNotWaitWhenNoCurrentRequestExists(t *testing.T) {
	client := fake.NewClientset()
	start := time.Now()
	if err := ApproveKubeletServingCSR(context.Background(), client, "worker-a", "172.20.10.2", start, false, time.Hour); err != nil {
		t.Fatalf("ApproveKubeletServingCSR: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("non-waiting serving CSR check blocked without a pending request")
	}
}

func TestApproveKubeletServingCSRIgnoresOldCompletedRequests(t *testing.T) {
	notBefore := time.Now().UTC()
	old := servingCSR(t, "old-denied", "worker-a", "172.20.10.2", []string{"system:nodes"}, servingUsages())
	old.CreationTimestamp = metav1.NewTime(notBefore.Add(-time.Minute))
	old.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{
		Type: certificatesv1.CertificateDenied, Status: corev1.ConditionTrue,
	}}
	oldApproved := servingCSR(t, "old-approved", "worker-a", "172.20.10.99", []string{"system:nodes"}, servingUsages())
	oldApproved.CreationTimestamp = metav1.NewTime(notBefore.Add(-time.Minute))
	oldApproved.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{
		Type: certificatesv1.CertificateApproved, Status: corev1.ConditionTrue,
	}}
	client := fake.NewClientset(old, oldApproved)
	if err := ApproveKubeletServingCSR(context.Background(), client, "worker-a", "172.20.10.2", notBefore, false, time.Hour); err != nil {
		t.Fatalf("old completed request affected current start: %v", err)
	}
}

func TestApproveKubeletServingCSRPrefersRecentPendingRequest(t *testing.T) {
	notBefore := time.Now().UTC()
	old := servingCSR(t, "old-invalid", "worker-a", "172.20.10.99", []string{"system:nodes"}, servingUsages())
	old.CreationTimestamp = metav1.NewTime(notBefore.Add(-time.Minute))
	recent := servingCSR(t, "recent-valid", "worker-a", "172.20.10.2", []string{"system:nodes"}, servingUsages())
	recent.CreationTimestamp = metav1.NewTime(notBefore.Add(time.Second))
	client := fake.NewClientset(old, recent)
	if err := ApproveKubeletServingCSR(context.Background(), client, "worker-a", "172.20.10.2", notBefore, false, time.Hour); err != nil {
		t.Fatalf("ApproveKubeletServingCSR: %v", err)
	}
	approved, err := client.CertificatesV1().CertificateSigningRequests().Get(context.Background(), recent.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !csrApproved(approved) {
		t.Fatal("recent pending serving CSR was not approved")
	}
}

func TestApproveKubeletServingCSRProcessesLongPendingRotation(t *testing.T) {
	enrollment := time.Now().UTC().Add(-24 * time.Hour)
	pending := servingCSR(t, "rotation-pending", "worker-a", "172.20.10.2", []string{"system:nodes"}, servingUsages())
	pending.CreationTimestamp = metav1.NewTime(time.Now().UTC().Add(-2 * time.Hour))
	client := fake.NewClientset(pending)
	if err := ApproveKubeletServingCSR(context.Background(), client, "worker-a", "172.20.10.2", enrollment, false, 0); err != nil {
		t.Fatalf("ApproveKubeletServingCSR: %v", err)
	}
	approved, err := client.CertificatesV1().CertificateSigningRequests().Get(context.Background(), pending.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !csrApproved(approved) {
		t.Fatal("long-pending valid rotation CSR was not approved")
	}
}

func TestApproveKubeletServingCSRWaitsForCertificateIssuance(t *testing.T) {
	notBefore := time.Now().UTC()
	approved := servingCSR(t, "approved-unissued", "worker-a", "172.20.10.2", []string{"system:nodes"}, servingUsages())
	approved.CreationTimestamp = metav1.NewTime(notBefore.Add(time.Second))
	approved.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{
		Type: certificatesv1.CertificateApproved, Status: corev1.ConditionTrue,
	}}
	client := fake.NewClientset(approved)
	if err := ApproveKubeletServingCSR(context.Background(), client, "worker-a", "172.20.10.2", notBefore, true, 20*time.Millisecond); err == nil {
		t.Fatal("approved but unissued serving CSR was treated as complete")
	}
	approved.Status.Certificate = []byte("issued")
	client = fake.NewClientset(approved)
	if err := ApproveKubeletServingCSR(context.Background(), client, "worker-a", "172.20.10.2", notBefore, true, time.Second); err != nil {
		t.Fatalf("issued serving CSR was rejected: %v", err)
	}
}

func servingCSR(t *testing.T, name, nodeName, guestIP string, organizations []string, usages []certificatesv1.KeyUsage) *certificatesv1.CertificateSigningRequest {
	t.Helper()
	return servingCSRWithKey(t, name, nodeName, guestIP, organizations, usages, newEd25519Key(t))
}

func servingCSRWithKey(t *testing.T, name, nodeName, guestIP string, organizations []string, usages []certificatesv1.KeyUsage, key any) *certificatesv1.CertificateSigningRequest {
	t.Helper()
	request, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: "system:node:" + nodeName, Organization: organizations},
		DNSNames:    []string{nodeName},
		IPAddresses: []net.IP{net.ParseIP(guestIP)},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	return &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			SignerName: certificatesv1.KubeletServingSignerName,
			Username:   "system:node:" + nodeName,
			Request:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: request}),
			Usages:     usages,
		},
	}
}

func servingUsages() []certificatesv1.KeyUsage {
	return []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageServerAuth,
	}
}

func newEd25519Key(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newECDSAKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestCreateBootstrapTokenRejectsNonPositiveTTL(t *testing.T) {
	if _, err := CreateBootstrapToken(context.Background(), fake.NewClientset(), 0); err == nil {
		t.Fatal("expected an error")
	}
}
