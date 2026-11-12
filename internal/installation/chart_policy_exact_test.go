package installation

// Exact-structure tests for the chart's network assets.
//
// The FQDN egress policy is only honest if it grants exactly the listed hosts on
// 443 plus DNS to kube-dns and nothing else. A widened template (an extra CIDR,
// a cluster/world entity, a peerless toPorts rule, an allow-all empty egress
// item or an ingress rule) would keep the looser shape checks green, so these
// tests pin the exact item count and the exact keys of every item.
//
// namespaces.yaml must render exactly one Namespace per declared tenant. A
// stray kube-system Namespace carrying the pod-security labels would otherwise
// pass the label checks and rewrite cluster policy.
//
// The rendered form of both invariants is checked with helm in
// test/component/deploy.

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// checkTenantNamespacesTemplate requires exactly one Namespace document, ranged
// over .Values.tenants, named kodama-tenant-<tenant>, with no literal cluster
// namespace anywhere.
func checkTenantNamespacesTemplate(body string) error {
	if got := strings.Count(body, "kind: Namespace"); got != 1 {
		return fmt.Errorf("namespaces.yaml renders %d Namespace documents, want exactly one per tenant from one ranged template", got)
	}
	if !regexp.MustCompile(`(?m)\{\{-?[ \t]*range[ \t]+\$tenant[ \t]*:=[ \t]*\.Values\.tenants[ \t]*\}\}`).MatchString(body) {
		return fmt.Errorf("namespaces.yaml does not range over .Values.tenants")
	}
	for _, match := range regexp.MustCompile(`(?m)^[ \t]*name:[ \t]*(.+?)[ \t]*$`).FindAllStringSubmatch(body, -1) {
		value := strings.TrimSpace(match[1])
		if strings.Contains(value, "{{") {
			if value != "kodama-tenant-{{ $tenant }}" {
				return fmt.Errorf("namespaces.yaml renders a templated object name %q that is not kodama-tenant-{{ $tenant }}", value)
			}
			continue
		}
		if value != "kodama-tenant-limits" {
			return fmt.Errorf("namespaces.yaml renders a literal object name %q, want only the scoped LimitRange name", value)
		}
	}
	if regexp.MustCompile(`(?m)^[ \t]*name:[ \t]*kube-system[ \t]*$`).MatchString(body) || strings.Contains(body, "kubernetes.io/metadata.name: kube-system") {
		return fmt.Errorf("namespaces.yaml names or targets kube-system; tenant assets must stay in kodama-tenant-*")
	}
	return nil
}

// ciliumEgressItems returns the egress list items of the template, split at the
// four-space indent the chart uses for policy egress peers.
func ciliumEgressItems(body string) []string {
	var items []string
	var current []string
	for line := range strings.SplitSeq(body, "\n") {
		if regexp.MustCompile(`^[ ]{4}-[ \t]+\S`).MatchString(line) {
			if current != nil {
				items = append(items, strings.Join(current, "\n"))
			}
			current = []string{line}
			continue
		}
		if current == nil {
			continue
		}
		if trimmed := strings.TrimSpace(line); trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if regexp.MustCompile(`^[ ]{4,}\S`).MatchString(line) {
			current = append(current, line)
			continue
		}
		items = append(items, strings.Join(current, "\n"))
		current = nil
	}
	if current != nil {
		items = append(items, strings.Join(current, "\n"))
	}
	return items
}

// checkCiliumPolicyExactTemplate pins the exact egress surface.
func checkCiliumPolicyExactTemplate(body string) error {
	if err := checkCiliumFQDNPolicyTemplate(body); err != nil {
		return err
	}
	if regexp.MustCompile(`(?m)^[ \t]*ingress:`).MatchString(body) {
		return fmt.Errorf("policy renders an ingress key; tenant ingress belongs to the default-deny policy alone")
	}
	for _, forbidden := range []string{
		"toCIDR", "toCIDRSet", "toEntities", "toServices", "toNodes", "toGroups",
		"fromCIDR", "fromCIDRSet", "fromEntities", "fromEndpoints", "fromFQDNs", "fromServices", "fromNodes",
		"matchPattern", "matchExpressions",
	} {
		if strings.Contains(body, forbidden) {
			return fmt.Errorf("policy uses %s; only exact matchName egress to the listed hosts is allowed", forbidden)
		}
	}
	items := ciliumEgressItems(body)
	if len(items) != 2 {
		return fmt.Errorf("policy egress has %d items, want exactly 2 (DNS to kube-dns and toFQDNs); a bare {} item allows everything", len(items))
	}
	if !strings.Contains(items[0], "- toEndpoints:") {
		return fmt.Errorf("first egress item is not the kube-dns DNS rule:\n%s", items[0])
	}
	if !strings.Contains(items[1], "- toFQDNs:") {
		return fmt.Errorf("second egress item is not the toFQDNs rule:\n%s", items[1])
	}
	if strings.Contains(items[0], "toFQDNs:") {
		return fmt.Errorf("the DNS rule also carries toFQDNs; DNS and host allowance must be separate items")
	}
	if strings.Contains(items[1], "toEndpoints:") {
		return fmt.Errorf("the toFQDNs rule also carries toEndpoints")
	}
	if got := strings.Count(body, "kube-dns"); got != 1 {
		return fmt.Errorf("policy names kube-dns %d times, want exactly the DNS rule", got)
	}
	if got := strings.Count(body, "protocol: ANY"); got != 1 {
		return fmt.Errorf("policy sets protocol ANY %d times, want only the port-53 DNS rule", got)
	}
	if got := strings.Count(body, "protocol: TCP"); got != 1 {
		return fmt.Errorf("policy sets protocol TCP %d times, want only the port-443 FQDN rule", got)
	}
	if got := strings.Count(body, "rules:"); got != 1 {
		return fmt.Errorf("policy has %d rules: blocks, want only the DNS rule", got)
	}
	if got := strings.Count(body, `port: "443"`); got != 1 {
		return fmt.Errorf("policy opens port 443 %d times, want once", got)
	}
	if got := strings.Count(body, `port: "53"`); got != 1 {
		return fmt.Errorf("policy opens port 53 %d times, want once", got)
	}
	return nil
}

func TestTenantNamespaceTemplateIsScopedToTenants(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "namespaces.yaml"))
	if err := checkTenantNamespacesTemplate(body); err != nil {
		t.Fatalf("namespaces.yaml: %v", err)
	}
}

func TestTenantNamespaceTemplateMutationSensitivity(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "namespaces.yaml"))
	kubeSystem := "\n---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: kube-system\n  labels:\n    pod-security.kubernetes.io/enforce: baseline\n"
	if err := checkTenantNamespacesTemplate(body + kubeSystem); err == nil {
		t.Fatal("checker accepted a kube-system Namespace with pod-security labels")
	}

	unscoped := strings.Replace(body, "name: kodama-tenant-{{ $tenant }}", "name: kodama-tenant-fixed", 1)
	if unscoped == body {
		t.Fatal("test mutation did not pin one namespace name")
	}
	if err := checkTenantNamespacesTemplate(unscoped); err == nil {
		t.Fatal("checker accepted a Namespace that ignores .Values.tenants")
	}
}

func TestCiliumPolicyTemplateIsExactlyTwoEgressItems(t *testing.T) {
	bodies := templateBodies(t)
	body, ok := bodies["ciliumpolicy.yaml"]
	if !ok {
		t.Fatal("missing templates/ciliumpolicy.yaml")
	}
	if err := checkCiliumPolicyExactTemplate(body); err != nil {
		t.Fatalf("ciliumpolicy.yaml: %v", err)
	}
}

func TestCiliumPolicyMutationSensitivityCoversWidening(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "ciliumpolicy.yaml"))

	mutations := []struct {
		name string
		edit func(string) string
	}{
		{"toCIDR metadata address", func(body string) string {
			return body + "\n      toCIDR:\n        - 169.254.169.254/32\n"
		}},
		{"toEntities cluster", func(body string) string {
			return body + "\n      toEntities:\n        - cluster\n"
		}},
		{"toEntities world", func(body string) string {
			return body + "\n      toEntities:\n        - world\n"
		}},
		{"peerless toPorts 443", func(body string) string {
			return body + "\n    - toPorts:\n        - ports:\n            - port: \"443\"\n              protocol: TCP\n"
		}},
		{"bare empty egress item", func(body string) string {
			return strings.Replace(body, "    - toFQDNs:\n", "    - {}\n    - toFQDNs:\n", 1)
		}},
		{"ingress from world", func(body string) string {
			return strings.Replace(body, "  egress:\n", "  ingress:\n    - fromEntities:\n        - world\n  egress:\n", 1)
		}},
		{"ingress allow-all", func(body string) string {
			return strings.Replace(body, "  egress:\n", "  ingress:\n    - {}\n  egress:\n", 1)
		}},
		{"second toEndpoints peer", func(body string) string {
			return body + "\n    - toEndpoints:\n        - matchLabels:\n            app: proxy\n"
		}},
		{"services peer", func(body string) string {
			return body + "\n      toServices:\n        - kube-system/kube-dns\n"
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := mutation.edit(body)
			if mutated == body {
				t.Fatalf("mutation %q changed nothing", mutation.name)
			}
			if err := checkCiliumPolicyExactTemplate(mutated); err == nil {
				t.Fatalf("checker accepted the %q policy widening", mutation.name)
			}
		})
	}
}
