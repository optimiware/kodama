package kube_full_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/test/support/stage"
)

// TestKubeChartInstall is kube_chart_install: a clean install of the committed chart reaches
// readiness under KODAMA_ENV=production and --assembly core, migration uses the migration role,
// the tenant namespace carries the baseline, and a bounded database-path interruption recovers.
func TestKubeChartInstall(t *testing.T) {
	i := stage.New(t)
	ctx := i.Context(12 * time.Minute)
	if i.Installed(ctx) {
		if err := i.Uninstall(ctx); err != nil {
			t.Fatalf("uninstall: %v", err)
		}
	}
	if err := i.Install(ctx); err != nil {
		t.Fatalf("install: %v", err)
	}
	i.WaitReady(ctx)

	deploy, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "get", "deploy", i.Deployment(), "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(deploy, `"server"`) || !strings.Contains(deploy, `"--assembly"`) || !strings.Contains(deploy, `"core"`) {
		t.Fatal("the server is not running kodama server --assembly core")
	}
	if !strings.Contains(deploy, `"KODAMA_ENV"`) || !strings.Contains(deploy, `"production"`) {
		t.Fatal("KODAMA_ENV=production is not set")
	}
	// The hook Job is not a durable record (Helm may remove it). The schema existing, applied so the
	// runtime role can use it and cannot own it, is the migration evidence.
	if n := i.OwnerCount(`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'tenants'`); n != 1 {
		t.Fatalf("migration did not apply the schema (tenants table count %d)", n)
	}
	var runtimeSelect bool
	i.MustScan(&runtimeSelect, `SELECT has_table_privilege('kodama_runtime', 'tenants', 'SELECT')`)
	if !runtimeSelect {
		t.Fatal("the runtime role cannot read tenant tables")
	}
	var runtimeOwner bool
	i.MustScan(&runtimeOwner, `SELECT pg_get_userbyid(c.relowner) = 'kodama_runtime' FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = 'tenants'`)
	if runtimeOwner {
		t.Fatal("the runtime role owns the schema; migration must run as the migration role")
	}

	resp, body, err := i.TryRaw(http.MethodGet, "/healthz", "", nil, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %v %s", err, body)
	}
	resp, body, err = i.TryRaw(http.MethodGet, "/readyz", "", nil, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz: %v %s", err, body)
	}
	me, err := i.API("admin").GetMeWithResponse(ctx)
	if err != nil || me.StatusCode() != http.StatusOK || me.JSON200 == nil || me.JSON200.Id == "" {
		t.Fatalf("/api/v1/me: %v", err)
	}

	ns := i.S.TenantA().Namespace
	raw, err := i.Kube.Run(ctx, "get", "ns", ns, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"pod-security.kubernetes.io/enforce": "restricted"`) {
		t.Fatal("tenant namespace does not enforce restricted")
	}
	if !strings.Contains(raw, `"kodama.optimi.com/tenant": "`+i.S.TenantA().TenantID+`"`) {
		t.Fatal("tenant namespace is not annotated for its tenant")
	}
	pol, err := i.Kube.Run(ctx, "-n", ns, "get", "netpol", "default-deny", "-o", "jsonpath={.spec.policyTypes}")
	if err != nil || !strings.Contains(pol, "Ingress") || !strings.Contains(pol, "Egress") {
		t.Fatalf("default-deny: %v %s", err, pol)
	}

	i.RelayDown(ctx)
	stage.WaitFor(t, "readiness to fail while the database path is down", 90*time.Second, func() bool {
		resp, _, err := i.TryRaw(http.MethodGet, "/readyz", "", nil, nil)
		return err != nil || resp.StatusCode != http.StatusOK
	})
	i.RelayUp(ctx)
	i.WaitReady(i.Context(4 * time.Minute))
}
