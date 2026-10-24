package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest/gatescontract"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Fixture identities.
var (
	ScopeA = domain.Scope{TenantID: "t_contract_a", ProjectID: "p_contract"}
	ScopeB = domain.Scope{TenantID: "t_contract_b", ProjectID: "p_contract"}
)

const (
	// Commit is the source commit every fixture run is pinned to and the
	// candidate its gate reviews.
	Commit = "1111111111111111111111111111111111111111"

	Reviewer = "u_contract_reviewer"
	Engineer = "u_contract_engineer"
	Stranger = "u_contract_stranger"
	Foreign  = "u_contract_foreign"
)

// Harness runs the shared gate contract over the memory adapters.
type Harness struct {
	T testing.TB
	E *Env
}

var _ gatescontract.Harness = (*Harness)(nil)

// NewHarness builds an isolated Env with the fixture principals.
func NewHarness(t testing.TB, o ...Options) *Harness {
	t.Helper()
	e := New(o...)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.Authority.Grant(ctx, ScopeA, Reviewer, RoleReviewer))
	must(e.Authority.Grant(ctx, ScopeA, Engineer, RoleEngineer))
	must(e.Authority.Grant(ctx, ScopeB, Foreign, RoleReviewer))
	return &Harness{T: t, E: e}
}

func (h *Harness) Service() *gates.Service { return h.E.Svc }
func (h *Harness) Store() gates.Store      { return h.E.Store }
func (h *Harness) Principals() gatescontract.Principals {
	return gatescontract.Principals{Reviewer: Reviewer, Engineer: Engineer, Stranger: Stranger, Foreign: Foreign}
}
func (h *Harness) Scope() domain.Scope   { return ScopeA }
func (h *Harness) Foreign() domain.Scope { return ScopeB }
func (h *Harness) TTL() time.Duration    { return h.E.ttl }

func (h *Harness) fatal(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// NewCase implements gatescontract.Harness: it seeds the work item, the report
// artifact, an optional deployment and a run
//
//	test (succeeded) -> review (gate, ready) -> deploy
//
// whose gate evidence is the test report.
func (h *Harness) NewCase(t testing.TB, deployment bool) gatescontract.Case {
	t.Helper()
	n := h.E.seq.Add(1)
	ctx := context.Background()
	scope := ScopeA
	c := gatescontract.Case{
		Scope: scope, Run: domain.RunID(fmt.Sprintf("run_c%d", n)), Step: "review", Item: domain.WorkItemID(fmt.Sprintf("wi_c%d", n)),
		Evidence: domain.ArtifactID(fmt.Sprintf("art_c%d", n)),
	}
	attempt := domain.AttemptID(fmt.Sprintf("att_c%d", n))
	data := []byte(fmt.Sprintf("all tests passed %d\n", n))
	gate := &pipeline.GateSpec{Evidence: []string{"${{ steps.test.outputs.report }}"}}
	outputs := map[string]runtimev1.Value{"report": runtimev1.StringValue(string(c.Evidence))}
	if deployment {
		c.Deployment = fmt.Sprintf("dep_c%d", n)
		gate.DeploymentID = "${{ steps.test.outputs.deployment }}"
		outputs["deployment"] = runtimev1.StringValue(c.Deployment)
	}
	now := h.E.Clock.Now()
	run := &execution.Run{
		Scope: scope, ID: c.Run, Purpose: execution.PurposeWork, WorkItemID: c.Item, WorkspaceID: "ws_contract", DefinitionID: "pd_contract",
		DefinitionDigest: "sha256:" + fmt.Sprintf("%064x", n), ContentRevision: 1, SourceCommit: Commit, SourceRetention: execution.RetentionConfirmed,
		Status: execution.RunRunning, Version: 1,
	}
	specs := []pipeline.Step{
		{ID: "test", Kind: pipeline.KindContainer, Workspace: &pipeline.Workspace{Access: pipeline.AccessRead, From: pipeline.RunSource},
			Outputs: map[string]pipeline.Output{"report": {Type: pipeline.TypeArtifact}, "deployment": {Type: pipeline.TypeString}}},
		{ID: "review", Kind: pipeline.KindGate, Needs: []string{"test"}, Gate: gate},
		{ID: "deploy", Kind: pipeline.KindContainer, Needs: []string{"review"}},
	}
	agg := &execution.Aggregate{Run: run}
	for i, s := range specs {
		agg.Steps = append(agg.Steps, execution.NewStepRun(run.ID, i, s, now))
	}
	test := agg.Step("test")
	test.Status, test.Outputs, test.AttemptsStarted = execution.StepSucceeded, outputs, 1
	agg.Attempts = append(agg.Attempts, &execution.Attempt{Scope: scope, ID: attempt, RunID: run.ID, StepID: "test", Ordinal: 1,
		Status: execution.AttemptSucceeded, InputCommit: Commit})
	run.MarkClean()
	for _, st := range agg.Steps {
		st.MarkClean()
	}

	h.E.Candidates.Allow(Commit)
	h.fatal(t, h.E.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if err := workTable.Put(tx, workRow{Scope: scope, ID: c.Item, Revision: 1}); err != nil {
			return err
		}
		if err := artTable.Put(tx, artifactRow{Data: data, Meta: gates.EvidenceMeta{
			ID: c.Evidence, Scope: scope, RunID: run.ID, StepID: "test", Attempt: attempt, Digest: digestOf(data), Usable: true, Size: int64(len(data)),
			Repository: Repository, Commit: Commit,
		}}); err != nil {
			return err
		}
		return runTable.Put(tx, runRow{agg: agg})
	}))
	if deployment {
		h.fatal(t, h.E.Deployments.Set(ctx, scope, c.Deployment, Commit, digestOf([]byte("config-1"))))
	}
	return c
}

// NewAgentCase implements gatescontract.Harness: it seeds
//
//	implement (agent writer, succeeded, published AgentCommit) -> review (gate, ready) -> deploy
//
// with the producing attempt's immutable pinned context carrying spec.Baseline,
// its accepted result naming the published commit and the declared report
// output, and the report and supporting artifacts it kept. The gate's own
// evidence references are empty: the report and its citations are bound by the
// conformance check.
func (h *Harness) NewAgentCase(t testing.TB, spec gatescontract.AgentSpec) gatescontract.Case {
	t.Helper()
	n := h.E.seq.Add(1)
	ctx := context.Background()
	scope := ScopeA
	c := gatescontract.Case{
		Scope: scope, Run: domain.RunID(fmt.Sprintf("run_a%d", n)), Step: "review", Item: domain.WorkItemID(fmt.Sprintf("wi_a%d", n)),
	}
	attempt := domain.AttemptID(fmt.Sprintf("att_a%d", n))
	now := h.E.Clock.Now()

	pctx := runtimev1.Context{TenantID: scope.TenantID, ProjectID: scope.ProjectID, RunID: c.Run, StepID: "implement", AttemptID: attempt, DesignBaseline: spec.Baseline}
	rawCtx, err := json.Marshal(pctx)
	h.fatal(t, err)
	sum := sha256.Sum256(rawCtx)

	meta := func(id domain.ArtifactID, data []byte) artifactRow {
		return artifactRow{Data: data, Meta: gates.EvidenceMeta{
			ID: id, Scope: scope, RunID: c.Run, StepID: "implement", Attempt: attempt, Digest: digestOf(data), Usable: true, Size: int64(len(data)),
			// The producing attempt's own source: the writer exception connects
			// it to the commit the attempt published.
			Repository: Repository, Commit: gatescontract.SourceCommit,
		}}
	}
	var rows []artifactRow
	var support []domain.ArtifactID
	for i := 0; i < spec.Supports; i++ {
		id := domain.ArtifactID(fmt.Sprintf("art_a%d_s%d", n, i))
		support = append(support, id)
		rows = append(rows, meta(id, []byte(fmt.Sprintf("review notes %d %d", n, i))))
	}
	outputs := map[string]pipeline.Output{}
	stepOutputs := map[string]runtimev1.Value{}
	if spec.Declare {
		var base runtimev1.DesignBaseline
		if spec.Baseline != nil {
			base = *spec.Baseline
		}
		var data []byte
		if spec.Report != nil {
			data = spec.Report(base, gatescontract.AgentCommit, support)
		}
		c.Evidence = domain.ArtifactID(fmt.Sprintf("art_a%d_report", n))
		rows = append(rows, meta(c.Evidence, data))
		outputs[ReportOutput] = pipeline.Output{Type: pipeline.TypeArtifact}
		stepOutputs[ReportOutput] = runtimev1.StringValue(string(c.Evidence))
	}

	run := &execution.Run{
		Scope: scope, ID: c.Run, Purpose: execution.PurposeWork, WorkItemID: c.Item, WorkspaceID: "ws_contract", DefinitionID: "pd_contract",
		DefinitionDigest: "sha256:" + fmt.Sprintf("%064x", n), ContentRevision: 1, SourceCommit: gatescontract.SourceCommit,
		SourceRetention: execution.RetentionConfirmed, Status: execution.RunRunning, Version: 1,
	}
	specs := []pipeline.Step{
		{ID: "implement", Kind: pipeline.KindAgent, Workspace: &pipeline.Workspace{Access: pipeline.AccessWrite, From: pipeline.RunSource}, Outputs: outputs},
		{ID: "review", Kind: pipeline.KindGate, Needs: []string{"implement"}, Gate: &pipeline.GateSpec{}},
		{ID: "deploy", Kind: pipeline.KindContainer, Needs: []string{"review"}},
	}
	agg := &execution.Aggregate{Run: run}
	for i, s := range specs {
		agg.Steps = append(agg.Steps, execution.NewStepRun(run.ID, i, s, now))
	}
	published := runtimev1.GitCommit(gatescontract.AgentCommit)
	impl := agg.Step("implement")
	impl.Status, impl.OutputCommit, impl.Outputs, impl.AttemptsStarted = execution.StepSucceeded, gatescontract.AgentCommit, stepOutputs, 1
	agg.Attempts = append(agg.Attempts, &execution.Attempt{
		Scope: scope, ID: attempt, RunID: run.ID, StepID: "implement", Ordinal: 1, Status: execution.AttemptSucceeded,
		InputCommit: gatescontract.SourceCommit, ContextDigest: "sha256:" + hex.EncodeToString(sum[:]), Context: rawCtx,
		Result: &runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, SourceCommit: &published, Outputs: stepOutputs},
	})
	run.MarkClean()
	for _, st := range agg.Steps {
		st.MarkClean()
	}

	h.E.Candidates.Allow(gatescontract.AgentCommit, gatescontract.SourceCommit)
	h.fatal(t, h.E.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if err := workTable.Put(tx, workRow{Scope: scope, ID: c.Item, Revision: 1}); err != nil {
			return err
		}
		for _, r := range rows {
			if err := artTable.Put(tx, r); err != nil {
				return err
			}
		}
		return runTable.Put(tx, runRow{agg: agg})
	}))
	return c
}

// Pins implements gatescontract.Harness.
func (h *Harness) Pins(t testing.TB, c gatescontract.Case, id gates.ID) int {
	t.Helper()
	n, err := h.E.PinCount(context.Background(), c.Scope, id)
	h.fatal(t, err)
	return n
}

// Grant implements gatescontract.Harness.
func (h *Harness) Grant(t testing.TB, principal string) {
	t.Helper()
	h.fatal(t, h.E.Authority.Grant(context.Background(), ScopeA, principal, RoleReviewer))
}

// Revoke implements gatescontract.Harness.
func (h *Harness) Revoke(t testing.TB, principal string) {
	t.Helper()
	h.fatal(t, h.E.Authority.Revoke(context.Background(), ScopeA, principal))
}

// ChangeContent implements gatescontract.Harness: a new content revision.
func (h *Harness) ChangeContent(t testing.TB, c gatescontract.Case) {
	t.Helper()
	h.fatal(t, h.E.DB.Update(context.Background(), c.Scope.TenantID, func(tx memdb.Tx) error {
		w, ok := workTable.Get(tx, projectKey(c.Scope, string(c.Item)))
		if !ok {
			return gates.ErrNotFound
		}
		w.Revision++
		return workTable.Put(tx, w)
	}))
}

// ChangeEvidence implements gatescontract.Harness: the artifact is removed.
func (h *Harness) ChangeEvidence(t testing.TB, c gatescontract.Case) {
	t.Helper()
	h.fatal(t, h.E.DB.Update(context.Background(), c.Scope.TenantID, func(tx memdb.Tx) error {
		a, ok := artTable.Get(tx, projectKey(c.Scope, string(c.Evidence)))
		if !ok {
			return gates.ErrNotFound
		}
		a.Removed = true
		return artTable.Put(tx, a)
	}))
}

// ChangeDeployment implements gatescontract.Harness.
func (h *Harness) ChangeDeployment(t testing.TB, c gatescontract.Case) {
	t.Helper()
	h.fatal(t, h.E.Deployments.Set(context.Background(), c.Scope, c.Deployment, Commit, digestOf([]byte("config-2"))))
}

// DeploymentOutage implements gatescontract.Harness.
func (h *Harness) DeploymentOutage(_ testing.TB, on bool) {
	if on {
		h.E.Deployments.Fail(fmt.Errorf("deployment service down"))
		return
	}
	h.E.Deployments.Fail(nil)
}

// Advance implements gatescontract.Harness.
func (h *Harness) Advance(_ testing.TB, d time.Duration) { h.E.Clock.Advance(d) }

func (h *Harness) aggregate(t testing.TB, c gatescontract.Case) *execution.Aggregate {
	t.Helper()
	agg, err := h.E.Runs.Get(context.Background(), c.Scope, c.Run)
	h.fatal(t, err)
	return agg
}

// StepStatus implements gatescontract.Harness.
func (h *Harness) StepStatus(t testing.TB, c gatescontract.Case) execution.StepStatus {
	t.Helper()
	return h.aggregate(t, c).Step(c.Step).Status
}

// Attempts implements gatescontract.Harness.
func (h *Harness) Attempts(t testing.TB, c gatescontract.Case) int {
	t.Helper()
	agg := h.aggregate(t, c)
	return agg.Step(c.Step).AttemptsStarted + len(agg.AttemptsOf(c.Step))
}

// Events implements gatescontract.Harness.
func (h *Harness) Events(t testing.TB, c gatescontract.Case, id gates.ID) gatescontract.Events {
	t.Helper()
	rows, err := h.E.Events(context.Background(), c.Scope.TenantID)
	h.fatal(t, err)
	var out gatescontract.Events
	for _, r := range rows {
		if r.Gate != id {
			continue
		}
		switch r.Type {
		case "gate.requested":
			out.Requested++
		case "gate.decided":
			out.Decided++
		case "gate.invalidated":
			out.Invalidated++
		}
	}
	return out
}
