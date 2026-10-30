package kube_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// pauseBefore returns a fault hook that runs fn exactly once right before the
// n-th call of op is applied: the call is then "outstanding" while fn runs.
func pauseBefore(op string, fn func()) func(string, int) *kubetestFault {
	done := false
	return func(o string, _ int) *kubetestFault {
		if o == op && !done {
			done = true
			fn()
		}
		return nil
	}
}

func inventoryEmpty(t *testing.T, e *runEnv) {
	t.Helper()
	if len(e.fake.Names("pod", e.ns))+len(e.fake.Names("service", e.ns))+len(e.fake.Names("pvc", e.ns))+len(e.fake.Names("job", e.ns))+len(liveSecrets(e)) != 0 ||
		!eqStrings(e.fake.Names("policy", e.ns), []string{"default-deny", "runtime-egress"}) {
		t.Fatalf("inventory left: pods %v services %v claims %v jobs %v secrets %v policies %v", e.fake.Names("pod", e.ns), e.fake.Names("service", e.ns),
			e.fake.Names("pvc", e.ns), e.fake.Names("job", e.ns), liveSecrets(e), e.fake.Names("policy", e.ns))
	}
}

// Test_unit_kube_companion_create_is_announced_and_fenced: a companion create
// (a service's Service or ingress policy, a network's policy) is announced in
// the record and the fence is rechecked before it is sent. A Stop and a Remove that
// run while the create is outstanding cannot complete: the resource is not
// stopped, its record stays, and the object that lands afterwards is deleted by
// the next stop pass. No orphan is left.
func Test_unit_kube_companion_create_is_announced_and_fenced(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		op   string
		nth  int
		spec func(*runEnv) execution.ResourceSpec
	}{
		{"service", "create service", 1, func(e *runEnv) execution.ResourceSpec { return e.specs.Service(t, "db", false) }},
		{"ingress policy", "create networkpolicy", 1, func(e *runEnv) execution.ResourceSpec { return e.specs.Service(t, "db", false) }},
		{"network policy", "create networkpolicy", 2, func(e *runEnv) execution.ResourceSpec {
			return e.specs.Network(t, nil, nil, map[string][]string{"build": {"registry"}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRunEnv(t, stageBody())
			spec := tc.spec(e)
			h := spec.Handle("")
			calls := 0
			stoppedInside, removedInside := false, false
			e.fake.Fault = func(op string, n int) *kubetestFault {
				if op != tc.op {
					return nil
				}
				calls++
				if calls != tc.nth {
					return nil
				}
				// The create below is outstanding: stop and remove the resource now.
				if pod, ok := e.fake.PodOf(e.ns, spec.Name); ok && spec.Kind == execution.KindService {
					h.UID = pod.Metadata.UID
				}
				ev, err := e.rr.Stop(ctx, h)
				stoppedInside = err != nil || ev.Stopped
				rm, err := e.rr.Remove(ctx, h)
				removedInside = err != nil && !errors.Is(err, execution.ErrResourceFenced) || rm.Removed
				return nil
			}
			err := e.ensureErr(spec)
			requireIs(t, err, execution.ErrResourceFenced)
			if stoppedInside || removedInside {
				t.Fatalf("a stop or removal completed while a create was outstanding: stopped=%v removed=%v", stoppedInside, removedInside)
			}
			if _, ok := e.fake.SecretOf(e.ns, spec.Name+"-rec"); !ok {
				t.Fatal("the record was deleted while a create was outstanding")
			}
			e.fake.Fault = nil
			// The create landed after the stop pass: the next passes find and delete it.
			if ev := e.stop(h); ev.Stopped {
				t.Fatalf("stopped before the create could no longer land: %+v", ev)
			}
			if rm, _ := e.rr.Remove(ctx, h); rm.Removed {
				t.Fatal("removed before the create could no longer land")
			}
			e.afterQuiescence()
			if ev := e.stop(h); !ev.Stopped {
				t.Fatalf("stop %+v", ev)
			}
			if rm := e.remove(h); !rm.Removed {
				t.Fatalf("remove %+v", rm)
			}
			inventoryEmpty(t, e)
		})
	}
}

// Test_unit_kube_removed_resource_is_never_created_again: Remove leaves a durable,
// credential-free removal marker, so a new adapter instance (a restarted
// controller) refuses to create a removed resource, whatever its kind; Observe says
// absent, Stop and Remove repeat harmlessly.
func Test_unit_kube_removed_resource_is_never_created_again(t *testing.T) {
	ctx := context.Background()
	e := newRunEnv(t, stageBody())
	// The workspace goes last: the preparation needs it while it is ensured.
	type named struct {
		name string
		spec execution.ResourceSpec
	}
	order := []named{
		{"plain service", e.specs.Service(t, "db", false)},
		{"supervised service", e.specs.Service(t, "app", true)},
		{"network", e.specs.Network(t, nil, nil, map[string][]string{"build": {"registry"}})},
		{"preparation", e.specs.Preparation(t)},
		{"workspace", e.specs.Workspace(t)},
	}
	e.ensure(order[4].spec)
	for _, tc := range order {
		spec := tc.spec
		t.Run(tc.name, func(t *testing.T) {
			o := e.ensure(spec)
			h := o.Handle
			e.stop(h)
			for i := 0; ; i++ {
				if rm := e.remove(h); rm.Removed {
					break
				}
				if i > 3 {
					t.Fatal("not removed")
				}
				e.afterQuiescence()
				e.stop(h)
			}
			creates := e.fake.Calls("create pvc") + e.fake.Calls("create pod") + e.fake.Calls("create job") + e.fake.Calls("create service") + e.fake.Calls("create networkpolicy")
			secrets := e.fake.Calls("create secret")
			fresh := e.build() // a restarted controller
			if _, err := fresh.Ensure(ctx, spec); !errors.Is(err, execution.ErrResourceFenced) {
				t.Fatalf("a removed resource was ensured again: %v", err)
			}
			if got := e.fake.Calls("create pvc") + e.fake.Calls("create pod") + e.fake.Calls("create job") + e.fake.Calls("create service") + e.fake.Calls("create networkpolicy"); got != creates || e.fake.Calls("create secret") != secrets {
				t.Fatal("objects were created for a removed resource")
			}
			if obs, err := fresh.Observe(ctx, h); err != nil || obs.Phase != execution.PhaseAbsent {
				t.Fatalf("observe %+v %v", obs, err)
			}
			if ev, err := fresh.Stop(ctx, h); err != nil || !ev.Stopped {
				t.Fatalf("stop %+v %v", ev, err)
			}
			if rm, err := fresh.Remove(ctx, h); err != nil || !rm.Removed {
				t.Fatalf("remove %+v %v", rm, err)
			}
			if _, ok := e.fake.SecretOf(e.ns, spec.Name+"-rec"); ok {
				t.Fatal("the record (and its credential) outlived the removal")
			}
			tomb, ok := e.fake.SecretOf(e.ns, spec.Name+"-tomb")
			if !ok || len(tomb.Data) != 0 {
				t.Fatalf("the removal marker is missing or holds data: %v", tomb.Data)
			}
		})
	}
}

// Test_unit_kube_release_is_decided_by_claim_identity: a claim object that is gone,
// vanishes or is replaced between the reads never counts as released while a Pod
// still references it or a volume of it is attached.
func Test_unit_kube_release_is_decided_by_claim_identity(t *testing.T) {
	ctx := context.Background()
	t.Run("absent claim with an attached volume", func(t *testing.T) {
		for _, recorded := range []bool{true, false} {
			e := newRunEnv(t, tenantBody())
			spec := e.specs.Workspace(t)
			o := e.ensure(spec)
			pvc, _ := e.fake.PVCOf(e.ns, spec.Name)
			if recorded {
				e.stop(o.Handle) // records the volume name
			}
			e.fake.DropPVC(e.ns, spec.Name)
			e.fake.Attach(pvc.Spec.VolumeName)
			ev := e.stop(o.Handle)
			if !ev.Stopped || ev.Released {
				t.Fatalf("recorded=%v: an attached volume of an absent claim: %+v", recorded, ev)
			}
			if rm := e.remove(o.Handle); rm.Removed {
				t.Fatal("removed while the volume of the claim is attached")
			}
			e.fake.Detach(pvc.Spec.VolumeName)
			if ev := e.stop(o.Handle); !ev.Released {
				t.Fatalf("recorded=%v: after the detach: %+v", recorded, ev)
			}
		}
	})
	t.Run("absent claim with a Pod that references it", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Workspace(t)
		o := e.ensure(spec)
		e.fake.DropPVC(e.ns, spec.Name)
		claimUsedBy(e, "straggler", spec.Name)
		if ev := e.stop(o.Handle); ev.Released {
			t.Fatalf("released with a Pod referencing the claim: %+v", ev)
		}
	})
	t.Run("claim vanishing between the reads", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Workspace(t)
		o := e.ensure(spec)
		e.fake.Fault = pauseBefore("list pods", func() { e.fake.DropPVC(e.ns, spec.Name) })
		if got := e.observe(o.Handle); got.Released {
			t.Fatalf("a claim that vanished between the reads is released: %+v", got)
		}
	})
	t.Run("claim replaced between the reads", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Workspace(t)
		o := e.ensure(spec)
		e.fake.Fault = pauseBefore("list pods", func() {
			c, _ := e.fake.PVCOf(e.ns, spec.Name)
			e.fake.DropPVC(e.ns, spec.Name)
			c.Metadata.UID = ""
			e.fake.PutPVC(e.ns, c)
		})
		if got := e.observe(o.Handle); got.Released {
			t.Fatalf("a claim replaced between the reads is released: %+v", got)
		}
		// Stop reads the same way.
		e2 := newRunEnv(t, tenantBody())
		o2 := e2.ensure(e2.specs.Workspace(t))
		e2.fake.Fault = pauseBefore("list pods", func() { e2.fake.DropPVC(e2.ns, o2.Handle.Name) })
		if ev, err := e2.rr.Stop(ctx, o2.Handle); err != nil || ev.Released {
			t.Fatalf("stop: %+v %v", ev, err)
		}
	})
}

// Test_unit_kube_volume_is_found_by_claim_reference: a volume of an arbitrary name
// bound to the claim (namespace, name and UID) is found, keeps cleanup pending and
// is fenced by its attachments; a volume of the same claim name but another UID is
// not ours and blocks nothing; an unreadable volume list concludes nothing.
func Test_unit_kube_volume_is_found_by_claim_reference(t *testing.T) {
	ctx := context.Background()
	e := newRunEnv(t, tenantBody())
	wffc(e)
	spec := e.specs.Workspace(t)
	o := e.ensure(spec)
	e.stop(o.Handle)
	e.fake.PutPV("pv-arbitrary-name", e.ns, spec.Name, o.Handle.UID)
	e.fake.PutPV("pv-of-an-earlier-claim", e.ns, spec.Name, "00000000-0000-4000-8000-0000000000dd")
	e.fake.Attach("pv-arbitrary-name")
	if ev := e.stop(o.Handle); ev.Released {
		t.Fatalf("released while a volume bound to the claim is attached: %+v", ev)
	}
	if rm := e.remove(o.Handle); rm.Removed {
		t.Fatal("removed while a volume bound to the claim exists")
	}
	e.fake.Detach("pv-arbitrary-name")
	e.fake.HoldPVs = true
	if rm := e.remove(o.Handle); rm.Removed {
		t.Fatal("removed while a volume bound to the claim exists")
	}
	e.fake.Fault = outage("list persistentvolumes")
	if rm, err := e.rr.Remove(ctx, o.Handle); err == nil && rm.Removed {
		t.Fatal("removed on an unreadable volume list")
	}
	e.fake.Fault = nil
	// The volume goes with its claim reference; the earlier claim's volume is not ours.
	e.fake.DropPV("pv-arbitrary-name")
	if rm := e.remove(o.Handle); !rm.Removed {
		t.Fatalf("remove %+v", rm)
	}
}

// Test_unit_kube_cleanup_never_deletes_lookalikes: with no committed UID (a create
// whose outcome was never confirmed) and for companions with no recorded UID, an
// object that copies every label and annotation of ours but differs in its
// controlled spec is not ours: Stop and Remove neither delete nor wait for it.
func Test_unit_kube_cleanup_never_deletes_lookalikes(t *testing.T) {
	ctx := context.Background()
	copyOf := func(m kubernetes.ObjectMeta) kubernetes.ObjectMeta {
		out := kubernetes.ObjectMeta{Name: m.Name, Labels: map[string]string{}, Annotations: map[string]string{}}
		for k, v := range m.Labels {
			out.Labels[k] = v
		}
		for k, v := range m.Annotations {
			out.Annotations[k] = v
		}
		return out
	}
	// unconfirmed plans a resource whose create never landed (an outcome lost with no
	// trace), and returns a twin environment whose object is copied in.
	unconfirmed := func(t *testing.T, op string, spec func(*runEnv) execution.ResourceSpec, pre func(*runEnv)) (*runEnv, *runEnv, execution.ResourceSpec) {
		e, twin := newRunEnv(t, tenantBody()), newRunEnv(t, tenantBody())
		for _, x := range []*runEnv{e, twin} {
			if pre != nil {
				pre(x)
			}
		}
		sp := spec(e)
		twin.ensure(sp)
		e.fake.Fault = func(o string, n int) *kubetestFault {
			if o == op {
				return &kubetestFault{Err: &kubernetes.Error{Op: o, Kind: kubernetes.ErrUnknownOutcome}}
			}
			return nil
		}
		if err := e.ensureErr(sp); !errors.Is(err, kube.ErrCreateUncertain) {
			t.Fatalf("ensure %v", err)
		}
		e.fake.Fault = nil
		return e, twin, sp
	}
	settle := func(t *testing.T, e *runEnv, h execution.ResourceHandle) {
		t.Helper()
		e.stop(h)
		e.afterQuiescence()
		if ev := e.stop(h); !ev.Stopped {
			t.Fatalf("a lookalike blocks the stop: %+v", ev)
		}
		if rm := e.remove(h); !rm.Removed {
			t.Fatalf("a lookalike blocks the removal: %+v", rm)
		}
	}

	t.Run("claim", func(t *testing.T) {
		e, twin, sp := unconfirmed(t, "create pvc", func(e *runEnv) execution.ResourceSpec { return e.specs.Workspace(t) }, nil)
		real, _ := twin.fake.PVCOf(twin.ns, sp.Name)
		fake := e.fake.PutPVC(e.ns, kubernetes.PersistentVolumeClaim{Metadata: copyOf(real.Metadata), Spec: real.Spec})
		e.fake.MutateRaw("pvc", e.ns, sp.Name, func(d map[string]any) {
			d["spec"].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)["storage"] = "999Gi"
		})
		settle(t, e, sp.Handle(""))
		if c, ok := e.fake.PVCOf(e.ns, sp.Name); !ok || c.Metadata.UID != fake.Metadata.UID {
			t.Fatal("a lookalike claim was deleted")
		}
		// The unchanged twin is ours and is deleted by the same pass.
		e2, twin2, sp2 := unconfirmed(t, "create pvc", func(e *runEnv) execution.ResourceSpec { return e.specs.Workspace(t) }, nil)
		real2, _ := twin2.fake.PVCOf(twin2.ns, sp2.Name)
		e2.fake.PutPVC(e2.ns, kubernetes.PersistentVolumeClaim{Metadata: copyOf(real2.Metadata), Spec: real2.Spec})
		settle(t, e2, sp2.Handle(""))
		if _, ok := e2.fake.PVCOf(e2.ns, sp2.Name); ok {
			t.Fatal("the late create of our own claim was not deleted")
		}
	})
	t.Run("pod", func(t *testing.T) {
		e, twin, sp := unconfirmed(t, "create pod", func(e *runEnv) execution.ResourceSpec { return e.specs.Service(t, "db", false) }, nil)
		real, _ := twin.fake.PodOf(twin.ns, sp.Name)
		spec := real.Spec
		spec.Containers = append([]kubernetes.Container(nil), spec.Containers...)
		spec.Containers[0].Image = "registry.example.invalid/evil@sha256:" + "ab"[:2] + "00000000000000000000000000000000000000000000000000000000000000"[:62]
		fake := e.fake.PutPod(e.ns, kubernetes.Pod{Metadata: copyOf(real.Metadata), Spec: spec})
		settle(t, e, sp.Handle(""))
		if p, ok := e.fake.PodOf(e.ns, sp.Name); !ok || p.Metadata.UID != fake.Metadata.UID {
			t.Fatal("a lookalike Pod was deleted")
		}
	})
	t.Run("job", func(t *testing.T) {
		pre := func(x *runEnv) { x.ensure(x.specs.Workspace(t)) }
		e, twin, sp := unconfirmed(t, "create job", func(e *runEnv) execution.ResourceSpec { return e.specs.Preparation(t) }, pre)
		real, _ := twin.fake.JobOf(twin.ns, sp.Name)
		spec := real.Spec
		bf := int32(6)
		spec.BackoffLimit = &bf
		fake := e.fake.PutJob(e.ns, kubernetes.Job{Metadata: copyOf(real.Metadata), Spec: spec})
		settle(t, e, sp.Handle(""))
		if j, ok := e.fake.JobOf(e.ns, sp.Name); !ok || j.Metadata.UID != fake.Metadata.UID {
			t.Fatal("a lookalike Job was deleted")
		}
	})

	// Companions: the service is bound and its Service and policy carry our marks, the
	// anchor and a recorded UID; a replacement that copies the marks is not ours.
	t.Run("companions of a service", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Service(t, "db", false)
		o := e.ensure(spec)
		host := execution.EndpointHost(spec.Scope, spec.RunID, "db")
		svc, _ := e.fake.ServiceOf(e.ns, host)
		pol, _ := e.fake.GetNetworkPolicy(ctx, e.ns, spec.Name+"-in")
		// Replace both with copies whose spec is wider.
		if err := e.fake.DeleteService(ctx, e.ns, host, kubernetes.DeleteOptions{UID: svc.Metadata.UID}); err != nil {
			t.Fatal(err)
		}
		fsvc := e.fake.PutService(e.ns, kubernetes.Service{Metadata: copyOf(svc.Metadata), Spec: kubernetes.ServiceSpec{
			Type: "ClusterIP", Selector: map[string]string{kube.LabelRunShared: "true"}, Ports: []kubernetes.ServicePort{{Name: "x", Port: 22, TargetPort: 22, Protocol: "TCP"}}}})
		if err := e.fake.DeleteNetworkPolicy(ctx, e.ns, pol.Metadata.Name, kubernetes.DeleteOptions{UID: pol.Metadata.UID}); err != nil {
			t.Fatal(err)
		}
		wide := kubernetes.NetworkPolicy{Metadata: copyOf(pol.Metadata), Spec: pol.Spec}
		wide.Spec.Ingress = []map[string]any{{"from": []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{kube.LabelRunShared: "true"}}}}, "ports": []any{map[string]any{"protocol": "TCP", "port": 22}}}}
		fpol, err := e.fake.CreateNetworkPolicy(ctx, e.ns, wide)
		if err != nil {
			t.Fatal(err)
		}
		settle(t, e, o.Handle)
		if s, ok := e.fake.ServiceOf(e.ns, host); !ok || s.Metadata.UID != fsvc.Metadata.UID {
			t.Fatal("a lookalike Service was deleted")
		}
		if p, err := e.fake.GetNetworkPolicy(ctx, e.ns, spec.Name+"-in"); err != nil || p.Metadata.UID != fpol.Metadata.UID {
			t.Fatal("a lookalike policy was deleted")
		}
	})
	// A companion that was never recorded (its create was announced but its response
	// never read back) is ours only if it equals its rendering.
	t.Run("an unrecorded companion", func(t *testing.T) {
		e, twin := newRunEnv(t, tenantBody()), newRunEnv(t, tenantBody())
		spec := e.specs.Service(t, "db", false)
		twin.ensure(spec)
		host := execution.EndpointHost(spec.Scope, spec.RunID, "db")
		real, _ := twin.fake.ServiceOf(twin.ns, host)
		var lookalike kubernetes.Service
		e.fake.Fault = pauseBefore("create service", func() {
			meta := copyOf(real.Metadata)
			pod, _ := e.fake.PodOf(e.ns, spec.Name)
			meta.Annotations[kube.AnnResourceUID] = pod.Metadata.UID // even the instance anchor is copied
			lookalike = e.fake.PutService(e.ns, kubernetes.Service{Metadata: meta, Spec: kubernetes.ServiceSpec{
				Type: "ClusterIP", Selector: map[string]string{kube.LabelRunShared: "true"}, Ports: []kubernetes.ServicePort{{Name: "x", Port: 22, TargetPort: 22, Protocol: "TCP"}}}})
		})
		requireIs(t, e.ensureErr(spec), execution.ErrResourceCollision)
		e.fake.Fault = nil
		pod, _ := e.fake.PodOf(e.ns, spec.Name)
		h := spec.Handle(pod.Metadata.UID)
		settle(t, e, h)
		if s, ok := e.fake.ServiceOf(e.ns, host); !ok || s.Metadata.UID != lookalike.Metadata.UID {
			t.Fatal("an unrecorded lookalike Service was deleted")
		}
	})
	t.Run("policies of a network", func(t *testing.T) {
		e := newRunEnv(t, stageBody())
		spec := e.specs.Network(t, nil, nil, map[string][]string{"build": {"registry"}})
		o := e.ensure(spec)
		pub, _ := e.fake.GetNetworkPolicy(ctx, e.ns, spec.Name+"-pub")
		if err := e.fake.DeleteNetworkPolicy(ctx, e.ns, pub.Metadata.Name, kubernetes.DeleteOptions{UID: pub.Metadata.UID}); err != nil {
			t.Fatal(err)
		}
		wide := kubernetes.NetworkPolicy{Metadata: copyOf(pub.Metadata), Spec: pub.Spec}
		wide.Spec.Egress = []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{{IPBlock: &kubernetes.IPBlock{CIDR: "0.0.0.0/0"}}}, Ports: []kubernetes.NetworkPort{{Protocol: "TCP", Port: float64(22)}}}}
		f, err := e.fake.CreateNetworkPolicy(ctx, e.ns, wide)
		if err != nil {
			t.Fatal(err)
		}
		settle(t, e, o.Handle)
		if p, err := e.fake.GetNetworkPolicy(ctx, e.ns, spec.Name+"-pub"); err != nil || p.Metadata.UID != f.Metadata.UID {
			t.Fatal("a lookalike policy of the network was deleted")
		}
	})
}

// Test_unit_kube_lost_record_is_not_fabricated_evidence: a record that disappears
// from a bound workload is lost bookkeeping, not "never issued": Stop reports
// unknown launch evidence and deletes the workload by its bound UID, and Remove
// succeeds only once the bound workload and its companions are verifiably gone.
func Test_unit_kube_lost_record_is_not_fabricated_evidence(t *testing.T) {
	ctx := context.Background()
	lose := func(e *runEnv, name string) {
		rec, ok := e.fake.SecretOf(e.ns, name+"-rec")
		if !ok {
			t.Fatal("no record")
		}
		if err := e.fake.DeleteSecret(ctx, e.ns, name+"-rec", kubernetes.DeleteOptions{UID: rec.Metadata.UID}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("workload gone", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Service(t, "db", false)
		o := e.ensure(spec)
		lose(e, spec.Name)
		pod, _ := e.fake.PodOf(e.ns, spec.Name)
		_ = e.fake.DeletePod(ctx, e.ns, spec.Name, kubernetes.DeleteOptions{UID: pod.Metadata.UID})
		host := execution.EndpointHost(spec.Scope, spec.RunID, "db")
		svc, _ := e.fake.ServiceOf(e.ns, host)
		_ = e.fake.DeleteService(ctx, e.ns, host, kubernetes.DeleteOptions{UID: svc.Metadata.UID})
		pol, _ := e.fake.GetNetworkPolicy(ctx, e.ns, spec.Name+"-in")
		_ = e.fake.DeleteNetworkPolicy(ctx, e.ns, spec.Name+"-in", kubernetes.DeleteOptions{UID: pol.Metadata.UID})
		ev := e.stop(o.Handle)
		if !ev.Stopped || ev.Launch != execution.LaunchUnknown {
			t.Fatalf("a lost record must never yield not_started: %+v", ev)
		}
		if rm := e.remove(o.Handle); !rm.Removed {
			t.Fatalf("remove %+v", rm)
		}
		// Without Stop first: Remove of a lost record with nothing left.
		e2 := newRunEnv(t, tenantBody())
		sp2 := e2.specs.Service(t, "db", false)
		o2 := e2.ensure(sp2)
		lose(e2, sp2.Name)
		p2, _ := e2.fake.PodOf(e2.ns, sp2.Name)
		_ = e2.fake.DeletePod(ctx, e2.ns, sp2.Name, kubernetes.DeleteOptions{UID: p2.Metadata.UID})
		h2 := execution.EndpointHost(sp2.Scope, sp2.RunID, "db")
		s2, _ := e2.fake.ServiceOf(e2.ns, h2)
		_ = e2.fake.DeleteService(ctx, e2.ns, h2, kubernetes.DeleteOptions{UID: s2.Metadata.UID})
		q2, _ := e2.fake.GetNetworkPolicy(ctx, e2.ns, sp2.Name+"-in")
		_ = e2.fake.DeleteNetworkPolicy(ctx, e2.ns, sp2.Name+"-in", kubernetes.DeleteOptions{UID: q2.Metadata.UID})
		if rm := e2.remove(o2.Handle); !rm.Removed {
			t.Fatalf("remove %+v", rm)
		}
	})
	t.Run("workload surviving", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Service(t, "db", false)
		o := e.ensure(spec)
		lose(e, spec.Name)
		if rm := e.remove(o.Handle); rm.Removed {
			t.Fatal("removal confirmed with the bound workload still running")
		}
		if _, ok := e.fake.PodOf(e.ns, spec.Name); !ok {
			t.Fatal("remove deleted a workload it was not asked to stop")
		}
		ev := e.stop(o.Handle)
		if !ev.Stopped || ev.Launch != execution.LaunchUnknown {
			t.Fatalf("stop %+v", ev)
		}
		if _, ok := e.fake.PodOf(e.ns, spec.Name); ok {
			t.Fatal("the bound workload survived the stop")
		}
		// The companions carry only copyable marks and the anchor: with the record (and
		// its recorded UIDs and spec) gone nothing independent proves them ours, so they
		// are left untouched and the removal stays unconfirmed with a safe reason.
		host := execution.EndpointHost(spec.Scope, spec.RunID, "db")
		svcBefore, ok := e.fake.ServiceOf(e.ns, host)
		if !ok {
			t.Fatal("the unverifiable Service was deleted")
		}
		polBefore, err := e.fake.GetNetworkPolicy(ctx, e.ns, spec.Name+"-in")
		if err != nil {
			t.Fatal("the unverifiable policy was deleted")
		}
		for i := 0; i < 2; i++ {
			rm, err := e.rr.Remove(ctx, o.Handle)
			if rm.Removed || !errors.Is(err, kube.ErrCleanupUnverified) {
				t.Fatalf("removal with unverifiable companions: %+v %v", rm, err)
			}
		}
		if s, _ := e.fake.ServiceOf(e.ns, host); s.Metadata.UID != svcBefore.Metadata.UID {
			t.Fatal("the Service was touched")
		}
		if p, err := e.fake.GetNetworkPolicy(ctx, e.ns, spec.Name+"-in"); err != nil || p.Metadata.UID != polBefore.Metadata.UID {
			t.Fatal("the policy was touched")
		}
		if e.fake.Calls("delete service") != 0 || e.fake.Calls("delete networkpolicy") != 0 {
			t.Fatal("an unverifiable companion was deleted")
		}
		// An operator resolves them; removal then completes.
		_ = e.fake.DeleteService(ctx, e.ns, host, kubernetes.DeleteOptions{UID: svcBefore.Metadata.UID})
		_ = e.fake.DeleteNetworkPolicy(ctx, e.ns, spec.Name+"-in", kubernetes.DeleteOptions{UID: polBefore.Metadata.UID})
		if rm := e.remove(o.Handle); !rm.Removed {
			t.Fatalf("remove %+v", rm)
		}
		inventoryEmpty(t, e)
	})
	t.Run("lost record with look-alike companions", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Service(t, "db", false)
		o := e.ensure(spec)
		host := execution.EndpointHost(spec.Scope, spec.RunID, "db")
		svc, _ := e.fake.ServiceOf(e.ns, host)
		pol, _ := e.fake.GetNetworkPolicy(ctx, e.ns, spec.Name+"-in")
		lose(e, spec.Name)
		// Replace the companions by objects of a different spec that copy every mark and
		// the anchor.
		_ = e.fake.DeleteService(ctx, e.ns, host, kubernetes.DeleteOptions{UID: svc.Metadata.UID})
		fsvc := e.fake.PutService(e.ns, kubernetes.Service{Metadata: kubernetes.ObjectMeta{Name: host, Labels: svc.Metadata.Labels, Annotations: svc.Metadata.Annotations},
			Spec: kubernetes.ServiceSpec{Type: "ClusterIP", Selector: map[string]string{kube.LabelRunShared: "true"}, Ports: []kubernetes.ServicePort{{Name: "x", Port: 22, TargetPort: 22, Protocol: "TCP"}}}})
		_ = e.fake.DeleteNetworkPolicy(ctx, e.ns, pol.Metadata.Name, kubernetes.DeleteOptions{UID: pol.Metadata.UID})
		wide := kubernetes.NetworkPolicy{Metadata: kubernetes.ObjectMeta{Name: pol.Metadata.Name, Labels: pol.Metadata.Labels, Annotations: pol.Metadata.Annotations}, Spec: pol.Spec}
		wide.Spec.Ingress = []map[string]any{{"from": []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{kube.LabelRunShared: "true"}}}}, "ports": []any{map[string]any{"protocol": "TCP", "port": 22}}}}
		fpol, err := e.fake.CreateNetworkPolicy(ctx, e.ns, wide)
		if err != nil {
			t.Fatal(err)
		}
		if rm, err := e.rr.Remove(ctx, o.Handle); rm.Removed || err != nil {
			t.Fatalf("remove before stop: %+v %v", rm, err)
		}
		pod, _ := e.fake.PodOf(e.ns, spec.Name)
		if pod.Metadata.UID != "" {
			if ev := e.stop(o.Handle); !ev.Stopped {
				t.Fatalf("stop %+v", ev)
			}
		}
		rm, err := e.rr.Remove(ctx, o.Handle)
		if rm.Removed || !errors.Is(err, kube.ErrCleanupUnverified) {
			t.Fatalf("remove %+v %v", rm, err)
		}
		if s, ok := e.fake.ServiceOf(e.ns, host); !ok || s.Metadata.UID != fsvc.Metadata.UID {
			t.Fatal("a look-alike Service was deleted")
		}
		if p, err := e.fake.GetNetworkPolicy(ctx, e.ns, spec.Name+"-in"); err != nil || p.Metadata.UID != fpol.Metadata.UID {
			t.Fatal("a look-alike policy was deleted")
		}
	})
	t.Run("lost record of a network", func(t *testing.T) {
		e := newRunEnv(t, stageBody())
		spec := e.specs.Network(t, nil, nil, map[string][]string{"build": {"registry"}})
		o := e.ensure(spec)
		before := e.fake.PoliciesWith(e.ns, kube.LabelResourceKind, "network")
		lose(e, spec.Name)
		if ev := e.stop(o.Handle); !ev.Stopped {
			t.Fatalf("stop %+v", ev)
		}
		rm, err := e.rr.Remove(ctx, o.Handle)
		if rm.Removed || !errors.Is(err, kube.ErrCleanupUnverified) {
			t.Fatalf("remove %+v %v", rm, err)
		}
		if got := e.fake.PoliciesWith(e.ns, kube.LabelResourceKind, "network"); !eqStrings(got, before) || e.fake.Calls("delete networkpolicy") != 0 {
			t.Fatalf("policies of a network with a lost record were deleted: %v -> %v", before, got)
		}
	})
	t.Run("a lost record of a lookalike object", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Service(t, "db", false)
		o := e.ensure(spec)
		lose(e, spec.Name)
		// The Pod is replaced by another instance: the bound UID no longer names it.
		pod, _ := e.fake.PodOf(e.ns, spec.Name)
		_ = e.fake.DeletePod(ctx, e.ns, spec.Name, kubernetes.DeleteOptions{UID: pod.Metadata.UID})
		other := e.fake.PutPod(e.ns, kubernetes.Pod{Metadata: pod.Metadata, Spec: pod.Spec})
		if _, err := e.rr.Stop(ctx, o.Handle); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("stop %v", err)
		}
		if p, ok := e.fake.PodOf(e.ns, spec.Name); !ok || p.Metadata.UID != other.Metadata.UID {
			t.Fatal("a replacement was deleted")
		}
	})
	t.Run("never bound and never recorded", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		h := e.specs.Service(t, "db", false).Handle("")
		if ev := e.stop(h); !ev.Stopped || ev.Launch != execution.LaunchUnknown {
			t.Fatalf("stop %+v", ev)
		}
		if rm := e.remove(h); !rm.Removed {
			t.Fatalf("remove %+v", rm)
		}
	})
	t.Run("workspace claim and volume", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		spec := e.specs.Workspace(t)
		o := e.ensure(spec)
		lose(e, spec.Name)
		if rm := e.remove(o.Handle); rm.Removed {
			t.Fatal("removal confirmed with the bound claim existing")
		}
		ev := e.stop(o.Handle)
		if !ev.Stopped || ev.Launch != execution.LaunchUnknown || !ev.Released {
			t.Fatalf("stop %+v", ev)
		}
		if rm := e.remove(o.Handle); !rm.Removed {
			t.Fatalf("remove %+v", rm)
		}
		inventoryEmpty(t, e)
	})
}

// Test_unit_kube_forged_owned_policies_are_refused: network policies are additive,
// so a policy that carries the owner label, the tenant and even every annotation of
// a real run policy but differs from what the resource's spec renders is refused by
// the namespace preflight and makes the resource's observation a collision.
func Test_unit_kube_forged_owned_policies_are_refused(t *testing.T) {
	ctx := context.Background()
	newEnv := func(t *testing.T, body execution.ExecutionPolicy) (*runEnv, execution.ResourceSpec, execution.ResourceObservation, execution.ResourceObservation) {
		e := newRunEnv(t, body)
		svc := e.ensure(e.specs.Service(t, "db", false))
		e.ensure(e.specs.Service(t, "app", false))
		spec := e.specs.Network(t, []string{"app", "db"}, []execution.EdgeSpec{{From: "app", To: "db", Ports: []string{"sql"}}}, map[string][]string{"build": {"registry"}})
		return e, spec, e.ensure(spec), svc
	}
	type forge struct {
		name   string
		body   func() execution.ExecutionPolicy
		forged func(e *runEnv, net execution.ResourceObservation) kubernetes.NetworkPolicy
	}
	strictBody := func() execution.ExecutionPolicy {
		b := runBody()
		b.EgressProfiles = stageBody().EgressProfiles
		return b
	}
	from := func(e *runEnv, name, suffix string) kubernetes.NetworkPolicy {
		p, err := e.fake.GetNetworkPolicy(ctx, e.ns, name)
		if err != nil {
			t.Fatal(err)
		}
		p.Metadata.Name, p.Metadata.UID, p.Raw = name+suffix, "", nil
		return p
	}
	forges := []forge{
		{"443 to an arbitrary address under publicEgress false", strictBody, func(e *runEnv, n execution.ResourceObservation) kubernetes.NetworkPolicy {
			p := from(e, n.Handle.Name+"-cli", "-x")
			p.Spec.Egress = []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{{IPBlock: &kubernetes.IPBlock{CIDR: "203.0.113.9/32"}}}, Ports: []kubernetes.NetworkPort{{Protocol: "TCP", Port: float64(443)}}}}
			return p
		}},
		{"public egress selecting services", tenantBodyWithProfiles, func(e *runEnv, n execution.ResourceObservation) kubernetes.NetworkPolicy {
			p := from(e, n.Handle.Name+"-pub", "-svc")
			p.Spec.PodSelector.MatchLabels[kube.LabelRole] = kube.RoleLabelService
			return p
		}},
		{"a peer of another run", strictBody, func(e *runEnv, n execution.ResourceObservation) kubernetes.NetworkPolicy {
			p := from(e, n.Handle.Name+"-cli", "-y")
			p.Spec.Egress = []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{{PodSelector: &kubernetes.LabelSelector{MatchLabels: map[string]string{kube.LabelRunKey: "0123456789abcdef0123456789abcdef", kube.LabelRole: kube.RoleLabelService}}}}}}
			return p
		}},
		{"an undeclared edge", strictBody, func(e *runEnv, n execution.ResourceObservation) kubernetes.NetworkPolicy {
			p := from(e, n.Handle.Name+"-eo0", "-z")
			sel := p.Spec.PodSelector.MatchLabels
			p.Spec.PodSelector.MatchLabels = map[string]string{kube.LabelRunKey: sel[kube.LabelRunKey], kube.LabelRole: kube.RoleLabelService, kube.LabelWorkload: kube.WorkloadHash(e.specs.Scope, e.specs.Run, "service.db")}
			p.Spec.Egress[0].To[0].PodSelector.MatchLabels[kube.LabelWorkload] = kube.WorkloadHash(e.specs.Scope, e.specs.Run, "service.app")
			return p
		}},
		{"an extra port on a rendered name", strictBody, func(e *runEnv, n execution.ResourceObservation) kubernetes.NetworkPolicy {
			p := from(e, n.Handle.Name+"-eo0", "")
			if err := e.fake.DeleteNetworkPolicy(ctx, e.ns, p.Metadata.Name, kubernetes.DeleteOptions{UID: mustPolicy(t, e, p.Metadata.Name).Metadata.UID}); err != nil {
				t.Fatal(err)
			}
			p.Spec.Egress[0].Ports = append(p.Spec.Egress[0].Ports, kubernetes.NetworkPort{Protocol: "TCP", Port: "other"})
			return p
		}},
		{"a copy of a rendered policy under another name", strictBody, func(e *runEnv, n execution.ResourceObservation) kubernetes.NetworkPolicy {
			return from(e, n.Handle.Name+"-cli", "-copy")
		}},
	}
	for _, f := range forges {
		t.Run(f.name, func(t *testing.T) {
			e, spec, net, _ := newEnv(t, f.body())
			check := func() error {
				e.clock.Advance(time.Minute)
				return e.rr.Preflight().CheckRun(ctx, e.specs.Scope.TenantID, e.body.PodSecurity)
			}
			if err := check(); err != nil {
				t.Fatalf("baseline: %v", err)
			}
			p := f.forged(e, net)
			if _, err := e.fake.CreateNetworkPolicy(ctx, e.ns, p); err != nil {
				t.Fatal(err)
			}
			if err := check(); err == nil || !errors.Is(err, kube.ErrPreflight) {
				t.Fatalf("the preflight accepted a forged policy: %v", err)
			}
			if _, err := e.rr.Observe(ctx, net.Handle); !errors.Is(err, execution.ErrResourceCollision) {
				t.Fatalf("the network is still observed healthy: %v", err)
			}
			if _, err := e.rr.Ensure(ctx, spec); err == nil || !errors.Is(err, execution.ErrResourceCollision) && !errors.Is(err, kube.ErrPreflight) {
				t.Fatalf("ensure over a forged policy: %v", err)
			}
		})
	}
	t.Run("a forged ingress policy beside a service", func(t *testing.T) {
		e, _, _, svc := newEnv(t, stageBody())
		pol, _ := e.fake.GetNetworkPolicy(ctx, e.ns, svc.Handle.Name+"-in")
		pol.Metadata.Name, pol.Metadata.UID, pol.Raw = svc.Handle.Name+"-in-wide", "", nil
		pol.Spec.Ingress = []map[string]any{{"from": []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{kube.LabelRunShared: "true"}}}}, "ports": []any{map[string]any{"protocol": "TCP", "port": 22}}}}
		if _, err := e.fake.CreateNetworkPolicy(ctx, e.ns, pol); err != nil {
			t.Fatal(err)
		}
		e.clock.Advance(time.Minute)
		if err := e.rr.Preflight().CheckRun(ctx, e.specs.Scope.TenantID, e.body.PodSecurity); err == nil || !errors.Is(err, kube.ErrPreflight) {
			t.Fatalf("preflight %v", err)
		}
		if _, err := e.rr.Observe(ctx, svc.Handle); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("observe %v", err)
		}
	})
}

func tenantBodyWithProfiles() execution.ExecutionPolicy { return stageBody() }

func mustPolicy(t *testing.T, e *runEnv, name string) kubernetes.NetworkPolicy {
	t.Helper()
	p, err := e.fake.GetNetworkPolicy(context.Background(), e.ns, name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Test_unit_kube_preflight_cache_separates_run_shared: a success of the
// attempt_private check never satisfies the run_shared one (and the reverse), even
// with identical Pod Security settings, because the run_shared check also refuses
// permissions only it knows.
func Test_unit_kube_preflight_cache_separates_run_shared(t *testing.T) {
	ctx := context.Background()
	body := stageBody()
	body.PodSecurity = execution.PolicyPodSecurity{Level: "restricted"}
	for _, order := range []string{"legacy first", "shared first"} {
		t.Run(order, func(t *testing.T) {
			e := newRunEnv(t, body)
			// Only the run_shared check refuses this permission.
			e.fake.Permitted[e.ns+"/create/persistentvolumeclaims"] = true
			p := kube.NewPreflight(e.fake, e.policy, 1<<40, e.clock.Now)
			tenant := e.specs.Scope.TenantID
			legacy := func() error { return p.Check(ctx, tenant) }
			shared := func() error { return p.CheckRun(ctx, tenant, body.PodSecurity) }
			first, second := legacy, shared
			if order == "shared first" {
				first, second = shared, legacy
			}
			a, b := first(), second()
			legacyErr, sharedErr := a, b
			if order == "shared first" {
				legacyErr, sharedErr = b, a
			}
			if legacyErr != nil {
				t.Fatalf("the attempt_private check must pass: %v", legacyErr)
			}
			if sharedErr == nil || !errors.Is(sharedErr, kube.ErrPreflight) {
				t.Fatalf("the run_shared check was satisfied by the other check's cached success: %v", sharedErr)
			}
			// And a second call still refuses (a refusal is never cached as success).
			if err := shared(); err == nil {
				t.Fatal("the run_shared check passed on repetition")
			}
		})
	}
}

// Test_unit_kube_concurrent_ensures_do_not_settle_each_others_creates: two Ensures
// of one resource announce their creates separately and each settles only its own.
// Ensure A is paused right before its Service create; Ensure B completes the
// companions; Stop and Remove then cannot finish while A's create may still land,
// and the Service that A creates afterwards is found and deleted, so no orphan
// outlives the removal.
func Test_unit_kube_concurrent_ensures_do_not_settle_each_others_creates(t *testing.T) {
	ctx := context.Background()
	e := newRunEnv(t, tenantBody())
	spec := e.specs.Service(t, "db", false)
	host := execution.EndpointHost(spec.Scope, spec.RunID, "db")
	h := spec.Handle("")
	stopDone, removeDone := false, false
	e.fake.Fault = pauseBefore("create service", func() {
		b := e.build() // a second controller instance
		o, err := b.Ensure(ctx, spec)
		if err != nil || o.Handle.UID == "" {
			t.Errorf("ensure B: %+v %v", o, err)
			return
		}
		hb := o.Handle
		ev, err := b.Stop(ctx, hb)
		stopDone = err != nil || ev.Stopped
		rm, err := b.Remove(ctx, hb)
		removeDone = (err != nil && !errors.Is(err, execution.ErrResourceFenced)) || rm.Removed
	})
	requireIs(t, e.ensureErr(spec), execution.ErrResourceFenced)
	e.fake.Fault = nil
	if stopDone || removeDone {
		t.Fatalf("B's settling let a stop or removal finish while A's create was outstanding: stopped=%v removed=%v", stopDone, removeDone)
	}
	if _, ok := e.fake.ServiceOf(e.ns, host); !ok {
		t.Fatal("the interleaving did not produce the late Service this test is about")
	}
	pod, _ := e.fake.PodOf(e.ns, spec.Name)
	h.UID = pod.Metadata.UID
	if ev := e.stop(h); ev.Stopped {
		t.Fatalf("stopped inside A's quiescence: %+v", ev)
	}
	if _, ok := e.fake.ServiceOf(e.ns, host); ok {
		t.Fatal("the late Service was not deleted")
	}
	e.afterQuiescence()
	if ev := e.stop(h); !ev.Stopped {
		t.Fatalf("stop %+v", ev)
	}
	if rm := e.remove(h); !rm.Removed {
		t.Fatalf("remove %+v", rm)
	}
	inventoryEmpty(t, e)
}

// Test_unit_kube_removal_during_record_creation_is_not_resurrected: an Ensure
// paused right before it creates the resource's record, overtaken by a Stop and a
// Remove that write the removal marker and delete the fence they created, must not
// bring the resource back: the marker is read again after the record is created and
// before any effect, the record just created is deleted and the Ensure is refused.
func Test_unit_kube_removal_during_record_creation_is_not_resurrected(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"workspace", "plain service", "supervised service", "preparation", "network"} {
		t.Run(kind, func(t *testing.T) {
			e := newRunEnv(t, stageBody())
			var spec execution.ResourceSpec
			switch kind {
			case "workspace":
				spec = e.specs.Workspace(t)
			case "plain service":
				spec = e.specs.Service(t, "db", false)
			case "supervised service":
				spec = e.specs.Service(t, "db", true)
			case "preparation":
				e.ensure(e.specs.Workspace(t))
				spec = e.specs.Preparation(t)
			case "network":
				spec = e.specs.Network(t, nil, nil, map[string][]string{"build": {"registry"}})
			}
			h := spec.Handle("")
			effects := func() int {
				return e.fake.Calls("create pvc") + e.fake.Calls("create pod") + e.fake.Calls("create job") + e.fake.Calls("create service") + e.fake.Calls("create networkpolicy")
			}
			before := effects()
			inStop, inRemove := false, false
			e.fake.Fault = pauseBefore("create secret", func() {
				ev, err := e.rr.Stop(ctx, h)
				inStop = err == nil && ev.Stopped
				rm, err := e.rr.Remove(ctx, h)
				inRemove = err == nil && rm.Removed
			})
			requireIs(t, e.ensureErr(spec), execution.ErrResourceFenced)
			e.fake.Fault = nil
			if !inStop || !inRemove {
				t.Fatalf("the racing stop and removal did not complete (stopped=%v removed=%v): the scenario is not the one under test", inStop, inRemove)
			}
			if effects() != before {
				t.Fatal("an object was created for a resource removed while it was being planned")
			}
			if _, ok := e.fake.SecretOf(e.ns, spec.Name+"-rec"); ok {
				t.Fatal("the record of the refused Ensure was left behind")
			}
			if _, ok := e.fake.SecretOf(e.ns, spec.Name+"-tomb"); !ok {
				t.Fatal("no removal marker")
			}
			// And a repeated Ensure stays refused.
			requireIs(t, e.ensureErr(spec), execution.ErrResourceFenced)
		})
	}
}

// Test_unit_kube_late_claim_identity_survives_cleanup_retries: a claim that arrives
// late (an uncertain create; no UID committed) and verifies by its spec has its UID
// persisted before it is deleted. With the volume's reclaim still pending, a retry
// (also by a restarted adapter) keeps finding the volume bound to that UID, whatever
// its name, neither colliding on the recorded volume nor confirming the removal
// early.
func Test_unit_kube_late_claim_identity_survives_cleanup_retries(t *testing.T) {
	ctx := context.Background()
	for _, variant := range []string{"arbitrary volume name", "recorded volume of a bound claim"} {
		t.Run(variant, func(t *testing.T) {
			e, twin := newRunEnv(t, tenantBody()), newRunEnv(t, tenantBody())
			spec := e.specs.Workspace(t)
			twin.ensure(spec)
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
			// The create lands late: a claim equal to ours, with no UID anyone committed.
			real, _ := twin.fake.PVCOf(twin.ns, spec.Name)
			meta := kubernetes.ObjectMeta{Name: real.Metadata.Name, Labels: real.Metadata.Labels, Annotations: real.Metadata.Annotations}
			late := e.fake.PutPVC(e.ns, kubernetes.PersistentVolumeClaim{Metadata: meta, Spec: kubernetes.PVCSpec{
				AccessModes: real.Spec.AccessModes, StorageClassName: real.Spec.StorageClassName, Resources: real.Spec.Resources, VolumeMode: real.Spec.VolumeMode}})
			h := spec.Handle("")
			vol := "pv-arbitrary-name"
			if variant == "arbitrary volume name" {
				e.fake.PutPV(vol, e.ns, spec.Name, late.Metadata.UID)
			} else {
				vol = e.fake.BindClaim(e.ns, spec.Name)
			}
			e.stop(h)
			e.afterQuiescence()
			if ev := e.stop(h); !ev.Stopped || ev.Released != true {
				t.Fatalf("stop %+v", ev)
			}
			e.fake.HoldPVs = true
			// First removal: the claim is deleted; its volume is still being reclaimed.
			if rm, err := e.rr.Remove(ctx, h); err != nil || rm.Removed {
				t.Fatalf("remove while the volume exists: %+v %v", rm, err)
			}
			if _, ok := e.fake.PVCOf(e.ns, spec.Name); ok {
				t.Fatal("the late claim was not deleted")
			}
			rec, _ := e.fake.SecretOf(e.ns, spec.Name+"-rec")
			if !strings.Contains(rec.Metadata.Annotations[kube.AnnRecord], late.Metadata.UID) {
				t.Fatalf("the claim's UID was not persisted before its deletion: %s", rec.Metadata.Annotations[kube.AnnRecord])
			}
			// Retries, the last by a restarted adapter, still know the volume belongs to it.
			for i := 0; i < 2; i++ {
				if i == 1 {
					e.rr = e.build()
				}
				if rm, err := e.rr.Remove(ctx, h); err != nil || rm.Removed {
					t.Fatalf("retry %d: %+v %v", i, rm, err)
				}
			}
			if _, ok := e.fake.PVOf(vol); !ok {
				t.Fatal("scenario: the volume is gone already")
			}
			e.fake.ReleasePVs()
			if rm, err := e.rr.Remove(ctx, h); err != nil || !rm.Removed {
				t.Fatalf("after the volume is gone: %+v %v", rm, err)
			}
			inventoryEmpty(t, e)
		})
	}
}

// Test_unit_kube_late_companion_after_a_lost_delete_response_is_deleted: Ensure A is
// paused before its companion create; Ensure B completes and records the companion's
// UID; Stop deletes that companion but the delete's response is lost; A then resumes
// and its outstanding create produces a new instance. The recorded UID of the deleted
// instance never stood for a foreign object: the new instance carries our marks, our
// anchor and exactly our rendering, so it is ours, deleted by cleanup, and removal is
// never confirmed while it exists.
func Test_unit_kube_late_companion_after_a_lost_delete_response_is_deleted(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, create, del string }{
		{"service", "create service", "delete service"},
		{"ingress policy", "create networkpolicy", "delete networkpolicy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRunEnv(t, tenantBody())
			spec := e.specs.Service(t, "db", false)
			host := execution.EndpointHost(spec.Scope, spec.RunID, "db")
			exists := func() bool {
				if tc.name == "service" {
					_, ok := e.fake.ServiceOf(e.ns, host)
					return ok
				}
				_, err := e.fake.GetNetworkPolicy(ctx, e.ns, spec.Name+"-in")
				return err == nil
			}
			paused, loseDelete, lost := false, false, 0
			e.fake.Fault = func(op string, n int) *kubetestFault {
				switch {
				case op == tc.create && !paused:
					paused = true
					b := e.build()
					o, err := b.Ensure(ctx, spec)
					if err != nil {
						t.Errorf("ensure B: %v", err)
						return nil
					}
					// Stop deletes B's companion; the response of that delete is lost.
					loseDelete = true
					if _, err := b.Stop(ctx, o.Handle); err == nil {
						t.Error("the lost delete response was not reported")
					}
					loseDelete = false
				case op == tc.del && loseDelete && lost == 0:
					lost++
					return &kubetestFault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}, After: true}
				}
				return nil
			}
			requireIs(t, e.ensureErr(spec), execution.ErrResourceFenced)
			e.fake.Fault = nil
			if lost != 1 {
				t.Fatalf("scenario: the delete response was not lost (%d)", lost)
			}
			if !exists() {
				t.Fatal("scenario: A's outstanding create did not produce the late object")
			}
			pod, _ := e.fake.PodOf(e.ns, spec.Name)
			h := spec.Handle(pod.Metadata.UID)
			removedWhileExists := func(rm execution.ResourceRemoveEvidence) {
				t.Helper()
				if rm.Removed && exists() {
					t.Fatal("removal was confirmed while the late object still exists")
				}
			}
			for i := 0; i < 3; i++ {
				e.stop(h)
				rm, _ := e.rr.Remove(ctx, h)
				removedWhileExists(rm)
				if rm.Removed {
					break
				}
				e.afterQuiescence()
			}
			e.stop(h)
			rm := e.remove(h)
			removedWhileExists(rm)
			if !rm.Removed {
				t.Fatalf("remove %+v", rm)
			}
			if exists() {
				t.Fatal("the late object survived the removal")
			}
			inventoryEmpty(t, e)
		})
	}
}
