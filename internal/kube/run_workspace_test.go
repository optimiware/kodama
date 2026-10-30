package kube_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Test_unit_kube_run_workspace_claim_lifecycle: the run's scratch is one
// ReadWriteOncePod claim of the policy's storage class and size, created behind
// its record, adopted idempotently (also after a lost create response), fenced by
// Stop (a stopped workspace is never created again) and removed together with its
// volume (delete reclaim) before the record is.
func Test_unit_kube_run_workspace_claim_lifecycle(t *testing.T) {
	e := newRunEnv(t, tenantBody())
	spec := e.specs.Workspace(t)

	o := e.ensure(spec)
	if o.Phase != execution.PhaseReady || o.Handle.UID == "" || o.Fingerprint == "" || !o.Released || o.ReadyAt == nil {
		t.Fatalf("observation %+v", o)
	}
	fp, _ := spec.Fingerprint()
	if o.Fingerprint != fp {
		t.Fatalf("fingerprint %s", o.Fingerprint)
	}
	pvc, ok := e.fake.PVCOf(e.ns, spec.Name)
	if !ok || pvc.Metadata.UID != o.Handle.UID {
		t.Fatalf("claim %+v", pvc.Metadata)
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != "ReadWriteOncePod" || pvc.Spec.StorageClassName == nil ||
		*pvc.Spec.StorageClassName != "test-scratch" || pvc.Spec.Resources.Requests["storage"] != "2048Mi" {
		t.Fatalf("claim spec %+v", pvc.Spec)
	}
	if pvc.Metadata.Labels[kube.LabelResourceOwner] != kube.ValueResourceOwner || pvc.Metadata.Annotations[kube.AnnFingerprint] != fp ||
		pvc.Metadata.Annotations[kube.AnnTenant] != string(spec.Scope.TenantID) || pvc.Metadata.Annotations[kube.AnnResourceKey] != execution.KeyWorkspace {
		t.Fatalf("ownership %+v", pvc.Metadata)
	}
	// Intent first: the record exists before the claim.
	secretAt, claimAt := -1, -1
	for i, op := range e.fake.Log {
		switch op {
		case "create secret":
			if secretAt < 0 {
				secretAt = i
			}
		case "create pvc":
			claimAt = i
		}
	}
	if secretAt < 0 || claimAt < 0 || secretAt > claimAt {
		t.Fatalf("the record was not created first: %v", e.fake.Log)
	}

	// Idempotent: a second Ensure (and a restarted controller) adopts the same claim.
	if again := e.ensure(spec); again.Handle.UID != o.Handle.UID || e.fake.Calls("create pvc") != 1 {
		t.Fatalf("not idempotent: %+v creates %d", again, e.fake.Calls("create pvc"))
	}
	e.rr = e.build()
	if again := e.ensure(spec); again.Handle.UID != o.Handle.UID || e.fake.Calls("create pvc") != 1 {
		t.Fatalf("restart did not adopt: %+v", again)
	}
	h := o.Handle
	if got := e.observe(h); got.Phase != execution.PhaseReady || got.Handle.UID != h.UID {
		t.Fatalf("observe %+v", got)
	}

	// Stop fences: Ensure refuses and creates nothing.
	if ev := e.stop(h); !ev.Stopped || !ev.Released {
		t.Fatalf("stop %+v", ev)
	}
	creates := e.fake.Calls("create pvc")
	requireIs(t, e.ensureErr(spec), execution.ErrResourceFenced)
	if e.fake.Calls("create pvc") != creates {
		t.Fatal("a stopped workspace was created again")
	}

	// Removal deletes the claim by its UID, waits for the volume (reclaim) and
	// deletes the record last.
	e.fake.HoldPVs = true
	vol := pvc.Spec.VolumeName
	if got := e.remove(h); got.Removed {
		t.Fatal("removed while the volume exists")
	}
	if _, ok := e.fake.PVCOf(e.ns, spec.Name); ok {
		t.Fatal("the claim was not deleted")
	}
	if _, ok := e.fake.SecretOf(e.ns, spec.Name+"-rec"); !ok {
		t.Fatal("the record was deleted before the volume was gone")
	}
	e.fake.ReleasePVs()
	if _, ok := e.fake.PVOf(vol); ok {
		t.Fatal("volume not released by the fake")
	}
	if got := e.remove(h); !got.Removed {
		t.Fatalf("not removed after the volume is gone: %+v", got)
	}
	if left := liveSecrets(e); len(left) != 0 {
		t.Fatalf("secrets left: %v", left)
	}
	if got := e.observe(h); got.Phase != execution.PhaseAbsent {
		t.Fatalf("after removal: %+v", got)
	}
	// Removing again is idempotent.
	if got := e.remove(h); !got.Removed {
		t.Fatal("second removal")
	}
}

// Test_unit_kube_run_workspace_lost_create_response: a create whose response was
// lost is resolved by reading: the next Ensure adopts the claim that exists and
// never creates a second one.
func Test_unit_kube_run_workspace_lost_create_response(t *testing.T) {
	e := newRunEnv(t, tenantBody())
	spec := e.specs.Workspace(t)
	e.fake.Fault = func(op string, n int) *kubetestFault {
		if op == "create pvc" && n == 1 {
			return &kubetestFault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}, After: true}
		}
		return nil
	}
	o, err := e.rr.Ensure(context.Background(), spec)
	if err != nil {
		t.Fatalf("a lost response is resolved by reading: %v", err)
	}
	if o.Handle.UID == "" || e.fake.Calls("create pvc") != 1 {
		t.Fatalf("%+v creates %d", o, e.fake.Calls("create pvc"))
	}
	if names := e.fake.Names("pvc", e.ns); len(names) != 1 {
		t.Fatalf("claims %v", names)
	}
	// Stopping right after an unknown outcome waits for the create to be unable to land.
	f2 := newRunEnv(t, tenantBody())
	f2.fake.Fault = func(op string, n int) *kubetestFault {
		if op == "create pvc" {
			return &kubetestFault{Err: &kubernetes.Error{Op: op, Kind: kubernetes.ErrUnknownOutcome}}
		}
		return nil
	}
	if _, err := f2.rr.Ensure(context.Background(), f2.specs.Workspace(t)); err == nil {
		t.Fatal("a create that is not visible must not report a bound claim")
	} else if !errors.Is(err, kube.ErrCreateUncertain) {
		t.Fatalf("error %v", err)
	}
	h := f2.specs.Workspace(t).Handle("")
	if ev := f2.stop(h); ev.Stopped {
		t.Fatal("stopped while a create may still land")
	}
	f2.afterQuiescence()
	if ev := f2.stop(h); !ev.Stopped {
		t.Fatalf("not stopped after quiescence: %+v", ev)
	}
}

// Test_unit_kube_workspace_release_fence: Released is true only when no Pod of the
// namespace references the claim (whatever its phase) and no VolumeAttachment of
// its volume remains; an unreadable cluster never reports it. Stop and Remove
// honor the fence: a workspace that may still be mounted is neither handed out nor
// deleted.
func Test_unit_kube_workspace_release_fence(t *testing.T) {
	e := newRunEnv(t, tenantBody())
	spec := e.specs.Workspace(t)
	o := e.ensure(spec)
	h := o.Handle
	pvc, _ := e.fake.PVCOf(e.ns, spec.Name)
	vol := pvc.Spec.VolumeName
	users := func(name string, phase string) {
		e.fake.PutPod(e.ns, kubernetes.Pod{
			Metadata: kubernetes.ObjectMeta{Name: name, Labels: map[string]string{"x": "y"}},
			Spec: kubernetes.PodSpec{Containers: []kubernetes.Container{{Name: "c", Image: "i"}}, Volumes: []kubernetes.Volume{
				{Name: "w", PersistentVolumeClaim: &kubernetes.PVCVolume{ClaimName: spec.Name}}}},
			Status: kubernetes.PodStatus{Phase: phase},
		})
	}
	released := func() bool { return e.observe(h).Released }
	if !released() {
		t.Fatal("an unused claim must be released")
	}
	// A Pod using the claim blocks release, even a terminated one: its mount may
	// not be cleaned up yet.
	drop := func(name string) {
		pod, _ := e.fake.PodOf(e.ns, name)
		if err := e.fake.DeletePod(context.Background(), e.ns, name, kubernetes.DeleteOptions{UID: pod.Metadata.UID}); err != nil {
			t.Fatal(err)
		}
	}
	for _, phase := range []string{"Running", "Pending", "Succeeded", "Failed"} {
		users("user-"+phase, phase)
		if released() {
			t.Fatalf("released while a %s Pod references the claim", phase)
		}
		drop("user-" + phase)
	}
	// A terminating Pod (held by its graceful shutdown) still references the claim.
	e.fake.HoldPods = true
	users("dying", "Running")
	drop("dying")
	if released() {
		t.Fatal("released while a terminating Pod references the claim")
	}
	e.fake.ReleasePods()
	e.fake.HoldPods = false
	if !released() {
		t.Fatal("not released after the Pods are gone")
	}
	e.fake.Attach(vol)
	if released() {
		t.Fatal("released while the volume is attached to a node")
	}
	// An attachment of another volume does not matter.
	e.fake.Detach(vol)
	e.fake.Attach("pvc-other")
	if !released() {
		t.Fatal("another volume's attachment blocked the release")
	}

	// Unreadable evidence is never "released" and never "absent".
	for _, op := range []string{"list pods", "list volumeattachments"} {
		op := op
		e.fake.Fault = func(o string, n int) *kubetestFault {
			if o == op {
				return &kubetestFault{Err: &kubernetes.Error{Op: o, Kind: kubernetes.ErrUnavailable}}
			}
			return nil
		}
		got := e.observe(h)
		if got.Phase != execution.PhaseUnknown || got.Released {
			t.Fatalf("%s outage: %+v", op, got)
		}
		if _, err := e.rr.Stop(context.Background(), h); err == nil {
			t.Fatalf("%s outage: a stop that cannot read the fence concluded", op)
		}
	}
	e.fake.Fault = nil

	// Stop reports the fence as it is; Remove never deletes a claim that may be mounted.
	users("late", "Running")
	if ev := e.stop(h); !ev.Stopped || ev.Released {
		t.Fatalf("stop with a user: %+v", ev)
	}
	if got := e.remove(h); got.Removed {
		t.Fatal("removed a claim that is still referenced")
	}
	if _, ok := e.fake.PVCOf(e.ns, spec.Name); !ok {
		t.Fatal("a referenced claim was deleted")
	}
	e.fake.Detach("pvc-other")
	drop("late")
	if ev := e.stop(h); !ev.Stopped || !ev.Released {
		t.Fatalf("stop after the user is gone: %+v", ev)
	}
	if got := e.remove(h); !got.Removed {
		t.Fatalf("remove: %+v", got)
	}

	// The tenant boundary of the evidence: a Pod of another namespace referencing a
	// same-named claim is not a user of this one.
	if names := e.fake.Names("pvc", e.ns); len(names) != 0 {
		t.Fatalf("claims left %v", names)
	}
}

func wffc(e *runEnv) {
	e.fake.PendingClaims = true
	e.fake.PutStorageClass(e.body.Workspace.StorageClass, "WaitForFirstConsumer", "Delete")
}

func claimUsedBy(e *runEnv, pod, claim string) {
	e.fake.PutPod(e.ns, kubernetes.Pod{
		Metadata: kubernetes.ObjectMeta{Name: pod},
		Spec: kubernetes.PodSpec{Containers: []kubernetes.Container{{Name: "c", Image: "i"}}, Volumes: []kubernetes.Volume{
			{Name: "w", PersistentVolumeClaim: &kubernetes.PVCVolume{ClaimName: claim}}}},
		Status: kubernetes.PodStatus{Phase: "Pending"},
	})
}

// Test_unit_kube_workspace_wait_for_first_consumer: a claim of a class that binds
// on first use stays Pending until its first consumer exists, yet it is the run's
// usable workspace: execution gates the preparation (and a source-free run's first
// attempt) on the workspace being ready, so waiting for Bound would deadlock. Any
// other Pending claim is not ready, a class that keeps volumes is never accepted
// and an unreadable class concludes nothing.
func Test_unit_kube_workspace_wait_for_first_consumer(t *testing.T) {
	ctx := context.Background()

	t.Run("pending claim of a first-consumer class is ready", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		wffc(e)
		spec := e.specs.Workspace(t)
		o := e.ensure(spec)
		if o.Phase != execution.PhaseReady || o.Handle.UID == "" || o.ReadyAt == nil || !o.Released || o.Fingerprint == "" {
			t.Fatalf("observation %+v", o)
		}
		pvc, _ := e.fake.PVCOf(e.ns, spec.Name)
		if pvc.Status.Phase != "Pending" || pvc.Spec.VolumeName != "" {
			t.Fatalf("the claim is expected to wait for its consumer: %+v", pvc.Status)
		}
		if got := e.observe(o.Handle); got.Phase != execution.PhaseReady {
			t.Fatalf("observe %+v", got)
		}
		// It binds after the first consumer appears; from then on the volume fence applies.
		claimUsedBy(e, "first-consumer", spec.Name)
		if got := e.observe(o.Handle); got.Phase != execution.PhaseReady || got.Released {
			t.Fatalf("with a consumer: %+v", got)
		}
		vol := e.fake.BindClaim(e.ns, spec.Name)
		if got := e.observe(o.Handle); got.Phase != execution.PhaseReady || got.Released {
			t.Fatalf("bound with a consumer: %+v", got)
		}
		e.fake.Attach(vol)
		pod, _ := e.fake.PodOf(e.ns, "first-consumer")
		if err := e.fake.DeletePod(ctx, e.ns, "first-consumer", kubernetes.DeleteOptions{UID: pod.Metadata.UID}); err != nil {
			t.Fatal(err)
		}
		if got := e.observe(o.Handle); got.Phase != execution.PhaseReady || got.Released {
			t.Fatalf("the Pod is gone but the volume is still attached: %+v", got)
		}
		e.fake.Detach(vol)
		if got := e.observe(o.Handle); got.Phase != execution.PhaseReady || !got.Released {
			t.Fatalf("released after the detach: %+v", got)
		}
		// Stop, then remove: the claim and the volume that appeared after the plan are gone.
		if ev := e.stop(o.Handle); !ev.Stopped || !ev.Released {
			t.Fatalf("stop %+v", ev)
		}
		if got := e.remove(o.Handle); !got.Removed {
			t.Fatalf("remove %+v", got)
		}
		if _, ok := e.fake.PVOf(vol); ok {
			t.Fatal("the volume outlived the claim")
		}
	})

	t.Run("pending claim of an immediate class is not ready", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		e.fake.PendingClaims = true // an Immediate class still provisioning
		o := e.ensure(e.specs.Workspace(t))
		if o.Phase != execution.PhasePending || o.ReadyAt != nil {
			t.Fatalf("%+v", o)
		}
	})

	t.Run("a class that keeps volumes is refused", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		wffc(e)
		e.fake.PutStorageClass(e.body.Workspace.StorageClass, "WaitForFirstConsumer", "Retain")
		err := e.ensureErr(e.specs.Workspace(t))
		if err == nil || !errors.Is(err, kube.ErrPolicy) {
			t.Fatalf("ensure %v", err)
		}
		if len(e.fake.Names("pvc", e.ns)) != 0 {
			t.Fatal("a claim was created against a Retain class")
		}
		// The class turning Retain under an existing claim, bound or waiting, is a
		// failure with a safe reason, never ready.
		for _, bind := range []bool{false, true} {
			x := newRunEnv(t, tenantBody())
			wffc(x)
			spec := x.specs.Workspace(t)
			o := x.ensure(spec)
			if bind {
				x.fake.BindClaim(x.ns, spec.Name)
			}
			x.fake.PutStorageClass(x.body.Workspace.StorageClass, "WaitForFirstConsumer", "Retain")
			got := x.observe(o.Handle)
			if got.Phase != execution.PhaseFailed || got.Reason != execution.ReasonCapabilityUnsupported || !got.Reason.Valid() {
				t.Fatalf("bound=%v: %+v", bind, got)
			}
		}
	})

	t.Run("a missing class", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		wffc(e)
		delete(e.fake.StorageClasses, e.body.Workspace.StorageClass)
		if err := e.ensureErr(e.specs.Workspace(t)); err == nil || !errors.Is(err, kube.ErrPolicy) {
			t.Fatalf("ensure %v", err)
		}
		// An existing waiting claim whose class vanished is pending; a bound one unknown.
		x := newRunEnv(t, tenantBody())
		wffc(x)
		spec := x.specs.Workspace(t)
		o := x.ensure(spec)
		delete(x.fake.StorageClasses, x.body.Workspace.StorageClass)
		if got := x.observe(o.Handle); got.Phase != execution.PhasePending {
			t.Fatalf("waiting claim, no class: %+v", got)
		}
		x.fake.BindClaim(x.ns, spec.Name)
		if got := x.observe(o.Handle); got.Phase != execution.PhaseUnknown {
			t.Fatalf("bound claim, no class: %+v", got)
		}
	})

	t.Run("a class of another name", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		wffc(e)
		spec := e.specs.Workspace(t)
		o := e.ensure(spec)
		e.fake.MutateRaw("pvc", e.ns, spec.Name, func(d map[string]any) { d["spec"].(map[string]any)["storageClassName"] = "other-class" })
		if _, err := e.rr.Observe(ctx, o.Handle); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("observe %v", err)
		}
	})

	t.Run("an unreadable class is unknown", func(t *testing.T) {
		for _, bind := range []bool{false, true} {
			e := newRunEnv(t, tenantBody())
			wffc(e)
			spec := e.specs.Workspace(t)
			o := e.ensure(spec)
			if bind {
				e.fake.BindClaim(e.ns, spec.Name)
			}
			e.fake.Fault = outage("get storageclass")
			got, err := e.rr.Observe(ctx, o.Handle)
			if err != nil || got.Phase != execution.PhaseUnknown || got.Fingerprint != "" {
				t.Fatalf("bound=%v: %+v %v", bind, got, err)
			}
			// Ensure of an owned claim reports what Observe does: never ready on an outage.
			if o2, err := e.rr.Ensure(ctx, spec); err == nil && o2.Phase == execution.PhaseReady {
				t.Fatalf("bound=%v: ready on a class outage", bind)
			}
		}
	})

	t.Run("a claim that never bound is released and removed", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		wffc(e)
		spec := e.specs.Workspace(t)
		o := e.ensure(spec)
		if ev := e.stop(o.Handle); !ev.Stopped || !ev.Released {
			t.Fatalf("stop %+v", ev)
		}
		if got := e.remove(o.Handle); !got.Removed {
			t.Fatalf("remove %+v", got)
		}
		if len(e.fake.Names("pvc", e.ns))+len(liveSecrets(e)) != 0 {
			t.Fatal("objects left")
		}
	})

	t.Run("a volume that appears after the claim was deleted keeps cleanup pending", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		wffc(e)
		spec := e.specs.Workspace(t)
		o := e.ensure(spec)
		e.stop(o.Handle)
		// The provisioner finishes after the claim was last read: the volume is bound to
		// this claim UID though no read ever showed one.
		e.fake.HoldPVs = true
		e.fake.PutPV("pvc-"+o.Handle.UID, e.ns, spec.Name, o.Handle.UID)
		if got := e.remove(o.Handle); got.Removed {
			t.Fatal("removed while a volume of the claim exists")
		}
		if _, ok := e.fake.SecretOf(e.ns, spec.Name+"-rec"); !ok {
			t.Fatal("the record was deleted before the volume was gone")
		}
		e.fake.ReleasePVs()
		if got := e.remove(o.Handle); !got.Removed {
			t.Fatalf("after the volume is gone: %+v", got)
		}
		// A volume of that name bound to another claim is a collision, never ours.
		x := newRunEnv(t, tenantBody())
		wffc(x)
		o2 := x.ensure(x.specs.Workspace(t))
		x.stop(o2.Handle)
		x.fake.PutPV("pvc-"+o2.Handle.UID, x.ns, "someone-elses-claim", "00000000-0000-4000-8000-0000000000cc")
		if _, err := x.rr.Remove(ctx, o2.Handle); !errors.Is(err, execution.ErrResourceCollision) {
			t.Fatalf("remove %v", err)
		}
	})

	t.Run("a claim that binds between the Pod list and the volume read is still fenced", func(t *testing.T) {
		e := newRunEnv(t, tenantBody())
		wffc(e)
		spec := e.specs.Workspace(t)
		o := e.ensure(spec)
		var vol string
		e.fake.Fault = func(op string, n int) *kubetestFault {
			// The claim binds and attaches right after the Pods were listed.
			if op == "list pods" && vol == "" {
				vol = e.fake.BindClaim(e.ns, spec.Name)
				e.fake.Attach(vol)
			}
			return nil
		}
		if got := e.observe(o.Handle); got.Released {
			t.Fatalf("released although the claim bound and attached: %+v", got)
		}
	})
}
