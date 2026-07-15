package idleloom

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type WorkerCompatibilityReport struct {
	Warnings []string
}

type hostResolver func(context.Context, string) ([]net.IPAddr, error)

func CheckWorkerCompatibility(ctx context.Context, cluster *Cluster) (WorkerCompatibilityReport, error) {
	if cluster == nil || cluster.Client == nil {
		return WorkerCompatibilityReport{}, fmt.Errorf("kubernetes cluster client is required")
	}
	return checkWorkerCompatibility(ctx, cluster.Client, cluster.Server, net.DefaultResolver.LookupIPAddr)
}

func checkWorkerCompatibility(ctx context.Context, client kubernetes.Interface, server string, resolve hostResolver) (WorkerCompatibilityReport, error) {
	if client == nil || resolve == nil {
		return WorkerCompatibilityReport{}, fmt.Errorf("kubernetes client and DNS resolver are required")
	}
	sets, err := client.AppsV1().DaemonSets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return WorkerCompatibilityReport{}, fmt.Errorf("inspect node bootstrap DaemonSets: %w", err)
	}
	report := WorkerCompatibilityReport{}
	registryState := make(map[string]registryReachability)
	for index := range sets.Items {
		set := &sets.Items[index]
		if !daemonSetTargetsLinux(set) {
			continue
		}
		critical := daemonSetInstallsCNI(set)
		for _, image := range daemonSetImages(set) {
			registry := imageRegistry(image)
			if registry == "docker.io" || registry == "ghcr.io" || registry == "gcr.io" || registry == "registry.k8s.io" || strings.HasSuffix(registry, ".pkg.dev") {
				continue
			}
			state, ok := registryState[registry]
			if !ok {
				state = inspectRegistry(ctx, registryHostname(registry), resolve)
				registryState[registry] = state
			}
			if state.public {
				continue
			}
			component := set.Namespace + "/" + set.Name
			message := fmt.Sprintf("DaemonSet %s pulls %s from registry %s, which %s", component, image, registry, state.reason)
			if critical {
				report.Warnings = append(report.Warnings, message+"; host-side DNS cannot prove whether the worker VM can reach this critical CNI registry")
				continue
			}
			report.Warnings = append(report.Warnings, message+"; this add-on may remain unavailable on an external node")
		}
	}
	if endpointHost := serverHostname(server); endpointHost != "" {
		state := inspectRegistry(ctx, endpointHost, resolve)
		if !state.public {
			report.Warnings = append(report.Warnings, fmt.Sprintf("the Kubernetes API endpoint %s %s; verify that the worker VM can reach the same private network through host NAT", endpointHost, state.reason))
		}
	}
	report.Warnings = sortedUnique(report.Warnings)
	return report, nil
}

type registryReachability struct {
	public bool
	reason string
}

func inspectRegistry(ctx context.Context, host string, resolve hostResolver) registryReachability {
	addresses, err := resolve(ctx, host)
	if err != nil {
		return registryReachability{reason: "does not resolve from this host: " + err.Error()}
	}
	if len(addresses) == 0 {
		return registryReachability{reason: "resolves to no addresses"}
	}
	private := make([]string, 0, len(addresses))
	for _, address := range addresses {
		if publicIP(address.IP) {
			return registryReachability{public: true}
		}
		private = append(private, address.IP.String())
	}
	sort.Strings(private)
	return registryReachability{reason: "resolves only to non-public addresses (" + strings.Join(private, ", ") + ")"}
}

func publicIP(address net.IP) bool {
	parsed, ok := netip.AddrFromSlice(address)
	if !ok {
		return false
	}
	parsed = parsed.Unmap()
	if !parsed.IsGlobalUnicast() || parsed.IsPrivate() {
		return false
	}
	for _, prefix := range nonPublicGlobalPrefixes {
		if prefix.Contains(parsed) {
			return false
		}
	}
	return true
}

var nonPublicGlobalPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // shared address space
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),  // documentation
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func daemonSetTargetsLinux(set *appsv1.DaemonSet) bool {
	if set == nil {
		return false
	}
	value, constrained := set.Spec.Template.Spec.NodeSelector[corev1.LabelOSStable]
	return !constrained || value == "linux"
}

func daemonSetInstallsCNI(set *appsv1.DaemonSet) bool {
	if set == nil {
		return false
	}
	for _, volume := range set.Spec.Template.Spec.Volumes {
		if volume.HostPath == nil {
			continue
		}
		path := strings.TrimSuffix(volume.HostPath.Path, "/")
		if path == "/opt/cni/bin" || path == "/etc/cni/net.d" {
			return true
		}
	}
	for _, container := range append(append([]corev1.Container(nil), set.Spec.Template.Spec.InitContainers...), set.Spec.Template.Spec.Containers...) {
		for _, mount := range container.VolumeMounts {
			path := strings.TrimSuffix(mount.MountPath, "/")
			if strings.HasSuffix(path, "/opt/cni/bin") || strings.HasSuffix(path, "/etc/cni/net.d") {
				return true
			}
		}
	}
	return false
}

func daemonSetImages(set *appsv1.DaemonSet) []string {
	seen := make(map[string]struct{})
	var images []string
	for _, container := range append(append([]corev1.Container(nil), set.Spec.Template.Spec.InitContainers...), set.Spec.Template.Spec.Containers...) {
		if container.Image == "" {
			continue
		}
		if _, ok := seen[container.Image]; ok {
			continue
		}
		seen[container.Image] = struct{}{}
		images = append(images, container.Image)
	}
	sort.Strings(images)
	return images
}

func imageRegistry(image string) string {
	image = strings.TrimSpace(image)
	first, _, found := strings.Cut(image, "/")
	if !found || !strings.Contains(first, ".") && !strings.Contains(first, ":") && first != "localhost" {
		return "docker.io"
	}
	return strings.ToLower(first)
}

func registryHostname(registry string) string {
	if host, _, err := net.SplitHostPort(registry); err == nil {
		return host
	}
	return strings.Trim(registry, "[]")
}

func serverHostname(server string) string {
	parsed, err := url.Parse(server)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
