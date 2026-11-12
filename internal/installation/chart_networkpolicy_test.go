package installation

// Network-policy and upgrade-safety chart checks. The chart is inspected as
// text: no helm binary, template engine, YAML decoder, cluster or network
// access is used. The check functions are exercised against deliberately
// mutated templates so a regression cannot silently pass.

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// checkTenantNetworkPolicyTemplate verifies that the NetworkPolicy template
// renders one default-deny policy per tenant namespace, never into the release
// namespace, and that every permitted destination is namespace-scoped.
func checkTenantNetworkPolicyTemplate(body string) error {
	if !regexp.MustCompile(`range\s+\$tenant\s*:=\s*\.Values\.tenants`).MatchString(body) {
		return fmt.Errorf("network policy template does not range over .Values.tenants")
	}
	if !regexp.MustCompile(`if\s+\.Values\.networkPolicy\.defaultDeny`).MatchString(body) {
		return fmt.Errorf("network policy template is not gated by networkPolicy.defaultDeny")
	}
	if got := strings.Count(body, "kind: NetworkPolicy"); got != 1 {
		return fmt.Errorf("network policy template renders %d policies per range iteration, want 1", got)
	}

	namespaceField := regexp.MustCompile(`(?m)^[ \t]*namespace:[ \t]*(\S.*)$`)
	matches := namespaceField.FindAllStringSubmatch(body, -1)
	if len(matches) != 1 {
		return fmt.Errorf("network policy template has %d namespace fields, want exactly 1 per tenant policy", len(matches))
	}
	namespace := strings.TrimSpace(matches[0][1])
	if namespace != "kodama-tenant-{{ $tenant }}" {
		return fmt.Errorf("tenant policy namespace = %q, want kodama-tenant-{{ $tenant }}", namespace)
	}
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "namespace:") && strings.Contains(line, ".Release.Namespace") {
			return fmt.Errorf("network policy is placed in the release namespace: %s", strings.TrimSpace(line))
		}
	}

	if !regexp.MustCompile(`(?m)^[ \t]*podSelector:[ \t]*\{[ \t]*\}[ \t]*$`).MatchString(body) {
		return fmt.Errorf("tenant policy has no empty default-deny podSelector")
	}
	for _, policyType := range []string{"Ingress", "Egress"} {
		if !regexp.MustCompile(`(?m)^[ \t]*-[ \t]*` + policyType + `[ \t]*$`).MatchString(body) {
			return fmt.Errorf("tenant policy omits the %s policy type", policyType)
		}
	}

	if got := strings.Count(body, "namespaceSelector:"); got < 3 {
		return fmt.Errorf("tenant policy has %d namespaceSelector peers, want the Kodama, Git and DNS peers", got)
	}
	kodamaPeer := regexp.MustCompile(`namespaceSelector:[ \t]*\r?\n[ \t]*matchLabels:[ \t]*\r?\n[ \t]*kubernetes\.io/metadata\.name:[ \t]*\{\{[ \t]*\$?\.Release\.Namespace[ \t]*\}\}`)
	if matches := kodamaPeer.FindAllString(body, -1); len(matches) != 2 {
		return fmt.Errorf("tenant policy has %d namespace-scoped Kodama allows, want ingress and egress", len(matches))
	}
	if !regexp.MustCompile(`(?m)^[ \t]*kubernetes\.io/metadata\.name:[ \t]*git[ \t]*$`).MatchString(body) {
		return fmt.Errorf("tenant policy does not scope the Git allow to a namespace")
	}
	if !regexp.MustCompile(`(?m)^[ \t]*k8s-app:[ \t]*kube-dns[ \t]*$`).MatchString(body) {
		return fmt.Errorf("tenant policy does not permit cluster DNS egress")
	}
	if !regexp.MustCompile(`(?m)^[ \t]*port:[ \t]*53[ \t]*$`).MatchString(body) {
		return fmt.Errorf("tenant policy does not permit DNS port 53")
	}
	for _, gate := range []string{"permitKodama", "permitGit"} {
		if !regexp.MustCompile(`if\s+\$?\.Values\.networkPolicy\.` + gate).MatchString(body) {
			return fmt.Errorf("tenant policy is not gated by networkPolicy.%s", gate)
		}
	}
	if strings.Contains(body, "8080") {
		return fmt.Errorf("tenant policy permits unserved port 8080")
	}
	if regexp.MustCompile(`(?m)^[ \t]*port:[ \t]*8443[ \t]*$`).MatchString(body) {
		return fmt.Errorf("tenant policy hardcodes port 8443 instead of reading service.targetPort")
	}
	if !strings.Contains(body, ".Values.service.targetPort | default 8443") {
		return fmt.Errorf("tenant policy does not derive $apiPort from .Values.service.targetPort | default 8443")
	}
	apiPortUses := regexp.MustCompile(`(?m)^[ \t]*port:[ \t]*\{\{[ \t]*\$apiPort[ \t]*\}\}[ \t]*$`).FindAllString(body, -1)
	if len(apiPortUses) != 2 {
		return fmt.Errorf("tenant policy uses $apiPort in %d port entries, want ingress and egress (2)", len(apiPortUses))
	}
	if !strings.Contains(body, "(ne (int ($.Values.gitService.port | default 8443)) $apiPort)") {
		return fmt.Errorf("tenant policy does not deduplicate gitService.port against $apiPort")
	}
	return nil
}

// checkRecreateStrategy verifies that the single controller never overlaps an
// old pod during an upgrade.
func checkRecreateStrategy(body string) error {
	if !regexp.MustCompile(`(?m)^[ \t]*strategy:[ \t]*\r?\n[ \t]+type:[ \t]*Recreate[ \t]*$`).MatchString(body) {
		return fmt.Errorf("Deployment does not use strategy.type: Recreate")
	}
	return nil
}

func TestTenantNetworkPoliciesAreNamespacedAndDefaultDeny(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "networkpolicy.yaml"))
	if err := checkTenantNetworkPolicyTemplate(body); err != nil {
		t.Fatalf("networkpolicy.yaml: %v", err)
	}

	// The tenant namespace naming must stay identical to namespaces.yaml.
	namespaces := readText(t, chartPath(t, "templates", "namespaces.yaml"))
	if !strings.Contains(namespaces, "kodama-tenant-{{ $tenant }}") {
		t.Fatalf("namespaces.yaml no longer names tenants kodama-tenant-<tenant>")
	}
}

func TestTenantNetworkPolicyTemplateRejectsReleaseNamespace(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "networkpolicy.yaml"))
	mutated := strings.Replace(body, "namespace: kodama-tenant-{{ $tenant }}", "namespace: {{ .Release.Namespace }}", 1)
	if mutated == body {
		t.Fatal("test mutation did not change the tenant namespace")
	}
	if err := checkTenantNetworkPolicyTemplate(mutated); err == nil {
		t.Fatal("checker accepted a tenant policy in the release namespace")
	}
}

func TestTenantNetworkPolicyTemplateRejectsUnscopedPeers(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "networkpolicy.yaml"))
	mutated := strings.ReplaceAll(body, "namespaceSelector:", "podSelector:")
	if mutated == body {
		t.Fatal("test mutation did not remove the namespace selectors")
	}
	if err := checkTenantNetworkPolicyTemplate(mutated); err == nil {
		t.Fatal("checker accepted same-namespace-only pod selectors")
	}
}

func TestTenantNetworkPolicyTemplateRejectsMissingGitGate(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "networkpolicy.yaml"))
	mutated := strings.Replace(body, "if $.Values.networkPolicy.permitGit", "if true", 1)
	if mutated == body {
		t.Fatal("test mutation did not remove the Git gate")
	}
	if err := checkTenantNetworkPolicyTemplate(mutated); err == nil {
		t.Fatal("checker accepted an ungated Git allow")
	}
}

func TestTenantNetworkPolicyUsesTargetPortAndDropsUnservedPort8080(t *testing.T) {
	deployment := readText(t, chartPath(t, "templates", "deployment.yaml"))
	if strings.Contains(deployment, "8080") {
		t.Fatalf("deployment.yaml unexpectedly references port 8080")
	}
	if !strings.Contains(deployment, ".Values.service.targetPort | default 8443") {
		t.Fatalf("deployment.yaml does not bind to .Values.service.targetPort | default 8443")
	}

	body := readText(t, chartPath(t, "templates", "networkpolicy.yaml"))
	if err := checkTenantNetworkPolicyTemplate(body); err != nil {
		t.Fatalf("networkpolicy.yaml: %v", err)
	}

	for _, mutation := range []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "re-add unserved port 8080",
			mutate: func(src string) string {
				return strings.Replace(src, "port: {{ $apiPort }}", "port: {{ $apiPort }}\n        - protocol: TCP\n          port: 8080", 1)
			},
		},
		{
			name: "hardcode port 8443",
			mutate: func(src string) string {
				return strings.Replace(src, "port: {{ $apiPort }}", "port: 8443", 1)
			},
		},
		{
			name: "deduplicate gitService.port against 8443 instead of $apiPort",
			mutate: func(src string) string {
				return strings.Replace(src, "(ne (int ($.Values.gitService.port | default 8443)) $apiPort)", "(ne (int ($.Values.gitService.port | default 8443)) 8443)", 1)
			},
		},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := mutation.mutate(body)
			if mutated == body {
				t.Fatalf("mutation %q did not change networkpolicy.yaml", mutation.name)
			}
			if err := checkTenantNetworkPolicyTemplate(mutated); err == nil {
				t.Fatalf("checker accepted %s", mutation.name)
			}
		})
	}
}

func TestControllerUsesRecreateStrategy(t *testing.T) {
	name, body := templateContaining(t, templateBodies(t), "kind: Deployment")
	if err := checkRecreateStrategy(body); err != nil {
		t.Fatalf("%s: %v", name, err)
	}

	mutated := strings.Replace(body, "strategy:\n    type: Recreate\n", "", 1)
	if mutated == body {
		t.Fatal("test mutation did not remove the strategy")
	}
	if err := checkRecreateStrategy(mutated); err == nil {
		t.Fatal("checker accepted a Deployment without the Recreate strategy")
	}
}

func TestNetworkPolicySchemaRequiresDefaultDeny(t *testing.T) {
	schema := objectAt(t, loadJSON(t, chartPath(t, "values.schema.json")), "schema")
	properties := objectAt(t, schema["properties"], "schema.properties")
	policy := objectAt(t, properties["networkPolicy"], "schema.properties.networkPolicy")
	if allowed, ok := policy["additionalProperties"].(bool); !ok || allowed {
		t.Errorf("networkPolicy additionalProperties = %v, want false", policy["additionalProperties"])
	}
	policyProperties := objectAt(t, policy["properties"], "networkPolicy.properties")
	defaultDeny := objectAt(t, policyProperties["defaultDeny"], "networkPolicy.properties.defaultDeny")
	if defaultDeny["type"] != "boolean" {
		t.Errorf("networkPolicy.defaultDeny type = %v, want boolean", defaultDeny["type"])
	}
	if defaultDeny["const"] != true {
		t.Errorf("networkPolicy.defaultDeny const = %v, want true", defaultDeny["const"])
	}
	for _, name := range []string{"permitKodama", "permitGit"} {
		property := objectAt(t, policyProperties[name], "networkPolicy.properties."+name)
		if property["type"] != "boolean" {
			t.Errorf("networkPolicy.%s type = %v, want boolean", name, property["type"])
		}
	}

	fixture := loadJSON(t, filepath.Join("testdata", "chart", "values.invalid-networkpolicy.json"))
	err := validateAgainstSchema(schema, fixture, "values")
	if err == nil {
		t.Fatal("schema accepted networkPolicy.defaultDeny: false")
	}
	if !strings.Contains(err.Error(), "networkPolicy") {
		t.Fatalf("schema refused defaultDeny:false for the wrong reason: %v", err)
	}
}
