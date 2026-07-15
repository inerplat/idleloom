package install

import (
	"context"
	"fmt"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	"github.com/inerplat/idleloom/internal/native/devruntime"
	nativekube "github.com/inerplat/idleloom/internal/native/kube"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const lockedModelContextLength = int32(2048)

const (
	lockedOllamaModelName   = "qwen3.5:9b"
	lockedOllamaModelDigest = "sha256:6488c96fa5faab64bb65cbd30d4289e20e6130ef535a93ef9a49f42eda893ea7"
	lockedOllamaModelSize   = int64(6594474711)
)

func ApplyCatalog(ctx context.Context, client dynamic.Interface, forceConflicts bool) error {
	descriptor, err := devruntime.LockedModel()
	if err != nil {
		return err
	}
	mlxModel := &nativev1alpha1.IdleloomModel{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomModel"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   descriptor.Name,
			Labels: map[string]string{"app.kubernetes.io/managed-by": "idleloom"},
		},
		Spec: nativev1alpha1.IdleloomModelSpec{
			Family: nativev1alpha1.ModelFamilyQwen35, RuntimeProfile: nativev1alpha1.RuntimeProfileMLXLMV1,
			Artifact: nativev1alpha1.ModelArtifact{
				OCIReference: descriptor.ArtifactIdentity, ManifestDigest: descriptor.ManifestDigest,
				Format: nativev1alpha1.ArtifactFormatSafetensorsV1, SizeBytes: descriptor.SizeBytes,
				Signature: &nativev1alpha1.SignaturePolicy{
					Issuer: "idleloom-development-lock", Subject: descriptor.Repository + "@" + descriptor.Revision,
				},
			},
			MinimumUnifiedMemory: nativev1alpha1.MinimumUnifiedMemoryForModel(descriptor.SizeBytes, lockedModelContextLength),
			MaxContextLength:     lockedModelContextLength, MaxConcurrentRequests: 1,
		},
	}
	ollamaModel := &nativev1alpha1.IdleloomModel{
		TypeMeta: metav1.TypeMeta{APIVersion: nativev1alpha1.GroupVersion.String(), Kind: "IdleloomModel"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   "qwen3-5-9b-ollama",
			Labels: map[string]string{"app.kubernetes.io/managed-by": "idleloom"},
		},
		Spec: nativev1alpha1.IdleloomModelSpec{
			Family: nativev1alpha1.ModelFamilyOllamaGGUF, RuntimeProfile: nativev1alpha1.RuntimeProfileOllamaGGUFV1,
			Artifact: nativev1alpha1.ModelArtifact{
				OllamaModel: lockedOllamaModelName, ManifestDigest: lockedOllamaModelDigest,
				Format: nativev1alpha1.ArtifactFormatGGUFV1, SizeBytes: lockedOllamaModelSize,
			},
			MinimumUnifiedMemory: nativev1alpha1.MinimumUnifiedMemoryForModel(lockedOllamaModelSize, lockedModelContextLength),
			MaxContextLength:     lockedModelContextLength, MaxConcurrentRequests: 1,
		},
	}
	for _, model := range []*nativev1alpha1.IdleloomModel{mlxModel, ollamaModel} {
		if err := nativev1alpha1.ValidateModel(model); err != nil {
			return fmt.Errorf("validate locked Native model catalog %s: %w", model.Name, err)
		}
		object, err := nativekube.ToUnstructured(model)
		if err != nil {
			return err
		}
		payload, err := object.MarshalJSON()
		if err != nil {
			return err
		}
		options := metav1.PatchOptions{FieldManager: "idleloom"}
		if forceConflicts {
			options.Force = ptr(true)
		}
		if _, err := client.Resource(nativekube.ModelsGVR).Patch(ctx, model.Name, types.ApplyPatchType, payload, options); err != nil {
			return fmt.Errorf("apply locked Native model catalog %s: %w", model.Name, err)
		}
	}
	return nil
}
