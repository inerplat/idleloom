package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/devruntime"
	"github.com/inerplat/idleloom/internal/native/execution"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	"github.com/inerplat/idleloom/internal/native/kubeletbridge"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

const testHFRevision = "0123456789abcdef0123456789abcdef01234567"

func hfAssignment(model devruntime.HFModel) *nativev1alpha1.IdleloomWorkloadAssignment {
	return &nativev1alpha1.IdleloomWorkloadAssignment{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("hf-assignment"), Generation: 1},
		Spec: nativev1alpha1.IdleloomWorkloadAssignmentSpec{
			DesiredState: nativev1alpha1.AssignmentDesiredRunning,
			WorkloadRef:  nativev1alpha1.WorkloadObjectReference{Namespace: "default", Name: "lfm", UID: types.UID("workload-uid"), Generation: 1},
			Model: &nativev1alpha1.ResolvedModel{
				CatalogRef: nativev1alpha1.ObjectReference{Name: "lfm2-8b-mlx", UID: types.UID("model-uid")},
				Family:     nativev1alpha1.ModelFamilyMLX, RuntimeProfile: nativev1alpha1.RuntimeProfileMLXLMV1,
				Artifact: nativev1alpha1.ModelArtifact{
					HuggingFace:    &nativev1alpha1.HuggingFaceSource{Repository: model.Repository, Revision: model.Revision},
					ManifestDigest: model.ManifestDigest, Format: nativev1alpha1.ArtifactFormatSafetensorsV1, SizeBytes: model.SizeBytes,
				},
				UnifiedMemoryRequest: resource.MustParse("8Gi"), MaxContextLength: 2048, MaxConcurrentRequests: 1,
			},
			ExecutionID: "123e4567-e89b-42d3-a456-426614174000", FencingEpoch: 1, LeaseDurationSeconds: 30,
		},
	}
}

func testHFModel() devruntime.HFModel {
	return devruntime.HFModel{
		Repository: "owner/model", Revision: testHFRevision,
		ManifestDigest: "sha256:" + strings.Repeat("b", 64), SizeBytes: 4096,
	}
}

func TestEnsureProcessPreparesHFModelOnFirstUseAndRunsItsDirectory(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	model := testHFModel()
	prepareCalls := 0
	var started *devruntime.HFModel
	agent := &DevAgent{
		store: store, logs: kubeletbridge.NewLogBuffer(1 << 20),
		config: DevAgentConfig{
			AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()),
			PrepareRuntime: func(context.Context, func(string)) (devruntime.Receipt, error) {
				t.Fatal("a Hugging Face model must not prepare the locked model")
				return devruntime.Receipt{}, nil
			},
			PrepareHFModel: func(_ context.Context, requested devruntime.HFModel, _ func(string)) (devruntime.HFModelReceipt, error) {
				prepareCalls++
				return devruntime.HFModelReceipt{
					ManifestDigest: requested.ManifestDigest, SizeBytes: requested.SizeBytes,
					Sources: []devruntime.HFSource{{Repository: requested.Repository, Revision: requested.Revision}},
				}, nil
			},
			StartProcess: func(_ context.Context, config devruntime.ProcessConfig) (Process, error) {
				started = config.HFModel
				return &fakeProcess{alive: true}, nil
			},
		},
	}
	if err := agent.ensureProcess(context.Background(), hfAssignment(model)); err != nil {
		t.Fatal(err)
	}
	if prepareCalls != 1 || started == nil || *started != model {
		t.Fatalf("prepareCalls=%d started=%#v", prepareCalls, started)
	}
	if current := store.Current(); current == nil || current.RuntimeVersion != devruntime.RuntimeVersion {
		t.Fatalf("journal = %#v, want the locked runtime version", current)
	}
	if err := agent.stopProcess(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureProcessRefusesPreparedHFModelThatDiffers(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	model := testHFModel()
	agent := &DevAgent{
		store: store, logs: kubeletbridge.NewLogBuffer(1 << 20),
		config: DevAgentConfig{
			AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()),
			PrepareHFModel: func(_ context.Context, requested devruntime.HFModel, _ func(string)) (devruntime.HFModelReceipt, error) {
				return devruntime.HFModelReceipt{
					ManifestDigest: "sha256:" + strings.Repeat("c", 64), SizeBytes: requested.SizeBytes,
					Sources: []devruntime.HFSource{{Repository: requested.Repository, Revision: requested.Revision}},
				}, nil
			},
			StartProcess: func(context.Context, devruntime.ProcessConfig) (Process, error) {
				t.Fatal("a mismatched model must not start")
				return nil, nil
			},
		},
	}
	if err := agent.ensureProcess(context.Background(), hfAssignment(model)); err == nil {
		t.Fatal("ensureProcess accepted a prepared model that differs from the assignment")
	}
}

func TestEnsureProcessServesHFModelThroughOwnServer(t *testing.T) {
	store, err := execution.Open(filepath.Join(t.TempDir(), "execution.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	model := testHFModel()
	underlying := &fakeServingRuntime{fakeBatchRunner: fakeBatchRunner{alive: true, waitForCancellation: true, pid: 127}}
	var served *devruntime.HFModel
	agent := &DevAgent{
		store: store, logs: kubeletbridge.NewLogBuffer(1 << 20),
		config: DevAgentConfig{
			AgentID: "studio.native", Layout: devruntime.NewLayout(t.TempDir()), StateDirectory: t.TempDir(),
			Platform: fakeAgentPlatform{}, ServeListenAddress: "198.18.18.104:18080", ListenServing: listenAnywhere,
			PrepareHFModel: func(_ context.Context, requested devruntime.HFModel, _ func(string)) (devruntime.HFModelReceipt, error) {
				return devruntime.HFModelReceipt{
					ManifestDigest: requested.ManifestDigest, SizeBytes: requested.SizeBytes,
					Sources: []devruntime.HFSource{{Repository: requested.Repository, Revision: requested.Revision}},
				}, nil
			},
			StartMLXServe: func(_ context.Context, config devruntime.MLXServeConfig) (Process, error) {
				served = config.HFModel
				if err := config.OnSpawn(127); err != nil {
					return nil, err
				}
				return underlying, nil
			},
		},
	}
	assignment := hfAssignment(model)
	assignment.Spec.Model.Server = &nativev1alpha1.ResolvedServer{ServiceName: "lfm-chat", ModelAlias: "lfm2-8b", Port: nativev1alpha1.NativeServingPort}
	if err := agent.ensureProcess(context.Background(), assignment); err != nil {
		t.Fatal(err)
	}
	if served == nil || *served != model {
		t.Fatalf("served model = %#v, want %#v", served, model)
	}
	if err := agent.stopProcess(); err != nil {
		t.Fatal(err)
	}
}

func TestHostAdvertisesMLXFamilyAndPreparedHFModels(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	host := &nativev1alpha1.IdleloomHost{
		TypeMeta:   metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomHost"},
		ObjectMeta: metav1.ObjectMeta{Name: "host", Namespace: "idleloom-host-studio", Generation: 1},
		Spec:       nativev1alpha1.IdleloomHostSpec{AgentID: "studio.native"},
	}
	layout := devruntime.NewLayout(t.TempDir())
	prepared := writePreparedHFModel(t, layout)

	unprepared := advertisedHost(t, scheme, host, DevAgentConfig{AgentID: "studio.native", Layout: layout, Platform: fakeAgentPlatform{}})
	if slices.Contains(unprepared.Status.ModelFamilies, nativev1alpha1.ModelFamilyMLX) || len(unprepared.Status.AvailableModels) != 0 {
		t.Fatalf("host without an MLX runtime advertised MLX models: %#v", unprepared.Status)
	}

	updated := advertisedHost(t, scheme, host, DevAgentConfig{
		AgentID: "studio.native", Layout: layout, Platform: fakeAgentPlatform{},
		PrepareHFModel: func(context.Context, devruntime.HFModel, func(string)) (devruntime.HFModelReceipt, error) {
			return devruntime.HFModelReceipt{}, nil
		},
	})
	if !slices.Contains(updated.Status.RuntimeProfiles, nativev1alpha1.RuntimeProfileMLXLMV1) ||
		!slices.Contains(updated.Status.ModelFamilies, nativev1alpha1.ModelFamilyMLX) ||
		slices.Contains(updated.Status.ModelFamilies, nativev1alpha1.ModelFamilyQwen35) ||
		!slices.Contains(updated.Status.Capabilities, nativev1alpha1.CapabilityBatchInferenceV1) {
		t.Fatalf("preparable MLX capabilities = %#v", updated.Status)
	}
	want := nativev1alpha1.HostModelStatus{
		RuntimeProfile: nativev1alpha1.RuntimeProfileMLXLMV1, Name: prepared.Repository + "@" + prepared.Revision,
		ManifestDigest: prepared.ManifestDigest, Family: nativev1alpha1.ModelFamilyMLX,
		Format: nativev1alpha1.ArtifactFormatSafetensorsV1, SizeBytes: prepared.SizeBytes,
	}
	if len(updated.Status.AvailableModels) != 1 || updated.Status.AvailableModels[0] != want {
		t.Fatalf("available models = %#v, want %#v", updated.Status.AvailableModels, want)
	}
}

func advertisedHost(t *testing.T, scheme *runtime.Scheme, host *nativev1alpha1.IdleloomHost, config DevAgentConfig) nativev1alpha1.IdleloomHost {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClient(scheme, host.DeepCopy())
	config.Dynamic = client
	agent := &DevAgent{config: config}
	if err := agent.updateHostStatus(context.Background(), host.DeepCopy(), false, ""); err != nil {
		t.Fatal(err)
	}
	object, err := client.Resource(nativekube.HostsGVR).Namespace(host.Namespace).Get(context.Background(), host.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var updated nativev1alpha1.IdleloomHost
	if err := nativekube.FromUnstructured(object, &updated); err != nil {
		t.Fatal(err)
	}
	return updated
}

func writePreparedHFModel(t *testing.T, layout devruntime.Layout) devruntime.HFModel {
	t.Helper()
	contents := map[string][]byte{
		"config.json":       []byte(`{"model_type":"lfm2_moe"}`),
		"model.safetensors": []byte(strings.Repeat("w", 512)),
	}
	var files []devruntime.ModelFile
	var size int64
	for name, content := range contents {
		sum := sha256.Sum256(content)
		files = append(files, devruntime.ModelFile{Path: name, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])})
		size += int64(len(content))
	}
	digest, err := devruntime.MLXManifestDigest(files)
	if err != nil {
		t.Fatal(err)
	}
	directory := devruntime.HFModelDirectory(layout, digest)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range contents {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	model := devruntime.HFModel{Repository: "owner/model", Revision: testHFRevision, ManifestDigest: digest, SizeBytes: size}
	data, err := json.Marshal(devruntime.HFModelReceipt{
		Version: 1, ManifestDigest: digest, SizeBytes: size,
		Sources: []devruntime.HFSource{{Repository: model.Repository, Revision: model.Revision}},
		Files:   files,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory+".json", data, 0o600); err != nil {
		t.Fatal(err)
	}
	return model
}
