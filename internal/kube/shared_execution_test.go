package kube_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/adapters"
	"github.com/optimiware/kodama/internal/kube/kubetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// Test_unit_kube_execution_produced_v2_attempt: an attempt execution itself
// produced for a run_shared run (its persisted ContextV2, its own fingerprint and
// committed launch, read back through the production attempts adapter) is
// dispatched by the real Kubernetes runtime over its fake cluster: the context is
// decoded on the v2 wire, the fingerprint reconstructed and verified, the Job
// rendered with the shared launch and the claim execution bound, and the admission
// credential registered on the v2 wire. A drifted fingerprint, a context of the
// other wire and a registrar without the v2 wire are refused.
func Test_unit_kube_execution_produced_v2_attempt(t *testing.T) {
	ctx := context.Background()
	scope := executiontest.SharedScope
	body := executiontest.TenantPolicy()

	cfg := kubetest.PolicyConfig(t)
	cfg.Tenants[scope.TenantID] = kube.TenantTarget{Namespace: "t020-acme", ServiceAccount: "t021-runner"}
	cfg.RunShared = &kube.RunSharedIdentity{RunAsUser: 1000, RunAsGroup: 1000, FSGroup: 1000}
	policy, err := kube.NewExecutionPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fake := kubetest.NewFake()
	kubetest.RunBaseline(fake, policy, scope.TenantID, body.PodSecurity)
	fake.PutStorageClass(body.Workspace.StorageClass, "WaitForFirstConsumer", "Delete")
	cat, _ := kubetest.NewCatalog(t, body)
	rr, err := kube.NewRunResources(kube.RunResourceDependencies{
		Policy: policy, Cluster: fake, Catalog: cat, Registrar: kubetest.NewResourceRegistrar(), Checkout: kubetest.NewCheckout(t),
	}, kube.RunResourceOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Execution admits and drives a run against its own double, except that the
	// workspace is the real adapter's claim in the fake cluster: the one execution
	// binds is the one the Job must mount.
	hybrid := &hybridResources{real: rr}
	_, s := executiontest.NewSharedMem(t, func(mo *executiontest.MemOptions) {
		hybrid.fake = mo.Shared.Resources.(*executiontest.FakeResources)
		mo.Shared = &execution.SharedConfig{Resources: hybrid, Policies: mo.Shared.Policies}
	})
	s.Policies.Set(body)
	s.Resources.SetBackend(kube.Backend)
	s.Manual.SetCapabilities(execution.Capabilities{Backend: kube.Backend, Guarantees: execution.AllGuarantees.WithRunResources()})
	run := executiontest.NewSharedRun(t, s, executiontest.SharedSpecDoc(false, executiontest.SharedStep("build")))
	run.Start()
	run.Up()
	claim, err := run.Claim()
	if err != nil || claim == nil {
		t.Fatalf("claim: %v", err)
	}
	agg := run.Get()
	launch, err := execution.SharedLaunchOf(agg, claim.StepID)
	if err != nil || launch.Workspace.UID == "" {
		t.Fatalf("launch %+v: %v", launch, err)
	}
	if launch.Policy != agg.Run.Shared.Policy {
		t.Fatal("the launch does not carry the run's pin")
	}

	attempts, err := adapters.NewAttempts(s.Store)
	if err != nil {
		t.Fatal(err)
	}
	reg := kubetest.NewRegistrar()
	rt, err := kube.NewRuntime(kube.Dependencies{Policy: policy, Cluster: fake, Attempts: attempts, Registrar: reg, OwnedPolicies: rr}, kube.Options{})
	if err != nil {
		t.Fatal(err)
	}
	intent := *claim
	intent.Shared = launch

	// The committed record is what the adapter rebuilds from.
	rec, err := attempts.Attempt(ctx, claim.Scope, claim.AttemptID)
	if err != nil || rec.Shared == nil {
		t.Fatalf("record: %+v %v", rec, err)
	}
	exp, err := policy.Expect(rec)
	if err != nil {
		t.Fatalf("an execution-produced attempt is not verified: %v", err)
	}
	if exp.ContextV2 == nil || exp.ContextV2.AttemptID != claim.AttemptID || exp.Identity.Fingerprint != claim.SpecFingerprint || exp.Shared == nil {
		t.Fatalf("expectation %+v", exp.Identity)
	}

	// Control: with the claim and the committed record valid (the same cluster, the
	// same attempt), a registrar without the v2 wire is refused for exactly that
	// reason, before any Job or credential exists for the attempt.
	jobs := fake.Calls("create job")
	rtV1, err := kube.NewRuntime(kube.Dependencies{Policy: policy, Cluster: fake, Attempts: attempts, Registrar: v1Registrar{kubetest.NewRegistrar()}, OwnedPolicies: rr}, kube.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rtV1.Start(ctx, intent)
	if !errors.Is(err, kube.ErrPolicy) || !strings.Contains(err.Error(), "cannot register a run_shared attempt") {
		t.Fatalf("a v1-only registrar: want the registrar-specific refusal, got %v", err)
	}
	if _, ok := fake.JobOf("t020-acme", claim.WorkloadName); ok || fake.Calls("create job") != jobs {
		t.Fatal("a Job was created for an attempt whose credential could not be registered")
	}

	w, err := rt.Start(ctx, intent)
	if err != nil || w.UID == "" {
		t.Fatalf("start: %+v %v", w, err)
	}
	job, ok := fake.JobOf("t020-acme", claim.WorkloadName)
	if !ok || job.Metadata.UID != w.UID || job.Metadata.Annotations[kube.AnnFingerprint] != claim.SpecFingerprint || job.Metadata.Annotations[kube.AnnRunModel] != kube.ValueRunShared {
		t.Fatalf("job %+v", job.Metadata)
	}
	pod := job.Spec.Template.Spec
	if sc := pod.SecurityContext; sc == nil || *sc.RunAsUser != 1000 || *sc.FSGroup != 1000 {
		t.Fatalf("the shared identity is not applied: %+v", sc)
	}
	if v, ok := volumeOf(pod, "workspace"); !ok || v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != launch.Workspace.Name {
		t.Fatalf("workspace volume %+v", v)
	}
	if envOf(pod.Containers[0])["KODAMA_RUNTIME_API"].Value != "v2" {
		t.Fatal("the Job does not select the v2 wire")
	}
	// Registration is repeated with the identical credential (idempotent), always on the v2 wire.
	if len(reg.V2) == 0 {
		t.Fatal("the attempt was not registered on the v2 wire")
	}
	for _, c := range reg.V2 {
		if c.AttemptID != claim.AttemptID || c.Context.RunID != claim.RunID || c.Context.ActionKey != claim.ActionKey || c.Context.RunModel != "run_shared" {
			t.Fatalf("v2 registration: %+v", c.Context)
		}
	}
	if _, err := rt.Observe(ctx, execution.Workload{Scope: claim.Scope, AttemptID: claim.AttemptID, Name: claim.WorkloadName, UID: w.UID}); err != nil {
		t.Fatalf("observe: %v", err)
	}

	// Refusals.
	drift := rec
	drift.SpecFingerprint = kube.SharedFingerprint(rec.JobName, rec.ContextDigest, "registry.example.invalid/other@sha256:0000000000000000000000000000000000000000000000000000000000000000", rec.ActionKey, launch.Policy.Digest)
	if _, err := policy.Expect(drift); !errors.Is(err, kube.ErrPolicyDrift) {
		t.Fatalf("a drifted fingerprint: %v", err)
	}
	v1only := rec
	v1only.Shared = nil
	if _, err := policy.Expect(v1only); err == nil {
		t.Fatal("a v2 context was accepted as an attempt_private one")
	}
	foreign := rec
	foreign.AttemptID = "att_other"
	if _, err := policy.Expect(foreign); err == nil {
		t.Fatal("a context of another attempt was accepted")
	}
}

// v1Registrar hides the registrar's v2 wire.
type v1Registrar struct{ r *kubetest.Registrar }

func (v v1Registrar) Register(ctx context.Context, cmd runtimeauth.RegisterCommand) (runtimeauth.Registration, error) {
	return v.r.Register(ctx, cmd)
}

// hybridResources is the companion port of the test: the run workspace is the real
// Kubernetes adapter's (so the claim, its record and its UID exist in the fake
// cluster), everything else is execution's double.
type hybridResources struct {
	real *kube.RunResources
	fake *executiontest.FakeResources
}

var _ execution.RunResources = (*hybridResources)(nil)

func (h *hybridResources) Capabilities(ctx context.Context) execution.ResourceCapabilities {
	c := h.fake.Capabilities(ctx)
	c.Backend = kube.Backend
	return c
}

func (h *hybridResources) Ensure(ctx context.Context, spec execution.ResourceSpec) (execution.ResourceObservation, error) {
	if spec.Kind == execution.KindWorkspace {
		return h.real.Ensure(ctx, spec)
	}
	return h.fake.Ensure(ctx, spec)
}

func (h *hybridResources) Observe(ctx context.Context, r execution.ResourceHandle) (execution.ResourceObservation, error) {
	if r.Kind == execution.KindWorkspace {
		return h.real.Observe(ctx, r)
	}
	return h.fake.Observe(ctx, r)
}

func (h *hybridResources) Logs(ctx context.Context, r execution.ResourceHandle, b execution.LogBound) (execution.ResourceLogs, error) {
	return h.fake.Logs(ctx, r, b)
}

func (h *hybridResources) Stop(ctx context.Context, r execution.ResourceHandle) (execution.ResourceStopEvidence, error) {
	if r.Kind == execution.KindWorkspace {
		return h.real.Stop(ctx, r)
	}
	return h.fake.Stop(ctx, r)
}

func (h *hybridResources) Remove(ctx context.Context, r execution.ResourceHandle) (execution.ResourceRemoveEvidence, error) {
	if r.Kind == execution.KindWorkspace {
		return h.real.Remove(ctx, r)
	}
	return h.fake.Remove(ctx, r)
}
