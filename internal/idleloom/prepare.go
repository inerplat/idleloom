package idleloom

import "strings"

// preparedMarker records that the base system has been brought up to the
// state kubelet needs. Create checks for it so a re-run is cheap.
const preparedMarker = "/var/lib/idleloom/.prepared"

// kubernetesModules are loaded before kubelet starts. overlay backs the
// containerd snapshotter and br_netfilter makes bridged traffic visible to
// iptables, which every CNI assumes.
const kubernetesModules = "overlay\nbr_netfilter\n"

// renderPrepareScript returns a POSIX shell script that turns a stock Ubuntu
// system into a Kubernetes worker base: no swap, the required kernel modules
// and sysctls, containerd on the systemd cgroup driver, and the CNI plugin
// directory kubelet looks in.
//
// The krunkit backend runs this through cloud-init at first boot; the
// in-place backends run it directly over the guest executor. Keeping one
// renderer means a worker is built the same way whichever backend created it.
func renderPrepareScript() string {
	return `set -eu

swapoff -a || true
sed -i.bak '/[[:space:]]swap[[:space:]]/d' /etc/fstab

install -d -m 0755 /etc/modules-load.d
cat > /etc/modules-load.d/idleloom.conf <<'IDLELOOM_MODULES'
` + kubernetesModules + `IDLELOOM_MODULES

install -d -m 0755 /etc/sysctl.d
cat > /etc/sysctl.d/99-idleloom-kubernetes.conf <<'IDLELOOM_SYSCTL'
` + kubernetesSysctls + `IDLELOOM_SYSCTL

# A kernel with these built in has nothing to load, and modprobe fails on
# some of those. What matters is that the functionality is there afterwards,
# so check for it rather than trusting the exit status: without br_netfilter
# the bridge sysctls do not exist and no CNI can filter bridged traffic.
modprobe overlay 2>/dev/null || true
modprobe br_netfilter 2>/dev/null || true
if [ ! -e /proc/sys/net/bridge/bridge-nf-call-iptables ]; then
  echo "idleloom: this kernel provides no br_netfilter; bridged Pod traffic would bypass iptables and no CNI can work" >&2
  exit 1
fi
sysctl --system >/dev/null

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends containerd containernetworking-plugins conntrack ebtables ethtool ipset iptables nfs-common open-iscsi socat
apt-get clean

install -d -m 0755 /opt/cni/bin
for plugin in /usr/lib/cni/*; do
  [ -f "$plugin" ] || continue
  ln -sf "$plugin" "/opt/cni/bin/${plugin##*/}"
done

install -d -m 0755 /etc/containerd
if [ ! -s /etc/containerd/config.toml ]; then
  containerd config default > /etc/containerd/config.toml
fi
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
systemctl enable --now containerd.service
systemctl enable --now iscsid.service || true

install -d -m 0755 /var/lib/idleloom
touch ` + preparedMarker + `
`
}

// indentScript prefixes every line so a script can be embedded in a YAML
// block scalar without breaking the surrounding document.
func indentScript(script, indent string) string {
	lines := strings.Split(strings.TrimRight(script, "\n"), "\n")
	for index, line := range lines {
		if line == "" {
			continue
		}
		lines[index] = indent + line
	}
	return strings.Join(lines, "\n")
}
