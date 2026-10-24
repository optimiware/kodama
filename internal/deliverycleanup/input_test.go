package deliverycleanup_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// declared is the project cleanup definition of the T-092 convention: one
// unchanged definition that declares the deployment_id run input.
func declared() string {
	return pipelinetest.DocNamed("project-cleanup", "{deployment_id: {type: string, required: true}}",
		pipelinetest.Container("cleanup", "workspace: {access: none}"))
}

// useDefinition replaces the world's cleanup definition and its scheduler.
func (w *world) useDefinition(yaml string) {
	w.t.Helper()
	w.def = w.m.Define(scopeA, yaml).ID
	w.sched = w.scheduler(deliverycleanup.Policy{})
}

// inputsOf is what the cleanup step's runtime context carries: the run's pinned
// input values (execution.BuildContext reads exactly these).
func inputsOf(r *execution.Run) map[string]runtimev1.Value { return r.Snapshot.Values() }

func wantDeployment(t *testing.T, r *execution.Run, dep string) {
	t.Helper()
	want := map[string]runtimev1.Value{"deployment_id": runtimev1.StringValue(dep)}
	if got := inputsOf(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("run %s inputs %v, want exactly %v", r.ID, got, want)
	}
	if len(r.Snapshot.Inputs) != 1 || r.Snapshot.Inputs[0].Source != "run_input" {
		t.Fatalf("the input is not a pinned run input: %+v", r.Snapshot.Inputs)
	}
}

// recordingRuns records the commands the scheduler gives admission.
type recordingRuns struct {
	deliverycleanup.Runs
	mu   sync.Mutex
	cmds []execution.CleanupCommand
}

func (r *recordingRuns) AdmitCleanupRound(ctx context.Context, cmd execution.CleanupCommand, round int) (*execution.Run, bool, error) {
	r.mu.Lock()
	r.cmds = append(r.cmds, cmd)
	r.mu.Unlock()
	return r.Runs.AdmitCleanupRound(ctx, cmd, round)
}

// cleanup_deployment_input: the cleanup step is handed the recorded deployment
// the run was admitted for through the run's pinned inputs, never from caller
// text, and the handoff is the same in every round (DQ-09, DQ-10, PX-06, PX-07).
func TestCleanupDeploymentInput(t *testing.T) {
	ctx := context.Background()

	t.Run("the admitted run carries exactly its own deployment", func(t *testing.T) {
		w := expired(t, deliverycleanup.Policy{})
		w.useDefinition(declared())
		rec := &recordingRuns{Runs: w.m.Svc}
		s := w.scheduler(deliverycleanup.Policy{}, func(c *deliverycleanup.Config) { c.Runs = rec })
		wantOutcome(t, one(t, w.sweep(s)), deliverycleanup.OutcomeAdmitted)
		runs := w.cleanupRuns()
		if len(runs) != 1 {
			t.Fatalf("%d runs", len(runs))
		}
		wantDeployment(t, runs[0], preview)
		// The scheduler supplies no text of its own: the input is set by admission.
		if len(rec.cmds) != 1 || len(rec.cmds[0].Inputs) != 0 || rec.cmds[0].DeploymentID != preview {
			t.Fatalf("scheduler command %+v", rec.cmds)
		}
		// The stored run, read back, still carries it.
		a, err := w.m.Store.Get(ctx, scopeA, runs[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		wantDeployment(t, a.Run, preview)
	})

	t.Run("a definition that does not declare the input keeps working", func(t *testing.T) {
		w := expired(t, deliverycleanup.Policy{})
		wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeAdmitted)
		runs := w.cleanupRuns()
		if len(runs) != 1 || len(runs[0].Snapshot.Inputs) != 0 {
			t.Fatalf("an undeclared input was pinned: %+v", runs)
		}
	})

	t.Run("a project variable or a caller cannot stand in for the deployment", func(t *testing.T) {
		w := expired(t, deliverycleanup.Policy{})
		w.useDefinition(declared())
		w.m.World.SetVariable(scopeA, "deployment_id", runtimev1.StringValue("dep_variable"))
		wantOutcome(t, one(t, w.sweep(w.sched)), deliverycleanup.OutcomeAdmitted)
		wantDeployment(t, w.cleanupRuns()[0], preview)
	})

	t.Run("a caller-supplied or foreign value is refused", func(t *testing.T) {
		for name, def := range map[string]string{"declared": declared(), "undeclared": ""} {
			t.Run(name, func(t *testing.T) {
				w := expired(t, deliverycleanup.Policy{})
				if def != "" {
					w.useDefinition(def)
				}
				w.sweep(w.sched)
				cmd := func(dep string, v runtimev1.Value) execution.CleanupCommand {
					return execution.CleanupCommand{Scope: scopeA, DeploymentID: dep, DefinitionID: w.def,
						Inputs: map[string]runtimev1.Value{"deployment_id": v}}
				}
				for _, v := range []runtimev1.Value{runtimev1.StringValue("dep_other"), runtimev1.StringValue(""), runtimev1.IntValue(7)} {
					if _, err := w.m.Svc.AdmitCleanup(ctx, cmd(preview, v)); !errors.Is(err, execution.ErrAdmissionRefused) {
						t.Fatalf("a supplied %v was not refused: %v", v, err)
					}
				}
				// Admission of another round refuses it the same way, and creates nothing.
				if _, _, err := w.m.Svc.AdmitCleanupRound(ctx, cmd(preview, runtimev1.StringValue("dep_other")), 2); !errors.Is(err, execution.ErrAdmissionRefused) {
					t.Fatalf("round admission: %v", err)
				}
				if n := len(w.cleanupRuns()); n != 1 {
					t.Fatalf("%d runs after refused admissions", n)
				}
				// A foreign deployment named as the admitted one is not authorized.
				if _, err := w.m.Svc.AdmitCleanup(ctx, cmd("dep_foreign", runtimev1.StringValue("dep_foreign"))); !errors.Is(err, execution.ErrDenied) {
					t.Fatalf("a foreign deployment: %v", err)
				}
			})
		}
	})

	t.Run("a value equal to the admitted deployment is the same handoff", func(t *testing.T) {
		w := expired(t, deliverycleanup.Policy{})
		w.useDefinition(declared())
		w.sweep(w.sched)
		w.failKnown()
		run, created, err := w.m.Svc.AdmitCleanupRound(ctx, execution.CleanupCommand{Scope: scopeA, DeploymentID: preview, DefinitionID: w.def,
			Inputs: map[string]runtimev1.Value{"deployment_id": runtimev1.StringValue(preview)}}, 2)
		if err != nil || !created {
			t.Fatalf("%v %v", created, err)
		}
		wantDeployment(t, run, preview)
	})

	t.Run("the command is not modified and other inputs pass through", func(t *testing.T) {
		w := expired(t, deliverycleanup.Policy{})
		w.useDefinition(pipelinetest.DocNamed("c", "{deployment_id: {type: string, required: true}, note: {type: string}}",
			pipelinetest.Container("cleanup", "workspace: {access: none}")))
		w.sweep(w.sched)
		w.failKnown()
		in := map[string]runtimev1.Value{"note": runtimev1.StringValue("x")}
		run, created, err := w.m.Svc.AdmitCleanupRound(ctx, execution.CleanupCommand{Scope: scopeA, DeploymentID: preview, DefinitionID: w.def, Inputs: in}, 2)
		if err != nil || !created {
			t.Fatal(err)
		}
		if _, ok := in["deployment_id"]; ok || len(in) != 1 {
			t.Fatalf("the caller's map was modified: %v", in)
		}
		if got := inputsOf(run); len(got) != 2 || got["deployment_id"].String() != preview || got["note"].String() != "x" {
			t.Fatalf("inputs %v", got)
		}
	})

	t.Run("a declared input of another type is refused", func(t *testing.T) {
		w := expired(t, deliverycleanup.Policy{})
		w.useDefinition(pipelinetest.DocNamed("c", "{deployment_id: {type: integer}}",
			pipelinetest.Container("cleanup", "workspace: {access: none}")))
		it := one(t, w.sweep(w.sched))
		wantOutcome(t, it, deliverycleanup.OutcomeRefused)
		if len(w.cleanupRuns()) != 0 {
			t.Fatal("a run was created")
		}
	})

	t.Run("the value survives a retry round, a restart and a lost answer", func(t *testing.T) {
		p := deliverycleanup.Policy{RetryBackoff: time.Minute, MaxRounds: 3}
		w := expired(t, p)
		w.useDefinition(declared())
		w.sched = w.scheduler(p)
		w.sweep(w.sched)
		w.failKnown()
		w.rec.report(scopeA, preview, deliverycleanup.RecordFailed)
		w.m.Advance(2 * time.Minute)
		// A restarted scheduler admits the next round.
		restarted := w.scheduler(p)
		it := one(t, w.sweep(restarted))
		wantOutcome(t, it, deliverycleanup.OutcomeAdmitted)
		if it.Round != 2 {
			t.Fatalf("round %d", it.Round)
		}
		runs := w.cleanupRuns()
		if len(runs) != 2 {
			t.Fatalf("%d runs", len(runs))
		}
		for _, r := range runs {
			wantDeployment(t, r, preview)
		}
		// The explicit follow-up round carries it as well.
		w.failKnown()
		next, err := restarted.Reconcile(ctx, scopeA, preview)
		if err != nil || next.Round != 3 {
			t.Fatalf("reconcile %+v %v", next, err)
		}
		a, err := w.m.Store.Get(ctx, scopeA, next.RunID)
		if err != nil {
			t.Fatal(err)
		}
		wantDeployment(t, a.Run, preview)
		// A repeated claim of a stored round returns that run unchanged.
		again, created, err := w.m.Svc.AdmitCleanupRound(ctx, execution.CleanupCommand{Scope: scopeA, DeploymentID: preview, DefinitionID: w.def}, 2)
		if err != nil || created {
			t.Fatalf("repeat %v %v", created, err)
		}
		wantDeployment(t, again, preview)
	})
}

// cleanup_many_due_one_definition: several deployments of one project that
// become due together are each cleaned through the project's single unchanged
// cleanup definition, each with its own deployment as input; duplicate claims
// still collapse to one run per deployment and round (DQ-09, PX-06).
func TestCleanupManyDueOneDefinition(t *testing.T) {
	deps := []string{"dep_a", "dep_b", "dep_c", "dep_d"}
	newMany := func(t *testing.T) *world {
		w := newWorld(t, deliverycleanup.Policy{})
		w.useDefinition(declared())
		for _, d := range deps {
			w.rec.add(scopeA, d, time.Hour)
		}
		w.m.Advance(2 * time.Hour)
		return w
	}
	check := func(t *testing.T, w *world, def domain.DefinitionID) {
		t.Helper()
		runs := w.cleanupRuns()
		if len(runs) != len(deps) {
			t.Fatalf("%d runs for %d deployments", len(runs), len(deps))
		}
		seen := map[string]bool{}
		for _, r := range runs {
			if r.DefinitionID != def {
				t.Fatalf("run %s uses definition %s, want the one project definition %s", r.ID, r.DefinitionID, def)
			}
			if seen[r.DeploymentID] {
				t.Fatalf("two runs for %s", r.DeploymentID)
			}
			seen[r.DeploymentID] = true
			wantDeployment(t, r, r.DeploymentID)
			if r.ID != execution.CleanupRunID(scopeA, r.DeploymentID, 1) {
				t.Fatalf("run %s is not the derived identity", r.ID)
			}
		}
		for _, d := range deps {
			if !seen[d] {
				t.Fatalf("%s has no run", d)
			}
		}
	}

	t.Run("each due deployment is admitted against the one definition", func(t *testing.T) {
		w := newMany(t)
		def := w.def
		rep := w.sweep(w.sched)
		if len(rep.Items) != len(deps) || rep.Count(deliverycleanup.OutcomeAdmitted) != len(deps) {
			t.Fatalf("report %+v", rep.Items)
		}
		check(t, w, def)
	})

	t.Run("duplicate claims still collapse to one run per deployment", func(t *testing.T) {
		w := newMany(t)
		def := w.def
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			s := w.scheduler(deliverycleanup.Policy{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Sweep(context.Background(), scopeA); err != nil {
					t.Errorf("sweep: %v", err)
				}
			}()
		}
		wg.Wait()
		w.sweep(w.sched)
		check(t, w, def)
	})
}
