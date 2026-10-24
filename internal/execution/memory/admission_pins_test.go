package memory_test

import (
	"context"
	"errors"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

const secretEnv = `env: {TOKEN: {secretRef: "secret://project/token"}}`

func pinnedInput(r *execution.Run, name string) (execution.PinnedInput, bool) {
	for _, in := range r.Snapshot.Inputs {
		if in.Name == name {
			return in, true
		}
	}
	return execution.PinnedInput{}, false
}

func contextOf(t *testing.T, in *execution.AttemptSpec) runtimev1.Context {
	t.Helper()
	c, err := in.DecodeContext()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestUnitAdmissionPins proves a run's definition, work content, source and
// configuration inputs are pinned at admission, never rewritten by later edits,
// and rejected before any state exists when the scope, version or content does
// not match (PX-01, PX-02, PX-15).
func TestUnitAdmissionPins(t *testing.T) {
	ctx := context.Background()
	tenant := scopeA.TenantID

	t.Run("inputs resolve by precedence and carry their provenance", func(t *testing.T) {
		m, p := fixture(t)
		def := m.Define(scopeA, pipelinetest.DocNamed("prec", "{region: {type: string, default: eu}}", job("a", readRun)))
		check := func(r *execution.Run, value, source string) {
			t.Helper()
			in, ok := pinnedInput(r, "region")
			if !ok || in.Value.String() != value || in.Source != source {
				t.Fatalf("region = %q from %q, want %q from %q", in.Value.String(), in.Source, value, source)
			}
		}
		check(m.MustAdmit(p, def, nil), "eu", "default")
		m.World.SetVariable(scopeA, "region", runtimev1.StringValue("us"))
		check(m.MustAdmit(p, def, nil), "us", "project_variable")
		check(m.MustAdmit(p, def, map[string]runtimev1.Value{"region": runtimev1.StringValue("ap")}), "ap", "run_input")
		r := m.MustAdmit(p, def, nil)
		if in, _ := pinnedInput(r, "region"); in.VariableID == "" || in.VariableVersion != 1 {
			t.Fatalf("the variable's exact version is not recorded: %+v", in)
		}
		// Editing the variable later never changes the pinned run, or what a
		// dispatched attempt of it receives.
		m.World.SetVariable(scopeA, "region", runtimev1.StringValue("sa"))
		got := m.Get(r).Run
		if in, _ := pinnedInput(got, "region"); in.Value.String() != "us" || in.VariableVersion != 1 {
			t.Fatalf("a later variable edit rewrote the pin: %+v", in)
		}
	})

	t.Run("a dispatched attempt receives the pinned inputs, not later edits", func(t *testing.T) {
		m, p := fixture(t)
		def := m.Define(scopeA, pipelinetest.DocNamed("prec", "{region: {type: string, default: eu}}", job("a", readRun)))
		m.World.SetVariable(scopeA, "region", runtimev1.StringValue("us"))
		run := m.MustAdmit(p, def, nil)
		m.World.SetVariable(scopeA, "region", runtimev1.StringValue("sa"))
		in := m.MustClaim(tenant)
		if in.RunID != run.ID {
			t.Fatalf("claimed %s", in.RunID)
		}
		if c := contextOf(t, in); c.Inputs["region"].String() != "us" || c.Definition.ID != def.ID {
			t.Fatalf("the attempt received %q of %s, not the pinned value", c.Inputs["region"].String(), c.Definition.ID)
		}
	})

	t.Run("invalid inputs, kinds, scopes and sources fail before any state exists", func(t *testing.T) {
		m, p := fixture(t, executiontest.MemOptions{Kinds: []pipeline.StepKind{pipeline.KindContainer}})
		def := m.Define(scopeA, pipelinetest.DocNamed("typed", "{count: {type: integer, required: true}}", job("a", readRun)))
		gate := m.Define(scopeA, pipelinetest.Doc(job("a", readRun), "{id: g, kind: gate, needs: [a]}"))
		good := map[string]runtimev1.Value{"count": runtimev1.IntValue(1)}
		for name, run := range map[string]func() error{
			"missing required input": func() error { _, err := m.Admit(p, def, nil); return err },
			"type mismatch": func() error {
				_, err := m.Admit(p, def, map[string]runtimev1.Value{"count": runtimev1.StringValue("seven")})
				return err
			},
			"unknown input": func() error {
				_, err := m.Admit(p, def, map[string]runtimev1.Value{"count": runtimev1.IntValue(1), "nope": runtimev1.IntValue(1)})
				return err
			},
			"disabled step kind": func() error { _, err := m.Admit(p, gate, nil); return err },
			"commit outside the workspace history": func() error {
				_, err := m.Svc.Admit(ctx, engineer, execution.AdmitCommand{Scope: scopeA, WorkItemID: p.Item, DefinitionID: def.ID,
					ContentRevision: m.ContentRevision(p), SourceCommit: executiontest.CommitC, Inputs: good})
				return err
			},
		} {
			if err := run(); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Errorf("%s: %v", name, err)
			}
		}
		noRuns := func(when string) {
			t.Helper()
			if runs, err := m.Store.List(ctx, scopeA, execution.ListQuery{Limit: 10}); err != nil || len(runs) != 0 {
				t.Fatalf("%s: %d runs (%v)", when, len(runs), err)
			}
			if n := m.EventCount(tenant, "run.created"); n != 0 {
				t.Fatalf("%s: %d run.created events", when, n)
			}
		}
		noRuns("after rejected inputs")

		// Scope and identity mismatches are not found, never partially admitted.
		cmd := execution.AdmitCommand{Scope: scopeA, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: m.ContentRevision(p), SourceCommit: executiontest.CommitA, Inputs: good}
		for name, mutate := range map[string]func(*execution.AdmitCommand){
			"unknown definition":             func(c *execution.AdmitCommand) { c.DefinitionID = "pd_missing" },
			"unknown work item":              func(c *execution.AdmitCommand) { c.WorkItemID = "wi_missing" },
			"another project's work item":    func(c *execution.AdmitCommand) { c.Scope = executiontest.ScopeA2 },
			"another tenant's work item":     func(c *execution.AdmitCommand) { c.Scope = executiontest.ScopeB },
			"a work item of another project": func(c *execution.AdmitCommand) { c.WorkItemID = m.NewProject(executiontest.ScopeA2).Item },
		} {
			c := cmd
			mutate(&c)
			caller := engineer
			if c.Scope == executiontest.ScopeB {
				caller = execution.Caller{PrincipalID: executiontest.OtherEng}
			}
			// The definition lives in project A: another project cannot see it.
			if _, err := m.Svc.Admit(ctx, caller, c); !errors.Is(err, execution.ErrNotFound) && !errors.Is(err, execution.ErrHidden) {
				t.Errorf("%s: %v", name, err)
			}
		}
		noRuns("after scope mismatches")
		if _, err := m.Svc.Admit(ctx, engineer, execution.AdmitCommand{Scope: scopeA, WorkItemID: "bad id", DefinitionID: def.ID, ContentRevision: 1, SourceCommit: executiontest.CommitA}); !errors.Is(err, execution.ErrInvalid) {
			t.Errorf("malformed identifier: %v", err)
		}
		if _, err := m.Svc.Admit(ctx, engineer, execution.AdmitCommand{Scope: scopeA, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: 1, SourceCommit: "main"}); !errors.Is(err, execution.ErrInvalid) {
			t.Errorf("a branch name as the source commit: %v", err)
		}
		// A cancelled item and an epic admit nothing.
		cancelled := m.NewProject(scopeA)
		m.CancelWork(scopeA, cancelled.Item)
		if _, err := m.Admit(cancelled, def, good); !errors.Is(err, execution.ErrWorkCancelled) {
			t.Errorf("cancelled work item: %v", err)
		}
		epic := m.NewProject(scopeA)
		if err := m.World.MakeEpic(ctx, scopeA, epic.Item); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Admit(epic, def, good); !errors.Is(err, execution.ErrNotActionable) {
			t.Errorf("epic: %v", err)
		}
		noRuns("after refused work items")
	})

	t.Run("a specification edit during admission is a conflict, never a stale pin", func(t *testing.T) {
		m, p := fixture(t)
		def := define(m, job("a", readRun))
		revision := m.ContentRevision(p)
		old := m.MustAdmit(p, def, nil)
		plan, err := m.Svc.Prepare(ctx, engineer, execution.AdmitCommand{Scope: scopeA, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: revision, SourceCommit: executiontest.CommitA})
		if err != nil {
			t.Fatal(err)
		}
		m.EditSpec(scopeA, p.Item, "# Plan\nChanged while the run was being admitted.")
		if _, _, err := m.Store.Admit(ctx, plan); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("racing admission: %v", err)
		}
		if runs, _ := m.Store.List(ctx, scopeA, execution.ListQuery{Limit: 10}); len(runs) != 1 {
			t.Fatalf("a conflicting admission created a run (%d runs)", len(runs))
		}
		if _, err := m.Svc.Admit(ctx, engineer, execution.AdmitCommand{Scope: scopeA, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: revision, SourceCommit: executiontest.CommitA}); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("old revision: %v", err)
		}
		if got := m.Get(old).Run; got.Specification.Spec != "# Plan\nDo the thing." || got.ContentRevision != revision {
			t.Fatalf("pinned specification changed: %q at %d", got.Specification.Spec, got.ContentRevision)
		}
		fresh := m.MustAdmit(p, def, nil)
		if fresh.ContentRevision != revision+1 || fresh.Specification.Spec == old.Specification.Spec {
			t.Fatalf("the new run did not pin the new specification (%d)", fresh.ContentRevision)
		}
		if old.SnapshotDigest == fresh.SnapshotDigest {
			t.Fatal("different pins share a snapshot digest")
		}
	})

	t.Run("the definition is pinned by ID and digest and never rewritten", func(t *testing.T) {
		m, p := fixture(t)
		first := define(m, job("a", readRun))
		run := m.MustAdmit(p, first, nil)
		if run.DefinitionID != first.ID || run.DefinitionDigest != string(first.Digest) {
			t.Fatalf("pinned %s/%s, want %s/%s", run.DefinitionID, run.DefinitionDigest, first.ID, first.Digest)
		}
		// A changed definition is another immutable definition: the old run keeps
		// its own steps and digest, and re-admitting the same definition repeats
		// the same pin of the definition.
		second := define(m, job("a", readRun), job("b", "needs: [a]", readRun))
		if second.ID == first.ID || second.Digest == first.Digest {
			t.Fatal("a changed definition reused an identity")
		}
		other := m.MustAdmit(p, second, nil)
		got := m.Get(run)
		if got.Run.DefinitionDigest != string(first.Digest) || len(got.Steps) != 1 || len(m.Get(other).Steps) != 2 {
			t.Fatalf("a later definition rewrote the run: %s", got)
		}
		if got.Run.SnapshotDigest == other.SnapshotDigest {
			t.Fatal("different definitions share a snapshot digest")
		}
		// Definitions are scoped: another project's identifier is not found.
		p2 := m.NewProject(executiontest.ScopeA2)
		if _, err := m.Admit(p2, first, nil); !errors.Is(err, execution.ErrNotFound) {
			t.Fatalf("a definition of another project: %v", err)
		}
	})

	t.Run("the retained source reference gates dispatch and recovers from an outage", func(t *testing.T) {
		m, p := fixture(t)
		m.World.SetRetainErr(errors.New("git down"))
		run, err := m.Admit(p, define(m, job("a", readRun)), nil)
		if !errors.Is(err, execution.ErrUnavailable) || run == nil || run.SourceRetention != execution.RetentionPending {
			t.Fatalf("admission during a source outage: %+v %v", run, err)
		}
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("dispatched before the source was retained: %v", err)
		}
		if rep, err := m.Svc.Reconcile(ctx); err == nil || rep.Retained != 0 || m.Get(run).Run.SourceRetention != execution.RetentionPending {
			t.Fatalf("reconcile during the outage: %+v %v", rep, err)
		}
		m.World.SetRetainErr(nil)
		rep, err := m.Svc.Reconcile(ctx)
		if err != nil || rep.Retained != 1 {
			t.Fatalf("reconcile after the outage: %+v %v", rep, err)
		}
		got := m.Get(run).Run
		pins, _ := m.World.PinsOf(ctx, scopeA, string(p.Workspace))
		if got.SourceRetention != execution.RetentionConfirmed || len(pins) != 1 || pins[0].ID != got.RetainedRefID || !pins[0].Confirmed || pins[0].Commit != executiontest.CommitA {
			t.Fatalf("retention %s ref %q pins %+v", got.SourceRetention, got.RetainedRefID, pins)
		}
		if _, err := m.Svc.ConfirmSource(ctx, scopeA, run.ID); err != nil {
			t.Fatal(err)
		}
		if pins, _ := m.World.PinsOf(ctx, scopeA, string(p.Workspace)); len(pins) != 1 {
			t.Fatalf("repeating the confirmation created %d references", len(pins))
		}
		if in := m.MustClaim(tenant); in.RunID != run.ID {
			t.Fatalf("claimed %s", in.RunID)
		}
	})

	t.Run("secret pins: exact versions, per-step bindings, rotation and revocation races", func(t *testing.T) {
		m, p := fixture(t)
		ref := m.World.CreateSecret(scopeA, "token")
		def := define(m,
			job("one", readRun, secretEnv),
			job("two", readRun, secretEnv, `secretFiles: {cert: "`+ref+`"}`),
		)
		run := m.MustAdmit(p, def, nil)
		// The same binding name in two steps is not flattened into one namespace.
		var env1, env2, files int
		for _, s := range run.Snapshot.Secrets {
			switch {
			case s.Binding.Name == "TOKEN" && s.Step == "one":
				env1++
			case s.Binding.Name == "TOKEN" && s.Step == "two":
				env2++
			case s.Binding.Name == "cert" && s.Step == "two" && s.Binding.Delivery == runtimev1.DeliveryFile:
				files++
			}
			if s.Binding.Version != 1 || s.Binding.Ref != ref {
				t.Fatalf("pinned %+v", s.Binding)
			}
		}
		if env1 != 1 || env2 != 1 || files != 1 || len(run.Snapshot.Secrets) != 3 {
			t.Fatalf("bindings: %+v", run.Snapshot.Secrets)
		}
		if pins, err := m.World.LivePins(ctx, scopeA, run.ID); err != nil || len(pins) != 1 {
			t.Fatalf("pins %v %v, want one per distinct version", pins, err)
		}
		// Rotation never changes a pinned run or its attempts; a new run pins the new version.
		if v := m.World.RotateSecret(scopeA, "token"); v != 2 {
			t.Fatalf("rotated to %d", v)
		}
		if r2 := m.MustAdmit(p, def, nil); r2.Snapshot.Secrets[0].Binding.Version != 2 {
			t.Fatalf("new run pinned %d", r2.Snapshot.Secrets[0].Binding.Version)
		}
		if got := m.Get(run).Run; got.Snapshot.Secrets[0].Binding.Version != 1 {
			t.Fatal("rotation rewrote a pinned run")
		}
		in := m.MustClaim(tenant)
		if in.RunID != run.ID {
			t.Fatalf("claimed %s", in.RunID)
		}
		if c := contextOf(t, in); len(c.Secrets) != 1 || c.Secrets[0].Version != 1 {
			t.Fatalf("the attempt's secret bindings %+v", c.Secrets)
		}
		// Revocation committed between resolution and admission is refused and
		// registers no pin; after it, resolution refuses too.
		plan, err := m.Svc.Prepare(ctx, engineer, execution.AdmitCommand{Scope: scopeA, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: m.ContentRevision(p), SourceCommit: executiontest.CommitA})
		if err != nil {
			t.Fatal(err)
		}
		before, _ := m.Store.List(ctx, scopeA, execution.ListQuery{Limit: 20})
		m.World.RevokeSecretVersion(scopeA, "token", 2)
		if _, _, err := m.Store.Admit(ctx, plan); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("admission racing a revocation: %v", err)
		}
		if after, _ := m.Store.List(ctx, scopeA, execution.ListQuery{Limit: 20}); len(after) != len(before) {
			t.Fatal("a refused admission left a run")
		}
		if pins, _ := m.World.LivePins(ctx, scopeA, plan.Aggregate.Run.ID); len(pins) != 0 {
			t.Fatalf("a refused admission left pins: %v", pins)
		}
		if _, err := m.Admit(p, def, nil); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("admission after revocation: %v", err)
		}
		// A run's secret pins end with the run.
		if _, err := m.Svc.Cancel(ctx, engineer, scopeA, run.ID); err != nil {
			t.Fatal(err)
		}
		if pins, _ := m.World.LivePins(ctx, scopeA, run.ID); len(pins) != 0 {
			t.Fatalf("a cancelled run kept its pins: %v", pins)
		}
	})

	t.Run("pinned columns are written once and a terminal run is immutable", func(t *testing.T) {
		m, p := fixture(t)
		run := m.MustAdmit(p, define(m, job("a", readRun)), nil)
		// A mutation that tampers with the pins while making a legitimate change
		// persists only the legitimate change.
		_, _, err := m.Store.Update(ctx, scopeA, run.ID, execution.UpdateOptions{}, func(ctx context.Context, env execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
			agg.Run.DefinitionDigest = "sha256:tampered"
			agg.Run.SnapshotDigest = "sha256:tampered"
			agg.Run.SourceCommit = executiontest.CommitC
			agg.Run.Specification = &execution.Specification{Spec: "tampered"}
			agg.Run.Snapshot.Inputs = append(agg.Run.Snapshot.Inputs, execution.PinnedInput{Name: "injected"})
			agg.Steps[0].Spec.Image = "registry.invalid/tampered"
			agg.RequestCancel(execution.ReasonCancelled, env.Now())
			return execution.Effects{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		got := m.Get(run)
		if got.Run.DefinitionDigest != run.DefinitionDigest || got.Run.SnapshotDigest != run.SnapshotDigest || got.Run.SourceCommit != executiontest.CommitA ||
			got.Run.Specification.Spec != "# Plan\nDo the thing." || len(got.Run.Snapshot.Inputs) != len(run.Snapshot.Inputs) {
			t.Fatalf("a mutation rewrote the pins: %+v", got.Run)
		}
		if got.Run.CancelRequestedAt == nil || got.Run.Version != run.Version+1 {
			t.Fatalf("the legitimate change was lost: %+v", got.Run)
		}
	})

	t.Run("admission binds the run to the runtime that meets what its steps require", func(t *testing.T) {
		m, p := fixture(t)
		def := define(m, job("a", readRun))
		m.Manual.SetCapabilities(execution.Capabilities{Backend: runtimetest.ManualBackend})
		if _, err := m.Admit(p, def, nil); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("a runtime without the required guarantees: %v", err)
		}
		if runs, _ := m.Store.List(ctx, scopeA, execution.ListQuery{Limit: 10}); len(runs) != 0 {
			t.Fatal("a refused admission left a run")
		}
		m.Manual.SetCapabilities(execution.Capabilities{Backend: runtimetest.ManualBackend, Guarantees: execution.AllGuarantees})
		run := m.MustAdmit(p, def, nil)
		if b := run.Snapshot.Runtime; b == nil || b.Backend != runtimetest.ManualBackend || b.Requires == 0 {
			t.Fatalf("runtime binding %+v", b)
		}
		// Another backend, or a runtime that lost a guarantee, never claims it.
		m.Manual.SetCapabilities(execution.Capabilities{Backend: "other", Guarantees: execution.AllGuarantees})
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("another backend claimed the run: %v", err)
		}
		m.Manual.SetCapabilities(execution.Capabilities{Backend: runtimetest.ManualBackend})
		if _, err := m.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a weaker runtime claimed the run: %v", err)
		}
		if attempts(m, run) != 0 || held(m) != 0 || len(m.Workloads.Starts()) != 0 {
			t.Fatal("a refused runtime left an attempt or started a workload")
		}
		m.Manual.SetCapabilities(execution.Capabilities{Backend: runtimetest.ManualBackend, Guarantees: execution.AllGuarantees})
		if in := m.MustClaim(tenant); in.RunID != run.ID {
			t.Fatalf("claimed %s", in.RunID)
		}
		// A run admitted with no runtime is unbound and never dispatched.
		bare, bp := fixture(t, executiontest.MemOptions{NoRuntime: true})
		unbound := bare.MustAdmit(bp, define(bare, job("a", readRun)), nil)
		if unbound.Snapshot.Runtime != nil {
			t.Fatalf("unbound run has a binding: %+v", unbound.Snapshot.Runtime)
		}
		if _, err := bare.Claim(tenant); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("an unbound run was claimed: %v", err)
		}
	})

	t.Run("cleanup admission fails closed without the deployment binding", func(t *testing.T) {
		m, _ := fixture(t)
		def := define(m, job("a", "workspace: {access: none}"))
		if _, err := m.Svc.AdmitCleanup(ctx, execution.CleanupCommand{Scope: scopeA, DeploymentID: "dep_1", DefinitionID: def.ID}); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("cleanup admission without the deployment binding: %v", err)
		}
	})
}
