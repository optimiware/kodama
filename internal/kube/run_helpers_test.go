package kube_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/kube/kubetest"
)

// runEnv is the fixture of the run-resource adapter: a fake cluster whose tenant
// namespace holds the run_shared baseline, a policy catalog that retains the run's
// pinned policy, the registrar and checkout doubles and a controllable clock.
type runEnv struct {
	t      *testing.T
	fake   *kubetest.Fake
	policy kube.ExecutionPolicy
	ns     string
	body   execution.ExecutionPolicy
	specs  kubetest.RunSpecs
	cat    *kubetest.Catalog
	reg    *kubetest.ResourceRegistrar
	chk    *kubetest.Checkout
	clock  *testClock
	rr     *kube.RunResources
}

func newRunEnv(t *testing.T, body execution.ExecutionPolicy, mod ...func(*kube.RunResourceDependencies, *kube.RunResourceOptions)) *runEnv {
	t.Helper()
	pol := kubetest.Policy(t, "a")
	id, tgt := kubetest.Tenant("a")
	e := &runEnv{t: t, fake: kubetest.NewFake(), policy: pol, ns: tgt.Namespace, body: body, reg: kubetest.NewResourceRegistrar(), chk: kubetest.NewCheckout(t),
		clock: &testClock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}}
	e.fake.Now = e.clock.Now
	kubetest.RunBaseline(e.fake, pol, id, body.PodSecurity)
	e.fake.PutStorageClass(body.Workspace.StorageClass, "Immediate", "Delete")
	e.cat, _ = kubetest.NewCatalog(t, body)
	e.specs = kubetest.NewRunSpecs(t, body)
	e.rr = e.build(mod...)
	return e
}

// build creates a fresh adapter over the same cluster: a controller restart.
func (e *runEnv) build(mod ...func(*kube.RunResourceDependencies, *kube.RunResourceOptions)) *kube.RunResources {
	e.t.Helper()
	d := kube.RunResourceDependencies{Policy: e.policy, Cluster: e.fake, Catalog: e.cat, Registrar: e.reg, Checkout: e.chk}
	o := kube.RunResourceOptions{Options: kube.Options{Clock: e.clock.Now}}
	for _, m := range mod {
		m(&d, &o)
	}
	rr, err := kube.NewRunResources(d, o)
	if err != nil {
		e.t.Fatal(err)
	}
	return rr
}

func tenantBody() execution.ExecutionPolicy { return executiontest.TenantPolicy() }
func runBody() execution.ExecutionPolicy    { return executiontest.TestPolicy() }

func (e *runEnv) ensure(spec execution.ResourceSpec) execution.ResourceObservation {
	e.t.Helper()
	o, err := e.rr.Ensure(context.Background(), spec)
	if err != nil {
		e.t.Fatalf("ensure %s: %v", spec.Key, err)
	}
	return o
}

func (e *runEnv) ensureErr(spec execution.ResourceSpec) error {
	_, err := e.rr.Ensure(context.Background(), spec)
	return err
}

func (e *runEnv) observe(h execution.ResourceHandle) execution.ResourceObservation {
	e.t.Helper()
	o, err := e.rr.Observe(context.Background(), h)
	if err != nil {
		e.t.Fatalf("observe %s: %v", h.Key, err)
	}
	return o
}

func (e *runEnv) stop(h execution.ResourceHandle) execution.ResourceStopEvidence {
	e.t.Helper()
	ev, err := e.rr.Stop(context.Background(), h)
	if err != nil {
		e.t.Fatalf("stop %s: %v", h.Key, err)
	}
	return ev
}

func (e *runEnv) remove(h execution.ResourceHandle) execution.ResourceRemoveEvidence {
	e.t.Helper()
	ev, err := e.rr.Remove(context.Background(), h)
	if err != nil {
		e.t.Fatalf("remove %s: %v", h.Key, err)
	}
	return ev
}

func handleOf(o execution.ResourceObservation) execution.ResourceHandle { return o.Handle }

// afterQuiescence moves the clock past the create quiescence.
func (e *runEnv) afterQuiescence() { e.clock.Advance(3 * time.Minute) }

func requireIs(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error %v, want %v", err, want)
	}
}

func jobsAndPods(e *runEnv) (jobs, pods []string) {
	return e.fake.Names("job", e.ns), e.fake.Names("pod", e.ns)
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ = kubernetes.ErrNotFound

// newCatalogWithout is a catalog that retains no policy at all.
func newCatalogWithout(t *testing.T) (*kubetest.Catalog, []execution.PolicyPin) {
	return kubetest.NewCatalog(t)
}

// liveSecrets lists the Secrets that are not removal markers: records and
// credentials. A removed resource leaves only its marker, which holds no data.
func liveSecrets(e *runEnv) []string {
	var out []string
	for _, n := range e.fake.Names("secret", e.ns) {
		if strings.HasSuffix(n, "-tomb") {
			sec, _ := e.fake.SecretOf(e.ns, n)
			if len(sec.Data) != 0 {
				e.t.Fatalf("a removal marker holds data: %s", n)
			}
			continue
		}
		out = append(out, n)
	}
	return out
}
