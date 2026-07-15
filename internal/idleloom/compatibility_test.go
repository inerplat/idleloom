package idleloom

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestWorkerCompatibilityWarnsForPrivateCNIRegistry(t *testing.T) {
	cni := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "network-agent", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			NodeSelector:   map[string]string{corev1.LabelOSStable: "linux"},
			InitContainers: []corev1.Container{{Name: "install", Image: "registry.internal.example/network/cni:v1"}},
			Containers:     []corev1.Container{{Name: "agent", Image: "registry.internal.example/network/cni:v1"}},
			Volumes:        []corev1.Volume{{Name: "cni", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/opt/cni/bin"}}}}}}},
	}
	report, err := checkWorkerCompatibility(context.Background(), fake.NewClientset(cni), "https://api.example.test", resolver(map[string][]string{
		"registry.internal.example": {"10.20.30.40"},
		"api.example.test":          {"8.8.8.8"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "cannot prove") {
		t.Fatalf("compatibility report = %#v", report)
	}
}

func TestWorkerCompatibilityWarnsForNonCriticalPrivateAddon(t *testing.T) {
	addon := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-agent", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: "registry.internal.example/storage/agent:v1"}},
		}}},
	}
	report, err := checkWorkerCompatibility(context.Background(), fake.NewClientset(addon), "https://api.example.test", resolver(map[string][]string{
		"registry.internal.example": {"192.168.1.20"},
		"api.example.test":          {"8.8.8.8"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 1 {
		t.Fatalf("report = %#v", report)
	}
}

func TestWorkerCompatibilityAcceptsPublicBootstrapImages(t *testing.T) {
	cni := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "network-agent", Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: "public.example/network/cni@sha256:" + strings.Repeat("a", 64)}},
			Volumes:    []corev1.Volume{{Name: "cni", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc/cni/net.d"}}}}}}},
	}
	report, err := checkWorkerCompatibility(context.Background(), fake.NewClientset(cni), "https://api.example.test", resolver(map[string][]string{
		"public.example":   {"1.1.1.1"},
		"api.example.test": {"8.8.8.8"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("report = %#v", report)
	}
}

func TestPublicIPRejectsNonUnicastAndPrivateAddresses(t *testing.T) {
	for _, address := range []string{
		"0.0.0.0", "127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.1.1",
		"192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1",
		"::1", "fc00::1", "2001:db8::1", "ff02::1",
	} {
		if publicIP(net.ParseIP(address)) {
			t.Fatalf("publicIP(%s) = true", address)
		}
	}
	if !publicIP(net.ParseIP("8.8.8.8")) || !publicIP(net.ParseIP("2606:4700:4700::1111")) {
		t.Fatal("public global-unicast address was rejected")
	}
}

func TestRegistryHostnameRemovesPortsAndIPv6Brackets(t *testing.T) {
	for input, want := range map[string]string{
		"registry.example:5000": "registry.example",
		"[2001:db8::10]:5000":   "2001:db8::10",
		"registry.example":      "registry.example",
	} {
		if got := registryHostname(input); got != want {
			t.Fatalf("registryHostname(%q) = %q, want %q", input, got, want)
		}
	}
}

func resolver(values map[string][]string) hostResolver {
	return func(_ context.Context, host string) ([]net.IPAddr, error) {
		addresses, ok := values[host]
		if !ok {
			return nil, errors.New("not found")
		}
		result := make([]net.IPAddr, 0, len(addresses))
		for _, address := range addresses {
			result = append(result, net.IPAddr{IP: net.ParseIP(address)})
		}
		return result, nil
	}
}
