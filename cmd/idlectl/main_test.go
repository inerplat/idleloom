package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/enroll"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	nativewirekube "github.com/inerplat/idleloom/internal/native/wirekube"
	"github.com/inerplat/idleloom/internal/native/wirekubecli"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestSecureClusterConfigRejectsImplicitInsecureTLS(t *testing.T) {
	config := &rest.Config{Host: "https://cluster.example", TLSClientConfig: rest.TLSClientConfig{Insecure: true}}
	if _, err := secureClusterConfig(context.Background(), config, t.TempDir(), false, false); err == nil {
		t.Fatal("insecure Kubernetes config was accepted without explicit TOFU")
	}
}

func TestShellAccessAndIsolationParsing(t *testing.T) {
	access, err := parseShellAccess("HOST")
	if err != nil || access != nativev1alpha1.ShellAccessHost {
		t.Fatalf("parseShellAccess = %q, %v", access, err)
	}
	isolation, err := parseShellIsolation("sandboxed")
	if err != nil || isolation != nativev1alpha1.ShellIsolationSandbox {
		t.Fatalf("parseShellIsolation = %q, %v", isolation, err)
	}
	if _, err := parseShellAccess("root"); err == nil {
		t.Fatal("parseShellAccess accepted an unsupported privilege")
	}
}

func TestKubernetesStyleResourceReferences(t *testing.T) {
	tests := []struct {
		args         []string
		allowBare    bool
		wantResource string
		wantName     string
	}{
		{args: []string{"workloads"}, wantResource: resourceWorkloads},
		{args: []string{"workload", "job"}, wantResource: resourceWorkloads, wantName: "job"},
		{args: []string{"workload/job"}, wantResource: resourceWorkloads, wantName: "job"},
		{args: []string{"hosts/studio"}, wantResource: resourceHosts, wantName: "studio"},
		{args: []string{"job"}, allowBare: true, wantResource: resourceWorkloads, wantName: "job"},
	}
	for _, test := range tests {
		resourceName, name, err := parseResourceReference(test.args, test.allowBare)
		if err != nil {
			t.Fatalf("parseResourceReference(%v): %v", test.args, err)
		}
		if resourceName != test.wantResource || name != test.wantName {
			t.Fatalf("parseResourceReference(%v) = %q, %q", test.args, resourceName, name)
		}
	}
	if _, _, err := parseResourceReference([]string{"pod/job"}, false); err == nil {
		t.Fatal("unsupported Kubernetes resource was accepted")
	}
}

func TestPublicUsageIsResourceOriented(t *testing.T) {
	if !strings.HasPrefix(usageText, "idlectl ") {
		t.Fatalf("usage does not expose the idlectl binary name: %s", usageText)
	}
	for _, expected := range []string{"join HOST", "run NAME", "recipe (list | show NAME | render NAME", "get (hosts|workloads) [NAME]", "logs (WORKLOAD | workload/WORKLOAD)", "delete ((host|workload) NAME | (host|workload)/NAME)", "version"} {
		if !strings.Contains(usageText, expected) {
			t.Fatalf("usage does not contain %q", expected)
		}
	}
	for _, legacy := range []string{" admin ", " serve ", " debug ", "enroll", "connectivity-run"} {
		if strings.Contains(usageText, legacy) {
			t.Fatalf("usage still exposes legacy command %q", legacy)
		}
	}
}

func TestRecipeCommandsExposeBothBackendsAndRenderYAML(t *testing.T) {
	var output bytes.Buffer
	if err := runRecipe([]string{"list"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"train/mlx-linear-regression@v1", "native", "train/container-linear-regression@v1", "worker"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("recipe list does not contain %q: %s", expected, output.String())
		}
	}

	output.Reset()
	if err := runRecipe([]string{"show", "train/mlx-linear-regression@v1"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"id: train/mlx-linear-regression@v1", "digest: sha256:", "backend: native", "parameters:", "example:"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("recipe show does not contain %q: %s", expected, output.String())
		}
	}

	output.Reset()
	values := "namespace: training\nepochs: 140\n"
	if err := runRecipe([]string{"render", "train/container-linear-regression@v1", "--name", "worker-train", "--values", "-", "-o", "yaml"}, strings.NewReader(values), &output); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"apiVersion: batch/v1", "kind: Job", `name: "worker-train"`, `namespace: "training"`, `value: "140"`} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("recipe render does not contain %q: %s", expected, output.String())
		}
	}
}

func TestRecipeCommandsRejectUnpinnedAndUnsupportedOutput(t *testing.T) {
	var output bytes.Buffer
	if err := runRecipe([]string{"show", "train/mlx-linear-regression"}, strings.NewReader(""), &output); err == nil || !strings.Contains(err.Error(), "version-pinned") {
		t.Fatalf("unpinned show error = %v", err)
	}
	if err := runRecipe([]string{"render", "train/mlx-linear-regression@v1", "--name", "train", "-o", "json"}, strings.NewReader(""), &output); err == nil || !strings.Contains(err.Error(), "--output must be yaml") {
		t.Fatalf("unsupported output error = %v", err)
	}
}

func TestVersionTextIncludesBuildIdentity(t *testing.T) {
	text := versionText()
	for _, expected := range []string{"idlectl ", version, commit, buildDate, goruntime.GOOS + "/" + goruntime.GOARCH} {
		if !strings.Contains(text, expected) {
			t.Fatalf("version text %q does not contain %q", text, expected)
		}
	}
}

func TestPublicSubcommandHelpIncludesCompleteUsage(t *testing.T) {
	for _, command := range []string{"join", "run", "get", "logs", "delete"} {
		flags, _, _ := clusterPFlags(command)
		var output bytes.Buffer
		flags.SetOutput(&output)
		if err := flags.Parse([]string{"--help"}); !errors.Is(err, pflag.ErrHelp) {
			t.Fatalf("%s --help error = %v", command, err)
		}
		if !strings.Contains(output.String(), "idlectl "+command) {
			t.Fatalf("%s --help lacks full command usage: %s", command, output.String())
		}
	}
}

func TestHostCommandsRejectNamespacedFlagsBeforeClusterAccess(t *testing.T) {
	if err := runGet(context.Background(), []string{"-n", "default", "hosts"}); err == nil || !strings.Contains(err.Error(), "cluster-wide") {
		t.Fatalf("get hosts namespace error = %v", err)
	}
	if err := runDelete(context.Background(), []string{"--wait=false", "host/studio"}); err == nil || !strings.Contains(err.Error(), "always waits") {
		t.Fatalf("delete host wait error = %v", err)
	}
}

func TestLegacyCommandsAreNotPubliclyDispatched(t *testing.T) {
	for _, command := range []string{"admin", "serve", "debug", "enroll", "prepare", "install", "controller", "agent", "projection", "connectivity", "connectivity-run"} {
		handled, err := runPublicCommand(context.Background(), command, nil)
		if handled || err != nil {
			t.Fatalf("legacy command %q handled=%t err=%v", command, handled, err)
		}
	}
}

func TestQualifiedKubernetesResourceNames(t *testing.T) {
	for input, want := range map[string]string{
		"idleloomworkloads.ai.idleloom.io": resourceWorkloads,
		"idleloomhosts.ai.idleloom.io":     resourceHosts,
	} {
		got, err := canonicalResource(input)
		if err != nil || got != want {
			t.Fatalf("canonicalResource(%q) = %q, %v", input, got, err)
		}
	}
}

func TestLogsRejectsNonWorkloadResourcesBeforeClusterAccess(t *testing.T) {
	err := runLogs(context.Background(), []string{"host/studio"})
	if err == nil || !strings.Contains(err.Error(), "workloads only") {
		t.Fatalf("runLogs host resource error = %v", err)
	}
}

func TestLocalLogsRejectsFollowBeforeClusterAccess(t *testing.T) {
	err := runLogs(context.Background(), []string{"--local", "--follow", "workload/job"})
	if err == nil || !strings.Contains(err.Error(), "completed snapshots") {
		t.Fatalf("runLogs local follow error = %v", err)
	}
}

func TestJoinRejectsEmptyNormalizedHostBeforeClusterAccess(t *testing.T) {
	err := runJoin(context.Background(), []string{"___"})
	if err == nil || !strings.Contains(err.Error(), "letter or digit") {
		t.Fatalf("runJoin invalid host error = %v", err)
	}
}

func TestJoinRejectsExistingLocalInstallationBeforeClusterAccess(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "services.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runJoin(context.Background(), []string{"--state-dir", directory, "studio"})
	if err == nil || !strings.Contains(err.Error(), "already joined") {
		t.Fatalf("runJoin existing installation error = %v", err)
	}
}

func TestMissingWireKubeRequiresExplicitNonInteractiveDependencyInstall(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nativewirekube.MeshesGVR:        "WireKubeMeshList",
		nativewirekube.ExternalPeersGVR: "WireKubeExternalPeerList",
	})
	called := false
	original := newWireKubeLifecycle
	newWireKubeLifecycle = func(context.Context, string, string) (wireKubeLifecycle, error) {
		called = true
		return nil, errors.New("unexpected resolver call")
	}
	t.Cleanup(func() { newWireKubeLifecycle = original })

	err := ensureWireKubeForJoin(context.Background(), wireKubeJoinConfig{
		Dynamic: client, HostID: "studio", Interactive: false, Input: &bytes.Buffer{}, Output: &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "--install-dependencies --yes") {
		t.Fatalf("error=%v", err)
	}
	if called {
		t.Fatal("dependency resolver ran before non-interactive authorization")
	}
}

func TestExistingCompatibleWireKubeSkipsDependencyResolver(t *testing.T) {
	mesh := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "wirekube.io/v1alpha1",
		"kind":       "WireKubeMesh",
		"metadata":   map[string]any{"name": "default"},
		"spec": map[string]any{
			"meshCIDR": "172.31.240.0/20",
			"relay":    map[string]any{"mode": "auto", "provider": "managed"},
		},
		"status": map[string]any{"readyPeers": int64(1)},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nativewirekube.MeshesGVR:        "WireKubeMeshList",
		nativewirekube.ExternalPeersGVR: "WireKubeExternalPeerList",
	})
	if err := client.Tracker().Create(nativewirekube.MeshesGVR, mesh, ""); err != nil {
		t.Fatal(err)
	}
	called := false
	original := newWireKubeLifecycle
	newWireKubeLifecycle = func(context.Context, string, string) (wireKubeLifecycle, error) {
		called = true
		return nil, errors.New("unexpected resolver call")
	}
	t.Cleanup(func() { newWireKubeLifecycle = original })
	var output bytes.Buffer

	err := ensureWireKubeForJoin(context.Background(), wireKubeJoinConfig{
		Dynamic: client, HostID: "studio", Interactive: false, Input: &bytes.Buffer{}, Output: &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("dependency resolver ran for an existing compatible WireKube installation")
	}
	if !strings.Contains(output.String(), "using existing WireKube mesh default") {
		t.Fatalf("output=%q", output.String())
	}
}

func TestMissingWireKubePlansInstallsAndContinuesJoin(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nativewirekube.MeshesGVR:        "WireKubeMeshList",
		nativewirekube.ExternalPeersGVR: "WireKubeExternalPeerList",
	})
	lifecycle := &fakeWireKubeLifecycle{plan: wirekubecli.Plan{
		Context: "cluster", WireKubeVersion: wirekubecli.CompatibleVersion,
		Image: "example.test/wirekube@sha256:" + strings.Repeat("a", 64),
		Relay: "load-balancer", RelayUDP: true, MeshCIDR: "100.96.0.0/11", NodeAddresses: "internal-ip",
		Impact: []string{"one public TCP LoadBalancer", "one separate public UDP LoadBalancer"},
	}}
	lifecycle.plan.Detection.KubernetesVersion = "v1.35.3"
	lifecycle.plan.Detection.CNI = "cilium"
	original := newWireKubeLifecycle
	newWireKubeLifecycle = func(context.Context, string, string) (wireKubeLifecycle, error) { return lifecycle, nil }
	t.Cleanup(func() { newWireKubeLifecycle = original })
	var output bytes.Buffer

	err := ensureWireKubeForJoin(context.Background(), wireKubeJoinConfig{
		Dynamic: client, HostID: "studio", ShellAccess: nativev1alpha1.ShellAccessSandboxed, Projection: true,
		Yes: true, InstallDependencies: true, Interactive: false, Input: &bytes.Buffer{}, Output: &output,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.planCalls != 1 || lifecycle.installCalls != 1 {
		t.Fatalf("plan calls=%d install calls=%d", lifecycle.planCalls, lifecycle.installCalls)
	}
	for _, text := range []string{"Idleloom connected-host plan", "one separate public UDP LoadBalancer", "continuing host enrollment"} {
		if !strings.Contains(output.String(), text) {
			t.Fatalf("output does not contain %q: %s", text, output.String())
		}
	}
}

func TestDependencyConfirmationDefaultsToYes(t *testing.T) {
	var output bytes.Buffer
	confirmed, err := confirmDefaultYes(strings.NewReader("\n"), &output, "Continue? [Y/n] ")
	if err != nil || !confirmed {
		t.Fatalf("confirmed=%t err=%v", confirmed, err)
	}
	confirmed, err = confirmDefaultYes(strings.NewReader("no\n"), &output, "Continue? [Y/n] ")
	if err != nil || confirmed {
		t.Fatalf("confirmed=%t err=%v", confirmed, err)
	}
}

type fakeWireKubeLifecycle struct {
	plan         wirekubecli.Plan
	planCalls    int
	installCalls int
}

func (f *fakeWireKubeLifecycle) Plan(context.Context) (wirekubecli.Plan, error) {
	f.planCalls++
	return f.plan, nil
}

func (f *fakeWireKubeLifecycle) Install(_ context.Context, plan wirekubecli.Plan) (wirekubecli.Result, error) {
	f.installCalls++
	if plan.MeshCIDR != f.plan.MeshCIDR {
		return wirekubecli.Result{}, fmt.Errorf("unexpected mesh CIDR %s", plan.MeshCIDR)
	}
	return wirekubecli.Result{InstallationID: "install-1", Ready: true}, nil
}

func TestWorkloadHostReferenceBlocksHostDeletion(t *testing.T) {
	workload := &nativev1alpha1.IdleloomWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "default"},
		Status: nativev1alpha1.IdleloomWorkloadStatus{SchedulingIntent: &nativev1alpha1.WorkloadSchedulingIntent{
			HostRef: nativev1alpha1.NamespacedObjectReference{Namespace: "idleloom-host-studio"},
		}},
	}
	if !workloadUsesHost(workload, "idleloom-host-studio") {
		t.Fatal("active workload did not retain its host reference")
	}
	workload.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	if workloadUsesHost(workload, "idleloom-host-studio") {
		t.Fatal("deleting workload still blocked host deletion")
	}
}

func TestHostAssignmentBlocksHostDeletion(t *testing.T) {
	hostNamespace := "idleloom-host-studio"
	assignment := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": nativev1alpha1.GroupVersion.String(),
		"kind":       "IdleloomWorkloadAssignment",
		"metadata": map[string]any{
			"name":      nativev1alpha1.AssignmentMailboxName,
			"namespace": hostNamespace,
		},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		nativekube.WorkloadsGVR:   "IdleloomWorkloadList",
		nativekube.AssignmentsGVR: "IdleloomWorkloadAssignmentList",
	}, assignment)
	err := ensureHostUnused(context.Background(), client, hostNamespace)
	if err == nil || !strings.Contains(err.Error(), "assignment") {
		t.Fatalf("ensureHostUnused assignment error = %v", err)
	}
}

func TestHostNamespaceMustMatchLocalEnrollment(t *testing.T) {
	identity := enroll.EnrollmentIdentity{HostID: "studio", Nonce: strings.Repeat("a", 64)}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "idleloom",
			"ai.idleloom.io/host-id":       "studio",
		},
		Annotations: map[string]string{"ai.idleloom.io/enrollment-id": identity.Nonce},
	}}
	if !namespaceOwnedByEnrollment(namespace, identity) {
		t.Fatal("matching namespace was not recognized")
	}
	namespace.Annotations["ai.idleloom.io/enrollment-id"] = strings.Repeat("b", 64)
	if namespaceOwnedByEnrollment(namespace, identity) {
		t.Fatal("namespace from another enrollment was accepted")
	}
}

func TestPrivilegedHelperNameDispatchesWithoutPublicSubcommand(t *testing.T) {
	handled, err := runInternalBinary(context.Background(), "io.idleloom.link.studio", []string{"--help"})
	if !handled || !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
}

func TestCompanionBinaryNamesDispatchInternally(t *testing.T) {
	for _, binary := range []string{"idleloom-controller", "idleloom-agent", "idleloom-link", "idleloom-projection"} {
		handled, err := runInternalBinary(context.Background(), binary, []string{"--help"})
		if !handled || !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("%s handled=%t err=%v", binary, handled, err)
		}
	}
}

func TestResolveNamespaceUsesSelectedKubeconfigContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	config := clientcmdapi.Config{
		CurrentContext: "test",
		Clusters: map[string]*clientcmdapi.Cluster{
			"cluster": {Server: "https://cluster.example"},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"user": {}},
		Contexts: map[string]*clientcmdapi.Context{
			"test": {Cluster: "cluster", AuthInfo: "user", Namespace: "tenant"},
		},
	}
	if err := clientcmd.WriteToFile(config, path); err != nil {
		t.Fatal(err)
	}
	namespace, err := resolveNamespace(path, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if namespace != "tenant" {
		t.Fatalf("namespace = %q, want tenant", namespace)
	}
	if namespace, err = resolveNamespace(path, "", "explicit"); err != nil || namespace != "explicit" {
		t.Fatalf("explicit namespace = %q, %v", namespace, err)
	}
}

func TestProjectionRequiresExplicitFeatureGate(t *testing.T) {
	if err := runProjection(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "--enable-kubernetes-projection") {
		t.Fatalf("projection feature gate error = %v", err)
	}
}

func TestProjectionSeparatesInClusterAndExternalCredentials(t *testing.T) {
	err := runProjection(context.Background(), []string{"--enable-kubernetes-projection", "--in-cluster", "--kubeconfig", "test"})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("projection credential separation error = %v", err)
	}
}

func TestSecureClusterConfigPreservesVerifiedTLS(t *testing.T) {
	config := &rest.Config{Host: "https://cluster.example", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("ca")}}
	secured, err := secureClusterConfig(context.Background(), config, t.TempDir(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	if secured != config {
		t.Fatal("verified Kubernetes config was unnecessarily copied")
	}
}
