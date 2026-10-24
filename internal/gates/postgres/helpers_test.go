package postgres_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const readRun = "workspace: {access: read, from: run}"

// pendingGate runs test -> review(gate) -> deploy up to a requested gate.
func pendingGate(t *testing.T, e *gatestest.Env, scope domain.Scope) (executiontest.Project, *execution.Run, *gates.Gate) {
	t.Helper()
	p := e.X.NewProject(scope)
	def := e.Define(scope,
		pipelinetest.Container("test", readRun, "outputs: {report: {type: artifact, path: /kodama/outputs/report.txt}}"),
		gatestest.Gate("review", []string{"test"}, "${{ steps.test.outputs.report }}"),
		pipelinetest.Container("deploy", "needs: [review]", readRun))
	run := e.MustAdmit(p, def, nil)
	t.Cleanup(func() {
		_, _ = e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: gatestest.Engineer}, run.Scope, run.ID)
	})
	in := e.MustClaim(scope.TenantID)
	e.Finish(in, "", gatestest.Output{Name: "report", Data: []byte("all tests passed\n")})
	if rep, err := e.Gates.Reconcile(context.Background(), scope.TenantID); err != nil || rep.Requested != 1 {
		t.Fatalf("reconcile: %+v %v", rep, err)
	}
	g, err := e.Store.ByStep(context.Background(), scope, run.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	return p, run, g
}

func approveCmd(g *gates.Gate) gates.DecideCommand {
	return gates.DecideCommand{Decision: gates.DecisionApprove, Reason: "Reviewed the identified source and test evidence.", ExpectedVersion: g.Version, SubjectDigest: g.SubjectDigest()}
}

func count(e *gatestest.Env, q string, args ...any) int { return e.X.Count(q, args...) }

// mustRun admits a run whose test step finished, leaving a ready gate step
// that has no gate yet.
func mustRun(t *testing.T, e *gatestest.Env, scope domain.Scope) *execution.Run {
	t.Helper()
	p := e.X.NewProject(scope)
	def := e.Define(scope,
		pipelinetest.Container("test", readRun, "outputs: {report: {type: artifact, path: /kodama/outputs/report.txt}}"),
		gatestest.Gate("review", []string{"test"}, "${{ steps.test.outputs.report }}"),
		pipelinetest.Container("deploy", "needs: [review]", readRun))
	run := e.MustAdmit(p, def, nil)
	t.Cleanup(func() {
		_, _ = e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: gatestest.Engineer}, run.Scope, run.ID)
	})
	e.Finish(e.MustClaim(scope.TenantID), "", gatestest.Output{Name: "report", Data: []byte("ok")})
	return run
}

// finalGate runs test -> review(gate) with nothing behind the gate, up to a
// requested gate. Completing that gate step settles the run, so the transition
// emits a run event: the only execution event a gate completion can produce,
// and therefore the seam where an injected failure proves rollback.
func finalGate(t *testing.T, e *gatestest.Env, scope domain.Scope) (*execution.Run, *gates.Gate) {
	t.Helper()
	p := e.X.NewProject(scope)
	def := e.Define(scope,
		pipelinetest.Container("test", readRun, "outputs: {report: {type: artifact, path: /kodama/outputs/report.txt}}"),
		gatestest.Gate("review", []string{"test"}, "${{ steps.test.outputs.report }}"))
	run := e.MustAdmit(p, def, nil)
	t.Cleanup(func() {
		_, _ = e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: gatestest.Engineer}, run.Scope, run.ID)
	})
	e.Finish(e.MustClaim(scope.TenantID), "", gatestest.Output{Name: "report", Data: []byte("all tests passed\n")})
	if rep, err := e.Gates.Reconcile(context.Background(), scope.TenantID); err != nil || rep.Requested != 1 {
		t.Fatalf("reconcile: %+v %v", rep, err)
	}
	g, err := e.Store.ByStep(context.Background(), scope, run.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	return run, g
}
