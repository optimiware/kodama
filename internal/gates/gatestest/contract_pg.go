package gatestest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest/gatescontract"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// PostgresHarness runs gatescontract.Contract over the real PostgreSQL gate
// store, the real identity authority and the real execution module. It is wired
// by internal/gates/integration behind layer.SkipService: the unit layer runs
// the memory side (gatestest/memory) and the test environment runs both
// (SD-A11).
type PostgresHarness struct {
	E *Env

	seq  int
	runs []*execution.Run
}

var _ gatescontract.Harness = (*PostgresHarness)(nil)

const (
	// Stranger has no binding in any fixture scope.
	Stranger = "u034_stranger"
	// ForeignReviewer is a reviewer of tenant B only.
	ForeignReviewer = "u034_reviewer_b"
)

// NewPostgresHarness wraps a gatestest environment, seeding the fixture users.
func NewPostgresHarness(e *Env) *PostgresHarness {
	e.GrantAll()
	return &PostgresHarness{E: e}
}

func (h *PostgresHarness) Service() *gates.Service { return h.E.Gates }
func (h *PostgresHarness) Store() gates.Store      { return h.E.Store }
func (h *PostgresHarness) Principals() gatescontract.Principals {
	return gatescontract.Principals{Reviewer: Reviewer, Engineer: Engineer, Stranger: Stranger, Foreign: ForeignReviewer}
}
func (h *PostgresHarness) Scope() domain.Scope   { return ScopeA }
func (h *PostgresHarness) Foreign() domain.Scope { return ScopeB }
func (h *PostgresHarness) TTL() time.Duration    { return h.E.ttl }

// NewCase runs test -> review(gate) -> deploy until the gate step is ready.
// Earlier cases of the harness are cancelled first, so a later case never
// claims a step an earlier, consumed gate released.
func (h *PostgresHarness) NewCase(t testing.TB, deployment bool) gatescontract.Case {
	t.Helper()
	e := h.E
	for _, r := range h.runs {
		_, _ = e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: Engineer, RequestID: "req_t034"}, r.Scope, r.ID)
	}
	h.runs = nil
	h.seq++
	p := e.X.NewProject(ScopeA)
	const readRun = "workspace: {access: read, from: run}"
	c := gatescontract.Case{Scope: ScopeA, Step: "review", Item: p.Item}
	var run *execution.Run
	if deployment {
		c.Deployment = fmt.Sprintf("dep_t034_contract_%d", h.seq)
		def := e.DefineDoc(ScopeA, pipelinetest.DocNamed(fmt.Sprintf("contract-deploying-%d", h.seq), "{}",
			pipelinetest.Container("test", readRun, "outputs: {deployment: {type: string}}"),
			DeploymentGate("review", []string{"test"}, nil, "${{ steps.test.outputs.deployment }}"),
			pipelinetest.Container("deploy", "needs: [review]", readRun)))
		e.Deployments.Set(ScopeA, c.Deployment, CommitA, contractDigest("config-1"))
		run = e.MustAdmit(p, def, nil)
		in := e.MustClaim(ScopeA.TenantID)
		h.mustTest(t, in)
		e.StartAndSucceed(in, map[string]runtimev1.Value{"deployment": runtimev1.StringValue(c.Deployment)})
	} else {
		def := e.Define(ScopeA,
			pipelinetest.Container("test", readRun, "outputs: {report: {type: artifact, path: /kodama/outputs/report.txt}}"),
			Gate("review", []string{"test"}, "${{ steps.test.outputs.report }}"),
			pipelinetest.Container("deploy", "needs: [review]", readRun))
		run = e.MustAdmit(p, def, nil)
		in := e.MustClaim(ScopeA.TenantID)
		h.mustTest(t, in)
		ids := e.Finish(in, "", Output{Name: "report", Data: []byte(fmt.Sprintf("all tests passed %d\n", h.seq))})
		c.Evidence = ids["report"]
	}
	h.runs = append(h.runs, run)
	c.Run = run.ID
	t.Cleanup(func() {
		_, _ = e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: Engineer, RequestID: "req_t034"}, run.Scope, run.ID)
	})
	return c
}

// NewAgentCase runs implement(agent writer) -> review(gate) -> deploy until the
// gate step is ready, pinning spec.Baseline into the agent attempt's context.
func (h *PostgresHarness) NewAgentCase(t testing.TB, spec gatescontract.AgentSpec) gatescontract.Case {
	t.Helper()
	e := h.E
	for _, r := range h.runs {
		_, _ = e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: Engineer, RequestID: "req_t034"}, r.Scope, r.ID)
	}
	h.runs = nil
	h.seq++
	e.SetBaseline(spec.Baseline)
	p := e.X.NewProject(ScopeA)
	var extra []string
	if spec.Declare {
		extra = append(extra, "outputs: {"+ReportOutput+": {type: artifact, path: /kodama/outputs/conformance.json}}")
	}
	def := e.Define(ScopeA, Agent("implement", extra...), Gate("review", []string{"implement"}),
		pipelinetest.Container("deploy", "needs: [review]", "workspace: {access: read, from: implement}"))
	run := e.MustAdmit(p, def, nil)
	e.X.Sources.History[gatescontract.AgentCommit] = true
	in := e.MustClaim(ScopeA.TenantID)
	h.mustImplement(t, in)
	var support []domain.ArtifactID
	for i := 0; i < spec.Supports; i++ {
		name := fmt.Sprintf("notes-%d-%d", h.seq, i)
		support = append(support, e.Artifact(in, name, []byte("review notes "+name), artifacts.RoleOutput))
	}
	var outs []Output
	if spec.Declare {
		var data []byte
		if spec.Report != nil {
			var base runtimev1.DesignBaseline
			if spec.Baseline != nil {
				base = *spec.Baseline
			}
			data = spec.Report(base, gatescontract.AgentCommit, support)
		}
		outs = append(outs, Output{Name: ReportOutput, Data: data})
	}
	ids := e.Finish(in, gatescontract.AgentCommit, outs...)
	h.runs = append(h.runs, run)
	t.Cleanup(func() {
		_, _ = e.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: Engineer, RequestID: "req_t034"}, run.Scope, run.ID)
	})
	return gatescontract.Case{Scope: ScopeA, Run: run.ID, Step: "review", Item: p.Item, Evidence: ids[ReportOutput]}
}

func (h *PostgresHarness) mustImplement(t testing.TB, in *execution.AttemptSpec) {
	t.Helper()
	if in.StepID != "implement" {
		t.Fatalf("claimed %s, want implement", in.StepID)
	}
}

// Pins counts the artifacts the gate pinned.
func (h *PostgresHarness) Pins(t testing.TB, _ gatescontract.Case, id gates.ID) int {
	t.Helper()
	var n int
	h.E.X.Scalar(&n, `SELECT count(*) FROM artifact_pins WHERE owner_kind = 'gate' AND owner_id = $1`, string(id))
	return n
}

func (h *PostgresHarness) mustTest(t testing.TB, in *execution.AttemptSpec) {
	t.Helper()
	if in.StepID != "test" {
		t.Fatalf("claimed %s, want test", in.StepID)
	}
}

func (h *PostgresHarness) Grant(t testing.TB, principal string) {
	t.Helper()
	h.E.X.Grant(ScopeA, principal, identity.RoleReviewer)
}

func (h *PostgresHarness) Revoke(t testing.TB, principal string) {
	t.Helper()
	h.E.X.Revoke(ScopeA, principal)
}

func (h *PostgresHarness) ChangeContent(t testing.TB, c gatescontract.Case) {
	t.Helper()
	h.seq++
	h.E.X.EditSpec(c.Scope, c.Item, fmt.Sprintf("# Plan\nA changed requirement %d.", h.seq))
}

func (h *PostgresHarness) ChangeEvidence(t testing.TB, c gatescontract.Case) {
	t.Helper()
	h.E.X.OwnerExec(`UPDATE artifacts SET disposition = 'removed', version = version + 1 WHERE id = $1`, string(c.Evidence))
}

func (h *PostgresHarness) ChangeDeployment(t testing.TB, c gatescontract.Case) {
	t.Helper()
	h.E.Deployments.Set(c.Scope, c.Deployment, CommitA, contractDigest("config-2"))
}

func (h *PostgresHarness) DeploymentOutage(_ testing.TB, on bool) {
	if on {
		h.E.Deployments.Fail(errors.New("deployment service down"))
		return
	}
	h.E.Deployments.Fail(nil)
}

func (h *PostgresHarness) Advance(_ testing.TB, d time.Duration) { h.E.X.Clock.Advance(d) }

func (h *PostgresHarness) StepStatus(t testing.TB, c gatescontract.Case) execution.StepStatus {
	t.Helper()
	agg, err := h.E.X.Store.Get(context.Background(), c.Scope, c.Run)
	must(t, err)
	return agg.Step(c.Step).Status
}

func (h *PostgresHarness) Attempts(t testing.TB, c gatescontract.Case) int {
	t.Helper()
	return h.E.X.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1 AND step_id = $2`, string(c.Run), c.Step)
}

func (h *PostgresHarness) Events(t testing.TB, c gatescontract.Case, id gates.ID) gatescontract.Events {
	t.Helper()
	count := func(typ string) int {
		return h.E.X.CountTenant(c.Scope.TenantID, `SELECT count(*) FROM outbox WHERE event_type = $1 AND convert_from(payload, 'UTF8')::jsonb->'resource'->>'id' = $2`, typ, string(id))
	}
	return gatescontract.Events{Requested: count("gate.requested"), Decided: count("gate.decided"), Invalidated: count("gate.invalidated")}
}

func contractDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}
