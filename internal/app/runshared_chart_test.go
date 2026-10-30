package app

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

// runSharedOverlay enables run_shared the way the stage installation does:
// admission open, one cluster range, and the record Secrets the run resources need.
func runSharedOverlay(extra ...object) []object {
	return append([]object{{
		"runShared": object{"enabled": true, "nodeProfilesInstalled": true, "admission": object{"runShared": "open", "legacyFormat": "open"}, "clusterCIDRs": []any{"10.32.0.0/12"}},
		"execution": object{"rbac": object{"manageSecrets": true}},
	}}, extra...)
}

func settingsDocument(t testing.TB, docs []object) string {
	t.Helper()
	for _, cm := range ofKind(docs, "ConfigMap") {
		if strings.HasSuffix(nameOf(cm), "-config") {
			return text(dig(cm, "data", "config.yaml"))
		}
	}
	t.Fatal("no settings document rendered")
	return ""
}

func ruleFor(role object, group, resource string) (object, bool) {
	for _, r := range asList(role["rules"]) {
		rule := asObject(r)
		if !hasString(stringList(rule["apiGroups"]), group) {
			continue
		}
		if hasString(stringList(rule["resources"]), resource) {
			return rule, true
		}
	}
	return nil, false
}

func hasString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Test_unit_chart_run_shared_render: with runShared disabled (the default) the
// chart renders none of the model and the tenant namespaces keep the fixed
// restricted baseline. Enabled, it renders the settings section the server reads
// strictly (the stage policy of O-30 with the T-120 profiles, the run_shared step
// identity, the digest-pinned preparation image), tenant namespaces at Pod
// Security baseline with a pinned version, the PVC/Service quota additions and
// the controller RBAC; the rendered values feed the real LoadCoreConfig, the
// execution policy and the namespace preflight, and controls break one property each.
func Test_unit_chart_run_shared_render(t *testing.T) {
	// Disabled by default: nothing of the model is rendered.
	off := chartValues(t, nil)
	if err := validateChartValues(t, off); err != nil {
		t.Fatal(err)
	}
	offDocs := newChartRenderer(t, off).objects()
	if strings.Contains(settingsDocument(t, offDocs), "runShared") {
		t.Fatal("the default render carries the run_shared section")
	}
	for _, ns := range ofKind(offDocs, "Namespace") {
		if text(dig(ns, "metadata", "labels", "pod-security.kubernetes.io/enforce")) != "restricted" {
			t.Fatalf("namespace %s is not restricted by default", nameOf(ns))
		}
	}
	for _, q := range ofKind(offDocs, "ResourceQuota") {
		if _, ok := asObject(dig(q, "spec", "hard"))["persistentvolumeclaims"]; ok {
			t.Fatal("the default quota bounds claims")
		}
	}
	for _, r := range ofKind(offDocs, "ClusterRole") {
		if _, ok := ruleFor(r, "", "persistentvolumes"); ok {
			t.Fatal("the default cluster role reads volumes")
		}
	}

	values := chartValues(t, nil, runSharedOverlay()...)
	if err := validateChartValues(t, values); err != nil {
		t.Fatalf("the enabled values do not follow the schema: %v", err)
	}
	docs := newChartRenderer(t, values).objects()
	doc := settingsDocument(t, docs)

	// Tenant namespaces: baseline at the pinned version (enforce, audit, warn).
	for _, ns := range ofKind(docs, "Namespace") {
		labels := asObject(dig(ns, "metadata", "labels"))
		for _, mode := range []string{"enforce", "audit", "warn"} {
			if text(labels["pod-security.kubernetes.io/"+mode]) != "baseline" || text(labels["pod-security.kubernetes.io/"+mode+"-version"]) != "v1.31" {
				t.Errorf("namespace %s %s labels: %v", nameOf(ns), mode, labels)
			}
		}
	}
	// Quota additions.
	for _, q := range ofKind(docs, "ResourceQuota") {
		hard := asObject(dig(q, "spec", "hard"))
		if toInt(hard["persistentvolumeclaims"]) != 20 || text(hard["requests.storage"]) != "80Gi" || toInt(hard["services"]) != 20 || toInt(hard["pods"]) != 20 {
			t.Errorf("quota %v", hard)
		}
	}
	// Controller RBAC: tenant namespaces and the cluster-scoped reads.
	for _, r := range ofKind(docs, "Role") {
		for _, want := range []struct{ group, resource, verb string }{
			{"", "persistentvolumeclaims", "create"}, {"", "persistentvolumeclaims", "delete"}, {"", "services", "create"}, {"", "pods", "create"},
			{"networking.k8s.io", "networkpolicies", "create"}, {"networking.k8s.io", "networkpolicies", "delete"}, {"networking.k8s.io", "networkpolicies", "get"},
		} {
			found := false
			for _, rule := range asList(r["rules"]) {
				ru := asObject(rule)
				if hasString(stringList(ru["apiGroups"]), want.group) && hasString(stringList(ru["resources"]), want.resource) && hasString(stringList(ru["verbs"]), want.verb) {
					found = true
				}
			}
			if !found {
				t.Errorf("role %s lacks %s on %s/%s", nameOf(r), want.verb, want.group, want.resource)
			}
		}
		if rule, ok := ruleFor(r, "", "secrets"); !ok || !hasString(stringList(rule["verbs"]), "update") {
			t.Errorf("role %s cannot keep resource records", nameOf(r))
		}
	}
	var clusterRole object
	for _, r := range ofKind(docs, "ClusterRole") {
		clusterRole = r
	}
	for _, want := range []struct{ group, resource, verb string }{
		{"", "persistentvolumes", "get"}, {"", "persistentvolumes", "list"}, {"storage.k8s.io", "volumeattachments", "list"}, {"storage.k8s.io", "storageclasses", "get"},
	} {
		rule, ok := ruleFor(clusterRole, want.group, want.resource)
		if !ok || !hasString(stringList(rule["verbs"]), want.verb) {
			t.Errorf("the cluster role lacks %s on %s", want.verb, want.resource)
		}
		if ok && (hasString(stringList(rule["verbs"]), "create") || hasString(stringList(rule["verbs"]), "delete") || hasString(stringList(rule["verbs"]), "update")) {
			t.Errorf("the cluster role writes %s", want.resource)
		}
	}

	// The settings document: strict parse, the section and the policy list.
	parsed, err := ParseChartConfig([]byte(doc))
	if err != nil {
		t.Fatalf("the rendered document does not parse: %v", err)
	}
	rs, err := parsed.RunShared()
	if err != nil || rs == nil {
		t.Fatalf("the run_shared section: %v %v", rs, err)
	}
	if len(rs.Policies) != 1 || rs.BuilderRootless || len(rs.ClusterCIDRs) != 1 {
		t.Fatalf("section: %+v", rs)
	}
	cur := rs.Current()
	releaseImage := fmt.Sprintf("%s:%s@%s", text(dig(values, "image", "repository")), text(dig(values, "image", "tag")), text(dig(values, "image", "digest")))
	step := cur.Roles[execution.RoleStep]
	switch {
	case cur.ID != "run-v1", cur.Admission.RunShared != execution.PolicyOpen, cur.Admission.LegacyFormat != execution.PolicyOpen:
		t.Errorf("admission/id: %+v", cur.Admission)
	case cur.Network.Isolation != execution.IsolationTenant || !cur.Network.PublicEgress:
		t.Errorf("network: %+v", cur.Network)
	case step.User != "nonroot" || step.UserNamespace != "pod" || strings.Join(step.CapabilitiesAdd, ",") != "SETUID,SETGID" ||
		step.Seccomp != "kodama-rootless-build-v1" || step.LSM != "kodama-rootless-build-v1" || step.ProcMount != "" || len(step.Devices) != 0:
		t.Errorf("step role: %+v", step)
	case cur.Roles[execution.RolePreparation].Image != releaseImage:
		t.Errorf("the preparation image is %q, not the digest-pinned release image", cur.Roles[execution.RolePreparation].Image)
	case cur.Workspace.StorageClass != "sbs-default" || cur.Workspace.Access != "ReadWriteOncePod" || cur.Workspace.Reclaim != "Delete":
		t.Errorf("workspace: %+v", cur.Workspace)
	case cur.PodSecurity.Level != "baseline" || cur.PodSecurity.Version != "v1.31":
		t.Errorf("pod security: %+v", cur.PodSecurity)
	case cur.Builders.Service.Mode != "none" || strings.Join(cur.Builders.Rootless, ",") != "ko,buildah":
		t.Errorf("builders: %+v", cur.Builders)
	}
	if !cur.RunSharedOpen() {
		t.Error("the policy does not admit run_shared though admission is open")
	}

	// The real consumers: the server's startup configuration, the execution
	// policy (with the run_shared identity) and the namespace preflight.
	s := materialize(t, docs)
	core, _ := LoadCoreConfig(s.getenv, Config{Assembly: AssemblyCore, ShutdownTimeout: time.Second})
	k := core.Runtime.Kubernetes
	if k.Policy == nil || k.RunShared == nil {
		t.Fatalf("the server did not load the policy and the run_shared section: %+v", k)
	}
	if id := k.Policy.RunShared; id == nil || id.RunAsUser != 1000 || id.RunAsGroup != 1000 || id.FSGroup != 1000 || k.Policy.RunAsUser != 65532 {
		t.Fatalf("identity: %+v (legacy %d)", id, k.Policy.RunAsUser)
	}
	policy, err := kube.NewExecutionPolicy(*k.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Runtime.Kubernetes.RunShared.Validate(); err != nil {
		t.Fatal(err)
	}
	// Under production the configuration validates; the same section under the
	// test environment is refused (run_shared is composed on Kubernetes only).
	core.Env = Production
	if errs := core.validateRuntime(); len(errs) != 0 {
		t.Errorf("production runtime validation: %v", errs)
	}
	core.Env = Test
	refused := false
	for _, e := range core.validateRuntime() {
		refused = refused || strings.Contains(e.Error(), "run_shared is configured")
	}
	if !refused {
		t.Error("the test environment accepted run_shared")
	}

	fake := kubetest.NewFake()
	convert := func(d object, out any) {
		t.Helper()
		b, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: %v", nameOf(d), err)
		}
	}
	for _, d := range ofKind(docs, "Namespace") {
		var ns kubernetes.Namespace
		convert(d, &ns)
		ns.Status.Phase = "Active"
		fake.Namespaces[ns.Metadata.Name] = ns
	}
	for _, d := range ofKind(docs, "ServiceAccount") {
		if ns := text(dig(d, "metadata", "namespace")); ns != "" {
			var sa kubernetes.ServiceAccount
			convert(d, &sa)
			fake.ServiceAccounts[ns+"/"+sa.Metadata.Name] = sa
		}
	}
	for _, d := range ofKind(docs, "ResourceQuota") {
		hard := object{}
		for k, v := range asObject(dig(d, "spec", "hard")) {
			hard[k] = fmt.Sprint(v)
		}
		d["spec"] = object{"hard": hard}
		var q kubernetes.ResourceQuota
		convert(d, &q)
		ns := text(dig(d, "metadata", "namespace"))
		fake.Quotas[ns] = append(fake.Quotas[ns], q)
	}
	for _, d := range ofKind(docs, "NetworkPolicy") {
		var np kubernetes.NetworkPolicy
		convert(d, &np)
		ns := text(dig(d, "metadata", "namespace"))
		fake.NetPols[ns] = append(fake.NetPols[ns], np)
	}
	pre := kube.NewPreflight(fake, policy, 0, nil)
	for _, tn := range asList(dig(values, "execution", "tenants")) {
		id := domain.TenantID(text(asObject(tn)["tenantId"]))
		if err := pre.CheckRun(context.Background(), id, cur.PodSecurity); err != nil {
			t.Errorf("tenant %s: the run_shared namespace is refused by the preflight: %v", id, err)
		}
		// The attempt_private preflight requires `restricted`: a baseline namespace
		// ends attempt_private dispatch (fail closed), so the cutover is a drained
		// switch and the chart enables the model per installation, never silently.
		if err := pre.Check(context.Background(), id); err == nil {
			t.Errorf("tenant %s: the legacy restricted preflight accepted a baseline namespace", id)
		}
	}

	// Controls: each breaks one property the chart provides.
	t.Run("a version other than the pinned one is refused", func(t *testing.T) {
		other := cur.PodSecurity
		other.Version = "v1.30"
		id := domain.TenantID(text(asObject(asList(dig(values, "execution", "tenants"))[0])["tenantId"]))
		if err := pre.CheckRun(context.Background(), id, other); err == nil {
			t.Fatal("the preflight accepted a Pod Security version the namespace does not enforce")
		}
	})
	t.Run("the schema refuses what the model must never take", func(t *testing.T) {
		for name, overlay := range map[string]object{
			"the latest Pod Security version": {"runShared": object{"podSecurity": object{"version": "latest"}}},
			"an unknown legacy format":        {"runShared": object{"admission": object{"legacyFormat": "retired"}}},
			"root identity":                   {"runShared": object{"identity": object{"runAsUser": 0}}},
			"an extra capability":             {"runShared": object{"policy": object{"roles": object{"step": object{"capabilitiesAdd": []any{"SYS_ADMIN"}}}}}},
			"an unpinned preparation image":   {"runShared": object{"policy": object{"roles": object{"preparationImage": "registry.example.invalid/prep:latest"}}}},
			"an unknown builder":              {"runShared": object{"policy": object{"builders": object{"rootless": []any{"kaniko"}}}}},
			"an unknown policy member":        {"runShared": object{"policy": object{"privileged": true}}},
		} {
			if err := validateChartValues(t, chartValues(t, nil, append(runSharedOverlay(), overlay)...)); err == nil {
				t.Errorf("%s was accepted", name)
			}
		}
	})
	t.Run("the node profiles must be attested", func(t *testing.T) {
		v := chartValues(t, nil, runSharedOverlay(object{"runShared": object{"nodeProfilesInstalled": false}})...)
		probe := &renderProbe{TB: t}
		done := make(chan struct{})
		go func() {
			defer close(done)
			newChartRenderer(probe, v)
		}()
		<-done
		if !strings.Contains(probe.failure, "node profiles") {
			t.Fatalf("the chart rendered run_shared without the node profiles attested: %q", probe.failure)
		}
		// A step role with no named profile needs none.
		plain := chartValues(t, nil, runSharedOverlay(object{"runShared": object{"nodeProfilesInstalled": false, "policy": object{"roles": object{"step": object{"seccomp": "RuntimeDefault", "lsm": ""}}}}})...)
		newChartRenderer(t, plain)
	})
	t.Run("record Secrets are mandatory", func(t *testing.T) {
		v := chartValues(t, nil, runSharedOverlay(object{"execution": object{"rbac": object{"manageSecrets": false}}})...)
		probe := &renderProbe{TB: t}
		done := make(chan struct{})
		go func() {
			defer close(done)
			newChartRenderer(probe, v)
		}()
		<-done
		if !strings.Contains(probe.failure, "manageSecrets") {
			t.Fatalf("the chart rendered run_shared without record Secrets: %q", probe.failure)
		}
	})
}

// renderProbe captures a template failure (helm's `fail`) instead of ending the test.
type renderProbe struct {
	testing.TB
	failure string
}

func (p *renderProbe) Fatalf(format string, args ...any) {
	p.failure = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// TestChartRunShared is Test_unit_chart_run_shared_render under the name `make test-chart` selects.
func TestChartRunShared(t *testing.T) { Test_unit_chart_run_shared_render(t) }
