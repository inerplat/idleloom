package manifests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
)

func TestPublicManifestsDecodeStrictly(t *testing.T) {
	decodeScheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{scheme.AddToScheme, nativev1alpha1.AddToScheme} {
		if err := add(decodeScheme); err != nil {
			t.Fatal(err)
		}
	}
	for _, root := range []string{
		filepath.Join("..", "..", "deploy"),
		filepath.Join("..", "..", "examples"),
	} {
		if err := walkManifests(root, decodeScheme); err != nil {
			t.Fatal(err)
		}
	}
}

func walkManifests(root string, decodeScheme *runtime.Scheme) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".yaml" {
			return nil
		}
		// Recipe values are rendered by internal/recipe tests instead.
		if entry.Name() == "kustomization.yaml" || strings.HasSuffix(entry.Name(), "-values.yaml") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		reader := yaml.NewYAMLReader(bufio.NewReaderSize(bytes.NewReader(data), 4096))
		strict := serializer.NewCodecFactory(decodeScheme, serializer.EnableStrict).UniversalDeserializer()
		for document := 1; ; document++ {
			raw, err := reader.Read()
			if err != nil {
				if err == io.EOF {
					break
				}
				return fmt.Errorf("decode %s document %d: %w", path, document, err)
			}
			if len(raw) == 0 {
				continue
			}
			jsonData, err := yaml.ToJSON(raw)
			if err != nil {
				return fmt.Errorf("convert %s document %d: %w", path, document, err)
			}
			var typeMeta metav1.TypeMeta
			if err := json.Unmarshal(jsonData, &typeMeta); err != nil {
				return fmt.Errorf("decode type metadata in %s document %d: %w", path, document, err)
			}
			// Generated CRDs are validated structurally by internal/native tests.
			if typeMeta.APIVersion == "apiextensions.k8s.io/v1" && typeMeta.Kind == "CustomResourceDefinition" {
				continue
			}
			if _, _, err := strict.Decode(jsonData, nil, nil); err != nil {
				return fmt.Errorf("strict decode %s document %d: %w", path, document, err)
			}
		}
		return nil
	})
}
