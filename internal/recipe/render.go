package recipe

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/template"

	nativev1alpha1 "github.com/inerplat/idleloom/api/native/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/validation"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"
)

const (
	managedByLabel   = "app.kubernetes.io/managed-by"
	runLabel         = "ai.idleloom.io/run"
	taskLabel        = "ai.idleloom.io/task"
	backendLabel     = "ai.idleloom.io/backend"
	runtimeLabel     = "ai.idleloom.io/runtime"
	recipeAnno       = "ai.idleloom.io/recipe"
	recipeDigestAnno = "ai.idleloom.io/recipe-digest"
	inputAnno        = "ai.idleloom.io/input-digest"
)

type RenderOptions struct {
	Name   string
	Values []byte
}

type RenderResult struct {
	Definition   Definition
	RecipeDigest string
	Values       map[string]any
	InputDigest  string
	Manifest     []byte
}

type templateData struct {
	Name         string
	Namespace    string
	RecipeID     string
	RecipeDigest string
	InputDigest  string
	Values       map[string]any
}

func (r *Registry) Render(id string, options RenderOptions) (RenderResult, error) {
	item, err := r.get(id)
	if err != nil {
		return RenderResult{}, err
	}
	if problems := validation.IsDNS1123Label(options.Name); len(problems) > 0 {
		return RenderResult{}, fmt.Errorf("run name %q must be a DNS label: %s", options.Name, strings.Join(problems, "; "))
	}
	values, err := item.schema.normalize(options.Values)
	if err != nil {
		return RenderResult{}, err
	}
	namespace, ok := values["namespace"].(string)
	if !ok || namespace == "" {
		return RenderResult{}, fmt.Errorf("recipe %s must define a namespace parameter", id)
	}
	digest, err := inputDigest(id, item.contentDigest, options.Name, values)
	if err != nil {
		return RenderResult{}, err
	}
	parsed, err := template.New(item.definition.Manifest).Funcs(template.FuncMap{
		"integer": templateInteger,
		"quote":   templateQuote,
	}).Option("missingkey=error").Parse(string(item.template))
	if err != nil {
		return RenderResult{}, fmt.Errorf("parse manifest template for %s: %w", id, err)
	}
	var output bytes.Buffer
	data := templateData{Name: options.Name, Namespace: namespace, RecipeID: id, RecipeDigest: item.contentDigest, InputDigest: digest, Values: values}
	if err := parsed.Execute(&output, data); err != nil {
		return RenderResult{}, fmt.Errorf("render manifest for %s: %w", id, err)
	}
	manifest := output.Bytes()
	if len(manifest) == 0 || manifest[len(manifest)-1] != '\n' {
		manifest = append(manifest, '\n')
	}
	if err := validateRenderedManifest(manifest, item.definition, data); err != nil {
		return RenderResult{}, fmt.Errorf("validate rendered manifest for %s: %w", id, err)
	}
	return RenderResult{
		Definition: item.definition, RecipeDigest: item.contentDigest, Values: values, InputDigest: digest,
		Manifest: append([]byte(nil), manifest...),
	}, nil
}

func inputDigest(recipeID, recipeDigest, name string, values map[string]any) (string, error) {
	payload := struct {
		Recipe       string         `json:"recipe"`
		RecipeDigest string         `json:"recipeDigest"`
		Name         string         `json:"name"`
		Values       map[string]any `json:"values"`
	}{Recipe: recipeID, RecipeDigest: recipeDigest, Name: name, Values: values}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode recipe input: %w", err)
	}
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func templateQuote(value any) (string, error) {
	data, err := json.Marshal(fmt.Sprint(value))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func templateInteger(value any) (string, error) {
	number, ok := value.(json.Number)
	if !ok {
		return "", fmt.Errorf("%v is not an integer", value)
	}
	if _, err := number.Int64(); err != nil {
		return "", fmt.Errorf("%v is not an integer", value)
	}
	return number.String(), nil
}

func validateRenderedManifest(manifest []byte, definition Definition, data templateData) error {
	scheme := runtime.NewScheme()
	if err := kubescheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := nativev1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	decoder := serializer.NewCodecFactory(scheme, serializer.EnableStrict).UniversalDeserializer()
	reader := utilyaml.NewYAMLReader(bufio.NewReaderSize(bytes.NewReader(manifest), 64<<10))
	documents := 0
	executionRoots := 0
	for {
		raw, err := reader.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		documents++
		jsonData, err := yaml.YAMLToJSONStrict(raw)
		if err != nil {
			return err
		}
		object, _, err := decoder.Decode(jsonData, nil, nil)
		if err != nil {
			return err
		}
		metadataObject, ok := object.(metav1.Object)
		if !ok {
			return fmt.Errorf("rendered object does not expose Kubernetes metadata")
		}
		if metadataObject.GetName() == "" || metadataObject.GetNamespace() != data.Namespace {
			return fmt.Errorf("rendered object must have a name and namespace %q", data.Namespace)
		}
		if err := validateMetadata(metadataObject.GetLabels(), metadataObject.GetAnnotations(), definition, data); err != nil {
			return err
		}
		switch typed := object.(type) {
		case *nativev1alpha1.IdleloomWorkload:
			if definition.Backend != "native" {
				return fmt.Errorf("worker recipe rendered an IdleloomWorkload")
			}
			executionRoots++
			if typed.Name != data.Name {
				return fmt.Errorf("Native execution root must be named %q", data.Name)
			}
			if err := nativev1alpha1.ValidateWorkload(typed); err != nil {
				return err
			}
		case *batchv1.Job:
			if definition.Backend != "worker" {
				return fmt.Errorf("native recipe rendered a Job")
			}
			executionRoots++
			if typed.Name != data.Name {
				return fmt.Errorf("Worker execution root must be named %q", data.Name)
			}
			if typed.Spec.Template.Spec.RestartPolicy != "Never" || len(typed.Spec.Template.Spec.Containers) == 0 {
				return fmt.Errorf("worker Job must use restartPolicy Never and define a container")
			}
			if err := validateMetadata(typed.Spec.Template.Labels, typed.Spec.Template.Annotations, definition, data); err != nil {
				return fmt.Errorf("pod template metadata: %w", err)
			}
		case *batchv1.CronJob:
			if definition.Backend != "worker" {
				return fmt.Errorf("native recipe rendered a CronJob")
			}
			executionRoots++
			if typed.Name != data.Name || len(typed.Spec.JobTemplate.Spec.Template.Spec.Containers) == 0 {
				return fmt.Errorf("Worker CronJob execution root must be named %q and define a container", data.Name)
			}
			if err := validateMetadata(typed.Spec.JobTemplate.Spec.Template.Labels, typed.Spec.JobTemplate.Spec.Template.Annotations, definition, data); err != nil {
				return fmt.Errorf("pod template metadata: %w", err)
			}
		case *appsv1.Deployment:
			if definition.Backend != "worker" {
				return fmt.Errorf("native recipe rendered a Deployment")
			}
			executionRoots++
			if typed.Name != data.Name || len(typed.Spec.Template.Spec.Containers) == 0 {
				return fmt.Errorf("Worker Deployment execution root must be named %q and define a container", data.Name)
			}
			if err := validateMetadata(typed.Spec.Template.Labels, typed.Spec.Template.Annotations, definition, data); err != nil {
				return fmt.Errorf("pod template metadata: %w", err)
			}
		case *corev1.Pod:
			if definition.Backend != "worker" {
				return fmt.Errorf("native recipe rendered a Pod")
			}
			executionRoots++
			if typed.Name != data.Name || len(typed.Spec.Containers) == 0 {
				return fmt.Errorf("Worker Pod execution root must be named %q and define a container", data.Name)
			}
		}
	}
	if documents == 0 {
		return fmt.Errorf("recipe rendered no objects")
	}
	if executionRoots != 1 {
		return fmt.Errorf("recipe must render exactly one execution root, got %d", executionRoots)
	}
	return nil
}

func validateMetadata(labels, annotations map[string]string, definition Definition, data templateData) error {
	wantLabels := map[string]string{
		managedByLabel: "idleloom", runLabel: data.Name, taskLabel: definition.Task,
		backendLabel: definition.Backend, runtimeLabel: definition.Runtime,
	}
	for key, want := range wantLabels {
		if labels[key] != want {
			return fmt.Errorf("label %s must be %q", key, want)
		}
	}
	if annotations[recipeAnno] != definition.ID() {
		return fmt.Errorf("annotation %s must be %q", recipeAnno, definition.ID())
	}
	if annotations[recipeDigestAnno] != data.RecipeDigest {
		return fmt.Errorf("annotation %s must be %q", recipeDigestAnno, data.RecipeDigest)
	}
	if annotations[inputAnno] != data.InputDigest {
		return fmt.Errorf("annotation %s must be %q", inputAnno, data.InputDigest)
	}
	return nil
}
