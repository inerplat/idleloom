package scheduler

import (
	"strings"
	"testing"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestSelectHostUsesSmallestEligibleExclusiveHost(t *testing.T) {
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	planner := Planner{Now: func() time.Time { return now }}
	workload := testWorkload()
	model := testModel()
	small := testHost("host-small", "24Gi", now)
	large := testHost("host-large", "64Gi", now)
	selected, err := planner.SelectHost(&workload, &model, []nativev1alpha1.IdleloomHost{large, small})
	if err != nil {
		t.Fatalf("SelectHost: %v", err)
	}
	if selected.Namespace != small.Namespace {
		t.Fatalf("SelectHost selected %s, want %s", selected.Namespace, small.Namespace)
	}
}

func TestSelectHostRejectsUnsafeResourceStates(t *testing.T) {
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	workload := testWorkload()
	model := testModel()
	tests := []struct {
		name   string
		mutate func(*nativev1alpha1.IdleloomHost)
	}{
		{name: "krunkit-running", mutate: func(h *nativev1alpha1.IdleloomHost) { h.Status.KrunkitState = nativev1alpha1.KrunkitStateRunning }},
		{name: "vulkan-lease", mutate: func(h *nativev1alpha1.IdleloomHost) { h.Status.VulkanLeaseActive = true }},
		{name: "native-busy", mutate: func(h *nativev1alpha1.IdleloomHost) { h.Status.ActiveAssignmentUID = types.UID("busy") }},
		{name: "stale", mutate: func(h *nativev1alpha1.IdleloomHost) {
			h.Status.LastHeartbeatTime = microTime(now.Add(-2 * time.Minute))
		}},
		{name: "memory", mutate: func(h *nativev1alpha1.IdleloomHost) { h.Status.AvailableUnifiedMemory = resource.MustParse("8Gi") }},
		{name: "protocol", mutate: func(h *nativev1alpha1.IdleloomHost) { h.Status.ProtocolVersion = "old" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := testHost("studio", "32Gi", now)
			test.mutate(&host)
			_, err := (Planner{Now: func() time.Time { return now }}).SelectHost(&workload, &model, []nativev1alpha1.IdleloomHost{host})
			if err == nil {
				t.Fatal("SelectHost accepted an unsafe host")
			}
		})
	}
}

func TestSelectHostRejectsWrongCatalogResolution(t *testing.T) {
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	workload := testWorkload()
	model := testModel()
	model.Name = "different-model"
	host := testHost("studio", "32Gi", now)
	if _, err := (Planner{Now: func() time.Time { return now }}).SelectHost(&workload, &model, []nativev1alpha1.IdleloomHost{host}); err == nil {
		t.Fatal("SelectHost accepted a model other than the requested catalog entry")
	}
}

func TestSelectHostAllowsBoundedPastClockSkew(t *testing.T) {
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	host := testHost("studio", "32Gi", now)
	host.Status.LastHeartbeatTime = microTime(now.Add(-45*time.Second - nativev1alpha1.HeartbeatClockSkewAllowance + time.Second))
	workload := testWorkload()
	model := testModel()
	if _, err := (Planner{Now: func() time.Time { return now }}).SelectHost(&workload, &model, []nativev1alpha1.IdleloomHost{host}); err != nil {
		t.Fatalf("bounded clock skew made host ineligible: %v", err)
	}
}

func TestPlanAssignmentCarriesFencingIdentityAndResolvedCatalog(t *testing.T) {
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	planner := Planner{
		Now:            func() time.Time { return now },
		NewExecutionID: func() (string, error) { return "123e4567-e89b-42d3-a456-426614174000", nil },
	}
	workload := testWorkload()
	model := testModel()
	host := testHost("studio", "32Gi", now)
	assignment, err := planner.PlanAssignment(&workload, &model, &host, 7)
	if err != nil {
		t.Fatalf("PlanAssignment: %v", err)
	}
	if assignment.Namespace != host.Namespace || assignment.Spec.FencingEpoch != 7 {
		t.Fatalf("unexpected assignment target: namespace=%s epoch=%d", assignment.Namespace, assignment.Spec.FencingEpoch)
	}
	if assignment.Name != nativev1alpha1.AssignmentMailboxName {
		t.Fatalf("assignment name = %s, want singleton mailbox %s", assignment.Name, nativev1alpha1.AssignmentMailboxName)
	}
	if assignment.Spec.Model.CatalogRef.UID != model.UID || assignment.Spec.Model.Artifact.ManifestDigest != model.Spec.Artifact.ManifestDigest {
		t.Fatal("assignment did not freeze the resolved catalog identity")
	}
	if assignment.Spec.Model.UnifiedMemoryRequest.Cmp(resource.MustParse("16Gi")) != 0 {
		t.Fatalf("memory request = %s, want 16Gi", assignment.Spec.Model.UnifiedMemoryRequest.String())
	}
}

func TestShellAccessNeverExceedsHostEnrollment(t *testing.T) {
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	planner := Planner{Now: func() time.Time { return now }}
	workload := shellWorkload(nativev1alpha1.ShellIsolationHost)
	host := testHost("studio", "32Gi", now)
	host.Status.RuntimeProfiles = append(host.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileShellV1)
	host.Spec.ShellAccess = nativev1alpha1.ShellAccessSandboxed
	if _, err := planner.SelectHost(&workload, nil, []nativev1alpha1.IdleloomHost{host}); err == nil {
		t.Fatal("sandbox-only host accepted a full host shell")
	}
	host.Spec.ShellAccess = nativev1alpha1.ShellAccessHost
	if _, err := planner.SelectHost(&workload, nil, []nativev1alpha1.IdleloomHost{host}); err != nil {
		t.Fatalf("host shell enrollment rejected a host shell: %v", err)
	}
	workload = shellWorkload(nativev1alpha1.ShellIsolationSandbox)
	if _, err := planner.SelectHost(&workload, nil, []nativev1alpha1.IdleloomHost{host}); err != nil {
		t.Fatalf("host shell enrollment rejected a lower sandbox privilege: %v", err)
	}
}

func TestPlanShellAssignmentFreezesExecutionPolicy(t *testing.T) {
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	planner := Planner{
		Now:            func() time.Time { return now },
		NewExecutionID: func() (string, error) { return "123e4567-e89b-42d3-a456-426614174000", nil },
	}
	workload := shellWorkload(nativev1alpha1.ShellIsolationHost)
	host := testHost("studio", "32Gi", now)
	host.Spec.ShellAccess = nativev1alpha1.ShellAccessHost
	host.Status.RuntimeProfiles = append(host.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileShellV1)
	assignment, err := planner.PlanAssignment(&workload, nil, &host, 3)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.Spec.Model != nil || assignment.Spec.Shell == nil || assignment.Spec.Shell.Script != "echo ready" || assignment.Spec.Shell.Isolation != nativev1alpha1.ShellIsolationHost {
		t.Fatalf("shell assignment = %#v", assignment.Spec)
	}
}

func TestPlanHostShellDefaultsNetworkToOutbound(t *testing.T) {
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	planner := Planner{
		Now:            func() time.Time { return now },
		NewExecutionID: func() (string, error) { return "123e4567-e89b-42d3-a456-426614174000", nil },
	}
	workload := shellWorkload(nativev1alpha1.ShellIsolationHost)
	workload.Spec.Shell.Network = ""
	host := testHost("studio", "32Gi", now)
	host.Spec.ShellAccess = nativev1alpha1.ShellAccessHost
	host.Status.RuntimeProfiles = append(host.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileShellV1)
	assignment, err := planner.PlanAssignment(&workload, nil, &host, 3)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.Spec.Shell.Network != nativev1alpha1.ShellNetworkOutbound {
		t.Fatalf("network = %q, want Outbound", assignment.Spec.Shell.Network)
	}
}

func shellWorkload(isolation string) nativev1alpha1.IdleloomWorkload {
	network := nativev1alpha1.ShellNetworkNone
	if isolation == nativev1alpha1.ShellIsolationHost {
		network = nativev1alpha1.ShellNetworkOutbound
	}
	return nativev1alpha1.IdleloomWorkload{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "shell", UID: types.UID("shell-uid"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadSpec{
			Mode: nativev1alpha1.WorkloadModeShell,
			Shell: &nativev1alpha1.WorkloadShell{
				Script: "echo ready", Isolation: isolation,
				Network: network, TimeoutSeconds: 30,
			},
			Resources: nativev1alpha1.WorkloadResources{UnifiedMemoryRequest: resource.MustParse("1Gi")},
		},
	}
}

func testWorkload() nativev1alpha1.IdleloomWorkload {
	return nativev1alpha1.IdleloomWorkload{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "qwen", UID: types.UID("workload-uid"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadSpec{
			Mode:      nativev1alpha1.WorkloadModeServer,
			Model:     &nativev1alpha1.WorkloadModelReference{CatalogRef: "qwen-approved"},
			Resources: nativev1alpha1.WorkloadResources{UnifiedMemoryRequest: resource.MustParse("16Gi")},
		},
	}
}

func testModel() nativev1alpha1.IdleloomModel {
	digest := "sha256:" + strings.Repeat("a", 64)
	return nativev1alpha1.IdleloomModel{
		ObjectMeta: metav1.ObjectMeta{Name: "qwen-approved", UID: types.UID("model-uid")},
		Spec: nativev1alpha1.IdleloomModelSpec{
			Family:         nativev1alpha1.ModelFamilyQwen35,
			RuntimeProfile: nativev1alpha1.RuntimeProfileMLXLMV1,
			Artifact: nativev1alpha1.ModelArtifact{
				OCIReference:   "oci://registry.example/models/qwen@" + digest,
				ManifestDigest: digest,
				Format:         nativev1alpha1.ArtifactFormatSafetensorsV1,
				SizeBytes:      1024,
				Signature:      nativev1alpha1.SignaturePolicy{Issuer: "https://issuer.example", Subject: "publisher"},
			},
			MinimumUnifiedMemory:  resource.MustParse("12Gi"),
			MaxContextLength:      2048,
			MaxConcurrentRequests: 1,
		},
	}
}

func testHost(name, memory string, now time.Time) nativev1alpha1.IdleloomHost {
	return nativev1alpha1.IdleloomHost{
		ObjectMeta: metav1.ObjectMeta{Namespace: "idleloom-host-" + name, Name: "host", UID: types.UID(name + "-uid"), Generation: 1},
		Spec:       nativev1alpha1.IdleloomHostSpec{AgentID: name},
		Status: nativev1alpha1.IdleloomHostStatus{
			ObservedGeneration:       1,
			ProtocolVersion:          nativev1alpha1.AgentProtocolV1Alpha1,
			RuntimeProfiles:          []string{nativev1alpha1.RuntimeProfileMLXLMV1},
			ModelFamilies:            []string{nativev1alpha1.ModelFamilyQwen35},
			AllocatableUnifiedMemory: resource.MustParse(memory),
			AvailableUnifiedMemory:   resource.MustParse(memory),
			KrunkitState:             nativev1alpha1.KrunkitStateStopped,
			LastHeartbeatTime:        microTime(now),
			Conditions:               []metav1.Condition{{Type: nativev1alpha1.HostConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1, Reason: "AgentReady", LastTransitionTime: metav1.NewTime(now)}},
		},
	}
}

func microTime(value time.Time) *metav1.MicroTime {
	time := metav1.NewMicroTime(value)
	return &time
}
