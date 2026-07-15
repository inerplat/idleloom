package native_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestNativeCRDsExposeOnlyTheRestrictedWorkloadIntent(t *testing.T) {
	crdDir := filepath.Join("..", "..", "deploy", "native", "crds")
	entries, err := os.ReadDir(crdDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		objects := decodeObjects(t, filepath.Join(crdDir, entry.Name()))
		if len(objects) != 1 || objects[0].GetKind() != "CustomResourceDefinition" {
			t.Fatalf("%s does not contain exactly one CRD", entry.Name())
		}
		names = append(names, objects[0].GetName())
	}
	sort.Strings(names)
	want := []string{
		"idleloomhosts.ai.idleloom.io",
		"idleloommodels.ai.idleloom.io",
		"idleloomworkloadassignments.ai.idleloom.io",
		"idleloomworkloads.ai.idleloom.io",
	}
	if len(names) != len(want) {
		t.Fatalf("CRDs = %v, want %v", names, want)
	}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("CRDs = %v, want %v", names, want)
		}
	}

	workload := decodeObjects(t, filepath.Join(crdDir, "ai.idleloom.io_idleloomworkloads.yaml"))[0]
	rootSchema := crdSchema(t, workload)
	specProperties := nestedMap(t, rootSchema, "properties", "spec", "properties")
	assertKeys(t, specProperties, []string{"batch", "mode", "model", "resources", "run", "server", "shell", "train"})
	batchProperties := nestedMap(t, specProperties, "batch", "properties")
	assertKeys(t, batchProperties, []string{"maxTokens", "prompt", "timeoutSeconds"})
	modelProperties := nestedMap(t, specProperties, "model", "properties")
	assertKeys(t, modelProperties, []string{"catalogRef"})
	serverProperties := nestedMap(t, specProperties, "server", "properties")
	assertKeys(t, serverProperties, []string{"modelAlias", "serviceName"})
	resourceProperties := nestedMap(t, specProperties, "resources", "properties")
	assertKeys(t, resourceProperties, []string{"unifiedMemoryRequest"})
	shellProperties := nestedMap(t, specProperties, "shell", "properties")
	assertKeys(t, shellProperties, []string{"isolation", "network", "script", "timeoutSeconds"})
	trainProperties := nestedMap(t, specProperties, "train", "properties")
	assertKeys(t, trainProperties, []string{"network", "runtimeProfile", "source", "timeoutSeconds"})
	runProperties := nestedMap(t, specProperties, "run", "properties")
	assertKeys(t, runProperties, []string{"attempt", "experiment", "parameters", "task"})
	parameterSchema := nestedMap(t, runProperties, "parameters", "additionalProperties")
	if fmt.Sprint(parameterSchema["maxLength"]) != "4096" || parameterSchema["pattern"] != `^[^\x00]*$` {
		t.Fatalf("run parameter value schema = %#v", parameterSchema)
	}
	validations, ok := rootSchema["x-kubernetes-validations"].([]any)
	if !ok || len(validations) == 0 {
		t.Fatal("IdleloomWorkload CRD does not enforce immutable spec")
	}
	assertValidationRule(t, rootSchema, "self.spec == oldSelf.spec")
	assertValidationRule(t, rootSchema, "quantity(self.spec.resources.unifiedMemoryRequest).isGreaterThan(quantity('0'))")
	assertValidationRuleContains(t, rootSchema, "self.spec.mode == 'Server' && has(self.spec.model) && has(self.spec.server)")
	assertValidationRuleContains(t, rootSchema, "k.matches('^[A-Z][A-Z0-9_]{0,62}$') && !k.startsWith('IDLELOOM_')")
	assertValidationRule(t, rootSchema, "!has(self.spec.train) || !self.spec.train.source.inline.contains('\\u0000')")

	model := decodeObjects(t, filepath.Join(crdDir, "ai.idleloom.io_idleloommodels.yaml"))[0]
	modelSchema := crdSchema(t, model)
	assertValidationRule(t, modelSchema, "quantity(self.spec.minimumUnifiedMemory).isGreaterThan(quantity('0'))")
	assertValidationRuleContains(t, modelSchema, "self.spec.runtimeProfile == 'ollama-gguf-v1' && self.spec.family == 'ollama-gguf'")
	assertValidationRuleContains(t, modelSchema, "self.spec.runtimeProfile == 'llama-cpp-metal-v1' && self.spec.family == 'gguf'")
	modelSpecProperties := nestedMap(t, modelSchema, "properties", "spec", "properties")
	artifactSchema := nestedMap(t, modelSpecProperties, "artifact")
	artifactProperties := nestedMap(t, artifactSchema, "properties")
	assertKeys(t, artifactProperties, []string{"format", "ggufFile", "manifestDigest", "ociReference", "ollamaModel", "signature", "sizeBytes"})
	assertValidationRuleContains(t, artifactSchema, "has(self.ociReference) && !has(self.ollamaModel) && !has(self.ggufFile)")
	host := decodeObjects(t, filepath.Join(crdDir, "ai.idleloom.io_idleloomhosts.yaml"))[0]
	hostSchema := crdSchema(t, host)
	assertValidationRule(t, hostSchema, "self.metadata.name == 'host'")
	hostSpecProperties := nestedMap(t, hostSchema, "properties", "spec", "properties")
	assertKeys(t, hostSpecProperties, []string{"agentID", "shellAccess"})
	hostStatusProperties := nestedMap(t, hostSchema, "properties", "status", "properties")
	if _, ok := hostStatusProperties["availableModels"]; !ok {
		t.Fatal("IdleloomHost status does not expose exact locally available models")
	}
	assignment := decodeObjects(t, filepath.Join(crdDir, "ai.idleloom.io_idleloomworkloadassignments.yaml"))[0]
	assignmentSchema := crdSchema(t, assignment)
	assertValidationRule(t, assignmentSchema, "self.metadata.name == 'active'")
	assertValidationRule(t, assignmentSchema, "size(self.spec.workloadRef.uid) > 0 && size(self.spec.hostRef.uid) > 0")
	assertValidationRule(t, assignmentSchema, "(has(self.spec.model) && !has(self.spec.shell) && !has(self.spec.training)) || (!has(self.spec.model) && has(self.spec.shell) && !has(self.spec.training)) || (!has(self.spec.model) && !has(self.spec.shell) && has(self.spec.training))")
	assertValidationRule(t, assignmentSchema, "has(self.spec.model) == has(oldSelf.spec.model) && (!has(self.spec.model) || self.spec.model == oldSelf.spec.model)")
	assertValidationRule(t, assignmentSchema, "has(self.spec.shell) == has(oldSelf.spec.shell) && (!has(self.spec.shell) || self.spec.shell == oldSelf.spec.shell)")
	assertValidationRule(t, assignmentSchema, "has(self.spec.training) == has(oldSelf.spec.training) && (!has(self.spec.training) || self.spec.training == oldSelf.spec.training)")
	assertValidationRule(t, assignmentSchema, "has(self.spec.run) == has(oldSelf.spec.run) && (!has(self.spec.run) || self.spec.run == oldSelf.spec.run)")
	assertValidationRule(t, assignmentSchema, "!has(self.spec.model) || (size(self.spec.model.catalogRef.uid) > 0 && quantity(self.spec.model.unifiedMemoryRequest).isGreaterThan(quantity('0')))")
	assertValidationRule(t, assignmentSchema, "!has(self.spec.shell) || quantity(self.spec.shell.unifiedMemoryRequest).isGreaterThan(quantity('0'))")
	assertValidationRule(t, assignmentSchema, "!has(self.spec.training) || quantity(self.spec.training.unifiedMemoryRequest).isGreaterThan(quantity('0'))")
	assertValidationRule(t, assignmentSchema, "!has(self.spec.training) || (has(self.spec.run) && self.spec.run.task == 'train')")
	assertValidationRuleContains(t, assignmentSchema, "has(self.spec.model.server) && self.spec.run.task == 'serve'")
	assertValidationRuleContains(t, assignmentSchema, "self.spec.run.parameters.all(k, k.matches('^[A-Z][A-Z0-9_]{0,62}$')")
}

func TestProjectionImageUsesCanonicalCLIInternalRole(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile.projection"))
	if err != nil {
		t.Fatal(err)
	}
	entrypoint := string(data)
	if !strings.Contains(entrypoint, `ENTRYPOINT ["/usr/local/bin/idlectl", "internal", "projection", "--in-cluster"`) || strings.Contains(entrypoint, `"serve"`) {
		t.Fatalf("projection image entrypoint does not use the canonical internal role: %s", entrypoint)
	}
}

func TestBuildUsesOneCanonicalIdlectlBinary(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	build := string(data)
	if !strings.Contains(build, "bin/idlectl") {
		t.Fatal("Makefile does not build bin/idlectl")
	}
	for _, copyCommand := range []string{"cp bin/idlectl bin/idleloom-controller", "cp bin/idlectl bin/idleloom-agent", "cp bin/idlectl bin/idleloom-link", "cp bin/idlectl bin/idleloom-projection"} {
		if strings.Contains(build, copyCommand) {
			t.Fatalf("Makefile still creates role-specific binary copy with %q", copyCommand)
		}
	}
}

func TestAgentRoleCannotReadUserWorkloadsCredentialsOrNodes(t *testing.T) {
	objects := decodeObjects(t, filepath.Join("..", "..", "deploy", "native", "rbac", "agent-role.yaml"))
	var role *unstructured.Unstructured
	var binding *unstructured.Unstructured
	for _, object := range objects {
		if object.GetKind() == "Role" {
			role = object
		}
		if object.GetKind() == "RoleBinding" {
			binding = object
		}
	}
	if role == nil || binding == nil {
		t.Fatal("agent Role or RoleBinding was not found")
	}
	rules, found, err := unstructured.NestedSlice(role.Object, "rules")
	if err != nil || !found {
		t.Fatalf("read Role rules: found=%v err=%v", found, err)
	}
	type expectedRule struct {
		verbs         []string
		resourceNames []string
	}
	expected := map[string]expectedRule{
		"/serviceaccounts/token":                            {verbs: []string{"create"}, resourceNames: []string{"idleloom-agent"}},
		"/secrets":                                          {verbs: []string{"get"}, resourceNames: []string{"active-serve-auth"}},
		"ai.idleloom.io/idleloomhosts":                      {verbs: []string{"get"}, resourceNames: []string{"host"}},
		"ai.idleloom.io/idleloomhosts/status":               {verbs: []string{"get", "patch", "update"}, resourceNames: []string{"host"}},
		"ai.idleloom.io/idleloomworkloadassignments":        {verbs: []string{"get", "list", "watch"}},
		"ai.idleloom.io/idleloomworkloadassignments/status": {verbs: []string{"get", "patch", "update"}, resourceNames: []string{"active"}},
	}
	seen := make(map[string]bool, len(expected))
	for _, rawRule := range rules {
		rule := rawRule.(map[string]any)
		apiGroups, _, _ := unstructured.NestedStringSlice(rule, "apiGroups")
		resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
		verbs, _, _ := unstructured.NestedStringSlice(rule, "verbs")
		resourceNames, _, _ := unstructured.NestedStringSlice(rule, "resourceNames")
		if len(apiGroups) != 1 || len(resources) != 1 {
			t.Fatalf("agent rule must contain exactly one API group and resource: %#v", rule)
		}
		key := apiGroups[0] + "/" + resources[0]
		want, ok := expected[key]
		if !ok {
			t.Fatalf("agent Role contains unexpected permission %s", key)
		}
		sort.Strings(verbs)
		sort.Strings(want.verbs)
		sort.Strings(resourceNames)
		sort.Strings(want.resourceNames)
		if !equalStrings(verbs, want.verbs) || !equalStrings(resourceNames, want.resourceNames) {
			t.Fatalf("agent permission %s verbs/names = %v/%v, want %v/%v", key, verbs, resourceNames, want.verbs, want.resourceNames)
		}
		seen[key] = true
	}
	for key := range expected {
		if !seen[key] {
			t.Fatalf("agent Role is missing permission %s", key)
		}
	}
	subjects, found, err := unstructured.NestedSlice(binding.Object, "subjects")
	if err != nil || !found || len(subjects) != 1 {
		t.Fatalf("agent RoleBinding subjects: found=%v count=%d err=%v", found, len(subjects), err)
	}
	subject := subjects[0].(map[string]any)
	if subject["kind"] != "ServiceAccount" || subject["name"] != "idleloom-agent" || subject["namespace"] != "idleloom-host-example" {
		t.Fatalf("unexpected agent RoleBinding subject: %#v", subject)
	}
}

func TestControllerLeasePermissionIsFencingOnly(t *testing.T) {
	objects := decodeObjects(t, filepath.Join("..", "..", "deploy", "native", "rbac", "controller.yaml"))
	found := false
	for _, object := range objects {
		if object.GetKind() != "ClusterRole" {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(object.Object, "rules")
		for _, rawRule := range rules {
			rule := rawRule.(map[string]any)
			resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
			for _, resource := range resources {
				if resource == "*" {
					t.Fatalf("controller ClusterRole grants wildcard access: %#v", rule)
				}
				if resource != "leases" {
					continue
				}
				resourceNames, _, _ := unstructured.NestedStringSlice(rule, "resourceNames")
				verbs, _, _ := unstructured.NestedStringSlice(rule, "verbs")
				sort.Strings(resourceNames)
				sort.Strings(verbs)
				if !equalStrings(resourceNames, []string{"idleloom-fencing"}) || !equalStrings(verbs, []string{"get", "update"}) {
					t.Fatalf("controller Lease access is broader than fencing CAS: %#v", rule)
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatal("controller ClusterRole is missing idleloom-fencing Lease CAS permission")
	}
}

func TestControllerServingPermissionsAreResourceScoped(t *testing.T) {
	objects := decodeObjects(t, filepath.Join("..", "..", "deploy", "native", "rbac", "controller.yaml"))
	want := map[string][]string{
		"/secrets":                        {"create", "delete", "get"},
		"/services":                       {"get"},
		"discovery.k8s.io/endpointslices": {"create", "delete", "get", "update"},
	}
	seen := make(map[string]bool, len(want))
	for _, object := range objects {
		if object.GetKind() != "ClusterRole" {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(object.Object, "rules")
		for _, rawRule := range rules {
			rule := rawRule.(map[string]any)
			apiGroups, _, _ := unstructured.NestedStringSlice(rule, "apiGroups")
			resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
			if len(apiGroups) != 1 || len(resources) != 1 {
				continue
			}
			key := apiGroups[0] + "/" + resources[0]
			verbs, ok := want[key]
			if !ok {
				continue
			}
			got, _, _ := unstructured.NestedStringSlice(rule, "verbs")
			sort.Strings(got)
			sort.Strings(verbs)
			if !equalStrings(got, verbs) {
				t.Fatalf("controller serving permission %s = %v, want %v", key, got, verbs)
			}
			seen[key] = true
		}
	}
	for key := range want {
		if !seen[key] {
			t.Fatalf("controller ClusterRole is missing serving permission %s", key)
		}
	}
}

func TestControllerRuntimeRoleUsesPrecreatedLeaderLease(t *testing.T) {
	objects := decodeObjects(t, filepath.Join("..", "..", "deploy", "native", "rbac", "controller.yaml"))
	var leaseFound bool
	var roleFound bool
	for _, object := range objects {
		if object.GetKind() == "Lease" && object.GetNamespace() == "idleloom-system" && object.GetName() == "idleloom-controller-leader" {
			leaseFound = true
		}
		if object.GetKind() != "Role" || object.GetName() != "idleloom-controller-runtime" {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(object.Object, "rules")
		for _, rawRule := range rules {
			rule := rawRule.(map[string]any)
			resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
			if !equalStrings(resources, []string{"leases"}) {
				continue
			}
			verbs, _, _ := unstructured.NestedStringSlice(rule, "verbs")
			resourceNames, _, _ := unstructured.NestedStringSlice(rule, "resourceNames")
			sort.Strings(verbs)
			if !equalStrings(verbs, []string{"get", "update"}) || !equalStrings(resourceNames, []string{"idleloom-controller-leader"}) {
				t.Fatalf("controller leader Lease permission is broader than required: %#v", rule)
			}
			roleFound = true
		}
	}
	if !leaseFound || !roleFound {
		t.Fatalf("precreated leader Lease/runtime permission found = %v/%v", leaseFound, roleFound)
	}
}

func TestWorkloadOperatorCannotRemoveControllerFinalizers(t *testing.T) {
	objects := decodeObjects(t, filepath.Join("..", "..", "deploy", "native", "rbac", "operator.yaml"))
	for _, object := range objects {
		if object.GetKind() != "ClusterRole" || object.GetName() != "idleloom-workload-operator" {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(object.Object, "rules")
		for _, rawRule := range rules {
			rule := rawRule.(map[string]any)
			resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
			if !equalStrings(resources, []string{"idleloomworkloads"}) {
				continue
			}
			verbs, _, _ := unstructured.NestedStringSlice(rule, "verbs")
			for _, forbidden := range []string{"patch", "update"} {
				if containsString(verbs, forbidden) {
					t.Fatalf("workload operator can %s workload finalizers", forbidden)
				}
			}
			return
		}
	}
	t.Fatal("workload operator rule was not found")
}

func TestProjectionPermissionsAreSeparatedFromHostController(t *testing.T) {
	controllerObjects := decodeObjects(t, filepath.Join("..", "..", "deploy", "native", "rbac", "controller.yaml"))
	for _, object := range controllerObjects {
		if object.GetKind() != "ClusterRole" || object.GetName() != "idleloom-controller" {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(object.Object, "rules")
		for _, rawRule := range rules {
			resources, _, _ := unstructured.NestedStringSlice(rawRule.(map[string]any), "resources")
			for _, resource := range resources {
				if resource == "nodes" || resource == "nodes/status" || resource == "pods" || resource == "pods/status" {
					t.Fatalf("host-side controller can modify projected %s", resource)
				}
			}
		}
	}

	objects := decodeObjects(t, filepath.Join("..", "..", "deploy", "native", "projection", "rbac.yaml"))
	expected := map[string][]string{
		"ai.idleloom.io/idleloomworkloadassignments": {"get", "list", "watch"},
		"ai.idleloom.io/idleloomworkloads":           {"get"},
		"ai.idleloom.io/idleloomhosts":               {"get"},
		"/nodes":                                     {"create", "delete", "get", "list", "update"},
		"/nodes/status":                              {"get", "patch", "update"},
		"/pods":                                      {"create", "delete", "get", "list", "update"},
		"/pods/log":                                  {"get"},
		"/pods/status":                               {"get", "patch", "update"},
	}
	seen := make(map[string]bool, len(expected))
	for _, object := range objects {
		if object.GetNamespace() == "kube-node-lease" {
			t.Fatalf("projection RBAC can affect real Node leases: %s %s", object.GetKind(), object.GetName())
		}
		if object.GetKind() != "ClusterRole" || object.GetName() != "idleloom-projection" {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(object.Object, "rules")
		for _, rawRule := range rules {
			rule := rawRule.(map[string]any)
			groups, _, _ := unstructured.NestedStringSlice(rule, "apiGroups")
			resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
			verbs, _, _ := unstructured.NestedStringSlice(rule, "verbs")
			if len(groups) != 1 || len(resources) != 1 {
				t.Fatalf("projection rule is not narrowly scoped: %#v", rule)
			}
			key := groups[0] + "/" + resources[0]
			want, ok := expected[key]
			if !ok {
				t.Fatalf("projection ClusterRole contains unexpected permission %s", key)
			}
			sort.Strings(verbs)
			sort.Strings(want)
			if !equalStrings(verbs, want) {
				t.Fatalf("projection permission %s verbs = %v, want %v", key, verbs, want)
			}
			seen[key] = true
		}
	}
	for key := range expected {
		if !seen[key] {
			t.Fatalf("projection ClusterRole is missing %s", key)
		}
	}
	refreshPermission := false
	for _, object := range objects {
		if object.GetKind() != "Role" || object.GetName() != "idleloom-projection-leader" {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(object.Object, "rules")
		for _, rawRule := range rules {
			rule := rawRule.(map[string]any)
			resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
			resourceNames, _, _ := unstructured.NestedStringSlice(rule, "resourceNames")
			verbs, _, _ := unstructured.NestedStringSlice(rule, "verbs")
			if equalStrings(resources, []string{"serviceaccounts/token"}) && equalStrings(resourceNames, []string{"idleloom-projection"}) && equalStrings(verbs, []string{"create"}) {
				refreshPermission = true
			}
		}
	}
	if !refreshPermission {
		t.Fatal("projection Role cannot refresh its short-lived credential")
	}
}

func TestProjectionAdmissionRestrictsControllerToManagedPodsAndNodes(t *testing.T) {
	objects := decodeObjects(t, filepath.Join("..", "..", "deploy", "native", "projection", "admission.yaml"))
	var policy *unstructured.Unstructured
	var podBindingPolicy *unstructured.Unstructured
	var bindingSubresourcePolicy *unstructured.Unstructured
	for _, object := range objects {
		if object.GetKind() == "ValidatingAdmissionPolicy" && object.GetName() == "idleloom-projection" {
			policy = object
		}
		if object.GetKind() == "ValidatingAdmissionPolicy" && object.GetName() == "idleloom-projection-pod-binding" {
			podBindingPolicy = object
		}
		if object.GetKind() == "ValidatingAdmissionPolicy" && object.GetName() == "idleloom-projection-binding-subresource" {
			bindingSubresourcePolicy = object
		}
	}
	if policy == nil || podBindingPolicy == nil || bindingSubresourcePolicy == nil {
		t.Fatal("a projection ownership, Pod, or binding subresource ValidatingAdmissionPolicy was not found")
	}
	rules, found, err := unstructured.NestedSlice(policy.Object, "spec", "matchConstraints", "resourceRules")
	if err != nil || !found || len(rules) != 1 {
		t.Fatalf("projection admission resource rules: found=%v count=%d err=%v", found, len(rules), err)
	}
	rule := rules[0].(map[string]any)
	operations, _, _ := unstructured.NestedStringSlice(rule, "operations")
	resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
	sort.Strings(operations)
	sort.Strings(resources)
	if !equalStrings(operations, []string{"CREATE", "DELETE", "UPDATE"}) || !equalStrings(resources, []string{"nodes", "nodes/status", "pods", "pods/status"}) {
		t.Fatalf("projection admission scope = operations %v resources %v", operations, resources)
	}
	validations, found, err := unstructured.NestedSlice(policy.Object, "spec", "validations")
	if err != nil || !found {
		t.Fatalf("projection admission validations: found=%v err=%v", found, err)
	}
	var expressions string
	for _, raw := range validations {
		expression, _, _ := unstructured.NestedString(raw.(map[string]any), "expression")
		expressions += expression
	}
	for _, required := range []string{"previousManaged", "previousReserved", "request.operation == 'DELETE'", "privilegedOperator"} {
		if !strings.Contains(expressions, required) {
			t.Fatalf("projection admission does not enforce %q: %s", required, expressions)
		}
	}
	variables, found, err := unstructured.NestedSlice(policy.Object, "spec", "variables")
	if err != nil || !found {
		t.Fatalf("projection admission variables: found=%v err=%v", found, err)
	}
	var variableExpressions string
	for _, raw := range variables {
		expression, _, _ := unstructured.NestedString(raw.(map[string]any), "expression")
		variableExpressions += expression
	}
	const projectionNamePattern = "^idleloom-[0-9a-f]{20}$"
	if !strings.Contains(variableExpressions, projectionNamePattern) || strings.Contains(variableExpressions, "startsWith('idleloom-')") {
		t.Fatalf("projection ownership policy does not isolate the exact ephemeral name space: %s", variableExpressions)
	}
	podValidations, found, err := unstructured.NestedSlice(podBindingPolicy.Object, "spec", "validations")
	if err != nil || !found || len(podValidations) != 1 {
		t.Fatalf("projection Pod binding validations: found=%v count=%d err=%v", found, len(podValidations), err)
	}
	podExpression, _, _ := unstructured.NestedString(podValidations[0].(map[string]any), "expression")
	if !strings.Contains(podExpression, "object.spec.nodeName") || !strings.Contains(podExpression, "currentManaged") || !strings.Contains(podExpression, projectionNamePattern) {
		t.Fatalf("projection Pod binding policy is incomplete: %s", podExpression)
	}
	bindingRules, found, err := unstructured.NestedSlice(bindingSubresourcePolicy.Object, "spec", "matchConstraints", "resourceRules")
	if err != nil || !found || len(bindingRules) != 1 {
		t.Fatalf("projection binding subresource rules: found=%v count=%d err=%v", found, len(bindingRules), err)
	}
	bindingResources, _, _ := unstructured.NestedStringSlice(bindingRules[0].(map[string]any), "resources")
	bindingValidations, _, _ := unstructured.NestedSlice(bindingSubresourcePolicy.Object, "spec", "validations")
	bindingExpression, _, _ := unstructured.NestedString(bindingValidations[0].(map[string]any), "expression")
	if !equalStrings(bindingResources, []string{"pods/binding"}) || !strings.Contains(bindingExpression, "object.target.name") || !strings.Contains(bindingExpression, projectionNamePattern) {
		t.Fatalf("projection binding subresource policy is incomplete: resources=%v expression=%s", bindingResources, bindingExpression)
	}
}

func decodeObjects(t *testing.T, path string) []*unstructured.Unstructured {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()
	decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
	var objects []*unstructured.Unstructured
	for {
		var object map[string]any
		if err := decoder.Decode(&object); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode %s: %v", path, err)
		}
		if len(object) > 0 {
			objects = append(objects, &unstructured.Unstructured{Object: object})
		}
	}
	return objects
}

func nestedMap(t *testing.T, object map[string]any, fields ...string) map[string]any {
	t.Helper()
	value, found, err := unstructured.NestedMap(object, fields...)
	if err != nil || !found {
		t.Fatalf("read %v: found=%v err=%v", fields, found, err)
	}
	return value
}

func crdSchema(t *testing.T, object *unstructured.Unstructured) map[string]any {
	t.Helper()
	versions, found, err := unstructured.NestedSlice(object.Object, "spec", "versions")
	if err != nil || !found || len(versions) != 1 {
		t.Fatalf("read CRD versions: found=%v count=%d err=%v", found, len(versions), err)
	}
	version, ok := versions[0].(map[string]any)
	if !ok {
		t.Fatalf("CRD version has type %T", versions[0])
	}
	return nestedMap(t, version, "schema", "openAPIV3Schema")
}

func assertKeys(t *testing.T, object map[string]any, want []string) {
	t.Helper()
	var got []string
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
}

func assertValidationRule(t *testing.T, schema map[string]any, expected string) {
	t.Helper()
	validations, ok := schema["x-kubernetes-validations"].([]any)
	if !ok {
		t.Fatalf("schema has no x-kubernetes-validations")
	}
	for _, rawValidation := range validations {
		validation := rawValidation.(map[string]any)
		if validation["rule"] == expected {
			return
		}
	}
	t.Fatalf("schema is missing validation rule %q", expected)
}

func assertValidationRuleContains(t *testing.T, schema map[string]any, expected string) {
	t.Helper()
	validations, ok := schema["x-kubernetes-validations"].([]any)
	if !ok {
		t.Fatalf("schema has no x-kubernetes-validations")
	}
	for _, rawValidation := range validations {
		validation := rawValidation.(map[string]any)
		if strings.Contains(validation["rule"].(string), expected) {
			return
		}
	}
	t.Fatalf("schema is missing validation rule containing %q", expected)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
