package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"reflect"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	servingWorkloadAnnotation = "ai.idleloom.io/native-workload"
	servingManagedBy          = "idleloom-controller"
	servingWorkloadUIDLabel   = "ai.idleloom.io/workload-uid"
	servingExecutionIDLabel   = "ai.idleloom.io/execution-id"
	servingServiceLabel       = "ai.idleloom.io/service-name"
)

type servingEndpointResult struct {
	Ready   bool
	Reason  string
	Message string
}

func (r *Reconciler) ensureServingSecrets(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload, intent *nativev1alpha1.WorkloadSchedulingIntent) error {
	if workload.Spec.Server == nil {
		return nil
	}
	if r.Kubernetes == nil {
		return fmt.Errorf("kubernetes client is required for Native serving")
	}
	hostObject, err := r.Dynamic.Resource(nativekube.HostsGVR).Namespace(intent.HostRef.Namespace).Get(ctx, intent.HostRef.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get serving host: %w", err)
	}
	var host nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(hostObject, &host); err != nil {
		return err
	}
	if host.UID != intent.HostRef.UID {
		return fmt.Errorf("serving host identity changed")
	}
	labels := map[string]string{
		"app.kubernetes.io/managed-by": servingManagedBy,
		servingWorkloadUIDLabel:        string(workload.UID),
		servingExecutionIDLabel:        intent.ExecutionID,
		servingServiceLabel:            workload.Spec.Server.ServiceName,
	}
	clientSecretName := workload.Spec.Server.ServiceName + "-auth"
	clientSecrets := r.Kubernetes.CoreV1().Secrets(workload.Namespace)
	clientSecret, err := clientSecrets.Get(ctx, clientSecretName, metav1.GetOptions{})
	clientMissing := apierrors.IsNotFound(err)
	if err != nil && !clientMissing {
		return fmt.Errorf("get client serving Secret: %w", err)
	}
	hostSecrets := r.Kubernetes.CoreV1().Secrets(host.Namespace)
	hostSecret, err := hostSecrets.Get(ctx, nativev1alpha1.ServingAuthSecretName, metav1.GetOptions{})
	hostMissing := apierrors.IsNotFound(err)
	if err != nil && !hostMissing {
		return fmt.Errorf("get host serving Secret: %w", err)
	}
	var key []byte
	if !clientMissing {
		key, err = validateServingSecret(clientSecret, workload, intent.ExecutionID, clientSecretName)
		if err != nil {
			return err
		}
	}
	if !hostMissing {
		hostKey, keyErr := validateServingSecret(hostSecret, workload, intent.ExecutionID, nativev1alpha1.ServingAuthSecretName)
		if keyErr != nil {
			return keyErr
		}
		if key == nil {
			key = hostKey
		} else if !reflect.DeepEqual(key, hostKey) {
			return fmt.Errorf("client and host serving Secrets contain different API keys")
		}
	}
	if key == nil {
		key, err = generateServingAPIKey()
		if err != nil {
			return err
		}
	}
	if clientMissing {
		clientSecret, err = clientSecrets.Create(ctx, servingClientSecret(workload, labels, clientSecretName, key), metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			clientSecret, err = clientSecrets.Get(ctx, clientSecretName, metav1.GetOptions{})
		}
		if err != nil {
			return fmt.Errorf("ensure client serving Secret: %w", err)
		}
		createdKey, keyErr := validateServingSecret(clientSecret, workload, intent.ExecutionID, clientSecretName)
		if keyErr != nil || !reflect.DeepEqual(key, createdKey) {
			return errors.Join(keyErr, fmt.Errorf("client serving Secret key changed during creation"))
		}
	}
	if hostMissing {
		hostSecret, err = hostSecrets.Create(ctx, servingHostSecret(&host, labels, key), metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			hostSecret, err = hostSecrets.Get(ctx, nativev1alpha1.ServingAuthSecretName, metav1.GetOptions{})
		}
		if err != nil {
			return fmt.Errorf("ensure host serving Secret: %w", err)
		}
		createdKey, keyErr := validateServingSecret(hostSecret, workload, intent.ExecutionID, nativev1alpha1.ServingAuthSecretName)
		if keyErr != nil || !reflect.DeepEqual(key, createdKey) {
			return errors.Join(keyErr, fmt.Errorf("host serving Secret key changed during creation"))
		}
	}
	return nil
}

func servingClientSecret(workload *nativev1alpha1.IdleloomWorkload, labels map[string]string, name string, key []byte) *corev1.Secret {
	immutable := true
	controller := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: workload.Namespace, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload",
				Name: workload.Name, UID: workload.UID, Controller: &controller,
			}},
		},
		Immutable: &immutable, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"api-key": append([]byte(nil), key...)},
	}
}

func servingHostSecret(host *nativev1alpha1.IdleloomHost, labels map[string]string, key []byte) *corev1.Secret {
	immutable := true
	controller := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: nativev1alpha1.ServingAuthSecretName, Namespace: host.Namespace, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomHost",
				Name: host.Name, UID: host.UID, Controller: &controller,
			}},
		},
		Immutable: &immutable, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"api-key": append([]byte(nil), key...)},
	}
}

func validateServingSecret(secret *corev1.Secret, workload *nativev1alpha1.IdleloomWorkload, executionID, name string) ([]byte, error) {
	if secret.Labels["app.kubernetes.io/managed-by"] != servingManagedBy ||
		secret.Labels[servingWorkloadUIDLabel] != string(workload.UID) ||
		secret.Labels[servingExecutionIDLabel] != executionID ||
		secret.Labels[servingServiceLabel] != workload.Spec.Server.ServiceName {
		return nil, fmt.Errorf("secret %s/%s is not owned by this Native serving execution", secret.Namespace, name)
	}
	key := secret.Data["api-key"]
	if len(key) < 32 || len(key) > 256 {
		return nil, fmt.Errorf("secret %s/%s has an invalid api-key", secret.Namespace, name)
	}
	return append([]byte(nil), key...), nil
}

func generateServingAPIKey() ([]byte, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return nil, fmt.Errorf("generate Native serving API key: %w", err)
	}
	encoded := make([]byte, hex.EncodedLen(len(value)))
	hex.Encode(encoded, value[:])
	return encoded, nil
}

func (r *Reconciler) reconcileServingEndpoint(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload) (servingEndpointResult, error) {
	if workload.Spec.Server == nil {
		return servingEndpointResult{}, nil
	}
	if r.Kubernetes == nil {
		return servingEndpointResult{Reason: "ServingControllerUnavailable", Message: "Kubernetes client is required for Native serving"}, fmt.Errorf("kubernetes client is required for Native serving")
	}
	if workload.DeletionTimestamp != nil || workload.Status.AssignmentRef == nil {
		result := servingEndpointResult{Reason: "ServingAssignmentPending", Message: "the Native serving assignment is not ready"}
		if workload.DeletionTimestamp != nil {
			result = servingEndpointResult{Reason: "ServingStopping", Message: "the Native serving endpoint is stopping"}
		}
		return result, r.deleteServingEndpoint(ctx, workload)
	}
	if workload.Status.SchedulingIntent == nil {
		return r.failServingEndpoint(ctx, workload,
			servingEndpointResult{Reason: "ServingIntentMissing", Message: "the serving assignment has no scheduling intent"},
			fmt.Errorf("serving workload has an assignment without scheduling intent"))
	}
	if err := r.ensureServingSecrets(ctx, workload, workload.Status.SchedulingIntent); err != nil {
		return r.failServingEndpoint(ctx, workload,
			servingEndpointResult{Reason: "ServingSecretsUnavailable", Message: "the Native serving credentials are unavailable"}, err)
	}
	service, err := r.Kubernetes.CoreV1().Services(workload.Namespace).Get(ctx, workload.Spec.Server.ServiceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return servingEndpointResult{Reason: "ServingServiceMissing", Message: "the selectorless Native serving Service does not exist"}, r.deleteServingEndpoint(ctx, workload)
	}
	if err != nil {
		return r.failServingEndpoint(ctx, workload,
			servingEndpointResult{Reason: "ServingServiceUnavailable", Message: "the Native serving Service could not be read"}, err)
	}
	if err := validateServingService(service, workload); err != nil {
		return r.failServingEndpoint(ctx, workload,
			servingEndpointResult{Reason: "ServingServiceInvalid", Message: "the Native serving Service is outside the required contract"}, err)
	}
	ref := workload.Status.AssignmentRef
	assignmentObject, err := r.Dynamic.Resource(nativekube.AssignmentsGVR).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return servingEndpointResult{Reason: "ServingAssignmentMissing", Message: "the Native serving assignment no longer exists"}, r.deleteServingEndpoint(ctx, workload)
	}
	if err != nil {
		return r.failServingEndpoint(ctx, workload,
			servingEndpointResult{Reason: "ServingAssignmentUnavailable", Message: "the Native serving assignment could not be read"}, err)
	}
	var assignment nativev1alpha1.IdleloomWorkloadAssignment
	if err := nativekube.FromUnstructured(assignmentObject, &assignment); err != nil {
		return r.failServingEndpoint(ctx, workload,
			servingEndpointResult{Reason: "ServingAssignmentInvalid", Message: "the Native serving assignment is invalid"}, err)
	}
	if assignment.UID != ref.UID || assignment.Spec.WorkloadRef.UID != workload.UID || assignment.Spec.Model == nil || assignment.Spec.Model.Server == nil {
		return r.failServingEndpoint(ctx, workload,
			servingEndpointResult{Reason: "ServingAssignmentInvalid", Message: "the Native serving assignment identity does not match the workload"},
			fmt.Errorf("serving assignment identity does not match workload"))
	}
	if assignment.Spec.DesiredState != nativev1alpha1.AssignmentDesiredRunning ||
		assignment.Status.ObservedGeneration != assignment.Generation || assignment.Status.AgentID == "" ||
		assignment.Status.ExecutionID != assignment.Spec.ExecutionID || assignment.Status.FencingEpoch != assignment.Spec.FencingEpoch {
		return servingEndpointResult{Reason: "ServingAssignmentNotReady", Message: "the Native serving assignment has not been acknowledged by its agent"}, r.deleteServingEndpoint(ctx, workload)
	}
	phase, stale := r.assignmentPhase(&assignment)
	if stale || phase != nativev1alpha1.PhaseRunning {
		return servingEndpointResult{Reason: "ServingAssignmentNotReady", Message: "the Native serving assignment is not running with a fresh heartbeat"}, r.deleteServingEndpoint(ctx, workload)
	}
	hostObject, err := r.Dynamic.Resource(nativekube.HostsGVR).Namespace(assignment.Namespace).Get(ctx, assignment.Spec.HostRef.Name, metav1.GetOptions{})
	if err != nil {
		return r.failServingEndpoint(ctx, workload,
			servingEndpointResult{Reason: "ServingHostUnavailable", Message: "the selected Native host could not be read"}, err)
	}
	var host nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(hostObject, &host); err != nil {
		return r.failServingEndpoint(ctx, workload,
			servingEndpointResult{Reason: "ServingHostInvalid", Message: "the selected Native host is invalid"}, err)
	}
	if host.UID != assignment.Spec.HostRef.UID {
		return r.failServingEndpoint(ctx, workload,
			servingEndpointResult{Reason: "ServingHostInvalid", Message: "the selected Native host identity changed"},
			fmt.Errorf("serving host identity does not match assignment"))
	}
	connected := apiMeta.FindStatusCondition(host.Status.Conditions, nativev1alpha1.HostConditionConnected)
	if connected == nil || connected.Status != metav1.ConditionTrue || connected.ObservedGeneration != host.Generation ||
		host.Status.ObservedGeneration != host.Generation || !freshHostHeartbeat(host.Status.LastHeartbeatTime, r.now()) ||
		host.Status.Connectivity == nil || host.Status.Connectivity.Mode != nativev1alpha1.ConnectivityModeWireKubeLeaf {
		return servingEndpointResult{Reason: "ServingHostDisconnected", Message: "the selected Native host has no ready WireKube connection"}, r.deleteServingEndpoint(ctx, workload)
	}
	ip, _, err := net.ParseCIDR(host.Status.Connectivity.Address)
	if err != nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return r.failServingEndpoint(ctx, workload,
			servingEndpointResult{Reason: "ServingHostInvalid", Message: "the selected Native host has an invalid serving address"},
			fmt.Errorf("connected Native host has an invalid serving address"))
	}
	if err := r.ensureServingEndpoint(ctx, workload, service, ip.String()); err != nil {
		return servingEndpointResult{Reason: "ServingEndpointUnavailable", Message: "the Native serving EndpointSlice could not be reconciled"}, err
	}
	return servingEndpointResult{Ready: true, Reason: "ServingEndpointReady", Message: "the Native serving Service has a ready WireKube endpoint"}, nil
}

func (r *Reconciler) failServingEndpoint(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload, result servingEndpointResult, cause error) (servingEndpointResult, error) {
	return result, errors.Join(cause, r.deleteServingEndpoint(ctx, workload))
}

func freshHostHeartbeat(heartbeat *metav1.MicroTime, now time.Time) bool {
	if heartbeat == nil || heartbeat.After(now.Add(nativev1alpha1.HeartbeatClockSkewAllowance)) {
		return false
	}
	return now.Sub(heartbeat.Time) <= nativev1alpha1.DefaultAgentHeartbeatTimeout+nativev1alpha1.HeartbeatClockSkewAllowance
}

func (r *Reconciler) updateServingReadyCondition(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload, result servingEndpointResult) error {
	if workload.Spec.Server == nil || workload.DeletionTimestamp != nil {
		return nil
	}
	// Before assignment, the scheduler owns Ready and can explain why the run is queued.
	if workload.Status.AssignmentRef == nil {
		return nil
	}
	status := metav1.ConditionFalse
	if result.Ready {
		status = metav1.ConditionTrue
	}
	copy := workload.DeepCopy()
	copy.Status.ObservedGeneration = workload.Generation
	apiMeta.SetStatusCondition(&copy.Status.Conditions, metav1.Condition{
		Type: nativev1alpha1.WorkloadConditionReady, Status: status,
		ObservedGeneration: workload.Generation, LastTransitionTime: metav1.NewTime(r.now()),
		Reason: result.Reason, Message: result.Message,
	})
	return r.updateWorkload(ctx, copy, true)
}

func validateServingService(service *corev1.Service, workload *nativev1alpha1.IdleloomWorkload) error {
	if service.Annotations[servingWorkloadAnnotation] != workload.Name {
		return fmt.Errorf("service %s/%s must annotate %s=%s", service.Namespace, service.Name, servingWorkloadAnnotation, workload.Name)
	}
	if len(service.Spec.Selector) != 0 || service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.ClusterIP == corev1.ClusterIPNone || len(service.Spec.ExternalIPs) != 0 {
		return fmt.Errorf("native serving Service must be a selectorless non-headless ClusterIP Service")
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Name != "http" || service.Spec.Ports[0].Protocol != corev1.ProtocolTCP {
		return fmt.Errorf("native serving Service must expose one TCP port named http")
	}
	return nil
}

func (r *Reconciler) ensureServingEndpoint(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload, service *corev1.Service, address string) error {
	name := service.Name
	portName := "http"
	protocol := corev1.ProtocolTCP
	appProtocol := "http"
	port := nativev1alpha1.NativeServingPort
	ready := true
	serving := true
	controller := true
	desired := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: service.Namespace,
			Labels: map[string]string{
				discoveryv1.LabelServiceName: service.Name, discoveryv1.LabelManagedBy: servingManagedBy,
				servingWorkloadUIDLabel: string(workload.UID),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "Service", Name: service.Name, UID: service.UID, Controller: &controller,
			}},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: &portName, Protocol: &protocol, Port: &port, AppProtocol: &appProtocol}},
		Endpoints: []discoveryv1.Endpoint{{
			Addresses: []string{address}, Conditions: discoveryv1.EndpointConditions{Ready: &ready, Serving: &serving},
			TargetRef: &corev1.ObjectReference{
				APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload",
				Namespace: workload.Namespace, Name: workload.Name, UID: workload.UID,
			},
		}},
	}
	slices := r.Kubernetes.DiscoveryV1().EndpointSlices(service.Namespace)
	existing, err := slices.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = slices.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if existing.Labels[discoveryv1.LabelManagedBy] != servingManagedBy || existing.Labels[servingWorkloadUIDLabel] != string(workload.UID) || len(existing.OwnerReferences) != 1 || existing.OwnerReferences[0].UID != service.UID {
		return fmt.Errorf("the EndpointSlice %s/%s is outside the Native serving contract", existing.Namespace, existing.Name)
	}
	if reflect.DeepEqual(existing.Labels, desired.Labels) && reflect.DeepEqual(existing.OwnerReferences, desired.OwnerReferences) && existing.AddressType == desired.AddressType && reflect.DeepEqual(existing.Ports, desired.Ports) && reflect.DeepEqual(existing.Endpoints, desired.Endpoints) {
		return nil
	}
	desired.ResourceVersion = existing.ResourceVersion
	_, err = slices.Update(ctx, desired, metav1.UpdateOptions{})
	return err
}

func (r *Reconciler) deleteServingEndpoint(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload) error {
	if r.Kubernetes == nil || workload.Spec.Server == nil {
		return nil
	}
	slices := r.Kubernetes.DiscoveryV1().EndpointSlices(workload.Namespace)
	existing, err := slices.Get(ctx, workload.Spec.Server.ServiceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.Labels[discoveryv1.LabelManagedBy] != servingManagedBy || existing.Labels[servingWorkloadUIDLabel] != string(workload.UID) {
		return fmt.Errorf("refusing to delete EndpointSlice outside the Native serving contract")
	}
	return slices.Delete(ctx, existing.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &existing.UID}})
}

func (r *Reconciler) cleanupServingResources(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload, hostNamespace string) error {
	if workload.Spec.Server == nil || r.Kubernetes == nil {
		return nil
	}
	var errs []error
	if err := r.deleteServingEndpoint(ctx, workload); err != nil {
		errs = append(errs, err)
	}
	if hostNamespace != "" {
		if err := r.deleteServingSecret(ctx, hostNamespace, nativev1alpha1.ServingAuthSecretName, workload.UID); err != nil {
			errs = append(errs, err)
		}
	}
	if err := r.deleteServingSecret(ctx, workload.Namespace, workload.Spec.Server.ServiceName+"-auth", workload.UID); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r *Reconciler) deleteServingSecret(ctx context.Context, namespace, name string, workloadUID types.UID) error {
	secrets := r.Kubernetes.CoreV1().Secrets(namespace)
	secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if secret.Labels["app.kubernetes.io/managed-by"] != servingManagedBy || secret.Labels[servingWorkloadUIDLabel] != string(workloadUID) {
		return fmt.Errorf("refusing to delete Secret %s/%s outside the Native serving contract", namespace, name)
	}
	return secrets.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &secret.UID}})
}
