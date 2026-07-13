package recipe

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/inerplat/idleloom/recipes"
	"sigs.k8s.io/yaml"
)

const definitionFile = "recipe.yaml"

type Definition struct {
	Name             string   `json:"name"`
	Version          string   `json:"version"`
	Description      string   `json:"description"`
	Task             string   `json:"task"`
	Backend          string   `json:"backend"`
	Runtime          string   `json:"runtime"`
	Manifest         string   `json:"manifest"`
	ParametersSchema string   `json:"parametersSchema"`
	Example          string   `json:"example"`
	Prerequisites    []string `json:"prerequisites,omitempty"`
}

func (d Definition) ID() string {
	return d.Name + "@" + d.Version
}

type Parameter struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     any    `json:"default,omitempty"`
}

type Details struct {
	ID            string         `json:"id"`
	Digest        string         `json:"digest"`
	Description   string         `json:"description"`
	Task          string         `json:"task"`
	Backend       string         `json:"backend"`
	Runtime       string         `json:"runtime"`
	Prerequisites []string       `json:"prerequisites,omitempty"`
	Parameters    []Parameter    `json:"parameters"`
	Example       map[string]any `json:"example"`
}

type entry struct {
	definition    Definition
	contentDigest string
	template      []byte
	schema        parameterSchema
	example       []byte
	exampleValues map[string]any
}

type Registry struct {
	entries map[string]entry
	ids     []string
}

func DefaultRegistry() (*Registry, error) {
	return Load(recipes.FS)
}

func Load(source fs.FS) (*Registry, error) {
	registry := &Registry{entries: make(map[string]entry)}
	err := fs.WalkDir(source, ".", func(filePath string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if item.IsDir() || item.Name() != definitionFile {
			return nil
		}
		definitionData, err := fs.ReadFile(source, filePath)
		if err != nil {
			return err
		}
		var definition Definition
		if err := yaml.UnmarshalStrict(definitionData, &definition); err != nil {
			return fmt.Errorf("decode %s: %w", filePath, err)
		}
		if err := validateDefinition(definition); err != nil {
			return fmt.Errorf("validate %s: %w", filePath, err)
		}
		directory := path.Dir(filePath)
		template, err := readReferencedFile(source, directory, definition.Manifest)
		if err != nil {
			return fmt.Errorf("load manifest for %s: %w", definition.ID(), err)
		}
		schemaData, err := readReferencedFile(source, directory, definition.ParametersSchema)
		if err != nil {
			return fmt.Errorf("load parameter schema for %s: %w", definition.ID(), err)
		}
		schema, err := parseParameterSchema(schemaData)
		if err != nil {
			return fmt.Errorf("validate parameter schema for %s: %w", definition.ID(), err)
		}
		example, err := readReferencedFile(source, directory, definition.Example)
		if err != nil {
			return fmt.Errorf("load example for %s: %w", definition.ID(), err)
		}
		exampleValues, err := schema.normalize(example)
		if err != nil {
			return fmt.Errorf("validate example for %s: %w", definition.ID(), err)
		}
		id := definition.ID()
		if _, exists := registry.entries[id]; exists {
			return fmt.Errorf("duplicate recipe %s", id)
		}
		registry.entries[id] = entry{
			definition: definition, contentDigest: recipeContentDigest(definitionData, schemaData, template),
			template: template, schema: schema, example: example, exampleValues: exampleValues,
		}
		registry.ids = append(registry.ids, id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(registry.entries) == 0 {
		return nil, fmt.Errorf("recipe registry is empty")
	}
	sort.Strings(registry.ids)
	return registry, nil
}

func (r *Registry) List() []Definition {
	definitions := make([]Definition, 0, len(r.ids))
	for _, id := range r.ids {
		definitions = append(definitions, r.entries[id].definition)
	}
	return definitions
}

func (r *Registry) Details(id string) (Details, error) {
	item, err := r.get(id)
	if err != nil {
		return Details{}, err
	}
	required := make(map[string]bool, len(item.schema.Required))
	for _, name := range item.schema.Required {
		required[name] = true
	}
	names := make([]string, 0, len(item.schema.Properties))
	for name := range item.schema.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	parameters := make([]Parameter, 0, len(names))
	for _, name := range names {
		property := item.schema.Properties[name]
		parameters = append(parameters, Parameter{
			Name: name, Type: property.Type, Description: property.Description,
			Required: required[name], Default: property.Default,
		})
	}
	return Details{
		ID: item.definition.ID(), Digest: item.contentDigest, Description: item.definition.Description,
		Task: item.definition.Task, Backend: item.definition.Backend, Runtime: item.definition.Runtime,
		Prerequisites: append([]string(nil), item.definition.Prerequisites...), Parameters: parameters,
		Example: cloneValues(item.exampleValues),
	}, nil
}

func (r *Registry) get(id string) (entry, error) {
	if !strings.Contains(id, "@") {
		return entry{}, fmt.Errorf("recipe %q is not version-pinned; use NAME@VERSION", id)
	}
	item, exists := r.entries[id]
	if !exists {
		return entry{}, fmt.Errorf("unknown recipe %q", id)
	}
	return item, nil
}

func readReferencedFile(source fs.FS, directory, name string) ([]byte, error) {
	if name == "" || !fs.ValidPath(name) || path.Base(name) != name {
		return nil, fmt.Errorf("reference %q must be a file in the recipe directory", name)
	}
	return fs.ReadFile(source, path.Join(directory, name))
}

func validateDefinition(definition Definition) error {
	for field, value := range map[string]string{
		"name": definition.Name, "version": definition.Version, "description": definition.Description,
		"task": definition.Task, "backend": definition.Backend, "runtime": definition.Runtime,
		"manifest": definition.Manifest, "parametersSchema": definition.ParametersSchema, "example": definition.Example,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", field)
		}
	}
	segments := strings.Split(definition.Name, "/")
	if len(segments) != 2 || segments[0] != definition.Task || !dnsLabel(segments[0]) || !dnsLabel(segments[1]) {
		return fmt.Errorf("name must be TASK/NAME using DNS labels")
	}
	if !strings.HasPrefix(definition.Version, "v") || !dnsLabel(definition.Version) {
		return fmt.Errorf("version must be a DNS label beginning with v")
	}
	if definition.Backend != "native" && definition.Backend != "worker" {
		return fmt.Errorf("backend must be native or worker")
	}
	if !dnsLabel(definition.Runtime) {
		return fmt.Errorf("runtime must be a DNS label")
	}
	return nil
}
