package executiontest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

// Seeder creates the peer rows a run needs: users, work items with their
// workspaces and registered definitions. Both fixtures implement it, each over
// its own peers.
type Seeder interface {
	GrantAll()
	NewProject(scope domain.Scope) Project
	Define(scope domain.Scope, yaml string) pipeline.Definition
	CancelWork(scope domain.Scope, id domain.WorkItemID)
	EditSpec(scope domain.Scope, id domain.WorkItemID, spec string)
	ContentRevision(p Project) domain.ContentRevision
}

// Subject is one backend under the contract: the execution Store, a Service
// wired to it and its peers, and the few controls the suite needs.
type Subject struct {
	Store execution.Store
	Svc   *execution.Service
	Seed  Seeder
	Drive *Driver
	// Advance moves the backend's clock (state, deadlines and leases).
	Advance func(time.Duration)
	// EventCount counts the tenant's published events of a catalog type.
	EventCount func(tenant domain.TenantID, eventType string) int
	// Decorator records the runtime calls the Service makes.
	Decorator *runtimetest.Decorator
	// Limits adjusts the dispatch policy.
	Limits *MutableLimits
}

// Subject adapts the PostgreSQL fixture to the contract suite. The PostgreSQL
// run of Contract belongs to the explicit `test` environment: New skips in the
// unit layer.
func (e *Env) Subject() Subject {
	d := &Driver{T: e.T, Svc: e.Svc, Store: e.Store, Rev: e.ContentRevision}
	return Subject{
		Store: e.Store, Svc: e.Svc, Seed: e, Drive: d, Advance: e.Clock.Advance, Decorator: e.Workloads, Limits: e.Limits,
		EventCount: func(tenant domain.TenantID, typ string) int {
			return e.CountTenant(tenant, `SELECT count(*) FROM outbox WHERE event_type = $1`, typ)
		},
	}
}

const (
	readRun  = "workspace: {access: read, from: run}"
	writeRun = "workspace: {access: write, from: run}"
)

// Contract is the execution.Store contract. Every adapter of the store passes
// it: the in-memory store on every unit run and the PostgreSQL store in the
// `test` environment. It asserts what callers of the store rely on (one
// transaction per call, pinned admission, tenant and project scoping,
// all-or-nothing updates, idempotent replay and cancellation, capacity and
// writer exclusion, settlement before terminal completion), never how a
// backend implements it: lock order, isolation level and row-level security
// belong to the PostgreSQL checkpoints.
func Contract(t *testing.T, open func(testing.TB) Subject) {
	t.Helper()
	ctx := context.Background()
	scope := ScopeA
	setup := func(t *testing.T, yaml string) (Subject, Project, pipeline.Definition) {
		t.Helper()
		s := open(t)
		s.Seed.GrantAll()
		p := s.Seed.NewProject(scope)
		return s, p, s.Seed.Define(scope, yaml)
	}
	one := pipelinetest.Doc(pipelinetest.Container("job", readRun))
	caller := execution.Caller{PrincipalID: Engineer, RequestID: "req_contract"}

	t.Run("admission pins the run and replays by run ID", func(t *testing.T) {
		s, p, def := setup(t, one)
		cmd := execution.AdmitCommand{Scope: scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: s.Seed.ContentRevision(p), SourceCommit: CommitA}
		plan, err := s.Svc.Prepare(ctx, caller, cmd)
		if err != nil {
			t.Fatal(err)
		}
		run, created, err := s.Store.Admit(ctx, plan)
		if err != nil || !created {
			t.Fatalf("admit: created=%v err=%v", created, err)
		}
		if run.Status != execution.RunQueued || run.SourceRetention != execution.RetentionPending || run.SnapshotDigest == "" || run.DefinitionID != def.ID {
			t.Fatalf("pinned run: %+v", run)
		}
		// The same plan again: the stored run, nothing new.
		again, created, err := s.Store.Admit(ctx, plan)
		if err != nil || created || again.ID != run.ID || again.SnapshotDigest != run.SnapshotDigest {
			t.Fatalf("replay: created=%v err=%v", created, err)
		}
		if n := s.EventCount(scope.TenantID, eventsv1.TypeRunCreated); n != 1 {
			t.Fatalf("%d run.created events after a replay", n)
		}
		// Later edits never rewrite the pinned specification.
		s.Seed.EditSpec(scope, p.Item, "# Changed\nSomething else.")
		agg, err := s.Store.Get(ctx, scope, run.ID)
		if err != nil || agg.Run.Specification == nil || agg.Run.Specification.Spec != "# Plan\nDo the thing." || agg.Run.ContentRevision != cmd.ContentRevision {
			t.Fatalf("pin moved: %+v %v", agg.Run.Specification, err)
		}
		if len(agg.Steps) != 1 || agg.Steps[0].Status != execution.StepPending || agg.Steps[0].ActionKey == "" {
			t.Fatalf("steps %+v", agg.Steps)
		}
	})

	t.Run("nested data crosses the store boundary by copy", func(t *testing.T) {
		const doc = "{region: {type: string, default: eu}}"
		s := open(t)
		s.Seed.GrantAll()
		p := s.Seed.NewProject(scope)
		def := s.Seed.Define(scope, pipelinetest.DocNamed("nested", doc, pipelinetest.Container("job", readRun, "outputs: {report: {type: string}}")))
		fingerprint := func(run *execution.Run) string {
			t.Helper()
			agg, err := s.Store.Get(ctx, scope, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			type view struct {
				Run      *execution.Run
				Steps    []execution.StepRun
				Specs    []string
				Attempts []*execution.Attempt
				Controls map[domain.AttemptID]*execution.Control
			}
			v := view{Run: agg.Run, Attempts: agg.Attempts, Controls: agg.Controls}
			for _, st := range agg.Steps {
				spec, err := execution.EncodeStep(st.Spec)
				if err != nil {
					t.Fatal(err)
				}
				cp := *st
				cp.Spec = pipeline.Step{}
				v.Steps, v.Specs = append(v.Steps, cp), append(v.Specs, string(spec))
			}
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		// tamper changes every nested value of an aggregate in place.
		tamper := func(agg *execution.Aggregate) {
			r := agg.Run
			if r.Specification != nil {
				r.Specification.Spec = "tampered"
				for i := range r.Specification.AcceptanceCriteria {
					r.Specification.AcceptanceCriteria[i].Text = "tampered"
				}
			}
			for i := range r.Snapshot.Inputs {
				r.Snapshot.Inputs[i].Name, r.Snapshot.Inputs[i].Value = "tampered", runtimev1.StringValue("tampered")
			}
			for i := range r.Snapshot.Secrets {
				r.Snapshot.Secrets[i].Step = "tampered"
			}
			if r.Snapshot.Runtime != nil {
				r.Snapshot.Runtime.Backend = "tampered"
			}
			for _, tp := range []*time.Time{r.StartedAt, r.FinishedAt, r.CancelRequestedAt, r.FailureIntentAt} {
				if tp != nil {
					*tp = tp.Add(time.Hour)
				}
			}
			for _, st := range agg.Steps {
				if len(st.Spec.Command) > 0 {
					st.Spec.Command[0] = "tampered"
				}
				st.Spec.Image = "tampered"
				if st.Spec.Workspace != nil {
					st.Spec.Workspace.Access = pipeline.AccessWrite
				}
				for k := range st.Spec.Outputs {
					st.Spec.Outputs[k] = pipeline.Output{Type: pipeline.TypeInteger}
				}
				for k := range st.Outputs {
					st.Outputs[k] = runtimev1.StringValue("tampered")
				}
				for _, tp := range []*time.Time{st.NextAttemptAt, st.FinishedAt} {
					if tp != nil {
						*tp = tp.Add(time.Hour)
					}
				}
			}
			for _, at := range agg.Attempts {
				for i := range at.Context {
					at.Context[i] = 'x'
				}
				for _, tp := range []*time.Time{at.StartedAt, at.FinishedAt, at.ExecutionDeadline} {
					if tp != nil {
						*tp = tp.Add(time.Hour)
					}
				}
				if at.Result != nil {
					for k := range at.Result.Outputs {
						at.Result.Outputs[k] = runtimev1.StringValue("tampered")
					}
					if at.Result.ExitCode != nil {
						*at.Result.ExitCode = 99
					}
					if at.Result.SourceCommit != nil {
						*at.Result.SourceCommit = "tampered"
					}
				}
			}
			for _, c := range agg.Controls {
				for _, tp := range []*time.Time{c.DispatchConfirmedAt, c.ClaimUntil, c.RevokedAt, c.TerminationRequestedAt, c.TerminationConfirmedAt, c.ReconciledAt, c.SettledAt, c.StepResolvedAt} {
					if tp != nil {
						*tp = tp.Add(time.Hour)
					}
				}
			}
		}
		boom := errors.New("boom")
		failing := map[string]execution.Mutation{
			"mutation error": func(ctx context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
				tamper(agg)
				return execution.Effects{}, boom
			},
			"peer effect error": func(ctx context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
				tamper(agg)
				return execution.Effects{After: []func(context.Context, execution.Env) error{func(context.Context, execution.Env) error { return boom }}}, nil
			},
		}
		checkpoint := func(when string, run *execution.Run) {
			t.Helper()
			want := fingerprint(run)
			agg, err := s.Store.Get(ctx, scope, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			tamper(agg) // a caller changing what a read returned
			if got := fingerprint(run); got != want {
				t.Fatalf("%s: changing a returned aggregate changed stored state\n got %s\nwant %s", when, got, want)
			}
			for name, fn := range failing {
				if _, _, err := s.Store.Update(ctx, scope, run.ID, execution.UpdateOptions{}, fn); !errors.Is(err, boom) {
					t.Fatalf("%s: %s: %v", when, name, err)
				}
				if got := fingerprint(run); got != want {
					t.Fatalf("%s: a failed update (%s) changed stored state\n got %s\nwant %s", when, name, got, want)
				}
			}
			// A committed no-op update stores nothing new.
			if _, _, err := s.Store.Update(ctx, scope, run.ID, execution.UpdateOptions{}, func(ctx context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
				return execution.Effects{}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if got := fingerprint(run); got != want {
				t.Fatalf("%s: a no-op update changed stored state", when)
			}
		}

		plan, err := s.Svc.Prepare(ctx, caller, execution.AdmitCommand{Scope: scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: s.Seed.ContentRevision(p), SourceCommit: CommitA})
		if err != nil {
			t.Fatal(err)
		}
		run, _, err := s.Store.Admit(ctx, plan)
		if err != nil {
			t.Fatal(err)
		}
		want := fingerprint(run)
		tamper(plan.Aggregate) // the caller's plan and the returned run are its own
		tamper(&execution.Aggregate{Run: run})
		if got := fingerprint(run); got != want {
			t.Fatalf("changing the admitted plan changed the stored run\n got %s\nwant %s", got, want)
		}
		checkpoint("admitted", run)
		if _, err := s.Svc.ConfirmSource(ctx, scope, run.ID); err != nil {
			t.Fatal(err)
		}
		checkpoint("source confirmed", run)
		in := s.Drive.MustClaim(scope.TenantID)
		s.Drive.Start(in)
		checkpoint("running", run)
		if _, err := s.Drive.Succeed(in, map[string]runtimev1.Value{"report": runtimev1.StringValue("ok")}, ""); err != nil {
			t.Fatal(err)
		}
		agg := s.Drive.Get(run)
		if agg.Run.Status != execution.RunSucceeded || agg.Step("job").Outputs["report"] != runtimev1.StringValue("ok") || agg.Run.Specification.Spec != "# Plan\nDo the thing." {
			t.Fatalf("the run did not keep its pins and outputs: %s", agg)
		}
		checkpoint("succeeded", run)
	})

	t.Run("a stale content revision conflicts and stores nothing", func(t *testing.T) {
		s, p, def := setup(t, one)
		plan, err := s.Svc.Prepare(ctx, caller, execution.AdmitCommand{Scope: scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: s.Seed.ContentRevision(p), SourceCommit: CommitA})
		if err != nil {
			t.Fatal(err)
		}
		s.Seed.EditSpec(scope, p.Item, "# Raced")
		if _, _, err := s.Store.Admit(ctx, plan); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("admitted a stale pin: %v", err)
		}
		if runs, err := s.Store.List(ctx, scope, execution.ListQuery{Limit: 10}); err != nil || len(runs) != 0 {
			t.Fatalf("%d runs after a refused admission (%v)", len(runs), err)
		}
		if n := s.EventCount(scope.TenantID, eventsv1.TypeRunCreated); n != 0 {
			t.Fatalf("%d events after a refused admission", n)
		}
	})

	t.Run("admission checks the caller's authority", func(t *testing.T) {
		s, p, def := setup(t, one)
		cmd := execution.AdmitCommand{Scope: scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: s.Seed.ContentRevision(p), SourceCommit: CommitA}
		if _, err := s.Svc.Admit(ctx, execution.Caller{PrincipalID: Viewer}, cmd); !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("a viewer admitted a run: %v", err)
		}
		if _, err := s.Svc.Admit(ctx, execution.Caller{PrincipalID: Stranger}, cmd); !errors.Is(err, execution.ErrHidden) {
			t.Fatalf("a stranger saw the project: %v", err)
		}
		if _, err := s.Svc.Admit(ctx, execution.Caller{PrincipalID: OtherEng}, cmd); !errors.Is(err, execution.ErrHidden) {
			t.Fatalf("another tenant's engineer saw the project: %v", err)
		}
	})

	t.Run("runs are scoped by tenant and project", func(t *testing.T) {
		s, p, def := setup(t, one)
		run := s.Drive.MustAdmit(p, def, nil)
		in := s.Drive.MustClaim(scope.TenantID)
		for _, other := range []domain.Scope{ScopeA2, ScopeB} {
			if _, err := s.Store.Get(ctx, other, run.ID); !errors.Is(err, execution.ErrNotFound) {
				t.Fatalf("%s: get: %v", other, err)
			}
			if runs, err := s.Store.List(ctx, other, execution.ListQuery{Limit: 10}); err != nil || len(runs) != 0 {
				t.Fatalf("%s: list %d %v", other, len(runs), err)
			}
			if _, err := s.Store.RunOf(ctx, other, in.AttemptID); !errors.Is(err, execution.ErrNotFound) {
				t.Fatalf("%s: run of attempt: %v", other, err)
			}
			if _, _, err := s.Store.Update(ctx, other, run.ID, execution.UpdateOptions{}, execution.CancelMutation(execution.ReasonCancelled, nil)); !errors.Is(err, execution.ErrNotFound) {
				t.Fatalf("%s: update: %v", other, err)
			}
			if err := s.Svc.ObserveAttempt(ctx, other, in.AttemptID, execution.Observation{Kind: execution.ObservedStarted, WorkloadUID: "11111111-2222-3333-4444-555555555555"}); !errors.Is(err, execution.ErrNotFound) {
				t.Fatalf("%s: observe: %v", other, err)
			}
			if err := s.Store.RecordRemoval(ctx, other, in.AttemptID); !errors.Is(err, execution.ErrNotFound) {
				t.Fatalf("%s: record removal: %v", other, err)
			}
		}
		if refs, err := s.Store.Active(ctx, ScopeB.TenantID, execution.RunRef{}, 10); err != nil || len(refs) != 0 {
			t.Fatalf("another tenant's active scan: %v %v", refs, err)
		}
		if agg := s.Drive.Get(run); agg.Run.CancelRequestedAt != nil || agg.Attempts[0].Status != execution.AttemptRunning {
			t.Fatal("a foreign scope changed the run")
		}
	})

	t.Run("lists page in creation order and scans continue after a cursor", func(t *testing.T) {
		s := open(t)
		s.Seed.GrantAll()
		def := s.Seed.Define(scope, one)
		var ids []domain.RunID
		for i := 0; i < 3; i++ {
			p := s.Seed.NewProject(scope)
			ids = append(ids, s.Drive.MustAdmit(p, def, nil).ID)
			s.Advance(time.Second)
		}
		all, err := s.Store.List(ctx, scope, execution.ListQuery{Limit: 10})
		if err != nil || len(all) != 3 {
			t.Fatalf("list %d %v", len(all), err)
		}
		if !sort.SliceIsSorted(all, func(i, j int) bool { return all[i].CreatedAt.Before(all[j].CreatedAt) }) {
			t.Fatal("list is not in creation order")
		}
		var paged []domain.RunID
		var after *execution.Position
		for {
			page, err := s.Store.List(ctx, scope, execution.ListQuery{Limit: 1, After: after})
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			paged = append(paged, page[0].ID)
			after = &execution.Position{CreatedAt: page[0].CreatedAt, ID: page[0].ID}
		}
		if fmt.Sprint(paged) != fmt.Sprint(ids) {
			t.Fatalf("paged %v, created %v", paged, ids)
		}
		if runs, _ := s.Store.List(ctx, scope, execution.ListQuery{Statuses: []execution.RunStatus{execution.RunFailed}, Limit: 10}); len(runs) != 0 {
			t.Fatalf("status filter returned %d runs", len(runs))
		}
		if runs, _ := s.Store.List(ctx, scope, execution.ListQuery{WorkItemID: all[1].WorkItemID, Limit: 10}); len(runs) != 1 || runs[0].ID != all[1].ID {
			t.Fatalf("work item filter returned %v", runs)
		}
		first, err := s.Store.Active(ctx, scope.TenantID, execution.RunRef{}, 2)
		if err != nil || len(first) != 2 {
			t.Fatalf("active page %v %v", first, err)
		}
		rest, err := s.Store.Active(ctx, scope.TenantID, first[1], 2)
		if err != nil || len(rest) != 1 || rest[0].ID != ids[2] {
			t.Fatalf("active continuation %v %v", rest, err)
		}
		if _, err := s.Store.Dispatchable(ctx, scope.TenantID, execution.RunRef{}, 0); !errors.Is(err, execution.ErrInvalid) {
			t.Fatalf("an unbounded scan was accepted: %v", err)
		}
	})

	t.Run("dispatch waits for the confirmed source reference", func(t *testing.T) {
		s, p, def := setup(t, one)
		plan, err := s.Svc.Prepare(ctx, caller, execution.AdmitCommand{Scope: scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: s.Seed.ContentRevision(p), SourceCommit: CommitA})
		if err != nil {
			t.Fatal(err)
		}
		run, _, err := s.Store.Admit(ctx, plan)
		if err != nil {
			t.Fatal(err)
		}
		if refs, _ := s.Store.Dispatchable(ctx, scope.TenantID, execution.RunRef{}, 10); len(refs) != 0 {
			t.Fatalf("a run with an unconfirmed source is dispatchable: %v", refs)
		}
		if _, err := s.Drive.Claim(scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("claimed before the source was retained: %v", err)
		}
		if _, err := s.Svc.ConfirmSource(ctx, scope, run.ID); err != nil {
			t.Fatal(err)
		}
		if refs, _ := s.Store.Dispatchable(ctx, scope.TenantID, execution.RunRef{}, 10); len(refs) != 1 {
			t.Fatalf("dispatchable %v after confirmation", refs)
		}
		if in := s.Drive.MustClaim(scope.TenantID); in.RunID != run.ID {
			t.Fatalf("claimed %s", in.RunID)
		}
	})

	t.Run("an update is all or nothing", func(t *testing.T) {
		s, p, def := setup(t, one)
		run := s.Drive.MustAdmit(p, def, nil)
		before := s.EventCount(scope.TenantID, eventsv1.TypeRunStateChanged)
		boom := errors.New("boom")
		for name, fn := range map[string]execution.Mutation{
			"mutation error": func(ctx context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
				agg.RequestCancel(execution.ReasonCancelled, env.Now())
				return execution.Effects{}, boom
			},
			"peer effect error": func(ctx context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
				agg.RequestCancel(execution.ReasonCancelled, env.Now())
				return execution.Effects{After: []func(context.Context, execution.Env) error{func(context.Context, execution.Env) error { return boom }}}, nil
			},
		} {
			if _, _, err := s.Store.Update(ctx, scope, run.ID, execution.UpdateOptions{}, fn); !errors.Is(err, boom) {
				t.Fatalf("%s: %v", name, err)
			}
			agg := s.Drive.Get(run)
			if agg.Run.CancelRequestedAt != nil || agg.Run.Version != run.Version || agg.Run.Status != execution.RunQueued {
				t.Fatalf("%s: the failed update changed the run: %+v", name, agg.Run)
			}
			if n := s.EventCount(scope.TenantID, eventsv1.TypeRunStateChanged); n != before {
				t.Fatalf("%s: %d events published by a failed update", name, n-before)
			}
		}
	})

	t.Run("a claim persists one attempt and holds capacity until settlement", func(t *testing.T) {
		s, p, def := setup(t, one)
		lim := DefaultLimit()
		lim.TenantConcurrency, lim.ProjectConcurrency = 1, 1
		s.Limits.Set(lim)
		first := s.Drive.MustAdmit(p, def, nil)
		s.Advance(time.Second)
		second := s.Drive.MustAdmit(s.Seed.NewProject(scope), def, nil)

		in := s.Drive.MustClaim(scope.TenantID)
		if in.RunID != first.ID || in.Ordinal != 1 || in.ActionKey == "" || in.WorkloadName == "" {
			t.Fatalf("intent %+v", in)
		}
		agg := s.Drive.Get(first)
		if len(agg.Attempts) != 1 || agg.Attempts[0].ID != in.AttemptID || agg.Attempts[0].Status != execution.AttemptRunning ||
			agg.Step("job").Status != execution.StepRunning || agg.Run.Status != execution.RunRunning {
			t.Fatalf("claim not persisted: %s", agg)
		}
		// The slot is held: the second run waits, and consumed no attempt.
		if _, err := s.Drive.Claim(scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a second claim exceeded the limit: %v", err)
		}
		if got := s.Drive.Get(second); len(got.Attempts) != 0 || got.Step("job").AttemptsStarted != 0 {
			t.Fatalf("a refused claim left an attempt behind: %s", got)
		}
		s.Drive.Start(in)
		ack, err := s.Drive.Succeed(in, nil, "")
		if err != nil || ack.AttemptStatus != execution.AttemptSucceeded {
			t.Fatalf("result: %+v %v", ack, err)
		}
		if got := s.Drive.Get(first); got.Run.Status != execution.RunSucceeded || !got.Control(in.AttemptID).Settled() {
			t.Fatalf("run %s after settlement", got.Run.Status)
		}
		if next := s.Drive.MustClaim(scope.TenantID); next.RunID != second.ID {
			t.Fatalf("the released slot went to %s", next.RunID)
		}
	})

	t.Run("a step that consumes no source is claimed with no workspace", func(t *testing.T) { ClaimWorkspaceFree(t, open) })

	t.Run("a lost start is redriven with the same identity", func(t *testing.T) {
		s, p, def := setup(t, one)
		run := s.Drive.MustAdmit(p, def, nil)
		in := s.Drive.MustClaim(scope.TenantID) // committed, never started
		if got := s.Drive.Get(run).Attempts; len(got) != 1 || s.Drive.Get(run).Control(in.AttemptID).DispatchConfirmedAt != nil {
			t.Fatal("the intent is not an unconfirmed attempt")
		}
		ref := execution.RunRef{Scope: scope, ID: run.ID}
		if rep, err := s.Svc.ReconcileRun(ctx, ref); err != nil || rep.Redriven != 0 {
			t.Fatalf("redrove inside the claim lease: %+v %v", rep, err)
		}
		s.Advance(5 * time.Second) // past the claim lease, inside the startup budget
		rep, err := s.Svc.ReconcileRun(ctx, ref)
		if err != nil || rep.Redriven != 1 {
			t.Fatalf("redrive: %+v %v", rep, err)
		}
		starts := s.Decorator.Starts()
		if len(starts) != 1 || starts[0].AttemptID != in.AttemptID || starts[0].WorkloadName != in.WorkloadName || starts[0].ActionKey != in.ActionKey || !starts[0].Redrive {
			t.Fatalf("starts %+v", starts)
		}
		if agg := s.Drive.Get(run); len(agg.Attempts) != 1 || agg.Control(in.AttemptID).DispatchConfirmedAt == nil {
			t.Fatalf("the redrive created another attempt or did not confirm: %s", agg)
		}
	})

	t.Run("cancellation is idempotent, fences the attempt and settles before it is terminal", func(t *testing.T) {
		s, p, def := setup(t, pipelinetest.Doc(pipelinetest.Container("job", readRun), pipelinetest.Container("after", "needs: [job]", readRun)))
		run := s.Drive.MustAdmit(p, def, nil)
		in := s.Drive.MustClaim(scope.TenantID)
		s.Drive.Start(in)
		got, err := s.Svc.Cancel(ctx, caller, scope, run.ID)
		if err != nil || got.CancelRequestedAt == nil || got.Status.Terminal() {
			t.Fatalf("cancel: %+v %v", got, err)
		}
		events := s.EventCount(scope.TenantID, eventsv1.TypeRunStateChanged)
		if again, err := s.Svc.Cancel(ctx, caller, scope, run.ID); err != nil || !again.CancelRequestedAt.Equal(*got.CancelRequestedAt) {
			t.Fatalf("a repeat moved the request: %+v %v", again, err)
		}
		if n := s.EventCount(scope.TenantID, eventsv1.TypeRunStateChanged); n != events {
			t.Fatalf("a repeated cancellation published %d events", n-events)
		}
		agg := s.Drive.Get(run)
		if agg.Attempts[0].Status != execution.AttemptCancelled || agg.Control(in.AttemptID).Live() || agg.Control(in.AttemptID).Settled() || agg.Step("after").Status != execution.StepCancelled {
			t.Fatalf("cancelled attempt: %s %v", agg, agg.Control(in.AttemptID))
		}
		if _, err := s.Drive.Succeed(in, nil, ""); !errors.Is(err, execution.ErrAttemptRevoked) {
			t.Fatalf("a cancelled attempt's result was accepted: %v", err)
		}
		if _, err := s.Drive.Claim(scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("dispatched after cancellation: %v", err)
		}
		if err := s.Drive.Terminate(in, execution.LaunchStarted); err != nil {
			t.Fatal(err)
		}
		if agg := s.Drive.Get(run); agg.Run.Status != execution.RunCancelled || !agg.Control(in.AttemptID).Settled() {
			t.Fatalf("run %s after settlement", agg.Run.Status)
		}
		if _, err := s.Svc.Cancel(ctx, caller, scope, run.ID); err != nil {
			t.Fatalf("cancelling a cancelled run: %v", err)
		}
	})

	t.Run("work cancellation cancels the item's runs once", func(t *testing.T) {
		s, p, def := setup(t, one)
		run := s.Drive.MustAdmit(p, def, nil)
		c := execution.WorkCancellation{Scope: scope, WorkItemID: p.Item, Version: 2}
		for i := 0; i < 2; i++ {
			if err := s.Svc.RequestCancellation(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		agg := s.Drive.Get(run)
		if agg.Run.Status != execution.RunCancelled || agg.Run.CancelReason != execution.ReasonWorkCancelled {
			t.Fatalf("run %s (%s)", agg.Run.Status, agg.Run.CancelReason)
		}
		cancelled := s.EventCount(scope.TenantID, eventsv1.TypeRunStateChanged)
		if err := s.Svc.RequestCancellation(ctx, c); err != nil {
			t.Fatal(err)
		}
		if n := s.EventCount(scope.TenantID, eventsv1.TypeRunStateChanged); n != cancelled {
			t.Fatalf("a redelivery published %d events", n-cancelled)
		}
		// The item is cancelled in the work module: nothing new is admitted, and
		// the periodic scan repeats the request without effect.
		s.Seed.CancelWork(scope, p.Item)
		if n, err := s.Svc.ReconcileCancellations(ctx, scope.TenantID); err != nil || n != 1 {
			t.Fatalf("scan: %d %v", n, err)
		}
		if _, err := s.Drive.Admit(p, def, nil); !errors.Is(err, execution.ErrWorkCancelled) {
			t.Fatalf("admitted work for a cancelled item: %v", err)
		}
	})

	t.Run("one writer holds a workspace until its attempt settles", func(t *testing.T) {
		s, p, def := setup(t, pipelinetest.Doc(pipelinetest.Container("edit", writeRun)))
		first := s.Drive.MustAdmit(p, def, nil)
		s.Advance(time.Second)
		second := s.Drive.MustAdmit(p, def, nil)
		in := s.Drive.MustClaim(scope.TenantID)
		if in.RunID != first.ID {
			t.Fatalf("claimed %s", in.RunID)
		}
		if _, err := s.Drive.Claim(scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a second writer was admitted: %v", err)
		}
		if got := s.Drive.Get(second); len(got.Attempts) != 0 {
			t.Fatalf("the refused writer left %d attempts", len(got.Attempts))
		}
		s.Drive.Start(in)
		// An output nobody published is not a settled writer result: refused, and
		// the attempt keeps running with its slot.
		if _, err := s.Drive.Succeed(in, nil, CommitB); err == nil {
			t.Fatal("an unpublished writer output was accepted")
		}
		if at := s.Drive.Get(first).Attempt(in.AttemptID); at.Status != execution.AttemptRunning || at.ResultHash != "" {
			t.Fatalf("a refused result changed the attempt: %s", at.Status)
		}
		if _, err := s.Drive.Succeed(in, nil, CommitA); err != nil { // a writer that changed nothing reports its tip
			t.Fatal(err)
		}
		if got := s.Drive.MustClaim(scope.TenantID); got.RunID != second.ID {
			t.Fatalf("the released writer slot went to %s", got.RunID)
		}
	})

	t.Run("a settled attempt's workload removal is recorded once", func(t *testing.T) {
		s, p, def := setup(t, one)
		run := s.Drive.MustAdmit(p, def, nil)
		in := s.Drive.MustClaim(scope.TenantID)
		backend := runtimetest.ManualBackend
		if err := s.Store.RecordRemoval(ctx, scope, in.AttemptID); !errors.Is(err, execution.ErrInvalidTransition) {
			t.Fatalf("removal of an unsettled attempt: %v", err)
		}
		s.Drive.Start(in)
		if _, err := s.Drive.Succeed(in, nil, ""); err != nil {
			t.Fatal(err)
		}
		rem, err := s.Store.Removable(ctx, scope.TenantID, backend, execution.RemovalCursor{}, 10)
		if err != nil || len(rem) != 1 || rem[0].Workload.AttemptID != in.AttemptID || rem[0].Workload.Name != in.WorkloadName {
			t.Fatalf("removable %+v %v", rem, err)
		}
		if other, _ := s.Store.Removable(ctx, scope.TenantID, "another-backend", execution.RemovalCursor{}, 10); len(other) != 0 {
			t.Fatalf("a run pinned to %q is removable by another backend: %v", backend, other)
		}
		for i := 0; i < 2; i++ {
			if err := s.Store.RecordRemoval(ctx, scope, in.AttemptID); err != nil {
				t.Fatalf("record %d: %v", i, err)
			}
		}
		if rem, _ := s.Store.Removable(ctx, scope.TenantID, backend, execution.RemovalCursor{}, 10); len(rem) != 0 {
			t.Fatalf("a recorded removal is listed again: %v", rem)
		}
		if agg := s.Drive.Get(run); agg.Run.Status != execution.RunSucceeded {
			t.Fatalf("run %s", agg.Run.Status)
		}
	})
}

// ClaimWorkspaceFree is the workspace-free claim shape of a work run: a step
// that consumes no source is claimed with no workspace, input commit or tip,
// while a source-consuming step keeps the run's workspace. Contract includes
// it; it also runs alone as the T-094 acceptance.
func ClaimWorkspaceFree(t *testing.T, open func(testing.TB) Subject) {
	t.Helper()
	scope := ScopeA
	s := open(t)
	s.Seed.GrantAll()
	p := s.Seed.NewProject(scope)
	def := s.Seed.Define(scope, pipelinetest.Doc(
		pipelinetest.Container("gen", "workspace: {access: none}"),
		pipelinetest.Container("job", "needs: [gen]", readRun),
		pipelinetest.Container("fix", "needs: [job]", writeRun),
	))
	run := s.Drive.MustAdmit(p, def, nil)
	if run.WorkspaceID == "" {
		t.Fatal("a work run is bound to its workspace")
	}
	want := map[string]struct {
		access    execution.Access
		workspace string
	}{
		"gen": {execution.AccessNone, ""},
		"job": {execution.AccessRead, run.WorkspaceID},
		"fix": {execution.AccessWrite, run.WorkspaceID},
	}
	for _, id := range []string{"gen", "job", "fix"} {
		in := s.Drive.MustClaim(scope.TenantID)
		if in.StepID != id || in.Access != want[id].access || in.WorkspaceID != want[id].workspace {
			t.Fatalf("claim of %s: step=%s access=%q workspace=%q", id, in.StepID, in.Access, in.WorkspaceID)
		}
		var at *execution.Attempt
		for _, a := range s.Drive.Get(run).Attempts {
			if a.ID == in.AttemptID {
				at = a
			}
		}
		if at == nil || at.Access != want[id].access || at.WorkspaceID != want[id].workspace {
			t.Fatalf("stored attempt of %s: %+v", id, at)
		}
		if (at.Access == execution.AccessNone) != (at.InputCommit == "" && at.ExpectedTip == "") ||
			(at.Access == execution.AccessWrite) != (at.ExpectedTip != "") {
			t.Fatalf("attempt of %s breaks the workspace shape: %+v", id, at)
		}
		s.Drive.Start(in)
		commit := ""
		if id == "fix" {
			commit = CommitA // a writer that changed nothing reports its tip
		}
		if _, err := s.Drive.Succeed(in, nil, commit); err != nil {
			t.Fatalf("settle %s: %v", id, err)
		}
	}
	if got := s.Drive.Get(run); got.Run.Status != execution.RunSucceeded {
		t.Fatalf("run %s", got.Run.Status)
	}
}
