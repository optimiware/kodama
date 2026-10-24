package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const readRun = "workspace: {access: read, from: run}"

var reportOutput = "outputs: {report: {type: artifact, path: /kodama/outputs/report.txt}}"

// reviewed is test -> review(gate) -> deploy: the common shape.
func reviewed(t *testing.T, e *gatestest.Env) (executiontest.Project, *execution.Run) {
	t.Helper()
	p := e.X.NewProject(gatestest.ScopeA)
	def := e.Define(p.Scope,
		pipelinetest.Container("test", readRun, reportOutput),
		gatestest.Gate("review", []string{"test"}, "${{ steps.test.outputs.report }}"),
		pipelinetest.Container("deploy", "needs: [review]", readRun))
	run := e.MustAdmit(p, def, nil)
	cancelAtEnd(t, e, run)
	return p, run
}

// cancelAtEnd cancels the run when the (sub)test ends, so a test that shares
// an environment never lets a leftover run's steps be claimed by the next one.
func cancelAtEnd(t *testing.T, e *gatestest.Env, run *execution.Run) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: gatestest.Engineer, RequestID: "req_t034"}, run.Scope, run.ID)
	})
}

// finishTest claims and finishes the test step with a report artifact.
func finishTest(t *testing.T, e *gatestest.Env, p executiontest.Project) map[string]domain.ArtifactID {
	t.Helper()
	in := e.MustClaim(p.Scope.TenantID)
	if in.StepID != "test" {
		t.Fatalf("claimed %s, want test", in.StepID)
	}
	return e.Finish(in, "", gatestest.Output{Name: "report", Data: []byte("all tests passed\n")})
}

func reconcile(t *testing.T, e *gatestest.Env, tenant domain.TenantID) gates.Report {
	t.Helper()
	rep, err := e.Gates.Reconcile(context.Background(), tenant)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return rep
}

func gateByID(t *testing.T, e *gatestest.Env, g *gates.Gate) *gates.Gate {
	t.Helper()
	cur, err := e.Store.Get(context.Background(), g.Scope, g.ID)
	if err != nil {
		t.Fatalf("gate %s: %v", g.ID, err)
	}
	return cur
}

func gateOf(t *testing.T, e *gatestest.Env, run *execution.Run, step string) *gates.Gate {
	t.Helper()
	g, err := e.Store.ByStep(context.Background(), run.Scope, run.ID, step)
	if err != nil {
		t.Fatalf("gate of %s: %v", step, err)
	}
	return g
}

func approve(t *testing.T, e *gatestest.Env, g *gates.Gate, principal string) (*gates.Gate, error) {
	t.Helper()
	return e.Gates.Decide(context.Background(), gatestest.Caller(principal), g.Scope, g.ID, gates.DecideCommand{
		Decision: gates.DecisionApprove, Reason: "Reviewed the identified source and test evidence.", ExpectedVersion: g.Version, SubjectDigest: g.SubjectDigest(),
	})
}

func reject(t *testing.T, e *gatestest.Env, g *gates.Gate, principal string) (*gates.Gate, error) {
	t.Helper()
	return e.Gates.Decide(context.Background(), gatestest.Caller(principal), g.Scope, g.ID, gates.DecideCommand{
		Decision: gates.DecisionReject, Reason: "The evidence does not support the change.", ExpectedVersion: g.Version, SubjectDigest: g.SubjectDigest(),
	})
}

func stepStatus(t *testing.T, e *gatestest.Env, run *execution.Run, step string) execution.StepStatus {
	t.Helper()
	return e.Get(run).Step(step).Status
}

func count(e *gatestest.Env, q string, args ...any) int { return e.X.Count(q, args...) }

func outboxCount(e *gatestest.Env, typ string, gate gates.ID) int {
	return e.X.CountTenant(gatestest.ScopeA.TenantID, `SELECT count(*) FROM outbox WHERE event_type = $1 AND convert_from(payload, 'UTF8')::jsonb->'resource'->>'id' = $2`, typ, string(gate))
}

func auditCount(e *gatestest.Env, action string, gate gates.ID) int {
	return count(e, `SELECT count(*) FROM audit WHERE action = $1 AND resource_id = $2`, action, string(gate))
}

// keyedDecision runs one decision through the keyed (idempotent) transaction
// the HTTP handler uses.
type keyedResult struct {
	Replayed bool
	Body     []byte
	Gate     *gates.Gate
}

func decideKeyed(ctx context.Context, e *gatestest.Env, g *gates.Gate, principal, key, body string, cmd gates.DecideCommand) (keyedResult, error) {
	caller := gatestest.Caller(principal)
	plan, err := e.Gates.PrepareDecision(ctx, caller, g.Scope, g.ID, cmd)
	if err != nil {
		return keyedResult{}, err
	}
	sum := sha256.Sum256([]byte(body))
	id := httpapi.RequestIdentity{
		Principal: httpapi.Principal{ID: principal}, TenantID: g.Scope.TenantID, ProjectID: g.Scope.ProjectID, Method: "POST",
		Route: "/api/v1/tenants/" + string(g.Scope.TenantID) + "/projects/" + string(g.Scope.ProjectID) + "/gates/" + string(g.ID) + "/decisions",
		Key:   httpapi.ClientKey(key), Hash: httpapi.RequestHash(sum),
	}
	authorize := func(ctx context.Context, _ httpapi.Reference) error {
		return e.Gates.Authorize(ctx, caller, g.Scope, gates.PermDecide)
	}
	render := func(g *gates.Gate) (httpapi.Outcome, error) {
		raw, _ := json.Marshal(map[string]any{"id": g.ID, "status": g.Status, "version": g.Version})
		return httpapi.Outcome{Response: httpapi.StoredResponse{Status: 200, Body: raw}, Target: httpapi.Reference{Kind: "gate", ID: string(g.ID)}}, nil
	}
	resp, replayed, decided, err := e.Keyed.DecideKeyed(ctx, id, authorize, plan, render)
	if err != nil {
		return keyedResult{}, e.Gates.AfterRefusal(ctx, plan, err)
	}
	return keyedResult{Replayed: replayed, Body: resp.Body, Gate: decided}, nil
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}
