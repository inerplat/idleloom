package idleloom

import (
	"strings"
	"testing"
)

// TestCloudInitEmbedsTheSharedPrepareScript keeps the krunkit VM and the
// in-place backends building the same base system. They used to carry
// separate copies of the steps, which could drift apart silently.
func TestCloudInitEmbedsTheSharedPrepareScript(t *testing.T) {
	config := renderCloudInit("worker-a", "ssh-ed25519 AAAA test")
	for _, fragment := range []string{
		"hostname: worker-a",
		"ssh-ed25519 AAAA test",
		"path: /usr/local/sbin/idleloom-prepare",
		"runcmd:",
	} {
		if !strings.Contains(config, fragment) {
			t.Errorf("cloud-init is missing %q", fragment)
		}
	}
	// Every line of the embedded script must be indented under the block
	// scalar, or cloud-init parses the rest of the file as its own keys.
	inBlock := false
	for _, line := range strings.Split(config, "\n") {
		if strings.Contains(line, "content: |") {
			inBlock = true
			continue
		}
		if inBlock {
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "      ") {
				break
			}
			if strings.Contains(line, "apt-get install") {
				inBlock = false
				break
			}
		}
	}
	prepare := renderPrepareScript()
	for _, step := range []string{"swapoff -a", "apt-get install", "SystemdCgroup", "/opt/cni/bin", preparedMarker} {
		if !strings.Contains(prepare, step) {
			t.Errorf("the shared prepare script lost %q", step)
		}
		if !strings.Contains(config, step) {
			t.Errorf("cloud-init does not carry %q from the shared script", step)
		}
	}
	// The sysctl and module files used to be separate write_files entries.
	// The script writes them now, so they must not have been dropped.
	for _, written := range []string{"/etc/modules-load.d/idleloom.conf", "/etc/sysctl.d/99-idleloom-kubernetes.conf"} {
		if !strings.Contains(config, written) {
			t.Errorf("cloud-init no longer provisions %s", written)
		}
	}
}

func TestIndentScriptIndentsEveryNonEmptyLine(t *testing.T) {
	got := indentScript("a\n\nb\n", "  ")
	if got != "  a\n\n  b" {
		t.Errorf("indentScript = %q", got)
	}
}
