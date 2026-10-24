package deliverycleanup_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

func workDefinition() string {
	return pipelinetest.Doc(pipelinetest.Container("work", "workspace: {access: write, from: run}"))
}

func writerOf(w *world, p executiontest.Project) domain.AttemptID {
	w.t.Helper()
	ws, err := w.m.World.WorkspaceOf(context.Background(), p.Scope, string(p.Workspace))
	if err != nil {
		w.t.Fatal(err)
	}
	return ws.Writer
}

// cleanup_scope: cleanup is bound to one recorded deployment of one scope. It
// cannot be requested for a deployment the record does not authorize, for another
// tenant's or project's, for an already deleted one, through a workspace-reading
// definition or from the public work admission, and the binding that lets a run
// report back names only its own deployment (DQ-10, PX-A09).
func TestCleanupScope(t *testing.T) {
	ctx := context.Background()

	t.Run("only a recorded, requested, undeleted deployment of the scope is authorized", func(t *testing.T) {
		w := newWorld(t, deliverycleanup.Policy{})
		w.rec.add(scopeA, preview, time.Hour)
		cmd := func(scope domain.Scope, dep string) execution.CleanupCommand {
			return execution.CleanupCommand{Scope: scope, DeploymentID: dep, DefinitionID: w.def}
		}
		// Recorded but cleanup not requested.
		if _, _, err := w.m.Svc.AdmitCleanupRound(ctx, cmd(scopeA, preview), 1); !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("an unrequested cleanup: %v", err)
		}
		// Unknown to the record.
		if _, _, err := w.m.Svc.AdmitCleanupRound(ctx, cmd(scopeA, "dep_arbitrary"), 1); !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("an arbitrary deployment: %v", err)
		}
		// Requested for the scope, named from another tenant and project.
		w.m.Advance(2 * time.Hour)
		w.sweep(w.sched)
		for _, other := range []domain.Scope{scopeB, executiontest.ScopeA2} {
			if _, _, err := w.m.Svc.AdmitCleanupRound(ctx, cmd(other, preview), 1); err == nil {
				t.Fatalf("a deployment of %v was cleaned through another scope", other)
			}
		}
		if n := len(w.cleanupRuns()); n != 1 {
			t.Fatalf("%d runs", n)
		}
		// After deletion nothing is admitted for the next round, whatever asks.
		w.succeed()
		w.rec.report(scopeA, preview, deliverycleanup.RecordDone)
		if _, _, err := w.m.Svc.AdmitCleanupRound(ctx, cmd(scopeA, preview), 2); !errors.Is(err, execution.ErrDenied) {
			t.Fatalf("a deleted deployment: %v", err)
		}
	})

	t.Run("the scheduler acts only on records of the swept scope", func(t *testing.T) {
		w := newWorld(t, deliverycleanup.Policy{})
		w.rec.add(scopeA, preview, time.Hour)
		w.rec.add(scopeB, "dep_foreign", time.Hour)
		w.rec.foreign = true
		w.m.Advance(2 * time.Hour)
		it := one(t, w.sweep(w.sched))
		if it.Deployment != preview || it.Scope != scopeA {
			t.Fatalf("acted on %+v", it)
		}
		if got := w.rec.snapshot(scopeB, "dep_foreign"); got.RequestedAt != nil {
			t.Fatal("another tenant's deployment was requested")
		}
		for _, r := range w.cleanupRuns() {
			if r.Scope != scopeA || r.DeploymentID != preview {
				t.Fatalf("run %+v", r)
			}
		}
		// Scopes are swept independently; a scope without a definition is refused
		// visibly and admits nothing.
		w.rec.foreign = false
		tick := w.scheduler(deliverycleanup.Policy{}, func(c *deliverycleanup.Config) { c.Scopes = scopes{scopeA, scopeB} })
		rep, err := tick.Tick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Count(deliverycleanup.OutcomeRefused) != 1 {
			t.Fatalf("a scope without a cleanup definition: %+v", rep.Items)
		}
		if runs, _ := w.m.Mem.List(ctx, scopeB, execution.ListQuery{Limit: 10}); len(runs) != 0 {
			t.Fatal("a run was admitted for a scope without a definition")
		}
	})

	t.Run("a cleanup definition that uses a workspace is refused", func(t *testing.T) {
		w := newWorld(t, deliverycleanup.Policy{})
		reads := w.m.Define(scopeA, pipelinetest.Doc(pipelinetest.Container("read", "workspace: {access: read, from: run}"))).ID
		bad := w.scheduler(deliverycleanup.Policy{}, func(c *deliverycleanup.Config) { c.Definitions = definitions{scopeA: reads} })
		w.rec.add(scopeA, preview, time.Hour)
		w.m.Advance(2 * time.Hour)
		wantOutcome(t, one(t, w.sweep(bad)), deliverycleanup.OutcomeRefused)
		if len(w.cleanupRuns()) != 0 {
			t.Fatal("a workspace-reading cleanup was admitted")
		}
	})

	t.Run("public work admission cannot create a cleanup run", func(t *testing.T) {
		w := newWorld(t, deliverycleanup.Policy{})
		run, err := w.m.Admit(w.proj, w.m.Define(scopeA, workDefinition()), nil)
		if err != nil {
			t.Fatal(err)
		}
		if run.Purpose != execution.PurposeWork || run.DeploymentID != "" {
			t.Fatalf("run %+v", run)
		}
		if len(w.cleanupRuns()) != 0 {
			t.Fatal("work admission created a cleanup run")
		}
	})

	t.Run("a run's binding names its own deployment and nothing else", func(t *testing.T) {
		w := newWorld(t, deliverycleanup.Policy{})
		w.rec.add(scopeA, preview, time.Hour)
		w.m.Advance(2 * time.Hour)
		it := one(t, w.sweep(w.sched))
		work := w.m.MustAdmit(w.proj, w.m.Define(scopeA, workDefinition()), nil)

		bindings, err := execution.NewCleanupBindings(w.m.Mem)
		if err != nil {
			t.Fatal(err)
		}
		dep, found, err := bindings.Binding(ctx, scopeA, it.RunID)
		if err != nil || !found || dep != preview {
			t.Fatalf("binding %q %v %v", dep, found, err)
		}
		for name, c := range map[string]struct {
			scope domain.Scope
			run   domain.RunID
		}{
			"another tenant":  {scopeB, it.RunID},
			"another project": {executiontest.ScopeA2, it.RunID},
			"a work run":      {scopeA, work.ID},
			"an unknown run":  {scopeA, "run_unknown"},
			"an invalid run":  {scopeA, "bad id"},
		} {
			if dep, found, err := bindings.Binding(ctx, c.scope, c.run); err != nil || found || dep != "" {
				t.Fatalf("%s: binding %q %v %v", name, dep, found, err)
			}
		}
		if _, err := execution.NewCleanupBindings(nil); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("a nil store: %v", err)
		}
	})

	t.Run("round identities are scoped and distinct", func(t *testing.T) {
		a := execution.CleanupRunID(scopeA, preview, 1)
		for name, other := range map[string]domain.RunID{
			"round":      execution.CleanupRunID(scopeA, preview, 2),
			"deployment": execution.CleanupRunID(scopeA, "dep_other", 1),
			"tenant":     execution.CleanupRunID(scopeB, preview, 1),
			"project":    execution.CleanupRunID(executiontest.ScopeA2, preview, 1),
		} {
			if other == a {
				t.Fatalf("%s did not change the run identity", name)
			}
		}
		if a != execution.CleanupRunID(scopeA, preview, 1) || a.Validate() != nil {
			t.Fatal("the identity is not stable or not valid")
		}
		w := newWorld(t, deliverycleanup.Policy{})
		for _, round := range []int{0, execution.MaxCleanupRounds + 1} {
			if _, _, err := w.m.Svc.AdmitCleanupRound(ctx, execution.CleanupCommand{Scope: scopeA, DeploymentID: preview, DefinitionID: w.def}, round); !errors.Is(err, execution.ErrInvalid) {
				t.Fatalf("round %d: %v", round, err)
			}
		}
	})
}
