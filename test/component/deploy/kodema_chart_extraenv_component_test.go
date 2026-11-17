//go:build component

// Rendered-chart checks for config.extraEnv and network exactness.
//
// config.extraEnv must not be able to override the fixed production
// composition, inject a development token or cloud credential, carry an object
// (valueFrom) or render a literal password. The schema and the template guard
// are separate layers; each is proven here with the other layer removed.
//
// The FQDN policy must render exactly two egress items per tenant and no
// ingress; the tenant Namespaces must be exactly the configured tenants.
// helm runs locally against a test-owned copy of the chart; nothing contacts a
// cluster or the network.
package deploy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// renderedObjects decodes every rendered YAML document.
func renderedObjects(t *testing.T, rendered string) []map[string]any {
	t.Helper()
	var objects []map[string]any
	for _, document := range renderedDocuments(rendered) {
		document = strings.TrimSpace(document)
		document = strings.TrimSpace(strings.TrimPrefix(document, "---"))
		if document == "" {
			continue
		}
		var object map[string]any
		if err := yaml.Unmarshal([]byte(document), &object); err != nil {
			t.Fatalf("decode rendered document: %v\n%s", err, document)
		}
		if len(object) != 0 {
			objects = append(objects, object)
		}
	}
	return objects
}

// objectAtKey asserts a mapping member.
func objectAtKey(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key].(map[string]any)
	if !ok {
		t.Fatalf("field %q is not an object: %T", key, object[key])
	}
	return value
}

// sliceAtKey asserts a list member.
func sliceAtKey(t *testing.T, object map[string]any, key string) []any {
	t.Helper()
	value, ok := object[key].([]any)
	if !ok {
		t.Fatalf("field %q is not a list: %T", key, object[key])
	}
	return value
}

// stringAtKey asserts a string member.
func stringAtKey(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	value, ok := object[key].(string)
	if !ok {
		t.Fatalf("field %q is not a string: %T", key, object[key])
	}
	return value
}

// renderedObject returns the single rendered object of a kind.
func renderedObjectOfKind(t *testing.T, rendered, kind string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, object := range renderedObjects(t, rendered) {
		if object["kind"] == kind {
			if found != nil {
				t.Fatalf("rendered output has more than one %s", kind)
			}
			found = object
		}
	}
	if found == nil {
		t.Fatalf("rendered output has no %s:\n%s", kind, rendered)
	}
	return found
}

// containersOf returns the containers of a workload object.
func containersOf(t *testing.T, object map[string]any) []map[string]any {
	t.Helper()
	var spec map[string]any
	switch object["kind"] {
	case "Deployment", "StatefulSet", "DaemonSet", "Job", "ReplicaSet":
		spec = objectAtKey(t, objectAtKey(t, objectAtKey(t, object, "spec"), "template"), "spec")
	case "CronJob":
		spec = objectAtKey(t, objectAtKey(t, objectAtKey(t, objectAtKey(t, objectAtKey(t, object, "spec"), "jobTemplate"), "spec"), "template"), "spec")
	case "Pod":
		spec = objectAtKey(t, object, "spec")
	default:
		return nil
	}
	var containers []map[string]any
	for _, key := range []string{"initContainers", "containers"} {
		list, ok := spec[key].([]any)
		if !ok {
			continue
		}
		for _, entry := range list {
			container, ok := entry.(map[string]any)
			if !ok {
				t.Fatalf("%s[].%s entry is not an object", object["kind"], key)
			}
			containers = append(containers, container)
		}
	}
	return containers
}

// deploymentEnvNamesRendered returns the env names of every container in the
// rendered Deployment, in order.
func deploymentEnvNamesRendered(t *testing.T, rendered string) []string {
	t.Helper()
	var names []string
	for _, container := range containersOf(t, renderedObjectOfKind(t, rendered, "Deployment")) {
		env, ok := container["env"].([]any)
		if !ok {
			continue
		}
		for _, entry := range env {
			environment, ok := entry.(map[string]any)
			if !ok {
				t.Fatalf("env entry is not an object")
			}
			names = append(names, stringAtKey(t, environment, "name"))
		}
	}
	return names
}

// assertContainerEnvNamesUnique fails on any duplicate env name: Kubernetes
// keeps the last duplicate, so a duplicate would silently override a fixed
// production variable.
func assertContainerEnvNamesUnique(t *testing.T, rendered string) {
	t.Helper()
	for _, object := range renderedObjects(t, rendered) {
		for _, container := range containersOf(t, object) {
			env, ok := container["env"].([]any)
			if !ok {
				continue
			}
			seen := map[string]int{}
			for _, entry := range env {
				environment, ok := entry.(map[string]any)
				if !ok {
					t.Fatalf("env entry is not an object")
				}
				seen[stringAtKey(t, environment, "name")]++
			}
			for name, count := range seen {
				if count > 1 {
					t.Errorf("%v %v has env name %s %d times; the last duplicate wins", object["kind"], object["metadata"], name, count)
				}
			}
		}
	}
}

// rewriteStagedFile replaces the staged chart file body.
func rewriteStagedFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write staged %s: %v", path, err)
	}
}

// renderKodema renders the staged kodema chart with the production OIDC
// inputs cmd/kodema/production.go requires.
func renderKodema(t *testing.T, helm, chart string, extra ...string) (string, error) {
	t.Helper()
	return renderChartArgs(t, helm, "kodema", chart, append(productionOIDCArgs(""), extra...)...)
}

// extraEnvOverrideAttempts are the override attacks the chart must refuse.
var extraEnvOverrideAttempts = []struct {
	name string
	set  string
	want string
}{
	{"profile", "config.extraEnv.KODEMA_PROFILE=development", "KODEMA_PROFILE"},
	{"development token", "config.extraEnv.KODEMA_DEV_TOKEN=s3cret", "KODEMA_DEV_TOKEN"},
	{"TLS directory", "config.extraEnv.KODEMA_TLS_DIR=/tmp", "KODEMA_TLS_DIR"},
	{"database URL", "config.extraEnv.KODEMA_DATABASE_URL=postgres://u:pw@h/db?sslmode=disable", "KODEMA_DATABASE_URL"},
}

func TestKodemaChartExtraEnvOverridesAreRefused(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodema")
	for _, attempt := range extraEnvOverrideAttempts {
		t.Run(attempt.name, func(t *testing.T) {
			rendered, err := renderKodema(t, helm, chart, "--set", attempt.set)
			if err == nil {
				t.Fatalf("helm template accepted %s; the production composition is not fail-closed:\n%s", attempt.set, rendered)
			}
			if !strings.Contains(rendered, attempt.want) {
				t.Errorf("refusal for %s does not name the offending variable: %v\n%s", attempt.want, err, rendered)
			}
		})
	}
}

func TestKodemaChartExtraEnvRefusesEveryFixedName(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodema")
	rendered, err := renderKodema(t, helm, chart)
	if err != nil {
		t.Fatalf("helm template kodema: %v\n%s", err, rendered)
	}
	fixed := deploymentEnvNamesRendered(t, rendered)
	if len(fixed) < 20 {
		t.Fatalf("rendered Deployment has only %d fixed env names: %v", len(fixed), fixed)
	}
	// The four production OIDC inputs are config.extraEnv values, not fixed
	// names; they are covered by the allowlist tests below.
	extraEnvNames := map[string]bool{
		"KODEMA_OIDC_ISSUER":       true,
		"KODEMA_OIDC_AUDIENCE":     true,
		"KODEMA_OIDC_JWKS_URL":     true,
		"KODEMA_OIDC_TENANT_CLAIM": true,
	}
	for _, name := range fixed {
		if extraEnvNames[name] {
			continue
		}
		t.Run(name, func(t *testing.T) {
			out, err := renderKodema(t, helm, chart, "--set", "config.extraEnv."+name+"=override")
			if err == nil {
				t.Fatalf("helm template accepted an extraEnv override of the fixed variable %s:\n%s", name, out)
			}
			if !strings.Contains(out, name) {
				t.Errorf("refusal of %s does not name it: %v\n%s", name, err, out)
			}
		})
	}
}

func TestKodemaChartExtraEnvRefusesDangerousNames(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodema")
	for _, name := range []string{
		"KODEMA_DEV_TOKEN",
		"KODEMA_DEV_PRINCIPAL",
		"SCW_SECRET_KEY",
		"AWS_ACCESS_KEY_ID",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"KODEMA_OIDC_TOKEN",
		"KODEMA_PRIVATE_KEY",
		"KODEMA_PASSWORD",
		"KODEMA_OIDC_ISSUER_EXTRA",
		"UNRELATED",
	} {
		t.Run(name, func(t *testing.T) {
			out, err := renderKodema(t, helm, chart, "--set", "config.extraEnv."+name+"=x")
			if err == nil {
				t.Fatalf("helm template accepted config.extraEnv.%s:\n%s", name, out)
			}
		})
	}

	// A mounted path is the one credential-shaped shape that is allowed; the
	// allowlist still decides which names may carry it.
	rendered, err := renderKodema(t, helm, chart, "--set", "config.extraEnv.KODEMA_OIDC_CA_FILE=/etc/kodema/oidc-ca/ca.crt")
	if err != nil {
		t.Fatalf("helm template refused the allowlisted KODEMA_OIDC_CA_FILE path: %v\n%s", err, rendered)
	}
}

func TestKodemaChartExtraEnvRendersAllowlistedNameOnce(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodema")
	rendered, err := renderKodema(t, helm, chart,
		"--set", "config.extraEnv.KODEMA_OIDC_ISSUER=https://issuer.example",
		"--set", "config.extraEnv.KODEMA_OIDC_AUDIENCE=kodema",
		"--set", "config.extraEnv.KODEMA_DISCOVERY_ISSUER=https://issuer.example",
		"--set", "config.extraEnv.KODEMA_TENANT_NAMESPACE=kodema-tenant-tenant-a",
		"--set-string", "config.extraEnv.KODEMA_WORKLOAD_CPU_LIMIT=2",
	)
	if err != nil {
		t.Fatalf("helm template kodema with allowlisted extraEnv: %v\n%s", err, rendered)
	}
	assertContainerEnvNamesUnique(t, rendered)

	names := deploymentEnvNamesRendered(t, rendered)
	counts := map[string]int{}
	for _, name := range names {
		counts[name]++
	}
	for _, name := range []string{
		"KODEMA_OIDC_ISSUER", "KODEMA_OIDC_AUDIENCE", "KODEMA_DISCOVERY_ISSUER",
		"KODEMA_TENANT_NAMESPACE", "KODEMA_WORKLOAD_CPU_LIMIT",
	} {
		if counts[name] != 1 {
			t.Errorf("config.extraEnv rendered %s %d times, want exactly 1", name, counts[name])
		}
	}

	deployment := renderedObjectOfKind(t, rendered, "Deployment")
	for _, container := range containersOf(t, deployment) {
		env, ok := container["env"].([]any)
		if !ok {
			continue
		}
		for _, entry := range env {
			environment, ok := entry.(map[string]any)
			if !ok {
				t.Fatalf("env entry is not an object")
			}
			if stringAtKey(t, environment, "name") == "KODEMA_OIDC_ISSUER" {
				if value := stringAtKey(t, environment, "value"); value != "https://issuer.example" {
					t.Errorf("KODEMA_OIDC_ISSUER = %q, want the literal configured value", value)
				}
			}
		}
	}
}

func TestKodemaChartExtraEnvRefusesCredentialURLAndObject(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodema")

	out, err := renderKodema(t, helm, chart, "--set", "config.extraEnv.KODEMA_OIDC_CA_FILE=postgres://u:pw@h/db?sslmode=disable")
	if err == nil {
		t.Fatalf("helm template accepted a URL with inline userinfo credentials:\n%s", out)
	}

	out, err = renderKodema(t, helm, chart, "--set-json", `config.extraEnv.KODEMA_OIDC_CA_FILE={"valueFrom":{"secretKeyRef":{"name":"x","key":"y"}}}`)
	if err == nil {
		t.Fatalf("helm template accepted an object (valueFrom) as config.extraEnv.KODEMA_OIDC_CA_FILE:\n%s", out)
	}
}

func TestKodemaChartEmptyExtraEnvRendersUnchanged(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodema")

	// A development profile has no production-only environment, so an explicit
	// empty extraEnv is the baseline and must change nothing.
	baseline, err := renderChartArgs(t, helm, "kodema", chart, "--set", "environment=development")
	if err != nil {
		t.Fatalf("helm template kodema: %v\n%s", err, baseline)
	}
	assertContainerEnvNamesUnique(t, baseline)
	names := deploymentEnvNamesRendered(t, baseline)
	for _, name := range names {
		switch name {
		case "KODEMA_OIDC_ISSUER", "KODEMA_OIDC_AUDIENCE", "KODEMA_DISCOVERY_ISSUER", "KODEMA_TENANT_NAMESPACE":
			t.Errorf("empty config.extraEnv rendered %s", name)
		}
	}

	explicit, err := renderChartArgs(t, helm, "kodema", chart, "--set", "environment=development", "--set-json", "config.extraEnv={}")
	if err != nil {
		t.Fatalf("helm template kodema with an explicit empty extraEnv: %v\n%s", err, explicit)
	}
	if got, want := len(renderedObjects(t, explicit)), len(renderedObjects(t, baseline)); got != want {
		t.Errorf("explicit empty extraEnv rendered %d objects, default rendered %d", got, want)
	}
}

// TestKodemaChartExtraEnvTemplateGuardAloneRefuses removes the schema layer and
// proves the template guard still refuses every attack by itself.
func TestKodemaChartExtraEnvTemplateGuardAloneRefuses(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodema")
	schemaPath := filepath.Join(chart, "values.schema.json")
	body, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read staged schema: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatalf("decode staged schema: %v", err)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("staged schema has no properties")
	}
	properties["config"] = map[string]any{"type": "object"}
	encoded, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		t.Fatalf("encode staged schema: %v", err)
	}
	rewriteStagedFile(t, schemaPath, string(encoded))

	attempts := []struct {
		name string
		args []string
		want string
	}{
		{"profile", []string{"--set", "config.extraEnv.KODEMA_PROFILE=development"}, "collides with a fixed variable"},
		{"development token", []string{"--set", "config.extraEnv.KODEMA_DEV_TOKEN=s3cret"}, "must not set a development token"},
		{"TLS directory", []string{"--set", "config.extraEnv.KODEMA_TLS_DIR=/tmp"}, "collides with a fixed variable"},
		{"database URL", []string{"--set", "config.extraEnv.KODEMA_DATABASE_URL=postgres://u:pw@h/db"}, "collides with a fixed variable"},
		{"cloud credential", []string{"--set", "config.extraEnv.SCW_SECRET_KEY=x"}, "must not set a cloud credential"},
		{"credential name", []string{"--set", "config.extraEnv.KODEMA_OIDC_TOKEN=x"}, "looks like a credential name"},
		{"not allowlisted", []string{"--set", "config.extraEnv.UNRELATED=x"}, "not an allowlisted optional server variable"},
		{"userinfo value", []string{"--set", "config.extraEnv.KODEMA_OIDC_ISSUER=postgres://u:pw@h/db"}, "inline userinfo credentials"},
		{"object value", []string{"--set-json", `config.extraEnv.KODEMA_OIDC_CA_FILE={"valueFrom":{"secretKeyRef":{"name":"x","key":"y"}}}`}, "must be a string literal value"},
	}
	for _, attempt := range attempts {
		t.Run(attempt.name, func(t *testing.T) {
			out, err := renderKodema(t, helm, chart, attempt.args...)
			if err == nil {
				t.Fatalf("the template guard alone accepted %s:\n%s", attempt.name, out)
			}
			if !strings.Contains(out, attempt.want) {
				t.Errorf("refusal for %s does not carry the guard message %q:\n%s", attempt.name, attempt.want, out)
			}
		})
	}
}

// TestKodemaChartExtraEnvSchemaAloneRefuses removes the template guard and
// proves the schema still refuses the four override attempts by itself.
func TestKodemaChartExtraEnvSchemaAloneRefuses(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodema")
	deploymentPath := filepath.Join(chart, "templates", "deployment.yaml")
	body, err := os.ReadFile(deploymentPath)
	if err != nil {
		t.Fatalf("read staged deployment: %v", err)
	}
	withoutInclude := strings.Replace(string(body), "{{- include \"kodema.validateExtraEnv\" . }}\n", "", 1)
	rewriteStagedFile(t, deploymentPath, withoutInclude)
	if err := os.Remove(filepath.Join(chart, "templates", "_extraenv.tpl")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove staged guard template: %v", err)
	}

	for _, attempt := range extraEnvOverrideAttempts {
		t.Run(attempt.name, func(t *testing.T) {
			out, err := renderKodema(t, helm, chart, "--set", attempt.set)
			if err == nil {
				t.Fatalf("the schema alone accepted %s:\n%s", attempt.name, out)
			}
			if !strings.Contains(out, "schema") {
				t.Errorf("refusal for %s does not come from the schema: %v\n%s", attempt.name, err, out)
			}
		})
	}
}

// TestKodemaChartCiliumPolicyExactShape pins the rendered policy: exactly two
// egress items, no ingress, exact hosts, one policy per tenant.
func TestKodemaChartCiliumPolicyExactShape(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodema")
	hosts := []string{"registry.npmjs.org", "api.github.com"}
	rendered, err := renderKodema(t, helm, chart,
		"--api-versions", "cilium.io/v2/CiliumNetworkPolicy",
		"--set-json", `tenants=["tenant-a","tenant-b"]`,
		"--set", "networkPolicy.egressFQDNs[0]="+hosts[0],
		"--set", "networkPolicy.egressFQDNs[1]="+hosts[1],
	)
	if err != nil {
		t.Fatalf("helm template kodema with egressFQDNs: %v\n%s", err, rendered)
	}

	var policies []map[string]any
	for _, object := range renderedObjects(t, rendered) {
		if object["kind"] == "CiliumNetworkPolicy" {
			policies = append(policies, object)
		}
	}
	if len(policies) != 2 {
		t.Fatalf("rendered %d CiliumNetworkPolicy objects, want exactly one per tenant (2)", len(policies))
	}
	wantNamespaces := map[string]bool{
		"kodema-tenant-tenant-a": false,
		"kodema-tenant-tenant-b": false,
	}
	for _, policy := range policies {
		metadata := objectAtKey(t, policy, "metadata")
		namespace := stringAtKey(t, metadata, "namespace")
		if _, ok := wantNamespaces[namespace]; !ok {
			t.Errorf("policy rendered in unexpected namespace %q", namespace)
			continue
		}
		wantNamespaces[namespace] = true

		spec := objectAtKey(t, policy, "spec")
		selector := objectAtKey(t, spec, "endpointSelector")
		if len(selector) != 0 {
			t.Errorf("%s endpointSelector selects only some endpoints: %v", namespace, selector)
		}
		if _, ok := spec["ingress"]; ok {
			t.Errorf("%s policy renders an ingress key; tenant ingress is the default-deny policy's job", namespace)
		}
		egress := sliceAtKey(t, spec, "egress")
		if len(egress) != 2 {
			t.Fatalf("%s has %d egress items, want exactly 2 (DNS to kube-dns and toFQDNs)", namespace, len(egress))
		}

		dnsRule, ok := egress[0].(map[string]any)
		if !ok {
			t.Fatalf("%s egress[0] is not an object", namespace)
		}
		if got := sortedKeys(dnsRule); strings.Join(got, ",") != "toEndpoints,toPorts" {
			t.Errorf("%s DNS rule keys = %v, want only toEndpoints and toPorts", namespace, got)
		}
		endpoints := sliceAtKey(t, dnsRule, "toEndpoints")
		if len(endpoints) != 1 {
			t.Errorf("%s DNS rule has %d peers, want only kube-dns", namespace, len(endpoints))
		}
		endpoint, ok := endpoints[0].(map[string]any)
		if !ok {
			t.Fatalf("%s DNS peer is not an object", namespace)
		}
		labels := objectAtKey(t, endpoint, "matchLabels")
		if len(labels) != 2 || labels["io.kubernetes.pod.namespace"] != "kube-system" || labels["k8s-app"] != "kube-dns" {
			t.Errorf("%s DNS peer labels = %v, want kube-system/kube-dns only", namespace, labels)
		}
		dnsPorts := sliceAtKey(t, dnsRule, "toPorts")
		if len(dnsPorts) != 1 {
			t.Errorf("%s DNS rule has %d toPorts entries, want 1", namespace, len(dnsPorts))
		}
		dnsPort, ok := dnsPorts[0].(map[string]any)
		if !ok {
			t.Fatalf("%s DNS toPorts entry is not an object", namespace)
		}
		ports := sliceAtKey(t, dnsPort, "ports")
		if len(ports) != 1 {
			t.Errorf("%s DNS rule opens %d ports, want only 53", namespace, len(ports))
		}
		port, ok := ports[0].(map[string]any)
		if !ok {
			t.Fatalf("%s DNS port is not an object", namespace)
		}
		if port["port"] != "53" || port["protocol"] != "ANY" {
			t.Errorf("%s DNS port = %v, want 53/ANY", namespace, port)
		}
		dns := objectAtKey(t, dnsPort, "rules")
		if got := sortedKeys(dns); strings.Join(got, ",") != "dns" {
			t.Errorf("%s DNS rule nested rules = %v, want only dns", namespace, got)
		}
		matchNames := sliceAtKey(t, dns, "dns")
		assertMatchNames(t, namespace+" DNS rule", matchNames, hosts)

		fqdnRule, ok := egress[1].(map[string]any)
		if !ok {
			t.Fatalf("%s egress[1] is not an object", namespace)
		}
		if got := sortedKeys(fqdnRule); strings.Join(got, ",") != "toFQDNs,toPorts" {
			t.Errorf("%s FQDN rule keys = %v, want only toFQDNs and toPorts", namespace, got)
		}
		assertMatchNames(t, namespace+" toFQDNs rule", sliceAtKey(t, fqdnRule, "toFQDNs"), hosts)
		fqdnPorts := sliceAtKey(t, fqdnRule, "toPorts")
		if len(fqdnPorts) != 1 {
			t.Errorf("%s FQDN rule has %d toPorts entries, want 1", namespace, len(fqdnPorts))
		}
		fqdnPort, ok := fqdnPorts[0].(map[string]any)
		if !ok {
			t.Fatalf("%s FQDN toPorts entry is not an object", namespace)
		}
		fqdnPortList := sliceAtKey(t, fqdnPort, "ports")
		if len(fqdnPortList) != 1 {
			t.Errorf("%s FQDN rule opens %d ports, want only 443", namespace, len(fqdnPortList))
		}
		fqdnSingle, ok := fqdnPortList[0].(map[string]any)
		if !ok {
			t.Fatalf("%s FQDN port is not an object", namespace)
		}
		if fqdnSingle["port"] != "443" || fqdnSingle["protocol"] != "TCP" {
			t.Errorf("%s FQDN port = %v, want 443/TCP", namespace, fqdnSingle)
		}
		if got := sortedKeys(fqdnPort); strings.Join(got, ",") != "ports" {
			t.Errorf("%s FQDN toPorts entry keys = %v, want only ports", namespace, got)
		}
	}
	for namespace, seen := range wantNamespaces {
		if !seen {
			t.Errorf("no CiliumNetworkPolicy rendered for tenant namespace %s", namespace)
		}
	}
}

// assertMatchNames requires exactly the configured hosts as matchName entries.
func assertMatchNames(t *testing.T, label string, entries []any, hosts []string) {
	t.Helper()
	if len(entries) != len(hosts) {
		t.Fatalf("%s has %d entries, want %d", label, len(entries), len(hosts))
	}
	for index, entry := range entries {
		object, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("%s entry %d is not an object", label, index)
		}
		if got := sortedKeys(object); strings.Join(got, ",") != "matchName" {
			t.Errorf("%s entry %d keys = %v, want only matchName", label, index, got)
		}
		if stringAtKey(t, object, "matchName") != hosts[index] {
			t.Errorf("%s entry %d = %v, want %q", label, index, object["matchName"], hosts[index])
		}
	}
}

// sortedKeys returns the map keys in sorted order.
func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// TestKodemaChartTenantNamespacesAreExactlyTenants pins the rendered Namespace
// set: one per declared tenant and nothing else (no kube-system, no literal
// cluster namespace).
func TestKodemaChartTenantNamespacesAreExactlyTenants(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodema")
	for _, tenants := range [][]string{
		{"tenant-a"},
		{"tenant-a", "tenant-b"},
	} {
		t.Run(strings.Join(tenants, "-"), func(t *testing.T) {
			encoded, err := json.Marshal(tenants)
			if err != nil {
				t.Fatalf("encode tenants: %v", err)
			}
			rendered, err := renderKodema(t, helm, chart, "--set-json", "tenants="+string(encoded))
			if err != nil {
				t.Fatalf("helm template kodema with tenants: %v\n%s", err, rendered)
			}
			got := map[string]bool{}
			for _, object := range renderedObjects(t, rendered) {
				if object["kind"] != "Namespace" {
					continue
				}
				name := stringAtKey(t, objectAtKey(t, object, "metadata"), "name")
				got[name] = true
			}
			if len(got) != len(tenants) {
				t.Fatalf("rendered %d Namespace objects %v, want %d", len(got), got, len(tenants))
			}
			want := map[string]bool{}
			for _, tenant := range tenants {
				want["kodema-tenant-"+tenant] = true
			}
			for name := range want {
				if !got[name] {
					t.Errorf("rendered Namespaces %v do not include %s", got, name)
				}
			}
			for name := range got {
				if !want[name] {
					t.Errorf("rendered a Namespace outside the tenant set: %s", name)
				}
			}
		})
	}
}

// TestKodemaChartSchemaBoundsTenantsAndFQDNs proves the two schema bounds are
// enforced by helm itself, with a clear values error instead of a render error.
func TestKodemaChartSchemaBoundsTenantsAndFQDNs(t *testing.T) {
	helm, charts := stagedCharts(t)
	chart := filepath.Join(charts, "kodema")

	out, err := renderKodema(t, helm, chart, "--set-json", "tenants=[]")
	if err == nil {
		t.Fatalf("helm template accepted an empty tenants list:\n%s", out)
	}
	if !strings.Contains(out, "schema") {
		t.Errorf("empty tenants was refused for a reason other than values.schema.json: %v\n%s", err, out)
	}

	label := strings.Repeat("a", 63)
	tooLong := strings.Join([]string{label, label, label, strings.Repeat("a", 62)}, ".")
	if len(tooLong) != 254 {
		t.Fatalf("test fixture length = %d, want 254", len(tooLong))
	}
	out, err = renderKodema(t, helm, chart, "--set", "networkPolicy.egressFQDNs[0]="+tooLong)
	if err == nil {
		t.Fatalf("helm template accepted a 254-character egressFQDN:\n%s", out)
	}
	if !strings.Contains(out, "schema") {
		t.Errorf("long egressFQDN was refused for a reason other than values.schema.json: %v\n%s", err, out)
	}
}
