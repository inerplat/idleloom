package recipe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

var parameterNamePattern = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)

type parameterSchema struct {
	Type                 string                    `json:"type"`
	AdditionalProperties *bool                     `json:"additionalProperties"`
	Properties           map[string]propertySchema `json:"properties"`
	Required             []string                  `json:"required,omitempty"`
}

type propertySchema struct {
	Type             string   `json:"type"`
	Description      string   `json:"description,omitempty"`
	Default          any      `json:"default,omitempty"`
	Pattern          string   `json:"pattern,omitempty"`
	Format           string   `json:"format,omitempty"`
	Minimum          *float64 `json:"minimum,omitempty"`
	ExclusiveMinimum *float64 `json:"exclusiveMinimum,omitempty"`
	Maximum          *float64 `json:"maximum,omitempty"`
	MinLength        *int     `json:"minLength,omitempty"`
	MaxLength        *int     `json:"maxLength,omitempty"`
}

func parseParameterSchema(data []byte) (parameterSchema, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var schema parameterSchema
	if err := decoder.Decode(&schema); err != nil {
		return parameterSchema{}, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return parameterSchema{}, fmt.Errorf("schema contains more than one JSON value")
		}
		return parameterSchema{}, fmt.Errorf("decode trailing schema data: %w", err)
	}
	if schema.Type != "object" {
		return parameterSchema{}, fmt.Errorf("schema type must be object")
	}
	if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
		return parameterSchema{}, fmt.Errorf("additionalProperties must be false")
	}
	if len(schema.Properties) == 0 {
		return parameterSchema{}, fmt.Errorf("properties must not be empty")
	}
	required := make(map[string]bool, len(schema.Required))
	for _, name := range schema.Required {
		if _, exists := schema.Properties[name]; !exists {
			return parameterSchema{}, fmt.Errorf("required parameter %q is not defined", name)
		}
		if required[name] {
			return parameterSchema{}, fmt.Errorf("required parameter %q is duplicated", name)
		}
		required[name] = true
	}
	for name, property := range schema.Properties {
		if !parameterNamePattern.MatchString(name) {
			return parameterSchema{}, fmt.Errorf("parameter name %q is invalid", name)
		}
		if err := validatePropertySchema(name, property); err != nil {
			return parameterSchema{}, err
		}
		if property.Default != nil {
			if err := validateParameter(name, property.Default, property); err != nil {
				return parameterSchema{}, fmt.Errorf("invalid default: %w", err)
			}
		}
	}
	return schema, nil
}

func validatePropertySchema(name string, property propertySchema) error {
	switch property.Type {
	case "string", "integer", "number", "boolean":
	default:
		return fmt.Errorf("parameter %q has unsupported type %q", name, property.Type)
	}
	if property.Pattern != "" {
		if property.Type != "string" {
			return fmt.Errorf("parameter %q uses pattern with non-string type", name)
		}
		if _, err := regexp.Compile(property.Pattern); err != nil {
			return fmt.Errorf("parameter %q has invalid pattern: %w", name, err)
		}
	}
	if property.Format != "" && property.Format != "namespace" && property.Format != "dnsSubdomain" && property.Format != "positiveQuantity" && property.Format != "httpsURL" && property.Format != "sha256Hex" {
		return fmt.Errorf("parameter %q has unsupported format %q", name, property.Format)
	}
	if property.Format != "" && property.Type != "string" {
		return fmt.Errorf("parameter %q uses a string format with type %q", name, property.Type)
	}
	if (property.MinLength != nil || property.MaxLength != nil) && property.Type != "string" {
		return fmt.Errorf("parameter %q uses string length limits with type %q", name, property.Type)
	}
	if (property.MinLength != nil && *property.MinLength < 0) || (property.MaxLength != nil && *property.MaxLength < 0) {
		return fmt.Errorf("parameter %q has a negative string length limit", name)
	}
	if property.MinLength != nil && property.MaxLength != nil && *property.MinLength > *property.MaxLength {
		return fmt.Errorf("parameter %q has minLength greater than maxLength", name)
	}
	return nil
}

func parseValues(data []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	jsonData, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return nil, fmt.Errorf("decode values YAML: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
	decoder.UseNumber()
	var values map[string]any
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("decode values object: %w", err)
	}
	if values == nil {
		return nil, fmt.Errorf("values must be a YAML object")
	}
	return values, nil
}

func (s parameterSchema) normalize(data []byte) (map[string]any, error) {
	values, err := parseValues(data)
	if err != nil {
		return nil, err
	}
	unknown := make([]string, 0)
	for name := range values {
		if _, exists := s.Properties[name]; !exists {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown parameter %q", unknown[0])
	}
	required := make(map[string]bool, len(s.Required))
	for _, name := range s.Required {
		required[name] = true
	}
	names := make([]string, 0, len(s.Properties))
	for name := range s.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		property := s.Properties[name]
		value, exists := values[name]
		if !exists && property.Default != nil {
			value = property.Default
			values[name] = value
			exists = true
		}
		if !exists {
			if required[name] {
				return nil, fmt.Errorf("parameter %q is required", name)
			}
			continue
		}
		if err := validateParameter(name, value, property); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func validateParameter(name string, value any, property propertySchema) error {
	number, isNumber := value.(json.Number)
	switch property.Type {
	case "string":
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("parameter %q must be a string", name)
		}
		if property.Pattern != "" && !regexp.MustCompile(property.Pattern).MatchString(text) {
			return fmt.Errorf("parameter %q does not match %s", name, property.Pattern)
		}
		if property.MinLength != nil && len([]byte(text)) < *property.MinLength {
			return fmt.Errorf("parameter %q must contain at least %d UTF-8 bytes", name, *property.MinLength)
		}
		if property.MaxLength != nil && len([]byte(text)) > *property.MaxLength {
			return fmt.Errorf("parameter %q must contain at most %d UTF-8 bytes", name, *property.MaxLength)
		}
		switch property.Format {
		case "namespace":
			if problems := validation.IsDNS1123Label(text); len(problems) > 0 {
				return fmt.Errorf("parameter %q must be a Kubernetes namespace: %s", name, strings.Join(problems, "; "))
			}
		case "dnsSubdomain":
			if problems := validation.IsDNS1123Subdomain(text); len(problems) > 0 {
				return fmt.Errorf("parameter %q must be a Kubernetes DNS subdomain: %s", name, strings.Join(problems, "; "))
			}
		case "positiveQuantity":
			quantity, err := resource.ParseQuantity(text)
			if err != nil || quantity.Sign() <= 0 {
				return fmt.Errorf("parameter %q must be a positive Kubernetes quantity", name)
			}
		case "httpsURL":
			parsed, err := url.Parse(text)
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
				return fmt.Errorf("parameter %q must be a credential-free HTTPS URL without a fragment", name)
			}
		case "sha256Hex":
			if matched, _ := regexp.MatchString(`^[a-f0-9]{64}$`, text); !matched {
				return fmt.Errorf("parameter %q must contain 64 lowercase SHA-256 hexadecimal characters", name)
			}
		}
		return nil
	case "integer":
		if !isNumber {
			return fmt.Errorf("parameter %q must be an integer", name)
		}
		integer, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil {
			return fmt.Errorf("parameter %q must be an integer", name)
		}
		return validateNumberBounds(name, float64(integer), property)
	case "number":
		if !isNumber {
			return fmt.Errorf("parameter %q must be a number", name)
		}
		parsed, err := number.Float64()
		if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
			return fmt.Errorf("parameter %q must be a finite number", name)
		}
		return validateNumberBounds(name, parsed, property)
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("parameter %q must be a boolean", name)
		}
		return nil
	default:
		return fmt.Errorf("parameter %q has unsupported schema type %q", name, property.Type)
	}
}

func validateNumberBounds(name string, value float64, property propertySchema) error {
	if property.Minimum != nil && value < *property.Minimum {
		return fmt.Errorf("parameter %q must be at least %s", name, formatNumber(*property.Minimum))
	}
	if property.ExclusiveMinimum != nil && value <= *property.ExclusiveMinimum {
		return fmt.Errorf("parameter %q must be greater than %s", name, formatNumber(*property.ExclusiveMinimum))
	}
	if property.Maximum != nil && value > *property.Maximum {
		return fmt.Errorf("parameter %q must be at most %s", name, formatNumber(*property.Maximum))
	}
	return nil
}

func formatNumber(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func dnsLabel(value string) bool {
	return len(validation.IsDNS1123Label(value)) == 0
}
