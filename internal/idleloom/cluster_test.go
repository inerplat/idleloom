package idleloom

import (
	"context"
	"io"
	"net/http"
	"strings"
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

type recordingTransport struct {
	statuses []int
	bodies   []string
	calls    int
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		payload, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		r.bodies = append(r.bodies, string(payload))
	} else {
		r.bodies = append(r.bodies, "")
	}
	status := http.StatusOK
	if r.calls < len(r.statuses) {
		status = r.statuses[r.calls]
	}
	r.calls++
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

func TestRetryUnauthorizedTransportResendsOnce(t *testing.T) {
	base := &recordingTransport{statuses: []int{http.StatusUnauthorized, http.StatusOK}}
	request, err := http.NewRequest(http.MethodPost, "https://example.invalid/api", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := (&retryUnauthorizedTransport{base: base}).RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if base.calls != 2 {
		t.Fatalf("base called %d times, want 2", base.calls)
	}
	if base.bodies[0] != "payload" || base.bodies[1] != "payload" {
		t.Fatalf("retry did not resend the body: %q", base.bodies)
	}
}

func TestRetryUnauthorizedTransportGivesUpAfterOneRetry(t *testing.T) {
	base := &recordingTransport{statuses: []int{http.StatusUnauthorized, http.StatusUnauthorized}}
	request, err := http.NewRequest(http.MethodGet, "https://example.invalid/api", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	response, err := (&retryUnauthorizedTransport{base: base}).RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	if base.calls != 2 {
		t.Fatalf("base called %d times, want 2", base.calls)
	}
}

func TestRetryUnauthorizedTransportPassesThroughSuccess(t *testing.T) {
	base := &recordingTransport{statuses: []int{http.StatusOK}}
	request, err := http.NewRequest(http.MethodGet, "https://example.invalid/api", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if _, err := (&retryUnauthorizedTransport{base: base}).RoundTrip(request); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if base.calls != 1 {
		t.Fatalf("base called %d times, want 1", base.calls)
	}
}

func TestRetryUnauthorizedTransportKeepsUnrewindableBody(t *testing.T) {
	base := &recordingTransport{statuses: []int{http.StatusUnauthorized, http.StatusOK}}
	request, err := http.NewRequest(http.MethodPost, "https://example.invalid/api", io.NopCloser(strings.NewReader("stream")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.GetBody = nil
	response, err := (&retryUnauthorizedTransport{base: base}).RoundTrip(request)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
	if base.calls != 1 {
		t.Fatalf("base called %d times, want 1", base.calls)
	}
}
