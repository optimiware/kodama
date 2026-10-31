package kernelrt

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Finding 1: compensation is bound to the UID the call created and to durable
// evidence that nobody else established or adopted it.
func Test_unit_kube_kernel_runtime_compensation_is_bound_to_the_created_instance(t *testing.T) {
	ctx := context.Background()

	t.Run("a newer writer adopted the Job, it completed and was stopped: its result survives", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		newer := bumpFence(b, 2)
		var stopped runtime.WorkloadObservation
		// A has created the Job and not yet re-read the record.
		h.cl.onAfter("create job", func() {
			if _, err := h.start(h.startRequest(newer, ws)); err != nil {
				t.Errorf("newer start: %v", err)
			}
			h.cl.startPod(testNS, h.jobName(b))
			h.cl.finishJob(testNS, h.jobName(b), 0)
			o, err := h.rt.Stop(ctx, newer)
			if err != nil {
				t.Errorf("newer stop: %v", err)
			}
			stopped = o
		})
		_, err := h.start(h.startRequest(b, ws))
		if !(errors.Is(err, ErrClosed) || errors.Is(err, runtime.ErrStaleFence)) {
			t.Fatalf("A = %v", err)
		}
		if stopped.Phase != runtime.PhaseExited || stopped.Result.Code != runtime.ResultSucceeded {
			t.Fatalf("the newer Stop = %+v", stopped)
		}
		if h.cl.deleteCount("job") != 0 {
			t.Fatal("the stale creator reaped an established Job")
		}
		if o, err := h.rt.Observe(ctx, newer); err != nil || o.Phase != runtime.PhaseExited || o.Result.Code != runtime.ResultSucceeded {
			t.Fatalf("the preserved result = %+v %v", o, err)
		}
	})

	t.Run("an instance that replaced the created one is never the one reaped", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		newer := bumpFence(b, 2)
		var created string
		j2 := h.ad.stepJob(workloadIdent(b), h.startRequest(b, ws), h.target(), workspaceIdent(h, "run-1").base(), "", nil)
		h.cl.onAfter("create job", func() {
			job, _ := h.cl.job(testNS, h.jobName(b))
			created = job.Metadata.UID
			// The identity is closed and the Job removed, then another instance takes the name.
			if _, err := h.rt.Stop(ctx, newer); err != nil {
				t.Errorf("newer stop: %v", err)
			}
			h.cl.putJob(testNS, j2)
		})
		if _, err := h.start(h.startRequest(b, ws)); !errors.Is(err, ErrClosed) {
			t.Fatalf("A = %v", err)
		}
		job, ok := h.cl.job(testNS, h.jobName(b))
		if !ok || job.Metadata.UID == created {
			t.Fatalf("the replacement instance must remain: found %v", ok)
		}
		if h.cl.deleteCount("job") != 1 {
			t.Fatalf("deletes = %d, want only the Stop's", h.cl.deleteCount("job"))
		}
	})

	t.Run("an instance claimed for reaping cannot be adopted meanwhile", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		newer := bumpFence(b, 2)
		h.cl.onAfter("create job", func() {
			h.cl.startPod(testNS, h.jobName(b))
			h.cl.finishJob(testNS, h.jobName(b), 0)
			// The identity is closed without anyone having observed the Job.
			if _, err := h.ad.settle(ctx, testNS, workloadIdent(b), newer.Fence, recStopped); err != nil {
				t.Errorf("close: %v", err)
			}
			h.cl.failNext("delete job", &kubernetes.Error{Op: "delete job", Kind: kubernetes.ErrForbidden}, false)
		})
		if _, err := h.start(h.startRequest(b, ws)); !errors.Is(err, ErrClosed) {
			t.Fatalf("A = %v", err)
		}
		if _, err := h.rt.Observe(ctx, newer); !errors.Is(err, ErrUnverified) {
			t.Fatalf("an instance its creator is deleting must not be adopted: %v", err)
		}
		if h.record(workloadIdent(b)).created[opJob] != "" {
			t.Fatal("the doomed instance was established")
		}
	})

	t.Run("workspace: a claim a newer writer adopted and stopped is kept", func(t *testing.T) {
		h := newHarness(t)
		spec := h.workspaceSpec(scopeA, "run-1", 1)
		newer := spec
		newer.Fence = 2
		handle := resourceFence(h.workspaceHandle(scopeA, "run-1"), 2)
		h.cl.onAfter("create pvc", func() {
			if _, err := h.res.Ensure(ctx, newer); err != nil {
				t.Errorf("newer ensure: %v", err)
			}
			if _, err := h.res.Stop(ctx, handle); err != nil {
				t.Errorf("newer stop: %v", err)
			}
		})
		if _, err := h.res.Ensure(ctx, spec); !(errors.Is(err, ErrClosed) || errors.Is(err, runtime.ErrStaleFence)) {
			t.Fatalf("A = %v", err)
		}
		if len(h.cl.names(h.cl.pvcs)) != 1 || h.cl.deleteCount("pvc") != 0 {
			t.Fatal("the stale creator deleted the established claim")
		}
	})

	t.Run("service: an instance that replaced the created Pod is not reaped", func(t *testing.T) {
		h := newHarness(t)
		spec := h.serviceSpec(scopeA, "run-1", "db", 1)
		id := workspaceIdentFor(h, spec)
		handle := runtime.ResourceHandle{Scope: scopeA, RunID: "run-1", Kind: runtime.ResourceService, Name: "db", Identity: spec.Identity, Fence: 2}
		var created string
		h.cl.onAfter("create pod", func() {
			p, _ := h.cl.GetPod(ctx, testNS, id.base())
			created = p.Metadata.UID
			if _, err := h.res.Stop(ctx, handle); err != nil { // closes the identity and deletes the Pod
				t.Errorf("newer stop: %v", err)
			}
			if _, err := h.cl.CreatePod(ctx, testNS, h.ad.servicePod(id, *spec.Service, h.target(), 2)); err != nil {
				t.Errorf("replacement: %v", err)
			}
		})
		if _, err := h.res.Ensure(ctx, spec); !errors.Is(err, ErrClosed) {
			t.Fatalf("A = %v", err)
		}
		p, err := h.cl.GetPod(ctx, testNS, id.base())
		if err != nil || p.Metadata.UID == created {
			t.Fatalf("the replacement Pod must remain: %v", err)
		}
		if h.cl.deleteCount("pod") != 1 {
			t.Fatalf("pod deletes = %d, want only the Stop's", h.cl.deleteCount("pod"))
		}
	})
}

// Finding 2: the committed layout is validated and initialized inside the
// reservation, never overwritten.
func Test_unit_kube_kernel_runtime_committed_layout_is_not_overwritten_by_a_reservation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ws := h.ensureWorkspace(scopeA, "run-1", 1)
	ref := runtime.SecretRef{Owner: domain.Owner{TenantID: "tenant-a"}, Name: "TOKEN"}
	b := binding(scopeA, "run-1", "deploy", 1, "d", 1)
	request := func(dir string) runtime.StartRequest {
		req := h.startRequest(b, ws)
		req.Spec.Secrets = []runtime.SecretRef{ref}
		req.Secrets = runtime.Materialization{Scope: scopeA, RunID: "run-1", StepKey: "deploy", Directory: dir, Refs: []runtime.SecretRef{ref}}
		return req
	}
	// A passes every earlier check against an empty record, then pauses at the
	// write that reserves the Job create (its third record write: the step
	// Secret's reservation, its settlement, then the Job's reservation).
	h.cl.onNext("update secret", func() {})
	h.cl.onNext("update secret", func() {})
	h.cl.onNext("update secret", func() {
		if _, err := h.start(request("/kodama/b")); err != nil {
			t.Errorf("B: %v", err)
		}
	})
	_, err := h.start(request("/kodama/a"))
	if !isNotReached(err) || !errors.Is(err, domain.ErrInvalidValue) {
		t.Fatalf("A must be refused for a different committed layout: %v", err)
	}
	id := workloadIdent(b)
	rec := h.record(id)
	if rec.secretsDir != "/kodama/b" {
		t.Fatalf("the committed layout was overwritten: %q", rec.secretsDir)
	}
	if h.cl.count("create job") != 1 {
		t.Fatalf("creates = %d; the losing call must not create", h.cl.count("create job"))
	}
	if o, err := h.rt.Observe(ctx, b); err != nil || o.Phase != runtime.PhaseRunning {
		t.Fatalf("the established Job must still verify: %+v %v", o, err)
	}
}

// Finding 3: identities discovered through claimRef are recorded even when the
// attachment list cannot be read.
func Test_unit_kube_kernel_runtime_discovered_volumes_survive_an_attachment_list_failure(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	o, _ := h.res.Ensure(ctx, h.workspaceSpec(scopeA, "run-1", 1))
	id := workspaceIdent(h, "run-1")
	uid := h.pvcUID(id)
	pv := kubernetes.PersistentVolume{}
	pv.Spec.PersistentVolumeReclaimPolicy = "Delete"
	pv.Spec.ClaimRef = &kubernetes.ObjectReference{Namespace: testNS, Name: id.base(), UID: uid}
	h.cl.pvs["pv-ref"] = pv
	name := "pv-ref"
	att := kubernetes.VolumeAttachment{}
	att.Spec.Source.PersistentVolumeName = &name
	att.Status.Attached = true
	h.cl.attached = []kubernetes.VolumeAttachment{att}
	h.dropObject(h.cl.pvcs, id) // deleted before anything saw it bound

	h.cl.failNext("list volumeattachments", &kubernetes.Error{Op: "list", Kind: kubernetes.ErrUnavailable}, false)
	if _, err := h.res.Remove(ctx, o.Handle); err == nil {
		t.Fatal("an unreadable attachment list must be an error, never a release")
	}
	if got := h.record(id).volumes; len(got) != 1 || got[0] != "pv-ref" {
		t.Fatalf("the discovered volume was not recorded: %v", got)
	}
	// The volume object vanishes while the node still reports it attached.
	delete(h.cl.pvs, "pv-ref")
	h.clock.Advance(2 * time.Minute)
	if rm, err := h.res.Remove(ctx, o.Handle); err != nil || rm.ConfirmedRemoved() {
		t.Fatalf("an attached volume is not released: %+v %v", rm, err)
	}
	h.cl.attached = nil
	if rm, err := h.res.Remove(ctx, o.Handle); err != nil || !rm.ConfirmedRemoved() {
		t.Fatalf("released = %+v %v", rm, err)
	}
}

// Every write that records an established instance applies the reaping
// exclusion, including the settlement that ends a create.
func Test_unit_kube_kernel_runtime_settlement_respects_a_reaping_claim(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	ws := h.ensureWorkspace(scopeA, "run-1", 1)
	b := binding(scopeA, "run-1", "build", 1, "d", 1)
	newer := bumpFence(b, 2)
	id := workloadIdent(b)
	j := h.ad.stepJob(id, h.startRequest(b, ws), h.target(), workspaceIdent(h, "run-1").base(), "", nil)

	// A reserves, then its create finds the name taken by an instance that an
	// earlier request made: it adopts through AlreadyExists.
	h.cl.onNext("create job", func() { h.cl.putJob(testNS, j) })
	// A's writes: the reservation, then the settlement. A pauses at the
	// settlement, after its authority reread showed an active identity.
	h.cl.onNext("update secret", func() {})
	h.cl.onNext("update secret", func() {
		job, _ := h.cl.job(testNS, h.jobName(b))
		// The identity closes and the instance's creator claims it for reaping.
		if _, err := h.ad.settle(ctx, testNS, id, newer.Fence, recStopped); err != nil {
			t.Errorf("close: %v", err)
		}
		if ok, err := h.ad.claimReap(ctx, testNS, id, opJob, job.Metadata.UID); err != nil || !ok {
			t.Errorf("claimReap = %v %v", ok, err)
		}
	})
	_, err := h.start(h.startRequest(b, ws))
	if !errors.Is(err, ErrUnverified) {
		t.Fatalf("the adopter's establishment must be refused: %v", err)
	}
	rec := h.record(id)
	if rec.created[opJob] != "" {
		t.Fatalf("an instance under a reaping claim was established: %v", rec.created)
	}
	if rec.marked(opJob) {
		t.Fatal("the adopter's own reservation must still be settled")
	}
	if _, err := h.rt.Observe(ctx, newer); !errors.Is(err, ErrUnverified) {
		t.Fatalf("observation must not accept the doomed instance: %v", err)
	}
}

// A settlement refusal after this call's create was applied is uncertain, never
// "not reached", and the created object is accounted for.
func Test_unit_kube_kernel_runtime_applied_create_is_never_reported_not_reached(t *testing.T) {
	ctx := context.Background()

	t.Run("workload", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		// A pauses before its create. Meanwhile an overlapping call creates and
		// establishes an instance, which then disappears.
		h.cl.onNext("create job", func() {
			if _, err := h.start(h.startRequest(b, ws)); err != nil {
				t.Errorf("overlapping start: %v", err)
			}
			h.cl.mu.Lock()
			delete(h.cl.jobs, key(testNS, h.jobName(b)))
			h.cl.mu.Unlock()
		})
		_, err := h.start(h.startRequest(b, ws))
		if runtime.ClassifyEffect(err) != runtime.CommandUncertain || isNotReached(err) || !errors.Is(err, ErrUnverified) {
			t.Fatalf("an applied create must be uncertain: %v", err)
		}
		if h.cl.count("create job") != 2 {
			t.Fatalf("creates = %d", h.cl.count("create job"))
		}
		// The instance this call created is accounted for: deleted by its UID, not
		// left as a silent replacement of the lost one.
		if _, ok := h.cl.job(testNS, h.jobName(b)); ok {
			t.Fatal("the conflicting instance was left behind")
		}
		if h.cl.deleteCount("job") != 1 {
			t.Fatalf("deletes = %d", h.cl.deleteCount("job"))
		}
	})

	t.Run("workspace", func(t *testing.T) {
		h := newHarness(t)
		spec := h.workspaceSpec(scopeA, "run-1", 1)
		id := workspaceIdent(h, "run-1")
		h.cl.onNext("create pvc", func() {
			if _, err := h.res.Ensure(ctx, spec); err != nil {
				t.Errorf("overlapping ensure: %v", err)
			}
			h.dropObject(h.cl.pvcs, id)
		})
		_, err := h.res.Ensure(ctx, spec)
		if runtime.ClassifyEffect(err) != runtime.CommandUncertain || isNotReached(err) || !errors.Is(err, ErrUnverified) {
			t.Fatalf("an applied create must be uncertain: %v", err)
		}
		if h.cl.count("create pvc") != 2 || len(h.cl.names(h.cl.pvcs)) != 0 || h.cl.deleteCount("pvc") != 1 {
			t.Fatalf("creates %d claims %v deletes %d", h.cl.count("create pvc"), h.cl.names(h.cl.pvcs), h.cl.deleteCount("pvc"))
		}
	})

	t.Run("workspace: a conflicting claim that bound before compensation keeps its volume evidence", func(t *testing.T) {
		h := newHarness(t)
		spec := h.workspaceSpec(scopeA, "run-1", 1)
		id := workspaceIdent(h, "run-1")
		handle := h.workspaceHandle(scopeA, "run-1")
		var conflicting string
		h.cl.onNext("create pvc", func() {
			if _, err := h.res.Ensure(ctx, spec); err != nil { // establishes claim B
				t.Errorf("overlapping ensure: %v", err)
			}
			h.dropObject(h.cl.pvcs, id) // B disappears
		})
		// The overlapping call's create passes through its own after-hook first.
		h.cl.onAfter("create pvc", func() {})
		h.cl.onAfter("create pvc", func() {
			// This call's claim A is being bound to a volume the cluster keeps, still
			// attached: the volume names it by claimRef, but the claim does not yet
			// carry the volume name, so the name is not known to the adapter.
			conflicting = h.pvcUID(id)
			pv := kubernetes.PersistentVolume{}
			pv.Spec.PersistentVolumeReclaimPolicy = "Retain"
			pv.Spec.ClaimRef = &kubernetes.ObjectReference{Namespace: testNS, Name: id.base(), UID: conflicting}
			h.cl.pvs["pv-a"] = pv
			name := "pv-a"
			att := kubernetes.VolumeAttachment{}
			att.Spec.Source.PersistentVolumeName = &name
			att.Status.Attached = true
			h.cl.attached = []kubernetes.VolumeAttachment{att}
		})
		_, err := h.res.Ensure(ctx, spec)
		if runtime.ClassifyEffect(err) != runtime.CommandUncertain {
			t.Fatalf("err = %v", err)
		}
		// Repeated Remove, before and after quiescence, never confirms: the volume
		// is retained and attached.
		for i := 0; i < 3; i++ {
			o, err := h.res.Remove(ctx, handle)
			if err != nil || o.ConfirmedRemoved() {
				t.Fatalf("remove %d confirmed cleanup with a surviving volume: %+v %v", i, o, err)
			}
			h.clock.Advance(2 * time.Minute)
		}
		if ev, _ := h.res.ReleaseEvidence(ctx, handle); ev.Released() || !ev.Retained || !ev.Attached {
			t.Fatalf("evidence = %+v", ev)
		}
		rec := h.record(id)
		if len(rec.compensated) != 1 || rec.compensated[0] != conflicting || len(rec.volumes) != 1 || rec.volumes[0] != "pv-a" {
			t.Fatalf("cleanup evidence not recorded: compensated %v volumes %v", rec.compensated, rec.volumes)
		}
		if rec.created[opPVC] == conflicting {
			t.Fatal("the compensated claim must not become the established one")
		}
		// The same with the volume object gone but the attachment still reported.
		delete(h.cl.pvs, "pv-a")
		if o, err := h.res.Remove(ctx, handle); err != nil || o.ConfirmedRemoved() {
			t.Fatalf("an attached volume without its PV object = %+v %v", o, err)
		}
		h.cl.attached = nil
		if o, err := h.res.Remove(ctx, handle); err != nil || !o.ConfirmedRemoved() {
			t.Fatalf("released = %+v %v", o, err)
		}
	})

	t.Run("an adopted object is still not reached", func(t *testing.T) {
		// The adopter made no create effect; the refusal stays a not-reached one.
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		id := workloadIdent(b)
		j := h.ad.stepJob(id, h.startRequest(b, ws), h.target(), workspaceIdent(h, "run-1").base(), "", nil)
		h.cl.onNext("create job", func() { h.cl.putJob(testNS, j) })
		h.cl.onNext("update secret", func() {})
		h.cl.onNext("update secret", func() {
			job, _ := h.cl.job(testNS, h.jobName(b))
			if _, err := h.ad.settle(ctx, testNS, id, 2, recStopped); err != nil {
				t.Errorf("close: %v", err)
			}
			if ok, err := h.ad.claimReap(ctx, testNS, id, opJob, job.Metadata.UID); err != nil || !ok {
				t.Errorf("claimReap = %v %v", ok, err)
			}
		})
		if _, err := h.start(h.startRequest(b, ws)); !isNotReached(err) || !errors.Is(err, ErrUnverified) {
			t.Fatalf("err = %v", err)
		}
	})
}

// A claim that was created and compensated is evidence that a claim was issued,
// even when no instance was ever established.
func Test_unit_kube_kernel_runtime_first_create_after_stop_keeps_the_binding_window_open(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	spec := h.workspaceSpec(scopeA, "run-1", 1)
	id := workspaceIdent(h, "run-1")
	handle := resourceFence(h.workspaceHandle(scopeA, "run-1"), 2)
	// The first create lands after a newer writer stopped the workspace.
	h.cl.onNext("create pvc", func() {
		if _, err := h.res.Stop(ctx, handle); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	if _, err := h.res.Ensure(ctx, spec); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ensure = %v", err)
	}
	rec := h.record(id)
	if rec.created[opPVC] != "" || len(rec.compensated) != 1 {
		t.Fatalf("the test needs compensation evidence only: created %v compensated %v", rec.created, rec.compensated)
	}
	if len(h.cl.names(h.cl.pvcs)) != 0 {
		t.Fatal("the compensated claim remains")
	}
	// An immediate Remove cannot yet rule out a binding in progress.
	o, err := h.res.Remove(ctx, handle)
	if err != nil || o.ConfirmedRemoved() {
		t.Fatalf("immediate remove confirmed: %+v %v", o, err)
	}
	if ev, _ := h.res.ReleaseEvidence(ctx, handle); !ev.Unconfirmed {
		t.Fatalf("evidence = %+v", ev)
	}
	// The volume the claim was being bound to becomes visible.
	pv := kubernetes.PersistentVolume{}
	pv.Spec.PersistentVolumeReclaimPolicy = "Delete"
	pv.Spec.ClaimRef = &kubernetes.ObjectReference{Namespace: testNS, Name: id.base(), UID: rec.compensated[0]}
	h.cl.pvs["pv-late"] = pv
	for i := 0; i < 2; i++ {
		h.clock.Advance(2 * time.Minute)
		if o, err := h.res.Remove(ctx, handle); err != nil || o.ConfirmedRemoved() {
			t.Fatalf("remove %d confirmed with a volume for the compensated claim: %+v %v", i, o, err)
		}
	}
	delete(h.cl.pvs, "pv-late")
	if o, err := h.res.Remove(ctx, handle); err != nil || !o.ConfirmedRemoved() {
		t.Fatalf("released = %+v %v", o, err)
	}
}

// The binding window is measured from the latest evidence event: an earlier
// recorded absence never shortens the window of a later compensated claim.
func Test_unit_kube_kernel_runtime_binding_window_runs_from_the_latest_evidence(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t) // quiescence window: one minute
	spec := h.workspaceSpec(scopeA, "run-1", 1)
	id := workspaceIdent(h, "run-1")
	handle := resourceFence(h.workspaceHandle(scopeA, "run-1"), 2)
	// Remove (not Stop) lands while the first create is outstanding and records
	// the claim's absence at t=0; the delayed create then lands and is
	// compensated 45s later.
	h.cl.onNext("create pvc", func() {
		if o, err := h.res.Remove(ctx, handle); err != nil || o.ConfirmedRemoved() {
			t.Errorf("remove during the create = %+v %v", o, err)
		}
		h.clock.Advance(45 * time.Second)
	})
	if _, err := h.res.Ensure(ctx, spec); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ensure = %v", err)
	}
	rec := h.record(id)
	if len(rec.compensated) != 1 || rec.claimGone.IsZero() {
		t.Fatalf("compensated %v claimGone %v", rec.compensated, rec.claimGone)
	}
	compensatedAt := h.clock.Now()
	if !rec.claimGone.Equal(compensatedAt) {
		t.Fatalf("the window runs from the compensation (%v), not the earlier absence: %v", compensatedAt, rec.claimGone)
	}
	// 20s after the compensating deletion, and again just inside the window.
	for _, d := range []time.Duration{20 * time.Second, 39 * time.Second} {
		h.clock.Advance(d)
		if o, err := h.res.Remove(ctx, handle); err != nil || o.ConfirmedRemoved() {
			t.Fatalf("%v after compensation: confirmed inside the window: %+v %v", h.clock.Now().Sub(compensatedAt), o, err)
		}
	}
	h.clock.Advance(2 * time.Second)
	if o, err := h.res.Remove(ctx, handle); err != nil || !o.ConfirmedRemoved() {
		t.Fatalf("after the full window = %+v %v", o, err)
	}
}

// A repeated observation of the same absence does not extend the window, and a
// later event never moves the timestamp backwards.
func Test_unit_kube_kernel_runtime_binding_window_is_monotonic(t *testing.T) {
	var r record
	r.advanceClaimGone(time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC))
	r.advanceClaimGone(time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)) // an older event
	if !r.claimGone.Equal(time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)) {
		t.Fatalf("claimGone moved backwards: %v", r.claimGone)
	}
}
