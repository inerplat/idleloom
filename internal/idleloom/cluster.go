package idleloom

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

type Cluster struct {
	KubeconfigPath string
	Context        string
	Server         string
	TLSServerName  string
	CAData         []byte
	Version        string
	KubeletVersion string
	ClusterDNS     string
	ClusterDomain  string
	RESTConfig     *rest.Config
	Client         kubernetes.Interface
}

func LoadCluster(ctx context.Context, kubeconfigPath, contextName string) (*Cluster, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		expanded, err := expandHome(kubeconfigPath)
		if err != nil {
			return nil, err
		}
		rules.ExplicitPath = expanded
		kubeconfigPath = expanded
	} else {
		kubeconfigPath = clientcmd.RecommendedHomeFile
	}

	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	raw, err := loader.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	if contextName == "" {
		contextName = raw.CurrentContext
	}
	if contextName == "" {
		if len(raw.Contexts) == 0 {
			return nil, fmt.Errorf("no usable kubeconfig found (looked at %s); pass --kubeconfig or set KUBECONFIG", strings.Join(rules.GetLoadingPrecedence(), ", "))
		}
		return nil, fmt.Errorf("the kubeconfig has no current-context; pass --context")
	}
	selectedContext := raw.Contexts[contextName]
	if selectedContext == nil {
		return nil, fmt.Errorf("kubeconfig context %q does not exist", contextName)
	}
	selectedCluster := raw.Clusters[selectedContext.Cluster]
	if selectedCluster == nil {
		return nil, fmt.Errorf("cluster %q referenced by context %q does not exist", selectedContext.Cluster, contextName)
	}

	restConfig, err := loader.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes client configuration: %w", err)
	}
	restConfig.UserAgent = "idleloom/dev"
	client, err := newClient(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	caData := append([]byte(nil), restConfig.CAData...)
	if len(caData) == 0 && restConfig.CAFile != "" {
		caData, err = os.ReadFile(restConfig.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read cluster CA %s: %w", restConfig.CAFile, err)
		}
	}
	if len(caData) == 0 {
		caData, err = discoverClusterCA(ctx, client)
		if err != nil {
			return nil, err
		}
	}
	secureConfig := rest.CopyConfig(restConfig)
	secureConfig.Insecure = false
	secureConfig.CAFile = ""
	secureConfig.CAData = caData
	secureClient, err := newClient(secureConfig)
	if err != nil {
		return nil, fmt.Errorf("create CA-verified Kubernetes client: %w", err)
	}
	version, err := secureClient.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("verify API endpoint %s with the cluster CA: %w", selectedCluster.Server, err)
	}
	client = secureClient
	kubeletVersion, err := normalizeKubernetesVersion(version.GitVersion)
	if err != nil {
		return nil, err
	}

	dns, err := discoverClusterDNS(ctx, client)
	if err != nil {
		return nil, err
	}
	clusterDomain := discoverClusterDomain(ctx, client)

	return &Cluster{
		KubeconfigPath: kubeconfigPath,
		Context:        contextName,
		Server:         selectedCluster.Server,
		TLSServerName:  restConfig.ServerName,
		CAData:         caData,
		Version:        version.GitVersion,
		KubeletVersion: kubeletVersion,
		ClusterDNS:     dns.Spec.ClusterIP,
		ClusterDomain:  clusterDomain,
		RESTConfig:     secureConfig,
		Client:         client,
	}, nil
}

// newClient builds a clientset that retries a request once after the API server
// rejects the credential.
//
// Exec credential plugins that omit expirationTimestamp from the ExecCredential
// leave client-go with a credential it considers valid forever, so it keeps
// sending the cached token after the server has stopped accepting it. client-go
// does re-run the plugin when it sees the resulting Unauthorized, but it returns
// that Unauthorized to the caller rather than retrying. kubectl never notices
// because every invocation is a fresh process; a single idlectl command that
// outlives the token fails instead. Retrying once picks up the credential that
// client-go has by then refreshed.
func newClient(config *rest.Config) (kubernetes.Interface, error) {
	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, err
	}
	if config.ExecProvider != nil {
		httpClient.Transport = &retryUnauthorizedTransport{base: httpClient.Transport}
	}
	return kubernetes.NewForConfigAndClient(config, httpClient)
}

// retryUnauthorizedTransport must wrap the transport client-go has already
// assembled, so that the credential refresh triggered by the first Unauthorized
// is in place before the retry re-enters the exec round tripper below it.
type retryUnauthorizedTransport struct {
	base http.RoundTripper
}

func (t *retryUnauthorizedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		return response, err
	}
	// A request whose body cannot be rewound can only be sent once.
	if req.Body != nil && req.GetBody == nil {
		return response, nil
	}
	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return response, nil
		}
		retry.Body = body
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return t.base.RoundTrip(retry)
}

func discoverClusterDNS(ctx context.Context, client kubernetes.Interface) (*corev1.Service, error) {
	services := client.CoreV1().Services("kube-system")
	for _, name := range []string{"kube-dns", "coredns"} {
		service, err := services.Get(ctx, name, metav1.GetOptions{})
		if err == nil && usableClusterIP(service) {
			return service, nil
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("discover cluster DNS Service %s: %w", name, err)
		}
	}
	list, err := services.List(ctx, metav1.ListOptions{LabelSelector: "k8s-app=kube-dns"})
	if err != nil {
		return nil, fmt.Errorf("discover cluster DNS Service by label: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	for index := range list.Items {
		if usableClusterIP(&list.Items[index]) {
			return list.Items[index].DeepCopy(), nil
		}
	}
	return nil, fmt.Errorf("discover cluster DNS Service: kube-system has no kube-dns/coredns Service with a usable ClusterIP")
}

func usableClusterIP(service *corev1.Service) bool {
	return service != nil && service.Spec.ClusterIP != "" && service.Spec.ClusterIP != corev1.ClusterIPNone
}

var upstreamKubernetesVersionPattern = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)(?:[-+].*)?$`)

func normalizeKubernetesVersion(gitVersion string) (string, error) {
	match := upstreamKubernetesVersionPattern.FindStringSubmatch(gitVersion)
	if len(match) != 4 {
		return "", fmt.Errorf("unsupported Kubernetes GitVersion %q", gitVersion)
	}
	return "v" + match[1] + "." + match[2] + "." + match[3], nil
}

func discoverClusterCA(ctx context.Context, client kubernetes.Interface) ([]byte, error) {
	cm, err := client.CoreV1().ConfigMaps("default").Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
	if err == nil && strings.TrimSpace(cm.Data["ca.crt"]) != "" {
		return []byte(cm.Data["ca.crt"]), nil
	}

	clusterInfo, clusterInfoErr := client.CoreV1().ConfigMaps("kube-public").Get(ctx, "cluster-info", metav1.GetOptions{})
	if clusterInfoErr == nil {
		var config struct {
			Clusters []struct {
				Cluster struct {
					CertificateAuthorityData string `yaml:"certificate-authority-data"`
				} `yaml:"cluster"`
			} `yaml:"clusters"`
		}
		if parseErr := yaml.Unmarshal([]byte(clusterInfo.Data["kubeconfig"]), &config); parseErr == nil && len(config.Clusters) > 0 {
			decoded, decodeErr := base64.StdEncoding.DecodeString(config.Clusters[0].Cluster.CertificateAuthorityData)
			if decodeErr == nil && len(decoded) > 0 {
				return decoded, nil
			}
		}
	}

	return nil, errors.New("cluster CA is unavailable; configure certificate-authority-data or expose kube-root-ca.crt")
}

func discoverClusterDomain(ctx context.Context, client kubernetes.Interface) string {
	cm, err := client.CoreV1().ConfigMaps("kube-system").Get(ctx, "kubelet-config", metav1.GetOptions{})
	if err != nil {
		return "cluster.local"
	}
	var cfg struct {
		ClusterDomain string `yaml:"clusterDomain"`
	}
	if err := yaml.Unmarshal([]byte(cm.Data["kubelet"]), &cfg); err != nil || cfg.ClusterDomain == "" || len(validation.IsDNS1123Subdomain(cfg.ClusterDomain)) > 0 {
		return "cluster.local"
	}
	return cfg.ClusterDomain
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return filepath.Abs(path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
