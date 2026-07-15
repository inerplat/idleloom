package idleloom

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

const (
	bootstrapGroup = "system:bootstrappers:idleloom:default-node-token"
)

type BootstrapToken struct {
	Value      string
	SecretName string
	UID        types.UID
	client     kubernetes.Interface
}

func ApproveKubeletServingCSR(ctx context.Context, client kubernetes.Interface, nodeName, guestIP string, notBefore time.Time, wait bool, timeout time.Duration) error {
	if nodeName == "" || guestIP == "" {
		return fmt.Errorf("node name and guest IP are required for serving certificate approval")
	}
	if wait && timeout <= 0 {
		return fmt.Errorf("serving certificate approval timeout must be positive")
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		csrs, err := client.CertificatesV1().CertificateSigningRequests().List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("list kubelet serving certificate requests: %w", err)
		}
		var pending, approved []*certificatesv1.CertificateSigningRequest
		var denied bool
		for i := range csrs.Items {
			csr := &csrs.Items[i]
			if csr.Spec.SignerName != certificatesv1.KubeletServingSignerName || csr.Spec.Username != "system:node:"+nodeName {
				continue
			}
			if csr.CreationTimestamp.Time.Before(notBefore) {
				continue
			}
			if csrDenied(csr) {
				denied = true
				continue
			}
			if csrApproved(csr) {
				approved = append(approved, csr)
				continue
			}
			pending = append(pending, csr)
		}
		if len(pending) > 0 {
			sort.Slice(pending, func(i, j int) bool {
				return pending[i].CreationTimestamp.After(pending[j].CreationTimestamp.Time)
			})
			csr := pending[0]
			if err := validateKubeletServingCSR(csr, nodeName, guestIP); err != nil {
				return fmt.Errorf("refusing kubelet serving certificate request %s: %w", csr.Name, err)
			}
			copy := csr.DeepCopy()
			copy.Status.Conditions = append(copy.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
				Type:           certificatesv1.CertificateApproved,
				Status:         corev1.ConditionTrue,
				Reason:         "IdleloomNodeServingCertificate",
				Message:        "Idleloom verified the node identity and serving certificate SANs",
				LastUpdateTime: metav1.Now(),
			})
			if _, err := client.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, copy.Name, copy, metav1.UpdateOptions{}); err != nil {
				return fmt.Errorf("approve kubelet serving certificate request %s: %w", copy.Name, err)
			}
			if wait {
				return waitForIssuedServingCertificate(ctx, client, copy.Name, timeout)
			}
			return nil
		}
		if len(approved) > 0 {
			sort.Slice(approved, func(i, j int) bool {
				return approved[i].CreationTimestamp.After(approved[j].CreationTimestamp.Time)
			})
			if err := validateKubeletServingCSR(approved[0], nodeName, guestIP); err != nil {
				return fmt.Errorf("approved kubelet serving certificate request %s does not match Idleloom state: %w", approved[0].Name, err)
			}
			if wait && len(approved[0].Status.Certificate) == 0 {
				return waitForIssuedServingCertificate(ctx, client, approved[0].Name, timeout)
			}
			return nil
		}
		if denied && wait {
			return fmt.Errorf("a recent kubelet serving certificate request for %s was denied", nodeName)
		}
		if !wait {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out after %s waiting for kubelet serving certificate request", timeout)
		case <-ticker.C:
		}
	}
}

func waitForIssuedServingCertificate(ctx context.Context, client kubernetes.Interface, csrName string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		csr, err := client.CertificatesV1().CertificateSigningRequests().Get(ctx, csrName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get approved kubelet serving certificate request %s: %w", csrName, err)
		}
		if csrDenied(csr) {
			return fmt.Errorf("approved kubelet serving certificate request %s was denied", csrName)
		}
		if len(csr.Status.Certificate) > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out after %s waiting for kubelet serving certificate issuance", timeout)
		case <-ticker.C:
		}
	}
}

func validateKubeletServingCSR(csr *certificatesv1.CertificateSigningRequest, nodeName, guestIP string) error {
	block, rest := pem.Decode(csr.Spec.Request)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return fmt.Errorf("request is not a PEM certificate request")
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		return fmt.Errorf("request contains trailing PEM data")
	}
	request, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse certificate request: %w", err)
	}
	if err := request.CheckSignature(); err != nil {
		return fmt.Errorf("verify certificate request signature: %w", err)
	}
	expectedUsages := []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageServerAuth,
	}
	switch key := request.PublicKey.(type) {
	case *rsa.PublicKey:
		if key.N.BitLen() < 2048 {
			return fmt.Errorf("the RSA public key must contain at least 2048 bits")
		}
		expectedUsages = append(expectedUsages, certificatesv1.UsageKeyEncipherment)
	case *ecdsa.PublicKey:
		if key.Curve == nil || key.Curve.Params().BitSize < 256 {
			return fmt.Errorf("the ECDSA public key must use a curve with at least 256 bits")
		}
	case ed25519.PublicKey:
		if len(key) != ed25519.PublicKeySize {
			return fmt.Errorf("the Ed25519 public key has an invalid size")
		}
	default:
		return fmt.Errorf("unsupported public key algorithm %T", request.PublicKey)
	}
	if !sameKeyUsages(csr.Spec.Usages, expectedUsages) {
		return fmt.Errorf("unexpected usages %v for %s public key", csr.Spec.Usages, request.PublicKeyAlgorithm)
	}
	if request.Subject.CommonName != "system:node:"+nodeName || !reflect.DeepEqual(request.Subject.Organization, []string{"system:nodes"}) {
		return fmt.Errorf("unexpected subject %q organizations=%v", request.Subject.CommonName, request.Subject.Organization)
	}
	if len(request.DNSNames) != 1 || request.DNSNames[0] != nodeName {
		return fmt.Errorf("unexpected DNS SANs %v", request.DNSNames)
	}
	if len(request.IPAddresses) != 1 || request.IPAddresses[0].String() != guestIP {
		return fmt.Errorf("unexpected IP SANs %v", request.IPAddresses)
	}
	if len(request.EmailAddresses) != 0 || len(request.URIs) != 0 {
		return fmt.Errorf("unexpected email or URI SANs")
	}
	return nil
}

func sameKeyUsages(actual, expected []certificatesv1.KeyUsage) bool {
	if len(actual) != len(expected) {
		return false
	}
	counts := make(map[certificatesv1.KeyUsage]int, len(expected))
	for _, usage := range expected {
		counts[usage]++
	}
	for _, usage := range actual {
		counts[usage]--
		if counts[usage] < 0 {
			return false
		}
	}
	return true
}

func csrApproved(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, condition := range csr.Status.Conditions {
		if condition.Type == certificatesv1.CertificateApproved && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func csrDenied(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, condition := range csr.Status.Conditions {
		if condition.Type == certificatesv1.CertificateDenied && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func CreateBootstrapToken(ctx context.Context, client kubernetes.Interface, ttl time.Duration) (*BootstrapToken, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("bootstrap token TTL must be positive")
	}
	if err := ensureBootstrapRBAC(ctx, client); err != nil {
		return nil, err
	}
	id, err := randomTokenPart(6)
	if err != nil {
		return nil, err
	}
	secret, err := randomTokenPart(16)
	if err != nil {
		return nil, err
	}
	name := "bootstrap-token-" + id
	expires := time.Now().UTC().Add(ttl).Format(time.RFC3339)
	created, err := client.CoreV1().Secrets("kube-system").Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "idleloom",
			},
		},
		Type: corev1.SecretTypeBootstrapToken,
		Data: map[string][]byte{
			"token-id":                       []byte(id),
			"token-secret":                   []byte(secret),
			"expiration":                     []byte(expires),
			"usage-bootstrap-authentication": []byte("true"),
			"usage-bootstrap-signing":        []byte("true"),
			"auth-extra-groups":              []byte(bootstrapGroup),
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create bootstrap token: %w", err)
	}
	return &BootstrapToken{
		Value:      id + "." + secret,
		SecretName: name,
		UID:        created.UID,
		client:     client,
	}, nil
}

func (t *BootstrapToken) Delete(ctx context.Context) error {
	if t == nil || t.client == nil {
		return nil
	}
	err := t.client.CoreV1().Secrets("kube-system").Delete(ctx, t.SecretName, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &t.UID},
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete bootstrap token %s: %w", t.SecretName, err)
	}
	return nil
}

func ensureBootstrapRBAC(ctx context.Context, client kubernetes.Interface) error {
	requiredRoles := []string{
		"system:node-bootstrapper",
		"system:certificates.k8s.io:certificatesigningrequests:nodeclient",
		"system:certificates.k8s.io:certificatesigningrequests:selfnodeclient",
	}
	for _, name := range requiredRoles {
		if _, err := client.RbacV1().ClusterRoles().Get(ctx, name, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("required bootstrap ClusterRole %s is unavailable: %w", name, err)
		}
	}
	bindings := []*rbacv1.ClusterRoleBinding{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "idleloom:node-bootstrapper"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:node-bootstrapper"},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "Group", Name: bootstrapGroup}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "idleloom:node-autoapprove-bootstrap"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:certificates.k8s.io:certificatesigningrequests:nodeclient"},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "Group", Name: bootstrapGroup}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "idleloom:node-autoapprove-rotation"},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "system:certificates.k8s.io:certificatesigningrequests:selfnodeclient"},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "Group", Name: "system:nodes"}},
		},
	}
	for _, desired := range bindings {
		existing, err := client.RbacV1().ClusterRoleBindings().Get(ctx, desired.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if _, err := client.RbacV1().ClusterRoleBindings().Create(ctx, desired, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("create ClusterRoleBinding %s: %w", desired.Name, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("get ClusterRoleBinding %s: %w", desired.Name, err)
		}
		if existing.RoleRef != desired.RoleRef {
			return fmt.Errorf("the ClusterRoleBinding %s has an unexpected immutable roleRef", desired.Name)
		}
		if reflect.DeepEqual(existing.Subjects, desired.Subjects) {
			continue
		}
		existing.Subjects = desired.Subjects
		if _, err := client.RbacV1().ClusterRoleBindings().Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update ClusterRoleBinding %s: %w", desired.Name, err)
		}
	}
	return nil
}

func randomTokenPart(length int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, 0, length)
	limit := byte(256 - (256 % len(alphabet)))
	for len(result) < length {
		var candidate [1]byte
		if _, err := rand.Read(candidate[:]); err != nil {
			return "", fmt.Errorf("generate bootstrap token: %w", err)
		}
		if candidate[0] >= limit {
			continue
		}
		result = append(result, alphabet[int(candidate[0])%len(alphabet)])
	}
	return string(result), nil
}
