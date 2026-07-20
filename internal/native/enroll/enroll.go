package enroll

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/fencing"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	"github.com/inerplat/idleloom/internal/native/kubeletbridge"
	nativewirekube "github.com/inerplat/idleloom/internal/native/wirekube"
	authenticationv1 "k8s.io/api/authentication/v1"
	certificatesv1 "k8s.io/api/certificates/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const managedBy = "idleloom"

var invalidDNS = regexp.MustCompile(`[^a-z0-9-]+`)

type Config struct {
	REST           *rest.Config
	Dynamic        dynamic.Interface
	Kubernetes     kubernetes.Interface
	HostID         string
	StateDirectory string
	TokenDuration  time.Duration
	Connectivity   string
	ShellAccess    string
}

type Result struct {
	Namespace            string
	AgentID              string
	ControllerKubeconfig string
	AgentKubeconfig      string
	LinkKubeconfig       string
	ExpiresAt            time.Time
	Connectivity         string
	WireKubePeer         string
	WireKubeAddress      string
	ShellAccess          string
}

type enrollmentIntent struct {
	Version int    `json:"version"`
	HostID  string `json:"hostID"`
	Nonce   string `json:"nonce"`
}

type EnrollmentIdentity struct {
	HostID string
	Nonce  string
}

type clusterTrust struct {
	Version    int    `json:"version"`
	Endpoint   string `json:"endpoint"`
	SPKISHA256 string `json:"spkiSHA256"`
}

func WriteServiceKubeconfig(ctx context.Context, restConfig *rest.Config, client kubernetes.Interface, namespace, serviceAccount, path, user string, duration time.Duration) (time.Time, error) {
	if restConfig == nil || client == nil || namespace == "" || serviceAccount == "" || path == "" || user == "" {
		return time.Time{}, fmt.Errorf("the REST config, client, namespace, service account, path, and user are required")
	}
	credential, expires, err := token(ctx, client, namespace, serviceAccount, duration)
	if err != nil {
		return time.Time{}, err
	}
	if err := writeKubeconfig(path, restConfig, credential, user); err != nil {
		return time.Time{}, err
	}
	return expires, nil
}

func DefaultStateDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "idleloom", "native"), nil
}

// PinServerCertificate converts an explicitly insecure development config
// into a TLS-verifying config by trusting the certificate observed on the
// current connection. Callers must require an explicit TOFU opt-in.
func PinServerCertificate(ctx context.Context, source *rest.Config, stateDirectory string, reset bool) (*rest.Config, error) {
	if source == nil || source.Host == "" {
		return nil, fmt.Errorf("kubernetes API endpoint is required")
	}
	parsed, err := url.Parse(source.Host)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return nil, fmt.Errorf("kubernetes API endpoint must be an HTTPS URL")
	}
	address := parsed.Host
	if parsed.Port() == "" {
		address = net.JoinHostPort(parsed.Hostname(), "443")
	}
	dialer := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("observe Kubernetes API certificate: %w", err)
	}
	defer func() { _ = connection.Close() }()
	tlsConnection, ok := connection.(*tls.Conn)
	if !ok || len(tlsConnection.ConnectionState().PeerCertificates) == 0 {
		return nil, fmt.Errorf("the Kubernetes API returned no TLS certificate")
	}
	certificate := tlsConnection.ConnectionState().PeerCertificates[0]
	spki := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	trust := clusterTrust{Version: 1, Endpoint: source.Host, SPKISHA256: hex.EncodeToString(spki[:])}
	if err := persistClusterTrust(stateDirectory, trust, reset); err != nil {
		return nil, err
	}
	copy := rest.CopyConfig(source)
	copy.Insecure = false
	copy.CAFile = ""
	copy.CAData = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	if copy.ServerName == "" {
		copy.ServerName = parsed.Hostname()
	}
	return copy, nil
}

func persistClusterTrust(directory string, observed clusterTrust, reset bool) error {
	if directory == "" {
		return fmt.Errorf("state directory is required for TOFU certificate pinning")
	}
	path := filepath.Join(directory, "cluster-trust.json")
	data, err := os.ReadFile(path)
	if err == nil && !reset {
		var trusted clusterTrust
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&trusted); err != nil {
			return fmt.Errorf("decode persisted cluster trust: %w", err)
		}
		if trusted.Version != 1 || trusted.Endpoint != observed.Endpoint || trusted.SPKISHA256 != observed.SPKISHA256 {
			return fmt.Errorf("the Kubernetes API identity changed; verify the new certificate and rerun with --reset-trust")
		}
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	data, err = json.MarshalIndent(observed, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, append(data, '\n'))
}

func Run(ctx context.Context, config Config) (Result, error) {
	if config.REST == nil || config.Dynamic == nil || config.Kubernetes == nil {
		return Result{}, fmt.Errorf("the REST, dynamic, and Kubernetes clients are required")
	}
	hostID := NormalizeHostID(config.HostID)
	if hostID == "" {
		return Result{}, fmt.Errorf("host ID must contain a letter or digit")
	}
	connectivity := config.Connectivity
	if connectivity == "" {
		connectivity = nativewirekube.ConnectivityAPIOnly
	}
	if connectivity != nativewirekube.ConnectivityAPIOnly && connectivity != nativewirekube.ConnectivityWireKube {
		return Result{}, fmt.Errorf("connectivity must be %q or %q", nativewirekube.ConnectivityAPIOnly, nativewirekube.ConnectivityWireKube)
	}
	shellAccess := config.ShellAccess
	if shellAccess == "" {
		shellAccess = nativev1alpha1.ShellAccessDisabled
	}
	if shellAccess != nativev1alpha1.ShellAccessDisabled && shellAccess != nativev1alpha1.ShellAccessSandboxed && shellAccess != nativev1alpha1.ShellAccessHost {
		return Result{}, fmt.Errorf("shell access must be %q, %q, or %q", nativev1alpha1.ShellAccessDisabled, nativev1alpha1.ShellAccessSandboxed, nativev1alpha1.ShellAccessHost)
	}
	stateDirectory := config.StateDirectory
	if stateDirectory == "" {
		var err error
		stateDirectory, err = DefaultStateDirectory()
		if err != nil {
			return Result{}, err
		}
	}
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return Result{}, err
	}
	if err := os.Chmod(stateDirectory, 0o700); err != nil {
		return Result{}, err
	}
	if connectivity == nativewirekube.ConnectivityAPIOnly {
		hasWireKubeState, err := nativewirekube.HasState(stateDirectory)
		if err != nil {
			return Result{}, err
		}
		if hasWireKubeState {
			return Result{}, fmt.Errorf("this Mac still has WireKube link state from a previous join; run \"idlectl delete host %s\" before rejoining with --link api-only", hostID)
		}
	}
	intent, err := loadOrCreateIntent(stateDirectory, hostID)
	if err != nil {
		return Result{}, err
	}
	namespace := "idleloom-host-" + hostID
	agentID := hostID + ".native"
	labels := map[string]string{"app.kubernetes.io/managed-by": managedBy, "ai.idleloom.io/host-id": hostID}
	annotations := map[string]string{"ai.idleloom.io/enrollment-id": intent.Nonce}
	if err := ensureNamespace(ctx, config.Kubernetes, namespace, labels, annotations); err != nil {
		return Result{}, err
	}
	if err := ensureServiceAccount(ctx, config.Kubernetes, namespace, "idleloom-agent", labels, annotations); err != nil {
		return Result{}, err
	}
	if err := ensureAgentRBAC(ctx, config.Kubernetes, namespace, labels, annotations); err != nil {
		return Result{}, err
	}
	host, err := ensureHost(ctx, config.Dynamic, namespace, agentID, shellAccess, labels, annotations)
	if err != nil {
		return Result{}, err
	}
	if err := ensureFencingLease(ctx, config.Kubernetes, namespace, string(host.UID), intent.Nonce); err != nil {
		return Result{}, err
	}
	var wireKubeState nativewirekube.State
	if connectivity == nativewirekube.ConnectivityWireKube {
		wireKubeState, err = nativewirekube.Enroll(ctx, nativewirekube.EnrollConfig{
			Dynamic: config.Dynamic, Kubernetes: config.Kubernetes, REST: config.REST,
			HostID: hostID, EnrollmentID: intent.Nonce, Namespace: namespace,
			StateDirectory: stateDirectory, APIEndpoint: config.REST.Host,
			TokenDuration: config.TokenDuration, WaitTimeout: time.Minute,
		})
		if err != nil {
			return Result{}, fmt.Errorf("enroll WireKube connected leaf: %w", err)
		}
		clientCA, err := kubernetesClientCA(ctx, config.Kubernetes)
		if err != nil {
			return Result{}, err
		}
		if _, err := kubeletbridge.EnsureClientCA(stateDirectory, clientCA); err != nil {
			return Result{}, fmt.Errorf("persist Kubernetes client CA for kubelet logs bridge: %w", err)
		}
		requester := func(ctx context.Context, request []byte) ([]byte, error) {
			return requestKubeletServingCertificate(ctx, config.Kubernetes, hostID, request)
		}
		if _, err := kubeletbridge.EnsureServingIdentity(ctx, stateDirectory, wireKubeState.AssignedMeshIP, "idleloom-"+hostID, time.Now().UTC(), requester); err != nil {
			return Result{}, fmt.Errorf("prepare Kubernetes-signed kubelet logs bridge identity: %w", err)
		}
	}
	duration := config.TokenDuration
	if duration <= 0 || duration > 24*time.Hour {
		duration = 8 * time.Hour
	}
	controllerToken, controllerExpiry, err := token(ctx, config.Kubernetes, "idleloom-system", "idleloom-controller", duration)
	if err != nil {
		return Result{}, fmt.Errorf("create restricted controller token: %w", err)
	}
	agentToken, agentExpiry, err := token(ctx, config.Kubernetes, namespace, "idleloom-agent", duration)
	if err != nil {
		return Result{}, fmt.Errorf("create restricted agent token: %w", err)
	}
	controllerPath := filepath.Join(stateDirectory, "controller.kubeconfig")
	agentPath := filepath.Join(stateDirectory, "agent.kubeconfig")
	if err := writeKubeconfig(controllerPath, config.REST, controllerToken, "idleloom-controller"); err != nil {
		return Result{}, err
	}
	if err := writeKubeconfig(agentPath, config.REST, agentToken, agentID); err != nil {
		return Result{}, err
	}
	expires := controllerExpiry
	if agentExpiry.Before(expires) {
		expires = agentExpiry
	}
	return Result{
		Namespace: namespace, AgentID: agentID, ControllerKubeconfig: controllerPath,
		AgentKubeconfig: agentPath, LinkKubeconfig: wireKubeState.LinkKubeconfig,
		ExpiresAt: expires, Connectivity: connectivity,
		WireKubePeer: wireKubeState.PeerName, WireKubeAddress: wireKubeState.AssignedMeshIP,
		ShellAccess: shellAccess,
	}, nil
}

func kubernetesClientCA(ctx context.Context, client kubernetes.Interface) ([]byte, error) {
	config, err := client.CoreV1().ConfigMaps("kube-system").Get(ctx, "extension-apiserver-authentication", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes client CA for kubelet logs bridge: %w", err)
	}
	certificate := []byte(config.Data["client-ca-file"])
	if len(certificate) == 0 {
		return nil, fmt.Errorf("the Kubernetes client CA for kubelet logs bridge is empty")
	}
	return certificate, nil
}

func requestKubeletServingCertificate(ctx context.Context, client kubernetes.Interface, hostID string, request []byte) ([]byte, error) {
	expiration := int32((30 * 24 * time.Hour) / time.Second)
	csr, err := client.CertificatesV1().CertificateSigningRequests().Create(ctx, &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "idleloom-" + hostID + "-", Labels: map[string]string{"app.kubernetes.io/managed-by": managedBy}},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Request: request, SignerName: certificatesv1.KubeletServingSignerName,
			ExpirationSeconds: &expiration,
			Usages: []certificatesv1.KeyUsage{
				certificatesv1.UsageDigitalSignature,
				certificatesv1.UsageKeyEncipherment,
				certificatesv1.UsageServerAuth,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create kubelet serving CSR: %w", err)
	}
	defer func() {
		_ = client.CertificatesV1().CertificateSigningRequests().Delete(context.Background(), csr.Name, metav1.DeleteOptions{})
	}()
	approved := csr.DeepCopy()
	approved.Status.Conditions = append(approved.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type: certificatesv1.CertificateApproved, Status: corev1.ConditionTrue,
		Reason: "IdleloomEnrollment", Message: "approved by the Idleloom host enrollment flow",
		LastUpdateTime: metav1.Now(), LastTransitionTime: metav1.Now(),
	})
	if _, err := client.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, approved.Name, approved, metav1.UpdateOptions{}); err != nil {
		return nil, fmt.Errorf("approve kubelet serving CSR: %w", err)
	}
	var certificate []byte
	err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
		current, err := client.CertificatesV1().CertificateSigningRequests().Get(ctx, csr.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		for _, condition := range current.Status.Conditions {
			if condition.Type == certificatesv1.CertificateDenied || condition.Type == certificatesv1.CertificateFailed {
				return false, fmt.Errorf("kubelet serving CSR was %s: %s", condition.Type, condition.Message)
			}
		}
		if len(current.Status.Certificate) == 0 {
			return false, nil
		}
		certificate = append([]byte(nil), current.Status.Certificate...)
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("wait for kubelet serving certificate: %w", err)
	}
	return certificate, nil
}

func ensureNamespace(ctx context.Context, client kubernetes.Interface, name string, labels, annotations map[string]string) error {
	existing, err := client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations}}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if existing.Labels["app.kubernetes.io/managed-by"] != managedBy || existing.Labels["ai.idleloom.io/host-id"] != labels["ai.idleloom.io/host-id"] || existing.Annotations["ai.idleloom.io/enrollment-id"] != annotations["ai.idleloom.io/enrollment-id"] {
		return fmt.Errorf("namespace %s already exists and is not owned by this enrollment", name)
	}
	return nil
}

func ensureServiceAccount(ctx context.Context, client kubernetes.Interface, namespace, name string, labels, annotations map[string]string) error {
	account, err := client.CoreV1().ServiceAccounts(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels, Annotations: annotations}}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if account.Labels["app.kubernetes.io/managed-by"] != managedBy || account.Annotations["ai.idleloom.io/enrollment-id"] != annotations["ai.idleloom.io/enrollment-id"] {
		return fmt.Errorf("service account %s/%s is not owned by this enrollment", namespace, name)
	}
	return nil
}

func ensureAgentRBAC(ctx context.Context, client kubernetes.Interface, namespace string, labels, annotations map[string]string) error {
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "idleloom-agent", Namespace: namespace, Labels: labels, Annotations: annotations},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"ai.idleloom.io"}, Resources: []string{"idleloomhosts"}, ResourceNames: []string{"host"}, Verbs: []string{"get"}},
			{APIGroups: []string{"ai.idleloom.io"}, Resources: []string{"idleloomhosts/status"}, ResourceNames: []string{"host"}, Verbs: []string{"get", "patch", "update"}},
			{APIGroups: []string{"ai.idleloom.io"}, Resources: []string{"idleloomworkloadassignments"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"ai.idleloom.io"}, Resources: []string{"idleloomworkloadassignments/status"}, ResourceNames: []string{"active"}, Verbs: []string{"get", "patch", "update"}},
			{APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"}, ResourceNames: []string{"idleloom-agent"}, Verbs: []string{"create"}},
			{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{nativev1alpha1.ServingAuthSecretName}, Verbs: []string{"get"}},
		},
	}
	roles := client.RbacV1().Roles(namespace)
	existing, err := roles.Get(ctx, role.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := roles.Create(ctx, role, metav1.CreateOptions{}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		if existing.Labels["app.kubernetes.io/managed-by"] != managedBy || existing.Annotations["ai.idleloom.io/enrollment-id"] != annotations["ai.idleloom.io/enrollment-id"] {
			return fmt.Errorf("role %s/%s is not owned by this enrollment", namespace, role.Name)
		}
		role.ResourceVersion = existing.ResourceVersion
		if _, err := roles.Update(ctx, role, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "idleloom-agent", Namespace: namespace, Labels: labels, Annotations: annotations},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "idleloom-agent"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "idleloom-agent", Namespace: namespace}},
	}
	bindings := client.RbacV1().RoleBindings(namespace)
	existingBinding, err := bindings.Get(ctx, binding.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = bindings.Create(ctx, binding, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if existingBinding.Labels["app.kubernetes.io/managed-by"] != managedBy || existingBinding.Annotations["ai.idleloom.io/enrollment-id"] != annotations["ai.idleloom.io/enrollment-id"] {
		return fmt.Errorf("role binding %s/%s is not owned by this enrollment", namespace, binding.Name)
	}
	binding.ResourceVersion = existingBinding.ResourceVersion
	_, err = bindings.Update(ctx, binding, metav1.UpdateOptions{})
	return err
}

func ensureHost(ctx context.Context, client dynamic.Interface, namespace, agentID, shellAccess string, labels, annotations map[string]string) (*nativev1alpha1.IdleloomHost, error) {
	resource := client.Resource(nativekube.HostsGVR).Namespace(namespace)
	object, err := resource.Get(ctx, "host", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		host := &nativev1alpha1.IdleloomHost{
			TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomHost"},
			ObjectMeta: metav1.ObjectMeta{Name: "host", Namespace: namespace, Labels: labels, Annotations: annotations},
			Spec:       nativev1alpha1.IdleloomHostSpec{AgentID: agentID, ShellAccess: shellAccess},
		}
		unstructured, conversionErr := nativekube.ToUnstructured(host)
		if conversionErr != nil {
			return nil, conversionErr
		}
		object, err = resource.Create(ctx, unstructured, metav1.CreateOptions{})
		if apierrors.IsNotFound(err) {
			// A newly established CRD can persist the first object while discovery
			// propagation still returns a transient NotFound to the client.
			err = wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
				created, getErr := resource.Get(ctx, "host", metav1.GetOptions{})
				if getErr == nil {
					object = created
					return true, nil
				}
				if apierrors.IsNotFound(getErr) {
					return false, nil
				}
				return false, getErr
			})
			if err != nil {
				err = fmt.Errorf("wait for created host mailbox: %w", err)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	var host nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(object, &host); err != nil {
		return nil, err
	}
	if host.Labels["app.kubernetes.io/managed-by"] != managedBy || host.Annotations["ai.idleloom.io/enrollment-id"] != annotations["ai.idleloom.io/enrollment-id"] || host.Spec.AgentID != agentID || effectiveShellAccess(host.Spec.ShellAccess) != shellAccess {
		return nil, fmt.Errorf("host mailbox %s/host is not owned by this enrollment", namespace)
	}
	return &host, nil
}

func effectiveShellAccess(access string) string {
	if access == "" {
		return nativev1alpha1.ShellAccessDisabled
	}
	return access
}

func ensureFencingLease(ctx context.Context, client kubernetes.Interface, namespace, hostUID, enrollmentID string) error {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fencing.LeaseName,
			Namespace: namespace,
			Labels:    map[string]string{fencing.ManagedByLabel: fencing.ManagedByValue, "app.kubernetes.io/part-of": "idleloom"},
			Annotations: map[string]string{
				fencing.EpochAnnotation:        "0",
				fencing.HostUIDAnnotation:      hostUID,
				"ai.idleloom.io/enrollment-id": enrollmentID,
			},
		},
	}
	leases := client.CoordinationV1().Leases(namespace)
	existing, err := leases.Get(ctx, lease.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = leases.Create(ctx, lease, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if existing.Labels[fencing.ManagedByLabel] != fencing.ManagedByValue || existing.Annotations[fencing.HostUIDAnnotation] != hostUID || existing.Annotations["ai.idleloom.io/enrollment-id"] != enrollmentID {
		return fmt.Errorf("fencing Lease %s/%s belongs to another host", namespace, lease.Name)
	}
	return nil
}

func loadOrCreateIntent(directory, hostID string) (enrollmentIntent, error) {
	path := filepath.Join(directory, "enrollment-intent.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var intent enrollmentIntent
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&intent); err != nil || intent.Version != 1 || intent.HostID != hostID || len(intent.Nonce) != 64 {
			return enrollmentIntent{}, fmt.Errorf("existing enrollment intent does not match host %s", hostID)
		}
		return intent, nil
	}
	if !os.IsNotExist(err) {
		return enrollmentIntent{}, err
	}
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return enrollmentIntent{}, err
	}
	intent := enrollmentIntent{Version: 1, HostID: hostID, Nonce: hex.EncodeToString(value[:])}
	data, err = json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return enrollmentIntent{}, err
	}
	if err := writePrivate(path, append(data, '\n')); err != nil {
		return enrollmentIntent{}, err
	}
	return intent, nil
}

func token(ctx context.Context, client kubernetes.Interface, namespace, serviceAccount string, duration time.Duration) (string, time.Time, error) {
	seconds := int64(duration / time.Second)
	request, err := client.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, serviceAccount, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}}, metav1.CreateOptions{})
	if err != nil {
		return "", time.Time{}, err
	}
	if request.Status.Token == "" || request.Status.ExpirationTimestamp.IsZero() {
		return "", time.Time{}, fmt.Errorf("the TokenRequest returned an incomplete credential")
	}
	return request.Status.Token, request.Status.ExpirationTimestamp.Time, nil
}

func writeKubeconfig(name string, source *rest.Config, token, user string) error {
	if source.Host == "" || source.Insecure {
		return fmt.Errorf("a TLS Kubernetes API endpoint is required")
	}
	caData := source.CAData
	if len(caData) == 0 && source.CAFile != "" {
		var err error
		caData, err = os.ReadFile(source.CAFile)
		if err != nil {
			return fmt.Errorf("read cluster CA: %w", err)
		}
	}
	if len(caData) == 0 {
		return fmt.Errorf("cluster CA data is required")
	}
	config := clientcmdapi.NewConfig()
	config.Clusters["cluster"] = &clientcmdapi.Cluster{Server: source.Host, CertificateAuthorityData: caData, TLSServerName: source.ServerName}
	config.AuthInfos[user] = &clientcmdapi.AuthInfo{Token: token}
	config.Contexts["restricted"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: user}
	config.CurrentContext = "restricted"
	data, err := clientcmd.Write(*config)
	if err != nil {
		return err
	}
	return writePrivate(name, data)
}

func writePrivate(name string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), ".kubeconfig-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, name)
}

func NormalizeHostID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = invalidDNS.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if len(value) > 40 {
		value = strings.Trim(value[:40], "-")
	}
	return value
}

func HostIDFromState(directory string) (string, error) {
	identity, err := IdentityFromState(directory)
	if err != nil {
		return "", err
	}
	return identity.HostID, nil
}

func IdentityFromState(directory string) (EnrollmentIdentity, error) {
	data, err := os.ReadFile(filepath.Join(directory, "enrollment-intent.json"))
	if err != nil {
		return EnrollmentIdentity{}, err
	}
	var intent enrollmentIntent
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		return EnrollmentIdentity{}, err
	}
	if intent.Version != 1 || intent.HostID == "" || intent.Nonce == "" {
		return EnrollmentIdentity{}, fmt.Errorf("enrollment intent is invalid")
	}
	return EnrollmentIdentity{HostID: intent.HostID, Nonce: intent.Nonce}, nil
}
