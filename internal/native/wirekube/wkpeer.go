package wirekube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	peerModeWireKube       = "wkpeer"
	peerServiceAccountBase = "wirekube-relay-peer-"
	peerRelayAudience      = "wirekube-relay"
	linkKubeconfigName     = "wirekube-peer.kubeconfig"
)

var ServicesGVR = schema.GroupVersionResource{Version: "v1", Resource: "services"}

func relayDialTarget(ctx context.Context, client dynamic.Interface, mesh *unstructured.Unstructured) (string, string, error) {
	provider, _, _ := unstructured.NestedString(mesh.Object, "spec", "relay", "provider")
	if provider == "" {
		return "", "", fmt.Errorf("WireKube relay provider is not configured")
	}
	transport, _, _ := unstructured.NestedString(mesh.Object, "spec", "relay", provider, "transport")
	if transport == "" {
		transport = "tcp"
	}
	switch transport {
	case "wss":
		endpoint, _, _ := unstructured.NestedString(mesh.Object, "spec", "relay", provider, "controlEndpoint")
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Scheme != "wss" || parsed.Host == "" {
			return "", "", fmt.Errorf("WireKube %s relay requires a public wss:// controlEndpoint", provider)
		}
		return transport, endpoint, nil
	case "ws":
		return "", "", fmt.Errorf("WireKube ws relay transport is insecure; configure wss")
	case "tcp":
		if provider == "external" {
			endpoint, _, _ := unstructured.NestedString(mesh.Object, "spec", "relay", "external", "controlEndpoint")
			if endpoint == "" {
				endpoint, _, _ = unstructured.NestedString(mesh.Object, "spec", "relay", "external", "endpoint")
			}
			if _, _, err := net.SplitHostPort(endpoint); err != nil {
				return "", "", fmt.Errorf("WireKube external TCP relay endpoint %q is invalid: %w", endpoint, err)
			}
			return transport, endpoint, nil
		}
		endpoint, err := managedRelayLoadBalancer(ctx, client)
		if err != nil {
			return "", "", err
		}
		return transport, endpoint, nil
	default:
		return "", "", fmt.Errorf("WireKube relay transport %q is unsupported", transport)
	}
}

func managedRelayLoadBalancer(ctx context.Context, client dynamic.Interface) (string, error) {
	services, err := client.Resource(ServicesGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("discover public WireKube relay Service: %w", err)
	}
	for index := range services.Items {
		service := &services.Items[index]
		if service.GetName() != "wirekube-relay" || service.GetLabels()["app.kubernetes.io/part-of"] != "wirekube" {
			continue
		}
		serviceType, _, _ := unstructured.NestedString(service.Object, "spec", "type")
		if serviceType != string(corev1.ServiceTypeLoadBalancer) {
			return "", fmt.Errorf("WireKube managed TCP relay Service %s/%s is %s; configure a public WSS control endpoint or a LoadBalancer", service.GetNamespace(), service.GetName(), serviceType)
		}
		port, err := relayServicePort(service)
		if err != nil {
			return "", err
		}
		ingress, _, _ := unstructured.NestedSlice(service.Object, "status", "loadBalancer", "ingress")
		for _, item := range ingress {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			host, _ := entry["hostname"].(string)
			if host == "" {
				host, _ = entry["ip"].(string)
			}
			if host != "" {
				return net.JoinHostPort(host, fmt.Sprint(port)), nil
			}
		}
		return "", fmt.Errorf("WireKube managed TCP relay LoadBalancer %s/%s has no public address yet", service.GetNamespace(), service.GetName())
	}
	return "", fmt.Errorf("WireKube managed TCP relay Service was not found; configure a public WSS control endpoint")
}

func relayServicePort(service *unstructured.Unstructured) (int64, error) {
	ports, _, _ := unstructured.NestedSlice(service.Object, "spec", "ports")
	for _, item := range ports {
		port, ok := item.(map[string]any)
		if !ok {
			continue
		}
		protocol, _ := port["protocol"].(string)
		name, _ := port["name"].(string)
		value, _ := port["port"].(int64)
		if (protocol == "" || protocol == string(corev1.ProtocolTCP)) && (name == "relay-tcp" || value == 3478) && value > 0 {
			return value, nil
		}
	}
	return 0, fmt.Errorf("WireKube relay Service %s/%s has no TCP relay port", service.GetNamespace(), service.GetName())
}

func relayTokenAudience(transport string) string {
	if transport == "wss" {
		return peerRelayAudience
	}
	return ""
}

func peerServiceAccountName(peerName string) string {
	return peerServiceAccountBase + peerName
}

func peerRBACName(peerName string) string {
	return "idleloom-wirekube-" + strings.TrimPrefix(peerName, "idleloom-")
}

func desiredWireKubePeer(state State, hostID, address string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wirekube.io/v1alpha1",
		"kind":       "WireKubePeer",
		"metadata": map[string]any{
			"name": state.PeerName,
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": managedBy,
				"app.kubernetes.io/part-of":    "idleloom",
				"ai.idleloom.io/host-id":       hostID,
			},
			"annotations": map[string]any{"ai.idleloom.io/enrollment-id": state.EnrollmentID},
		},
		"spec": map[string]any{
			"publicKey":           state.PublicKey,
			"allowedIPs":          []any{address},
			"persistentKeepalive": int64(25),
		},
	}}
}

func ensureWireKubePeer(ctx context.Context, client dynamic.Interface, desired *unstructured.Unstructured, state State) (*unstructured.Unstructured, bool, error) {
	peers := client.Resource(PeersGVR)
	existing, err := peers.Get(ctx, desired.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		created, createErr := peers.Create(ctx, desired, metav1.CreateOptions{})
		if createErr != nil {
			return nil, false, fmt.Errorf("create WireKubePeer/%s: %w", desired.GetName(), createErr)
		}
		return created, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get WireKubePeer/%s: %w", desired.GetName(), err)
	}
	if err := validateOwnedWireKubePeer(existing, desired, state); err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

func validateOwnedWireKubePeer(existing, desired *unstructured.Unstructured, state State) error {
	if !hasOwnedPeerMetadata(existing, state) {
		return fmt.Errorf("WireKubePeer/%s is not owned by this enrollment", existing.GetName())
	}
	if state.PeerUID != "" && existing.GetUID() != state.PeerUID {
		return fmt.Errorf("WireKubePeer/%s identity changed", existing.GetName())
	}
	existingSpec, _, _ := unstructured.NestedMap(existing.Object, "spec")
	desiredSpec, _, _ := unstructured.NestedMap(desired.Object, "spec")
	if !reflect.DeepEqual(existingSpec, desiredSpec) {
		return fmt.Errorf("WireKubePeer/%s does not match the enrolled key and route contract", existing.GetName())
	}
	return nil
}

func refreshWireKubePeerState(ctx context.Context, client dynamic.Interface, directory string, state State) (State, error) {
	peer, err := client.Resource(PeersGVR).Get(ctx, state.PeerName, metav1.GetOptions{})
	if err != nil {
		return State{}, fmt.Errorf("get WireKubePeer/%s: %w", state.PeerName, err)
	}
	if peer.GetUID() != state.PeerUID {
		return State{}, fmt.Errorf("WireKubePeer/%s identity changed", state.PeerName)
	}
	desired := desiredWireKubePeer(state, strings.TrimPrefix(state.PeerName, "idleloom-"), state.AssignedMeshIP)
	if err := validateOwnedWireKubePeer(peer, desired, state); err != nil {
		return State{}, err
	}
	if err := writeState(directory, state); err != nil {
		return State{}, err
	}
	return state, nil
}

func ensurePeerIdentity(ctx context.Context, client kubernetes.Interface, state State, hostID string) error {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": managedBy,
		"app.kubernetes.io/part-of":    "idleloom",
		"ai.idleloom.io/host-id":       hostID,
	}
	annotations := map[string]string{"ai.idleloom.io/enrollment-id": state.EnrollmentID}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: state.PeerServiceAccount, Namespace: state.PeerNamespace, Labels: labels, Annotations: annotations,
	}}
	if err := ensureOwnedServiceAccount(ctx, client, account, state); err != nil {
		return err
	}
	name := peerRBACName(state.PeerName)
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: state.PeerNamespace, Labels: labels, Annotations: annotations},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"serviceaccounts/token"}, ResourceNames: []string{state.PeerServiceAccount}, Verbs: []string{"create"},
		}},
	}
	if err := ensureOwnedRole(ctx, client, role, state); err != nil {
		return err
	}
	roleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: state.PeerNamespace, Labels: labels, Annotations: annotations},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: state.PeerServiceAccount, Namespace: state.PeerNamespace}},
	}
	if err := ensureOwnedRoleBinding(ctx, client, roleBinding, state); err != nil {
		return err
	}
	clusterRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"wirekube.io"}, Resources: []string{"wirekubepeers"}, Verbs: []string{"list"}},
			{APIGroups: []string{"wirekube.io"}, Resources: []string{"wirekubepeers/status"}, ResourceNames: []string{state.PeerName}, Verbs: []string{"patch"}},
		},
	}
	if err := ensureOwnedClusterRole(ctx, client, clusterRole, state); err != nil {
		return err
	}
	clusterBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: state.PeerServiceAccount, Namespace: state.PeerNamespace}},
	}
	return ensureOwnedClusterRoleBinding(ctx, client, clusterBinding, state)
}

func ensureOwnedServiceAccount(ctx context.Context, client kubernetes.Interface, desired *corev1.ServiceAccount, state State) error {
	accounts := client.CoreV1().ServiceAccounts(desired.Namespace)
	existing, err := accounts.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = accounts.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	return validateOwnedObject(existing.Labels, existing.Annotations, "ServiceAccount", desired.Namespace+"/"+desired.Name, state)
}

func ensureOwnedRole(ctx context.Context, client kubernetes.Interface, desired *rbacv1.Role, state State) error {
	roles := client.RbacV1().Roles(desired.Namespace)
	existing, err := roles.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = roles.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if err := validateOwnedObject(existing.Labels, existing.Annotations, "Role", desired.Namespace+"/"+desired.Name, state); err != nil {
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	_, err = roles.Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

func ensureOwnedRoleBinding(ctx context.Context, client kubernetes.Interface, desired *rbacv1.RoleBinding, state State) error {
	bindings := client.RbacV1().RoleBindings(desired.Namespace)
	existing, err := bindings.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = bindings.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if err := validateOwnedObject(existing.Labels, existing.Annotations, "RoleBinding", desired.Namespace+"/"+desired.Name, state); err != nil {
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	_, err = bindings.Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

func ensureOwnedClusterRole(ctx context.Context, client kubernetes.Interface, desired *rbacv1.ClusterRole, state State) error {
	roles := client.RbacV1().ClusterRoles()
	existing, err := roles.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = roles.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if err := validateOwnedObject(existing.Labels, existing.Annotations, "ClusterRole", desired.Name, state); err != nil {
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	_, err = roles.Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

func ensureOwnedClusterRoleBinding(ctx context.Context, client kubernetes.Interface, desired *rbacv1.ClusterRoleBinding, state State) error {
	bindings := client.RbacV1().ClusterRoleBindings()
	existing, err := bindings.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = bindings.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if err := validateOwnedObject(existing.Labels, existing.Annotations, "ClusterRoleBinding", desired.Name, state); err != nil {
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	_, err = bindings.Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

func validateOwnedObject(labels, annotations map[string]string, kind, name string, state State) error {
	if labels["app.kubernetes.io/managed-by"] != managedBy || annotations["ai.idleloom.io/enrollment-id"] != state.EnrollmentID {
		return fmt.Errorf("%s %s is not owned by this enrollment", kind, name)
	}
	return nil
}

func writePeerKubeconfig(ctx context.Context, client kubernetes.Interface, source *rest.Config, state State, path string, duration time.Duration) error {
	seconds := int64(duration / time.Second)
	request, err := client.CoreV1().ServiceAccounts(state.PeerNamespace).CreateToken(ctx, state.PeerServiceAccount, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds},
	}, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create restricted WireKube peer token: %w", err)
	}
	if request.Status.Token == "" || request.Status.ExpirationTimestamp.IsZero() {
		return fmt.Errorf("WireKube peer TokenRequest returned an incomplete credential")
	}
	if source.Host == "" || source.Insecure {
		return fmt.Errorf("a TLS Kubernetes API endpoint is required")
	}
	caData := append([]byte(nil), source.CAData...)
	if len(caData) == 0 && source.CAFile != "" {
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
	config.AuthInfos["wirekube-peer"] = &clientcmdapi.AuthInfo{Token: request.Status.Token}
	config.Contexts["restricted"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "wirekube-peer"}
	config.CurrentContext = "restricted"
	data, err := clientcmd.Write(*config)
	if err != nil {
		return err
	}
	return writePrivate(path, data)
}

func RelayTokenPath(directory string) string {
	return filepath.Join(directory, "wirekube-relay.token")
}

func WriteRelayToken(ctx context.Context, client kubernetes.Interface, state State, directory string, duration time.Duration) (time.Time, error) {
	if state.RelayTransport != "wss" {
		return time.Time{}, nil
	}
	if client == nil || state.PeerNamespace == "" || state.PeerServiceAccount == "" || state.RelayTokenAudience == "" {
		return time.Time{}, fmt.Errorf("WireKube relay credential configuration is incomplete")
	}
	if duration <= 0 || duration > time.Hour {
		duration = time.Hour
	}
	seconds := int64(duration / time.Second)
	request, err := client.CoreV1().ServiceAccounts(state.PeerNamespace).CreateToken(ctx, state.PeerServiceAccount, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			Audiences:         []string{state.RelayTokenAudience},
			ExpirationSeconds: &seconds,
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return time.Time{}, fmt.Errorf("create WireKube relay token: %w", err)
	}
	if request.Status.Token == "" || request.Status.ExpirationTimestamp.IsZero() {
		return time.Time{}, fmt.Errorf("WireKube relay TokenRequest returned an incomplete credential")
	}
	if err := writePrivate(RelayTokenPath(directory), []byte(request.Status.Token+"\n")); err != nil {
		return time.Time{}, err
	}
	return request.Status.ExpirationTimestamp.Time, nil
}

func UpdatePeerStatus(ctx context.Context, client dynamic.Interface, state State, snapshot TunnelSnapshot, connected bool) error {
	status := map[string]any{
		"connected":     connected,
		"bytesReceived": snapshot.BytesReceived,
		"bytesSent":     snapshot.BytesSent,
		"bindMode":      "userspace",
		"iceState":      "relay",
	}
	if !snapshot.LastHandshake.IsZero() {
		status["lastHandshake"] = snapshot.LastHandshake.UTC().Format(time.RFC3339Nano)
	}
	payload, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return err
	}
	_, err = client.Resource(PeersGVR).Patch(ctx, state.PeerName, types.MergePatchType, payload, metav1.PatchOptions{}, "status")
	if err != nil {
		return fmt.Errorf("update WireKubePeer/%s status: %w", state.PeerName, err)
	}
	return nil
}

func rollbackNewWireKubeEnrollment(ctx context.Context, client dynamic.Interface, peer, claim *unstructured.Unstructured, state State, peerCreated, claimCreated bool) error {
	var values []error
	if peerCreated && peer != nil && peer.GetUID() != "" {
		uid := peer.GetUID()
		if err := client.Resource(PeersGVR).Delete(ctx, peer.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			values = append(values, fmt.Errorf("rollback WireKubePeer/%s: %w", peer.GetName(), err))
		}
		values = append(values, deletePeerIdentity(ctx, client, state))
	}
	if claimCreated && claim != nil && claim.GetUID() != "" {
		uid := claim.GetUID()
		if err := client.Resource(IPClaimsGVR).Namespace(ipClaimNamespace).Delete(ctx, claim.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			values = append(values, fmt.Errorf("rollback mesh IP claim Lease/%s: %w", claim.GetName(), err))
		}
	}
	return errors.Join(values...)
}

func deletePeerIdentity(ctx context.Context, client dynamic.Interface, state State) error {
	name := peerRBACName(state.PeerName)
	resources := []struct {
		gvr       schema.GroupVersionResource
		namespace string
		name      string
	}{
		{schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}, "", name},
		{schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, "", name},
		{schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}, state.PeerNamespace, name},
		{schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}, state.PeerNamespace, name},
		{schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}, state.PeerNamespace, state.PeerServiceAccount},
	}
	var values []error
	for _, item := range resources {
		var resource dynamic.ResourceInterface
		if item.namespace == "" {
			resource = client.Resource(item.gvr)
		} else {
			resource = client.Resource(item.gvr).Namespace(item.namespace)
		}
		existing, err := resource.Get(ctx, item.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			values = append(values, err)
			continue
		}
		if !hasOwnedPeerMetadata(existing, state) {
			values = append(values, fmt.Errorf("%s/%s is not owned by this enrollment", item.gvr.Resource, item.name))
			continue
		}
		uid := existing.GetUID()
		options := metav1.DeleteOptions{}
		if uid != "" {
			options.Preconditions = &metav1.Preconditions{UID: &uid}
		}
		if err := resource.Delete(ctx, item.name, options); err != nil && !apierrors.IsNotFound(err) {
			values = append(values, err)
		}
	}
	return errors.Join(values...)
}

func revokeWireKubePeer(ctx context.Context, config RevokeConfig, state State) error {
	peer, err := config.Dynamic.Resource(PeersGVR).Get(ctx, state.PeerName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get WireKubePeer/%s: %w", state.PeerName, err)
	}
	if peer != nil {
		desired := desiredWireKubePeer(state, strings.TrimPrefix(state.PeerName, "idleloom-"), state.AssignedMeshIP)
		if err := validateOwnedWireKubePeer(peer, desired, state); err != nil {
			return err
		}
		uid := peer.GetUID()
		if uid == "" {
			return fmt.Errorf("WireKubePeer/%s has no UID", state.PeerName)
		}
		if err := config.Dynamic.Resource(PeersGVR).Delete(ctx, state.PeerName, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete WireKubePeer/%s: %w", state.PeerName, err)
		}
		waitTimeout := config.WaitTimeout
		if waitTimeout <= 0 {
			waitTimeout = time.Minute
		}
		deadline := time.NewTimer(waitTimeout)
		defer deadline.Stop()
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			_, getErr := config.Dynamic.Resource(PeersGVR).Get(ctx, state.PeerName, metav1.GetOptions{})
			if apierrors.IsNotFound(getErr) {
				break
			}
			if getErr != nil {
				return fmt.Errorf("wait for WireKubePeer/%s deletion: %w", state.PeerName, getErr)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				return fmt.Errorf("wait for WireKubePeer/%s deletion: timeout", state.PeerName)
			case <-ticker.C:
			}
		}
	}
	if err := deletePeerIdentity(ctx, config.Dynamic, state); err != nil {
		return fmt.Errorf("delete WireKube peer identity: %w", err)
	}
	waitTimeout := config.WaitTimeout
	if waitTimeout <= 0 {
		waitTimeout = time.Minute
	}
	if err := deleteMeshIPClaim(ctx, config.Dynamic, state, waitTimeout); err != nil {
		return err
	}
	if state.LinkKubeconfig != "" {
		if err := os.Remove(state.LinkKubeconfig); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return removeLocalState(config.StateDirectory)
}

func LinkKubeconfigPath(rootStateDirectory string) string {
	return filepath.Join(rootStateDirectory, linkKubeconfigName)
}
