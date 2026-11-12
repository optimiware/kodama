package installation

// Tests for the config.extraEnv contract of the Kodama chart.
//
// config.extraEnv is a pass-through for the optional server environment the Go
// composition reads. It must never be able to override a fixed production
// variable, inject a development token or a cloud credential, carry an object
// (valueFrom) or render a literal password in the manifest.
//
// The schema is one layer of defence and the template guard is the other; the
// tests pin both independently so a regression in either is caught. The
// rendered behaviour of the same rules is checked with helm in
// test/component/deploy.

import (
	"fmt"
	"maps"
	"regexp"
	"strings"
	"testing"
)

// extraEnvAllowlistPattern is the single allowlist expression. It must appear
// verbatim in values.schema.json (propertyNames) and in _extraenv.tpl.
const extraEnvAllowlistPattern = `^(KODAMA_OIDC_(ISSUER|AUDIENCE|JWKS_URL|TENANT_CLAIM|SUBJECT_CLAIM|PROJECT_CLAIM|CA_FILE)|KODAMA_DISCOVERY_(ISSUER|CLIENT_ID|AUDIENCE|AUTHORIZATION_ENDPOINT|TOKEN_ENDPOINT|DEVICE_AUTHORIZATION_ENDPOINT|REVOCATION_ENDPOINT|SCOPES|API_URL)|KODAMA_TENANT_NAMESPACE|KODAMA_WORKLOAD_(CPU|MEMORY)_(REQUEST|LIMIT))$`

// extraEnvUserinfoPattern refuses scheme://user:password@host.
const extraEnvUserinfoPattern = `://[^/@]*:[^@]*@`

// extraEnvValues builds a schema-valid values document whose only variable is
// config.extraEnv.
func extraEnvValues(entries map[string]any) map[string]any {
	values := fqdnValues([]any{})
	merged := map[string]any{
		"KODAMA_OIDC_ISSUER":       "https://issuer.example",
		"KODAMA_OIDC_AUDIENCE":     "kodama",
		"KODAMA_OIDC_JWKS_URL":     "https://issuer.example/jwks",
		"KODAMA_OIDC_TENANT_CLAIM": "org_id",
	}
	maps.Copy(merged, entries)
	values["config"] = map[string]any{"extraEnv": merged}
	return values
}

func extraEnvSchema(t *testing.T) map[string]any {
	t.Helper()
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	properties := objectAt(t, schema["properties"], "schema.properties")
	config := objectAt(t, properties["config"], "schema.properties.config")
	configProperties := objectAt(t, config["properties"], "schema.properties.config.properties")
	return objectAt(t, configProperties["extraEnv"], "schema.properties.config.properties.extraEnv")
}

// extraEnvGuardBody returns the shared validation template.
func extraEnvGuardBody(t *testing.T) string {
	t.Helper()
	body, ok := templateBodies(t)["_extraenv.tpl"]
	if !ok {
		t.Fatal("missing templates/_extraenv.tpl")
	}
	return body
}

// checkExtraEnvSchema verifies the schema layer: an object whose property
// names are the exact allowlist, whose values are strings, and whose strings
// cannot carry URL userinfo.
func checkExtraEnvSchema(schema map[string]any) error {
	if schema["type"] != "object" {
		return fmt.Errorf("config.extraEnv type = %v, want object", schema["type"])
	}
	names, ok := schema["propertyNames"].(map[string]any)
	if !ok {
		return fmt.Errorf("config.extraEnv has no propertyNames allowlist")
	}
	pattern, _ := names["pattern"].(string)
	if pattern != extraEnvAllowlistPattern {
		return fmt.Errorf("config.extraEnv propertyNames pattern = %q, want the exact allowlist", pattern)
	}
	values, ok := schema["additionalProperties"].(map[string]any)
	if !ok {
		return fmt.Errorf("config.extraEnv additionalProperties is not a value schema")
	}
	if values["type"] != "string" {
		return fmt.Errorf("config.extraEnv values type = %v, want string (valueFrom objects are not allowed)", values["type"])
	}
	negation, ok := values["not"].(map[string]any)
	if !ok {
		return fmt.Errorf("config.extraEnv values do not refuse URL userinfo")
	}
	if negation["pattern"] != extraEnvUserinfoPattern {
		return fmt.Errorf("config.extraEnv userinfo pattern = %q, want %q", negation["pattern"], extraEnvUserinfoPattern)
	}
	return nil
}

// checkExtraEnvGuard verifies the template layer.
func checkExtraEnvGuard(deployment, guard string) error {
	if !regexp.MustCompile(`(?m)^\{\{-?[ \t]*include[ \t]+"kodama\.validateExtraEnv"[ \t]+\.`).MatchString(deployment) {
		return fmt.Errorf("deployment.yaml does not include the extraEnv guard")
	}
	if !regexp.MustCompile(`(?m)^[ \t]*-[ \t]*name:[ \t]*\{\{[ \t]*\$name[ \t]*\|[ \t]*quote[ \t]*\}\}[ \t]*$`).MatchString(deployment) {
		return fmt.Errorf("deployment.yaml does not render the extraEnv name quoted")
	}
	allow := regexp.MustCompile(`\$allowlist[ \t]*:=[ \t]*"([^"]+)"`).FindStringSubmatch(guard)
	if allow == nil {
		return fmt.Errorf("_extraenv.tpl declares no allowlist")
	}
	if allow[1] != extraEnvAllowlistPattern {
		return fmt.Errorf("_extraenv.tpl allowlist = %q, want the exact allowlist", allow[1])
	}
	fixed := regexp.MustCompile(`\$fixed[ \t]*:=[ \t]*list ([^\n]+)`).FindStringSubmatch(guard)
	if fixed == nil {
		return fmt.Errorf("_extraenv.tpl declares no fixed-name collision list")
	}
	for _, name := range chartDirectEnvNames {
		if !strings.Contains(fixed[1], `"`+name+`"`) {
			return fmt.Errorf("_extraenv.tpl collision list does not name %s", name)
		}
	}
	for _, rule := range []struct {
		expression  string
		description string
	}{
		{`kindIs[ \t]+"string"`, "object/valueFrom refusal"},
		{`has[ \t]+\$name[ \t]+\$fixed`, "fixed-name collision refusal"},
		{`hasPrefix[ \t]+"KODAMA_DEV_"`, "development token refusal"},
		{`regexMatch[ \t]+"\^\(SCW\|AWS\|GOOGLE\)_"`, "cloud credential refusal"},
		{`regexMatch[ \t]+"\(\?i\)\(token\|secret\|password\|passwd\|credential\|private\|key\)"`, "credential-shaped name refusal"},
		{`regexMatch[ \t]+"_FILE\$"`, "mounted file exception"},
		{`regexMatch[ \t]+\$allowlist`, "allowlist enforcement"},
		{`regexMatch[ \t]+":\/\/\[\^/@\]\*:\[\^@\]\*@"`, "URL userinfo refusal"},
	} {
		if !regexp.MustCompile(rule.expression).MatchString(guard) {
			return fmt.Errorf("_extraenv.tpl has no %s", rule.description)
		}
	}
	if !strings.Contains(guard, "fail (printf") {
		return fmt.Errorf("_extraenv.tpl does not fail rendering on a refused entry")
	}
	return nil
}

func TestExtraEnvSchemaAllowlistIsExact(t *testing.T) {
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	if err := checkExtraEnvSchema(extraEnvSchema(t)); err != nil {
		t.Fatalf("values.schema.json config.extraEnv: %v", err)
	}

	// The allowlist is exactly the optional surface the unit tests already
	// document; adding a name to one place without the other fails here.
	for _, name := range chartExtraEnvNames {
		if err := validateAgainstSchema(schema, extraEnvValues(map[string]any{name: "x"}), "values"); err != nil {
			t.Errorf("schema refused allowlisted %s: %v", name, err)
		}
	}
	for _, name := range chartDirectEnvNames {
		if err := validateAgainstSchema(schema, extraEnvValues(map[string]any{name: "x"}), "values"); err == nil {
			t.Errorf("schema accepted fixed production variable %s through config.extraEnv", name)
		}
	}
	for _, name := range []string{
		"KODAMA_DEV_TOKEN",
		"KODAMA_DEV_PROFILE",
		"SCW_SECRET_KEY",
		"AWS_ACCESS_KEY_ID",
		"KODAMA_OIDC_TOKEN",
		"KODAMA_SECRET_VALUE",
		"KODAMA_PRIVATE_KEY",
		"KODAMA_OIDC_ISSUER_EXTRA",
		"FOO",
	} {
		if err := validateAgainstSchema(schema, extraEnvValues(map[string]any{name: "x"}), "values"); err == nil {
			t.Errorf("schema accepted non-allowlisted config.extraEnv name %s", name)
		}
	}
}

func TestExtraEnvSchemaRejectsCredentialURLAndObjectValues(t *testing.T) {
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")

	for _, value := range []string{
		"postgres://u:pw@h/db?sslmode=disable",
		"https://user:pass@issuer.example",
		"tls://client:secret@nats:4222",
	} {
		if err := validateAgainstSchema(schema, extraEnvValues(map[string]any{"KODAMA_OIDC_ISSUER": value}), "values"); err == nil {
			t.Errorf("schema accepted URL userinfo value %q", value)
		}
	}
	for _, value := range []string{
		"https://issuer.example",
		"registry.example.com",
		"250m",
		"postgres://kodama@database:5432/kodama",
	} {
		if err := validateAgainstSchema(schema, extraEnvValues(map[string]any{"KODAMA_OIDC_ISSUER": value}), "values"); err != nil {
			t.Errorf("schema refused an ordinary string value %q: %v", value, err)
		}
	}

	object := extraEnvValues(map[string]any{
		"KODAMA_OIDC_ISSUER": map[string]any{
			"valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "x", "key": "y"}},
		},
	})
	if err := validateAgainstSchema(schema, object, "values"); err == nil {
		t.Error("schema accepted an object value (valueFrom) in config.extraEnv")
	}
}

func TestExtraEnvTemplateGuardIsExact(t *testing.T) {
	deployment := readText(t, chartPath(t, "templates", "deployment.yaml"))
	if err := checkExtraEnvGuard(deployment, extraEnvGuardBody(t)); err != nil {
		t.Fatalf("extraEnv template guard: %v", err)
	}
}

func TestExtraEnvTemplateGuardMutationSensitivity(t *testing.T) {
	deployment := readText(t, chartPath(t, "templates", "deployment.yaml"))
	guard := extraEnvGuardBody(t)

	mutations := []struct {
		name string
		body string
		edit func(string) string
	}{
		{"allowlist widened", guard, func(body string) string {
			return strings.Replace(body, extraEnvAllowlistPattern, `.*`, 1)
		}},
		{"fixed collision guard removed", guard, func(body string) string {
			return regexp.MustCompile(`(?m)^.*has \$name \$fixed.*\r?\n.*collides with a fixed variable.*\r?\n`).ReplaceAllString(body, "")
		}},
		{"development guard removed", guard, func(body string) string {
			return regexp.MustCompile(`(?m)^.*hasPrefix "KODAMA_DEV_".*\r?\n.*development token.*\r?\n`).ReplaceAllString(body, "")
		}},
		{"userinfo guard removed", guard, func(body string) string {
			return regexp.MustCompile(`(?m)^.*regexMatch "://\[\^/@\]\*:\[\^@\]\*@".*\r?\n.*inline userinfo.*\r?\n`).ReplaceAllString(body, "")
		}},
		{"object guard removed", guard, func(body string) string {
			return strings.Replace(body, `{{- if not (kindIs "string" $value) }}`, "", 1)
		}},
		{"allowlist enforcement removed", guard, func(body string) string {
			return strings.Replace(body, `{{- if not (regexMatch $allowlist $name) }}`, "", 1)
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := mutation.edit(mutation.body)
			if mutated == mutation.body {
				t.Fatalf("mutation %q changed nothing", mutation.name)
			}
			if err := checkExtraEnvGuard(deployment, mutated); err == nil {
				t.Fatalf("checker accepted the %s template mutation", mutation.name)
			}
		})
	}

	// The include is the only thing that runs the guard.
	noInclude := strings.Replace(deployment, `{{- include "kodama.validateExtraEnv" . }}`, "", 1)
	if noInclude == deployment {
		t.Fatal("test mutation did not remove the guard include")
	}
	if err := checkExtraEnvGuard(noInclude, guard); err == nil {
		t.Fatal("checker accepted a Deployment that does not run the extraEnv guard")
	}

	unquoted := strings.Replace(deployment, `- name: {{ $name | quote }}`, `- name: {{ $name }}`, 1)
	if unquoted == deployment {
		t.Fatal("test mutation did not unquote the extraEnv name")
	}
	if err := checkExtraEnvGuard(unquoted, guard); err == nil {
		t.Fatal("checker accepted an unquoted extraEnv name")
	}
}

func TestTenantsSchemaRequiresAtLeastOne(t *testing.T) {
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	values := fqdnValues([]any{})
	values["tenants"] = []any{}
	if err := validateAgainstSchema(schema, values, "values"); err == nil {
		t.Fatal("schema accepted tenants: []")
	}
}

func TestEgressFQDNSchemaCapsWholeNameAt253Characters(t *testing.T) {
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	label := strings.Repeat("a", 63)
	// 63 + 1 + 63 + 1 + 63 + 1 + 61 = 253
	longest := strings.Join([]string{label, label, label, strings.Repeat("a", 61)}, ".")
	if len(longest) != 253 {
		t.Fatalf("test fixture length = %d, want 253", len(longest))
	}
	if err := validateAgainstSchema(schema, fqdnValues([]any{longest}), "values"); err != nil {
		t.Errorf("schema refused a 253-character DNS name: %v", err)
	}
	tooLong := strings.Join([]string{label, label, label, strings.Repeat("a", 62)}, ".")
	if len(tooLong) != 254 {
		t.Fatalf("test fixture length = %d, want 254", len(tooLong))
	}
	if err := validateAgainstSchema(schema, fqdnValues([]any{tooLong}), "values"); err == nil {
		t.Error("schema accepted a 254-character DNS name")
	}
}
