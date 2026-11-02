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

// Finding 1: a settlement clears only its own reservation.
func Test_unit_kube_kernel_runtime_overlapping_creates_keep_their_own_evidence(t *testing.T) {
	ctx := context.Background()
	unknown := &kubernetes.Error{Op: "create", Kind: kubernetes.ErrUnknownOutcome}
	forbidden := &kubernetes.Error{Op: "create", Kind: kubernetes.ErrForbidden, Status: 403}

	t.Run("workload: a definite rejection does not erase another invocation's uncertain create", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		// B's create is uncertain; A's, issued after, is definitely refused.
		h.cl.failNext("create job", unknown, false)
		h.cl.failNext("create job", forbidden, false)
		var inner error
		h.cl.onNext("create job", func() { _, inner = h.start(h.startRequest(b, ws)) })
		_, outer := h.start(h.startRequest(b, ws))
		if runtime.ClassifyEffect(inner) != runtime.CommandUncertain {
			t.Fatalf("B = %v", inner)
		}
		if !isNotReached(outer) {
			t.Fatalf("A = %v", outer)
		}
		if o, err := h.rt.Remove(ctx, b); err != nil || o.Phase != runtime.PhaseUnknown {
			t.Fatalf("B's create may still land: remove = %+v %v", o, err)
		}
		// B's delayed create lands after Remove returned; the next Remove deletes it.
		j := h.ad.stepJob(workloadIdent(b), h.startRequest(b, ws), h.target(), workspaceIdent(h, "run-1").base(), "", nil)
		if _, err := h.cl.CreateJob(ctx, testNS, j); err != nil {
			t.Fatal(err)
		}
		if o, err := h.rt.Remove(ctx, b); err != nil || o.Phase == runtime.PhaseAbsent {
			t.Fatalf("remove with the late Job inside the window = %+v %v", o, err)
		}
		if len(h.cl.names(h.cl.jobs)) != 0 {
			t.Fatal("the late Job remains")
		}
		h.clock.Advance(2 * time.Minute)
		if o, err := h.rt.Remove(ctx, b); err != nil || o.Phase != runtime.PhaseAbsent {
			t.Fatalf("final remove = %+v %v", o, err)
		}
	})

	t.Run("workspace: the same interleaving", func(t *testing.T) {
		h := newHarness(t)
		spec := h.workspaceSpec(scopeA, "run-1", 1)
		handle := h.workspaceHandle(scopeA, "run-1")
		h.cl.failNext("create pvc", unknown, false)
		h.cl.failNext("create pvc", forbidden, false)
		var inner error
		h.cl.onNext("create pvc", func() { _, inner = h.res.Ensure(ctx, spec) })
		_, outer := h.res.Ensure(ctx, spec)
		if runtime.ClassifyEffect(inner) != runtime.CommandUncertain || !isNotReached(outer) {
			t.Fatalf("B = %v, A = %v", inner, outer)
		}
		if o, err := h.res.Remove(ctx, handle); err != nil || o.ConfirmedRemoved() {
			t.Fatalf("B's create may still land: remove = %+v %v", o, err)
		}
		h.clock.Advance(2 * time.Minute)
		if o, err := h.res.Remove(ctx, handle); err != nil || !o.ConfirmedRemoved() {
			t.Fatalf("final remove = %+v %v", o, err)
		}
	})
}

// Finding 2: a stale creator never reaps a newer writer's established object.
func Test_unit_kube_kernel_runtime_stale_creator_does_not_reap_a_newer_writers_object(t *testing.T) {
	ctx := context.Background()

	t.Run("workload", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		newer := bumpFence(b, 2)
		// A reserves at fence 1 and pauses just before its create; B, a newer
		// writer, starts the workload; A resumes and finds the name taken.
		h.cl.onNext("create job", func() {
			if _, err := h.start(h.startRequest(newer, ws)); err != nil {
				t.Errorf("newer start: %v", err)
			}
		})
		_, err := h.start(h.startRequest(b, ws))
		if !errors.Is(err, runtime.ErrStaleFence) {
			t.Fatalf("A = %v", err)
		}
		if _, ok := h.cl.job(testNS, h.jobName(b)); !ok || h.cl.deleteCount("job") != 0 {
			t.Fatal("the stale creator deleted the newer writer's Job")
		}
		if o, err := h.rt.Observe(ctx, newer); err != nil || o.Phase != runtime.PhaseRunning {
			t.Fatalf("the newer writer's workload = %+v %v", o, err)
		}
	})

	t.Run("workload: a closed identity's adopted object is left to Remove, not reaped by the adopter", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		newer := bumpFence(b, 2)
		j := h.ad.stepJob(workloadIdent(b), h.startRequest(b, ws), h.target(), workspaceIdent(h, "run-1").base(), "", nil)
		h.cl.onNext("create job", func() {
			if _, err := h.rt.Stop(ctx, newer); err != nil {
				t.Errorf("stop: %v", err)
			}
			h.cl.putJob(testNS, j) // an earlier request lands now
		})
		if _, err := h.start(h.startRequest(b, ws)); !errors.Is(err, ErrClosed) {
			t.Fatalf("A = %v", err)
		}
		if h.cl.deleteCount("job") != 0 {
			t.Fatal("the adopter deleted an object it did not create")
		}
		if o, err := h.rt.Remove(ctx, newer); err != nil || o.Phase == runtime.PhaseExited {
			t.Fatalf("remove = %+v %v", o, err)
		}
		if h.cl.deleteCount("job") != 1 {
			t.Fatal("Remove must delete the landed Job")
		}
	})

	t.Run("workspace", func(t *testing.T) {
		h := newHarness(t)
		spec := h.workspaceSpec(scopeA, "run-1", 1)
		newer := spec
		newer.Fence = 2
		h.cl.onNext("create pvc", func() {
			if _, err := h.res.Ensure(ctx, newer); err != nil {
				t.Errorf("newer ensure: %v", err)
			}
		})
		if _, err := h.res.Ensure(ctx, spec); !errors.Is(err, runtime.ErrStaleFence) {
			t.Fatalf("A = %v", err)
		}
		if len(h.cl.names(h.cl.pvcs)) != 1 || h.cl.deleteCount("pvc") != 0 {
			t.Fatal("the stale creator deleted the newer writer's claim")
		}
	})

	t.Run("service", func(t *testing.T) {
		h := newHarness(t)
		spec := h.serviceSpec(scopeA, "run-1", "db", 1)
		newer := spec
		newer.Fence = 2
		h.cl.onNext("create pod", func() {
			if _, err := h.res.Ensure(ctx, newer); err != nil {
				t.Errorf("newer ensure: %v", err)
			}
		})
		if _, err := h.res.Ensure(ctx, spec); !errors.Is(err, runtime.ErrStaleFence) {
			t.Fatalf("A = %v", err)
		}
		if len(h.cl.names(h.cl.pods)) != 1 || h.cl.deleteCount("pod") != 0 {
			t.Fatal("the stale creator deleted the newer writer's Pod")
		}
	})
}

// Finding 3: the intended volumes and mounts are required, each exactly once.
func Test_unit_kube_kernel_runtime_adoption_requires_the_intended_volumes(t *testing.T) {
	ctx := context.Background()
	jobSpec := func(d map[string]any) map[string]any { return sub(sub(sub(d, "spec"), "template"), "spec") }
	podSpec := func(d map[string]any) map[string]any { return sub(d, "spec") }
	container := func(s map[string]any) map[string]any { return s["containers"].([]any)[0].(map[string]any) }
	dropNamed := func(list []any, name string) []any {
		var out []any
		for _, x := range list {
			if x.(map[string]any)["name"] != name {
				out = append(out, x)
			}
		}
		return out
	}
	dup := func(list []any, name string) []any {
		for _, x := range list {
			if x.(map[string]any)["name"] == name {
				return append(list, x)
			}
		}
		return list
	}
	removeVolume := func(name string) func(map[string]any) {
		return func(s map[string]any) { s["volumes"] = dropNamed(s["volumes"].([]any), name) }
	}
	removeMount := func(name string) func(map[string]any) {
		return func(s map[string]any) {
			c := container(s)
			c["volumeMounts"] = dropNamed(c["volumeMounts"].([]any), name)
		}
	}
	both := func(fs ...func(map[string]any)) func(map[string]any) {
		return func(s map[string]any) {
			for _, f := range fs {
				f(s)
			}
		}
	}
	cases := map[string]func(map[string]any){
		"workspace volume removed":           removeVolume("workspace"),
		"workspace mount removed":            removeMount("workspace"),
		"workspace volume and mount removed": both(removeVolume("workspace"), removeMount("workspace")),
		"scratch volume and mount removed":   both(removeVolume("tmp"), removeMount("tmp")),
		"workspace volume duplicated":        func(s map[string]any) { s["volumes"] = dup(s["volumes"].([]any), "workspace") },
		"workspace mount duplicated": func(s map[string]any) {
			c := container(s)
			c["volumeMounts"] = dup(c["volumeMounts"].([]any), "workspace")
		},
		"every volume removed": func(s map[string]any) { delete(s, "volumes"); delete(container(s), "volumeMounts") },
	}
	for name, mutate := range cases {
		t.Run("job template: "+name, func(t *testing.T) {
			h := newHarness(t)
			ws := h.ensureWorkspace(scopeA, "run-1", 1)
			b := binding(scopeA, "run-1", "build", 1, "d", 1)
			h.mustStart(h.startRequest(b, ws))
			h.cl.tamper("job", testNS, h.jobName(b), func(d map[string]any) { mutate(jobSpec(d)) })
			if _, err := h.rt.Observe(ctx, b); !errors.Is(err, ErrUnverified) {
				t.Fatalf("observe = %v", err)
			}
			if _, err := h.start(h.startRequest(b, ws)); !errors.Is(err, ErrUnverified) {
				t.Fatalf("adopt = %v", err)
			}
		})
		t.Run("job pod: "+name, func(t *testing.T) {
			h := newHarness(t)
			ws := h.ensureWorkspace(scopeA, "run-1", 1)
			b := binding(scopeA, "run-1", "build", 1, "d", 1)
			h.mustStart(h.startRequest(b, ws))
			pod := h.cl.startPod(testNS, h.jobName(b))
			h.cl.tamper("pod", testNS, pod.Metadata.Name, func(d map[string]any) { mutate(podSpec(d)) })
			if _, err := h.rt.Observe(ctx, b); !errors.Is(err, ErrUnverified) {
				t.Fatalf("observe = %v", err)
			}
		})
	}
	t.Run("service pod: scratch volume or mount removed", func(t *testing.T) {
		for name, mutate := range map[string]func(map[string]any){
			"volume": removeVolume("tmp"), "mount": removeMount("tmp"), "both": both(removeVolume("tmp"), removeMount("tmp")),
			"duplicate": func(s map[string]any) { s["volumes"] = dup(s["volumes"].([]any), "tmp") },
		} {
			h := newHarness(t)
			spec := h.serviceSpec(scopeA, "run-1", "db", 1)
			o, err := h.res.Ensure(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			h.cl.tamper("pod", testNS, workspaceIdentFor(h, spec).base(), func(d map[string]any) { mutate(podSpec(d)) })
			if _, err := h.res.Observe(ctx, o.Handle); !errors.Is(err, ErrUnverified) {
				t.Fatalf("%s: observe = %v", name, err)
			}
		}
	})

	// The step secrets are expected because the workload's committed record says
	// so, not because the adopted document mentions them.
	secretsRequest := func(h *harness, ws runtime.ResourceHandle, step string) (runtime.RuntimeBinding, runtime.StartRequest) {
		ref := runtime.SecretRef{Owner: domain.Owner{TenantID: "tenant-a"}, Name: "TOKEN"}
		b := binding(scopeA, "run-1", step, 1, "d", 1)
		req := h.startRequest(b, ws)
		req.Spec.Secrets = []runtime.SecretRef{ref}
		req.Secrets = runtime.Materialization{Scope: scopeA, RunID: "run-1", StepKey: domain.StepID(step), Directory: "/kodama/secrets", Refs: []runtime.SecretRef{ref}}
		return b, req
	}
	t.Run("job template: secret volume and mount removed", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b, req := secretsRequest(h, ws, "deploy")
		h.mustStart(req)
		h.cl.tamper("job", testNS, h.jobName(b), func(d map[string]any) { both(removeVolume("secrets"), removeMount("secrets"))(jobSpec(d)) })
		if _, err := h.rt.Observe(ctx, b); !errors.Is(err, ErrUnverified) {
			t.Fatalf("observe = %v", err)
		}
	})
	t.Run("job pod: secret volume and mount removed", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b, req := secretsRequest(h, ws, "deploy")
		h.mustStart(req)
		pod := h.cl.startPod(testNS, h.jobName(b))
		h.cl.tamper("pod", testNS, pod.Metadata.Name, func(d map[string]any) { both(removeVolume("secrets"), removeMount("secrets"))(podSpec(d)) })
		if _, err := h.rt.Observe(ctx, b); !errors.Is(err, ErrUnverified) {
			t.Fatalf("observe = %v", err)
		}
	})
	t.Run("job template: secret mounted elsewhere", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b, req := secretsRequest(h, ws, "deploy")
		h.mustStart(req)
		h.cl.tamper("job", testNS, h.jobName(b), func(d map[string]any) {
			for _, m := range container(jobSpec(d))["volumeMounts"].([]any) {
				if mm := m.(map[string]any); mm["name"] == "secrets" {
					mm["mountPath"] = "/etc/secrets"
				}
			}
		})
		if _, err := h.rt.Observe(ctx, b); !errors.Is(err, ErrUnverified) {
			t.Fatalf("observe = %v", err)
		}
	})
	t.Run("job template: a secret volume added where none was committed", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.mustStart(h.startRequest(b, ws))
		id := workloadIdent(b)
		h.cl.tamper("job", testNS, h.jobName(b), func(d map[string]any) {
			s := jobSpec(d)
			s["volumes"] = append(s["volumes"].([]any), map[string]any{"name": "secrets", "secret": map[string]any{"secretName": id.materialName(), "defaultMode": 288, "optional": false}})
			c := container(s)
			c["volumeMounts"] = append(c["volumeMounts"].([]any), map[string]any{"name": "secrets", "mountPath": "/kodama/secrets", "readOnly": true})
		})
		if _, err := h.rt.Observe(ctx, b); !errors.Is(err, ErrUnverified) {
			t.Fatalf("observe = %v", err)
		}
	})
}

// Finding 4: attachments are judged against the volumes the claim was or may
// have been bound to, not against the volumes that still exist.
func Test_unit_kube_kernel_runtime_attachment_outlives_its_volume(t *testing.T) {
	ctx := context.Background()
	attach := func(h *harness, pv string) {
		name := pv
		a := kubernetes.VolumeAttachment{}
		a.Spec.Source.PersistentVolumeName = &name
		a.Status.Attached = true
		h.cl.attached = []kubernetes.VolumeAttachment{a}
	}

	t.Run("a volume seen bound", func(t *testing.T) {
		h := newHarness(t)
		o, _ := h.res.Ensure(ctx, h.workspaceSpec(scopeA, "run-1", 1))
		id := workspaceIdent(h, "run-1")
		uid := h.pvcUID(id)
		h.cl.setPVC(testNS, id.base(), func(p *kubernetes.PersistentVolumeClaim) { p.Spec.VolumeName = "pv-1"; p.Status.Phase = "Bound" })
		pv := kubernetes.PersistentVolume{}
		pv.Spec.PersistentVolumeReclaimPolicy = "Delete"
		pv.Spec.ClaimRef = &kubernetes.ObjectReference{Namespace: testNS, Name: id.base(), UID: uid}
		h.cl.pvs["pv-1"] = pv
		attach(h, "pv-1")
		if rm, err := h.res.Remove(ctx, o.Handle); err != nil || rm.ConfirmedRemoved() {
			t.Fatalf("remove = %+v %v", rm, err)
		}
		// The volume object goes away while the node still reports it attached.
		delete(h.cl.pvs, "pv-1")
		h.clock.Advance(2 * time.Minute)
		rm, err := h.res.Remove(ctx, o.Handle)
		if err != nil || rm.ConfirmedRemoved() {
			t.Fatalf("an attached volume without its PV object is not released: %+v %v", rm, err)
		}
		if ev, _ := h.res.ReleaseEvidence(ctx, o.Handle); !ev.Attached || ev.Released() {
			t.Fatalf("evidence = %+v", ev)
		}
		h.cl.attached = nil
		if rm, err := h.res.Remove(ctx, o.Handle); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("released = %+v %v", rm, err)
		}
	})

	t.Run("a volume found only through its claimRef", func(t *testing.T) {
		h := newHarness(t)
		o, _ := h.res.Ensure(ctx, h.workspaceSpec(scopeA, "run-1", 1))
		id := workspaceIdent(h, "run-1")
		uid := h.pvcUID(id)
		pv := kubernetes.PersistentVolume{}
		pv.Spec.PersistentVolumeReclaimPolicy = "Delete"
		pv.Spec.ClaimRef = &kubernetes.ObjectReference{Namespace: testNS, Name: id.base(), UID: uid}
		h.cl.pvs["pv-ref"] = pv
		h.dropObject(h.cl.pvcs, id) // the claim is deleted before anything saw it bound
		attach(h, "pv-ref")
		if rm, err := h.res.Remove(ctx, o.Handle); err != nil || rm.ConfirmedRemoved() {
			t.Fatalf("remove = %+v %v", rm, err)
		}
		delete(h.cl.pvs, "pv-ref")
		h.clock.Advance(2 * time.Minute)
		if rm, err := h.res.Remove(ctx, o.Handle); err != nil || rm.ConfirmedRemoved() {
			t.Fatalf("the volume found through its claimRef is still attached: %+v %v", rm, err)
		}
		h.cl.attached = nil
		if rm, err := h.res.Remove(ctx, o.Handle); err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("released = %+v %v", rm, err)
		}
	})
}

// Finding 5: a successful observation after an uncertain create establishes the
// instance durably, so a later loss is never papered over.
func Test_unit_kube_kernel_runtime_observation_establishes_the_instance(t *testing.T) {
	ctx := context.Background()
	unknown := &kubernetes.Error{Op: "create", Kind: kubernetes.ErrUnknownOutcome}

	t.Run("workspace", func(t *testing.T) {
		h := newHarness(t)
		spec := h.workspaceSpec(scopeA, "run-1", 1)
		h.cl.failNext("create pvc", unknown, true) // applied, answer lost
		if _, err := h.res.Ensure(ctx, spec); runtime.ClassifyEffect(err) != runtime.CommandUncertain {
			t.Fatalf("err = %v", err)
		}
		handle := h.workspaceHandle(scopeA, "run-1")
		if o, err := h.res.Observe(ctx, handle); err != nil || o.State != runtime.ResourceReady {
			t.Fatalf("observe = %+v %v", o, err)
		}
		h.dropObject(h.cl.pvcs, workspaceIdent(h, "run-1"))
		// The lost create's window is still open, so the state is unknown; the
		// point is that no empty replacement is created, now or later.
		o, err := h.res.Ensure(ctx, spec)
		if err != nil || o.State == runtime.ResourceReady || h.cl.count("create pvc") != 1 {
			t.Fatalf("ensure after loss = %+v %v creates %d", o, err, h.cl.count("create pvc"))
		}
		h.clock.Advance(2 * time.Minute)
		o, err = h.res.Ensure(ctx, spec)
		if err != nil || o.State != runtime.ResourceFailed || o.Reason != runtime.ReasonWorkspaceLost || h.cl.count("create pvc") != 1 {
			t.Fatalf("ensure after the window = %+v %v creates %d", o, err, h.cl.count("create pvc"))
		}
	})

	t.Run("service", func(t *testing.T) {
		h := newHarness(t)
		spec := h.serviceSpec(scopeA, "run-1", "db", 1)
		id := workspaceIdentFor(h, spec)
		h.cl.failNext("create pod", unknown, true)
		if _, err := h.res.Ensure(ctx, spec); runtime.ClassifyEffect(err) != runtime.CommandUncertain {
			t.Fatalf("err = %v", err)
		}
		handle := h.ad.handleOf(id, 1)
		if o, err := h.res.Observe(ctx, handle); err != nil || o.State != runtime.ResourceStarting {
			t.Fatalf("observe = %+v %v", o, err)
		}
		h.dropObject(h.cl.pods, id)
		o, err := h.res.Ensure(ctx, spec)
		if err != nil || o.State == runtime.ResourceStarting || o.State == runtime.ResourceReady || h.cl.count("create pod") != 1 {
			t.Fatalf("ensure after loss = %+v %v creates %d", o, err, h.cl.count("create pod"))
		}
		h.clock.Advance(2 * time.Minute)
		o, err = h.res.Ensure(ctx, spec)
		if err != nil || o.State != runtime.ResourceFailed || o.Reason != runtime.ReasonServiceLost || h.cl.count("create pod") != 1 {
			t.Fatalf("ensure after the window = %+v %v creates %d", o, err, h.cl.count("create pod"))
		}
	})

	t.Run("service object", func(t *testing.T) {
		h := newHarness(t)
		spec := h.serviceSpec(scopeA, "run-1", "db", 1)
		id := workspaceIdentFor(h, spec)
		h.cl.failNext("create service", unknown, true)
		_, _ = h.res.Ensure(ctx, spec)
		h.cl.setPod(testNS, id.base(), func(p *kubernetes.Pod) {
			p.Status.Phase = "Running"
			p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: "service", Ready: true}}
		})
		handle := h.ad.handleOf(id, 1)
		if o, err := h.res.Observe(ctx, handle); err != nil || o.State != runtime.ResourceReady {
			t.Fatalf("observe = %+v %v", o, err)
		}
		h.dropObject(h.cl.services, id)
		o, err := h.res.Ensure(ctx, spec)
		if err != nil || o.State != runtime.ResourceFailed || o.Reason != runtime.ReasonServiceLost || h.cl.count("create service") != 1 {
			t.Fatalf("ensure after loss = %+v %v creates %d", o, err, h.cl.count("create service"))
		}
	})

	t.Run("workload", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.cl.failNext("create job", unknown, true)
		if _, err := h.start(h.startRequest(b, ws)); runtime.ClassifyEffect(err) != runtime.CommandUncertain {
			t.Fatalf("err = %v", err)
		}
		if o, err := h.rt.Observe(ctx, b); err != nil || o.Phase != runtime.PhaseRunning {
			t.Fatalf("observe = %+v %v", o, err)
		}
		h.cl.mu.Lock()
		delete(h.cl.jobs, key(testNS, h.jobName(b)))
		h.cl.mu.Unlock()
		o, err := h.start(h.startRequest(b, ws))
		if err != nil || o.Phase == runtime.PhaseRunning || h.cl.count("create job") != 1 {
			t.Fatalf("start after loss = %+v %v creates %d", o, err, h.cl.count("create job"))
		}
	})
}

// Finding 6: an address block is judged by the addresses it allows, not by its
// spelling.
func Test_unit_kube_kernel_runtime_network_blocks_are_judged_by_their_addresses(t *testing.T) {
	ctx := context.Background()
	web := []kubernetes.NetworkPort{{Protocol: "TCP", Port: float64(80)}, {Protocol: "TCP", Port: float64(443)}}
	block := func(cidr string, except []string, ports []kubernetes.NetworkPort) kubernetes.NetworkPolicy {
		return kubernetes.NetworkPolicy{Metadata: kubernetes.ObjectMeta{Name: "egress"}, Spec: kubernetes.NetworkPolicySpec{
			PolicyTypes: []string{"Egress"},
			Egress:      []kubernetes.NetworkEgress{{To: []kubernetes.NetworkPeer{{IPBlock: &kubernetes.IPBlock{CIDR: cidr, Except: except}}}, Ports: ports}},
		}}
	}
	admitted := func(t *testing.T, p ...kubernetes.NetworkPolicy) error {
		h := newHarness(t, func(c *Config) { c.NetworkPolicyEnforced = true })
		h.cl.netpols[testNS] = append([]kubernetes.NetworkPolicy{defaultDeny()}, p...)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		req := h.startRequest(binding(scopeA, "run-1", "net", 1, "d", 1), ws)
		req.Spec.Requires = runtime.CapabilitySet{runtime.CapNetworkIsolation}
		_, err := h.rt.Start(ctx, req)
		if err != nil && !(errors.Is(err, runtime.ErrCapability) && isNotReached(err)) {
			t.Fatalf("a refusal must be a not-reached capability error: %v", err)
		}
		return err
	}
	lowHalf := []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8"}
	highHalf := []string{"169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/3"}

	refused := map[string][]kubernetes.NetworkPolicy{
		"split default route, low half":  {block("0.0.0.0/1", nil, web)},
		"split default route, both":      {block("0.0.0.0/1", nil, web), block("128.0.0.0/1", nil, web)},
		"split with one missing except":  {block("0.0.0.0/1", lowHalf[:3], web), block("128.0.0.0/1", highHalf, web)},
		"a /8 private range":             {block("10.0.0.0/8", nil, web)},
		"a private subnet":               {block("10.1.2.0/24", nil, web)},
		"a private range on port 5432":   {block("192.168.0.0/16", nil, []kubernetes.NetworkPort{{Protocol: "TCP", Port: float64(5432)}})},
		"a /12 containing 172.16/12":     {block("172.16.0.0/10", nil, web)},
		"link-local metadata":            {block("169.254.169.254/32", nil, web)},
		"loopback":                       {block("127.0.0.0/8", nil, web)},
		"public block without ports":     {block("203.0.113.0/24", nil, nil)},
		"public block with a port range": {block("203.0.113.0/24", nil, []kubernetes.NetworkPort{{Protocol: "TCP", Port: float64(1), EndPort: ptr(int32(65535))}})},
		"unparsable":                     {block("not-a-cidr", nil, web)},
		"unparsable except":              {block("203.0.113.0/24", []string{"zzz"}, web)},
		"ipv6 everything":                {block("::/0", nil, web)},
		"ipv6 unique local":              {block("fd00::/8", nil, web)},
		"ipv6 link-local":                {block("fe80::/10", nil, web)},
		"ipv6 loopback":                  {block("::1/128", nil, web)},
		"ipv6 mapped private v4":         {block("::ffff:10.0.0.0/104", nil, web)},
		"ipv6 nat64 to private v4":       {block("64:ff9b::/96", nil, web)},
		"ipv6 unique local half":         {block("fc00::/7", nil, web)},
	}
	for name, p := range refused {
		t.Run("refused: "+name, func(t *testing.T) {
			if err := admitted(t, p...); err == nil {
				t.Fatal("a block that allows protected addresses was admitted as isolated")
			}
		})
	}

	accepted := map[string][]kubernetes.NetworkPolicy{
		"split default route with every protected range excepted": {block("0.0.0.0/1", lowHalf, web), block("128.0.0.0/1", highHalf, web)},
		"whole space with the ranges excepted":                    {block("0.0.0.0/0", append(append([]string{}, lowHalf...), highHalf...), web)},
		"a public documentation range":                            {block("203.0.113.0/24", nil, web)},
		"a public range on a database port":                       {block("198.51.100.0/24", nil, []kubernetes.NetworkPort{{Protocol: "TCP", Port: float64(5432)}})},
		"public ipv6":                                             {block("2001:db8::/32", nil, web)},
		"a public block with a private hole excepted":             {block("0.0.0.0/2", []string{"0.0.0.0/8", "10.0.0.0/8"}, web)},
	}
	for name, p := range accepted {
		t.Run("accepted: "+name, func(t *testing.T) {
			// 0.0.0.0/2 also covers 100.64/10 and 127/8: add those to be public-only.
			if name == "a public block with a private hole excepted" {
				p = []kubernetes.NetworkPolicy{block("0.0.0.0/2", []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8"}, web)}
			}
			if err := admitted(t, p...); err != nil {
				t.Fatalf("a block of public addresses was refused: %v", err)
			}
		})
	}
}
