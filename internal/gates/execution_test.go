package gates

import (
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// pipelineAgg builds, without any database, the run
//
//	test(read) -> implement(agent writer) -> review(gate) -> deploy
//
// with every ancestor of the gate already finished the way the test needs.
func pipelineAgg(t *testing.T, mutate func(a *execution.Aggregate)) *execution.Aggregate {
	t.Helper()
	run := &execution.Run{
		Scope: scopeA, ID: "run_1", Purpose: execution.PurposeWork, WorkItemID: "wi_1042", WorkspaceID: "ws_1", DefinitionID: "pd_01",
		DefinitionDigest: digA, ContentRevision: 2, SourceCommit: commitA, SourceRetention: execution.RetentionConfirmed,
		Status: execution.RunRunning, Version: 1,
		Snapshot: execution.Snapshot{Inputs: []execution.PinnedInput{{Name: "doc", Type: "artifact", Value: runtimev1.StringValue("art_input")}}},
	}
	steps := []pipeline.Step{
		{ID: "test", Kind: pipeline.KindContainer, Workspace: &pipeline.Workspace{Access: pipeline.AccessRead, From: pipeline.RunSource},
			Outputs: map[string]pipeline.Output{"report": {Type: pipeline.TypeArtifact}}},
		{ID: "implement", Kind: pipeline.KindAgent, Needs: []string{"test"}, Workspace: &pipeline.Workspace{Access: pipeline.AccessWrite, From: pipeline.RunSource}},
		{ID: "review", Kind: pipeline.KindGate, Needs: []string{"implement"}, Gate: &pipeline.GateSpec{Evidence: []string{"${{ steps.test.outputs.report }}", "${{ inputs.doc }}"}}},
		{ID: "deploy", Kind: pipeline.KindContainer, Needs: []string{"review"}},
	}
	a := &execution.Aggregate{Run: run}
	for i, s := range steps {
		a.Steps = append(a.Steps, execution.NewStepRun(run.ID, i, s, t0))
	}
	test, impl := a.Step("test"), a.Step("implement")
	test.Status, test.OutputCommit, test.Outputs = execution.StepSucceeded, "", map[string]runtimev1.Value{"report": runtimev1.StringValue("art_report")}
	impl.Status, impl.OutputCommit = execution.StepSucceeded, commitB
	if mutate != nil {
		mutate(a)
	}
	return a
}

func TestGateInputsResolveOnlyPinnedInputsAndSuccessfulAncestors(t *testing.T) {
	a := pipelineAgg(t, nil)
	in, err := a.GateInputs("review")
	if err != nil {
		t.Fatal(err)
	}
	if in.Candidate.Commit != commitB || in.Candidate.Producer != "implement" || in.Candidate.ProducerKind != pipeline.KindAgent {
		t.Fatalf("candidate %+v: the last successful writer's commit is the candidate", in.Candidate)
	}
	if in.ContentRevision != 2 || in.DefinitionID != "pd_01" || in.WorkItemID != "wi_1042" {
		t.Fatalf("pinned run facts %+v", in)
	}
	if len(in.Evidence) != 2 || in.Evidence[0].ArtifactID != "art_report" || in.Evidence[0].Producer != "test" || in.Evidence[1].ArtifactID != "art_input" || in.Evidence[1].Producer != "" {
		t.Fatalf("evidence %+v", in.Evidence)
	}

	// Without a writer the run's original commit is the candidate.
	a = pipelineAgg(t, func(a *execution.Aggregate) {
		a.Step("review").Spec.Needs = []string{"test"}
		a.Step("implement").Status, a.Step("implement").OutputCommit = execution.StepSucceeded, commitB
	})
	if in, err = a.GateInputs("review"); err != nil || in.Candidate.Commit != commitA || in.Candidate.Producer != "" {
		t.Fatalf("original commit candidate: %+v %v", in.Candidate, err)
	}

	// A producer that did not succeed yields no inputs.
	a = pipelineAgg(t, func(a *execution.Aggregate) { a.Step("test").Status = execution.StepRunning })
	if _, err := a.GateInputs("review"); err == nil {
		t.Fatalf("an unfinished ancestor produced gate inputs")
	}
	// A writer without a published commit is not a candidate.
	a = pipelineAgg(t, func(a *execution.Aggregate) { a.Step("implement").OutputCommit = "" })
	if _, err := a.GateInputs("review"); err == nil {
		t.Fatalf("a writer without a commit produced gate inputs")
	}
	// A reference to a step that is not an ancestor is refused.
	a = pipelineAgg(t, func(a *execution.Aggregate) {
		a.Step("review").Spec.Gate.Evidence = []string{"${{ steps.deploy.outputs.report }}"}
		a.Step("deploy").Status, a.Step("deploy").Outputs = execution.StepSucceeded, map[string]runtimev1.Value{"report": runtimev1.StringValue("art_x")}
	})
	if _, err := a.GateInputs("review"); err == nil {
		t.Fatalf("a non-ancestor reference resolved")
	}
	// Unordered writers are an ambiguous source binding.
	a = pipelineAgg(t, func(a *execution.Aggregate) {
		a.Step("review").Spec.Needs = []string{"implement", "other"}
		a.Steps = append(a.Steps, execution.NewStepRun(a.Run.ID, 9, pipeline.Step{ID: "other", Kind: pipeline.KindContainer, Workspace: &pipeline.Workspace{Access: pipeline.AccessWrite, From: pipeline.RunSource}}, t0))
		a.Step("other").Status, a.Step("other").OutputCommit = execution.StepSucceeded, commitA
	})
	if _, err := a.GateInputs("review"); err == nil {
		t.Fatalf("unordered writers produced a candidate")
	}
}

func TestGateStepLifecycleCreatesNoAttempt(t *testing.T) {
	a := pipelineAgg(t, nil)
	if got := a.ReadyGates(); len(got) != 1 || got[0].ID != "review" {
		t.Fatalf("ready gates %v", got)
	}
	if a.Runnable(a.Step("review"), t0, execution.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate}) {
		t.Fatalf("a gate step is runnable as a Job")
	}
	if _, err := a.BindGate("review", "gate_1", t0); err != nil {
		t.Fatal(err)
	}
	if changed, err := a.BindGate("review", "gate_1", t0); err != nil || changed {
		t.Fatalf("rebinding the same gate: %v %v", changed, err)
	}
	if _, err := a.BindGate("review", "gate_2", t0); !errors.Is(err, execution.ErrInvalidTransition) {
		t.Fatalf("a second gate bound: %v", err)
	}
	s := a.Step("review")
	if s.Status != execution.StepPending || s.AttemptsStarted != 0 || len(a.Attempts) != 0 || len(a.Created) != 0 {
		t.Fatalf("a bound gate must stay pending with no attempt: %+v", s)
	}
	if err := a.CompleteGate("review", t0); err != nil {
		t.Fatal(err)
	}
	if s.Status != execution.StepSucceeded || s.AttemptsStarted != 0 || len(a.Attempts) != 0 {
		t.Fatalf("explicit pending-to-succeeded transition made an attempt: %+v", s)
	}
	// Terminal: never reopened, rejected or completed again.
	for name, err := range map[string]error{"complete again": a.CompleteGate("review", t0), "reject": a.RejectGate("review", t0)} {
		if !errors.Is(err, execution.ErrInvalidTransition) {
			t.Errorf("%s on a terminal gate step: %v", name, err)
		}
	}
	if s.Status != execution.StepSucceeded {
		t.Fatalf("terminal result changed to %s", s.Status)
	}
}

func TestGateTransitionDenials(t *testing.T) {
	t.Run("an unbound gate cannot complete", func(t *testing.T) {
		a := pipelineAgg(t, nil)
		if err := a.CompleteGate("review", t0); !errors.Is(err, execution.ErrInvalidTransition) {
			t.Fatalf("completed without a gate record: %v", err)
		}
	})
	t.Run("only a ready gate can be bound", func(t *testing.T) {
		a := pipelineAgg(t, func(a *execution.Aggregate) { a.Step("implement").Status = execution.StepRunning })
		if _, err := a.BindGate("review", "gate_1", t0); !errors.Is(err, execution.ErrStale) {
			t.Fatalf("bound a gate whose dependencies are unfinished: %v", err)
		}
	})
	t.Run("cancellation wins over a late approval", func(t *testing.T) {
		a := pipelineAgg(t, nil)
		if _, err := a.BindGate("review", "gate_1", t0); err != nil {
			t.Fatal(err)
		}
		a.RequestCancel(execution.ReasonCancelled, t0)
		if got := a.Step("review").Status; got != execution.StepCancelled {
			t.Fatalf("gate step %s after cancellation", got)
		}
		if err := a.CompleteGate("review", t0); !errors.Is(err, execution.ErrInvalidTransition) {
			t.Fatalf("a cancelled gate step completed: %v", err)
		}
		if got := a.Step("review").Status; got != execution.StepCancelled {
			t.Fatalf("terminal result changed to %s", got)
		}
	})
	t.Run("a draining run does not progress its gate", func(t *testing.T) {
		a := pipelineAgg(t, nil)
		a.Step("review").GateRef = "gate_1"
		a.Run.FailureIntentAt = &t0
		if err := a.CompleteGate("review", t0); err == nil {
			t.Fatalf("a draining run completed a gate")
		}
		if a.GateReady(a.Step("review")) {
			t.Fatalf("a draining run reports a ready gate")
		}
	})
	t.Run("a skipped ancestor skips the gate and its successors", func(t *testing.T) {
		a := pipelineAgg(t, func(a *execution.Aggregate) {
			a.Step("implement").Status, a.Step("implement").OutputCommit = execution.StepSkipped, ""
		})
		if a.GateReady(a.Step("review")) {
			t.Fatalf("a gate behind a skipped ancestor is ready")
		}
		a.Propagate(t0)
		if a.Step("review").Status != execution.StepSkipped || a.Step("deploy").Status != execution.StepSkipped {
			t.Fatalf("skipping did not propagate: %s %s", a.Step("review").Status, a.Step("deploy").Status)
		}
		if _, err := a.BindGate("review", "gate_1", t0); err == nil {
			t.Fatalf("bound a gate that was skipped")
		}
	})
	t.Run("rejection fails the run fast and drains successors", func(t *testing.T) {
		a := pipelineAgg(t, nil)
		a.Step("review").GateRef = "gate_1"
		if err := a.RejectGate("review", t0); err != nil {
			t.Fatal(err)
		}
		if a.Step("review").Status != execution.StepFailed || a.Step("review").Reason != execution.ReasonGateRejected {
			t.Fatalf("rejected gate step: %+v", a.Step("review"))
		}
		if a.Step("deploy").Status != execution.StepCancelled || a.Run.FailureIntentAt == nil {
			t.Fatalf("successor %s, failure intent %v", a.Step("deploy").Status, a.Run.FailureIntentAt)
		}
		if a.Runnable(a.Step("deploy"), t0, execution.EnabledKinds{pipeline.KindContainer}) {
			t.Fatalf("a successor of a rejected gate is runnable")
		}
	})
	t.Run("a gate is never conditional", func(t *testing.T) {
		a := pipelineAgg(t, func(a *execution.Aggregate) {
			a.Step("review").Spec.When = &pipeline.Condition{Input: "nope", Equals: pipeline.Scalar{Type: pipeline.TypeBoolean, Bool: true}}
		})
		a.Propagate(t0)
		if a.Step("review").Status != execution.StepPending {
			t.Fatalf("a false author condition skipped a mandatory gate: %s", a.Step("review").Status)
		}
	})
}

func TestProtectionRequiresAGateAfterEveryWriter(t *testing.T) {
	a := pipelineAgg(t, nil)
	p, err := a.Protection("deploy")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Gates) != 1 || p.Gates[0].StepID != "review" || p.Gates[0].Producer != "implement" || p.LastWriter != "implement" || !p.Covered() {
		t.Fatalf("protection %+v", p)
	}
	// A writer behind the gate means the approval covers the earlier commit
	// only: the steps after it need a new gate.
	a = pipelineAgg(t, func(a *execution.Aggregate) {
		a.Steps = append(a.Steps,
			execution.NewStepRun(a.Run.ID, 5, pipeline.Step{ID: "polish", Kind: pipeline.KindContainer, Needs: []string{"review"}, Workspace: &pipeline.Workspace{Access: pipeline.AccessWrite, From: "implement"}}, t0),
			execution.NewStepRun(a.Run.ID, 6, pipeline.Step{ID: "ship", Kind: pipeline.KindContainer, Needs: []string{"polish"}}, t0))
	})
	if p, err = a.Protection("ship"); err != nil || p.Covered() || p.LastWriter != "polish" {
		t.Fatalf("a write after the approval left the successor covered: %+v %v", p, err)
	}
	if p, err = a.Protection("polish"); err != nil || !p.Covered() {
		t.Fatalf("the writer itself is covered by the approval of its input: %+v %v", p, err)
	}
	// With a second gate after the writer the successor is covered again.
	a.Steps = append(a.Steps, execution.NewStepRun(a.Run.ID, 7, pipeline.Step{ID: "review2", Kind: pipeline.KindGate, Needs: []string{"polish"}, Gate: &pipeline.GateSpec{}}, t0))
	a.Step("ship").Spec.Needs = []string{"review2"}
	if p, err = a.Protection("ship"); err != nil || !p.Covered() || len(p.Gates) != 2 {
		t.Fatalf("a new gate after the writer did not cover the successor: %+v %v", p, err)
	}
	if p, err = a.Protection("test"); err != nil || len(p.Gates) != 0 || !p.Covered() {
		t.Fatalf("an unprotected step: %+v %v", p, err)
	}
}

// --- entity ---

func newGate(t *testing.T, conf *ConformanceSnapshot) *Gate {
	t.Helper()
	s, err := NewSubject(baseInput())
	if err != nil {
		t.Fatal(err)
	}
	g, err := Request(RequestInput{ID: "gate_1", Scope: scopeA, RunID: "run_1", StepID: "review", Subject: s,
		Required: Required{Evidence: []RequiredEvidence{{Role: RoleEvidence, ID: "art_tests", Digest: digA}}, Conformance: conf},
		Now:      t0, Deadline: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	g.MarkClean() // as loaded from storage: the next change bumps the version
	return g
}

func factsFor(g *Gate) Facts {
	id, _ := g.Subject.Deployment()
	f := Facts{
		Inputs: execution.GateInputs{Scope: g.Scope, RunID: g.RunID, StepID: g.StepID, WorkItemID: g.Subject.WorkItemID(), ContentRevision: g.Subject.ContentRevision(),
			DefinitionID: g.Subject.DefinitionID(), Candidate: execution.GateCandidate{Commit: g.Subject.CandidateCommit()}, DeploymentID: id},
		ContentRevision: g.Subject.ContentRevision(), StepWaiting: true, Evidence: map[domain.ArtifactID]EvidenceFacts{},
	}
	for _, e := range g.Subject.Evidence() {
		f.Evidence[e.ID] = EvidenceFacts{Found: true, Usable: true, Digest: e.Digest, Scope: g.Scope}
	}
	for _, r := range g.Required.Evidence {
		if r.Role == RoleEvidence {
			f.Inputs.Evidence = append(f.Inputs.Evidence, execution.GateEvidenceInput{ArtifactID: r.ID})
		}
	}
	return f
}

func decide(g *Gate, d Decision, version domain.Version, at time.Time) DecideCommand {
	return DecideCommand{Decision: d, Reason: "Reviewed the identified source and evidence.", Actor: Actor{Type: ActorUser, ID: "usr_reviewer"}, ExpectedVersion: version, SubjectDigest: g.SubjectDigest()}
}

func TestGateDecisionIsImmutableAndRejectionIsFinal(t *testing.T) {
	g := newGate(t, nil)
	if g.Status != StatusPending || g.Version != 1 {
		t.Fatalf("new gate %+v", g)
	}
	cmd := decide(g, DecisionReject, 1, t0)
	if err := g.Decide(cmd, factsFor(g), t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusRejected || g.Version != 2 || g.Decision == nil || g.Decision.By.ID != "usr_reviewer" || g.Decision.Reason == "" {
		t.Fatalf("rejected gate %+v %+v", g, g.Decision)
	}
	before := *g.Decision
	for _, v := range []domain.Version{1, 2} {
		err := g.Decide(decide(g, DecisionApprove, v, t0), factsFor(g), t0.Add(2*time.Minute))
		if err == nil {
			t.Fatalf("a rejected gate was approved at version %d", v)
		}
	}
	if g.Status != StatusRejected || *g.Decision != before {
		t.Fatalf("decision rewritten: %+v", g.Decision)
	}
	if err := g.Applicable(t0.Add(time.Minute)); !errors.Is(err, ErrStaleSubject) {
		t.Fatalf("a rejected gate is applicable: %v", err)
	}
}

func TestGateDecisionRefusals(t *testing.T) {
	now := t0.Add(time.Minute)
	for name, tc := range map[string]struct {
		mutate func(g *Gate, c *DecideCommand, f *Facts)
		want   error
	}{
		"a nonhuman actor": {func(g *Gate, c *DecideCommand, f *Facts) { c.Actor.Type = ActorService }, ErrDenied},
		"a workload":       {func(g *Gate, c *DecideCommand, f *Facts) { c.Actor.Type = ActorWorkload }, ErrDenied},
		"stale version":    {func(g *Gate, c *DecideCommand, f *Facts) { c.ExpectedVersion = 7 }, ErrVersionConflict},
		"wrong digest":     {func(g *Gate, c *DecideCommand, f *Facts) { c.SubjectDigest = digB }, ErrStaleSubject},
		"blank reason":     {func(g *Gate, c *DecideCommand, f *Facts) { c.Reason = "  " }, ErrInvalid},
		"unknown decision": {func(g *Gate, c *DecideCommand, f *Facts) { c.Decision = "maybe" }, ErrInvalid},
		"invalidated": {func(g *Gate, c *DecideCommand, f *Facts) {
			g.Invalidate(ReasonContentChanged, "", 3, t0)
			c.ExpectedVersion = g.Version
		}, ErrStaleSubject},
		"content changed": {func(g *Gate, c *DecideCommand, f *Facts) { f.ContentRevision = 3 }, ErrStaleSubject},
		"candidate moved": {func(g *Gate, c *DecideCommand, f *Facts) { f.Inputs.Candidate.Commit = commitB }, ErrStaleSubject},
		"definition":      {func(g *Gate, c *DecideCommand, f *Facts) { f.Inputs.DefinitionID = "pd_other" }, ErrStaleSubject},
		"evidence digest": {func(g *Gate, c *DecideCommand, f *Facts) {
			f.Evidence["art_tests"] = EvidenceFacts{Found: true, Usable: true, Digest: digB, Scope: g.Scope}
		}, ErrStaleSubject},
		"evidence removed": {func(g *Gate, c *DecideCommand, f *Facts) {
			f.Evidence["art_tests"] = EvidenceFacts{Found: true, Usable: false, Digest: digA, Scope: g.Scope}
		}, ErrStaleSubject},
		"evidence foreign": {func(g *Gate, c *DecideCommand, f *Facts) {
			f.Evidence["art_tests"] = EvidenceFacts{Found: true, Usable: true, Digest: digA, Scope: domain.Scope{TenantID: "t_x", ProjectID: "p_x"}}
		}, ErrStaleSubject},
		"step not waiting":    {func(g *Gate, c *DecideCommand, f *Facts) { f.StepWaiting = false }, ErrStaleSubject},
		"work cancelled":      {func(g *Gate, c *DecideCommand, f *Facts) { f.Cancelled = true }, ErrStaleSubject},
		"deployment appeared": {func(g *Gate, c *DecideCommand, f *Facts) { f.Inputs.DeploymentID = "dep_1" }, ErrStaleSubject},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGate(t, nil)
			c, f := decide(g, DecisionApprove, 1, now), factsFor(g)
			tc.mutate(g, &c, &f)
			ver, status := g.Version, g.Status
			if err := g.Decide(c, f, now); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if g.Status != status || g.Version != ver || g.Decision != nil {
				t.Fatalf("a refusal changed the gate: %+v", g)
			}
		})
	}
}

func TestGateExpiryBoundaries(t *testing.T) {
	g := newGate(t, nil)
	if g.Expired(g.Deadline.Add(-time.Nanosecond)) || !g.Expired(g.Deadline) || !g.Expired(g.Deadline.Add(time.Second)) {
		t.Fatalf("expiry is derived from the snapshotted deadline: not before it, always at and after it")
	}
	cmd := decide(g, DecisionApprove, 1, t0)
	if err := g.Decide(cmd, factsFor(g), g.Deadline); !errors.Is(err, ErrStaleSubject) {
		t.Fatalf("a decision at the deadline: %v", err)
	}
	if g.Status != StatusPending {
		t.Fatalf("expiry changed the status to %s: expiry is derived, not a status", g.Status)
	}
	if err := g.Decide(cmd, factsFor(g), g.Deadline.Add(-time.Nanosecond)); err != nil {
		t.Fatalf("a decision just before the deadline: %v", err)
	}
	// An approval stops being applicable at the deadline, without any write.
	if err := g.Applicable(g.Deadline.Add(-time.Nanosecond)); err != nil {
		t.Fatalf("approval before the deadline: %v", err)
	}
	var se *StaleError
	if err := g.Applicable(g.Deadline); !errors.As(err, &se) || se.Reason != ReasonExpired || se.Definitive {
		t.Fatalf("approval at the deadline: %v", err)
	}
	if err := g.Revalidate(factsFor(g), g.Deadline); !errors.Is(err, ErrStaleSubject) {
		t.Fatalf("revalidation at the deadline: %v", err)
	}
	if _, err := Request(RequestInput{ID: "gate_2", Scope: scopeA, RunID: "run_1", StepID: "review", Subject: g.Subject, Required: g.Required, Now: t0, Deadline: t0}); err == nil {
		t.Fatalf("a gate whose deadline is not in the future was created")
	}
}

func TestInvalidationIsSeparateFromTheDecision(t *testing.T) {
	g := newGate(t, nil)
	if err := g.Decide(decide(g, DecisionApprove, 1, t0), factsFor(g), t0); err != nil {
		t.Fatal(err)
	}
	decided := *g.Decision
	if !g.Invalidate(ReasonCandidateChanged, commitB, 0, t0.Add(time.Minute)) || g.Invalidate(ReasonContentChanged, "", 9, t0.Add(2*time.Minute)) {
		t.Fatalf("invalidation is recorded once; the first reason wins")
	}
	if g.Invalidation.Reason != ReasonCandidateChanged || g.Invalidation.SupersedingCommit != commitB {
		t.Fatalf("invalidation %+v", g.Invalidation)
	}
	if g.Status != StatusApproved || *g.Decision != decided {
		t.Fatalf("invalidation rewrote the decision: %s %+v", g.Status, g.Decision)
	}
	if err := g.Revalidate(factsFor(g), t0.Add(time.Minute)); !errors.Is(err, ErrStaleSubject) {
		t.Fatalf("an invalidated approval is applicable: %v", err)
	}
}

func TestConformanceBlocksApprovalButNotRejection(t *testing.T) {
	blocked := &ConformanceSnapshot{Status: ConformanceBlocked, Code: "conformance_blocked", ProducerStep: "implement", RequiredChecks: []string{"module-boundaries"}}
	g := newGate(t, blocked)
	err := g.Decide(decide(g, DecisionApprove, 1, t0), factsFor(g), t0)
	var ce *ConformanceError
	if !errors.As(err, &ce) || ce.Code != "conformance_blocked" || !errors.Is(err, ErrConformance) {
		t.Fatalf("approving blocked evidence: %v", err)
	}
	if g.Status != StatusPending {
		t.Fatalf("status %s", g.Status)
	}
	if err := g.Decide(decide(g, DecisionReject, 1, t0), factsFor(g), t0); err != nil || g.Status != StatusRejected {
		t.Fatalf("rejecting blocked evidence: %v %s", err, g.Status)
	}
	ok, err := newConformingGate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ok.Decide(decide(ok, DecisionApprove, 1, t0), factsFor(ok), t0); err != nil || ok.Status != StatusApproved {
		t.Fatalf("approving verified evidence: %v", err)
	}
}

func TestRequiredEvidenceMustMatchTheSubject(t *testing.T) {
	s, _ := NewSubject(baseInput())
	bad := []Required{
		{Evidence: nil},
		{Evidence: []RequiredEvidence{{Role: RoleEvidence, ID: "art_tests", Digest: digB}}},
		{Evidence: []RequiredEvidence{{Role: RoleEvidence, ID: "art_tests", Digest: digA}, {Role: RoleSupport, ID: "art_x", Digest: digA}}},
		{Evidence: []RequiredEvidence{{Role: "other", ID: "art_tests", Digest: digA}}},
		{Evidence: []RequiredEvidence{{Role: RoleEvidence, ID: "art_tests", Digest: digA}}, Conformance: &ConformanceSnapshot{Status: ConformanceBlocked}},
	}
	for i, r := range bad {
		if _, err := Request(RequestInput{ID: "gate_1", Scope: scopeA, RunID: "run_1", StepID: "review", Subject: s, Required: r, Now: t0, Deadline: t0.Add(time.Hour)}); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestRestoreRejectsCorruptRows(t *testing.T) {
	g := newGate(t, nil)
	rec := Record{ID: g.ID, Scope: g.Scope, RunID: g.RunID, StepID: g.StepID, Subject: g.Subject.Canonical(), Required: g.Required, Status: StatusPending, Version: 1,
		RequestedAt: t0, Deadline: t0.Add(time.Hour), CreatedAt: t0, UpdatedAt: t0}
	if _, err := Restore(rec); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Record){
		"approved without a decision": func(r *Record) { r.Status = StatusApproved },
		"pending with a decision": func(r *Record) {
			r.Decision = &DecisionRecord{By: Actor{Type: ActorUser, ID: "u"}, Decision: DecisionApprove, Reason: "x", At: t0}
		},
		"decision contradicts status": func(r *Record) {
			r.Status = StatusApproved
			r.Decision = &DecisionRecord{By: Actor{Type: ActorUser, ID: "u"}, Decision: DecisionReject, Reason: "x", At: t0}
		},
		"foreign subject": func(r *Record) { r.Scope = domain.Scope{TenantID: "t_other", ProjectID: "p_inventory"} },
		"edited subject":  func(r *Record) { r.Subject = append([]byte(nil), r.Subject...); r.Subject[5] ^= 1 },
		"unknown status":  func(r *Record) { r.Status = "expired" },
	} {
		r := rec
		mutate(&r)
		if _, err := Restore(r); err == nil {
			t.Errorf("%s: restored", name)
		}
	}
}

const repoURL = "https://git.example.test/acme/app.git"

// withAttempts records the accepted attempts of the candidate's producers: the
// reader "test" (att_1, from commitA) and the writer "implement" (att_2, from
// commitA, published commitB).
func withAttempts(a *execution.Aggregate) {
	pub := runtimev1.GitCommit(commitB)
	a.Attempts = append(a.Attempts,
		&execution.Attempt{Scope: scopeA, ID: "att_1", RunID: "run_1", StepID: "test", Ordinal: 1, Status: execution.AttemptSucceeded, InputCommit: commitA,
			Result: &runtimev1.Result{Outcome: runtimev1.OutcomeCompleted}},
		&execution.Attempt{Scope: scopeA, ID: "att_2", RunID: "run_1", StepID: "implement", Ordinal: 1, Status: execution.AttemptSucceeded, InputCommit: commitA,
			Result: &runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, SourceCommit: &pub}})
}

func TestVerifyEvidenceRefusesWrongOwnershipProducerAndCommit(t *testing.T) {
	build := func(mutate func(a *execution.Aggregate)) (*execution.Aggregate, execution.GateInputs) {
		a := pipelineAgg(t, func(a *execution.Aggregate) {
			a.Step("test").Spec.Workspace = &pipeline.Workspace{Access: pipeline.AccessRead, From: pipeline.RunSource}
			withAttempts(a)
			if mutate != nil {
				mutate(a)
			}
		})
		in, err := a.GateInputs("review")
		if err != nil {
			t.Fatal(err)
		}
		return a, in
	}
	src := VerifiedSource{Repository: repoURL}
	good := EvidenceMeta{ID: "art_report", Scope: scopeA, RunID: "run_1", StepID: "test", Attempt: "att_1", Digest: digA, Usable: true, Repository: repoURL, Commit: commitB}
	with := func(f func(m *EvidenceMeta)) EvidenceMeta { m := good; f(&m); return m }
	// The writer's own artifacts record the commit it worked from.
	writer := with(func(m *EvidenceMeta) { m.StepID, m.Attempt, m.Commit = "implement", "att_2", commitA })
	for name, tc := range map[string]struct {
		meta     EvidenceMeta
		producer string
		mutate   func(a *execution.Aggregate)
		ok       bool
	}{
		"candidate commit": {good, "test", nil, true},
		// test read the original commit but the candidate is the writer's commit.
		"another source commit":           {with(func(m *EvidenceMeta) { m.Commit = commitA }), "test", nil, false},
		"no source provenance":            {with(func(m *EvidenceMeta) { m.Commit, m.Repository = "", "" }), "test", nil, false},
		"no commit":                       {with(func(m *EvidenceMeta) { m.Commit = "" }), "test", nil, false},
		"no repository":                   {with(func(m *EvidenceMeta) { m.Repository = "" }), "test", nil, false},
		"another repository":              {with(func(m *EvidenceMeta) { m.Repository = "https://git.example.test/acme/other.git" }), "test", nil, false},
		"foreign project":                 {with(func(m *EvidenceMeta) { m.Scope.ProjectID = "p_other" }), "test", nil, false},
		"foreign tenant":                  {with(func(m *EvidenceMeta) { m.Scope.TenantID = "t_other" }), "test", nil, false},
		"not usable":                      {with(func(m *EvidenceMeta) { m.Usable = false }), "test", nil, false},
		"wrong producer step":             {with(func(m *EvidenceMeta) { m.StepID = "implement" }), "test", nil, false},
		"another run":                     {with(func(m *EvidenceMeta) { m.RunID = "run_2" }), "test", nil, false},
		"missing digest":                  {with(func(m *EvidenceMeta) { m.Digest = "" }), "test", nil, false},
		"the writer's own artifact":       {writer, "implement", nil, true},
		"the writer under another commit": {with(func(m *EvidenceMeta) { m.StepID, m.Attempt, m.Commit = "implement", "att_2", commitC }), "implement", nil, false},
		"the writer's artifact of a failed attempt": {with(func(m *EvidenceMeta) { m.StepID, m.Attempt, m.Commit = "implement", "att_failed", commitA }), "implement", func(a *execution.Aggregate) {
			a.Attempts = append(a.Attempts, &execution.Attempt{Scope: scopeA, ID: "att_failed", RunID: "run_1", StepID: "implement", Ordinal: 0, Status: execution.AttemptFailed, InputCommit: commitA})
		}, false},
		"the writer's input is not the artifact's commit": {with(func(m *EvidenceMeta) { m.StepID, m.Attempt, m.Commit = "implement", "att_2", commitC }), "implement", func(a *execution.Aggregate) {
			for _, at := range a.Attempts {
				if at.ID == "att_2" {
					at.InputCommit = commitA
				}
			}
		}, false},
		"the writer published another commit": {writer, "implement", func(a *execution.Aggregate) {
			other := runtimev1.GitCommit(commitC)
			for _, at := range a.Attempts {
				if at.ID == "att_2" {
					at.Result.SourceCommit = &other
				}
			}
		}, false},
		"ambiguous successful attempts": {writer, "implement", func(a *execution.Aggregate) {
			a.Attempts = append(a.Attempts, &execution.Attempt{Scope: scopeA, ID: "att_3", RunID: "run_1", StepID: "implement", Ordinal: 2, Status: execution.AttemptSucceeded, InputCommit: commitA})
		}, false},
		"the reader's artifact of a failed attempt": {with(func(m *EvidenceMeta) { m.Attempt = "att_old" }), "test", nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			a, in := build(tc.mutate)
			err := verifyEvidence(a, in, src, tc.meta, tc.producer)
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrEvidence) {
				t.Fatalf("accepted or wrong class: %v", err)
			}
		})
	}
}

func TestVerifyDeploymentOwnershipAndCandidate(t *testing.T) {
	a := pipelineAgg(t, nil)
	in, err := a.GateInputs("review")
	if err != nil {
		t.Fatal(err)
	}
	in.DeploymentID = "dep_1"
	good := DeploymentSnapshot{ID: "dep_1", ConfigDigest: digA, Scope: scopeA, Candidate: in.Candidate.Commit}
	if err := verifyDeployment(good, scopeA, in); err != nil {
		t.Fatalf("the project's deployment of the candidate: %v", err)
	}
	for name, mutate := range map[string]func(s *DeploymentSnapshot){
		"another deployment":    func(s *DeploymentSnapshot) { s.ID = "dep_2" },
		"another project":       func(s *DeploymentSnapshot) { s.Scope.ProjectID = "p_other" },
		"another tenant":        func(s *DeploymentSnapshot) { s.Scope.TenantID = "t_other" },
		"another candidate":     func(s *DeploymentSnapshot) { s.Candidate = commitA },
		"no recorded candidate": func(s *DeploymentSnapshot) { s.Candidate = "" },
		"no recorded ownership": func(s *DeploymentSnapshot) { s.Scope = domain.Scope{} },
	} {
		s := good
		mutate(&s)
		if err := verifyDeployment(s, scopeA, in); !errors.Is(err, ErrEvidence) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

const commitC = "3333333333333333333333333333333333333333"

func TestDeploymentApplicabilityOwnership(t *testing.T) {
	in := baseInput()
	in.DeploymentID, in.DeploymentConfigDigest = "dep_1", digA
	s, err := NewSubject(in)
	if err != nil {
		t.Fatal(err)
	}
	g, err := Request(RequestInput{ID: "gate_1", Scope: scopeA, RunID: "run_1", StepID: "review", Subject: s,
		Required: Required{Evidence: []RequiredEvidence{{Role: RoleEvidence, ID: "art_tests", Digest: digA}}}, Now: t0, Deadline: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	good := DeploymentSnapshot{ID: "dep_1", ConfigDigest: digA, Scope: scopeA, Candidate: commitA}
	facts := func(d *DeploymentSnapshot) Facts {
		f := factsFor(g)
		f.Deployment = d
		return f
	}
	if err := g.CheckCurrent(facts(&good)); err != nil {
		t.Fatalf("the project's deployment of the candidate: %v", err)
	}
	for name, mutate := range map[string]func(s *DeploymentSnapshot){
		"another project":       func(s *DeploymentSnapshot) { s.Scope.ProjectID = "p_other" },
		"another tenant":        func(s *DeploymentSnapshot) { s.Scope.TenantID = "t_other" },
		"no recorded ownership": func(s *DeploymentSnapshot) { s.Scope = domain.Scope{} },
		"another candidate":     func(s *DeploymentSnapshot) { s.Candidate = commitB },
		"no recorded candidate": func(s *DeploymentSnapshot) { s.Candidate = "" },
		"another configuration": func(s *DeploymentSnapshot) { s.ConfigDigest = digB },
		"another deployment":    func(s *DeploymentSnapshot) { s.ID = "dep_2" },
	} {
		d := good
		mutate(&d)
		var se *StaleError
		if err := g.CheckCurrent(facts(&d)); !errors.As(err, &se) || se.Reason != ReasonDeploymentChanged || !se.Definitive {
			t.Errorf("%s: %v", name, err)
		}
	}
	var se *StaleError
	if err := g.CheckCurrent(facts(nil)); !errors.As(err, &se) || se.Reason != ReasonDeploymentChanged {
		t.Errorf("no current deployment: %v", err)
	}
}
