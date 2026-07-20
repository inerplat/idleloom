package idleloom

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type BundleConfig struct {
	NodeName      string
	Taint         string
	Server        string
	TLSServerName string
	CAData        []byte
	Token         string
	ClusterDNS    string
	ClusterDomain string
	KubeletPath   string
	// RegistryMirrors are resolved containerd certs.d mirror entries.
	RegistryMirrors []RegistryMirror
	// CredentialProviderBins are host paths to linux/arm64 provider binaries.
	CredentialProviderBins []string
	// CredentialProviderConfig is the host path to a kubelet
	// CredentialProviderConfig YAML, or "" when none is configured.
	CredentialProviderConfig string
	// CredentialProviderEnv is the host path to an optional KEY=VALUE env file
	// for the credential providers, or "" when none is configured.
	CredentialProviderEnv string
}

func CreateWorkerBundle(cfg BundleConfig) (string, func(), error) {
	temporaryDir, err := os.MkdirTemp("", "idleloom-bundle-*")
	if err != nil {
		return "", nil, fmt.Errorf("create worker bundle directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(temporaryDir) }
	bundlePath := filepath.Join(temporaryDir, "idleloom-bundle.tar")
	file, err := os.OpenFile(bundlePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("create worker bundle: %w", err)
	}
	tw := tar.NewWriter(file)
	fail := func(cause error) (string, func(), error) {
		closeErr := tw.Close()
		fileErr := file.Close()
		cleanup()
		return "", nil, errors.Join(cause, closeErr, fileErr)
	}

	bootstrapConfig, err := renderBootstrapKubeconfig(cfg)
	if err != nil {
		return fail(err)
	}
	entries := []struct {
		Name string
		Mode int64
		Data []byte
	}{
		{Name: "bootstrap-kubelet.conf", Mode: 0o600, Data: bootstrapConfig},
		{Name: "ca.crt", Mode: 0o644, Data: cfg.CAData},
		{Name: "config.yaml", Mode: 0o644, Data: []byte(renderKubeletConfig(cfg))},
		{Name: "kubelet.service", Mode: 0o644, Data: []byte(kubeletService)},
		{Name: "install.sh", Mode: 0o755, Data: []byte(renderInstallScript(cfg))},
		{Name: "k8s-sysctl.conf", Mode: 0o644, Data: []byte(kubernetesSysctls)},
	}
	for _, entry := range entries {
		if err := writeTarBytes(tw, entry.Name, entry.Mode, entry.Data); err != nil {
			return fail(err)
		}
	}
	if err := writeTarFile(tw, "bin/kubelet", 0o755, cfg.KubeletPath); err != nil {
		return fail(err)
	}
	for _, mirror := range cfg.RegistryMirrors {
		name := "certs.d/" + mirror.Host + "/hosts.toml"
		if err := writeTarBytes(tw, name, 0o644, []byte(renderHostsTOML(mirror))); err != nil {
			return fail(err)
		}
	}
	for _, bin := range cfg.CredentialProviderBins {
		name := "credential-providers/" + filepath.Base(bin)
		if err := writeTarFile(tw, name, 0o755, bin); err != nil {
			return fail(err)
		}
	}
	if cfg.CredentialProviderConfig != "" {
		if err := writeTarFile(tw, "credential-providers.yaml", 0o644, cfg.CredentialProviderConfig); err != nil {
			return fail(err)
		}
	}
	if cfg.CredentialProviderEnv != "" {
		if err := writeTarFile(tw, "credential-providers.env", 0o600, cfg.CredentialProviderEnv); err != nil {
			return fail(err)
		}
	}
	if err := tw.Close(); err != nil {
		closeErr := file.Close()
		cleanup()
		return "", nil, errors.Join(fmt.Errorf("finish worker bundle: %w", err), closeErr)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close worker bundle: %w", err)
	}
	return bundlePath, cleanup, nil
}

func renderBootstrapKubeconfig(cfg BundleConfig) ([]byte, error) {
	config := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"cluster": {Server: cfg.Server, TLSServerName: cfg.TLSServerName, CertificateAuthorityData: cfg.CAData},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"kubelet-bootstrap": {Token: cfg.Token},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"bootstrap": {Cluster: "cluster", AuthInfo: "kubelet-bootstrap"},
		},
		CurrentContext: "bootstrap",
	}
	data, err := clientcmd.Write(config)
	if err != nil {
		return nil, fmt.Errorf("render bootstrap kubeconfig: %w", err)
	}
	return data, nil
}

func renderKubeletConfig(cfg BundleConfig) string {
	return fmt.Sprintf(`apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
authentication:
  anonymous:
    enabled: false
  webhook:
    enabled: true
  x509:
    clientCAFile: /var/lib/idleloom/config/ca.crt
authorization:
  mode: Webhook
cgroupDriver: systemd
clusterDNS:
  - %s
clusterDomain: %s
containerRuntimeEndpoint: unix:///run/containerd/containerd.sock
failSwapOn: true
rotateCertificates: true
serverTLSBootstrap: true
resolvConf: /run/systemd/resolve/resolv.conf
staticPodPath: /etc/kubernetes/manifests
`, cfg.ClusterDNS, cfg.ClusterDomain)
}

const kubeletService = `[Unit]
Description=Idleloom Kubernetes Worker
Documentation=https://kubernetes.io/docs/reference/command-line-tools-reference/kubelet/
Wants=network-online.target
After=network-online.target containerd.service
Requires=containerd.service

[Service]
EnvironmentFile=-/etc/default/idleloom-kubelet
EnvironmentFile=-/etc/default/idleloom-credential-providers
ExecStart=/var/lib/idleloom/bin/kubelet \
  --bootstrap-kubeconfig=/var/lib/idleloom/config/bootstrap-kubelet.conf \
  --kubeconfig=/var/lib/kubelet/kubeconfig \
  --config=/var/lib/kubelet/config.yaml \
  $KUBELET_EXTRA_ARGS
Restart=always
RestartSec=10
StartLimitInterval=0

[Install]
WantedBy=multi-user.target
`

func renderInstallScript(cfg BundleConfig) string {
	extraArgs := "--node-ip=${node_ip} --hostname-override=" + cfg.NodeName
	if cfg.Taint != "" {
		extraArgs += " --register-with-taints=" + cfg.Taint
	}
	if cfg.CredentialProviderConfig != "" {
		extraArgs += " --image-credential-provider-config=/var/lib/idleloom/config/credential-providers.yaml" +
			" --image-credential-provider-bin-dir=/var/lib/idleloom/credential-providers"
	}
	return fmt.Sprintf(`#!/bin/sh
set -eu

base=/var/lib/idleloom/config

/usr/bin/install -d -m 0755 /var/lib/idleloom/bin
/usr/bin/install -m 0755 "$base/bin/kubelet" /var/lib/idleloom/bin/kubelet
/usr/bin/rm -f "$base/bin/kubelet"
/usr/bin/install -d -m 0755 /etc/default /etc/kubernetes/manifests /etc/systemd/system /var/lib/kubelet
/usr/bin/install -m 0644 "$base/config.yaml" /var/lib/kubelet/config.yaml
/usr/bin/install -m 0644 "$base/kubelet.service" /etc/systemd/system/kubelet.service
/usr/bin/install -m 0644 "$base/k8s-sysctl.conf" /etc/sysctl.d/99-idleloom-kubernetes.conf

/usr/bin/modprobe br_netfilter 2>/dev/null || true
/usr/sbin/sysctl --system >/dev/null

if [ ! -s /etc/containerd/config.toml ]; then
    /usr/bin/containerd config default > /etc/containerd/config.toml
fi
if /usr/bin/grep -q 'SystemdCgroup = false' /etc/containerd/config.toml; then
    /usr/bin/sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
fi

if [ -d "$base/certs.d" ]; then
    /usr/bin/install -d -m 0755 /etc/containerd/certs.d
    for host_dir in "$base"/certs.d/*/; do
        [ -d "$host_dir" ] || continue
        host=$(/usr/bin/basename "$host_dir")
        /usr/bin/install -d -m 0755 "/etc/containerd/certs.d/$host"
        /usr/bin/install -m 0644 "$host_dir/hosts.toml" "/etc/containerd/certs.d/$host/hosts.toml"
    done
    /usr/bin/sed -i "s#config_path = ''#config_path = '/etc/containerd/certs.d'#" /etc/containerd/config.toml
    /usr/bin/sed -i "s#config_path = \"\"#config_path = '/etc/containerd/certs.d'#" /etc/containerd/config.toml
    if ! /usr/bin/grep -q "config_path = '/etc/containerd/certs.d'" /etc/containerd/config.toml; then
        echo "idleloom: could not set containerd registry config_path; the requested registry mirror would be silently ignored" >&2
        exit 1
    fi
fi

if [ -d "$base/credential-providers" ]; then
    /usr/bin/install -d -m 0755 /var/lib/idleloom/credential-providers
    for provider_bin in "$base"/credential-providers/*; do
        [ -f "$provider_bin" ] || continue
        /usr/bin/install -m 0755 "$provider_bin" "/var/lib/idleloom/credential-providers/$(/usr/bin/basename "$provider_bin")"
    done
fi
if [ -f "$base/credential-providers.yaml" ]; then
    /usr/bin/chmod 0644 "$base/credential-providers.yaml"
fi
if [ -f "$base/credential-providers.env" ]; then
    /usr/bin/install -m 0600 "$base/credential-providers.env" /etc/default/idleloom-credential-providers
    /usr/bin/rm -f "$base/credential-providers.env"
fi

node_ip=$(/usr/sbin/ip -4 route get 1.1.1.1 | /usr/bin/awk '{for (i=1; i<=NF; i++) if ($i == "src") {print $(i+1); exit}}')
if [ -z "$node_ip" ]; then
    echo "idleloom: could not determine the worker node IP" >&2
    exit 1
fi
cat > /etc/default/idleloom-kubelet <<EOF
KUBELET_EXTRA_ARGS="%s"
EOF

/usr/bin/systemctl daemon-reload
/usr/bin/systemctl enable containerd.service kubelet.service >/dev/null
/usr/bin/systemctl restart containerd.service
/usr/bin/systemctl restart kubelet.service
`, extraArgs)
}

const kubernetesSysctls = `net.ipv4.ip_forward = 1
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
`

func writeTarBytes(tw *tar.Writer, name string, mode int64, data []byte) error {
	header := &tar.Header{Name: name, Mode: mode, Size: int64(len(data))}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("write %s header: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func writeTarFile(tw *tar.Writer, name string, mode int64, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: info.Size()}); err != nil {
		return fmt.Errorf("write %s header: %w", name, err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}
