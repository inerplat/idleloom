package enroll

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	nativewirekube "github.com/inerplat/idleloom/internal/native/wirekube"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
)

func TestRunRejectsAPIOnlyDowngradeWithWireKubeState(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "wirekube-leaf.json"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Config{
		REST: &rest.Config{Host: "https://203.0.113.10:6443"}, Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		Kubernetes: fake.NewSimpleClientset(), HostID: "mac-one", StateDirectory: directory,
		Connectivity: nativewirekube.ConnectivityAPIOnly,
	})
	if err == nil {
		t.Fatal("Run silently downgraded an enrolled WireKube leaf to API-only")
	}
}

func TestEnsureHostRecoversFromPersistedCreateNotFound(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nativekube.HostsGVR: "IdleloomHostList",
	})
	var persisted runtime.Object
	client.PrependReactor("get", "idleloomhosts", func(clienttesting.Action) (bool, runtime.Object, error) {
		if persisted == nil {
			return false, nil, nil
		}
		return true, persisted.DeepCopyObject(), nil
	})
	client.PrependReactor("create", "idleloomhosts", func(action clienttesting.Action) (bool, runtime.Object, error) {
		create := action.(clienttesting.CreateAction)
		persisted = create.GetObject().DeepCopyObject()
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: nativekube.HostsGVR.Group, Resource: nativekube.HostsGVR.Resource}, "host")
	})
	labels := map[string]string{"app.kubernetes.io/managed-by": managedBy}
	annotations := map[string]string{"ai.idleloom.io/enrollment-id": "enrollment"}
	host, err := ensureHost(context.Background(), client, "host-ns", "agent.native", nativev1alpha1.ShellAccessHost, labels, annotations)
	if err != nil {
		t.Fatal(err)
	}
	if host.Name != "host" || host.Spec.AgentID != "agent.native" || host.Spec.ShellAccess != nativev1alpha1.ShellAccessHost {
		t.Fatalf("host = %#v", host)
	}
	if _, err := ensureHost(context.Background(), client, "host-ns", "agent.native", nativev1alpha1.ShellAccessSandboxed, labels, annotations); err == nil {
		t.Fatal("ensureHost silently changed the enrolled shell access boundary")
	}
}

func TestKubernetesClientCAUsesAuthenticationConfigMap(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "extension-apiserver-authentication", Namespace: "kube-system"},
		Data:       map[string]string{"client-ca-file": "certificate"},
	})
	certificate, err := kubernetesClientCA(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if string(certificate) != "certificate" {
		t.Fatalf("client CA = %q", certificate)
	}
}

func TestRequestKubeletServingCertificateCreatesAndApprovesCSR(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor("update", "certificatesigningrequests", func(action clienttesting.Action) (bool, runtime.Object, error) {
		update := action.(clienttesting.UpdateAction)
		csr := update.GetObject().(*certificatesv1.CertificateSigningRequest).DeepCopy()
		if action.GetSubresource() != "approval" || csr.Spec.SignerName != certificatesv1.KubeletServingSignerName {
			t.Fatalf("unexpected CSR approval: subresource=%s signer=%s", action.GetSubresource(), csr.Spec.SignerName)
		}
		approved := false
		for _, condition := range csr.Status.Conditions {
			approved = approved || condition.Type == certificatesv1.CertificateApproved
		}
		if !approved {
			t.Fatal("CSR was not approved")
		}
		csr.Status.Certificate = []byte("signed-certificate")
		gvr := schema.GroupVersionResource{Group: certificatesv1.GroupName, Version: "v1", Resource: "certificatesigningrequests"}
		if err := client.Tracker().Update(gvr, csr, ""); err != nil {
			t.Fatal(err)
		}
		return true, csr, nil
	})
	certificate, err := requestKubeletServingCertificate(context.Background(), client, "mac-one", []byte("csr"))
	if err != nil {
		t.Fatal(err)
	}
	if string(certificate) != "signed-certificate" {
		t.Fatalf("certificate = %q", certificate)
	}
}
