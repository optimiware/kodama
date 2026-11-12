package installation

// The runtime Role must grant exactly one extra Job verb, patch, for the
// suspension-based Stop. Job deletion already requires delete; suspending the
// same Job needs patch on batch/jobs and nothing else.
//
// The checks below parse the literal rules of templates/rbac.yaml. The rendered
// form of the same invariant is asserted by
// TestKodamaChartsRenderExactRuntimeJobPermissions in test/component/deploy.
// Keeping the text check exact means a widened rule (jobs/status, a pod
// finalizer, a wildcard, an extra API group or resource) fails here before a
// render is attempted.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// chartRoleRule is one literal rule of a chart RBAC object.
type chartRoleRule struct {
	apiGroups     []string
	resources     []string
	verbs         []string
	resourceNames []string
}

// chartRBACChunks splits a template on top-level YAML document separators.
func chartRBACChunks(body string) []string {
	var chunks []string
	var current []string
	for line := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(line) == "---" {
			if len(current) != 0 {
				chunks = append(chunks, strings.Join(current, "\n"))
				current = nil
			}
			continue
		}
		current = append(current, line)
	}
	if len(current) != 0 {
		chunks = append(chunks, strings.Join(current, "\n"))
	}
	return chunks
}

// chartChunkKind returns the kind: of a document chunk.
func chartChunkKind(chunk string) string {
	match := regexp.MustCompile(`(?m)^kind:\s*(\S+)\s*$`).FindStringSubmatch(chunk)
	if match == nil {
		return ""
	}
	return match[1]
}

// chartInlineList parses a literal YAML flow sequence of quoted strings.
func chartInlineList(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
		return nil, fmt.Errorf("expected an inline list, got %q", value)
	}
	inner := strings.TrimSpace(value[1 : len(value)-1])
	if inner == "" {
		return nil, nil
	}
	var values []string
	for part := range strings.SplitSeq(inner, ",") {
		item := strings.TrimSpace(part)
		if item == "" {
			return nil, fmt.Errorf("empty list entry in %q", value)
		}
		if len(item) >= 2 {
			if (item[0] == '"' && item[len(item)-1] == '"') || (item[0] == '\'' && item[len(item)-1] == '\'') {
				item = item[1 : len(item)-1]
			}
		}
		values = append(values, item)
	}
	return values, nil
}

// chartRuleField assigns one rule field. Nested list fields are handled by the
// caller.
func chartRuleField(rule *chartRoleRule, field string) error {
	key, value, ok := strings.Cut(field, ":")
	if !ok {
		return fmt.Errorf("rule field %q has no colon", field)
	}
	key = strings.TrimSpace(key)
	if key == "resourceNames" {
		names, err := chartInlineList(strings.TrimSpace(value))
		if err != nil {
			// A nested block list follows; chartRoleRules collects it.
			return nil
		}
		rule.resourceNames = names
		return nil
	}
	values, err := chartInlineList(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("rule field %q: %w", key, err)
	}
	switch key {
	case "apiGroups":
		rule.apiGroups = values
	case "resources":
		rule.resources = values
	case "verbs":
		rule.verbs = values
	default:
		return fmt.Errorf("unexpected rule field %q", key)
	}
	return nil
}

// chartRoleRules parses the literal rules: block of one RBAC document chunk.
func chartRoleRules(body string) ([]chartRoleRule, error) {
	lines := strings.Split(body, "\n")
	start := -1
	for index, line := range lines {
		if strings.TrimSpace(line) == "rules:" {
			start = index
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("document has no rules: block")
	}

	var rules []chartRoleRule
	var current *chartRoleRule
	inResourceNames := false
	flush := func() {
		if current != nil {
			rules = append(rules, *current)
			current = nil
		}
	}
	for _, line := range lines[start+1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if inResourceNames {
			if indent > 4 && strings.HasPrefix(trimmed, "- ") && !strings.HasPrefix(trimmed, "- {{") {
				current.resourceNames = append(current.resourceNames, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
				continue
			}
			if indent > 4 {
				// Template control lines for the resourceNames range.
				continue
			}
			inResourceNames = false
		}
		if indent == 0 {
			break
		}
		if indent == 2 && strings.HasPrefix(trimmed, "- ") {
			flush()
			current = &chartRoleRule{}
			if err := chartRuleField(current, strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))); err != nil {
				return nil, err
			}
			continue
		}
		if indent >= 4 && current != nil {
			if strings.TrimSpace(trimmed) == "resourceNames:" {
				inResourceNames = true
				continue
			}
			if err := chartRuleField(current, trimmed); err != nil {
				return nil, err
			}
			continue
		}
		break
	}
	flush()
	if len(rules) == 0 {
		return nil, fmt.Errorf("document has an empty rules: block")
	}
	return rules, nil
}

// chartRuntimeRolePermissions expands rules into "<apiGroup>/<resource>" verb
// lists. A resource mentioned twice is an error so a widened second rule
// cannot hide behind the first.
func chartRuntimeRolePermissions(rules []chartRoleRule) (map[string][]string, error) {
	permissions := make(map[string][]string)
	for _, rule := range rules {
		if len(rule.apiGroups) == 0 || len(rule.resources) == 0 || len(rule.verbs) == 0 {
			return nil, fmt.Errorf("rule has an empty apiGroups/resources/verbs list: %+v", rule)
		}
		for _, group := range rule.apiGroups {
			for _, resource := range rule.resources {
				key := group + "/" + resource
				if _, exists := permissions[key]; exists {
					return nil, fmt.Errorf("resource %q appears in more than one rule", key)
				}
				permissions[key] = append([]string(nil), rule.verbs...)
			}
		}
	}
	return permissions, nil
}

// chartExpectedRuntimeRolePermissions is the complete runtime Role surface:
// the existing permissions plus patch on batch/jobs only.
func chartExpectedRuntimeRolePermissions() map[string][]string {
	read := []string{"get", "list", "watch"}
	write := []string{"create", "get", "list", "watch", "update", "patch", "delete"}
	return map[string][]string{
		"batch/jobs":              {"create", "get", "list", "watch", "delete", "patch"},
		"/pods":                   read,
		"/pods/log":               {"get"},
		"/persistentvolumeclaims": write,
		"/configmaps":             write,
		"/secrets":                write,
	}
}

// chartSortVerbs returns a sorted copy for order-independent comparison.
func chartSortVerbs(verbs []string) []string {
	sorted := append([]string(nil), verbs...)
	sort.Strings(sorted)
	return sorted
}

// checkChartRuntimeRole asserts the exact runtime Role surface and scoping.
func checkChartRuntimeRole(body string) error {
	var roleChunk, bindingChunk, clusterRoleChunk string
	roleCount, bindingCount, clusterRoleCount := 0, 0, 0
	for _, chunk := range chartRBACChunks(body) {
		switch chartChunkKind(chunk) {
		case "Role":
			roleCount++
			roleChunk = chunk
		case "RoleBinding":
			bindingCount++
			bindingChunk = chunk
		case "ClusterRole":
			clusterRoleCount++
			clusterRoleChunk = chunk
		}
	}
	if roleCount != 1 || bindingCount != 1 || clusterRoleCount != 1 {
		return fmt.Errorf("rbac.yaml has %d Role, %d RoleBinding and %d ClusterRole documents, want one ranged template of each", roleCount, bindingCount, clusterRoleCount)
	}

	rules, err := chartRoleRules(roleChunk)
	if err != nil {
		return fmt.Errorf("runtime Role rules: %w", err)
	}
	permissions, err := chartRuntimeRolePermissions(rules)
	if err != nil {
		return err
	}
	expected := chartExpectedRuntimeRolePermissions()
	for key, wantVerbs := range expected {
		gotVerbs, present := permissions[key]
		if !present {
			return fmt.Errorf("runtime Role does not grant %q", key)
		}
		if strings.Join(chartSortVerbs(gotVerbs), ",") != strings.Join(chartSortVerbs(wantVerbs), ",") {
			return fmt.Errorf("runtime Role grants %q verbs %v, want %v", key, gotVerbs, wantVerbs)
		}
		delete(permissions, key)
	}
	for key := range permissions {
		return fmt.Errorf("runtime Role grants unexpected %q", key)
	}

	jobVerbs := chartSortVerbs(expected["batch/jobs"])
	if !containsString(jobVerbs, "patch") {
		return fmt.Errorf("expected batch/jobs permissions do not contain patch")
	}
	if containsString(jobVerbs, "update") {
		return fmt.Errorf("batch/jobs must not include update")
	}

	for _, forbidden := range []string{"jobs/status", "pods/exec", "pods/attach", "pods/binding", "deletecollection", `"*"`, "'*'"} {
		if strings.Contains(body, forbidden) {
			return fmt.Errorf("rbac.yaml grants forbidden %s", forbidden)
		}
	}
	if regexp.MustCompile(`apiGroups:\s*\[[^\]]*"\*"`).MatchString(body) {
		return fmt.Errorf("rbac.yaml grants a wildcard API group")
	}
	if regexp.MustCompile(`resources:\s*\[[^\]]*"\*"`).MatchString(body) {
		return fmt.Errorf("rbac.yaml grants a wildcard resource")
	}

	for _, want := range []string{
		"namespace: kodama-tenant-{{ $tenant }}",
		"name: kodama-runtime",
		"kind: Role",
	} {
		if !strings.Contains(roleChunk, want) {
			return fmt.Errorf("runtime Role is not tenant-scoped: missing %q", want)
		}
	}

	for _, want := range []string{
		"kind: RoleBinding",
		"namespace: kodama-tenant-{{ $tenant }}",
		"roleRef:",
		"kind: Role",
		"name: kodama-runtime",
		"kind: ServiceAccount",
		"namespace: {{ $.Release.Namespace }}",
	} {
		if !strings.Contains(bindingChunk, want) {
			return fmt.Errorf("runtime RoleBinding is scoped incorrectly: missing %q", want)
		}
	}
	if got := strings.Count(bindingChunk, "- kind:"); got != 1 {
		return fmt.Errorf("runtime RoleBinding has %d subjects, want exactly the installation ServiceAccount", got)
	}
	subject := `name: {{ if $.Values.serviceAccount }}{{ $.Values.serviceAccount.name | default "kodama" }}{{ else }}kodama{{ end }}`
	if !strings.Contains(bindingChunk, subject) {
		return fmt.Errorf("runtime RoleBinding does not bind the configured ServiceAccount")
	}

	clusterRules, err := chartRoleRules(clusterRoleChunk)
	if err != nil {
		return fmt.Errorf("ClusterRole rules: %w", err)
	}
	if len(clusterRules) != 1 {
		return fmt.Errorf("ClusterRole has %d rules, want only the tenant Namespace read", len(clusterRules))
	}
	namespaceRule := clusterRules[0]
	if len(namespaceRule.apiGroups) != 1 || namespaceRule.apiGroups[0] != "" ||
		len(namespaceRule.resources) != 1 || namespaceRule.resources[0] != "namespaces" ||
		len(namespaceRule.verbs) != 1 || namespaceRule.verbs[0] != "get" {
		return fmt.Errorf("ClusterRole widens cluster-wide access: %+v", namespaceRule)
	}
	if len(namespaceRule.resourceNames) == 0 {
		return fmt.Errorf("ClusterRole read is not restricted with resourceNames")
	}
	for _, name := range namespaceRule.resourceNames {
		if name != "kodama-tenant-{{ $tenant }}" {
			return fmt.Errorf("ClusterRole reads an unexpected namespace %q", name)
		}
	}
	return nil
}

// TestRuntimeRoleJobVerbsIncludeOnlySuspensionPatchAddition pins the exact
// runtime Role: patch is granted on batch/jobs and nowhere new.
func TestRuntimeRoleJobVerbsIncludeOnlySuspensionPatchAddition(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "rbac.yaml"))
	if err := checkChartRuntimeRole(body); err != nil {
		t.Fatalf("templates/rbac.yaml: %v", err)
	}
}

// mutateJobsRule rewrites the batch/jobs verbs line through edit.
func mutateJobsRule(body string, edit func([]string) []string) (string, error) {
	expression := regexp.MustCompile(`(?ms)- apiGroups: \["batch"\]\s*\n\s*resources: \["jobs"\]\s*\n\s*verbs: \[([^\]]*)\]`)
	locations := expression.FindStringSubmatchIndex(body)
	if locations == nil {
		return "", fmt.Errorf("no batch/jobs rule found")
	}
	verbs, err := chartInlineList("[" + body[locations[2]:locations[3]] + "]")
	if err != nil {
		return "", err
	}
	replacement := strings.Join(edit(verbs), `", "`)
	if replacement != "" {
		replacement = `"` + replacement + `"`
	}
	return body[:locations[2]] + replacement + body[locations[3]:], nil
}

// TestRuntimeRoleJobRuleMutationSensitivity proves the checker rejects each
// mutation: a lost patch, update, jobs/status, a pod
// finalizer, a wildcard, a widened group, a cluster-scoped rule and a
// mis-scoped binding.
func TestRuntimeRoleJobRuleMutationSensitivity(t *testing.T) {
	body := readText(t, chartPath(t, "templates", "rbac.yaml"))

	jobRule := regexp.MustCompile(`(?ms)- apiGroups: \["batch"\]\s*\n\s*resources: \["jobs"\]\s*\n\s*verbs: \[[^\]]*\]`)
	if !jobRule.MatchString(body) {
		t.Fatal("rbac.yaml has no batch/jobs rule to mutate")
	}

	mutations := []struct {
		name string
		edit func(string) string
	}{
		{"remove job patch", func(body string) string {
			mutated, err := mutateJobsRule(body, func(verbs []string) []string {
				var kept []string
				for _, verb := range verbs {
					if verb != "patch" {
						kept = append(kept, verb)
					}
				}
				if len(kept) == len(verbs) && len(kept) > 0 {
					// The baseline is expected to carry patch; while it does
					// not, still exercise the checker with a lost verb.
					kept = kept[:len(kept)-1]
				}
				return kept
			})
			if err != nil {
				t.Fatalf("mutate job verbs: %v", err)
			}
			return mutated
		}},
		{"add job update", func(body string) string {
			mutated, err := mutateJobsRule(body, func(verbs []string) []string {
				return append(verbs, "update")
			})
			if err != nil {
				t.Fatalf("mutate job verbs: %v", err)
			}
			return mutated
		}},
		{"add job deletecollection", func(body string) string {
			mutated, err := mutateJobsRule(body, func(verbs []string) []string {
				return append(verbs, "deletecollection")
			})
			if err != nil {
				t.Fatalf("mutate job verbs: %v", err)
			}
			return mutated
		}},
		{"wildcard job verbs", func(body string) string {
			mutated, err := mutateJobsRule(body, func(verbs []string) []string {
				return []string{"*"}
			})
			if err != nil {
				t.Fatalf("mutate job verbs: %v", err)
			}
			return mutated
		}},
		{"widen the job api group", func(body string) string {
			return strings.Replace(body, `- apiGroups: ["batch"]`, `- apiGroups: ["batch", "apps"]`, 1)
		}},
		{"widen the job resources", func(body string) string {
			return strings.Replace(body, `resources: ["jobs"]`, `resources: ["jobs", "cronjobs"]`, 1)
		}},
		{"grant jobs/status", func(body string) string {
			return strings.Replace(body, `resources: ["jobs"]`, `resources: ["jobs", "jobs/status"]`, 1)
		}},
		{"grant pod patch", func(body string) string {
			return strings.Replace(body, `resources: ["pods"]
    verbs: ["get", "list", "watch"]`, `resources: ["pods"]
    verbs: ["get", "list", "watch", "patch"]`, 1)
		}},
		{"grant a pod finalizer rule", func(body string) string {
			return strings.Replace(body, `  - apiGroups: [""]
    resources: ["pods/log"]`, `  - apiGroups: [""]
    resources: ["pods/finalizers"]
    verbs: ["patch"]
  - apiGroups: [""]
    resources: ["pods/log"]`, 1)
		}},
		{"render a cluster-wide runtime Role", func(body string) string {
			return strings.Replace(body, "kind: Role\nmetadata:", "kind: ClusterRole\nmetadata:", 1)
		}},
		{"bind the wrong subject namespace", func(body string) string {
			return strings.Replace(body, "namespace: {{ $.Release.Namespace }}", "namespace: kodama-tenant-{{ $tenant }}", 1)
		}},
		{"bind a hardcoded ServiceAccount", func(body string) string {
			return strings.Replace(body, `{{ $.Values.serviceAccount.name | default "kodama" }}`, `{{ $.Values.serviceAccount.name | default "kodama-admin" }}`, 1)
		}},
	}

	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := mutation.edit(body)
			if mutated == body {
				t.Fatalf("mutation %q changed nothing", mutation.name)
			}
			if err := checkChartRuntimeRole(mutated); err == nil {
				t.Fatalf("checker accepted the %q mutation", mutation.name)
			}
		})
	}
}
