package meshclaim

import "strings"

// AgentDaemonSetName is the name every WireKube installation gives its agent
// DaemonSet, whether it came from the Helm chart or from "wirekubectl install".
const AgentDaemonSetName = "wirekube-agent"

// IsAgentDaemonSet reports whether a DaemonSet is the WireKube agent.
//
// Finding it is how Idleloom learns where address claims live, and a wrong
// answer is silent: the claim lands in a namespace nothing else reads, so two
// callers both believe they arbitrated and both take the same address. The
// rule is therefore deliberately narrow, and callers that find no match are
// expected to say so rather than guess.
//
// The exact name is the primary signal because it is the one thing both
// install paths agree on. They label the DaemonSet differently — the chart
// sets app.kubernetes.io/name=wirekube-agent, wirekubectl sets
// app.kubernetes.io/name=wirekube with component=agent — so the labels are
// only a fallback for an installation that renamed the workload.
func IsAgentDaemonSet(name string, labels map[string]string) bool {
	if name == AgentDaemonSetName {
		return true
	}
	if labels["app.kubernetes.io/component"] != "agent" {
		return false
	}
	partOf := labels["app.kubernetes.io/part-of"]
	appName := labels["app.kubernetes.io/name"]
	return strings.HasPrefix(partOf, "wirekube") || strings.HasPrefix(appName, "wirekube")
}
