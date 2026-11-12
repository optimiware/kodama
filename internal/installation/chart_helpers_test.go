package installation

// Static chart helpers. The deployment assets are checked as text and as a
// small, explicitly bounded JSON-Schema subset using only the standard
// library: no helm binary, no YAML dependency and no template rendering.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// chartRequiredKeys is the top-level values contract.
var chartRequiredKeys = []string{
	"environment",
	"image",
	"replicaCount",
	"config",
	"secrets",
	"tenants",
	"networkPolicy",
	"securityContext",
	"resources",
}

// repoRoot walks up from the test working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// chartPath resolves one path inside deploy/charts/kodama.
func chartPath(t *testing.T, parts ...string) string {
	t.Helper()
	all := append([]string{repoRoot(t), "deploy", "charts", "kodama"}, parts...)
	return filepath.Join(all...)
}

// readText reads one required deployment file.
func readText(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// templateBodies returns every chart template body keyed by file name.
func templateBodies(t *testing.T) map[string]string {
	t.Helper()
	dir := chartPath(t, "templates")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read chart templates: %v", err)
	}
	bodies := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		switch filepath.Ext(name) {
		case ".yaml", ".yml", ".tpl":
			bodies[name] = readText(t, filepath.Join(dir, name))
		}
	}
	if len(bodies) == 0 {
		t.Fatalf("no chart templates in %s", dir)
	}
	return bodies
}

// templateContaining returns the first template body that contains marker.
func templateContaining(t *testing.T, bodies map[string]string, marker string) (string, string) {
	t.Helper()
	names := make([]string, 0, len(bodies))
	for name := range bodies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.Contains(bodies[name], marker) {
			return name, bodies[name]
		}
	}
	t.Fatalf("no chart template contains %q", marker)
	return "", ""
}

// requirePattern fails unless body matches expression.
func requirePattern(t *testing.T, body, expression, description string) {
	t.Helper()
	if !regexp.MustCompile(expression).MatchString(body) {
		t.Fatalf("%s: no match for %s", description, expression)
	}
}

// loadJSON decodes one JSON document.
func loadJSON(t *testing.T, path string) any {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return value
}

// schemaKeywords is the bounded JSON-Schema subset the chart schema may use.
var schemaKeywords = map[string]bool{
	"type":                 true,
	"required":             true,
	"properties":           true,
	"additionalProperties": true,
	"propertyNames":        true,
	"enum":                 true,
	"const":                true,
	"items":                true,
	"minimum":              true,
	"maximum":              true,
	"minItems":             true,
	"minLength":            true,
	"maxLength":            true,
	"pattern":              true,
	"not":                  true,
	"allOf":                true,
	"if":                   true,
	"then":                 true,
	"else":                 true,
}

// schemaMetadataKeywords are ignored annotations.
var schemaMetadataKeywords = map[string]bool{
	"$schema":     true,
	"$id":         true,
	"title":       true,
	"description": true,
	"default":     true,
	"examples":    true,
	"comment":     true,
}

// validateAgainstSchema validates value against the allowed schema subset. An
// unsupported keyword is an error so the schema cannot silently depend on
// validation the tests do not perform.
func validateAgainstSchema(schema map[string]any, value any, path string) error {
	for keyword := range schema {
		if !schemaKeywords[keyword] && !schemaMetadataKeywords[keyword] {
			return fmt.Errorf("%s: unsupported schema keyword %q", path, keyword)
		}
	}

	if expected, ok := schema["type"].(string); ok {
		if err := checkSchemaType(expected, value, path); err != nil {
			return err
		}
	}
	if negation, ok := schema["not"].(map[string]any); ok {
		if err := validateAgainstSchema(negation, value, path); err == nil {
			return fmt.Errorf("%s: value %v matches a forbidden schema", path, value)
		}
	}
	if values, ok := schema["enum"].([]any); ok {
		matched := false
		for _, candidate := range values {
			if jsonEqual(candidate, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: value %v is not one of the declared enum values", path, value)
		}
	}
	if constant, ok := schema["const"]; ok {
		if !jsonEqual(constant, value) {
			return fmt.Errorf("%s: value %v is not the declared constant %v", path, value, constant)
		}
	}

	switch typed := value.(type) {
	case map[string]any:
		if err := validateObject(schema, typed, path); err != nil {
			return err
		}
	case []any:
		if minimum, ok := schemaInt(schema, "minItems"); ok && len(typed) < minimum {
			return fmt.Errorf("%s: array has %d items, fewer than %d", path, len(typed), minimum)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for index, element := range typed {
				if err := validateAgainstSchema(items, element, fmt.Sprintf("%s[%d]", path, index)); err != nil {
					return err
				}
			}
		}
	case string:
		if minimum, ok := schemaInt(schema, "minLength"); ok && utf8.RuneCountInString(typed) < minimum {
			return fmt.Errorf("%s: string is shorter than %d", path, minimum)
		}
		if maximum, ok := schemaInt(schema, "maxLength"); ok && utf8.RuneCountInString(typed) > maximum {
			return fmt.Errorf("%s: string is longer than %d", path, maximum)
		}
		if pattern, ok := schema["pattern"].(string); ok {
			expression, err := regexp.Compile(pattern)
			if err != nil {
				return fmt.Errorf("%s: invalid pattern %q: %v", path, pattern, err)
			}
			if !expression.MatchString(typed) {
				return fmt.Errorf("%s: string %q does not match %q", path, typed, pattern)
			}
		}
	case float64:
		if minimum, ok := schemaNumber(schema, "minimum"); ok && typed < minimum {
			return fmt.Errorf("%s: number %v is below %v", path, typed, minimum)
		}
		if maximum, ok := schemaNumber(schema, "maximum"); ok && typed > maximum {
			return fmt.Errorf("%s: number %v is above %v", path, typed, maximum)
		}
	}

	// Conditional subschemas are evaluated after the direct keywords so the
	// most specific error (a wrong property, for example) is reported first.
	if entries, ok := schema["allOf"].([]any); ok {
		for index, entry := range entries {
			sub, ok := entry.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: allOf[%d] is not a schema object", path, index)
			}
			if err := validateAgainstSchema(sub, value, path); err != nil {
				return err
			}
		}
	}
	if condition, ok := schema["if"].(map[string]any); ok {
		applies := validateAgainstSchema(condition, value, path) == nil
		branch := "else"
		if applies {
			branch = "then"
		}
		if sub, ok := schema[branch].(map[string]any); ok {
			if err := validateAgainstSchema(sub, value, path); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateObject(schema map[string]any, object map[string]any, path string) error {
	properties, _ := schema["properties"].(map[string]any)
	if names, ok := schema["propertyNames"].(map[string]any); ok {
		for name := range object {
			if err := validateAgainstSchema(names, name, path+"."+name); err != nil {
				return fmt.Errorf("%s: property name %q is not allowed: %w", path, name, err)
			}
		}
	}
	if required, ok := schema["required"].([]any); ok {
		for _, entry := range required {
			name, ok := entry.(string)
			if !ok {
				return fmt.Errorf("%s: required entry is not a string", path)
			}
			if _, present := object[name]; !present {
				return fmt.Errorf("%s: missing required property %q", path, name)
			}
		}
	}
	for name, value := range object {
		if property, ok := properties[name].(map[string]any); ok {
			if err := validateAgainstSchema(property, value, path+"."+name); err != nil {
				return err
			}
			continue
		}
		switch additional := schema["additionalProperties"].(type) {
		case bool:
			if !additional {
				return fmt.Errorf("%s: unexpected property %q", path, name)
			}
		case map[string]any:
			if err := validateAgainstSchema(additional, value, path+"."+name); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkSchemaType(expected string, value any, path string) error {
	switch expected {
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("%s: value is not an object", path)
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("%s: value is not an array", path)
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: value is not a string", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: value is not a boolean", path)
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("%s: value is not a number", path)
		}
	case "integer":
		number, ok := value.(float64)
		if !ok || math.Trunc(number) != number {
			return fmt.Errorf("%s: value is not an integer", path)
		}
	default:
		return fmt.Errorf("%s: unsupported schema type %q", path, expected)
	}
	return nil
}

func schemaInt(schema map[string]any, keyword string) (int, bool) {
	number, ok := schemaNumber(schema, keyword)
	if !ok || math.Trunc(number) != number {
		return 0, false
	}
	return int(number), true
}

func schemaNumber(schema map[string]any, keyword string) (float64, bool) {
	number, ok := schema[keyword].(float64)
	return number, ok
}

// jsonEqual compares two decoded JSON values.
func jsonEqual(left, right any) bool {
	leftNumber, leftIsNumber := left.(float64)
	rightNumber, rightIsNumber := right.(float64)
	if leftIsNumber && rightIsNumber {
		return leftNumber == rightNumber
	}
	return fmt.Sprintf("%#v", left) == fmt.Sprintf("%#v", right)
}

// objectAt asserts one decoded JSON object.
func objectAt(t *testing.T, value any, path string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %T", path, value)
	}
	return object
}

// stringsAt asserts one decoded JSON array of strings.
func stringsAt(t *testing.T, value any, path string) []string {
	t.Helper()
	array, ok := value.([]any)
	if !ok {
		t.Fatalf("%s is not an array: %T", path, value)
	}
	values := make([]string, 0, len(array))
	for _, entry := range array {
		text, ok := entry.(string)
		if !ok {
			t.Fatalf("%s contains a non-string entry: %T", path, entry)
		}
		values = append(values, text)
	}
	return values
}

// sortedCopy returns a sorted copy of values.
func sortedCopy(values []string) []string {
	copied := append([]string(nil), values...)
	sort.Strings(copied)
	return copied
}
