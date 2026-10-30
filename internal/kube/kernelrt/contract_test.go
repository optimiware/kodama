package kernelrt

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/runtime"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
)

// Test_unit_kube_kernel_runtime_contract proves the mapping of the kernel
// Runtime and ResourceManager lifecycle onto Kubernetes objects against an
// in-memory cluster double: create or adopt by identity, observation, fenced
// stop, removal, orphan reconciliation and capability refusal. It does not
// prove CSI provisioning or reclaim, CNI enforcement, quota admission or
// kubelet behavior.
func Test_unit_kube_kernel_runtime_contract(t *testing.T) {
	ctx := context.Background()

	t.Run("start creates one Job and adopts it by identity", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "digest-1", 1)
		o := h.mustStart(h.startRequest(b, ws))
		if o.Phase != runtime.PhaseRunning || o.Binding != b {
			t.Fatalf("first observation = %+v", o)
		}
		if _, ok := h.cl.job(testNS, h.jobName(b)); !ok {
			t.Fatal("the Job does not exist under its derived name")
		}
		again := h.mustStart(h.startRequest(b, ws))
		if again.Phase != runtime.PhaseRunning || h.cl.count("create job") != 1 {
			t.Fatalf("a repeated Start must adopt: phase %s, creates %d", again.Phase, h.cl.count("create job"))
		}
		// A restarted engine with a higher fence adopts too.
		b2 := b
		b2.Fence = 2
		if _, err := h.start(h.startRequest(b2, ws)); err != nil || h.cl.count("create job") != 1 {
			t.Fatalf("a higher fence must adopt: %v, creates %d", err, h.cl.count("create job"))
		}
	})

	t.Run("a different specification digest is a mismatch", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "digest-1", 1)
		h.mustStart(h.startRequest(b, ws))
		other := b
		other.SpecDigest = "digest-2"
		_, err := h.start(h.startRequest(other, ws))
		if !errors.Is(err, runtime.ErrSpecMismatch) || !isNotReached(err) {
			t.Fatalf("err = %v", err)
		}
		if _, err := h.rt.Observe(ctx, other); !errors.Is(err, runtime.ErrSpecMismatch) {
			t.Fatalf("observe with another digest = %v", err)
		}
	})

	t.Run("a create from an older fence is refused", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "digest-1", 5)
		h.mustStart(h.startRequest(b, ws))
		stale := b
		stale.Fence = 3
		_, err := h.start(h.startRequest(stale, ws))
		if !errors.Is(err, runtime.ErrStaleFence) || !isNotReached(err) || h.cl.count("create job") != 1 {
			t.Fatalf("err = %v, creates %d", err, h.cl.count("create job"))
		}
	})

	t.Run("a foreign object under the derived name is never adopted, stopped or deleted", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "digest-1", 1)
		id := workloadIdent(b)
		fi := id
		fi.scope = scopeB
		j := kubernetes.Job{Metadata: kubernetes.ObjectMeta{Name: id.base(), Labels: id.labels(roleObject), Annotations: fi.annotations()}}
		h.cl.putJob(testNS, j)
		if _, err := h.start(h.startRequest(b, ws)); !errors.Is(err, ErrForeign) || !isNotReached(err) {
			t.Fatalf("start = %v", err)
		}
		if _, err := h.rt.Observe(ctx, b); !errors.Is(err, ErrForeign) {
			t.Fatalf("observe = %v", err)
		}
		if _, err := h.rt.Stop(ctx, b); !errors.Is(err, ErrForeign) {
			t.Fatalf("stop = %v", err)
		}
		if _, err := h.rt.Remove(ctx, b); !errors.Is(err, ErrForeign) {
			t.Fatalf("remove = %v", err)
		}
		if h.cl.deleteCount("job") != 0 {
			t.Fatal("a foreign Job was deleted")
		}
	})

	t.Run("observation maps Job and Pod status to a result", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		cases := []struct {
			name   string
			finish func(job string)
			phase  runtime.WorkloadPhase
			result runtime.Result
		}{
			{"running", func(string) {}, runtime.PhaseRunning, runtime.Result{}},
			{"succeeded", func(j string) { h.cl.finishJob(testNS, j, 0) }, runtime.PhaseExited, runtime.Result{Code: runtime.ResultSucceeded}},
			{"failed", func(j string) { h.cl.finishJob(testNS, j, 3) }, runtime.PhaseExited, runtime.Result{Code: runtime.ResultFailed, ExitCode: 3, Reason: runtime.ReasonCommandFailed}},
			{"timed out", func(j string) { h.cl.failJob(testNS, j, "DeadlineExceeded") }, runtime.PhaseExited, runtime.Result{Code: runtime.ResultTimedOut, Reason: runtime.ReasonStepTimeout}},
			{"lost", func(j string) { h.cl.failJob(testNS, j, "PodFailurePolicy") }, runtime.PhaseExited, runtime.Result{Code: runtime.ResultLost, Reason: runtime.ReasonWorkloadLost}},
		}
		for i, c := range cases {
			b := binding(scopeA, "run-1", "step"+string(rune('a'+i)), 1, "d", 1)
			h.mustStart(h.startRequest(b, ws))
			h.cl.startPod(testNS, h.jobName(b))
			c.finish(h.jobName(b))
			o, err := h.rt.Observe(ctx, b)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if err := o.Validate(); err != nil {
				t.Fatalf("%s: invalid observation: %v", c.name, err)
			}
			if o.Phase != c.phase || o.Result != c.result {
				t.Fatalf("%s: got %s %+v, want %s %+v", c.name, o.Phase, o.Result, c.phase, c.result)
			}
		}
	})

	t.Run("logs are a bounded snapshot of the step container", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.mustStart(h.startRequest(b, ws))
		if _, err := h.rt.Logs(ctx, b, runtime.LogOptions{}); err == nil {
			t.Fatal("logs before a Pod exists must report not found")
		}
		h.cl.startPod(testNS, h.jobName(b))
		rc, err := h.rt.Logs(ctx, b, runtime.LogOptions{Follow: true})
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(data) != "hello from the step\n" {
			t.Fatalf("log = %q", data)
		}
	})

	t.Run("a lost create response is uncertain, then adopted", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.cl.failNext("create job", &kubernetes.Error{Op: "create job", Kind: kubernetes.ErrUnknownOutcome}, true)
		_, err := h.start(h.startRequest(b, ws))
		if err == nil || isNotReached(err) || runtime.ClassifyEffect(err) != runtime.CommandUncertain {
			t.Fatalf("a lost response must be uncertain, got %v", err)
		}
		o, err := h.rt.Observe(ctx, b)
		if err != nil || o.Phase != runtime.PhaseRunning {
			t.Fatalf("the applied create is observable by identity: %+v %v", o, err)
		}
		if _, err := h.start(h.startRequest(b, ws)); err != nil || h.cl.count("create job") != 1 {
			t.Fatalf("retry must adopt: %v, creates %d", err, h.cl.count("create job"))
		}
	})

	t.Run("absence is not confirmed while a create may still land", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.cl.failNext("create job", &kubernetes.Error{Op: "create job", Kind: kubernetes.ErrUnknownOutcome}, false)
		if _, err := h.start(h.startRequest(b, ws)); err == nil || isNotReached(err) {
			t.Fatalf("err = %v", err)
		}
		if o, _ := h.rt.Observe(ctx, b); o.Phase != runtime.PhaseUnknown {
			t.Fatalf("phase = %s, want unknown inside the quiescence window", o.Phase)
		}
		if o, err := h.rt.Remove(ctx, b); err != nil || o.Phase != runtime.PhaseUnknown {
			t.Fatalf("remove inside the window = %+v %v; unknown must never count as removed", o, err)
		}
		h.clock.Advance(2 * time.Minute)
		if o, err := h.rt.Remove(ctx, b); err != nil || o.Phase != runtime.PhaseAbsent {
			t.Fatalf("remove after the window = %+v %v", o, err)
		}
	})

	t.Run("a definite refusal is not reached and leaves nothing to wait for", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.cl.failNext("create job", &kubernetes.Error{Op: "create job", Kind: kubernetes.ErrForbidden, Status: 403}, false)
		_, err := h.start(h.startRequest(b, ws))
		if !isNotReached(err) {
			t.Fatalf("err = %v", err)
		}
		if o, err := h.rt.Observe(ctx, b); err != nil || o.Phase != runtime.PhaseAbsent {
			t.Fatalf("observe = %+v %v", o, err)
		}
		if _, err := h.start(h.startRequest(b, ws)); err != nil {
			t.Fatalf("a rejected command may be retried: %v", err)
		}
	})

	t.Run("stop is fenced, deletes under the UID and never restarts", func(t *testing.T) {
		h := newHarness(t)
		h.cl.foreground = true
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 2)
		h.mustStart(h.startRequest(b, ws))
		h.cl.startPod(testNS, h.jobName(b))
		job, _ := h.cl.job(testNS, h.jobName(b))

		stale := b
		stale.Fence = 1
		if _, err := h.rt.Stop(ctx, stale); !errors.Is(err, runtime.ErrStaleFence) {
			t.Fatalf("a stale stop must be refused, got %v", err)
		}
		if h.cl.deleteCount("job") != 0 {
			t.Fatal("a stale stop deleted the Job")
		}
		o, err := h.rt.Stop(ctx, b)
		if err != nil {
			t.Fatal(err)
		}
		if o.Phase != runtime.PhaseRunning {
			t.Fatalf("a terminating Job is not yet stopped, got %s", o.Phase)
		}
		if got := h.cl.deletes[len(h.cl.deletes)-1]; got != "job "+h.jobName(b)+" uid="+job.Metadata.UID {
			t.Fatalf("the delete was not UID-preconditioned: %q", got)
		}
		h.cl.finalize()
		o, err = h.rt.Observe(ctx, b)
		if err != nil || o.Phase != runtime.PhaseExited || o.Result.Code != runtime.ResultCancelled {
			t.Fatalf("after the Job is gone: %+v %v", o, err)
		}
		// A repeated stop is idempotent.
		if o, err := h.rt.Stop(ctx, b); err != nil || o.Phase != runtime.PhaseExited {
			t.Fatalf("repeat stop = %+v %v", o, err)
		}
		// Neither the same writer nor a delayed older one can start it again.
		for _, fence := range []runtime.Fence{1, 2, 3} {
			again := b
			again.Fence = fence
			if _, err := h.start(h.startRequest(again, ws)); !isNotReached(err) || (!errors.Is(err, ErrClosed) && !errors.Is(err, runtime.ErrStaleFence)) {
				t.Fatalf("fence %d: start after stop = %v", fence, err)
			}
		}
		if h.cl.count("create job") != 1 {
			t.Fatal("a stopped workload was created again")
		}
	})

	t.Run("stop keeps the result of a workload that already exited", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.mustStart(h.startRequest(b, ws))
		h.cl.startPod(testNS, h.jobName(b))
		h.cl.finishJob(testNS, h.jobName(b), 0)
		o, err := h.rt.Stop(ctx, b)
		if err != nil || o.Phase != runtime.PhaseExited || o.Result.Code != runtime.ResultSucceeded {
			t.Fatalf("stop = %+v %v", o, err)
		}
		if h.cl.deleteCount("job") != 0 {
			t.Fatal("an exited Job must be retained for its result until Remove")
		}
	})

	t.Run("a stop with no record writes a tombstone that rejects a delayed create", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 4)
		if _, err := h.rt.Stop(ctx, b); err != nil {
			t.Fatal(err)
		}
		delayed := b
		delayed.Fence = 3
		if _, err := h.start(h.startRequest(delayed, ws)); !isNotReached(err) {
			t.Fatalf("delayed create = %v", err)
		}
		if h.cl.count("create job") != 0 {
			t.Fatal("the delayed create reached the cluster")
		}
	})

	t.Run("remove confirms absence only after the Job and Pods are gone", func(t *testing.T) {
		h := newHarness(t)
		h.cl.foreground = true
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.mustStart(h.startRequest(b, ws))
		h.cl.startPod(testNS, h.jobName(b))
		o, err := h.rt.Remove(ctx, b)
		if err != nil || o.Phase == runtime.PhaseAbsent {
			t.Fatalf("a terminating Job must not be confirmed removed: %+v %v", o, err)
		}
		h.cl.finalize()
		o, err = h.rt.Remove(ctx, b)
		if err != nil || o.Phase != runtime.PhaseAbsent || o.Validate() != nil {
			t.Fatalf("remove = %+v %v", o, err)
		}
		if o, _ := h.rt.Observe(ctx, b); o.Phase != runtime.PhaseAbsent {
			t.Fatalf("observe after remove = %s", o.Phase)
		}
		if len(h.cl.names(h.cl.jobs)) != 0 || len(h.cl.names(h.cl.pods)) != 0 {
			t.Fatal("objects remain")
		}
	})

	t.Run("orphan Pods of a deleted Job are removed", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.mustStart(h.startRequest(b, ws))
		h.cl.startPod(testNS, h.jobName(b))
		// The Job disappears without garbage collection of its Pod.
		h.cl.mu.Lock()
		delete(h.cl.jobs, key(testNS, h.jobName(b)))
		h.cl.mu.Unlock()
		o, err := h.rt.Remove(ctx, b)
		if err != nil || o.Phase != runtime.PhaseAbsent {
			t.Fatalf("remove = %+v %v", o, err)
		}
		if len(h.cl.names(h.cl.pods)) != 0 {
			t.Fatal("the Pod of the vanished Job remains")
		}
	})

	t.Run("the engine's references are scoped to its own run", func(t *testing.T) {
		h := newHarness(t)
		wsOther := h.ensureWorkspace(scopeA, "run-2", 1)
		wsB := h.ensureWorkspace(scopeB, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		for name, ws := range map[string]runtime.ResourceHandle{"another run": wsOther, "another tenant": wsB} {
			if _, err := h.start(h.startRequest(b, ws)); !isNotReached(err) || !errors.Is(err, domain.ErrInvalidValue) {
				t.Fatalf("%s: err = %v", name, err)
			}
		}
		if h.cl.count("create job") != 0 {
			t.Fatal("a Job was created against a foreign workspace")
		}
		// Another tenant addresses a different identity, so it sees nothing.
		h.ensureWorkspace(scopeA, "run-1", 1)
		h.mustStart(h.startRequest(b, h.workspaceHandle(scopeA, "run-1")))
		bb := b
		bb.Scope = scopeB
		if o, err := h.rt.Observe(ctx, bb); err != nil || o.Phase != runtime.PhaseAbsent {
			t.Fatalf("tenant B sees tenant A's workload: %+v %v", o, err)
		}
	})

	t.Run("a binding for another backend is refused", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		b := binding(scopeA, "run-1", "build", 1, "d", 1)
		b.Backend = "podman"
		if _, err := h.start(h.startRequest(b, ws)); !isNotReached(err) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("capabilities are declared honestly and admission refuses what the cluster cannot satisfy", func(t *testing.T) {
		h := newHarness(t)
		caps, _ := h.rt.Capabilities(ctx)
		rcaps, _ := h.res.Capabilities(ctx)
		if len(caps) != len(rcaps) {
			t.Fatalf("runtime %v and resources %v disagree", caps, rcaps)
		}
		for _, c := range []runtime.Capability{runtime.CapNonRoot, runtime.CapResourceLimits, runtime.CapImagePinning, runtime.CapAdoptWorkload, runtime.CapFencedStop, runtime.CapRunServices} {
			if !caps.Has(c) {
				t.Errorf("missing %s", c)
			}
		}
		if caps.Has(runtime.CapPersistentService) || caps.Has(runtime.CapNetworkIsolation) {
			t.Fatalf("over-claimed without an attestation: %v", caps)
		}

		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		refuse := func(name string, req runtime.StartRequest) {
			t.Helper()
			before := h.cl.count("create job")
			_, err := h.start(req)
			if !errors.Is(err, runtime.ErrCapability) || !isNotReached(err) {
				t.Fatalf("%s: err = %v", name, err)
			}
			if h.cl.count("create job") != before {
				t.Fatalf("%s: a Job was created", name)
			}
		}
		req := h.startRequest(binding(scopeA, "run-1", "net", 1, "d", 1), ws)
		req.Spec.Requires = runtime.CapabilitySet{runtime.CapNetworkIsolation}
		refuse("network isolation not attested", req)

		req = h.startRequest(binding(scopeA, "run-1", "persist", 1, "d", 1), ws)
		req.Spec.Requires = runtime.CapabilitySet{runtime.CapPersistentService}
		refuse("persistent services", req)

		req = h.startRequest(binding(scopeA, "run-1", "tag", 1, "d", 1), ws)
		req.Spec.Image = "registry.example/step:latest"
		refuse("unpinned image", req)

		req = h.startRequest(binding(scopeA, "run-1", "env", 1, "d", 1), ws)
		req.Spec.Env = map[string]string{"KODAMA_RUN_ID": "spoof"}
		refuse("reserved environment", req)

		// Attested, but the namespace has no default-deny policy.
		h2 := newHarness(t, func(c *Config) { c.NetworkPolicyEnforced = true })
		h2.cl.netpols[testNS] = nil
		ws2 := h2.ensureWorkspace(scopeA, "run-1", 1)
		req = h2.startRequest(binding(scopeA, "run-1", "net", 1, "d", 1), ws2)
		req.Spec.Requires = runtime.CapabilitySet{runtime.CapNetworkIsolation}
		if _, err := h2.start(req); !errors.Is(err, runtime.ErrCapability) || !isNotReached(err) {
			t.Fatalf("no default-deny: %v", err)
		}
		h2.cl.netpols[testNS] = []kubernetes.NetworkPolicy{defaultDeny()}
		if _, err := h2.start(req); err != nil {
			t.Fatalf("attested with default-deny: %v", err)
		}
		if c, _ := h2.rt.Capabilities(ctx); !c.Has(runtime.CapNetworkIsolation) {
			t.Fatal("the attested capability must be declared")
		}

		// The cluster lacks the namespace or the service account.
		h3 := newHarness(t)
		ws3 := h3.workspaceHandle(scopeA, "run-1")
		delete(h3.cl.accounts, key(testNS, testSA))
		if _, err := h3.res.Ensure(ctx, h3.workspaceSpec(scopeA, "run-1", 1)); !errors.Is(err, runtime.ErrCapability) || !isNotReached(err) {
			t.Fatalf("no service account: %v", err)
		}
		delete(h3.cl.namespaces, testNS)
		if _, err := h3.start(h3.startRequest(binding(scopeA, "run-1", "x", 1, "d", 1), ws3)); !errors.Is(err, runtime.ErrCapability) || !isNotReached(err) {
			t.Fatalf("no namespace: %v", err)
		}
	})

	t.Run("resource requests this backend cannot provide are refused", func(t *testing.T) {
		h := newHarness(t)
		check := func(name string, spec runtime.ResourceSpec) {
			t.Helper()
			if _, err := h.res.Ensure(ctx, spec); !errors.Is(err, runtime.ErrCapability) || !isNotReached(err) {
				t.Fatalf("%s: err = %v", name, err)
			}
		}
		ws := h.workspaceSpec(scopeA, "run-1", 1)
		ws.Preparation.Sources = []runtime.SourceInput{{Name: "app", Locator: "git://x", Revision: "abc", Path: "app"}}
		check("workspace sources", ws)

		svc := h.serviceSpec(scopeA, "run-1", "db", 1)
		svc.Lifetime, svc.RunID = runtime.LifetimeProject, ""
		svc.Service.Lifetime = runtime.LifetimeProject
		check("project lifetime", svc)

		svc = h.serviceSpec(scopeA, "run-1", "db", 1)
		svc.Service.Image = "registry.example/db:16"
		check("unpinned service image", svc)

		svc = h.serviceSpec(scopeA, "run-1", "db", 1)
		svc.Service.Secrets = []runtime.SecretRef{{Owner: domain.Owner{TenantID: "tenant-a"}, Name: "PASSWORD"}}
		check("service secrets", svc)

		svc = h.serviceSpec(scopeA, "run-1", "db", 1)
		svc.Requires = runtime.CapabilitySet{runtime.CapPersistentService}
		check("capability", svc)

		h.cl.classes = map[string]kubernetes.StorageClass{}
		check2 := h.workspaceSpec(scopeA, "run-1", 1)
		if _, err := h.res.Ensure(ctx, check2); !errors.Is(err, runtime.ErrCapability) || !isNotReached(err) {
			t.Fatalf("missing storage class: %v", err)
		}
	})

	t.Run("workspace ensure is idempotent, fenced and observable", func(t *testing.T) {
		h := newHarness(t)
		spec := h.workspaceSpec(scopeA, "run-1", 3)
		o, err := h.res.Ensure(ctx, spec)
		if err != nil || o.State != runtime.ResourceReady || o.Validate() != nil {
			t.Fatalf("a claim that binds on first use is usable now: %+v %v", o, err)
		}
		if _, err := h.res.Ensure(ctx, spec); err != nil || h.cl.count("create pvc") != 1 {
			t.Fatalf("re-ensure: %v creates %d", err, h.cl.count("create pvc"))
		}
		mismatch := spec
		mismatch.SpecDigest = "other"
		if _, err := h.res.Ensure(ctx, mismatch); !errors.Is(err, runtime.ErrSpecMismatch) || !isNotReached(err) {
			t.Fatalf("mismatch = %v", err)
		}
		stale := spec
		stale.Fence = 2
		if _, err := h.res.Ensure(ctx, stale); !errors.Is(err, runtime.ErrStaleFence) {
			t.Fatalf("stale = %v", err)
		}
		if _, err := h.res.Logs(ctx, o.Handle, runtime.LogOptions{}); err == nil {
			t.Fatal("a workspace has no logs")
		}
		// An Immediate class is starting until the claim binds, then ready.
		h.cl.classes["fast"] = kubernetes.StorageClass{VolumeBindingMode: "Immediate"}
		if o, _ := h.res.Observe(ctx, o.Handle); o.State != runtime.ResourceStarting {
			t.Fatalf("pending on Immediate = %s", o.State)
		}
		pvc := workspaceIdent(h, "run-1").base()
		h.cl.setPVC(testNS, pvc, func(p *kubernetes.PersistentVolumeClaim) { p.Status.Phase = "Bound" })
		if o, _ := h.res.Observe(ctx, o.Handle); o.State != runtime.ResourceReady {
			t.Fatalf("bound = %s", o.State)
		}
		h.cl.setPVC(testNS, pvc, func(p *kubernetes.PersistentVolumeClaim) { p.Status.Phase = "Lost" })
		if o, _ := h.res.Observe(ctx, o.Handle); o.State != runtime.ResourceFailed || o.Reason != runtime.ReasonWorkspaceLost {
			t.Fatalf("lost = %+v", o)
		}
	})

	t.Run("a service is ready only when its Pod is, and stop and remove are fenced", func(t *testing.T) {
		h := newHarness(t)
		spec := h.serviceSpec(scopeA, "run-1", "db", 2)
		o, err := h.res.Ensure(ctx, spec)
		if err != nil || o.State != runtime.ResourceStarting {
			t.Fatalf("ensure = %+v %v", o, err)
		}
		if _, err := h.res.Ensure(ctx, spec); err != nil || h.cl.count("create pod") != 1 {
			t.Fatalf("idempotent ensure: %v creates %d", err, h.cl.count("create pod"))
		}
		pod := workspaceIdentFor(h, spec).base()
		h.cl.setPod(testNS, pod, func(p *kubernetes.Pod) {
			p.Status.Phase = "Running"
			p.Status.ContainerStatuses = []kubernetes.ContainerStatus{{Name: "service", Ready: true}}
		})
		if o, _ := h.res.Observe(ctx, o.Handle); o.State != runtime.ResourceReady {
			t.Fatalf("running and ready = %s", o.State)
		}
		h.cl.logs[key(testNS, pod)+"/service"] = "db up\n"
		rc, err := h.res.Logs(ctx, o.Handle, runtime.LogOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if data, _ := io.ReadAll(rc); string(data) != "db up\n" {
			t.Fatalf("log = %q", data)
		}

		stale := o.Handle
		stale.Fence = 1
		if _, err := h.res.Stop(ctx, stale); !errors.Is(err, runtime.ErrStaleFence) {
			t.Fatalf("stale stop = %v", err)
		}
		if _, err := h.res.Remove(ctx, stale); !errors.Is(err, runtime.ErrStaleFence) {
			t.Fatalf("stale remove = %v", err)
		}
		if st, err := h.res.Stop(ctx, o.Handle); err != nil || st.State != runtime.ResourceStopped {
			t.Fatalf("stop = %+v %v", st, err)
		}
		if len(h.cl.names(h.cl.pods)) != 0 || len(h.cl.names(h.cl.services)) != 0 {
			t.Fatal("stop left the Pod or Service")
		}
		if _, err := h.res.Ensure(ctx, spec); !errors.Is(err, ErrClosed) {
			t.Fatalf("a stopped service must not restart: %v", err)
		}
		rm, err := h.res.Remove(ctx, o.Handle)
		if err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("remove = %+v %v", rm, err)
		}
		if o, _ := h.res.Observe(ctx, o.Handle); !o.ConfirmedRemoved() {
			t.Fatalf("observe after remove = %s", o.State)
		}
	})

	t.Run("a service whose Pod fails is reported lost", func(t *testing.T) {
		h := newHarness(t)
		o, _ := h.res.Ensure(ctx, h.serviceSpec(scopeA, "run-1", "db", 1))
		pod := workspaceIdentFor(h, h.serviceSpec(scopeA, "run-1", "db", 1)).base()
		h.cl.setPod(testNS, pod, func(p *kubernetes.Pod) { p.Status.Phase = "Failed" })
		if got, _ := h.res.Observe(ctx, o.Handle); got.State != runtime.ResourceFailed || got.Reason != runtime.ReasonServiceLost {
			t.Fatalf("observation = %+v", got)
		}
	})

	t.Run("a workspace release is confirmed only with volume evidence", func(t *testing.T) {
		h := newHarness(t)
		spec := h.workspaceSpec(scopeA, "run-1", 1)
		o, _ := h.res.Ensure(ctx, spec)
		pvc := workspaceIdent(h, "run-1").base()
		h.cl.setPVC(testNS, pvc, func(p *kubernetes.PersistentVolumeClaim) { p.Spec.VolumeName = "pv-1"; p.Status.Phase = "Bound" })
		pv := kubernetes.PersistentVolume{}
		pv.Spec.PersistentVolumeReclaimPolicy = "Delete"
		h.cl.pvs["pv-1"] = pv
		name := "pv-1"
		att := kubernetes.VolumeAttachment{}
		att.Spec.Source.PersistentVolumeName = &name
		att.Status.Attached = true
		h.cl.attached = []kubernetes.VolumeAttachment{att}

		rm, err := h.res.Remove(ctx, o.Handle)
		if err != nil || rm.State != runtime.ResourceStopped {
			t.Fatalf("a claim whose volume still exists and is attached is not removed: %+v %v", rm, err)
		}
		ev, err := h.res.ReleaseEvidence(ctx, o.Handle)
		if err != nil || !ev.ClaimGone || ev.VolumeGone || !ev.Attached || ev.Volume != "pv-1" || ev.Released() {
			t.Fatalf("evidence = %+v %v", ev, err)
		}
		h.cl.attached = nil
		delete(h.cl.pvs, "pv-1")
		rm, err = h.res.Remove(ctx, o.Handle)
		if err != nil || !rm.ConfirmedRemoved() {
			t.Fatalf("released = %+v %v", rm, err)
		}
		if ev, _ := h.res.ReleaseEvidence(ctx, o.Handle); !ev.Released() {
			t.Fatalf("evidence = %+v", ev)
		}
	})

	t.Run("a retained volume is reported as failed cleanup, never removed", func(t *testing.T) {
		h := newHarness(t)
		o, _ := h.res.Ensure(ctx, h.workspaceSpec(scopeA, "run-1", 1))
		pvc := workspaceIdent(h, "run-1").base()
		h.cl.setPVC(testNS, pvc, func(p *kubernetes.PersistentVolumeClaim) { p.Spec.VolumeName = "pv-9"; p.Status.Phase = "Bound" })
		pv := kubernetes.PersistentVolume{}
		pv.Spec.PersistentVolumeReclaimPolicy = "Retain"
		h.cl.pvs["pv-9"] = pv
		rm, err := h.res.Remove(ctx, o.Handle)
		if err != nil || rm.State != runtime.ResourceFailed || rm.Reason != runtime.ReasonCleanupFailed || rm.ConfirmedRemoved() {
			t.Fatalf("remove = %+v %v", rm, err)
		}
	})

	t.Run("reconcile removes unreferenced objects of one scope and reports what remains", func(t *testing.T) {
		h := newHarness(t)
		// Referenced: run-1. Orphans: run-2 (workload, workspace, service). Another
		// tenant's run-9 shares the namespace and must be left alone.
		ws1 := h.ensureWorkspace(scopeA, "run-1", 1)
		b1 := binding(scopeA, "run-1", "build", 1, "d", 1)
		h.mustStart(h.startRequest(b1, ws1))
		ws2 := h.ensureWorkspace(scopeA, "run-2", 1)
		b2 := binding(scopeA, "run-2", "build", 1, "d", 1)
		h.mustStart(h.startRequest(b2, ws2))
		svc2 := h.serviceSpec(scopeA, "run-2", "db", 1)
		if _, err := h.res.Ensure(ctx, svc2); err != nil {
			t.Fatal(err)
		}
		wsB := h.ensureWorkspace(scopeB, "run-9", 1)
		bB := binding(scopeB, "run-9", "build", 1, "d", 1)
		h.mustStart(h.startRequest(bB, wsB))

		inv := Inventory{
			Workloads: map[string]bool{b1.WorkloadID: true},
			Resources: map[string]bool{ws1.Identity: true},
		}
		rep, err := h.ad.Reconcile(ctx, scopeA, inv)
		if err != nil || len(rep.Removed) != 0 || rep.Spared == 0 {
			t.Fatalf("young orphans must be spared: %+v %v", rep, err)
		}

		h.clock.Advance(2 * time.Minute)
		// The cluster refuses to delete one orphan: it must be reported retained.
		h.cl.failNext("delete job", &kubernetes.Error{Op: "delete job", Kind: kubernetes.ErrForbidden}, false)
		rep, err = h.ad.Reconcile(ctx, scopeA, inv)
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Retained) != 1 || rep.Retained[0].Identity != b2.WorkloadID || rep.Retained[0].Reason != runtime.ReasonCleanupFailed {
			t.Fatalf("retained = %+v", rep.Retained)
		}
		if len(rep.Removed) != 2 {
			t.Fatalf("removed = %v, want the workspace and the service", rep.Removed)
		}
		rep, err = h.ad.Reconcile(ctx, scopeA, inv)
		if err != nil || len(rep.Retained) != 0 || len(rep.Removed) != 1 || rep.Removed[0] != b2.WorkloadID {
			t.Fatalf("second pass: %+v %v", rep, err)
		}

		if _, ok := h.cl.job(testNS, h.jobName(b1)); !ok {
			t.Fatal("a referenced Job was removed")
		}
		if _, ok := h.cl.job(testNS, h.jobName(bB)); !ok {
			t.Fatal("another tenant's Job was removed")
		}
		if o, err := h.res.Observe(ctx, ws1); err != nil || o.State != runtime.ResourceReady {
			t.Fatalf("a referenced workspace changed: %+v %v", o, err)
		}
		if o, err := h.res.Observe(ctx, wsB); err != nil || o.State != runtime.ResourceReady {
			t.Fatalf("another tenant's workspace changed: %+v %v", o, err)
		}
		if o, _ := h.rt.Observe(ctx, b2); o.Phase != runtime.PhaseAbsent {
			t.Fatalf("the orphan is %s", o.Phase)
		}

		// Tombstones outlive the delayed-create window, then go.
		before := len(h.cl.names(h.cl.secrets))
		h.clock.Advance(2 * time.Hour)
		rep, err = h.ad.Reconcile(ctx, scopeA, inv)
		if err != nil || rep.Pruned != 3 || len(h.cl.names(h.cl.secrets)) != before-3 {
			t.Fatalf("prune: %+v %v secrets %d -> %d", rep, err, before, len(h.cl.names(h.cl.secrets)))
		}
	})

	t.Run("a step's declared secrets are mounted from an immutable Secret and removed with it", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		ref := runtime.SecretRef{Owner: domain.Owner{TenantID: "tenant-a"}, Name: "TOKEN"}
		b := binding(scopeA, "run-1", "deploy", 1, "d", 1)
		req := h.startRequest(b, ws)
		req.Spec.Secrets = []runtime.SecretRef{ref}
		req.Secrets = runtime.Materialization{Scope: scopeA, RunID: "run-1", StepKey: "deploy", Directory: "/kodama/secrets", Refs: []runtime.SecretRef{ref}}
		h.mustStart(req)

		job, _ := h.cl.job(testNS, h.jobName(b))
		var mounted bool
		for _, m := range job.Spec.Template.Spec.Containers[0].VolumeMounts {
			if m.Name == "secrets" && m.MountPath == "/kodama/secrets" && m.ReadOnly {
				mounted = true
			}
		}
		if !mounted {
			t.Fatal("the secrets are not mounted read-only at the declared directory")
		}
		doc := strings.ToLower(jobDocument(t, job))
		if strings.Contains(doc, "s3cr3t-value") {
			t.Fatal("a secret value is in the Job document")
		}
		if _, err := h.rt.Remove(ctx, b); err != nil {
			t.Fatal(err)
		}
		for _, n := range h.cl.names(h.cl.secrets) {
			if strings.HasSuffix(n, "-sec") {
				t.Fatalf("the step secrets remain: %s", n)
			}
		}
	})

	t.Run("secret declarations are refused unless declared, scoped, private and available", func(t *testing.T) {
		h := newHarness(t)
		ws := h.ensureWorkspace(scopeA, "run-1", 1)
		ref := runtime.SecretRef{Owner: domain.Owner{TenantID: "tenant-a"}, Name: "TOKEN"}
		base := func(step string) runtime.StartRequest {
			req := h.startRequest(binding(scopeA, "run-1", step, 1, "d", 1), ws)
			req.Spec.Secrets = []runtime.SecretRef{ref}
			req.Secrets = runtime.Materialization{Scope: scopeA, RunID: "run-1", StepKey: domain.StepID(step), Directory: "/kodama/secrets", Refs: []runtime.SecretRef{ref}}
			return req
		}
		expect := func(name string, req runtime.StartRequest, want error) {
			t.Helper()
			if _, err := h.start(req); !errors.Is(err, want) || !isNotReached(err) {
				t.Fatalf("%s: %v", name, err)
			}
		}
		r := base("a")
		r.Secrets.Directory = "/workspace/secrets"
		expect("inside the workspace", r, runtime.ErrCapability)
		r = base("b")
		r.Secrets.Directory = "relative"
		expect("relative directory", r, runtime.ErrCapability)
		r = base("c")
		r.Spec.Secrets = nil
		expect("undeclared", r, domain.ErrInvalidValue)
		r = base("d")
		foreign := runtime.SecretRef{Owner: domain.Owner{TenantID: "tenant-b"}, Name: "TOKEN"}
		r.Spec.Secrets, r.Secrets.Refs = []runtime.SecretRef{foreign}, []runtime.SecretRef{foreign}
		expect("another tenant's secret", r, domain.ErrInvalidValue)
		r = base("e")
		r.Secrets = runtime.Materialization{}
		expect("not materialized", r, ErrSecretUnavailable)
		r = base("f")
		r.Secrets.StepKey = "other"
		expect("another step's materialization", r, domain.ErrInvalidValue)
		h.files.err = errors.New("vault says s3cr3t-value")
		_, err := h.start(base("g"))
		if !errors.Is(err, ErrSecretUnavailable) || !isNotReached(err) || strings.Contains(err.Error(), "s3cr3t") {
			t.Fatalf("an unavailable secret must not echo its cause: %v", err)
		}
		if h.cl.count("create job") != 0 {
			t.Fatal("a Job was created without its secrets")
		}

		noSource, err := New(testConfig(), Dependencies{Cluster: h.cl, Targets: SingleTarget(Target{Namespace: testNS, ServiceAccount: testSA})}, Options{Clock: h.clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := noSource.Runtime().Start(ctx, base("h")); !errors.Is(err, runtime.ErrCapability) || !isNotReached(err) {
			t.Fatalf("no secret source: %v", err)
		}
	})
}

func (h *harness) workspaceHandle(scope domain.Scope, run domain.RunID) runtime.ResourceHandle {
	spec := h.workspaceSpec(scope, run, 1)
	return runtime.ResourceHandle{Scope: scope, RunID: run, Kind: runtime.ResourceWorkspace, Name: spec.Name, Identity: spec.Identity, Fence: 1}
}

func workspaceIdent(h *harness, run domain.RunID) ident {
	s := h.workspaceSpec(scopeA, run, 1)
	return resourceIdent(s.Scope, s.RunID, s.Kind, s.Name, s.Identity, "", "kubernetes")
}

func workspaceIdentFor(h *harness, s runtime.ResourceSpec) ident {
	return resourceIdent(s.Scope, s.RunID, s.Kind, s.Name, s.Identity, "", "kubernetes")
}
