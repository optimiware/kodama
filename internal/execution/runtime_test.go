package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// ---- a scripted runtime and a one-run store ----

// stubRuntime answers each operation with what the test scripted and records
// the calls. Lifecycle behavior is covered by the real runtimes' contract.
type stubRuntime struct {
	caps    Capabilities
	uid     string
	observe func(Workload) (Observation, error)
	stop    StopEvidence
	remove  map[domain.AttemptID]error
	calls   []string
}

func (r *stubRuntime) Start(_ context.Context, spec AttemptSpec) (Workload, error) {
	r.calls = append(r.calls, "start")
	return spec.Workload(r.uid), nil
}
func (r *stubRuntime) Observe(_ context.Context, w Workload) (Observation, error) {
	r.calls = append(r.calls, "observe")
	return r.observe(w)
}
func (r *stubRuntime) Logs(context.Context, Workload, io.Writer) error { return nil }
func (r *stubRuntime) Stop(context.Context, Workload) (StopEvidence, error) {
	r.calls = append(r.calls, "stop")
	return r.stop, nil
}
func (r *stubRuntime) Remove(_ context.Context, w Workload) error {
	r.calls = append(r.calls, "remove:"+string(w.AttemptID))
	return r.remove[w.AttemptID]
}
func (r *stubRuntime) Capabilities() Capabilities { return r.caps }

func fullRuntime(backend string) *stubRuntime {
	return &stubRuntime{caps: Capabilities{Backend: backend, Guarantees: AllGuarantees}}
}

// memStore holds one aggregate and applies mutations to it directly.
type memStore struct {
	Store
	agg      *Aggregate
	removals []Removal
	recorded []domain.AttemptID
}

func (m *memStore) RunOf(context.Context, domain.Scope, domain.AttemptID) (domain.RunID, error) {
	return m.agg.Run.ID, nil
}

func (m *memStore) Update(ctx context.Context, _ domain.Scope, _ domain.RunID, _ UpdateOptions, fn Mutation) (*Aggregate, any, error) {
	m.agg.Revoked = nil
	env := &fakeEnv{}
	eff, err := fn(ctx, env, m.agg)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range eff.After {
		if err := f(ctx, env); err != nil {
			return nil, nil, err
		}
	}
	return m.agg, eff.Result, nil
}

type countingStore struct {
	*memStore
	updates int
}

func (c *countingStore) Update(ctx context.Context, scope domain.Scope, id domain.RunID, o UpdateOptions, fn Mutation) (*Aggregate, any, error) {
	c.updates++
	return c.memStore.Update(ctx, scope, id, o, fn)
}

func (m *memStore) Removable(_ context.Context, _ domain.TenantID, backend string, after RemovalCursor, limit int) ([]Removal, error) {
	var page []Removal
	for _, r := range m.removals {
		if r.Backend != backend {
			continue
		}
		c := r.Cursor()
		if after.AttemptID != "" && !cursorAfter(c, after) {
			continue
		}
		if slices.Contains(m.recorded, r.Workload.AttemptID) {
			continue
		}
		if page = append(page, r); len(page) == limit {
			break
		}
	}
	return page, nil
}

func cursorAfter(c, after RemovalCursor) bool {
	switch {
	case !c.SettledAt.Equal(after.SettledAt):
		return c.SettledAt.After(after.SettledAt)
	case c.ProjectID != after.ProjectID:
		return c.ProjectID > after.ProjectID
	}
	return c.AttemptID > after.AttemptID
}

func (m *memStore) RecordRemoval(_ context.Context, _ domain.Scope, attempt domain.AttemptID) error {
	m.recorded = append(m.recorded, attempt)
	return nil
}

func runtimeService(t *testing.T, store Store, rt Runtime, req Requirements) *Service {
	t.Helper()
	d := Dependencies{
		Store: store, Authority: allowAuthority{}, Definitions: fakeDefs{}, Capabilities: pipeline.EnabledKinds{pipeline.KindContainer},
		Work: fakeWork{}, Configuration: emptyConfig{}, Sources: &fakeSources{}, Limits: fixedLimits{}, Hooks: hooks,
		Clock: func() time.Time { return t0 }, Requirements: req,
	}
	if rt != nil {
		d.Runtime = rt
	}
	s, err := NewService(d, Options{Batch: 2})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// running returns a store holding one run with a claimed, running attempt.
func running(t *testing.T, binding *RuntimeBinding) (*memStore, *Attempt) {
	t.Helper()
	agg := newAgg(t, nil, container("build"))
	agg.Run.Snapshot.Runtime = binding
	at := claim(t, agg, t0, "att_1")
	return &memStore{agg: agg}, at
}

const uidA, uidB = "11111111-2222-4333-8444-555555555555", "99999999-2222-4333-8444-555555555555"

// ---- tests ----

// Test_runtime_admission_binding: a run is admitted only on a runtime that
// provides what its steps require, and the binding is pinned in the snapshot
// and its digest.
func Test_runtime_admission_binding(t *testing.T) {
	ctx := context.Background()
	clean := newDefinition(t, scope, pipelinetest.DocNamed("clean", "{}", "{id: teardown, kind: container, image: "+pipelinetest.Image+", command: [/bin/true], timeoutSeconds: 60, workspace: {access: none}}"))
	defs := fakeDefs{defs: map[domain.DefinitionID]pipeline.Definition{clean.ID: clean}}
	prepare := func(rt Runtime, req Requirements) (*AdmissionPlan, error) {
		d := Dependencies{Definitions: defs, Work: fakeWork{}, Sources: &fakeSources{}, Cleanup: allowCleanup{}, Requirements: req}
		if rt != nil {
			d.Runtime = rt
		}
		return service(t, d).PrepareCleanup(ctx, CleanupCommand{Scope: scope, DeploymentID: "dep_1", DefinitionID: clean.ID})
	}

	t.Run("the default policy requires the container semantics", func(t *testing.T) {
		plan, err := prepare(fullRuntime("kubernetes"), nil)
		if err != nil {
			t.Fatal(err)
		}
		got := plan.Aggregate.Run.Snapshot.Runtime
		if got == nil || got.Backend != "kubernetes" || got.Requires != OCIImage|Isolation|ResourceLimits {
			t.Fatalf("binding %+v", got)
		}
	})
	t.Run("a runtime lacking a requirement refuses the run before any state", func(t *testing.T) {
		hostOnly := &stubRuntime{caps: Capabilities{Backend: "process"}}
		_, err := prepare(hostOnly, nil)
		if !errors.Is(err, ErrAdmissionRefused) || !strings.Contains(err.Error(), "oci_image,isolation,resource_limits") {
			t.Fatalf("got %v", err)
		}
		plan, err := prepare(hostOnly, func(pipeline.Step) Guarantees { return 0 })
		if err != nil || plan.Aggregate.Run.Snapshot.Runtime.Requires != 0 {
			t.Fatalf("explicit host-fixture policy: %v", err)
		}
	})
	t.Run("without a runtime the run is admitted unbound", func(t *testing.T) {
		plan, err := prepare(nil, nil)
		if err != nil || plan.Aggregate.Run.Snapshot.Runtime != nil {
			t.Fatalf("%v %+v", err, plan)
		}
	})
	t.Run("the binding is part of the snapshot digest", func(t *testing.T) {
		snap := Snapshot{}
		unbound, err := Digest("pd_1", "sha256:"+hex64("a"), "wi_1", 1, commitA, snap)
		if err != nil {
			t.Fatal(err)
		}
		snap.Runtime = &RuntimeBinding{Backend: "podman", Requires: OCIImage}
		bound, _ := Digest("pd_1", "sha256:"+hex64("a"), "wi_1", 1, commitA, snap)
		snap.Runtime = &RuntimeBinding{Backend: "kubernetes", Requires: OCIImage}
		other, _ := Digest("pd_1", "sha256:"+hex64("a"), "wi_1", 1, commitA, snap)
		if unbound == bound || bound == other {
			t.Fatal("the runtime binding does not change the snapshot digest")
		}
	})
	t.Run("guarantees encode as stable names", func(t *testing.T) {
		raw, err := json.Marshal(RuntimeBinding{Backend: "process", Requires: Isolation | CrashRecovery})
		if err != nil || string(raw) != `{"backend":"process","requires":["isolation","crash_recovery"]}` {
			t.Fatalf("%s %v", raw, err)
		}
		var b RuntimeBinding
		if err := json.Unmarshal(raw, &b); err != nil || b.Requires != Isolation|CrashRecovery {
			t.Fatalf("%+v %v", b, err)
		}
		if err := json.Unmarshal([]byte(`{"requires":["gpu"]}`), &b); err == nil {
			t.Fatal("an unknown guarantee was accepted")
		}
		if Guarantees(0).String() != "none" || AllGuarantees.Missing(OCIImage) != 0 || Isolation.Missing(OCIImage|Isolation) != OCIImage {
			t.Fatal("set operations")
		}
	})
}

// Test_runtime_binding_gates_actions: no other backend ever starts, stops or
// removes a bound run's workloads, and nothing launches without the
// guarantees the current policy requires.
func Test_runtime_binding_gates_actions(t *testing.T) {
	agg := newAgg(t, nil, container("build"))
	run, step := agg.Run, agg.Step("build")
	cases := []struct {
		name    string
		rt      Runtime
		binding *RuntimeBinding
		ok      bool
	}{
		{"no runtime", nil, nil, false},
		{"unbound run", fullRuntime("podman"), nil, false},
		{"bound to the runtime", fullRuntime("podman"), &RuntimeBinding{Backend: "podman", Requires: OCIImage}, true},
		{"bound to another backend", fullRuntime("podman"), &RuntimeBinding{Backend: "kubernetes"}, false},
		{"a pinned guarantee was lost", &stubRuntime{caps: Capabilities{Backend: "podman", Guarantees: Isolation}}, &RuntimeBinding{Backend: "podman", Requires: OCIImage}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run.Snapshot.Runtime = c.binding
			s := runtimeService(t, &memStore{agg: agg}, c.rt, nil)
			_, err := s.runtimeFor(run)
			if (err == nil) != c.ok || (err != nil && !errors.Is(err, ErrNotConfigured)) {
				t.Fatalf("runtimeFor: %v", err)
			}
		})
	}
	t.Run("launch re-applies the current policy to the step", func(t *testing.T) {
		run.Snapshot.Runtime = &RuntimeBinding{Backend: "process"}
		hostOnly := &stubRuntime{caps: Capabilities{Backend: "process"}}
		if _, err := runtimeService(t, &memStore{agg: agg}, hostOnly, nil).launchable(run, step); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("a container step launched on a host runtime: %v", err)
		}
		if _, err := runtimeService(t, &memStore{agg: agg}, hostOnly, func(pipeline.Step) Guarantees { return 0 }).launchable(run, step); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("revoked attempts of a foreign-bound run are not stopped", func(t *testing.T) {
		store, at := running(t, &RuntimeBinding{Backend: "kubernetes"})
		rt := fullRuntime("podman")
		s := runtimeService(t, store, rt, nil)
		store.agg.Revoked = []*Attempt{at}
		s.terminateRevoked(context.Background(), store.agg)
		if len(rt.calls) != 0 {
			t.Fatalf("calls %v", rt.calls)
		}
	})
}

// Test_redrive_applies_launch_policy: an unconfirmed attempt is started again
// only where a first start would be allowed: the run's backend and the current
// policy must both still hold.
func Test_redrive_applies_launch_policy(t *testing.T) {
	ctx := context.Background()
	hostOnly := func() *stubRuntime { return &stubRuntime{caps: Capabilities{Backend: "process"}, uid: uidA} }
	cases := []struct {
		name    string
		binding *RuntimeBinding
		rt      *stubRuntime
		req     Requirements
		started bool
	}{
		{"policy still met", &RuntimeBinding{Backend: "process"}, hostOnly(), func(pipeline.Step) Guarantees { return 0 }, true},
		{"policy tightened since admission", &RuntimeBinding{Backend: "process"}, hostOnly(), nil, false},
		{"bound to another backend", &RuntimeBinding{Backend: "kubernetes"}, fullRuntime("podman"), nil, false},
		{"admitted without a runtime", nil, fullRuntime("podman"), nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, at := running(t, c.binding)
			store.agg.Control(at.ID).ClaimUntil = nil
			s := runtimeService(t, store, c.rt, c.req)
			ref := RunRef{Scope: scope, ID: store.agg.Run.ID}
			if got := s.redrive(ctx, store.agg, ref, at); got != c.started {
				t.Fatalf("redriven %v, want %v", got, c.started)
			}
			if started := slices.Contains(c.rt.calls, "start"); started != c.started {
				t.Fatalf("start called %v: %v", started, c.rt.calls)
			}
			if confirmed := store.agg.Control(at.ID).DispatchConfirmedAt != nil; confirmed != c.started {
				t.Fatalf("confirmed %v", confirmed)
			}
		})
	}
}

// Test_dispatch_binds_the_workload_instance: the instance UID the runtime
// reports at start is bound to the attempt once; another instance afterwards
// is a conflict recorded as evidence.
func Test_dispatch_binds_the_workload_instance(t *testing.T) {
	ctx := context.Background()
	store, at := running(t, nil)
	rt := fullRuntime("podman")
	rt.uid = uidA
	s := runtimeService(t, store, rt, nil)
	if err := s.drive(ctx, rt, at.spec(PurposeWork, false)); err != nil {
		t.Fatal(err)
	}
	if at.WorkloadUID != uidA || store.agg.Control(at.ID).DispatchConfirmedAt == nil {
		t.Fatalf("uid %q confirmed %v", at.WorkloadUID, store.agg.Control(at.ID).DispatchConfirmedAt)
	}
	if err := s.drive(ctx, rt, at.spec(PurposeWork, true)); err != nil {
		t.Fatalf("a redrive adopting the same instance: %v", err)
	}
	rt.uid = uidB
	if err := s.drive(ctx, rt, at.spec(PurposeWork, true)); !errors.Is(err, ErrConflict) || at.WorkloadUID != uidA {
		t.Fatalf("another instance: %v, uid %q", err, at.WorkloadUID)
	}
	if !hasEvidence(store.agg, at.ID, EvidenceMismatch) {
		t.Fatal("the mismatch was not recorded")
	}
	rt.uid = "not-a-uuid"
	if err := s.drive(ctx, rt, at.spec(PurposeWork, true)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a malformed UID: %v", err)
	}
}

// Test_observing_running_workloads: a workload the runtime affirms absent is
// lost, with the launch evidence already known; other answers change nothing.
func Test_observing_running_workloads(t *testing.T) {
	ctx := context.Background()
	confirmed := func(t *testing.T, started bool) (*memStore, *Attempt, *Control) {
		store, at := running(t, nil)
		c := store.agg.Control(at.ID)
		c.DispatchConfirmedAt, at.WorkloadUID = &t0, uidA
		if started {
			at.StartedAt = &t0
		}
		return store, at, c
	}
	lost := func(Workload) (Observation, error) { return Observation{}, ErrNotFound }

	t.Run("lost after start", func(t *testing.T) {
		store, at, c := confirmed(t, true)
		rt := fullRuntime("podman")
		rt.observe = lost
		if err := runtimeService(t, store, rt, nil).observeWorkload(ctx, rt, at, c); err != nil {
			t.Fatal(err)
		}
		if at.Status != AttemptFailed || at.Reason != ReasonWorkloadLost || at.Class != ClassInfrastructure || c.Launch != LaunchStarted {
			t.Fatalf("attempt %s/%s/%s launch %s", at.Status, at.Reason, at.Class, c.Launch)
		}
	})
	t.Run("lost before any start evidence is an unknown outcome", func(t *testing.T) {
		store, at, c := confirmed(t, false)
		rt := fullRuntime("podman")
		rt.observe = lost
		if err := runtimeService(t, store, rt, nil).observeWorkload(ctx, rt, at, c); err != nil {
			t.Fatal(err)
		}
		if at.Status != AttemptFailed || at.Reason != ReasonOutcomeUnknown {
			t.Fatalf("attempt %s/%s", at.Status, at.Reason)
		}
	})
	t.Run("started evidence is applied", func(t *testing.T) {
		store, at, c := confirmed(t, false)
		rt := fullRuntime("podman")
		rt.observe = func(w Workload) (Observation, error) {
			return Observation{Kind: ObservedStarted, WorkloadUID: w.UID, Launch: LaunchStarted}, nil
		}
		if err := runtimeService(t, store, rt, nil).observeWorkload(ctx, rt, at, c); err != nil || at.StartedAt == nil || at.Status != AttemptRunning {
			t.Fatalf("%v started %v", err, at.StartedAt)
		}
	})
	t.Run("a repeated answer writes nothing", func(t *testing.T) {
		store, at, c := confirmed(t, true)
		rt := fullRuntime("podman")
		rt.observe = func(w Workload) (Observation, error) {
			return Observation{Kind: ObservedStarted, WorkloadUID: w.UID, Launch: LaunchStarted}, nil
		}
		version := at.Version
		updates := &countingStore{memStore: store}
		if err := runtimeService(t, updates, rt, nil).observeWorkload(ctx, rt, at, c); err != nil || updates.updates != 0 || at.Version != version {
			t.Fatalf("%v: %d updates", err, updates.updates)
		}
	})
	for name, answer := range map[string]func(Workload) (Observation, error){
		"an outage":                     func(Workload) (Observation, error) { return Observation{}, errors.New("daemon down") },
		"a terminated running workload": func(Workload) (Observation, error) { return Observation{Kind: ObservedTerminated}, nil },
	} {
		t.Run(name+" changes nothing", func(t *testing.T) {
			store, at, c := confirmed(t, false)
			rt := fullRuntime("podman")
			rt.observe = answer
			if err := runtimeService(t, store, rt, nil).observeWorkload(ctx, rt, at, c); !errors.Is(err, ErrUnavailable) || at.Status != AttemptRunning {
				t.Fatalf("%v, attempt %s", err, at.Status)
			}
		})
	}
}

// Test_remove_settled: removal is retried until the runtime confirms it,
// recorded only then, and never attempted through another backend or for a run
// that was admitted without one.
func Test_remove_settled(t *testing.T) {
	ctx := context.Background()
	removal := func(attempt domain.AttemptID, backend string, minute int) Removal {
		return Removal{
			Workload:  Workload{Scope: scope, AttemptID: attempt, Name: WorkloadName(scope.TenantID, attempt)},
			Backend:   backend,
			SettledAt: t0.Add(time.Duration(minute) * time.Minute),
		}
	}
	store := &memStore{removals: []Removal{
		removal("att_a", "podman", 1), removal("att_b", "", 2), removal("att_c", "kubernetes", 3),
		removal("att_d", "podman", 4), removal("att_e", "podman", 5),
	}}
	rt := fullRuntime("podman")
	rt.remove = map[domain.AttemptID]error{"att_d": ErrBusy, "att_e": errors.New("daemon down")}
	s := runtimeService(t, store, rt, nil)

	n, err := s.RemoveSettled(ctx, scope.TenantID)
	if n != 1 || err == nil || !slices.Equal(store.recorded, []domain.AttemptID{"att_a"}) {
		t.Fatalf("removed %d, err %v, recorded %v", n, err, store.recorded)
	}
	if slices.Contains(rt.calls, "remove:att_b") || slices.Contains(rt.calls, "remove:att_c") {
		t.Fatalf("a workload of an unbound run or another backend was removed: %v", rt.calls)
	}
	// Retained and failed removals stay pending and are retried.
	rt.remove, rt.calls = nil, nil
	if n, err := s.RemoveSettled(ctx, scope.TenantID); n != 2 || err != nil {
		t.Fatalf("retry: %d %v", n, err)
	}
	if !slices.Equal(rt.calls, []string{"remove:att_d", "remove:att_e"}) {
		t.Fatalf("retry calls %v", rt.calls)
	}
	if _, err := runtimeService(t, store, nil, nil).RemoveSettled(ctx, scope.TenantID); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("without a runtime: %v", err)
	}
}

// Test_attempt_spec: a spec is valid only for the attempt's deterministic
// workload and its pinned context.
func Test_attempt_spec(t *testing.T) {
	ctx := pinnedContext(t)
	plan, err := DefaultPlanner{}.Plan(scope.TenantID, ctx)
	if err != nil {
		t.Fatal(err)
	}
	spec := AttemptSpec{
		Scope: scope, RunID: ctx.RunID, StepID: ctx.StepID, AttemptID: ctx.AttemptID, ActionKey: ctx.ActionKey,
		WorkloadName: plan.WorkloadName, SpecFingerprint: plan.SpecFingerprint, ContextDigest: plan.ContextDigest, Context: plan.Context,
	}
	if got, err := spec.DecodeContext(); err != nil || got.AttemptID != ctx.AttemptID {
		t.Fatalf("%v %+v", err, got)
	}
	for name, edit := range map[string]func(*AttemptSpec){
		"another name":       func(s *AttemptSpec) { s.WorkloadName = "kdm-other" },
		"a tampered context": func(s *AttemptSpec) { s.Context = append(slices.Clone(s.Context), ' ') },
		"no fingerprint":     func(s *AttemptSpec) { s.SpecFingerprint = "" },
		"another attempt":    func(s *AttemptSpec) { s.AttemptID, s.WorkloadName = "att_2", WorkloadName(scope.TenantID, "att_2") },
		"a malformed handle": func(s *AttemptSpec) { s.Scope = domain.Scope{} },
	} {
		bad := spec
		edit(&bad)
		if _, err := bad.DecodeContext(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	w := spec.Workload(uidA)
	if w.Validate() != nil {
		t.Fatal("a valid handle")
	}
	w.UID = "ABC"
	if !errors.Is(w.Validate(), ErrInvalid) {
		t.Fatal("a malformed UID")
	}
}

func hasEvidence(agg *Aggregate, attempt domain.AttemptID, kind EvidenceKind) bool {
	for _, e := range agg.Evidence {
		if e.Attempt == attempt && e.Kind == kind {
			return true
		}
	}
	return false
}

func pinnedContext(t *testing.T) runtimev1.Context {
	t.Helper()
	return runtimev1.Context{
		TenantID: scope.TenantID, ProjectID: scope.ProjectID, RunID: "run_1", StepID: "build", AttemptID: "att_1",
		ActionKey:  ActionKey("run_1", "build"),
		Definition: runtimev1.Definition{ID: "pd_1", Digest: runtimev1.Digest("sha256:" + hex64("2"))},
		Command:    runtimev1.Command{Image: testImage, Command: []string{"/bin/sh", "-ec"}, Args: []string{"true"}, Env: map[string]string{}},
		Workspace:  runtimev1.Workspace{Access: runtimev1.AccessNone},
		Inputs:     map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{}, Secrets: []runtimev1.SecretBinding{},
		Limits: runtimev1.Limits{TimeoutSeconds: 60, MaxOutputBytes: 1 << 20, MaxOutputFiles: 16, MaxDiagnosticBytes: 1 << 20},
	}
}

// Test_runtime_never_started_capability (T-102, O-25): the never-started proof
// is a capability separate from the execution guarantees. A backend without it
// is still eligible for the default policy, and its admission binding is
// identical, so existing run snapshots do not change.
func Test_runtime_never_started_capability(t *testing.T) {
	ctx := context.Background()
	clean := newDefinition(t, scope, pipelinetest.DocNamed("clean", "{}", "{id: teardown, kind: container, image: "+pipelinetest.Image+", command: [/bin/true], timeoutSeconds: 60, workspace: {access: none}}"))
	defs := fakeDefs{defs: map[domain.DefinitionID]pipeline.Definition{clean.ID: clean}}
	binding := func(t *testing.T, caps Capabilities) *RuntimeBinding {
		t.Helper()
		d := Dependencies{Definitions: defs, Work: fakeWork{}, Sources: &fakeSources{}, Cleanup: allowCleanup{}, Runtime: &stubRuntime{caps: caps}}
		plan, err := service(t, d).PrepareCleanup(ctx, CleanupCommand{Scope: scope, DeploymentID: "dep_1", DefinitionID: clean.ID})
		if err != nil {
			t.Fatalf("a backend without the proof was refused: %v", err)
		}
		return plan.Aggregate.Run.Snapshot.Runtime
	}
	t.Run("absent by default", func(t *testing.T) {
		if (Capabilities{}).ProvesNeverStarted || (Capabilities{Backend: "k", Guarantees: AllGuarantees}).ProvesNeverStarted {
			t.Fatal("the proof is declared explicitly, never implied by guarantees")
		}
	})
	t.Run("not a guarantee", func(t *testing.T) {
		if slices.Contains(AllGuarantees.Names(), "never_started") || AllGuarantees.Missing(AllGuarantees) != 0 {
			t.Fatal("launch-history proof leaked into the guarantee set")
		}
	})
	t.Run("the admission binding is independent of the proof", func(t *testing.T) {
		without := binding(t, Capabilities{Backend: "kubernetes", Guarantees: AllGuarantees})
		with := binding(t, Capabilities{Backend: "kubernetes", Guarantees: AllGuarantees, ProvesNeverStarted: true})
		if *without != *with || without.Requires != OCIImage|Isolation|ResourceLimits {
			t.Fatalf("without %+v, with %+v", without, with)
		}
		a, _ := Digest("pd_1", "sha256:"+hex64("a"), "wi_1", 1, commitA, Snapshot{Runtime: without})
		b, _ := Digest("pd_1", "sha256:"+hex64("a"), "wi_1", 1, commitA, Snapshot{Runtime: with})
		if a != b {
			t.Fatal("the proof capability changed the snapshot digest")
		}
	})
	t.Run("the proof does not stand in for a missing guarantee", func(t *testing.T) {
		d := Dependencies{Definitions: defs, Work: fakeWork{}, Sources: &fakeSources{}, Cleanup: allowCleanup{},
			Runtime: &stubRuntime{caps: Capabilities{Backend: "process", ProvesNeverStarted: true}}}
		if _, err := service(t, d).PrepareCleanup(ctx, CleanupCommand{Scope: scope, DeploymentID: "dep_1", DefinitionID: clean.ID}); !errors.Is(err, ErrAdmissionRefused) {
			t.Fatalf("got %v", err)
		}
	})
}
