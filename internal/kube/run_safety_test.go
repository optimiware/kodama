package kube_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

func outage(op string) func(string, int) *kubetestFault {
	return func(o string, _ int) *kubetestFault {
		if o == op {
			return &kubetestFault{Err: &kubernetes.Error{Op: o, Kind: kubernetes.ErrUnavailable}}
		}
		return nil
	}
}

func foreign(name string) kubernetes.ObjectMeta {
	return kubernetes.ObjectMeta{Name: name, Labels: map[string]string{"app": "someone-elses"}}
}

// Test_unit_kube_resource_collision_never_adopted: a same-name object that is not
// the resource's is a collision. It is never adopted, stopped, modified or
// deleted, whether it has no record, a record of another spec or another UID, or
// copies every label and annotation of ours.
func Test_unit_kube_resource_collision_never_adopted(t *testing.T) {
	ctx := context.Background()

	t.Run("foreign objects at the resource names", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		ws := e.ensure(e.specs.Workspace(t))
		_ = ws
		prep, svc := e.specs.Preparation(t), e.specs.Service(t, "db", false)
		other := e.specs.Workspace(t)
		other.RunID = "run_other"
		other.Name = execution.ResourceName(other.Scope, other.RunID, other.Kind, other.Key)
		oe := newRunEnv(t, tenantBody())
		pvc := oe.fake.PutPVC(oe.ns, kubernetes.PersistentVolumeClaim{Metadata: foreign(oe.specs.Workspace(t).Name)})
		job := e.fake.PutJob(e.ns, kubernetes.Job{Metadata: foreign(prep.Name)})
		pod := e.fake.PutPod(e.ns, kubernetes.Pod{Metadata: foreign(svc.Name)})
		for name, c := range map[string]struct {
			e    *runEnv
			spec execution.ResourceSpec
			uid  string
		}{
			"claim": {oe, oe.specs.Workspace(t), pvc.Metadata.UID},
			"job":   {e, prep, job.Metadata.UID},
			"pod":   {e, svc, pod.Metadata.UID},
		} {
			secrets := len(c.e.fake.Names("secret", c.e.ns))
			err := c.e.ensureErr(c.spec)
			requireIs(t, err, execution.ErrResourceCollision)
			if len(c.e.fake.Names("secret", c.e.ns)) != secrets || len(c.e.reg.Calls) != 0 {
				t.Fatalf("%s: a collision left a record or a registration", name)
			}
			h := c.spec.Handle("")
			if _, err := c.e.rr.Observe(ctx, h); !errors.Is(err, execution.ErrResourceCollision) {
				t.Fatalf("%s: observe %v", name, err)
			}
			// Stop and Remove fence the resource and touch nothing of someone else's.
			if ev := c.e.stop(h); !ev.Stopped {
				t.Fatalf("%s: a resource that never existed must stop: %+v", name, ev)
			}
			if got := c.e.remove(h); !got.Removed {
				t.Fatalf("%s: remove %+v", name, got)
			}
		}
		if c, ok := oe.fake.PVCOf(oe.ns, oe.specs.Workspace(t).Name); !ok || c.Metadata.UID != pvc.Metadata.UID || c.Metadata.DeletionTimestamp != "" || c.Metadata.Labels["app"] != "someone-elses" {
			t.Fatal("the foreign claim was touched")
		}
		if j, ok := e.fake.JobOf(e.ns, prep.Name); !ok || j.Metadata.UID != job.Metadata.UID || j.Metadata.DeletionTimestamp != "" {
			t.Fatal("the foreign Job was touched")
		}
		if p, ok := e.fake.PodOf(e.ns, svc.Name); !ok || p.Metadata.UID != pod.Metadata.UID {
			t.Fatal("the foreign Pod was touched")
		}
		if e.fake.Calls("delete job")+e.fake.Calls("delete pod")+oe.fake.Calls("delete pvc") != 0 {
			t.Fatal("a foreign object was deleted")
		}
	})

	t.Run("record of another spec", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Workspace(t)
		o := e.ensure(spec)
		bigger := spec
		w := *spec.Workspace
		w.SizeMiB = 4096
		bigger.Workspace = &w
		requireIs(t, e.ensureErr(bigger), execution.ErrResourceCollision)
		if c, _ := e.fake.PVCOf(e.ns, spec.Name); c.Metadata.UID != o.Handle.UID || c.Spec.Resources.Requests["storage"] != "2048Mi" || e.fake.Calls("create pvc") != 1 {
			t.Fatal("the existing claim was modified or replaced")
		}
	})

	t.Run("bound instance replaced by an object that copies our marks", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Workspace(t)
		o := e.ensure(spec)
		copied, _ := e.fake.PVCOf(e.ns, spec.Name)
		e.fake.DropPVC(e.ns, spec.Name)
		copied.Metadata.UID = ""
		repl := e.fake.PutPVC(e.ns, copied) // every label, annotation and spec field copied, a new UID
		if _, err := e.rr.Observe(ctx, o.Handle); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("observe %v", err)
		}
		// Ensure refuses the replacement too (the record binds the original UID).
		requireIs(t, e.ensureErr(spec), execution.ErrResourceCollision)
		if _, err := e.rr.Stop(ctx, o.Handle); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("stop %v", err)
		}
		if _, err := e.rr.Remove(ctx, o.Handle); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("remove %v", err)
		}
		if c, ok := e.fake.PVCOf(e.ns, spec.Name); !ok || c.Metadata.UID != repl.Metadata.UID || c.Metadata.DeletionTimestamp != "" {
			t.Fatal("the replacement was deleted")
		}
	})

	t.Run("handle of another instance", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Workspace(t)
		o := e.ensure(spec)
		h := o.Handle
		h.UID = "uid-someone-elses"
		if _, err := e.rr.Observe(ctx, h); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("observe %v", err)
		}
		if _, err := e.rr.Stop(ctx, h); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("stop %v", err)
		}
		if c, ok := e.fake.PVCOf(e.ns, spec.Name); !ok || c.Metadata.UID != o.Handle.UID {
			t.Fatal("claim touched")
		}
		// A handle of another run or tenant names other objects entirely.
		h2 := o.Handle
		h2.RunID = "run_other"
		if _, err := e.rr.Observe(ctx, h2); err == nil {
			t.Fatal("a handle whose name is not its run's was accepted")
		}
	})

	t.Run("an object of ours with a changed spec", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		svc := e.specs.Service(t, "db", false)
		o := e.ensure(svc)
		e.fake.MutateRaw("pod", e.ns, svc.Name, func(d map[string]any) {
			d["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"] = "registry.example.invalid/evil@sha256:" + "e"[:1] + "0000000000000000000000000000000000000000000000000000000000000000"[1:]
		})
		if _, err := e.rr.Observe(ctx, o.Handle); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("observe %v", err)
		}
		requireIs(t, e.ensureErr(svc), execution.ErrResourceCollision)
	})

	t.Run("a foreign endpoint is not ours to adopt or delete", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		svc := e.specs.Service(t, "db", false)
		host := execution.EndpointHost(svc.Scope, svc.RunID, "db")
		f := e.fake.PutService(e.ns, kubernetes.Service{Metadata: foreign(host)})
		requireIs(t, e.ensureErr(svc), execution.ErrResourceCollision)
		h := svc.Handle("")
		pod, _ := e.fake.PodOf(e.ns, svc.Name)
		h.UID = pod.Metadata.UID
		// The Pod was created before the endpoint collided; stopping removes ours and
		// leaves the foreign Service untouched.
		for i := 0; i < 2; i++ {
			e.stop(h)
		}
		if _, ok := e.fake.PodOf(e.ns, svc.Name); ok {
			t.Fatal("our Pod survived the stop")
		}
		if got, ok := e.fake.ServiceOf(e.ns, host); !ok || got.Metadata.UID != f.Metadata.UID {
			t.Fatal("the foreign Service was deleted")
		}
		if got := e.remove(h); !got.Removed {
			t.Fatalf("a foreign Service blocks removal: %+v", got)
		}
	})

	t.Run("a foreign record Secret", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Workspace(t)
		e.fake.PutSecret(e.ns, kubernetes.Secret{Metadata: foreign(spec.Name + "-rec")})
		requireIs(t, e.ensureErr(spec), execution.ErrResourceCollision)
		if names := e.fake.Names("pvc", e.ns); len(names) != 0 {
			t.Fatal("created against a foreign record")
		}
		if _, err := e.rr.Stop(ctx, spec.Handle("")); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("stop %v", err)
		}
	})

	t.Run("record of another resource at the name", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Workspace(t)
		other := e.specs.Workspace(t)
		other.RunID = "run_other"
		other.Name = execution.ResourceName(other.Scope, other.RunID, other.Kind, other.Key)
		e.ensure(other)
		sec, _ := e.fake.SecretOf(e.ns, other.Name+"-rec")
		sec.Metadata.Name = spec.Name + "-rec"
		e.fake.PutSecret(e.ns, sec)
		requireIs(t, e.ensureErr(spec), execution.ErrResourceCollision)
	})
}

// Test_unit_kube_resource_unknown_is_never_absent: Observe reports absent only for
// a definite not-found of the object (and its record); an outage, an unreadable
// policy or an unverifiable object is unknown, and a create that may still land is
// neither absent nor stopped.
func Test_unit_kube_resource_unknown_is_never_absent(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		spec func(*runEnv) execution.ResourceSpec
		op   string
	}{
		{"claim", func(e *runEnv) execution.ResourceSpec { return e.specs.Workspace(t) }, "get pvc"},
		{"pod", func(e *runEnv) execution.ResourceSpec { return e.specs.Service(t, "db", false) }, "get pod"},
		{"job", func(e *runEnv) execution.ResourceSpec {
			return e.specs.Preparation(t)
		}, "get job"},
		{"network", func(e *runEnv) execution.ResourceSpec { return e.specs.Network(t, nil, nil, nil) }, "get networkpolicy"},
		{"record", func(e *runEnv) execution.ResourceSpec { return e.specs.Workspace(t) }, "get secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRunEnv(t, tenantBody())
			if tc.name == "job" {
				e.ensure(e.specs.Workspace(t))
			}
			spec := tc.spec(e)
			o := e.ensure(spec)
			e.fake.Fault = outage(tc.op)
			got, err := e.rr.Observe(ctx, o.Handle)
			if err != nil || got.Phase != execution.PhaseUnknown || got.Phase == execution.PhaseAbsent {
				t.Fatalf("outage: %+v %v", got, err)
			}
			if got.Fingerprint != "" || got.Released {
				t.Fatalf("an unknown observation carries evidence: %+v", got)
			}
			e.fake.Fault = nil
			if got := e.observe(o.Handle); got.Phase == execution.PhaseUnknown || got.Phase == execution.PhaseAbsent {
				t.Fatalf("after recovery: %+v", got)
			}
		})
	}

	t.Run("policy unavailable", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		o := e.ensure(e.specs.Workspace(t))
		e.cat.Err = errors.New("catalog down")
		e.rr = e.build() // no cached body
		if got := e.observe(o.Handle); got.Phase != execution.PhaseUnknown {
			t.Fatalf("catalog outage: %+v", got)
		}
		e.cat.Err = nil
		e.cat, _ = newCatalogWithout(t)
		e.rr = e.build()
		if got := e.observe(o.Handle); got.Phase != execution.PhaseUnknown || got.Reason != execution.ReasonPolicyUnavailable {
			t.Fatalf("unretained policy: %+v", got)
		}
		if err := e.ensureErr(e.specs.Workspace(t)); !errors.Is(err, kube.ErrPolicyUnavailable) {
			t.Fatalf("ensure without the pinned policy: %v", err)
		}
	})

	t.Run("definite absence", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Workspace(t)
		o := e.ensure(spec)
		e.fake.DropPVC(e.ns, spec.Name)
		if got := e.observe(o.Handle); got.Phase != execution.PhaseAbsent {
			t.Fatalf("a claim that is definitely gone: %+v", got)
		}
		// Never planned: nothing exists.
		fresh := newRunEnv(t, tenantBody())
		if got := fresh.observe(fresh.specs.Workspace(t).Handle("")); got.Phase != execution.PhaseAbsent {
			t.Fatalf("never created: %+v", got)
		}
	})

	t.Run("a create that may still land", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Workspace(t)
		e.fake.Fault = func(op string, n int) *kubetestFault {
			if op == "create pvc" {
				return &kubetestFault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}}
			}
			return nil
		}
		if err := e.ensureErr(spec); !errors.Is(err, kube.ErrCreateUncertain) {
			t.Fatalf("ensure %v", err)
		}
		e.fake.Fault = nil
		h := spec.Handle("")
		if got := e.observe(h); got.Phase != execution.PhaseUnknown {
			t.Fatalf("an unconfirmed create is neither absent nor pending: %+v", got)
		}
		e.afterQuiescence()
		if got := e.observe(h); got.Phase != execution.PhaseAbsent {
			t.Fatalf("after quiescence: %+v", got)
		}
	})

	t.Run("stop and remove conclude nothing from an outage", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		svc := e.specs.Service(t, "db", false)
		o := e.ensure(svc)
		e.fake.Fault = outage("get pod")
		if ev, err := e.rr.Stop(ctx, o.Handle); err == nil && ev.Stopped {
			t.Fatalf("stopped on an outage: %+v", ev)
		}
		e.fake.Fault = nil
		e.stop(o.Handle)
		e.fake.Fault = outage("get pod")
		if ev, err := e.rr.Remove(ctx, o.Handle); err == nil && ev.Removed {
			t.Fatal("removed on an outage")
		}
	})
}

// Test_unit_kube_stop_fences_ensure: after Stop no Ensure creates the resource
// again, whether or not it ever existed; a stop racing a create refuses the
// create; Stop is idempotent and verified.
func Test_unit_kube_stop_fences_ensure(t *testing.T) {
	ctx := context.Background()
	e := newRunEnv(t, tenantBody())
	spec := e.specs.Workspace(t)
	h := spec.Handle("")
	// Stopped before it ever existed: a durable fence, so a late Ensure is refused.
	if ev := e.stop(h); !ev.Stopped || ev.Launch != execution.LaunchUnknown {
		t.Fatalf("stop of the unplanned %+v", ev)
	}
	requireIs(t, e.ensureErr(spec), execution.ErrResourceFenced)
	if len(e.fake.Names("pvc", e.ns)) != 0 || e.fake.Calls("create pvc") != 0 {
		t.Fatal("created after the fence")
	}
	if got := e.observe(h); got.Phase != execution.PhaseStopped {
		t.Fatalf("a fenced tombstone: %+v", got)
	}
	if got := e.remove(h); !got.Removed {
		t.Fatalf("remove %+v", got)
	}
	// Remove before stop is refused.
	e2 := newRunEnv(t, tenantBody())
	o := e2.ensure(e2.specs.Service(t, "db", false))
	if _, err := e2.rr.Remove(ctx, o.Handle); !errors.Is(err, execution.ErrResourceFenced) {
		t.Fatalf("remove before stop: %v", err)
	}
	if _, ok := e2.fake.PodOf(e2.ns, o.Handle.Name); !ok {
		t.Fatal("a Pod was removed before it was stopped")
	}
	// A stop that lands while the create is in flight (cancellation racing the
	// create): the create is not reported as an owned resource, the late Pod is
	// found and deleted by the next stop, and the stop is not reported complete
	// until a create can no longer land.
	e3 := newRunEnv(t, tenantBody())
	svc := e3.specs.Service(t, "db", false)
	h3 := svc.Handle("")
	raced := false
	e3.fake.Fault = func(op string, n int) *kubetestFault {
		if op == "create pod" && !raced {
			raced = true
			if ev, err := e3.rr.Stop(ctx, h3); err != nil || ev.Stopped {
				t.Errorf("a stop racing a create reported %+v %v", ev, err)
			}
		}
		return nil
	}
	if err := e3.ensureErr(svc); !errors.Is(err, execution.ErrResourceFenced) {
		t.Fatalf("an Ensure overtaken by a stop: %v", err)
	}
	e3.fake.Fault = nil
	if _, ok := e3.fake.PodOf(e3.ns, svc.Name); !ok {
		t.Fatal("the racing create did not land in this scenario")
	}
	if ev := e3.stop(h3); ev.Stopped {
		t.Fatalf("stopped before a create can no longer land: %+v", ev)
	}
	if _, ok := e3.fake.PodOf(e3.ns, svc.Name); ok {
		t.Fatal("the late Pod was not deleted")
	}
	e3.afterQuiescence()
	if ev := e3.stop(h3); !ev.Stopped {
		t.Fatalf("stop %+v", ev)
	}
	if e3.fake.Calls("create pod") != 1 {
		t.Fatal("a second Pod was created")
	}
}

// Test_unit_kube_run_lost_responses: a mutation whose response was lost after it
// was applied is resolved by reading: Ensure adopts what exists, never creates a
// second object and never reports the resource unowned.
func Test_unit_kube_run_lost_responses(t *testing.T) {
	lose := func(op string, nth int) func(string, int) *kubetestFault {
		return func(o string, n int) *kubetestFault {
			if o == op && n == nth {
				return &kubetestFault{Err: &kubernetes.Error{Op: o, Kind: kubernetes.ErrUnknownOutcome}, After: true}
			}
			return nil
		}
	}
	cases := []struct {
		name string
		op   string
		nth  int
		spec func(*runEnv) execution.ResourceSpec
		pre  func(*runEnv)
	}{
		{"record", "create secret", 1, func(e *runEnv) execution.ResourceSpec { return e.specs.Service(t, "db", false) }, nil},
		{"record update", "update secret", 1, func(e *runEnv) execution.ResourceSpec { return e.specs.Service(t, "db", false) }, nil},
		{"pod", "create pod", 1, func(e *runEnv) execution.ResourceSpec { return e.specs.Service(t, "db", false) }, nil},
		{"service", "create service", 1, func(e *runEnv) execution.ResourceSpec { return e.specs.Service(t, "db", false) }, nil},
		{"ingress policy", "create networkpolicy", 1, func(e *runEnv) execution.ResourceSpec { return e.specs.Service(t, "db", false) }, nil},
		{"job", "create job", 1, func(e *runEnv) execution.ResourceSpec { return e.specs.Preparation(t) }, func(e *runEnv) { e.ensure(e.specs.Workspace(t)) }},
		{"network policy", "create networkpolicy", 2, func(e *runEnv) execution.ResourceSpec {
			return e.specs.Network(t, nil, nil, map[string][]string{"build": {"registry"}})
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRunEnv(t, stageBody())
			if tc.pre != nil {
				tc.pre(e)
			}
			spec := tc.spec(e)
			e.fake.Fault = lose(tc.op, tc.nth)
			o, err := e.rr.Ensure(context.Background(), spec)
			if err != nil {
				// A lost record update may surface as one retry round; the next Ensure converges.
				if o, err = e.rr.Ensure(context.Background(), spec); err != nil {
					t.Fatalf("ensure after a lost response: %v", err)
				}
			}
			if o.Handle.UID == "" || o.Fingerprint == "" {
				t.Fatalf("not bound: %+v", o)
			}
			e.fake.Fault = nil
			again := e.ensure(spec)
			if again.Handle.UID != o.Handle.UID {
				t.Fatalf("the instance changed: %s -> %s", o.Handle.UID, again.Handle.UID)
			}
			for _, op := range []string{"create pod", "create job", "create service", "create pvc"} {
				if e.fake.Calls(op) > 1 {
					t.Fatalf("%s issued %d times", op, e.fake.Calls(op))
				}
			}
			if names := e.fake.Names("secret", e.ns); len(names) < 1 || len(names) > 2 {
				t.Fatalf("records %v", names)
			}
		})
	}
}
