package adapters_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// These are deterministic unit tests over delivery and execution port doubles.
// They prove the verdict table of the deployment effect reconciler, that an
// unreadable record never yields an affirmative verdict, and that only the
// query's own scope is ever read. They do not certify SQL or row-level
// security: T-067 qualifies those against real persistence.

var (
	effNow      = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	effScope    = domain.Scope{TenantID: "t_acme", ProjectID: "p_inv"}
	effOtherPrj = domain.Scope{TenantID: "t_acme", ProjectID: "p_other"}
	effOtherTen = domain.Scope{TenantID: "t_globex", ProjectID: "p_inv"}
)

const (
	effNS     = "preview"
	effAction = "act:0123456789abcdef"
	effRun    = domain.RunID("run_deploy")
	effStep   = "deploy"
)

func effRegistration(scope domain.Scope, run domain.RunID, step, action string) delivery.Registration {
	exp := effNow.Add(24 * time.Hour)
	return delivery.Registration{
		Scope: scope, Environment: "env_1", ConfigDigest: runtimev1.Digest("sha256:" + strings.Repeat("a", 64)), Namespace: effNS,
		Source:   delivery.Source{RepositoryURL: "https://git.example.invalid/r.git", Commit: strings.Repeat("1", 40)},
		Image:    delivery.Image{ArtifactID: "art_1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("b", 64))},
		Producer: delivery.Producer{Attempt: "att_build", Run: "run_build", Step: "build", ToolImage: "registry.example.invalid/tools@sha256:" + strings.Repeat("c", 64)},
		Run:      run, Step: step, ActionKey: action, Attempt: "att_deploy", ExpiresAt: &exp,
	}
}

func effObjects() []delivery.ObjectRef {
	return []delivery.ObjectRef{{APIVersion: "apps/v1", Kind: "Deployment", Namespace: effNS, Name: "web"}}
}

// recordState builds a deployment in one lifecycle state.
type recordState func(t *testing.T, d *delivery.Deployment)

// must returns v or fails the test through a panic the test runner reports.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func inState(t *testing.T, scope domain.Scope, run domain.RunID, step, action string, states ...recordState) delivery.Deployment {
	t.Helper()
	d := must(delivery.NewDeployment(effRegistration(scope, run, step, action), effNow))
	for _, s := range states {
		s(t, &d)
	}
	return d
}

var (
	applying recordState = func(t *testing.T, d *delivery.Deployment) {
		must(d.BeginApply(effObjects(), effNow))
	}
	unknown recordState = func(t *testing.T, d *delivery.Deployment) {
		must(d.MarkOutcomeUnknown(effNow))
	}
	succeeded recordState = func(t *testing.T, d *delivery.Deployment) {
		obs := effObjects()
		obs[0].UID, obs[0].Ready = "00000000-0000-4000-8000-000000000001", true
		must(d.Settle(delivery.Settlement{Succeeded: true, Objects: obs}, effNow))
	}
	failed recordState = func(t *testing.T, d *delivery.Deployment) {
		must(d.Settle(delivery.Settlement{Reason: "apply_failed"}, effNow))
	}
	cleanupRequested recordState = func(t *testing.T, d *delivery.Deployment) {
		must(d.RequestCleanup(effNow))
	}
	// applyingNoObjects is a running record that names no object identity: the
	// script could not observe anything for it.
	applyingNoObjects recordState = func(t *testing.T, d *delivery.Deployment) {
		rec := d.Record()
		rec.Status, rec.Reason = delivery.StatusRunning, delivery.ReasonApplying
		*d = must(delivery.RestoreDeployment(rec))
	}
)

// recordMode selects how the record double answers.
type recordMode int

const (
	modeServe recordMode = iota
	// modeWithheld is an outage: every read fails with an unclassified error.
	modeWithheld
	// modeUnavailable is a classified dependency outage.
	modeUnavailable
	// modeUnreadable is a stored record that cannot be interpreted.
	modeUnreadable
	// modeLeaky ignores the scope of the read and answers from any scope: a
	// defective store the adapter must not trust blindly.
	modeLeaky
)

// recordsDouble is the scoped deployment-record port double.
type recordsDouble struct {
	mu     sync.Mutex
	mode   recordMode
	byKey  map[recKey]delivery.Deployment
	reads  []domain.Scope
	failed int
}

type recKey struct {
	scope  domain.Scope
	action string
}

func newRecords() *recordsDouble { return &recordsDouble{byKey: map[recKey]delivery.Deployment{}} }

func (r *recordsDouble) put(d delivery.Deployment) {
	rec := d.Record()
	r.byKey[recKey{rec.Scope, rec.ActionKey}] = d
}

func (r *recordsDouble) setMode(m recordMode) { r.mu.Lock(); r.mode = m; r.mu.Unlock() }

func (r *recordsDouble) readsOf() []domain.Scope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Scope(nil), r.reads...)
}

func (r *recordsDouble) refuse() error {
	switch r.mode {
	case modeWithheld:
		r.failed++
		return errors.New("connection reset by peer")
	case modeUnavailable:
		r.failed++
		return delivery.Fail(delivery.ErrUnavailable, nil, "the dependency could not answer")
	case modeUnreadable:
		r.failed++
		return delivery.Fail(delivery.ErrInvalid, nil, "stored record cannot be restored")
	}
	return nil
}

func (r *recordsDouble) FindByAction(_ context.Context, scope domain.Scope, action string) (delivery.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, scope)
	if err := r.refuse(); err != nil {
		return delivery.Deployment{}, err
	}
	if r.mode == modeLeaky {
		for k, d := range r.byKey {
			if k.action == action {
				return d, nil
			}
		}
	}
	if d, ok := r.byKey[recKey{scope, action}]; ok {
		return d, nil
	}
	return delivery.Deployment{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
}

func (r *recordsDouble) GetDeployment(_ context.Context, scope domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, scope)
	if err := r.refuse(); err != nil {
		return delivery.Deployment{}, err
	}
	for k, d := range r.byKey {
		if d.ID() == id && (k.scope == scope || r.mode == modeLeaky) {
			return d, nil
		}
	}
	return delivery.Deployment{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
}

// bindingsDouble is the cleanup-binding port double: scope-aware, with a
// withheld mode.
type bindingsDouble struct {
	mu       sync.Mutex
	m        map[recBinding]delivery.DeploymentID
	err      error
	consults []domain.Scope
}

type recBinding struct {
	scope domain.Scope
	run   domain.RunID
}

func newBindings() *bindingsDouble { return &bindingsDouble{m: map[recBinding]delivery.DeploymentID{}} }

func (b *bindingsDouble) bind(scope domain.Scope, run domain.RunID, id delivery.DeploymentID) {
	b.m[recBinding{scope, run}] = id
}

func (b *bindingsDouble) Binding(_ context.Context, scope domain.Scope, run domain.RunID) (delivery.DeploymentID, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consults = append(b.consults, scope)
	if b.err != nil {
		return "", false, b.err
	}
	id, ok := b.m[recBinding{scope, run}]
	return id, ok, nil
}

func newEffects(t *testing.T, r *recordsDouble, b *bindingsDouble) *adapters.Effects {
	t.Helper()
	return must(adapters.NewEffects(r, b))
}

func effQuery(scope domain.Scope, run domain.RunID, step, action string) execution.EffectQuery {
	return execution.EffectQuery{Scope: scope, RunID: run, StepID: step, AttemptID: "att_deploy_1", ActionKey: action}
}

func Test_delivery_effect_constructor(t *testing.T) {
	if _, err := adapters.NewEffects(nil, newBindings()); err == nil {
		t.Fatal("records are required")
	}
	if _, err := adapters.NewEffects(newRecords(), nil); err == nil {
		t.Fatal("cleanup bindings are required")
	}
	e := newEffects(t, newRecords(), newBindings())
	for name, q := range map[string]execution.EffectQuery{
		"no scope":      {RunID: effRun, StepID: effStep, ActionKey: effAction},
		"no run":        {Scope: effScope, StepID: effStep, ActionKey: effAction},
		"no action key": {Scope: effScope, RunID: effRun, StepID: effStep},
	} {
		ev, err := e.Reconcile(context.Background(), q)
		if !errors.Is(err, execution.ErrInvalid) || ev.Verdict != "" {
			t.Fatalf("%s: (%+v, %v)", name, ev, err)
		}
	}
}

func Test_delivery_effect_replay_safe(t *testing.T) {
	ctx := context.Background()
	const cleanupRun = domain.RunID("run_cleanup")

	apply := []struct {
		name   string
		states []recordState // nil: no record at all
		absent bool
		want   execution.Verdict
	}{
		{name: "no record: nothing was applied", absent: true, want: execution.VerdictReplaySafe},
		{name: "registered but not started", want: execution.VerdictReplaySafe},
		{name: "applying with recorded identities", states: []recordState{applying}, want: execution.VerdictReplaySafe},
		{name: "applying without recorded identities", states: []recordState{applyingNoObjects}, want: execution.VerdictStillUnknown},
		{name: "unknown outcome with recorded identities", states: []recordState{applying, unknown}, want: execution.VerdictReplaySafe},
		{name: "unknown outcome without recorded identities", states: []recordState{applyingNoObjects, unknown}, want: execution.VerdictReplaySafe},
		{name: "settled succeeded", states: []recordState{applying, succeeded}, want: execution.VerdictReplaySafe},
		{name: "settled failed", states: []recordState{applying, failed}, want: execution.VerdictReplaySafe},
		{name: "failed before it ever applied", states: []recordState{failed}, want: execution.VerdictReplaySafe},
		{name: "settled with cleanup requested", states: []recordState{applying, succeeded, cleanupRequested}, want: execution.VerdictReplaySafe},
	}
	for _, c := range apply {
		t.Run(c.name, func(t *testing.T) {
			records, bindings := newRecords(), newBindings()
			if !c.absent {
				records.put(inState(t, effScope, effRun, effStep, effAction, c.states...))
			}
			ev, err := newEffects(t, records, bindings).Reconcile(ctx, effQuery(effScope, effRun, effStep, effAction))
			if err != nil || ev.Verdict != c.want || ev.Source != adapters.EffectSource {
				t.Fatalf("got (%+v, %v), want %s from %s", ev, err, c.want, adapters.EffectSource)
			}
			if err := ev.Validate(); err != nil {
				t.Fatalf("evidence is not acceptable to execution: %v", err)
			}
		})
	}

	cleanup := []struct {
		name   string
		states []recordState
		want   execution.Verdict
	}{
		{name: "cleanup requested", states: []recordState{applying, succeeded, cleanupRequested}, want: execution.VerdictReplaySafe},
		{name: "cleanup requested on an unknown outcome", states: []recordState{applying, unknown, cleanupRequested}, want: execution.VerdictReplaySafe},
		{name: "cleanup not requested", states: []recordState{applying, succeeded}, want: execution.VerdictStillUnknown},
		{name: "cleanup not requested while pending", want: execution.VerdictStillUnknown},
	}
	for _, c := range cleanup {
		t.Run("cleanup run: "+c.name, func(t *testing.T) {
			records, bindings := newRecords(), newBindings()
			d := inState(t, effScope, effRun, effStep, effAction, c.states...)
			records.put(d)
			bindings.bind(effScope, cleanupRun, d.ID())
			// The cleanup run's own action key has no deployment of its own:
			// the verdict comes from the deployment the run is bound to.
			ev, err := newEffects(t, records, bindings).Reconcile(ctx, effQuery(effScope, cleanupRun, "cleanup", "act:fedcba9876543210"))
			if err != nil || ev.Verdict != c.want || ev.Source != adapters.EffectSource {
				t.Fatalf("got (%+v, %v), want %s", ev, err, c.want)
			}
		})
	}

	t.Run("a cleanup run bound to a deployment that cannot be found stays unknown", func(t *testing.T) {
		records, bindings := newRecords(), newBindings()
		bindings.bind(effScope, cleanupRun, delivery.DeploymentIDFor(effScope, "act:aaaaaaaaaaaaaaaa"))
		ev, err := newEffects(t, records, bindings).Reconcile(ctx, effQuery(effScope, cleanupRun, "cleanup", "act:fedcba9876543210"))
		if err != nil || ev.Verdict != execution.VerdictStillUnknown {
			t.Fatalf("got (%+v, %v)", ev, err)
		}
	})

	t.Run("a record that is not the step's own proves nothing", func(t *testing.T) {
		for name, d := range map[string]delivery.Deployment{
			"another run":  inState(t, effScope, "run_other", effStep, effAction, applying),
			"another step": inState(t, effScope, effRun, "other", effAction, applying),
		} {
			records := newRecords()
			records.put(d)
			ev, err := newEffects(t, records, newBindings()).Reconcile(ctx, effQuery(effScope, effRun, effStep, effAction))
			if err != nil || ev.Verdict != execution.VerdictStillUnknown {
				t.Fatalf("%s: got (%+v, %v)", name, ev, err)
			}
		}
	})

	t.Run("the verdict follows the record, not the attempt: it changes when the record does", func(t *testing.T) {
		records, bindings := newRecords(), newBindings()
		e := newEffects(t, records, bindings)
		records.put(inState(t, effScope, effRun, effStep, effAction, applyingNoObjects))
		q := effQuery(effScope, effRun, effStep, effAction)
		if ev, _ := e.Reconcile(ctx, q); ev.Verdict != execution.VerdictStillUnknown {
			t.Fatalf("got %s", ev.Verdict)
		}
		records.put(inState(t, effScope, effRun, effStep, effAction, applying))
		if ev, _ := e.Reconcile(ctx, q); ev.Verdict != execution.VerdictReplaySafe {
			t.Fatalf("got %s", ev.Verdict)
		}
	})
}

// uncertainAttempt admits a deployment step whose launch is unknown and returns
// its blocked attempt.
func uncertainAttempt(t *testing.T) (*executiontest.Mem, *execution.AttemptSpec, *execution.Run) {
	t.Helper()
	m := executiontest.NewMem(t)
	m.GrantAll()
	p := m.NewProject(executiontest.ScopeA)
	def := m.Define(executiontest.ScopeA, pipelinetest.Doc(pipelinetest.Container("deploy",
		"workspace: {access: write, from: run}", "retry: {maxAttempts: 3, backoffSeconds: 30, on: [infrastructure]}")))
	run := m.MustAdmit(p, def, nil)
	in := m.MustClaim(executiontest.ScopeA.TenantID)
	if err := m.Observe(in, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := m.Terminate(in, execution.LaunchUnknown); err != nil {
		t.Fatal(err)
	}
	return m, in, run
}

// reconcileThroughExecution does what execution does with a reconciler: ask it,
// and apply the evidence only when it answered.
func reconcileThroughExecution(ctx context.Context, m *executiontest.Mem, e execution.EffectReconciler, in *execution.AttemptSpec) error {
	ev, err := e.Reconcile(ctx, execution.EffectQuery{Scope: in.Scope, RunID: in.RunID, StepID: in.StepID, AttemptID: in.AttemptID, ActionKey: in.ActionKey})
	if err != nil {
		return err
	}
	return m.Svc.ReconcileAttempt(ctx, in.Scope, in.AttemptID, ev)
}

func Test_delivery_effect_withheld_blocks(t *testing.T) {
	ctx := context.Background()

	assertBlocked := func(t *testing.T, m *executiontest.Mem, in *execution.AttemptSpec, run *execution.Run) {
		t.Helper()
		agg := m.Get(run)
		c := agg.Control(in.AttemptID)
		if c.Settled() || c.ReconciledAt != nil || c.ReplaySafe || c.Effect != execution.EffectUnknown {
			t.Fatalf("an uncertain attempt was released: %+v", c)
		}
		if st := agg.Step("deploy"); st.Status != execution.StepRunning {
			t.Fatalf("step is %s, want blocked in running", st.Status)
		}
		m.Advance(time.Hour)
		if _, err := m.Svc.ClaimRunnable(ctx, in.Scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a blocked step was claimed: %v", err)
		}
		if len(m.Get(run).Attempts) != 1 {
			t.Fatal("a second attempt was created")
		}
	}

	modes := map[string]recordMode{
		"an outage":            modeWithheld,
		"a classified outage":  modeUnavailable,
		"an unreadable record": modeUnreadable,
	}
	for name, mode := range modes {
		t.Run(name+" never releases the attempt, and a later answer does", func(t *testing.T) {
			m, in, run := uncertainAttempt(t)
			records, bindings := newRecords(), newBindings()
			// The record exists and would be replay safe, but is withheld.
			records.put(inState(t, in.Scope, in.RunID, in.StepID, in.ActionKey, applying))
			e := newEffects(t, records, bindings)

			records.setMode(mode)
			q := execution.EffectQuery{Scope: in.Scope, RunID: in.RunID, StepID: in.StepID, AttemptID: in.AttemptID, ActionKey: in.ActionKey}
			ev, err := e.Reconcile(ctx, q)
			if ev.Verdict == execution.VerdictReplaySafe || ev.Verdict == execution.VerdictSettled {
				t.Fatalf("withheld record produced %+v", ev)
			}
			if err == nil && ev.Verdict != execution.VerdictStillUnknown {
				t.Fatalf("no error and no still_unknown: %+v", ev)
			}
			if err != nil && !errors.Is(err, execution.ErrUnavailable) {
				t.Fatalf("an outage must be unavailable, got %v", err)
			}
			_ = reconcileThroughExecution(ctx, m, e, in)
			assertBlocked(t, m, in, run)

			// Recovery: once the record is readable the same adapter resolves
			// the attempt without any change to the attempt.
			records.setMode(modeServe)
			if err := reconcileThroughExecution(ctx, m, e, in); err != nil {
				t.Fatal(err)
			}
			agg := m.Get(run)
			if c := agg.Control(in.AttemptID); !c.Settled() || !c.ReplaySafe {
				t.Fatalf("a readable record did not release the attempt: %+v", c)
			}
			if st := agg.Step("deploy"); st.Status != execution.StepPending {
				t.Fatalf("step is %s after replay-safe evidence", st.Status)
			}
		})
	}

	t.Run("an unreadable cleanup binding blocks", func(t *testing.T) {
		m, in, run := uncertainAttempt(t)
		records, bindings := newRecords(), newBindings()
		records.put(inState(t, in.Scope, in.RunID, in.StepID, in.ActionKey, applying))
		bindings.err = errors.New("binding store down")
		e := newEffects(t, records, bindings)
		if err := reconcileThroughExecution(ctx, m, e, in); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if n := len(records.readsOf()); n != 0 {
			t.Fatalf("a record was read (%d) although the run could not be classified", n)
		}
		assertBlocked(t, m, in, run)
	})

	t.Run("a record without observable identities leaves the attempt blocked", func(t *testing.T) {
		m, in, run := uncertainAttempt(t)
		records := newRecords()
		records.put(inState(t, in.Scope, in.RunID, in.StepID, in.ActionKey, applyingNoObjects))
		if err := reconcileThroughExecution(ctx, m, newEffects(t, records, newBindings()), in); err != nil {
			t.Fatal(err)
		}
		assertBlocked(t, m, in, run)
	})

	t.Run("a store that answers for another scope cannot release the attempt", func(t *testing.T) {
		m, in, run := uncertainAttempt(t)
		records := newRecords()
		records.put(inState(t, effOtherTen, in.RunID, in.StepID, in.ActionKey, applying))
		records.setMode(modeLeaky)
		if err := reconcileThroughExecution(ctx, m, newEffects(t, records, newBindings()), in); err != nil {
			t.Fatal(err)
		}
		assertBlocked(t, m, in, run)
	})

	t.Run("the no-record verdict is replay safe and releases exactly once", func(t *testing.T) {
		m, in, run := uncertainAttempt(t)
		e := newEffects(t, newRecords(), newBindings())
		for i := 0; i < 2; i++ {
			if err := reconcileThroughExecution(ctx, m, e, in); err != nil {
				t.Fatal(err)
			}
		}
		if c := m.Get(run).Control(in.AttemptID); !c.Settled() || !c.ReplaySafe {
			t.Fatalf("%+v", c)
		}
	})
}

func Test_delivery_effect_scope(t *testing.T) {
	ctx := context.Background()
	q := effQuery(effScope, effRun, effStep, effAction)

	t.Run("another project's or tenant's record is not consulted or revealed", func(t *testing.T) {
		for name, foreign := range map[string]domain.Scope{"project": effOtherPrj, "tenant": effOtherTen} {
			records, bindings := newRecords(), newBindings()
			// The foreign scope holds a record for the very same action key, in a
			// state that would be affirmative or distinct.
			records.put(inState(t, foreign, effRun, effStep, effAction, applyingNoObjects))
			ev, err := newEffects(t, records, bindings).Reconcile(ctx, q)
			// Same answer as if the foreign record did not exist: no record here.
			if err != nil || ev.Verdict != execution.VerdictReplaySafe || ev.Source != adapters.EffectSource {
				t.Fatalf("%s: got (%+v, %v)", name, ev, err)
			}
			for _, s := range records.readsOf() {
				if s != effScope {
					t.Fatalf("%s: the store was asked about %v", name, s)
				}
			}
			for _, s := range bindings.consults {
				if s != effScope {
					t.Fatalf("%s: the bindings were asked about %v", name, s)
				}
			}
		}
	})

	t.Run("a foreign record is indistinguishable from none", func(t *testing.T) {
		withForeign, without := newRecords(), newRecords()
		withForeign.put(inState(t, effOtherTen, effRun, effStep, effAction, applying, succeeded))
		a, errA := newEffects(t, withForeign, newBindings()).Reconcile(ctx, q)
		b, errB := newEffects(t, without, newBindings()).Reconcile(ctx, q)
		if errA != nil || errB != nil || a != b {
			t.Fatalf("(%+v, %v) differs from (%+v, %v)", a, errA, b, errB)
		}
	})

	t.Run("the query's own scope is used for each query", func(t *testing.T) {
		records, bindings := newRecords(), newBindings()
		records.put(inState(t, effScope, effRun, effStep, effAction, applyingNoObjects))
		records.put(inState(t, effOtherPrj, effRun, effStep, effAction, applying))
		e := newEffects(t, records, bindings)
		own, _ := e.Reconcile(ctx, q)
		theirs, _ := e.Reconcile(ctx, effQuery(effOtherPrj, effRun, effStep, effAction))
		if own.Verdict != execution.VerdictStillUnknown || theirs.Verdict != execution.VerdictReplaySafe {
			t.Fatalf("own %s, other %s", own.Verdict, theirs.Verdict)
		}
	})

	t.Run("a binding of another scope does not make a run a cleanup run", func(t *testing.T) {
		records, bindings := newRecords(), newBindings()
		d := inState(t, effOtherTen, effRun, effStep, effAction, applying, succeeded, cleanupRequested)
		records.put(d)
		// The same run identifier is bound to a deployment in a foreign scope.
		bindings.bind(effOtherTen, effRun, d.ID())
		ev, err := newEffects(t, records, bindings).Reconcile(ctx, q)
		if err != nil || ev.Verdict != execution.VerdictReplaySafe {
			t.Fatalf("got (%+v, %v)", ev, err)
		}
		if got := records.readsOf(); len(got) != 1 || got[0] != effScope {
			t.Fatalf("reads %v", got)
		}
	})

	t.Run("a cleanup binding naming a foreign deployment gives no affirmative evidence", func(t *testing.T) {
		records, bindings := newRecords(), newBindings()
		foreign := inState(t, effOtherTen, effRun, effStep, effAction, applying, succeeded, cleanupRequested)
		records.put(foreign)
		// A corrupt binding in our scope points at the foreign deployment id.
		bindings.bind(effScope, "run_cleanup", foreign.ID())
		ev, err := newEffects(t, records, bindings).Reconcile(ctx, effQuery(effScope, "run_cleanup", "cleanup", "act:fedcba9876543210"))
		if err != nil || ev.Verdict != execution.VerdictStillUnknown {
			t.Fatalf("got (%+v, %v)", ev, err)
		}
		for _, s := range records.readsOf() {
			if s != effScope {
				t.Fatalf("the store was asked about %v", s)
			}
		}
	})

	t.Run("a leaky store's foreign record is not believed", func(t *testing.T) {
		records := newRecords()
		records.put(inState(t, effOtherTen, effRun, effStep, effAction, applying, succeeded, cleanupRequested))
		records.setMode(modeLeaky)
		bindings := newBindings()
		e := newEffects(t, records, bindings)
		ev, err := e.Reconcile(ctx, q)
		if err != nil || ev.Verdict != execution.VerdictStillUnknown {
			t.Fatalf("apply: got (%+v, %v)", ev, err)
		}
		foreign := inState(t, effOtherTen, effRun, effStep, effAction)
		bindings.bind(effScope, "run_cleanup", foreign.ID())
		ev, err = e.Reconcile(ctx, effQuery(effScope, "run_cleanup", "cleanup", "act:fedcba9876543210"))
		if err != nil || ev.Verdict != execution.VerdictStillUnknown {
			t.Fatalf("cleanup: got (%+v, %v)", ev, err)
		}
	})

	t.Run("errors and evidence carry no foreign identifier", func(t *testing.T) {
		records := newRecords()
		records.put(inState(t, effOtherTen, effRun, effStep, effAction, applying))
		records.setMode(modeWithheld)
		_, err := newEffects(t, records, newBindings()).Reconcile(ctx, q)
		if err == nil || strings.Contains(err.Error(), string(effOtherTen.TenantID)) {
			t.Fatalf("got %v", err)
		}
	})
}
