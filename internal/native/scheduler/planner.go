package scheduler

import (
	"crypto/rand"
	"fmt"
	"sort"
	"strings"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultLeaseDuration = 30 * time.Second

type Planner struct {
	Now              func() time.Time
	HeartbeatTimeout time.Duration
	LeaseDuration    time.Duration
	NewExecutionID   func() (string, error)
}

type NoEligibleHostsError struct {
	Reasons []string
}

func (e *NoEligibleHostsError) Error() string {
	if len(e.Reasons) == 0 {
		return "no Idleloom hosts are registered"
	}
	return "no eligible Idleloom host: " + strings.Join(e.Reasons, "; ")
}

func (p Planner) SelectHost(workload *nativev1alpha1.IdleloomWorkload, model *nativev1alpha1.IdleloomModel, hosts []nativev1alpha1.IdleloomHost) (*nativev1alpha1.IdleloomHost, error) {
	if err := nativev1alpha1.ValidateWorkload(workload); err != nil {
		return nil, fmt.Errorf("validate workload: %w", err)
	}
	if workload.Spec.Mode == nativev1alpha1.WorkloadModeServer || workload.Spec.Mode == nativev1alpha1.WorkloadModeBatch {
		if model == nil {
			return nil, fmt.Errorf("model workload requires a resolved model")
		}
		if err := nativev1alpha1.ValidateModel(model); err != nil {
			return nil, fmt.Errorf("validate model: %w", err)
		}
		if workload.Spec.Model.CatalogRef != model.Name {
			return nil, fmt.Errorf("workload requested model %q but controller resolved %q", workload.Spec.Model.CatalogRef, model.Name)
		}
	} else if model != nil {
		return nil, fmt.Errorf("shell workload must not resolve a model")
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	heartbeatTimeout := p.HeartbeatTimeout
	if heartbeatTimeout <= 0 {
		heartbeatTimeout = 45 * time.Second
	}
	request := workload.Spec.Resources.UnifiedMemoryRequest.DeepCopy()
	if model != nil {
		request = nativev1alpha1.EffectiveUnifiedMemoryRequest(request, model.Spec.MinimumUnifiedMemory)
	}

	type candidate struct {
		host nativev1alpha1.IdleloomHost
	}
	var candidates []candidate
	var reasons []string
	for _, host := range hosts {
		if reason := hostIneligible(host, workload, model, request, now, heartbeatTimeout); reason != "" {
			reasons = append(reasons, fmt.Sprintf("%s/%s: %s", host.Namespace, host.Name, reason))
			continue
		}
		candidates = append(candidates, candidate{host: host})
	}
	if len(candidates) == 0 {
		sort.Strings(reasons)
		return nil, &NoEligibleHostsError{Reasons: reasons}
	}
	sort.Slice(candidates, func(i, j int) bool {
		left := candidates[i].host.Status.AvailableUnifiedMemory
		right := candidates[j].host.Status.AvailableUnifiedMemory
		if cmp := left.Cmp(right); cmp != 0 {
			return cmp < 0
		}
		leftName := candidates[i].host.Namespace + "/" + candidates[i].host.Name
		rightName := candidates[j].host.Namespace + "/" + candidates[j].host.Name
		return leftName < rightName
	})
	selected := candidates[0].host.DeepCopy()
	return selected, nil
}

func (p Planner) PlanAssignment(workload *nativev1alpha1.IdleloomWorkload, model *nativev1alpha1.IdleloomModel, host *nativev1alpha1.IdleloomHost, fencingEpoch int64) (*nativev1alpha1.IdleloomWorkloadAssignment, error) {
	if workload.UID == "" || workload.Generation < 1 {
		return nil, fmt.Errorf("workload UID and positive generation are required")
	}
	if model != nil && model.UID == "" {
		return nil, fmt.Errorf("model UID is required")
	}
	if host.UID == "" || host.Namespace == "" {
		return nil, fmt.Errorf("host UID and namespace are required")
	}
	if fencingEpoch < 1 {
		return nil, fmt.Errorf("fencing epoch must be positive")
	}
	if _, err := p.SelectHost(workload, model, []nativev1alpha1.IdleloomHost{*host}); err != nil {
		return nil, err
	}
	newExecutionID := p.NewExecutionID
	if newExecutionID == nil {
		newExecutionID = randomUUID
	}
	executionID, err := newExecutionID()
	if err != nil {
		return nil, fmt.Errorf("generate execution ID: %w", err)
	}
	leaseDuration := p.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = defaultLeaseDuration
	}
	if leaseDuration < 10*time.Second || leaseDuration > 300*time.Second || leaseDuration%time.Second != 0 {
		return nil, fmt.Errorf("lease duration must be a whole number of seconds between 10s and 5m")
	}
	memoryRequest := workload.Spec.Resources.UnifiedMemoryRequest.DeepCopy()
	if model != nil {
		memoryRequest = nativev1alpha1.EffectiveUnifiedMemoryRequest(memoryRequest, model.Spec.MinimumUnifiedMemory)
	}
	assignment := &nativev1alpha1.IdleloomWorkloadAssignment{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomWorkloadAssignment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      nativev1alpha1.AssignmentMailboxName,
			Namespace: host.Namespace,
			Labels: map[string]string{
				"ai.idleloom.io/host-namespace": host.Namespace,
				"ai.idleloom.io/workload-uid":   string(workload.UID),
				"app.kubernetes.io/managed-by":  "idleloom-controller",
			},
		},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef: nativev1alpha1.WorkloadObjectReference{
				Namespace:  workload.Namespace,
				Name:       workload.Name,
				UID:        workload.UID,
				Generation: workload.Generation,
			},
			HostRef:              nativev1alpha1.ObjectReference{Name: host.Name, UID: host.UID},
			ExecutionID:          executionID,
			FencingEpoch:         fencingEpoch,
			LeaseDurationSeconds: int32(leaseDuration / time.Second),
		},
	}
	if model != nil {
		assignment.Spec.Model = &nativev1alpha1.ResolvedModel{
			CatalogRef:            nativev1alpha1.ObjectReference{Name: model.Name, UID: model.UID},
			Family:                model.Spec.Family,
			RuntimeProfile:        model.Spec.RuntimeProfile,
			Artifact:              model.Spec.Artifact,
			UnifiedMemoryRequest:  memoryRequest,
			MaxContextLength:      model.Spec.MaxContextLength,
			MaxConcurrentRequests: model.Spec.MaxConcurrentRequests,
		}
		if workload.Spec.Batch != nil {
			batch := *workload.Spec.Batch
			if batch.TimeoutSeconds == 0 {
				batch.TimeoutSeconds = 600
			}
			assignment.Spec.Model.Batch = &batch
		}
	} else {
		isolation := workload.Spec.Shell.Isolation
		if isolation == "" {
			isolation = nativev1alpha1.ShellIsolationSandbox
		}
		network := workload.Spec.Shell.Network
		if network == "" {
			network = nativev1alpha1.ShellNetworkNone
			if isolation == nativev1alpha1.ShellIsolationHost {
				network = nativev1alpha1.ShellNetworkOutbound
			}
		}
		timeout := workload.Spec.Shell.TimeoutSeconds
		if timeout == 0 {
			timeout = 3600
		}
		assignment.Spec.Shell = &nativev1alpha1.ResolvedShell{
			Script: workload.Spec.Shell.Script, Isolation: isolation, Network: network,
			TimeoutSeconds: timeout, UnifiedMemoryRequest: memoryRequest,
		}
	}
	if err := nativev1alpha1.ValidateAssignment(assignment); err != nil {
		return nil, fmt.Errorf("validate planned assignment: %w", err)
	}
	return assignment, nil
}

func hostIneligible(host nativev1alpha1.IdleloomHost, workload *nativev1alpha1.IdleloomWorkload, model *nativev1alpha1.IdleloomModel, request resource.Quantity, now time.Time, heartbeatTimeout time.Duration) string {
	if host.DeletionTimestamp != nil {
		return "host is deleting"
	}
	if host.UID == "" || host.Namespace == "" {
		return "host identity is incomplete"
	}
	if err := nativev1alpha1.ValidateHost(&host); err != nil {
		return err.Error()
	}
	if host.Status.ObservedGeneration != host.Generation {
		return "agent status has not observed the current host generation"
	}
	if host.Status.ProtocolVersion != nativev1alpha1.AgentProtocolV1Alpha1 {
		return fmt.Sprintf("unsupported agent protocol %q", host.Status.ProtocolVersion)
	}
	ready := apiMeta.FindStatusCondition(host.Status.Conditions, nativev1alpha1.HostConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return "agent is not Ready"
	}
	if ready.ObservedGeneration != host.Generation {
		return "Ready condition has not observed the current host generation"
	}
	if host.Status.LastHeartbeatTime == nil {
		return "agent heartbeat is missing"
	}
	heartbeat := host.Status.LastHeartbeatTime.Time
	if heartbeat.After(now.Add(nativev1alpha1.HeartbeatClockSkewAllowance)) {
		return "agent heartbeat is too far in the future"
	}
	if now.Sub(heartbeat) > heartbeatTimeout+nativev1alpha1.HeartbeatClockSkewAllowance {
		return "agent heartbeat is stale"
	}
	if host.Status.KrunkitState != nativev1alpha1.KrunkitStateStopped {
		return fmt.Sprintf("krunkit worker is %s", host.Status.KrunkitState)
	}
	if host.Status.VulkanLeaseActive {
		return "a Vulkan lease is active"
	}
	if host.Status.ActiveAssignmentUID != "" {
		return "another native assignment is active"
	}
	if model != nil {
		if !contains(host.Status.RuntimeProfiles, model.Spec.RuntimeProfile) {
			return fmt.Sprintf("runtime %s is not supported", model.Spec.RuntimeProfile)
		}
		if !contains(host.Status.ModelFamilies, model.Spec.Family) {
			return fmt.Sprintf("model family %s is not supported", model.Spec.Family)
		}
		if workload.Spec.Mode == nativev1alpha1.WorkloadModeBatch && !contains(host.Status.Capabilities, nativev1alpha1.CapabilityBatchInferenceV1) {
			return "agent does not support Native batch inference"
		}
	} else if workload.Spec.Shell != nil {
		access := host.Spec.ShellAccess
		if access == "" {
			access = nativev1alpha1.ShellAccessDisabled
		}
		isolation := workload.Spec.Shell.Isolation
		if isolation == "" {
			isolation = nativev1alpha1.ShellIsolationSandbox
		}
		if isolation == nativev1alpha1.ShellIsolationHost && access != nativev1alpha1.ShellAccessHost {
			return "host shell access is not enabled"
		}
		if isolation == nativev1alpha1.ShellIsolationSandbox && access != nativev1alpha1.ShellAccessSandboxed && access != nativev1alpha1.ShellAccessHost {
			return "sandboxed shell access is not enabled"
		}
	}
	if host.Status.AllocatableUnifiedMemory.Cmp(request) < 0 {
		return fmt.Sprintf("allocatable unified memory %s is less than request %s", host.Status.AllocatableUnifiedMemory.String(), request.String())
	}
	if host.Status.AvailableUnifiedMemory.Cmp(request) < 0 {
		return fmt.Sprintf("available unified memory %s is less than request %s", host.Status.AvailableUnifiedMemory.String(), request.String())
	}
	return ""
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func randomUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
