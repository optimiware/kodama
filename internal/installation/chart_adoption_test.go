package installation

// Chart tests for pod-security labels, FQDN egress, the KMS credentials Secret
// and the server environment.
//
// The chart is inspected as text, exactly like the other chart tests: no
// helm binary, template engine, YAML decoder, cluster or network access is
// used. Every check function is exercised against deliberately mutated
// templates so a regression cannot silently pass. The rendered form of the
// same invariants is checked separately in test/component/deploy with a real
// `helm template`.
//
// Invariants pinned here:
//
//   - tenant Namespaces enforce the baseline Pod Security profile and warn on
//     the restricted profile;
//   - the opt-in Cilium FQDN egress policy renders only when
//     networkPolicy.egressFQDNs is non-empty and the Cilium CRD is available,
//     carries the DNS rule in the same policy, and never names a wildcard;
//   - values.schema.json validates every egressFQDNs entry as a DNS name, so an
//     empty, wildcard, scheme, port or path entry is refused before rendering;
//   - the cloud KMS credential is referenced through the kodama-kms-credentials
//     Secret by name only (never inlined), separate from the binary KEK Secret;
//   - the Deployment sets exactly the environment names the production
//     composition reads, including KODAMA_NATS_CERT_FILE/KEY_FILE and the
//     tenant configuration: KODAMA_TENANTS for an explicit tenant list, or
//     KODAMA_TENANT for the singleton compatibility form.

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// chartDirectEnvNames is the exact set of environment variables the chart
// templates set directly on the server container. The names are taken from the
// production composition (cmd/kodama/production.go) and the server option
// overlay (cmd/kodama/server.go).
// Secrets arrive through secretKeyRef/envFrom; OIDC and discovery settings are
// opt-in through config.extraEnv and are not listed here.
var chartDirectEnvNames = []string{
	"KODAMA_AUTH_DATABASE_URL",
	"KODAMA_CONTROLLER_DATABASE_URL",
	"KODAMA_DATABASE_URL",
	"KODAMA_DATA_DIR",
	"KODAMA_GIT_STORAGE_ROOT",
	"KODAMA_INSTALLATION_ID",
	"KODAMA_KEY_ADAPTER",
	"KODAMA_KEY_BLOBS_DIR",
	"KODAMA_KEY_CURRENT_VERSION",
	"KODAMA_KEY_REF",
	"KODAMA_MCP_PATH",
	"KODAMA_NATS_CA_FILE",
	"KODAMA_NATS_CALLOUT_ACCOUNT",
	"KODAMA_NATS_CALLOUT_ISSUER_SEED",
	"KODAMA_NATS_CALLOUT_PASSWORD",
	"KODAMA_NATS_CALLOUT_USER",
	"KODAMA_NATS_CERT_FILE",
	"KODAMA_NATS_KEY_FILE",
	"KODAMA_NATS_PASSWORD",
	"KODAMA_NATS_URL",
	"KODAMA_NATS_USER",
	"KODAMA_PROFILE",
	"KODAMA_PUBLIC_URL",
	"KODAMA_PUBLISHER_DATABASE_URL",
	"KODAMA_TENANT",
	"KODAMA_TENANTS",
	"KODAMA_TLS_DIR",
	"KODAMA_WORKLOAD_CA_SECRET",
	"KODAMA_WORKSPACE_PVC_ACCESS_MODE",
	"KODAMA_WORKSPACE_PVC_SIZE",
	"KODAMA_WORKSPACE_PVC_STORAGE_CLASS",
}

// chartExtraEnvNames is the optional config.extraEnv surface. production.go
// requires the OIDC names, so a deployment without them must be refused by the
// binary (it is); the chart only provides the pass-through. Discovery settings
// feed the public login document from server.go.
var chartExtraEnvNames = []string{
	"KODAMA_DISCOVERY_API_URL",
	"KODAMA_DISCOVERY_AUDIENCE",
	"KODAMA_DISCOVERY_AUTHORIZATION_ENDPOINT",
	"KODAMA_DISCOVERY_CLIENT_ID",
	"KODAMA_DISCOVERY_DEVICE_AUTHORIZATION_ENDPOINT",
	"KODAMA_DISCOVERY_ISSUER",
	"KODAMA_DISCOVERY_REVOCATION_ENDPOINT",
	"KODAMA_DISCOVERY_SCOPES",
	"KODAMA_DISCOVERY_TOKEN_ENDPOINT",
	"KODAMA_OIDC_AUDIENCE",
	"KODAMA_OIDC_CA_FILE",
	"KODAMA_OIDC_ISSUER",
	"KODAMA_OIDC_JWKS_URL",
	"KODAMA_OIDC_PROJECT_CLAIM",
	"KODAMA_OIDC_SUBJECT_CLAIM",
	"KODAMA_OIDC_TENANT_CLAIM",
	"KODAMA_TENANT_NAMESPACE",
	"KODAMA_WORKLOAD_CPU_LIMIT",
	"KODAMA_WORKLOAD_CPU_REQUEST",
	"KODAMA_WORKLOAD_MEMORY_LIMIT",
	"KODAMA_WORKLOAD_MEMORY_REQUEST",
}

// chartRequiredProductionEnvNames are the names production.go refuses to start
// without. The chart must set each one directly or document it as an
// config.extraEnv input; a rename in either place breaks a real deployment.
var chartRequiredProductionEnvNames = []string{
	"KODAMA_AUTH_DATABASE_URL",
	"KODAMA_CONTROLLER_DATABASE_URL",
	"KODAMA_DATABASE_URL",
	"KODAMA_KEY_BLOBS_DIR",
	"KODAMA_KEY_REF",
	"KODAMA_NATS_CA_FILE",
	"KODAMA_NATS_CALLOUT_ACCOUNT",
	"KODAMA_NATS_CALLOUT_ISSUER_SEED",
	"KODAMA_NATS_CALLOUT_PASSWORD",
	"KODAMA_NATS_CALLOUT_USER",
	"KODAMA_NATS_CERT_FILE",
	"KODAMA_NATS_KEY_FILE",
	"KODAMA_NATS_PASSWORD",
	"KODAMA_NATS_URL",
	"KODAMA_NATS_USER",
	"KODAMA_PUBLISHER_DATABASE_URL",
	"KODAMA_TENANT",
	"KODAMA_TENANTS",
	"KODAMA_TLS_DIR",
	"KODAMA_WORKSPACE_PVC_ACCESS_MODE",
	"KODAMA_WORKSPACE_PVC_SIZE",
	"KODAMA_WORKSPACE_PVC_STORAGE_CLASS",
}

func containsName(values []string, want string) bool {
	return slices.Contains(values, want)
}

// indentedBlock returns the lines nested under the first line whose trimmed
// text is header, stopping at the next line with the same or lower indent.
func indentedBlock(body, header string) (string, bool) {
	lines := strings.Split(body, "\n")
	start := -1
	indent := 0
	for index, line := range lines {
		if strings.TrimSpace(line) == header {
			start = index
			indent = len(line) - len(strings.TrimLeft(line, " \t"))
			break
		}
	}
	if start < 0 {
		return "", false
	}
	var block []string
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) == "" {
			block = append(block, line)
			continue
		}
		current := len(line) - len(strings.TrimLeft(line, " \t"))
		if current <= indent {
			break
		}
		block = append(block, line)
	}
	return strings.Join(block, "\n"), true
}

// deploymentEnvBlock returns the container env: block, which ends at envFrom:.
func deploymentEnvBlock(t *testing.T, body string) string {
	t.Helper()
	block, ok := indentedBlock(body, "env:")
	if !ok {
		t.Fatalf("Deployment has no env: block")
	}
	return block
}

// deploymentEnvNames returns the direct environment names in the env: block.
func deploymentEnvNames(t *testing.T, body string) []string {
	t.Helper()
	var names []string
	for _, match := range regexp.MustCompile(`(?m)^[ \t]*-[ \t]*name:[ \t]*([A-Z][A-Z0-9_]*)[ \t]*$`).FindAllStringSubmatch(deploymentEnvBlock(t, body), -1) {
		names = append(names, match[1])
	}
	return names
}

// checkDeploymentEnvironment names every production-composition variable and
// fails on an unknown, renamed or missing name.
func checkDeploymentEnvironment(body string) error {
	names := deploymentEnvNamesFor(body)
	if len(names) == 0 {
		return fmt.Errorf("Deployment sets no environment variables")
	}
	known := make(map[string]bool, len(chartDirectEnvNames))
	for _, name := range chartDirectEnvNames {
		known[name] = true
	}
	for _, name := range names {
		if name == "KODAMA_ENVIRONMENT" {
			return fmt.Errorf("Deployment sets %s, which no Go entrypoint reads; the production profile comes from KODAMA_PROFILE and the --profile flag", name)
		}
		if !known[name] {
			return fmt.Errorf("Deployment sets %s, which is not part of the production environment contract", name)
		}
	}
	for _, name := range chartRequiredProductionEnvNames {
		if !containsName(names, name) {
			return fmt.Errorf("Deployment does not set required production variable %s", name)
		}
	}
	for _, name := range []string{"KODAMA_NATS_CERT_FILE", "KODAMA_NATS_KEY_FILE", "KODAMA_TENANT"} {
		if !containsName(names, name) {
			return fmt.Errorf("Deployment does not set %s", name)
		}
	}
	return nil
}

// deploymentEnvNamesFor is the test-independent parser used by the checker.
func deploymentEnvNamesFor(body string) []string {
	block, ok := indentedBlock(body, "env:")
	if !ok {
		return nil
	}
	var names []string
	for _, entry := range regexp.MustCompile(`(?m)^[ \t]*-[ \t]*name:[ \t]*([A-Z][A-Z0-9_]*)[ \t]*$`).FindAllStringSubmatch(block, -1) {
		names = append(names, entry[1])
	}
	return names
}

func TestDeploymentEnvironmentMatchesProductionComposition(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "deployment.yaml"))
	if err := checkDeploymentEnvironment(body); err != nil {
		t.Fatalf("deployment.yaml: %v", err)
	}

	names := deploymentEnvNames(t, body)
	got := sortedCopy(names)
	want := sortedCopy(chartDirectEnvNames)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Deployment env names = %v, want %v", got, want)
	}

	// The tenant configuration renders from the chart's tenant list: the
	// explicit list form carries every configured tenant as JSON, and the
	// singleton compatibility branch carries the only tenant as KODAMA_TENANT.
	// A render with more than one tenant must never fall back to the first
	// one.
	requirePattern(t, body, `(?ms)if[ \t]+gt[ \t]+\(len[ \t]+\.Values\.tenants\)[ \t]+1`, "deployment.yaml tenant-count branch")
	requirePattern(t, body, `(?ms)- name: KODAMA_TENANTS\s*\r?\n\s*value: \{\{ \.Values\.tenants \| toJson \| quote \}\}`, "deployment.yaml KODAMA_TENANTS from the whole tenant list")
	requirePattern(t, body, `(?ms)- name: KODAMA_TENANT\s*\r?\n\s*value: \{\{ index \.Values\.tenants 0 \| quote \}\}`, "deployment.yaml singleton KODAMA_TENANT from the only tenant")
}

// TestEnvironmentContractIsComplete keeps the three tables above coherent:
// the direct and extraEnv surfaces are disjoint, and together they cover every
// variable production.go requires. A name moved between the lists without a
// template or comment update fails here before a deployment does.
func TestEnvironmentContractIsComplete(t *testing.T) {
	for _, direct := range chartDirectEnvNames {
		if containsName(chartExtraEnvNames, direct) {
			t.Errorf("%s is both a chart-direct and a config.extraEnv variable", direct)
		}
	}
	for _, required := range chartRequiredProductionEnvNames {
		if !containsName(chartDirectEnvNames, required) && !containsName(chartExtraEnvNames, required) {
			t.Errorf("required production variable %s is neither set by the chart nor documented for config.extraEnv", required)
		}
	}
	for _, name := range []string{"KODAMA_OIDC_ISSUER", "KODAMA_OIDC_AUDIENCE", "KODAMA_OIDC_JWKS_URL", "KODAMA_OIDC_TENANT_CLAIM"} {
		if !containsName(chartExtraEnvNames, name) {
			t.Errorf("production-required %s is not documented as a config.extraEnv input", name)
		}
	}
	if containsName(chartDirectEnvNames, "KODAMA_ENVIRONMENT") {
		t.Error("KODAMA_ENVIRONMENT is listed as part of the production contract but no Go entrypoint reads it")
	}
}

func TestDeploymentEnvironmentRejectsRenamedVariable(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "deployment.yaml"))
	mutated := strings.Replace(body, "- name: KODAMA_NATS_CERT_FILE", "- name: KODAMA_NATS_CLIENT_CERT_FILE", 1)
	if mutated == body {
		t.Fatal("test mutation did not rename KODAMA_NATS_CERT_FILE")
	}
	if err := checkDeploymentEnvironment(mutated); err == nil {
		t.Fatal("checker accepted a renamed NATS client certificate variable")
	}

	removed := strings.Replace(body, "            - name: KODAMA_TENANTS\n              value: {{ .Values.tenants | toJson | quote }}\n", "", 1)
	if removed == body {
		t.Fatal("test mutation did not remove KODAMA_TENANTS")
	}
	if err := checkDeploymentEnvironment(removed); err == nil {
		t.Fatal("checker accepted a Deployment without the multi-tenant KODAMA_TENANTS")
	}

	removed = strings.Replace(body, "            - name: KODAMA_TENANT\n              value: {{ index .Values.tenants 0 | quote }}\n", "", 1)
	if removed == body {
		t.Fatal("test mutation did not remove KODAMA_TENANT")
	}
	if err := checkDeploymentEnvironment(removed); err == nil {
		t.Fatal("checker accepted a Deployment without the singleton KODAMA_TENANT")
	}
}

// checkTenantNamespacePodSecurity requires the tenant Namespace document (not
// the LimitRange) to carry the pod-security labels: enforce baseline, and
// warn/audit restricted so a workload that would break under restricted is
// visible long before enforcement moves.
func checkTenantNamespacePodSecurity(body string) error {
	var namespace string
	for document := range strings.SplitSeq(body, "\n---") {
		if regexp.MustCompile(`(?m)^[ \t]*kind:[ \t]*Namespace[ \t]*$`).MatchString(document) {
			namespace = document
			break
		}
	}
	if namespace == "" {
		return fmt.Errorf("no Namespace document")
	}
	want := []struct {
		label string
		level string
	}{
		{label: "pod-security.kubernetes.io/enforce", level: "baseline"},
		{label: "pod-security.kubernetes.io/warn", level: "restricted"},
		{label: "pod-security.kubernetes.io/audit", level: "restricted"},
	}
	for _, entry := range want {
		expression := `(?m)^[ \t]*` + regexp.QuoteMeta(entry.label) + `:[ \t]*"?` + entry.level + `"?[ \t]*$`
		if !regexp.MustCompile(expression).MatchString(namespace) {
			return fmt.Errorf("tenant Namespace does not set %s: %s", entry.label, entry.level)
		}
	}
	// Reject any other level on the same labels (for example privileged).
	level := regexp.MustCompile(`(?m)^[ \t]*(pod-security\.kubernetes\.io/(?:enforce|warn|audit)):[ \t]*"?([^"\r\n ]+)"?[ \t]*$`)
	for _, match := range level.FindAllStringSubmatch(namespace, -1) {
		switch match[1] {
		case "pod-security.kubernetes.io/enforce":
			if match[2] != "baseline" {
				return fmt.Errorf("tenant Namespace enforces pod security level %q, want baseline", match[2])
			}
		default:
			if match[2] != "restricted" {
				return fmt.Errorf("tenant Namespace sets %s to %q, want restricted", match[1], match[2])
			}
		}
	}
	return nil
}

func TestTenantNamespacesEnforceBaselinePodSecurity(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "namespaces.yaml"))
	if err := checkTenantNamespacePodSecurity(body); err != nil {
		t.Fatalf("namespaces.yaml: %v", err)
	}
}

func TestTenantNamespacePodSecurityMutationSensitivity(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "namespaces.yaml"))

	dropped := regexp.MustCompile(`(?m)^[ \t]*pod-security\.kubernetes\.io/enforce:.*\r?\n`).ReplaceAllString(body, "")
	if dropped == body {
		t.Fatal("test mutation did not remove the pod-security enforce label")
	}
	if err := checkTenantNamespacePodSecurity(dropped); err == nil {
		t.Fatal("checker accepted a tenant Namespace without the pod-security enforce label")
	}

	privileged := strings.Replace(body, "pod-security.kubernetes.io/enforce: baseline", "pod-security.kubernetes.io/enforce: privileged", 1)
	if privileged == body {
		t.Fatal("test mutation did not set enforce to privileged")
	}
	if err := checkTenantNamespacePodSecurity(privileged); err == nil {
		t.Fatal("checker accepted pod-security enforce=privileged")
	}
}

// checkCiliumFQDNPolicyTemplate verifies the opt-in FQDN policy: the
// egressFQDNs gate keeps an empty list from rendering an allow-all, the CRD
// capability gate keeps it off clusters that cannot enforce FQDN rules, and the
// DNS rule required by Cilium lives in the same policy as the toFQDNs rule.
func checkCiliumFQDNPolicyTemplate(body string) error {
	if !regexp.MustCompile(`(?m)\{\{-?\s*if\s+and\s+\.Values\.networkPolicy\.egressFQDNs\b`).MatchString(body) {
		return fmt.Errorf("policy is not gated by a non-empty .Values.networkPolicy.egressFQDNs")
	}
	if !strings.Contains(body, `.Capabilities.APIVersions.Has "cilium.io/v2/CiliumNetworkPolicy"`) {
		return fmt.Errorf("policy is not gated by the Cilium CRD capability")
	}
	if got := strings.Count(body, "kind: CiliumNetworkPolicy"); got != 1 {
		return fmt.Errorf("template renders %d CiliumNetworkPolicy objects per tenant, want 1", got)
	}
	if !regexp.MustCompile(`(?m)^[ \t]*apiVersion:[ \t]*cilium\.io/v2[ \t]*$`).MatchString(body) {
		return fmt.Errorf("policy does not use cilium.io/v2")
	}
	if !regexp.MustCompile(`(?m)^[ \t]*namespace:[ \t]*kodama-tenant-\{\{[ \t]*\$tenant[ \t]*\}\}[ \t]*$`).MatchString(body) {
		return fmt.Errorf("policy is not rendered into kodama-tenant-{{ $tenant }}")
	}
	if !regexp.MustCompile(`(?m)^[ \t]*endpointSelector:[ \t]*\{[ \t]*\}[ \t]*$`).MatchString(body) {
		return fmt.Errorf("policy does not select every tenant endpoint")
	}
	if !regexp.MustCompile(`(?ms)toEndpoints:[ \t]*\r?\n[ \t]*-[ \t]*matchLabels:[ \t]*\r?\n[ \t]*io\.kubernetes\.pod\.namespace:[ \t]*kube-system[ \t]*\r?\n[ \t]*k8s-app:[ \t]*kube-dns`).MatchString(body) {
		return fmt.Errorf("DNS rule is not scoped to kube-system/kube-dns")
	}
	if !regexp.MustCompile(`\{\{-?[ \t]*range[ \t]+\$tenant[ \t]*:=[ \t]*\.Values\.tenants`).MatchString(body) {
		return fmt.Errorf("policy does not range over .Values.tenants")
	}
	if got := strings.Count(body, "range $.Values.networkPolicy.egressFQDNs"); got != 2 {
		return fmt.Errorf("policy ranges over egressFQDNs %d times, want the DNS rule and the toFQDNs rule", got)
	}
	if !regexp.MustCompile(`(?m)^[ \t]*dns:[ \t]*$`).MatchString(body) {
		return fmt.Errorf("policy has no DNS rule; Cilium cannot learn FQDN addresses without it in the same policy")
	}
	if !regexp.MustCompile(`(?m)^[ \t]*-[ \t]*toFQDNs:[ \t]*$`).MatchString(body) {
		return fmt.Errorf("policy has no toFQDNs rule")
	}
	if !regexp.MustCompile(`(?m)^[ \t]*-[ \t]*matchName:[ \t]*\{\{[ \t]*\.[ \t]*\|[ \t]*quote[ \t]*\}\}[ \t]*$`).MatchString(body) {
		return fmt.Errorf("policy does not name each egressFQDN with matchName")
	}
	if strings.Contains(body, "matchPattern") {
		return fmt.Errorf("policy uses matchPattern; exact matchName entries are required so a wildcard cannot widen the allow list")
	}
	if !regexp.MustCompile(`(?ms)toPorts:[ \t]*\r?\n[ \t]*-[ \t]*ports:[ \t]*\r?\n[ \t]*-[ \t]*port:[ \t]*"443"[ \t]*\r?\n[ \t]*protocol:[ \t]*TCP`).MatchString(body) {
		return fmt.Errorf("policy does not restrict the FQDN egress to TCP 443")
	}
	if !regexp.MustCompile(`(?ms)toPorts:[ \t]*\r?\n[ \t]*-[ \t]*ports:[ \t]*\r?\n[ \t]*-[ \t]*port:[ \t]*"53"[ \t]*\r?\n[ \t]*protocol:[ \t]*ANY`).MatchString(body) {
		return fmt.Errorf("policy does not scope the DNS rule to port 53")
	}
	return nil
}

func TestCiliumFQDNPolicyTemplate(t *testing.T) {
	bodies := templateBodies(t)
	body, ok := bodies["ciliumpolicy.yaml"]
	if !ok {
		t.Fatal("missing templates/ciliumpolicy.yaml")
	}
	if err := checkCiliumFQDNPolicyTemplate(body); err != nil {
		t.Fatalf("ciliumpolicy.yaml: %v", err)
	}

	// An empty default is what keeps the opt-in honest: the chart's own values
	// must not turn the policy on for every installation.
	values := readText(t, chartPath(t, "values.yaml"))
	requirePattern(t, values, `(?m)^[ \t]*egressFQDNs:[ \t]*\[\][ \t]*$`, "values.yaml egressFQDNs defaults to empty")
}

func TestCiliumFQDNPolicyMutationSensitivity(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "ciliumpolicy.yaml"))

	noDNS := regexp.MustCompile(`(?ms)^[ \t]*rules:\s*\r?\n[ \t]*dns:\s*\r?\n(?:[ \t]+.*\r?\n?)*`).ReplaceAllString(body, "")
	if noDNS == body {
		t.Fatal("test mutation did not remove the DNS rule")
	}
	if err := checkCiliumFQDNPolicyTemplate(noDNS); err == nil {
		t.Fatal("checker accepted an FQDN policy without the DNS rule")
	}

	noGate := strings.Replace(body, "{{- if and .Values.networkPolicy.egressFQDNs (.Capabilities.APIVersions.Has \"cilium.io/v2/CiliumNetworkPolicy\") }}", "{{- if .Capabilities.APIVersions.Has \"cilium.io/v2/CiliumNetworkPolicy\" }}", 1)
	if noGate == body {
		t.Fatal("test mutation did not remove the egressFQDNs gate")
	}
	if err := checkCiliumFQDNPolicyTemplate(noGate); err == nil {
		t.Fatal("checker accepted a policy that renders with an empty FQDN list")
	}

	wildcard := strings.Replace(body, "matchName: {{ . | quote }}", `matchPattern: "*"`, 1)
	if wildcard == body {
		t.Fatal("test mutation did not replace matchName with a wildcard")
	}
	if err := checkCiliumFQDNPolicyTemplate(wildcard); err == nil {
		t.Fatal("checker accepted a wildcard DNS match")
	}
}

// fqdnValues builds a schema-valid values document whose only variable is the
// egressFQDNs list. Keeping it in Go avoids a fixture per case.
func fqdnValues(entries []any) map[string]any {
	return map[string]any{
		"environment":  "production",
		"image":        map[string]any{"repository": "registry.example.com/kodama", "tag": "0.1.0"},
		"replicaCount": float64(1),
		"config": map[string]any{
			"extraEnv": map[string]any{
				"KODAMA_OIDC_ISSUER":       "https://issuer.example",
				"KODAMA_OIDC_AUDIENCE":     "kodama",
				"KODAMA_OIDC_JWKS_URL":     "https://issuer.example/jwks",
				"KODAMA_OIDC_TENANT_CLAIM": "org_id",
			},
		},
		"secrets": map[string]any{
			"database":       map[string]any{"existingSecret": "kodama-database"},
			"kmsCredentials": map[string]any{"existingSecret": "kodama-kms-credentials"},
		},
		"tenants": []any{"tenant-a"},
		"networkPolicy": map[string]any{
			"defaultDeny":  true,
			"permitKodama": true,
			"permitGit":    true,
			"egressFQDNs":  entries,
		},
		"securityContext": map[string]any{},
		"resources":       map[string]any{},
	}
}

func TestNetworkPolicyFQDNSSchemaGrammar(t *testing.T) {
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	properties := objectAt(t, schema["properties"], "schema.properties")
	policy := objectAt(t, properties["networkPolicy"], "schema.properties.networkPolicy")
	policyProperties := objectAt(t, policy["properties"], "networkPolicy.properties")
	fqdns := objectAt(t, policyProperties["egressFQDNs"], "networkPolicy.properties.egressFQDNs")
	if fqdns["type"] != "array" {
		t.Fatalf("networkPolicy.egressFQDNs type = %v, want array", fqdns["type"])
	}
	items := objectAt(t, fqdns["items"], "networkPolicy.egressFQDNs.items")
	if items["type"] != "string" {
		t.Fatalf("networkPolicy.egressFQDNs.items type = %v, want string", items["type"])
	}
	if _, ok := items["pattern"].(string); !ok {
		t.Fatalf("networkPolicy.egressFQDNs.items has no pattern: %v", items["pattern"])
	}

	valid := []string{
		"registry.npmjs.org",
		"a.b",
		"localhost",
		"x1.y-2.example",
		"api.github.com",
	}
	for _, name := range valid {
		if err := validateAgainstSchema(schema, fqdnValues([]any{name}), "values"); err != nil {
			t.Errorf("schema refused DNS name %q: %v", name, err)
		}
	}

	invalid := []string{
		"*",                    // bare wildcard
		"*.example.com",        // wildcard label
		"",                     // empty entry
		"https://example.com",  // scheme
		"example.com:443",      // port
		"example.com/path",     // path
		".example.com",         // leading dot
		"example.com.",         // trailing dot
		"a..b",                 // empty label
		"-a.example.com",       // leading hyphen
		"a-.example.com",       // trailing hyphen
		"Example.COM",          // uppercase
		"exa mple.com",         // space
		"registry.npmjs.org\n", // trailing newline
	}
	for _, name := range invalid {
		if err := validateAgainstSchema(schema, fqdnValues([]any{name}), "values"); err == nil {
			t.Errorf("schema accepted egressFQDN %q", name)
		}
	}

	// The list itself must be an array of strings, and an empty list is a valid
	// opt-out (the template, not the schema, decides not to render).
	if err := validateAgainstSchema(schema, fqdnValues([]any{}), "values"); err != nil {
		t.Errorf("schema refused an empty egressFQDNs list: %v", err)
	}
	scalar := fqdnValues(nil)
	scalar["networkPolicy"].(map[string]any)["egressFQDNs"] = "registry.npmjs.org"
	if err := validateAgainstSchema(schema, scalar, "values"); err == nil {
		t.Error("schema accepted egressFQDNs as a scalar string")
	}

	// An unknown networkPolicy key must still be refused.
	unknown := fqdnValues([]any{})
	unknown["networkPolicy"].(map[string]any)["egressFqdn"] = []any{"registry.npmjs.org"}
	if err := validateAgainstSchema(schema, unknown, "values"); err == nil {
		t.Error("schema accepted an unknown networkPolicy key; additionalProperties must stay false")
	}
}

func TestNetworkPolicyFQDNSSchemaRejectsEmptyEntryRenderingAll(t *testing.T) {
	// An empty-string entry is the classic way to render an empty/allow-all
	// rule; the pattern must reject it before helm ever sees it.
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	err := validateAgainstSchema(schema, fqdnValues([]any{""}), "values")
	if err == nil {
		t.Fatal("schema accepted an empty egressFQDN")
	}
	if !strings.Contains(err.Error(), "networkPolicy.egressFQDNs") {
		t.Fatalf("schema rejected the empty egressFQDN for the wrong reason: %v", err)
	}
}

// checkKMSCredentialsCustody keeps the cloud credential in its own Secret and
// the binary KEK blob in its own volume: the credential reaches the process
// through envFrom, by name only, and nothing in the chart carries a value.
func checkKMSCredentialsCustody(values, deployment string) error {
	if line := firstSecretLiteral(values); line != "" {
		return fmt.Errorf("values.yaml carries a literal secret value: %s", line)
	}
	block, ok := indentedBlock(values, "kmsCredentials:")
	if !ok {
		return fmt.Errorf("values.yaml does not declare secrets.kmsCredentials")
	}
	name := ""
	for line := range strings.SplitSeq(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, found := strings.Cut(trimmed, ":")
		if !found {
			return fmt.Errorf("secrets.kmsCredentials has an unparseable line %q", trimmed)
		}
		if strings.TrimSpace(key) != "existingSecret" {
			return fmt.Errorf("secrets.kmsCredentials carries %q; only existingSecret (a name) is allowed", trimmed)
		}
		name = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	if name != "kodama-kms-credentials" {
		return fmt.Errorf("secrets.kmsCredentials.existingSecret = %q, want kodama-kms-credentials", name)
	}

	envFrom, ok := indentedBlock(deployment, "envFrom:")
	if !ok {
		return fmt.Errorf("Deployment has no envFrom block")
	}
	if got := strings.Count(envFrom, "secretRef:"); got != 1 {
		return fmt.Errorf("envFrom has %d secret references, want only the KMS credentials Secret", got)
	}
	if !regexp.MustCompile(`(?m)^[ \t]*name:[ \t]*\{\{[ \t]*\.Values\.secrets\.kmsCredentials\.existingSecret[ \t]*\}\}[ \t]*$`).MatchString(envFrom) {
		return fmt.Errorf("envFrom does not reference .Values.secrets.kmsCredentials.existingSecret")
	}
	if strings.Contains(envFrom, ".Values.secrets.keys.existingSecret") {
		return fmt.Errorf("envFrom loads the binary KEK Secret as environment variables")
	}
	if !regexp.MustCompile(`(?ms)if ne \.Values\.environment "production".*?optional:[ \t]*true`).MatchString(deployment) {
		return fmt.Errorf("KMS credentials secretRef is not optional outside production, or the production requirement is missing")
	}
	inline := regexp.MustCompile(`(?ms)^[ \t]*-[ \t]*name:[ \t]*(?:SCW_[A-Z0-9_]+|AWS_[A-Z0-9_]+|GOOGLE_[A-Z0-9_]+)[ \t]*\r?\n[ \t]*value:`)
	if inline.MatchString(deployment) {
		return fmt.Errorf("Deployment inlines a cloud credential as an environment value")
	}
	// The KEK blob Secret must still be mounted as a file for KODAMA_KEY_BLOBS_DIR.
	if !regexp.MustCompile(`(?ms)^[ \t]*-[ \t]*name:[ \t]*keys[ \t]*\r?\n[ \t]*secret:[ \t]*\r?\n[ \t]*secretName:[ \t]*\{\{[ \t]*\.Values\.secrets\.keys\.existingSecret[ \t]*\}\}`).MatchString(deployment) {
		return fmt.Errorf("Deployment does not mount the keys Secret as the KEK blob volume")
	}
	return nil
}

func TestKMSCredentialsSecretReferencedByNameOnly(t *testing.T) {
	values := readText(t, chartPath(t, "values.yaml"))
	deployment := readText(t, chartPath(t, "templates", "deployment.yaml"))
	if err := checkKMSCredentialsCustody(values, deployment); err != nil {
		t.Fatalf("KMS credential custody: %v", err)
	}

	// No template may create a Secret or carry string data.
	for name, body := range templateBodies(t) {
		if strings.Contains(body, "kind: Secret") || strings.Contains(body, "stringData:") {
			t.Errorf("%s defines or inlines a Secret; secrets are referenced by name", name)
		}
	}
}

func TestKMSCredentialsMutationSensitivity(t *testing.T) {
	values := readText(t, chartPath(t, "values.yaml"))
	deployment := readText(t, chartPath(t, "templates", "deployment.yaml"))

	inlined := strings.Replace(values, "  kmsCredentials:\n    existingSecret: kodama-kms-credentials\n", "  kmsCredentials:\n    existingSecret: kodama-kms-credentials\n    value: hunter2\n", 1)
	if inlined == values {
		t.Fatal("test mutation did not inline a KMS credential value")
	}
	if err := checkKMSCredentialsCustody(inlined, deployment); err == nil {
		t.Fatal("checker accepted an inlined KMS credential value")
	}

	inlineEnv := strings.Replace(deployment, "          env:\n", "          env:\n            - name: SCW_SECRET_KEY\n              value: hunter2\n", 1)
	if inlineEnv == deployment {
		t.Fatal("test mutation did not inline SCW_SECRET_KEY as an env value")
	}
	if err := checkKMSCredentialsCustody(values, inlineEnv); err == nil {
		t.Fatal("checker accepted an inlined SCW_SECRET_KEY")
	}

	keysEnvFrom := strings.Replace(deployment, "{{ .Values.secrets.kmsCredentials.existingSecret }}", "{{ .Values.secrets.keys.existingSecret }}", 1)
	if keysEnvFrom == deployment {
		t.Fatal("test mutation did not point envFrom at the KEK Secret")
	}
	if err := checkKMSCredentialsCustody(values, keysEnvFrom); err == nil {
		t.Fatal("checker accepted the binary KEK Secret as an envFrom source")
	}
}

// TestTenantListSchemaGrammar pins the values contract of the tenant list the
// Deployment renders as KODAMA_TENANTS. Every entry derives a namespace, so a
// non-DNS-label id, an empty id or a wrong type is refused before rendering; a
// multi-tenant installation additionally refuses the singleton namespace
// override, which cannot describe more than one namespace.
func TestTenantListSchemaGrammar(t *testing.T) {
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	properties := objectAt(t, schema["properties"], "schema.properties")
	tenants := objectAt(t, properties["tenants"], "schema.properties.tenants")
	if tenants["type"] != "array" {
		t.Fatalf("tenants type = %v, want array", tenants["type"])
	}
	if minimum, ok := tenants["minItems"].(float64); !ok || minimum != 1 {
		t.Fatalf("tenants minItems = %v, want 1", tenants["minItems"])
	}
	items := objectAt(t, tenants["items"], "tenants.items")
	if items["type"] != "string" {
		t.Fatalf("tenants.items type = %v, want string", items["type"])
	}
	if _, ok := items["pattern"].(string); !ok {
		t.Fatalf("tenants.items has no pattern: %v", items["pattern"])
	}

	tenantValues := func(list []any) map[string]any {
		values := fqdnValues([]any{})
		values["tenants"] = list
		return values
	}

	for _, valid := range [][]any{
		{"tenant-a"},
		{"tenant-a", "tenant-b"},
		{"t1", "team-2", "a"},
	} {
		if err := validateAgainstSchema(schema, tenantValues(valid), "values"); err != nil {
			t.Errorf("schema refused tenant list %v: %v", valid, err)
		}
	}

	for _, invalid := range [][]any{
		{},
		{""},
		{"Tenant-A"},
		{"tenant_a"},
		{"tenant a"},
		{"-tenant"},
		{"tenant-"},
		{"tenant."},
		{"tenant-a", 1},
		{"tenant-a", map[string]any{"id": "tenant-b"}},
	} {
		if err := validateAgainstSchema(schema, tenantValues(invalid), "values"); err == nil {
			t.Errorf("schema accepted tenant list %v", invalid)
		}
	}

	// A tenant id so long that kodama-tenant-<id> exceeds the 63-character
	// Kubernetes namespace limit is refused.
	tooLong := strings.Repeat("a", 50)
	if err := validateAgainstSchema(schema, tenantValues([]any{tooLong}), "values"); err == nil {
		t.Errorf("schema accepted a %d-character tenant id", len(tooLong))
	}

	// A multi-tenant installation derives every namespace; the singleton
	// KODAMA_TENANT_NAMESPACE override is ambiguous and refused.
	multi := fqdnValues([]any{})
	multi["tenants"] = []any{"tenant-a", "tenant-b"}
	multi["config"].(map[string]any)["extraEnv"].(map[string]any)["KODAMA_TENANT_NAMESPACE"] = "kodama-tenant-explicit"
	if err := validateAgainstSchema(schema, multi, "values"); err == nil {
		t.Error("schema accepted KODAMA_TENANT_NAMESPACE with more than one tenant")
	}

	single := fqdnValues([]any{})
	single["config"].(map[string]any)["extraEnv"].(map[string]any)["KODAMA_TENANT_NAMESPACE"] = "kodama-tenant-explicit"
	if err := validateAgainstSchema(schema, single, "values"); err != nil {
		t.Errorf("schema refused the singleton namespace override: %v", err)
	}
}

// checkTenantListGuard verifies the template layer of the tenant-list rules.
// The schema states the id grammar; this guard is the line of defence for a
// render that skips schema validation and for the duplicate check the schema
// cannot express in the bounded keyword subset. It is kept in the text tests so
// removing a rule is caught without a helm binary.
func checkTenantListGuard(guard string) error {
	requirements := []struct {
		expression  string
		description string
	}{
		{`\$tenantNames[ \t]*:=[ \t]*dict`, "duplicate tracking map"},
		{`regexMatch[ \t]+"\^\[a-z0-9\]\(\[a-z0-9-\]\{0,47\}\[a-z0-9\]\)\?\$"[ \t]+\$name`, "lowercase DNS-label tenant id guard"},
		{`hasKey[ \t]+\$tenantNames[ \t]+\$name`, "duplicate tenant lookup"},
		{`tenants repeats`, "duplicate tenant refusal"},
		{`set[ \t]+\$tenantNames[ \t]+\$name[ \t]+true`, "duplicate tracking insert"},
		{`gt[ \t]+\(len[ \t]+\.Values\.tenants\)[ \t]+1`, "multi-tenant branch"},
		{`hasKey[ \t]+\.Values\.config\.extraEnv[ \t]+"KODAMA_TENANT_NAMESPACE"`, "singleton namespace override refusal"},
	}
	for _, requirement := range requirements {
		if !regexp.MustCompile(requirement.expression).MatchString(guard) {
			return fmt.Errorf("_extraenv.tpl has no %s", requirement.description)
		}
	}
	return nil
}

// TestTenantListGuardRejectsDuplicatesAndOverrides pins the tenant-list
// template guard and proves every rule is load-bearing: removing one makes the
// checker fail. The rendered behavior of the same guard is checked with a real
// helm render in test/component/deploy.
func TestTenantListGuardRejectsDuplicatesAndOverrides(t *testing.T) {
	guard := readText(t, chartPath(t, "templates", "_extraenv.tpl"))
	if err := checkTenantListGuard(guard); err != nil {
		t.Fatalf("_extraenv.tpl: %v", err)
	}
	mutations := []struct {
		name   string
		remove string
	}{
		{
			name: "duplicate lookup",
			remove: `{{- if hasKey $tenantNames $name }}
{{- fail (printf "tenants repeats %q; one tenant id would own two namespaces and RBAC roles" $name) }}
{{- end }}
`,
		},
		{
			name: "duplicate tracking insert",
			remove: `{{- $_ := set $tenantNames $name true }}
`,
		},
		{
			name: "tenant id grammar",
			remove: `{{- if not (regexMatch "^[a-z0-9]([a-z0-9-]{0,47}[a-z0-9])?$" $name) }}
{{- fail (printf "tenants entry %q is not a lowercase DNS label; the chart derives the namespace kodama-tenant-<tenant> from it" $name) }}
{{- end }}
`,
		},
		{
			name: "multi-tenant namespace override",
			remove: `{{- if and (gt (len .Values.tenants) 1) .Values.config .Values.config.extraEnv (hasKey .Values.config.extraEnv "KODAMA_TENANT_NAMESPACE") }}
{{- fail "config.extraEnv.KODAMA_TENANT_NAMESPACE is a singleton override; a multi-tenant installation derives one namespace per tenant" }}
{{- end }}
`,
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := strings.Replace(guard, mutation.remove, "", 1)
			if mutated == guard {
				t.Fatalf("test mutation did not remove the %s guard", mutation.name)
			}
			if err := checkTenantListGuard(mutated); err == nil {
				t.Fatalf("guard checker accepted a template without the %s rule", mutation.name)
			}
		})
	}
}
