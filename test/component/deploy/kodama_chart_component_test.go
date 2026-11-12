//go:build component

// Rendered-chart checks for the Kodama control-plane chart: pod-security
// labels, the opt-in Cilium FQDN egress policy, the KMS credentials Secret and
// the production server environment.
//
// The installation package inspects the chart as text, so a template edit that
// puts the pod-security labels on the wrong object, drops the DNS rule from the
// FQDN policy, renders an allow-all policy for an empty list or inlines a
// credential could keep those checks green. These tests render the real chart
// with helm, in a test-owned temporary copy, and assert the resulting objects.
// Both the standalone chart and the stack chart (which embeds it as a file
// dependency) are exercised. helm runs locally; nothing contacts a cluster or
// the network.
package deploy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// stagedCharts copies deploy/charts into a test-owned directory. The Kodama
// chart needs no dependency build; the stack chart is staged by
// stageStackChart, which also builds the file dependency in the same copy.
func stagedCharts(t *testing.T) (string, string) {
	t.Helper()
	helm := helmBinary(t)
	destination := filepath.Join(t.TempDir(), "charts")
	copyChartTree(t, filepath.Join(repoRoot(t), "deploy", "charts"), destination)
	return helm, destination
}

// renderChartArgs runs `helm template` with arbitrary extra arguments.
func renderChartArgs(t *testing.T, helm, release, chart string, extra ...string) (string, error) {
	t.Helper()
	args := []string{"template", release, chart, "--namespace", "kodama-system"}
	args = append(args, extra...)
	command := exec.Command(helm, args...)
	command.Dir = chart
	output, err := command.CombinedOutput()
	return string(output), err
}

// helmLint runs `helm lint` on one staged chart with optional values flags.
func helmLint(t *testing.T, helm, chart string, extra ...string) {
	t.Helper()
	args := append([]string{"lint", chart}, extra...)
	command := exec.Command(helm, args...)
	command.Dir = chart
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helm lint %s: %v\n%s", filepath.Base(chart), err, output)
	}
}

// renderedDocuments splits helm output into documents.
func renderedDocuments(rendered string) []string {
	return strings.Split(rendered, "\n---")
}

// renderedDocument returns the first document with the given kind and name.
func renderedDocument(t *testing.T, rendered, kind, name string) string {
	t.Helper()
	kindPattern := regexp.MustCompile(`(?m)^kind:[ \t]*` + regexp.QuoteMeta(kind) + `[ \t]*$`)
	namePattern := regexp.MustCompile(`(?m)^[ \t]*name:[ \t]*` + regexp.QuoteMeta(name) + `[ \t]*$`)
	for _, document := range renderedDocuments(rendered) {
		if kindPattern.MatchString(document) && namePattern.MatchString(document) {
			return document
		}
	}
	t.Fatalf("rendered output has no %s named %s:\n%s", kind, name, rendered)
	return ""
}

// renderedKindCount counts documents of one kind.
func renderedKindCount(rendered, kind string) int {
	kindPattern := regexp.MustCompile(`(?m)^kind:[ \t]*` + regexp.QuoteMeta(kind) + `[ \t]*$`)
	count := 0
	for _, document := range renderedDocuments(rendered) {
		if kindPattern.MatchString(document) {
			count++
		}
	}
	return count
}

// renderedEnvNames returns the direct environment names of the first
// container of the rendered Deployment.
func renderedEnvNames(t *testing.T, deployment string) map[string]bool {
	t.Helper()
	block, ok := indentedBlockRendered(deployment, "env:")
	if !ok {
		t.Fatalf("rendered Deployment has no env: block")
	}
	names := make(map[string]bool)
	for _, match := range regexp.MustCompile(`(?m)^[ \t]*-[ \t]*name:[ \t]*"?([A-Z][A-Z0-9_]*)"?[ \t]*$`).FindAllStringSubmatch(block, -1) {
		names[match[1]] = true
	}
	return names
}

// indentedBlockRendered mirrors the unit-test helper for rendered YAML.
func indentedBlockRendered(body, header string) (string, bool) {
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

// assertRenderedPodSecurity requires every rendered tenant Namespace to carry
// the baseline/restricted labels on the Namespace object itself.
func assertRenderedPodSecurity(t *testing.T, rendered string) {
	t.Helper()
	namespaces := 0
	for _, document := range renderedDocuments(rendered) {
		if !regexp.MustCompile(`(?m)^kind:[ \t]*Namespace[ \t]*$`).MatchString(document) {
			continue
		}
		namespaces++
		for label, level := range map[string]string{
			"pod-security.kubernetes.io/enforce": "baseline",
			"pod-security.kubernetes.io/warn":    "restricted",
			"pod-security.kubernetes.io/audit":   "restricted",
		} {
			expression := `(?m)^[ \t]*` + regexp.QuoteMeta(label) + `:[ \t]*` + level + `[ \t]*$`
			if !regexp.MustCompile(expression).MatchString(document) {
				t.Errorf("rendered tenant Namespace does not set %s: %s", label, level)
			}
		}
	}
	if namespaces == 0 {
		t.Fatal("rendered output has no tenant Namespace")
	}
}

// assertNoRenderedJob keeps the non-replay rule in the rendered output.
func assertNoRenderedJob(t *testing.T, rendered string) {
	t.Helper()
	for _, kind := range []string{"Job", "CronJob"} {
		if count := renderedKindCount(rendered, kind); count != 0 {
			t.Errorf("rendered output has %d %s objects; the chart ships no Job by policy", count, kind)
		}
	}
}

// assertKMSCredentialsEnvFrom proves the rendered Deployment reads the static
// cloud credential from its own Secret by name. That is the production rule
// for the scw-kms adapter; aws-kms and gcp-kms render no envFrom.
func assertKMSCredentialsEnvFrom(t *testing.T, deployment string) {
	t.Helper()
	block, ok := indentedBlockRendered(deployment, "envFrom:")
	if !ok {
		t.Fatal("rendered Deployment has no envFrom block")
	}
	if !regexp.MustCompile(`(?m)^[ \t]*name:[ \t]*kodama-kms-credentials[ \t]*$`).MatchString(block) {
		t.Errorf("rendered Deployment envFrom does not reference the kodama-kms-credentials Secret:\n%s", block)
	}
	if regexp.MustCompile(`(?m)^[ \t]*optional:[ \t]*true[ \t]*$`).MatchString(block) {
		t.Errorf("rendered production KMS credentials secretRef is optional; the scw-kms production composition requires SCW_SECRET_KEY:\n%s", block)
	}
	if strings.Contains(block, "kodama-keys") {
		t.Errorf("rendered Deployment loads the KEK Secret as environment variables:\n%s", block)
	}
	if regexp.MustCompile(`(?ms)^[ \t]*-[ \t]*name:[ \t]*(?:SCW|AWS|GOOGLE)_[A-Z0-9_]+[ \t]*\r?\n[ \t]*value:`).MatchString(deployment) {
		t.Error("rendered Deployment inlines a cloud credential environment value")
	}
}

// assertFQDNEgressPolicy checks one rendered CiliumNetworkPolicy document: the
// DNS rule and the toFQDNs rule are in the same policy, and every configured
// host appears in both.
func assertFQDNEgressPolicy(t *testing.T, document string, hosts []string) {
	t.Helper()
	if !regexp.MustCompile(`(?m)^[ \t]*dns:[ \t]*$`).MatchString(document) {
		t.Error("rendered CiliumNetworkPolicy has no DNS rule")
	}
	if !regexp.MustCompile(`(?m)^[ \t]*-[ \t]*toFQDNs:[ \t]*$`).MatchString(document) {
		t.Error("rendered CiliumNetworkPolicy has no toFQDNs rule")
	}
	if strings.Contains(document, "matchPattern") {
		t.Error("rendered CiliumNetworkPolicy uses matchPattern; exact matchName entries are required")
	}
	for _, host := range hosts {
		if got := strings.Count(document, `matchName: "`+host+`"`); got != 2 {
			t.Errorf("rendered CiliumNetworkPolicy names %q %d times, want once in the DNS rule and once in toFQDNs", host, got)
		}
	}
	if !regexp.MustCompile(`(?ms)toPorts:[ \t]*\r?\n[ \t]*-[ \t]*ports:[ \t]*\r?\n[ \t]*-[ \t]*port:[ \t]*"443"[ \t]*\r?\n[ \t]*protocol:[ \t]*TCP`).MatchString(document) {
		t.Error("rendered CiliumNetworkPolicy does not restrict FQDN egress to TCP 443")
	}
}

func TestKodamaChartLintsAndRendersProductionWorkload(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	helmLint(t, helm, chart, kodamaProductionArgs()...)

	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs()...)
	if err != nil {
		t.Fatalf("helm template kodama: %v\n%s", err, rendered)
	}

	assertRenderedPodSecurity(t, rendered)
	assertNoRenderedJob(t, rendered)

	deployment := renderedDocument(t, rendered, "Deployment", "kodama")
	assertKMSCredentialsEnvFrom(t, deployment)
	deploymentObj := renderedObjectNamed(t, rendered, "Deployment", "kodama")
	podSecurity := objectAtKey(t, objectAtKey(t, objectAtKey(t, objectAtKey(t, deploymentObj, "spec"), "template"), "spec"), "securityContext")
	if got := stringAtKey(t, objectAtKey(t, podSecurity, "seccompProfile"), "type"); got != "RuntimeDefault" {
		t.Errorf("rendered Deployment pod seccompProfile.type = %q, want RuntimeDefault", got)
	}

	env := renderedEnvNames(t, deployment)
	for _, name := range []string{
		"KODAMA_NATS_CA_FILE",
		"KODAMA_NATS_CERT_FILE",
		"KODAMA_NATS_KEY_FILE",
		"KODAMA_TENANT",
		"KODAMA_TLS_DIR",
	} {
		if !env[name] {
			t.Errorf("rendered Deployment does not set %s", name)
		}
	}
	if env["KODAMA_ENVIRONMENT"] {
		t.Error("rendered Deployment sets KODAMA_ENVIRONMENT, which no Go entrypoint reads")
	}
	if !regexp.MustCompile(`(?ms)- name: KODAMA_TENANT\s*\r?\n\s*value: "?tenant-a"?`).MatchString(deployment) {
		t.Errorf("rendered KODAMA_TENANT is not the first configured tenant:\n%s", deployment)
	}

	// No FQDN policy by default, even when the CRD capability is available.
	rendered, err = renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs("--api-versions", "cilium.io/v2/CiliumNetworkPolicy")...)
	if err != nil {
		t.Fatalf("helm template kodama with the Cilium CRD: %v\n%s", err, rendered)
	}
	if count := renderedKindCount(rendered, "CiliumNetworkPolicy"); count != 0 {
		t.Errorf("default values render %d CiliumNetworkPolicy objects; egressFQDNs is opt-in and empty", count)
	}
}

func TestKodamaChartFQDNEgressRendersOnlyWhenEnabled(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	const ciliumCRD = "cilium.io/v2/CiliumNetworkPolicy"

	// With the CRD and a configured host: one policy per tenant, DNS rule in
	// the same document.
	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs(
		"--api-versions", ciliumCRD,
		"--set", "networkPolicy.egressFQDNs[0]=registry.npmjs.org",
		"--set", "networkPolicy.egressFQDNs[1]=api.github.com",
	)...)
	if err != nil {
		t.Fatalf("helm template kodama with egressFQDNs: %v\n%s", err, rendered)
	}
	if count := renderedKindCount(rendered, "CiliumNetworkPolicy"); count != 1 {
		t.Fatalf("rendered %d CiliumNetworkPolicy objects, want one per tenant (1)", count)
	}
	policy := renderedDocument(t, rendered, "CiliumNetworkPolicy", "kodama-tenant-egress-fqdn")
	if !regexp.MustCompile(`(?m)^[ \t]*namespace:[ \t]*kodama-tenant-tenant-a[ \t]*$`).MatchString(policy) {
		t.Error("rendered FQDN policy is not in the tenant namespace")
	}
	assertFQDNEgressPolicy(t, policy, []string{"registry.npmjs.org", "api.github.com"})

	// Without the CRD the policy is skipped, never approximated.
	rendered, err = renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs(
		"--set", "networkPolicy.egressFQDNs[0]=registry.npmjs.org",
	)...)
	if err != nil {
		t.Fatalf("helm template kodama without the Cilium CRD: %v\n%s", err, rendered)
	}
	if count := renderedKindCount(rendered, "CiliumNetworkPolicy"); count != 0 {
		t.Errorf("rendered %d CiliumNetworkPolicy objects on a cluster without the CRD", count)
	}

	// An explicitly empty list is a no-op even with the CRD.
	rendered, err = renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs(
		"--api-versions", ciliumCRD,
		"--set-json", "networkPolicy.egressFQDNs=[]",
	)...)
	if err != nil {
		t.Fatalf("helm template kodama with an empty egressFQDNs list: %v\n%s", err, rendered)
	}
	if count := renderedKindCount(rendered, "CiliumNetworkPolicy"); count != 0 {
		t.Errorf("an empty egressFQDNs list rendered %d CiliumNetworkPolicy objects; empty must not mean allow-all", count)
	}
}

func TestKodamaChartRendersExtraEnvironment(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs(
		"--set", "config.extraEnv.KODAMA_DISCOVERY_ISSUER=https://issuer.example",
	)...)
	if err != nil {
		t.Fatalf("helm template kodama with config.extraEnv: %v\n%s", err, rendered)
	}
	deployment := renderedDocument(t, rendered, "Deployment", "kodama")
	env := renderedEnvNames(t, deployment)
	for _, name := range []string{"KODAMA_OIDC_ISSUER", "KODAMA_OIDC_AUDIENCE", "KODAMA_DISCOVERY_ISSUER"} {
		if !env[name] {
			t.Errorf("config.extraEnv did not render %s", name)
		}
	}
	if !regexp.MustCompile(`(?ms)-\s*name: "?KODAMA_OIDC_ISSUER"?\s*\r?\n\s*value: "https://issuer\.example"`).MatchString(deployment) {
		t.Errorf("config.extraEnv value is not rendered as a quoted string:\n%s", deployment)
	}
}

func TestKodamaChartSchemaRejectsBadEgressFQDNs(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	for _, value := range []string{
		"*",
		"*.example.com",
		"https://example.com",
		"example.com:443",
		"example.com/path",
		"a..b",
		"a-.example.com",
	} {
		t.Run(strings.NewReplacer("*", "star", ".", "_", "/", "_", ":", "_", "-", "_").Replace(value), func(t *testing.T) {
			rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs("--set", "networkPolicy.egressFQDNs[0]="+value)...)
			if err == nil {
				t.Fatalf("helm template accepted egressFQDN %q:\n%s", value, rendered)
			}
			if !strings.Contains(rendered, "schema") {
				t.Errorf("helm template rejected %q for a reason other than values.schema.json: %v\n%s", value, err, rendered)
			}
		})
	}

	// An empty entry is the allow-all spelling; the schema must refuse it.
	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs("--set", "networkPolicy.egressFQDNs[0]=")...)
	if err == nil {
		t.Fatalf("helm template accepted an empty egressFQDN entry:\n%s", rendered)
	}
}

func TestStackChartLintsAndRendersKodamaSubchart(t *testing.T) {
	helm, stack := stageStackChart(t)
	helmLint(t, helm, stack, stackProductionArgs()...)

	rendered, err := renderChartArgs(t, helm, "kodama-stack", stack, stackProductionArgs()...)
	if err != nil {
		t.Fatalf("helm template kodama-stack: %v\n%s", err, rendered)
	}
	assertRenderedPodSecurity(t, rendered)
	assertNoRenderedJob(t, rendered)
	deployment := renderedDocument(t, rendered, "Deployment", "kodama")
	assertKMSCredentialsEnvFrom(t, deployment)
	if !renderedEnvNames(t, deployment)["KODAMA_TENANT"] {
		t.Error("stack chart does not render KODAMA_TENANT into the subchart Deployment")
	}
	if count := renderedKindCount(rendered, "CiliumNetworkPolicy"); count != 0 {
		t.Errorf("stack chart renders %d CiliumNetworkPolicy objects without an egressFQDNs opt-in", count)
	}

	rendered, err = renderChartArgs(t, helm, "kodama-stack", stack, stackProductionArgs(
		"--api-versions", "cilium.io/v2/CiliumNetworkPolicy",
		"--set", "kodama.networkPolicy.egressFQDNs[0]=registry.npmjs.org",
	)...)
	if err != nil {
		t.Fatalf("helm template kodama-stack with egressFQDNs: %v\n%s", err, rendered)
	}
	if count := renderedKindCount(rendered, "CiliumNetworkPolicy"); count != 1 {
		t.Fatalf("stack chart rendered %d CiliumNetworkPolicy objects, want one per tenant", count)
	}
	assertFQDNEgressPolicy(t, renderedDocument(t, rendered, "CiliumNetworkPolicy", "kodama-tenant-egress-fqdn"), []string{"registry.npmjs.org"})
}

// TestStackChartRejectsBadEgressFQDNs proves the subchart schema still
// validates values passed through the parent chart.
func TestStackChartRejectsBadEgressFQDNs(t *testing.T) {
	helm, stack := stageStackChart(t)
	rendered, err := renderChartArgs(t, helm, "kodama-stack", stack, stackProductionArgs("--set", "kodama.networkPolicy.egressFQDNs[0]=*.example.com")...)
	if err == nil {
		t.Fatalf("stack chart accepted a wildcard egressFQDN:\n%s", rendered)
	}
}

// TestChartsKeepDatabaseURLVerifyFull pins the two chart producers of the
// database URL to sslmode=verify-full. The production composition refuses a
// producer that emits verify-ca.
func TestChartsKeepDatabaseURLVerifyFull(t *testing.T) {
	for _, relative := range []string{
		filepath.Join("deploy", "charts", "kodama", "values.yaml"),
		filepath.Join("deploy", "charts", "kodama-stack", "values.yaml"),
	} {
		path := filepath.Join(repoRoot(t), relative)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		text := string(body)
		if got := strings.Count(text, "sslmode=verify-full"); got != 1 {
			t.Errorf("%s has %d sslmode=verify-full occurrences, want exactly 1", relative, got)
		}
		if strings.Contains(text, "sslmode=verify-ca") {
			t.Errorf("%s still emits sslmode=verify-ca, which the production composition refuses", relative)
		}
		if !regexp.MustCompile(`sslmode=verify-full&sslrootcert=/etc/kodama/pg-ca/ca\.crt`).MatchString(text) {
			t.Errorf("%s does not pair sslmode=verify-full with the mounted CA", relative)
		}
	}
}

// TestKodamaChartRendersEveryConfiguredTenant renders the real chart with two
// tenants. The Deployment must carry the whole list as KODAMA_TENANTS (never
// the first tenant as KODAMA_TENANT), and the namespace, RBAC and network
// policy set must still cover both tenants.
func TestKodamaChartRendersEveryConfiguredTenant(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs(
		"--set", "tenants[0]=tenant-a",
		"--set", "tenants[1]=tenant-b",
	)...)
	if err != nil {
		t.Fatalf("helm template kodama with two tenants: %v\n%s", err, rendered)
	}
	deployment := renderedDocument(t, rendered, "Deployment", "kodama")
	env := renderedEnvNames(t, deployment)
	if !env["KODAMA_TENANTS"] {
		t.Errorf("rendered Deployment does not set KODAMA_TENANTS for a two-tenant installation:\n%s", deployment)
	}
	if env["KODAMA_TENANT"] {
		t.Errorf("rendered two-tenant Deployment still sets the singleton KODAMA_TENANT:\n%s", deployment)
	}
	entry := renderedEnvEntry(t, renderedObjectNamed(t, rendered, "Deployment", "kodama"), "KODAMA_TENANTS")
	raw := stringAtKey(t, entry, "value")
	var tenants []string
	if err := json.Unmarshal([]byte(raw), &tenants); err != nil {
		t.Fatalf("KODAMA_TENANTS value %q is not a JSON tenant list: %v", raw, err)
	}
	if len(tenants) != 2 || tenants[0] != "tenant-a" || tenants[1] != "tenant-b" {
		t.Fatalf("KODAMA_TENANTS = %v; want both configured tenants in order", tenants)
	}
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if count := renderedKindCount(rendered, "Namespace"); count < 2 {
			t.Fatalf("rendered %d Namespace objects; want one per tenant", count)
		}
		if !regexp.MustCompile(`(?ms)kind:[ \t]*Namespace\s*\r?\nmetadata:\s*\r?\n[ \t]*name:[ \t]*"?kodama-tenant-` + tenant + `"?`).MatchString(rendered) {
			t.Errorf("rendered output has no namespace kodama-tenant-%s", tenant)
		}
		if !regexp.MustCompile(`(?ms)namespace:[ \t]*"?kodama-tenant-` + tenant + `"?`).MatchString(rendered) {
			t.Errorf("rendered output has no tenant-scoped object in namespace kodama-tenant-%s", tenant)
		}
	}
}

// TestKodamaChartRefusesAmbiguousTenantConfiguration pins the render-time
// guards that the schema cannot express or that must also hold for a render
// skipping schema validation: duplicate tenants, a non-DNS tenant id and a
// singleton namespace override next to a multi-tenant list are all refused.
func TestKodamaChartRefusesAmbiguousTenantConfiguration(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{
			name: "duplicate tenant",
			args: kodamaProductionArgs("--set", "tenants[0]=tenant-a", "--set", "tenants[1]=tenant-a"),
		},
		{
			name: "uppercase tenant",
			args: kodamaProductionArgs("--set", "tenants[0]=Tenant-A"),
		},
		{
			name: "underscore tenant",
			args: kodamaProductionArgs("--set", "tenants[0]=tenant_a"),
		},
		{
			name: "namespace override with several tenants",
			args: kodamaProductionArgs(
				"--set", "tenants[0]=tenant-a",
				"--set", "tenants[1]=tenant-b",
				"--set", "config.extraEnv.KODAMA_TENANT_NAMESPACE=kodama-tenant-explicit",
			),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rendered, err := renderChartArgs(t, helm, "kodama", chart, testCase.args...)
			if err == nil {
				t.Fatalf("helm template accepted %s:\n%s", testCase.name, rendered)
			}
		})
	}
}

func TestKodamaChartServiceIsClusterIPOnly(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")

	for _, invalid := range []struct {
		name string
		arg  string
	}{
		{name: "NodePort type", arg: "service.type=NodePort"},
		{name: "LoadBalancer type", arg: "service.type=LoadBalancer"},
		{name: "ExternalName type", arg: "service.type=ExternalName"},
		{name: "empty type", arg: "service.type="},
		{name: "zero port", arg: "service.port=0"},
		{name: "port above 65535", arg: "service.port=65536"},
		{name: "zero targetPort", arg: "service.targetPort=0"},
		{name: "targetPort above 65535", arg: "service.targetPort=65536"},
		{name: "unknown service field", arg: "service.nodePort=30000"},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs("--set", invalid.arg)...)
			if err == nil {
				t.Fatalf("helm template accepted %s:\n%s", invalid.arg, rendered)
			}
		})
	}

	for _, nonClusterIP := range []string{"NodePort", "LoadBalancer", "ExternalName", ""} {
		t.Run("template-guard-"+nonClusterIP, func(t *testing.T) {
			rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs(
				"--skip-schema-validation",
				"--set-string", "service.type="+nonClusterIP,
			)...)
			if err == nil {
				t.Fatalf("helm template --skip-schema-validation accepted service.type=%q:\n%s", nonClusterIP, rendered)
			}
			if !strings.Contains(rendered, "service.type must be ClusterIP") {
				t.Errorf("template guard failed for an unexpected reason: %v\n%s", err, rendered)
			}
		})
	}

	rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs(
		"--set-string", `service.annotations.kodama\.dev/internal=true`,
	)...)
	if err != nil {
		t.Fatalf("helm template refused valid service.annotations: %v\n%s", err, rendered)
	}
	service := renderedObjectNamed(t, rendered, "Service", "kodama")
	if got := stringAtKey(t, objectAtKey(t, service, "spec"), "type"); got != "ClusterIP" {
		t.Errorf("rendered Service type = %q, want ClusterIP", got)
	}
	annotations := objectAtKey(t, objectAtKey(t, service, "metadata"), "annotations")
	if got := stringAtKey(t, annotations, "kodama.dev/internal"); got != "true" {
		t.Errorf("rendered Service annotation = %q, want true", got)
	}
}

func TestKodamaChartRefusesOptionalCapabilities(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodama")

	for _, extra := range [][]string{
		{"--set-json", "config.optionalCapabilities=[]"},
		{"--set", "config.optionalCapabilities[0]=git"},
	} {
		rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs(extra...)...)
		if err == nil {
			t.Fatalf("helm template accepted %v:\n%s", extra, rendered)
		}
		if !strings.Contains(rendered, "optionalCapabilities") {
			t.Errorf("schema refusal did not mention optionalCapabilities: %v\n%s", err, rendered)
		}

		skipArgs := append([]string{"--skip-schema-validation"}, extra...)
		rendered, err = renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs(skipArgs...)...)
		if err == nil {
			t.Fatalf("helm template --skip-schema-validation accepted %v:\n%s", extra, rendered)
		}
		if !strings.Contains(rendered, "config.optionalCapabilities") {
			t.Errorf("template guard refusal did not mention config.optionalCapabilities: %v\n%s", err, rendered)
		}
	}

	helmStack, stack := stageStackChart(t)
	rendered, err := renderChartArgs(t, helmStack, "kodama-stack", stack, stackProductionArgs("--set-json", "kodama.config.optionalCapabilities=[]")...)
	if err == nil {
		t.Fatalf("kodama-stack accepted kodama.config.optionalCapabilities:\n%s", rendered)
	}
}
