package idleloom

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNormalizeKubernetesVersion(t *testing.T) {
	for input, expected := range map[string]string{
		"v1.35.4":             "v1.35.4",
		"v1.31.5-gke.1234000": "v1.31.5",
		"v1.30.13-eks-abcdef": "v1.30.13",
		"v1.29.8+rke2r1":      "v1.29.8",
	} {
		got, err := normalizeKubernetesVersion(input)
		if err != nil {
			t.Errorf("normalizeKubernetesVersion(%q): %v", input, err)
			continue
		}
		if got != expected {
			t.Errorf("normalizeKubernetesVersion(%q) = %q, want %q", input, got, expected)
		}
	}
	if _, err := normalizeKubernetesVersion("development"); err == nil {
		t.Fatal("expected an invalid GitVersion to be rejected")
	}
}

func TestDiscoverClusterDNSUsesKnownNamesThenLabels(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name     string
		services []*corev1.Service
		want     string
	}{
		{
			name:     "kube-dns",
			services: []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system"}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.10"}}},
			want:     "kube-dns",
		},
		{
			name:     "coredns",
			services: []*corev1.Service{{ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: "kube-system"}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.11"}}},
			want:     "coredns",
		},
		{
			name: "label",
			services: []*corev1.Service{{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-dns", Namespace: "kube-system", Labels: map[string]string{"k8s-app": "kube-dns"}},
				Spec:       corev1.ServiceSpec{ClusterIP: "10.96.0.12"},
			}},
			want: "cluster-dns",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := make([]runtime.Object, 0, len(test.services))
			for _, service := range test.services {
				objects = append(objects, service)
			}
			service, err := discoverClusterDNS(ctx, fake.NewClientset(objects...))
			if err != nil {
				t.Fatal(err)
			}
			if service.Name != test.want {
				t.Fatalf("service = %q, want %q", service.Name, test.want)
			}
		})
	}
}

func TestDiscoverClusterDNSRejectsHeadlessServices(t *testing.T) {
	client := fake.NewClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system", Labels: map[string]string{"k8s-app": "kube-dns"}},
		Spec:       corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	})
	if _, err := discoverClusterDNS(context.Background(), client); err == nil {
		t.Fatal("headless DNS Service was accepted")
	}
}
