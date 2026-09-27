package meshclaim

import "testing"

// TestIsAgentDaemonSet covers both installation paths, which label the
// DaemonSet differently, and the workloads that must not be mistaken for it.
// A false positive sends claims to a namespace nothing else reads, where they
// arbitrate against nothing.
func TestIsAgentDaemonSet(t *testing.T) {
	for _, c := range []struct {
		what   string
		name   string
		labels map[string]string
		want   bool
	}{
		{"wirekubectl install", "wirekube-agent", map[string]string{
			"app.kubernetes.io/name": "wirekube", "app.kubernetes.io/component": "agent",
			"app.kubernetes.io/part-of": "wirekube",
		}, true},
		{"helm chart", "wirekube-agent", map[string]string{
			"app": "wirekube-agent", "app.kubernetes.io/name": "wirekube-agent",
			"app.kubernetes.io/component": "agent",
		}, true},
		{"renamed but labelled", "mesh-agent", map[string]string{
			"app.kubernetes.io/name": "wirekube", "app.kubernetes.io/component": "agent",
		}, true},
		{"the relay", "wirekube-relay", map[string]string{
			"app.kubernetes.io/name": "wirekube-relay", "app.kubernetes.io/part-of": "wirekube",
		}, false},
		{"somebody's debug copy", "my-wirekube-debug", map[string]string{}, false},
		{"an unrelated agent", "datadog-agent", map[string]string{
			"app.kubernetes.io/component": "agent", "app.kubernetes.io/name": "datadog",
		}, false},
		{"nothing at all", "nginx", nil, false},
	} {
		if got := IsAgentDaemonSet(c.name, c.labels); got != c.want {
			t.Errorf("%s: IsAgentDaemonSet(%q) = %v, want %v", c.what, c.name, got, c.want)
		}
	}
}
