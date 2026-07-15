package install

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestDefaultInstallExcludesProjectionPrivileges(t *testing.T) {
	names, err := manifestNames()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.Contains(name, "projection") {
			t.Fatalf("default install includes optional projection manifest %s", name)
		}
	}
}

func TestApplyDoesNotForceConflictsByDefault(t *testing.T) {
	assertApplyForce(t, false)
}

func TestApplyForcesConflictsOnlyWhenRequested(t *testing.T) {
	assertApplyForce(t, true)
}

func TestApplyProjectionIncludesRBACAndAdmissionWithoutDeployment(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	var kinds []string
	client.PrependReactor("patch", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(clienttesting.PatchActionImpl)
		var object map[string]any
		if err := json.Unmarshal(patchAction.GetPatch(), &object); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, object["kind"].(string))
		return true, &unstructured.Unstructured{Object: object}, nil
	})
	if err := ApplyProjection(context.Background(), client, false); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(kinds, ",")
	if !strings.Contains(joined, "ClusterRole") || !strings.Contains(joined, "ValidatingAdmissionPolicy") || strings.Contains(joined, "Deployment") {
		t.Fatalf("projection install kinds = %v", kinds)
	}
}

func TestApplyCatalogPublishesLockedModel(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	models := map[string]map[string]any{}
	client.PrependReactor("patch", "idleloommodels", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(clienttesting.PatchActionImpl)
		var object map[string]any
		if err := json.Unmarshal(patchAction.GetPatch(), &object); err != nil {
			t.Fatal(err)
		}
		spec := object["spec"].(map[string]any)
		artifact := spec["artifact"].(map[string]any)
		name := object["metadata"].(map[string]any)["name"].(string)
		models[name] = artifact
		return true, &unstructured.Unstructured{Object: object}, nil
	})
	if err := ApplyCatalog(context.Background(), client, false); err != nil {
		t.Fatal(err)
	}
	if artifact := models["qwen3-5-0-8b-mlx"]; artifact == nil || !strings.Contains(artifact["ociReference"].(string), "@sha256:") {
		t.Fatalf("MLX catalog model = %#v", artifact)
	}
	if artifact := models["qwen3-5-9b-ollama"]; artifact == nil || artifact["ollamaModel"] != "qwen3.5:9b" || !strings.HasPrefix(artifact["manifestDigest"].(string), "sha256:") {
		t.Fatalf("Ollama catalog model = %#v", artifact)
	}
}

func assertApplyForce(t *testing.T, force bool) {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	patches := 0
	client.PrependReactor("patch", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(clienttesting.PatchActionImpl)
		options := patchAction.GetPatchOptions()
		if force {
			if options.Force == nil || !*options.Force {
				t.Fatal("server-side apply did not request conflict ownership")
			}
		} else if options.Force != nil {
			t.Fatalf("default server-side apply set Force=%v", *options.Force)
		}
		var object map[string]any
		if err := json.Unmarshal(patchAction.GetPatch(), &object); err != nil {
			t.Fatal(err)
		}
		patches++
		return true, &unstructured.Unstructured{Object: object}, nil
	})
	if err := Apply(context.Background(), client, force); err != nil {
		t.Fatal(err)
	}
	if patches == 0 {
		t.Fatal("no embedded manifests were applied")
	}
}
