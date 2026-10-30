package kube_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

// Test_unit_kube_resource_capabilities: the adapter advertises exactly what it
// renders and verifies for every run, never a builder service or an isolated
// worker, and builder_rootless only when the installation vouches for it after
// qualification. Its backend is the attempt runtime's.
func Test_unit_kube_resource_capabilities(t *testing.T) {
	ctx := context.Background()
	e := newRunEnv(t, tenantBody())
	c := e.rr.Capabilities(ctx)
	if c.Backend != kube.Backend || c.Backend != (&kube.Runtime{}).Capabilities().Backend {
		t.Fatalf("backend %q", c.Backend)
	}
	want := execution.Set(
		execution.CapRunWorkspace, execution.CapWorkspaceFence, execution.CapWorkspacePreparation, execution.CapResourceRecovery,
		execution.CapRunServices, execution.CapServiceIsolation, execution.CapRunNetworkIsolation, execution.CapDeclaredEdges,
		execution.CapNamedEgress, execution.CapEgressDefaultDeny,
	)
	if c.Set != want {
		t.Fatalf("set %s, want %s", c.Set, want)
	}
	for _, never := range []execution.ResourceCapability{execution.CapBuilderService, execution.CapBuilderIsolatedWorker, execution.CapBuilderRootless} {
		if c.Set.Has(never) {
			t.Fatalf("advertises %v without qualification", execution.Set(never))
		}
	}
	if missing := c.Set.Missing(execution.NormalResourceRequirements); missing != 0 {
		t.Fatalf("a run with nothing declared needs %s", missing)
	}
	// The attempt runtime and the companion port agree on the backend and the set is
	// stable across restarts.
	if again := e.build().Capabilities(ctx); again != c {
		t.Fatal("capabilities changed across a restart")
	}

	// builder_rootless comes only from the installation's explicit vouching.
	q := e.build(func(_ *kube.RunResourceDependencies, o *kube.RunResourceOptions) {
		o.Qualified = execution.Set(execution.CapBuilderRootless)
	})
	if got := q.Capabilities(ctx).Set; !got.Has(execution.CapBuilderRootless) || got.Has(execution.CapBuilderService) || got.Has(execution.CapBuilderIsolatedWorker) {
		t.Fatalf("qualified set %s", got)
	}
	for _, bad := range []execution.ResourceCapabilitySet{
		execution.Set(execution.CapBuilderService), execution.Set(execution.CapBuilderIsolatedWorker), execution.Set(execution.CapRunWorkspace, execution.CapBuilderRootless),
	} {
		_, err := kube.NewRunResources(kube.RunResourceDependencies{Policy: e.policy, Cluster: e.fake, Catalog: e.cat}, kube.RunResourceOptions{Qualified: bad})
		if err == nil {
			t.Fatalf("the installation vouched for %s", bad)
		}
	}
}

// Test_unit_kube_run_resources_constructor: the adapter has no allow-all default.
func Test_unit_kube_run_resources_constructor(t *testing.T) {
	e := newRunEnv(t, tenantBody())
	cases := map[string]kube.RunResourceDependencies{
		"no policy":  {Cluster: e.fake, Catalog: e.cat},
		"no cluster": {Policy: e.policy, Catalog: e.cat},
		"no catalog": {Policy: e.policy, Cluster: e.fake},
	}
	for name, d := range cases {
		if _, err := kube.NewRunResources(d, kube.RunResourceOptions{}); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	for _, cidr := range []string{"nonsense", "0.0.0.0/0", "2001:db8::/32"} {
		_, err := kube.NewRunResources(kube.RunResourceDependencies{Policy: e.policy, Cluster: e.fake, Catalog: e.cat}, kube.RunResourceOptions{ClusterCIDRs: []string{cidr}})
		if err == nil {
			t.Fatalf("cluster CIDR %q accepted", cidr)
		}
	}
	// A spec of another backend, tenant or role shape is refused before any effect.
	spec := e.specs.Workspace(t)
	other := spec
	other.Backend = "podman"
	if err := e.ensureErr(other); err == nil {
		t.Fatal("another backend's spec accepted")
	}
	shape := spec
	shape.RoleShape = "sha256:" + string(make([]byte, 0)) + "0000000000000000000000000000000000000000000000000000000000000000"
	if err := e.ensureErr(shape); err == nil {
		t.Fatal("a spec with another role shape accepted")
	}
	stranger := spec
	stranger.Scope.TenantID = "t021_unknown"
	stranger.Name = execution.ResourceName(stranger.Scope, stranger.RunID, stranger.Kind, stranger.Key)
	if err := e.ensureErr(stranger); err == nil {
		t.Fatal("a tenant without an execution target accepted")
	}
	if len(e.fake.Names("secret", e.ns)) != 0 {
		t.Fatal("effects after refusals")
	}
	_ = kubetest.RunID
}
