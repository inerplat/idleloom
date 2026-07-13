package projection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
)

const (
	LabelProjection    = "native.idleloom.io/projection"
	LabelAssignmentUID = "native.idleloom.io/assignment-uid"
	LabelWorkloadUID   = "native.idleloom.io/workload-uid"
	LabelExecutionID   = "native.idleloom.io/execution-id"

	AnnotationContract      = "native.idleloom.io/contract"
	AnnotationKubeletAPI    = "native.idleloom.io/kubelet-api"
	AnnotationLogs          = "native.idleloom.io/logs"
	AnnotationExec          = "native.idleloom.io/exec"
	AnnotationPortForward   = "native.idleloom.io/port-forward"
	AnnotationConnectivity  = "native.idleloom.io/connectivity"
	AnnotationModelArtifact = "native.idleloom.io/model-artifact"

	ProjectionValue = "true"
	ContractValue   = "observability-only-alpha"
	containerName   = "native-metal"
)

type Controller struct {
	Dynamic    dynamic.Interface
	Kubernetes kubernetes.Interface
	Now        func() time.Time
	ProbeLogs  func(context.Context, string, string) error
}

type projectionRef struct {
	NodeName     string
	PodNamespace string
	PodName      string
}

func (c *Controller) ReconcileOnce(ctx context.Context) error {
	if c.Dynamic == nil || c.Kubernetes == nil {
		return fmt.Errorf("dynamic and Kubernetes clients are required")
	}
	objects, err := c.Dynamic.Resource(nativekube.AssignmentsGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list native assignments: %w", err)
	}
	active := make(map[string]projectionRef, len(objects.Items))
	var errs []error
	allowGarbageCollection := true
	for index := range objects.Items {
		var assignment nativev1alpha1.IdleloomWorkloadAssignment
		if err := nativekube.FromUnstructured(&objects.Items[index], &assignment); err != nil {
			errs = append(errs, err)
			allowGarbageCollection = false
			continue
		}
		if assignment.UID == "" {
			errs = append(errs, fmt.Errorf("assignment %s/%s has no UID", assignment.Namespace, assignment.Name))
			allowGarbageCollection = false
			continue
		}
		if projectionComplete(&assignment) {
			if err := c.deleteProjection(ctx, refFor(&assignment)); err != nil {
				errs = append(errs, fmt.Errorf("delete stopped projection for %s/%s: %w", assignment.Namespace, assignment.Name, err))
			}
			continue
		}
		ref := refFor(&assignment)
		active[string(assignment.UID)] = ref
		workload, err := c.getWorkload(ctx, &assignment)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve workload for assignment %s/%s: %w", assignment.Namespace, assignment.Name, err))
			continue
		}
		host, err := c.getHost(ctx, &assignment)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve host for assignment %s/%s: %w", assignment.Namespace, assignment.Name, err))
			continue
		}
		if err := c.project(ctx, workload, host, &assignment, ref); err != nil {
			errs = append(errs, fmt.Errorf("project assignment %s/%s: %w", assignment.Namespace, assignment.Name, err))
		}
	}
	if allowGarbageCollection {
		if err := c.garbageCollect(ctx, active); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) project(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload, host *nativev1alpha1.IdleloomHost, assignment *nativev1alpha1.IdleloomWorkloadAssignment, ref projectionRef) error {
	address, candidate := connectedKubeletAddress(host)
	pod, err := c.ensurePod(ctx, workload, assignment, ref, candidate)
	if err != nil {
		return err
	}
	node, err := c.ensureNode(ctx, assignment, ref.NodeName, candidate)
	if err != nil {
		return err
	}
	state := stateFor(assignment, c.now())
	if err := c.updateNodeStatus(ctx, node, assignment, state, address, candidate); err != nil {
		return err
	}
	logsReady := candidate && c.probeLogs(ctx, ref.PodNamespace, ref.PodName) == nil
	if logsReady != candidate {
		pod, err = c.ensurePod(ctx, workload, assignment, ref, logsReady)
		if err != nil {
			return err
		}
		node, err = c.ensureNode(ctx, assignment, ref.NodeName, logsReady)
		if err != nil {
			return err
		}
		if err := c.updateNodeStatus(ctx, node, assignment, state, address, logsReady); err != nil {
			return err
		}
	}
	return c.updatePodStatus(ctx, pod, assignment, state)
}

func (c *Controller) probeLogs(ctx context.Context, namespace, pod string) error {
	if c.ProbeLogs != nil {
		return c.ProbeLogs(ctx, namespace, pod)
	}
	tail := int64(1)
	_, err := c.Kubernetes.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{TailLines: &tail}).DoRaw(ctx)
	return err
}

func (c *Controller) getHost(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment) (*nativev1alpha1.IdleloomHost, error) {
	ref := assignment.Spec.HostRef
	object, err := c.Dynamic.Resource(nativekube.HostsGVR).Namespace(assignment.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var host nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(object, &host); err != nil {
		return nil, err
	}
	if host.UID != ref.UID {
		return nil, fmt.Errorf("host identity does not match assignment")
	}
	return &host, nil
}

func (c *Controller) getWorkload(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment) (*nativev1alpha1.IdleloomWorkload, error) {
	ref := assignment.Spec.WorkloadRef
	object, err := c.Dynamic.Resource(nativekube.WorkloadsGVR).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var workload nativev1alpha1.IdleloomWorkload
	if err := nativekube.FromUnstructured(object, &workload); err != nil {
		return nil, err
	}
	if workload.UID != ref.UID {
		return nil, fmt.Errorf("workload identity does not match assignment")
	}
	if workload.Generation != ref.Generation && workload.DeletionTimestamp == nil {
		return nil, fmt.Errorf("workload generation does not match assignment")
	}
	return &workload, nil
}

func (c *Controller) ensurePod(ctx context.Context, workload *nativev1alpha1.IdleloomWorkload, assignment *nativev1alpha1.IdleloomWorkloadAssignment, ref projectionRef, logsReady bool) (*corev1.Pod, error) {
	pods := c.Kubernetes.CoreV1().Pods(ref.PodNamespace)
	desired := managedPod(workload, assignment, ref, logsReady)
	existing, err := pods.Get(ctx, ref.PodName, metav1.GetOptions{})
	if err == nil {
		if err := validateManagedPod(existing, desired, assignment); err != nil {
			return nil, err
		}
		if !apiequality.Semantic.DeepEqual(existing.Annotations, desired.Annotations) {
			copy := existing.DeepCopy()
			copy.Annotations = desired.Annotations
			updated, updateErr := pods.Update(ctx, copy, metav1.UpdateOptions{})
			if updateErr != nil {
				return nil, updateErr
			}
			return updated, nil
		}
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	created, err := pods.Create(ctx, desired, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		created, err = pods.Get(ctx, ref.PodName, metav1.GetOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("create managed projection Pod: %w", err)
	}
	if err := validateManagedPod(created, desired, assignment); err != nil {
		return nil, err
	}
	return created, nil
}

func managedPod(workload *nativev1alpha1.IdleloomWorkload, assignment *nativev1alpha1.IdleloomWorkloadAssignment, ref projectionRef, logsReady bool) *corev1.Pod {
	automount := false
	enableLinks := false
	grace := int64(0)
	controller := true
	image := "native.idleloom.invalid/metal-projection:alpha"
	if assignment.Spec.Shell != nil {
		image = "native.idleloom.invalid/shell-projection:alpha"
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        ref.PodName,
			Namespace:   ref.PodNamespace,
			Labels:      projectionLabels(assignment),
			Annotations: projectionAnnotations(assignment, logsReady),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkload",
				Name: workload.Name, UID: workload.UID, Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{
			NodeName:                      ref.NodeName,
			RestartPolicy:                 corev1.RestartPolicyNever,
			DNSPolicy:                     corev1.DNSClusterFirst,
			ServiceAccountName:            "default",
			DeprecatedServiceAccount:      "default",
			SchedulerName:                 corev1.DefaultSchedulerName,
			Priority:                      ptr(int32(0)),
			PreemptionPolicy:              ptr(corev1.PreemptLowerPriority),
			AutomountServiceAccountToken:  &automount,
			EnableServiceLinks:            &enableLinks,
			TerminationGracePeriodSeconds: &grace,
			Tolerations: []corev1.Toleration{
				{Key: "native.idleloom.io/reserved", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule},
				{Key: "native.idleloom.io/reserved", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoExecute},
			},
			Containers: []corev1.Container{{
				Name:                     containerName,
				Image:                    image,
				ImagePullPolicy:          corev1.PullIfNotPresent,
				TerminationMessagePath:   corev1.TerminationMessagePathDefault,
				TerminationMessagePolicy: corev1.TerminationMessageReadFile,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{"native.idleloom.io/execution-slot": resource.MustParse("1")},
					Limits:   corev1.ResourceList{"native.idleloom.io/execution-slot": resource.MustParse("1")},
				},
			}},
		},
	}
	kubescheme.Scheme.Default(pod)
	return pod
}

func (c *Controller) ensureNode(ctx context.Context, assignment *nativev1alpha1.IdleloomWorkloadAssignment, name string, logsReady bool) (*corev1.Node, error) {
	nodes := c.Kubernetes.CoreV1().Nodes()
	existing, err := nodes.Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if existing.Labels[LabelProjection] != ProjectionValue || existing.Labels[LabelAssignmentUID] != string(assignment.UID) || !existing.Spec.Unschedulable {
			return nil, fmt.Errorf("Node %s exists and is not the expected projection", name)
		}
		desiredAnnotations := projectionAnnotations(assignment, logsReady)
		if !apiequality.Semantic.DeepEqual(existing.Annotations, desiredAnnotations) {
			copy := existing.DeepCopy()
			copy.Annotations = desiredAnnotations
			return nodes.Update(ctx, copy, metav1.UpdateOptions{})
		}
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: projectionLabels(assignment), Annotations: projectionAnnotations(assignment, logsReady)},
		Spec: corev1.NodeSpec{
			Unschedulable: true,
			Taints: []corev1.Taint{
				{Key: "native.idleloom.io/reserved", Value: "true", Effect: corev1.TaintEffectNoSchedule},
				{Key: "native.idleloom.io/reserved", Value: "true", Effect: corev1.TaintEffectNoExecute},
			},
		},
	}
	node.Labels[corev1.LabelOSStable] = "darwin"
	node.Labels[corev1.LabelArchStable] = "arm64"
	created, err := nodes.Create(ctx, node, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		created, err = nodes.Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("create ephemeral projection Node: %w", err)
	}
	return created, nil
}

func (c *Controller) updateNodeStatus(ctx context.Context, node *corev1.Node, assignment *nativev1alpha1.IdleloomWorkloadAssignment, state projectionState, address string, logsReady bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := c.Kubernetes.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		copy := current.DeepCopy()
		copy.Status.Capacity = corev1.ResourceList{
			corev1.ResourcePods:                 resource.MustParse("1"),
			"native.idleloom.io/execution-slot": resource.MustParse("1"),
		}
		copy.Status.Allocatable = copy.Status.Capacity.DeepCopy()
		copy.Status.Addresses = nil
		copy.Status.DaemonEndpoints = corev1.NodeDaemonEndpoints{}
		if logsReady {
			copy.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: address}}
			copy.Status.DaemonEndpoints.KubeletEndpoint.Port = 10250
		}
		copy.Status.NodeInfo = corev1.NodeSystemInfo{
			MachineID: string(assignment.UID), SystemUUID: assignment.Spec.ExecutionID,
			KubeletVersion: "idleloom-projection-alpha", ContainerRuntimeVersion: "idleloom://native-metal",
			OSImage: "Idleloom Native Metal Projection", OperatingSystem: "darwin", Architecture: "arm64",
		}
		now := metav1.NewTime(c.now())
		heartbeat := now
		if assignment.Status.LastHeartbeatTime != nil {
			heartbeat = metav1.NewTime(assignment.Status.LastHeartbeatTime.Time)
		}
		condition := corev1.NodeCondition{
			Type: corev1.NodeReady, Status: state.nodeStatus, Reason: state.reason, Message: state.message,
			LastHeartbeatTime: heartbeat, LastTransitionTime: now,
		}
		for _, existing := range current.Status.Conditions {
			if existing.Type == corev1.NodeReady && existing.Status == condition.Status && existing.Reason == condition.Reason {
				condition.LastTransitionTime = existing.LastTransitionTime
			}
		}
		copy.Status.Conditions = []corev1.NodeCondition{condition}
		_, err = c.Kubernetes.CoreV1().Nodes().UpdateStatus(ctx, copy, metav1.UpdateOptions{})
		return err
	})
}

func (c *Controller) updatePodStatus(ctx context.Context, pod *corev1.Pod, assignment *nativev1alpha1.IdleloomWorkloadAssignment, state projectionState) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current, err := c.Kubernetes.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		copy := current.DeepCopy()
		now := metav1.NewTime(c.now())
		phase := state.podPhase
		if current.Status.Phase == corev1.PodRunning && phase == corev1.PodPending {
			phase = corev1.PodRunning
		}
		copy.Status.Phase = phase
		copy.Status.Reason = state.reason
		copy.Status.Message = state.message
		copy.Status.Conditions = podConditions(state, now, current.Status.Conditions)
		if copy.Status.StartTime == nil && phase != corev1.PodPending {
			copy.Status.StartTime = &now
		}
		imageID := "idleloom://shell/" + assignment.Spec.ExecutionID
		if assignment.Spec.Model != nil {
			imageID = assignment.Spec.Model.Artifact.OCIReference
		}
		containerStatus := corev1.ContainerStatus{
			Name: containerName, Image: copy.Spec.Containers[0].Image,
			ImageID: imageID, ContainerID: "idleloom://" + assignment.Spec.ExecutionID,
			Ready: state.ready, Started: ptr(phase != corev1.PodPending), RestartCount: 0,
		}
		switch phase {
		case corev1.PodRunning:
			startedAt := now
			if len(current.Status.ContainerStatuses) == 1 && current.Status.ContainerStatuses[0].State.Running != nil {
				startedAt = current.Status.ContainerStatuses[0].State.Running.StartedAt
			}
			containerStatus.State.Running = &corev1.ContainerStateRunning{StartedAt: startedAt}
		case corev1.PodSucceeded, corev1.PodFailed:
			startedAt := now
			if current.Status.StartTime != nil {
				startedAt = *current.Status.StartTime
			}
			containerStatus.State.Terminated = &corev1.ContainerStateTerminated{
				ExitCode: state.exitCode, Reason: state.reason, Message: state.message,
				StartedAt: startedAt, FinishedAt: now,
			}
		default:
			containerStatus.State.Waiting = &corev1.ContainerStateWaiting{Reason: state.reason, Message: state.message}
		}
		copy.Status.ContainerStatuses = []corev1.ContainerStatus{containerStatus}
		payload, err := json.Marshal(map[string]any{"status": copy.Status})
		if err != nil {
			return err
		}
		_, err = c.Kubernetes.CoreV1().Pods(copy.Namespace).Patch(ctx, copy.Name, types.StrategicMergePatchType, payload, metav1.PatchOptions{}, "status")
		return err
	})
}

func (c *Controller) garbageCollect(ctx context.Context, active map[string]projectionRef) error {
	selector := LabelProjection + "=" + ProjectionValue
	var errs []error
	blockedNodes := make(map[string]bool)
	pods, err := c.Kubernetes.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return fmt.Errorf("list projection Pods for garbage collection: %w", err)
	} else {
		for index := range pods.Items {
			pod := &pods.Items[index]
			ref, ok := active[pod.Labels[LabelAssignmentUID]]
			if ok && ref.PodNamespace == pod.Namespace && ref.PodName == pod.Name {
				continue
			}
			expected := refForUID(pod.Labels[LabelAssignmentUID])
			if expected.PodName != pod.Name || pod.Annotations[AnnotationContract] != ContractValue {
				errs = append(errs, fmt.Errorf("refusing to garbage collect Pod %s/%s outside the projection contract", pod.Namespace, pod.Name))
				blockedNodes[expected.NodeName] = true
				continue
			}
			uid := pod.UID
			if err := c.Kubernetes.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, err)
				blockedNodes[expected.NodeName] = true
			}
		}
	}
	nodes, err := c.Kubernetes.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		errs = append(errs, err)
	} else {
		for index := range nodes.Items {
			node := &nodes.Items[index]
			ref, ok := active[node.Labels[LabelAssignmentUID]]
			if ok && ref.NodeName == node.Name {
				continue
			}
			if blockedNodes[node.Name] {
				continue
			}
			if refForUID(node.Labels[LabelAssignmentUID]).NodeName != node.Name || node.Annotations[AnnotationContract] != ContractValue {
				errs = append(errs, fmt.Errorf("refusing to garbage collect Node %s outside the projection contract", node.Name))
				continue
			}
			uid := node.UID
			if err := c.Kubernetes.CoreV1().Nodes().Delete(ctx, node.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Controller) deleteProjection(ctx context.Context, ref projectionRef) error {
	var errs []error
	if err := c.deleteManagedPod(ctx, ref); err != nil {
		errs = append(errs, err)
	}
	if err := c.deleteManagedNode(ctx, ref); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (c *Controller) deleteManagedPod(ctx context.Context, ref projectionRef) error {
	pods := c.Kubernetes.CoreV1().Pods(ref.PodNamespace)
	pod, err := pods.Get(ctx, ref.PodName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if pod.Labels[LabelProjection] != ProjectionValue || pod.Annotations[AnnotationContract] != ContractValue || refForUID(pod.Labels[LabelAssignmentUID]).PodName != ref.PodName {
		return fmt.Errorf("refusing to delete Pod %s/%s outside the projection contract", ref.PodNamespace, ref.PodName)
	}
	uid := pod.UID
	return pods.Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
}

func (c *Controller) deleteManagedNode(ctx context.Context, ref projectionRef) error {
	nodes := c.Kubernetes.CoreV1().Nodes()
	node, err := nodes.Get(ctx, ref.NodeName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if node.Labels[LabelProjection] != ProjectionValue || node.Annotations[AnnotationContract] != ContractValue || refForUID(node.Labels[LabelAssignmentUID]).NodeName != ref.NodeName {
		return fmt.Errorf("refusing to delete Node %s outside the projection contract", ref.NodeName)
	}
	uid := node.UID
	return nodes.Delete(ctx, node.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

type projectionState struct {
	podPhase   corev1.PodPhase
	nodeStatus corev1.ConditionStatus
	ready      bool
	reason     string
	message    string
	exitCode   int32
}

func stateFor(assignment *nativev1alpha1.IdleloomWorkloadAssignment, now time.Time) projectionState {
	if assignment.Spec.DesiredState == nativev1alpha1.AssignmentDesiredStopped {
		return projectionState{podPhase: corev1.PodPending, nodeStatus: corev1.ConditionFalse, reason: "IdleloomStopping", message: "the native execution is stopping"}
	}
	if assignment.Status.Phase == nativev1alpha1.PhaseSucceeded {
		return projectionState{podPhase: corev1.PodSucceeded, nodeStatus: corev1.ConditionFalse, reason: "IdleloomExecutionSucceeded", message: "the native execution completed successfully"}
	}
	if assignment.Status.Phase == nativev1alpha1.PhaseFailed {
		return projectionState{podPhase: corev1.PodFailed, nodeStatus: corev1.ConditionFalse, reason: "IdleloomExecutionFailed", message: "the native execution failed", exitCode: 1}
	}
	if heartbeatStale(assignment, now) {
		return projectionState{podPhase: corev1.PodPending, nodeStatus: corev1.ConditionUnknown, reason: "IdleloomLeaseExpired", message: "the native execution heartbeat expired"}
	}
	switch assignment.Status.Phase {
	case nativev1alpha1.PhaseRunning:
		return projectionState{podPhase: corev1.PodRunning, nodeStatus: corev1.ConditionTrue, ready: true, reason: "IdleloomAssignmentRunning", message: "the sandboxed native Metal execution is running"}
	case nativev1alpha1.PhaseFenced:
		return projectionState{podPhase: corev1.PodPending, nodeStatus: corev1.ConditionUnknown, reason: "IdleloomExecutionFenced", message: "the native execution was fenced"}
	case nativev1alpha1.PhaseBlocked:
		return projectionState{podPhase: corev1.PodPending, nodeStatus: corev1.ConditionFalse, reason: "IdleloomExecutionBlocked", message: "the native execution is blocked by host resource ownership"}
	default:
		return projectionState{podPhase: corev1.PodPending, nodeStatus: corev1.ConditionFalse, reason: "IdleloomPreparing", message: "the native execution is preparing"}
	}
}

func heartbeatStale(assignment *nativev1alpha1.IdleloomWorkloadAssignment, now time.Time) bool {
	last := assignment.CreationTimestamp.Time
	if assignment.Status.LastHeartbeatTime != nil {
		last = assignment.Status.LastHeartbeatTime.Time
	}
	if last.IsZero() {
		return false
	}
	lease := time.Duration(assignment.Spec.LeaseDurationSeconds) * time.Second
	return last.After(now.Add(nativev1alpha1.HeartbeatClockSkewAllowance)) || now.Sub(last) > lease+nativev1alpha1.HeartbeatClockSkewAllowance
}

func projectionComplete(assignment *nativev1alpha1.IdleloomWorkloadAssignment) bool {
	return assignment.Spec.DesiredState == nativev1alpha1.AssignmentDesiredStopped && nativev1alpha1.ValidateStopAcknowledgement(assignment) == nil
}

func refFor(assignment *nativev1alpha1.IdleloomWorkloadAssignment) projectionRef {
	ref := refForUID(string(assignment.UID))
	ref.PodNamespace = assignment.Spec.WorkloadRef.Namespace
	return ref
}

func refForUID(uid string) projectionRef {
	suffix := compactUID(types.UID(uid))
	return projectionRef{NodeName: "idleloom-" + suffix, PodName: "idleloom-" + suffix}
}

func compactUID(uid types.UID) string {
	value := strings.ReplaceAll(strings.ToLower(string(uid)), "-", "")
	if len(value) > 20 {
		value = value[:20]
	}
	return value
}

func projectionLabels(assignment *nativev1alpha1.IdleloomWorkloadAssignment) map[string]string {
	return map[string]string{
		LabelProjection: ProjectionValue, LabelAssignmentUID: string(assignment.UID),
		LabelWorkloadUID: string(assignment.Spec.WorkloadRef.UID), LabelExecutionID: assignment.Spec.ExecutionID,
	}
}

func projectionAnnotations(assignment *nativev1alpha1.IdleloomWorkloadAssignment, logsReady bool) map[string]string {
	annotations := map[string]string{
		AnnotationContract: ContractValue, AnnotationKubeletAPI: "none", AnnotationLogs: "unsupported",
		AnnotationExec: "unsupported", AnnotationPortForward: "unsupported", AnnotationConnectivity: "outbound-only",
	}
	if assignment.Spec.Model != nil {
		annotations[AnnotationModelArtifact] = assignment.Spec.Model.Artifact.OCIReference
	}
	if logsReady {
		annotations[AnnotationKubeletAPI] = "logs-only"
		annotations[AnnotationLogs] = "supported"
		annotations[AnnotationConnectivity] = "wirekube-relay"
	}
	return annotations
}

func connectedKubeletAddress(host *nativev1alpha1.IdleloomHost) (string, bool) {
	if host == nil || host.Status.Connectivity == nil || host.Status.Connectivity.Mode != nativev1alpha1.ConnectivityModeWireKubeLeaf {
		return "", false
	}
	condition := apiMeta.FindStatusCondition(host.Status.Conditions, nativev1alpha1.HostConditionConnected)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		return "", false
	}
	ip, network, err := net.ParseCIDR(host.Status.Connectivity.Address)
	if err != nil || ip.To4() == nil {
		return "", false
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones != 32 {
		return "", false
	}
	return ip.String(), true
}

func validateManagedPod(pod, desired *corev1.Pod, assignment *nativev1alpha1.IdleloomWorkloadAssignment) error {
	if pod.Name != desired.Name || pod.Namespace != desired.Namespace || pod.Labels[LabelProjection] != ProjectionValue || pod.Labels[LabelAssignmentUID] != string(assignment.UID) {
		return fmt.Errorf("Pod %s/%s exists and is not the expected projection", pod.Namespace, pod.Name)
	}
	actualSpec := normalizedProjectionSpec(pod.Spec)
	desiredSpec := normalizedProjectionSpec(desired.Spec)
	if pod.Annotations[AnnotationContract] != ContractValue || !apiequality.Semantic.DeepEqual(pod.OwnerReferences, desired.OwnerReferences) || !apiequality.Semantic.DeepEqual(actualSpec, desiredSpec) {
		return fmt.Errorf("Pod %s/%s was mutated outside the projection contract", pod.Namespace, pod.Name)
	}
	return nil
}

func normalizedProjectionSpec(spec corev1.PodSpec) corev1.PodSpec {
	copy := *spec.DeepCopy()
	// Default ServiceAccounts may inject registry credentials. Projection Pods
	// never execute through a container runtime, so this default is irrelevant
	// to the ownership contract.
	copy.ImagePullSecrets = nil
	if copy.SecurityContext != nil && apiequality.Semantic.DeepEqual(*copy.SecurityContext, corev1.PodSecurityContext{}) {
		copy.SecurityContext = nil
	}
	filtered := copy.Tolerations[:0]
	for _, toleration := range copy.Tolerations {
		if isDefaultNodeToleration(toleration) || isDefaultExtendedResourceToleration(toleration, copy) {
			continue
		}
		filtered = append(filtered, toleration)
	}
	copy.Tolerations = filtered
	return copy
}

func isDefaultNodeToleration(toleration corev1.Toleration) bool {
	if toleration.Operator != corev1.TolerationOpExists || toleration.Effect != corev1.TaintEffectNoExecute || toleration.TolerationSeconds == nil || *toleration.TolerationSeconds != 300 {
		return false
	}
	return toleration.Key == corev1.TaintNodeNotReady || toleration.Key == corev1.TaintNodeUnreachable
}

func isDefaultExtendedResourceToleration(toleration corev1.Toleration, spec corev1.PodSpec) bool {
	if toleration.Operator != corev1.TolerationOpExists || toleration.Effect != corev1.TaintEffectNoSchedule || toleration.Value != "" || toleration.TolerationSeconds != nil {
		return false
	}
	resourceName := corev1.ResourceName(toleration.Key)
	for _, container := range append(spec.InitContainers, spec.Containers...) {
		if _, ok := container.Resources.Requests[resourceName]; ok {
			return true
		}
		if _, ok := container.Resources.Limits[resourceName]; ok {
			return true
		}
	}
	return false
}

func podConditions(state projectionState, now metav1.Time, existing []corev1.PodCondition) []corev1.PodCondition {
	ready := corev1.ConditionFalse
	if state.ready {
		ready = corev1.ConditionTrue
	}
	conditions := []corev1.PodCondition{
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: "IdleloomPrebound", Message: "the projection controller assigned the managed Pod"},
		{Type: corev1.PodInitialized, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: "IdleloomManagedProjection"},
		{Type: corev1.ContainersReady, Status: ready, LastTransitionTime: now, Reason: state.reason, Message: state.message},
		{Type: corev1.PodReady, Status: ready, LastTransitionTime: now, Reason: state.reason, Message: state.message},
	}
	for index := range conditions {
		for _, previous := range existing {
			if previous.Type == conditions[index].Type && previous.Status == conditions[index].Status && previous.Reason == conditions[index].Reason {
				conditions[index].LastTransitionTime = previous.LastTransitionTime
				break
			}
		}
	}
	return conditions
}

func ptr[T any](value T) *T { return &value }
