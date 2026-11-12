//go:build component

// Rendered RBAC checks: the runtime Role grants exactly patch on batch/jobs
// beyond its other permissions, and nothing else.
//
// The installation package inspects templates/rbac.yaml as text, so a template
// edit that renders a widened rule (Job update, jobs/status, a pod finalizer,
// a wildcard, an extra API group or resource, a cluster-scoped workload rule)
// or that covers only the first tenant could keep the text checks green. These
// tests render both the standalone and the stack chart with two tenants and a
// custom ServiceAccount and assert the complete rendered permission sets.
// helm runs locally; nothing contacts a cluster or the network.
package deploy

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// rbacObjectsOfKind returns every rendered object of one kind.
func rbacObjectsOfKind(t *testing.T, rendered, kind string) []map[string]any {
	t.Helper()
	var objects []map[string]any
	for _, object := range renderedObjects(t, rendered) {
		if object["kind"] == kind {
			objects = append(objects, object)
		}
	}
	return objects
}

// rbacObjectNamed returns the rendered object with the given kind, name and
// namespace. An empty namespace matches any object of that kind and name.
func rbacObjectNamed(t *testing.T, rendered, kind, name, namespace string) map[string]any {
	t.Helper()
	for _, object := range rbacObjectsOfKind(t, rendered, kind) {
		metadata := objectAtKey(t, object, "metadata")
		if stringAtKey(t, metadata, "name") != name {
			continue
		}
		if namespace != "" && stringAtKey(t, metadata, "namespace") != namespace {
			continue
		}
		return object
	}
	t.Fatalf("rendered output has no %s named %s in namespace %q:\n%s", kind, name, namespace, rendered)
	return nil
}

// rbacStringList decodes one string list field.
func rbacStringList(t *testing.T, object map[string]any, key string) []string {
	t.Helper()
	values, ok := object[key].([]any)
	if !ok {
		t.Fatalf("field %q is not a list: %T", key, object[key])
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("field %q contains a non-string entry: %T", key, value)
		}
		result = append(result, text)
	}
	return result
}

// rbacSorted returns a sorted copy for order-independent comparison.
func rbacSorted(values []string) []string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return sorted
}

// rbacPermissionMap expands a rendered role's rules into
// "<apiGroup>/<resource>" verb sets. A resource declared twice is an error so a
// widened second rule cannot hide behind the first.
func rbacPermissionMap(t *testing.T, role map[string]any) map[string][]string {
	t.Helper()
	permissions := make(map[string][]string)
	for _, raw := range sliceAtKey(t, role, "rules") {
		rule, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("role rule is not an object: %T", raw)
		}
		apiGroups := rbacStringList(t, rule, "apiGroups")
		resources := rbacStringList(t, rule, "resources")
		verbs := rbacStringList(t, rule, "verbs")
		if len(apiGroups) == 0 || len(resources) == 0 || len(verbs) == 0 {
			t.Fatalf("role rule has an empty apiGroups/resources/verbs list: %v", rule)
		}
		for _, group := range apiGroups {
			for _, resource := range resources {
				key := group + "/" + resource
				if _, exists := permissions[key]; exists {
					t.Fatalf("resource %q appears in more than one rendered rule", key)
				}
				permissions[key] = append([]string(nil), verbs...)
			}
		}
	}
	return permissions
}

// rbacExpectedRuntimePermissions is the complete runtime Role surface for each
// tenant namespace: the existing permissions plus patch on batch/jobs only.
func rbacExpectedRuntimePermissions() map[string][]string {
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

// assertRenderedRuntimeRole checks one rendered tenant Role document.
func assertRenderedRuntimeRole(t *testing.T, role map[string]any, tenant string) {
	t.Helper()
	metadata := objectAtKey(t, role, "metadata")
	if got := stringAtKey(t, metadata, "namespace"); got != "kodama-tenant-"+tenant {
		t.Errorf("runtime Role is in namespace %q, want kodama-tenant-%s", got, tenant)
	}
	labels := objectAtKey(t, metadata, "labels")
	if got := stringAtKey(t, labels, "kodama.dev/tenant"); got != tenant {
		t.Errorf("runtime Role tenant label = %q, want %q", got, tenant)
	}

	permissions := rbacPermissionMap(t, role)
	expected := rbacExpectedRuntimePermissions()
	for key, wantVerbs := range expected {
		gotVerbs, present := permissions[key]
		if !present {
			t.Errorf("runtime Role does not grant %q", key)
			continue
		}
		if strings.Join(rbacSorted(gotVerbs), ",") != strings.Join(rbacSorted(wantVerbs), ",") {
			t.Errorf("runtime Role grants %q verbs %v, want %v", key, gotVerbs, wantVerbs)
		}
		delete(permissions, key)
	}
	for key, verbs := range permissions {
		t.Errorf("runtime Role grants unexpected %q with verbs %v", key, verbs)
	}
}

// assertRenderedRuntimeRBAC checks the complete rendered RBAC surface of one
// chart render: per-tenant Roles and RoleBindings plus the namespace-reader
// ClusterRole, with no cluster-wide workload permission.
func assertRenderedRuntimeRBAC(t *testing.T, rendered string, tenants []string, serviceAccountName, releaseNamespace string) {
	t.Helper()

	roles := rbacObjectsOfKind(t, rendered, "Role")
	if len(roles) != len(tenants) {
		t.Fatalf("rendered %d runtime Roles, want one per tenant (%d):\n%s", len(roles), len(tenants), rendered)
	}
	bindings := rbacObjectsOfKind(t, rendered, "RoleBinding")
	if len(bindings) != len(tenants) {
		t.Fatalf("rendered %d runtime RoleBindings, want one per tenant (%d):\n%s", len(bindings), len(tenants), rendered)
	}
	if got := len(rbacObjectsOfKind(t, rendered, "ClusterRole")); got != 1 {
		t.Fatalf("rendered %d ClusterRoles, want only the namespace reader", got)
	}
	if got := len(rbacObjectsOfKind(t, rendered, "ClusterRoleBinding")); got != 1 {
		t.Fatalf("rendered %d ClusterRoleBindings, want only the namespace reader", got)
	}

	tenantNamespaces := make(map[string]bool, len(tenants))
	for _, tenant := range tenants {
		tenantNamespaces["kodama-tenant-"+tenant] = true
	}
	for _, role := range roles {
		namespace := stringAtKey(t, objectAtKey(t, role, "metadata"), "namespace")
		if !tenantNamespaces[namespace] {
			t.Errorf("rendered runtime Role is in unexpected namespace %q", namespace)
		}
	}

	for _, tenant := range tenants {
		namespace := "kodama-tenant-" + tenant
		assertRenderedRuntimeRole(t, rbacObjectNamed(t, rendered, "Role", "kodama-runtime", namespace), tenant)

		binding := rbacObjectNamed(t, rendered, "RoleBinding", "kodama-runtime", namespace)
		roleRef := objectAtKey(t, binding, "roleRef")
		if got := stringAtKey(t, roleRef, "apiGroup"); got != "rbac.authorization.k8s.io" {
			t.Errorf("RoleBinding roleRef apiGroup = %q", got)
		}
		if got := stringAtKey(t, roleRef, "kind"); got != "Role" {
			t.Errorf("RoleBinding roleRef kind = %q, want Role", got)
		}
		if got := stringAtKey(t, roleRef, "name"); got != "kodama-runtime" {
			t.Errorf("RoleBinding roleRef name = %q, want kodama-runtime", got)
		}
		subjects := sliceAtKey(t, binding, "subjects")
		if len(subjects) != 1 {
			t.Fatalf("RoleBinding has %d subjects, want exactly the installation ServiceAccount", len(subjects))
		}
		subject, ok := subjects[0].(map[string]any)
		if !ok {
			t.Fatalf("RoleBinding subject is not an object: %T", subjects[0])
		}
		if got := stringAtKey(t, subject, "kind"); got != "ServiceAccount" {
			t.Errorf("RoleBinding subject kind = %q, want ServiceAccount", got)
		}
		if got := stringAtKey(t, subject, "name"); got != serviceAccountName {
			t.Errorf("RoleBinding subject name = %q, want %q", got, serviceAccountName)
		}
		if got := stringAtKey(t, subject, "namespace"); got != releaseNamespace {
			t.Errorf("RoleBinding subject namespace = %q, want %q", got, releaseNamespace)
		}
	}

	clusterRole := rbacObjectsOfKind(t, rendered, "ClusterRole")[0]
	metadata := objectAtKey(t, clusterRole, "metadata")
	if got := stringAtKey(t, metadata, "name"); got != "kodama-namespace-reader-"+releaseNamespace {
		t.Errorf("ClusterRole name = %q, want the release-scoped namespace reader", got)
	}
	clusterPermissions := rbacPermissionMap(t, clusterRole)
	if len(clusterPermissions) != 1 {
		t.Fatalf("ClusterRole grants %v, want only Namespace get", clusterPermissions)
	}
	if verbs, present := clusterPermissions["/namespaces"]; !present {
		t.Errorf("ClusterRole does not grant Namespace get: %v", clusterPermissions)
	} else if strings.Join(rbacSorted(verbs), ",") != "get" {
		t.Errorf("ClusterRole grants Namespace verbs %v, want only get", verbs)
	}
	clusterRules := sliceAtKey(t, clusterRole, "rules")
	namespaceRule, ok := clusterRules[0].(map[string]any)
	if !ok {
		t.Fatalf("ClusterRole rule is not an object: %T", clusterRules[0])
	}
	resourceNames := rbacStringList(t, namespaceRule, "resourceNames")
	sortedNames := rbacSorted(resourceNames)
	wantNames := make([]string, 0, len(tenants))
	for _, tenant := range tenants {
		wantNames = append(wantNames, "kodama-tenant-"+tenant)
	}
	if strings.Join(sortedNames, ",") != strings.Join(rbacSorted(wantNames), ",") {
		t.Errorf("ClusterRole reads namespaces %v, want exactly %v", resourceNames, wantNames)
	}

	clusterBinding := rbacObjectsOfKind(t, rendered, "ClusterRoleBinding")[0]
	subjects := sliceAtKey(t, clusterBinding, "subjects")
	if len(subjects) != 1 {
		t.Fatalf("ClusterRoleBinding has %d subjects, want exactly the installation ServiceAccount", len(subjects))
	}
	subject, ok := subjects[0].(map[string]any)
	if !ok {
		t.Fatalf("ClusterRoleBinding subject is not an object: %T", subjects[0])
	}
	if got := stringAtKey(t, subject, "name"); got != serviceAccountName {
		t.Errorf("ClusterRoleBinding subject name = %q, want %q", got, serviceAccountName)
	}
	if got := stringAtKey(t, subject, "namespace"); got != releaseNamespace {
		t.Errorf("ClusterRoleBinding subject namespace = %q, want %q", got, releaseNamespace)
	}
}

// assertNoForbiddenRBACWidening rejects wildcard verbs, resources and API
// groups anywhere in the rendered output, plus any Job status or pod mutation
// grant that the exact permission assertion could not see by name.
func assertNoForbiddenRBACWidening(t *testing.T, rendered string) {
	t.Helper()
	for _, kind := range []string{"Role", "ClusterRole"} {
		for _, object := range rbacObjectsOfKind(t, rendered, kind) {
			for _, raw := range sliceAtKey(t, object, "rules") {
				rule, ok := raw.(map[string]any)
				if !ok {
					t.Fatalf("%s rule is not an object: %T", kind, raw)
				}
				for _, key := range []string{"apiGroups", "resources", "verbs"} {
					for _, value := range rbacStringList(t, rule, key) {
						if value == "*" {
							t.Errorf("rendered %s grants a wildcard %s", kind, key)
						}
						if strings.Contains(value, "jobs/status") {
							t.Errorf("rendered %s grants jobs/status", kind)
						}
						if strings.Contains(value, "pods/finalizers") || strings.Contains(value, "pods/exec") || strings.Contains(value, "pods/attach") {
							t.Errorf("rendered %s grants pod mutation %q", kind, value)
						}
					}
				}
				if kind == "ClusterRole" {
					for _, resource := range rbacStringList(t, rule, "resources") {
						if resource != "namespaces" {
							t.Errorf("ClusterRole grants cluster-wide resource %q", resource)
						}
					}
				}
			}
		}
	}
}

// TestKodamaChartsRenderExactRuntimeJobPermissions renders the standalone and
// stack charts with two tenants and a custom ServiceAccount and pins the
// complete runtime permission sets.
func TestKodamaChartsRenderExactRuntimeJobPermissions(t *testing.T) {
	tenants := []string{"tenant-a", "tenant-b"}

	t.Run("kodama", func(t *testing.T) {
		helm, charts := stagedCharts(t)
		chart := filepath.Join(charts, "kodama")
		rendered, err := renderChartArgs(t, helm, "kodama", chart, kodamaProductionArgs(
			"--set", "tenants[0]=tenant-a",
			"--set", "tenants[1]=tenant-b",
			"--set", "serviceAccount.name=kodama-custom",
		)...)
		if err != nil {
			t.Fatalf("helm template kodama: %v\n%s", err, rendered)
		}
		assertRenderedRuntimeRBAC(t, rendered, tenants, "kodama-custom", "kodama-system")
		assertNoForbiddenRBACWidening(t, rendered)
	})

	t.Run("kodama-stack", func(t *testing.T) {
		helm, stack := stageStackChart(t)
		rendered, err := renderChartArgs(t, helm, "kodama-stack", stack, stackProductionArgs(
			"--set", "kodama.tenants[0]=tenant-a",
			"--set", "kodama.tenants[1]=tenant-b",
			"--set", "kodama.serviceAccount.name=kodama-custom",
		)...)
		if err != nil {
			t.Fatalf("helm template kodama-stack: %v\n%s", err, rendered)
		}
		assertRenderedRuntimeRBAC(t, rendered, tenants, "kodama-custom", "kodama-system")
		assertNoForbiddenRBACWidening(t, rendered)
	})
}
