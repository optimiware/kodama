package gitworker_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kube/gitworker"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

func noDeletes(t *testing.T, e *env) {
	t.Helper()
	for _, c := range e.f.Mutations() {
		if strings.HasPrefix(c, "delete ") {
			t.Fatalf("a reconciliation deleted something: %s", c)
		}
	}
}

// Test_unit_gitworker_volume_retained_on_replace (T-129,
// unit_gitworker_volume_retained_on_replace, GR-04, GR-A03): replacing the worker,
// losing the Deployment, failing reconciliations and repeating them never delete,
// recreate or replace the tenant's claim, volume or namespace; only the explicit
// OffboardTenant, behind the retention gate, removes the claim.
func Test_unit_gitworker_volume_retained_on_replace(t *testing.T) {
	ns := nsOf(acme)

	t.Run("replacing the worker keeps the volume", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		claim, _ := e.f.pvc(ns)
		nsObj, _ := e.f.namespace(ns)
		pvs := len(e.f.pvs)
		dep, _ := e.f.deployment(ns)
		e.f.ResetCalls()

		for _, image := range []string{testImage2, testImage, testImage2} {
			e.spec.Image = image
			st := e.reconcile(acme)
			if !st.Ready || len(st.Actions) != 1 || st.Actions[0] != "replaced worker deployment" {
				t.Fatalf("%+v", st)
			}
		}
		for _, c := range e.f.Mutations() {
			if c != "replace deployment" {
				t.Fatalf("a worker replacement did more than replace the Deployment: %v", e.f.Mutations())
			}
		}
		noDeletes(t, e)
		after, _ := e.f.pvc(ns)
		afterNS, _ := e.f.namespace(ns)
		afterDep, _ := e.f.deployment(ns)
		if after.Metadata.UID != claim.Metadata.UID || after.Spec.VolumeName != claim.Spec.VolumeName || len(e.f.pvs) != pvs ||
			afterNS.Metadata.UID != nsObj.Metadata.UID || afterDep.Metadata.UID != dep.Metadata.UID {
			t.Fatal("the volume, claim or namespace changed with the worker")
		}
		// The new Pod runs the Recreate strategy: exactly one Pod at the end.
		if len(e.f.pods) != 1 {
			t.Fatalf("pods %d", len(e.f.pods))
		}
		if e.f.Calls("create pvc") != 0 {
			t.Fatal("the claim was created again")
		}
	})

	t.Run("a lost Deployment is recreated over the same claim", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		claim, _ := e.f.pvc(ns)
		e.f.mu.Lock()
		delete(e.f.deps, key(ns, gitworker.DeploymentName))
		for k := range e.f.pods {
			delete(e.f.pods, k)
		}
		e.f.mu.Unlock()
		e.f.ResetCalls()
		st := e.reconcile(acme)
		if !st.Ready || len(st.Actions) != 1 || st.Actions[0] != "created worker deployment" {
			t.Fatalf("%+v", st)
		}
		after, _ := e.f.pvc(ns)
		if after.Metadata.UID != claim.Metadata.UID || e.f.Calls("create pvc") != 0 {
			t.Fatal("the claim was not kept")
		}
		noDeletes(t, e)
	})

	t.Run("failures and refusals never delete", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		claim, _ := e.f.pvc(ns)
		e.f.ResetCalls()
		e.f.putPolicy(ns, kubernetes.NetworkPolicy{Metadata: kubernetes.ObjectMeta{Name: "open"}})
		e.id.err = errors.New("down")
		for range 3 {
			_, _ = e.try(acme)
		}
		e.id.err = nil
		for range 3 {
			if _, err := e.try(acme); !isErr(err, gitworker.ErrIsolation) {
				t.Fatalf("%v", err)
			}
		}
		e.f.classes["fast"] = kubernetes.StorageClass{Provisioner: "csi.test", ReclaimPolicy: "Delete"}
		if _, err := e.try(acme); !isErr(err, gitworker.ErrStorage) {
			t.Fatalf("%v", err)
		}
		noDeletes(t, e)
		after, ok := e.f.pvc(ns)
		if !ok || after.Metadata.UID != claim.Metadata.UID {
			t.Fatal("the claim is gone")
		}
	})

	t.Run("offboarding is gated by retention", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		claim, _ := e.f.pvc(ns)
		for name, setup := range map[string]func(){
			"retention not elapsed": func() { e.ret.allow, e.ret.err = false, nil },
			"retention unknown":     func() { e.ret.allow, e.ret.err = true, errors.New("store down") },
		} {
			setup()
			err := e.rec.OffboardTenant(context.Background(), acme)
			if !isErr(err, gitworker.ErrRetention) {
				t.Fatalf("%s: %v", name, err)
			}
			if e.f.Calls("delete namespace") != 0 {
				t.Fatalf("%s: the namespace was deleted", name)
			}
		}
		if _, ok := e.f.pvc(ns); !ok {
			t.Fatal("the claim is gone")
		}
		// Without a configured gate the operation is refused as well.
		bare, err := gitworker.New(gitworker.Dependencies{Cluster: e.f, Identity: e.id}, gitworker.Options{RWOPProvisioners: []string{"csi.test"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := bare.OffboardTenant(context.Background(), acme); !isErr(err, gitworker.ErrRetention) {
			t.Fatalf("no gate: %v", err)
		}

		e.ret.allow, e.ret.err = true, nil
		if err := e.rec.OffboardTenant(context.Background(), acme); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.f.namespace(ns); ok {
			t.Fatal("the namespace survived")
		}
		if _, ok := e.f.pvc(ns); ok {
			t.Fatal("the claim survived its namespace")
		}
		// A retained volume is released by the storage administrator, not by Kodama.
		if _, ok := e.f.pvs["pv-"+claim.Metadata.UID]; !ok {
			t.Fatal("the retained volume was removed by Kodama")
		}
		if _, err := e.rec.Endpoint(context.Background(), acme); !isErr(err, gitworker.ErrWorkerUnavailable) {
			t.Fatalf("an offboarded tenant has an endpoint: %v", err)
		}
		if err := e.rec.OffboardTenant(context.Background(), acme); err != nil {
			t.Fatalf("offboarding is not idempotent: %v", err)
		}
	})

	t.Run("offboarding deletes only a verified namespace, under its UID", func(t *testing.T) {
		e := newEnv(t)
		e.ret.allow = true
		e.f.putNamespace(kubernetes.Namespace{Metadata: kubernetes.ObjectMeta{Name: ns}})
		if err := e.rec.OffboardTenant(context.Background(), acme); !isErr(err, gitworker.ErrForeign) {
			t.Fatalf("a foreign namespace: %v", err)
		}
		if _, ok := e.f.namespace(ns); !ok || e.f.Calls("delete namespace") != 0 {
			t.Fatal("a foreign namespace was deleted")
		}

		e2 := newEnv(t)
		e2.ret.allow = true
		e2.reconcile(acme)
		// The namespace is replaced between the read and the delete.
		e2.f.fail["delete namespace"] = &kubernetes.Error{Op: "delete namespace", Kind: kubernetes.ErrConflict}
		if err := e2.rec.OffboardTenant(context.Background(), acme); !isErr(err, gitworker.ErrForeign) {
			t.Fatalf("a replaced namespace: %v", err)
		}
		e2.f.fail["delete namespace"] = &kubernetes.Error{Op: "delete namespace", Kind: kubernetes.ErrUnknownOutcome}
		if err := e2.rec.OffboardTenant(context.Background(), acme); !isErr(err, gitworker.ErrUnavailable) {
			t.Fatalf("a lost delete: %v", err)
		}
		if _, ok := e2.f.namespace(ns); !ok {
			t.Fatal("namespace vanished")
		}
	})

	t.Run("another tenant's hosting is not affected", func(t *testing.T) {
		e := newEnv(t)
		e.ret.allow = true
		e.reconcile(acme)
		e.reconcile("t_beta")
		if err := e.rec.OffboardTenant(context.Background(), acme); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.f.pvc(nsOf("t_beta")); !ok {
			t.Fatal("offboarding one tenant removed another's volume")
		}
		if _, err := e.rec.Endpoint(context.Background(), "t_beta"); err != nil {
			t.Fatalf("%v", err)
		}
	})

	t.Run("concurrent reconciliations of a tenant create once", func(t *testing.T) {
		e := newEnv(t)
		var wg sync.WaitGroup
		for range 6 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := e.try(acme); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		for _, op := range []string{"create namespace", "create pvc", "create deployment", "create secret"} {
			if n := e.f.Calls(op); n != 1 {
				t.Fatalf("%s: %d", op, n)
			}
		}
	})
}

// Test_unit_gitworker_service_discovery (T-129, unit_gitworker_service_discovery,
// S2/S3): the control plane resolves a tenant to its worker's Service URL only
// while the last reconciliation found that worker ready and recent; every other
// state is a retryable unavailable error, and a tenant never resolves to another
// tenant's endpoint.
func Test_unit_gitworker_service_discovery(t *testing.T) {
	ctx := context.Background()
	t.Run("unknown tenants and invalid input", func(t *testing.T) {
		e := newEnv(t)
		for _, tenant := range []string{"t_acme", "", "bad id!"} {
			if _, err := e.rec.Endpoint(ctx, tid(tenant)); !isErr(err, gitworker.ErrWorkerUnavailable) {
				t.Fatalf("%q: %v", tenant, err)
			}
		}
	})

	t.Run("a ready worker resolves to its own Service", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.reconcile("t_beta")
		a, err := e.rec.Endpoint(ctx, acme)
		if err != nil {
			t.Fatal(err)
		}
		b, err := e.rec.Endpoint(ctx, "t_beta")
		if err != nil {
			t.Fatal(err)
		}
		if a.BaseURL != "https://git-worker."+nsOf(acme)+".svc:8443" || b.BaseURL != "https://git-worker."+nsOf("t_beta")+".svc:8443" || a.BaseURL == b.BaseURL {
			t.Fatalf("%q %q", a.BaseURL, b.BaseURL)
		}
		if a.BaseURL != gitworker.EndpointURL(acme, e.spec) {
			t.Fatal("the endpoint is not the rendered Service")
		}
		if _, err := e.rec.Endpoint(ctx, "t_gamma"); !isErr(err, gitworker.ErrWorkerUnavailable) {
			t.Fatalf("an unprovisioned tenant resolves: %v", err)
		}
	})

	t.Run("http workers", func(t *testing.T) {
		e := newEnv(t)
		e.spec.Scheme = "http"
		e.reconcile(acme)
		if ep, _ := e.rec.Endpoint(ctx, acme); !strings.HasPrefix(ep.BaseURL, "http://git-worker.") {
			t.Fatalf("%q", ep.BaseURL)
		}
	})

	t.Run("a worker that is not ready is unavailable", func(t *testing.T) {
		e := newEnv(t)
		e.f.setReady(false)
		st := e.reconcile(acme)
		if st.Ready {
			t.Fatal("ready")
		}
		if _, err := e.rec.Endpoint(ctx, acme); !isErr(err, gitworker.ErrWorkerUnavailable) {
			t.Fatalf("%v", err)
		}
		// Becoming ready is observed by the next pass, not before.
		e.f.setReady(true)
		e.spec.Image = testImage2
		e.reconcile(acme)
		if _, err := e.rec.Endpoint(ctx, acme); err != nil {
			t.Fatalf("%v", err)
		}
	})

	t.Run("a worker being replaced is unavailable until it is ready again", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.f.setReady(false)
		e.spec.Image = testImage2
		if st := e.reconcile(acme); st.Ready {
			t.Fatalf("%+v", st)
		}
		if _, err := e.rec.Endpoint(ctx, acme); !isErr(err, gitworker.ErrWorkerUnavailable) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("a blocked tenant is unavailable", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.f.putPod(nsOf(acme), kubernetes.Pod{Metadata: kubernetes.ObjectMeta{Name: "x"}})
		_, _ = e.try(acme)
		if _, err := e.rec.Endpoint(ctx, acme); !isErr(err, gitworker.ErrWorkerUnavailable) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("an old observation ages out; an outage alone does not flip it", func(t *testing.T) {
		e := newEnv(t)
		e.reconcile(acme)
		e.f.fail["get namespace"] = &kubernetes.Error{Op: "get namespace", Kind: kubernetes.ErrUnavailable}
		if _, err := e.try(acme); !isErr(err, gitworker.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		e.clk.Advance(time.Minute)
		if _, err := e.rec.Endpoint(ctx, acme); err != nil {
			t.Fatalf("a fresh observation was lost to an outage: %v", err)
		}
		e.clk.Advance(2 * time.Minute)
		if _, err := e.rec.Endpoint(ctx, acme); !isErr(err, gitworker.ErrWorkerUnavailable) {
			t.Fatalf("a stale observation answers: %v", err)
		}
	})

	t.Run("status is the last observation", func(t *testing.T) {
		e := newEnv(t)
		if _, ok := e.rec.Status(acme); ok {
			t.Fatal("status before any pass")
		}
		st := e.reconcile(acme)
		got, ok := e.rec.Status(acme)
		if !ok || got.Phase != gitworker.PhaseReady || got.Fingerprint != st.Fingerprint || got.Namespace != nsOf(acme) {
			t.Fatalf("%+v", got)
		}
	})
}
