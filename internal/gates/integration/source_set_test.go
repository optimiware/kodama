package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest"
	"github.com/optimiware/kodama/internal/identity"
)

const (
	sqlBase  = "9999999999999999999999999999999999999999"
	sqlMoved = "8888888888888888888888888888888888888888"
	sqlTwo   = "[{repository: backend, path: backend, access: write}, {repository: frontend, path: frontend, access: read}]"
)

// sourceSetGate is a work-item run of the internal Git service on real SQL whose
// gate step is ready. The PostgreSQL execution fixture has no real internally
// hosted workspace (its writer reservation is exercised in memory), so the run is
//
//	check (reader, succeeded) -> review (gate) -> integrate (writer)
//
// and the commit a writer before the gate would have published is recorded as the
// accepted result of the succeeded check attempt (the attempt_commits row the
// execution store writes for a result), so the store's read of the run's heads is
// exercised on real rows.
type sourceSetGate struct {
	E       *gatestest.Env
	SS      executiontest.SourceSubject
	Run     *execution.Run
	Backend execution.SourceRepository
	Front   execution.SourceRepository
	Task    string
	Commit  string
}

func newSourceSetGate(t *testing.T) *sourceSetGate {
	t.Helper()
	x, ss := executiontest.NewSourcePG(t)
	e := gatestest.New(t, gatestest.Options{Exec: x})
	e.X.Grant(executiontest.SharedScope, gatestest.Reviewer, identity.RoleReviewer)
	ctx := context.Background()
	scope := executiontest.SharedScope

	backend := ss.Repos.AddRepository(scope, "backend")
	front := ss.Repos.AddRepository(scope, "frontend")
	task := "refs/heads/kodama/work/" + string(ss.Project.Item)
	ss.Repos.SetTip(backend, task, executiontest.CommitB)
	ss.Repos.SetTip(front, "refs/heads/main", executiontest.CommitC)
	ss.Internal.Accepted[backend.ID] = executiontest.CommitB
	def := ss.DefineSource(executiontest.SourceDoc(sqlTwo,
		executiontest.SharedStep("check", "workspace: {access: read}"),
		gatestest.Gate("review", []string{"check"}),
		executiontest.SharedStep("integrate", "needs: [review]", "workspace: {access: write}")))
	run, err := ss.Svc.AdmitSourceRun(ctx, execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t149"}, execution.SourceRunCommand{
		Scope: scope, DefinitionID: def.ID, Origin: execution.Origin{Kind: execution.OriginWorkItem, WorkItemID: ss.Project.Item},
		ContentRevision: x.ContentRevision(ss.Project),
	})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ss.Svc.Cancel(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t149"}, scope, run.ID)
	})
	ref := execution.RunRef{Scope: scope, ID: run.ID}
	pass := func() {
		t.Helper()
		if _, err := ss.Svc.ReconcileRun(ctx, ref); err != nil && !errors.Is(err, execution.ErrBusy) {
			t.Fatalf("reconcile: %v", err)
		}
	}
	// Bring the run's resources up, record the per-source preparation receipt.
	ss.Resources.AutoReady = true
	if in, err := ss.Svc.ClaimRunnable(ctx, scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
		t.Fatalf("the first claim only starts resource holding: %v %v", in, err)
	}
	pass()
	receipt := runtimev3.PreparationReceiptRequestV3{HooksDisabled: true, Submodules: runtimev3.SubmodulesSkipped, CredentialsRemoved: true, Attempts: 1}
	for _, s := range run.Sources.Entries {
		receipt.Sources = append(receipt.Sources, runtimev3.PreparedSource{Path: s.Path, Commit: runtimev1.GitCommit(s.Commit), TreeID: executiontest.CommitC})
	}
	if _, err := ss.Svc.RecordPreparationReceiptV3(ctx, scope, run.ID, executiontest.ContractClaim, receipt); err != nil {
		t.Fatalf("receipt: %v", err)
	}
	pass()
	pass()
	// The reader runs, begins and completes.
	in, err := ss.Svc.ClaimRunnable(ctx, scope.TenantID)
	if err != nil || in.StepID != "check" {
		t.Fatalf("claim the reader: %v %+v", err, in)
	}
	ss.Drive.Start(in)
	c, err := in.DecodeContextV3()
	if err != nil {
		t.Fatal(err)
	}
	begin := runtimev3.BeginRequestV3{Sources: []runtimev3.BeginSource{}}
	for _, s := range c.Workspace.Sources {
		begin.Sources = append(begin.Sources, runtimev3.BeginSource{Path: s.Path, Head: s.HeadCommit, TrackedClean: true})
	}
	if _, err := ss.Svc.RecordBeginV3(ctx, scope, in.AttemptID, executiontest.ContractClaim, begin); err != nil {
		t.Fatalf("begin: %v", err)
	}
	zero := int64(0)
	res := runtimev3.ResultV3{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}, Commits: []runtimev3.CommitOutput{}}
	hash, err := runtimev3.ResultHash(res)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Svc.CommitResultV3(ctx, execution.ResultCommitV3{Scope: scope, AttemptID: in.AttemptID, Request: runtimev3.ResultRequestV3{ResultHash: hash, Result: res}}); err != nil {
		t.Fatalf("result: %v", err)
	}
	const published = "7777777777777777777777777777777777777777"
	x.OwnerExec(`INSERT INTO attempt_commits (tenant_id, project_id, attempt_id, run_id, ordinal, repository_id, ref, commit, push_id) VALUES ($1,$2,$3,$4,0,$5,$6,$7,'push_t149')`,
		string(scope.TenantID), string(scope.ProjectID), string(in.AttemptID), string(run.ID), backend.ID, task, published)
	pass()
	pass()
	e.Bases.Set(backend.ID, "refs/heads/main", sqlBase)
	return &sourceSetGate{E: e, SS: ss, Run: run, Backend: backend, Front: front, Task: task, Commit: published}
}

func (s *sourceSetGate) gate(t *testing.T) *gates.Gate {
	t.Helper()
	g, created, err := s.E.Gates.RequestForStep(context.Background(), s.Run.Scope, s.Run.ID, "review")
	if err != nil || !created {
		t.Fatalf("request the gate: %v created=%v", err, created)
	}
	return g
}

// TestSourceSetGateSQL (DQ-17, GR-12, DQ-A13) over the real PostgreSQL gate
// store, execution store and migration 013: a gate over a work-item run with a
// source set stores the digest of the run's pinned set and a canonical subject
// naming each repository's commit and the writable repository's integration
// base; its approval is consumed once; the integration approval exists only
// while the default branch is at the approved base; a moved base invalidates the
// approval once; and the database refuses a subject that names a foreign
// repository, a single commit beside a set, or a rewritten subject.
func TestSourceSetGateSQL(t *testing.T) {
	s := newSourceSetGate(t)
	ctx := context.Background()
	scope := s.Run.Scope
	g := s.gate(t)

	set, ok := g.Subject.SourceSet()
	if !ok || set.Digest != s.Run.Sources.Digest || len(set.Sources) != 2 {
		t.Fatalf("the subject names the run's source set: %+v %v", set, ok)
	}
	var back, front gates.SubjectSource
	for _, x := range set.Sources {
		switch x.RepositoryID {
		case s.Backend.ID:
			back = x
		case s.Front.ID:
			front = x
		}
	}
	if back.Commit != s.Commit || back.Base == nil || back.Base.Commit != sqlBase || back.Base.Ref != "refs/heads/main" || front.Commit != executiontest.CommitC || front.Base != nil {
		t.Fatalf("the candidate is the writer's commit and the pinned commit, with the base of the writable repository only: back %+v front %+v", back, front)
	}

	t.Run("the row stores the digest, no single commit, and restores", func(t *testing.T) {
		var nullCommit bool
		var digest string
		s.E.X.Scalar(&nullCommit, `SELECT candidate_commit IS NULL FROM gates WHERE id = $1`, string(g.ID))
		s.E.X.Scalar(&digest, `SELECT source_set_digest FROM gates WHERE id = $1`, string(g.ID))
		if !nullCommit || digest != s.Run.Sources.Digest {
			t.Fatalf("columns: null commit %v, digest %q", nullCommit, digest)
		}
		got, err := s.E.Store.Get(ctx, scope, g.ID)
		if err != nil || got.SubjectDigest() != g.SubjectDigest() {
			t.Fatalf("restore: %v", err)
		}
		if again, created, err := s.E.Gates.RequestForStep(ctx, scope, s.Run.ID, "review"); err != nil || created || again.ID != g.ID {
			t.Fatalf("an idempotent request: %v %v", err, created)
		}
	})

	t.Run("no integration approval before approval, the step or after a reject", func(t *testing.T) {
		if _, err := s.E.Gates.IntegrationApproval(ctx, scope, s.Run.ID, g.ID, s.Backend.ID); !errors.Is(err, gates.ErrNoApproval) {
			t.Fatalf("a pending gate: %v", err)
		}
	})

	t.Run("the database refuses a malformed or foreign subject and any rewrite", func(t *testing.T) {
		fail := func(name, sql string, args ...any) {
			t.Helper()
			if err := s.E.X.OwnerExecErr(sql, args...); err == nil {
				t.Errorf("%s: statement succeeded", name)
			}
		}
		const q = `UPDATE gates SET `
		fail("rewrite the digest", q+`source_set_digest = 'sha256:`+strings.Repeat("e", 64)+`' WHERE id = $1`, string(g.ID))
		fail("add a single commit", q+`candidate_commit = '`+strings.Repeat("1", 40)+`' WHERE id = $1`, string(g.ID))
		fail("drop the set", q+`source_set_digest = NULL WHERE id = $1`, string(g.ID))
		fail("rewrite the subject", q+`subject = replace(subject, '"base"', '"bases"') WHERE id = $1`, string(g.ID))
		fail("a subject with neither", `INSERT INTO gates (tenant_id, project_id, id, run_id, step_id, work_item_id, content_revision, candidate_commit, definition_id, subject, subject_digest,
			required, status, version, requested_at, deadline, created_at, updated_at)
			SELECT tenant_id, project_id, 'gate_neither', run_id, 'deploy', work_item_id, content_revision, NULL, definition_id, subject, subject_digest, required, 'pending', 1,
			requested_at, deadline, created_at, updated_at FROM gates WHERE id = $1`, string(g.ID))
		fail("a foreign repository in the subject", `INSERT INTO gates (tenant_id, project_id, id, run_id, step_id, work_item_id, content_revision, candidate_commit, definition_id, subject, subject_digest,
			required, status, version, requested_at, deadline, created_at, updated_at, source_set_digest)
			SELECT tenant_id, project_id, 'gate_foreign', run_id, 'deploy', work_item_id, content_revision, NULL, definition_id,
			replace(subject, '`+s.Front.ID+`', 'repo_foreign'), subject_digest, required, 'pending', 1, requested_at, deadline, created_at, updated_at, source_set_digest
			FROM gates WHERE id = $1`, string(g.ID))
	})

	g, err := approve(t, s.E, g, gatestest.Reviewer)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	t.Run("an approved gate yields no integration approval until its step passed", func(t *testing.T) {
		if _, err := s.E.Gates.IntegrationApproval(ctx, scope, s.Run.ID, g.ID, s.Backend.ID); !errors.Is(err, gates.ErrNoApproval) {
			t.Fatalf("approved, step still waiting: %v", err)
		}
		rep, err := s.E.Gates.ProcessGate(ctx, scope, g.ID)
		if err != nil || rep.Completed != 1 {
			t.Fatalf("progress: %+v %v", rep, err)
		}
	})

	t.Run("the integration approval names exactly the base and the approved commit", func(t *testing.T) {
		got, err := s.E.Gates.IntegrationApproval(ctx, scope, s.Run.ID, g.ID, s.Backend.ID)
		want := gates.IntegrationApproval{GateID: g.ID, RunID: s.Run.ID, RepositoryID: s.Backend.ID, Ref: "refs/heads/main", Base: sqlBase, Candidate: s.Commit, CurrentTip: sqlBase}
		if err != nil || got != want {
			t.Fatalf("approval %+v %v, want %+v", got, err, want)
		}
		if _, err := s.E.Gates.IntegrationApproval(ctx, scope, s.Run.ID, g.ID, s.Front.ID); !errors.Is(err, gates.ErrNoApproval) {
			t.Fatalf("a read repository: %v", err)
		}
		other := domain.Scope{TenantID: gatestest.ScopeB.TenantID, ProjectID: gatestest.ScopeB.ProjectID}
		if _, err := s.E.Gates.IntegrationApproval(ctx, other, s.Run.ID, g.ID, s.Backend.ID); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("another tenant: %v", err)
		}
	})

	t.Run("a moved base invalidates the approval once and yields a conflict", func(t *testing.T) {
		s.E.Bases.Set(s.Backend.ID, "refs/heads/main", sqlMoved)
		got, err := s.E.Gates.IntegrationApproval(ctx, scope, s.Run.ID, g.ID, s.Backend.ID)
		var se *gates.StaleError
		if !errors.As(err, &se) || !se.Definitive || se.Reason != gates.ReasonBaseMoved || se.SupersedingCommit != sqlMoved || got != (gates.IntegrationApproval{}) {
			t.Fatalf("a moved base: %+v %v", got, err)
		}
		cur := gateByID(t, s.E, g)
		if cur.Invalidation == nil || cur.Invalidation.Reason != gates.ReasonBaseMoved || cur.Status != gates.StatusApproved {
			t.Fatalf("the approval stays historical, invalidated once: %+v", cur)
		}
		if n := outboxCount(s.E, "gate.invalidated", g.ID); n != 1 {
			t.Fatalf("gate.invalidated events: %d", n)
		}
		s.E.Bases.Set(s.Backend.ID, "refs/heads/main", sqlBase)
		if _, err := s.E.Gates.IntegrationApproval(ctx, scope, s.Run.ID, g.ID, s.Backend.ID); !errors.Is(err, gates.ErrStaleSubject) {
			t.Fatalf("a revived approval: %v", err)
		}
		if n := outboxCount(s.E, "gate.invalidated", g.ID); n != 1 {
			t.Fatalf("a second invalidation was recorded: %d", n)
		}
	})
}

// TestSourceSetGateBaseMovedBeforeDecision: a pending gate whose base moved takes
// no decision and is invalidated once, on real SQL.
func TestSourceSetGateBaseMovedBeforeDecision(t *testing.T) {
	s := newSourceSetGate(t)
	g := s.gate(t)
	s.E.Bases.Set(s.Backend.ID, "refs/heads/main", sqlMoved)
	_, err := approve(t, s.E, g, gatestest.Reviewer)
	var se *gates.StaleError
	if !errors.As(err, &se) || !se.Definitive || se.Reason != gates.ReasonBaseMoved {
		t.Fatalf("decide over a moved base: %v", err)
	}
	cur := gateByID(t, s.E, g)
	if cur.Status != gates.StatusPending || cur.Decision != nil || cur.Invalidation == nil {
		t.Fatalf("the refused decision: %+v", cur)
	}
	if n := count(s.E, `SELECT count(*) FROM gates WHERE id = $1 AND decision IS NULL AND invalidation_reason = 'base_moved'`, string(g.ID)); n != 1 {
		t.Fatalf("the stored gate: %d", n)
	}
	if stepStatus(t, s.E, s.Run, "review") != execution.StepPending {
		t.Fatal("the gate step progressed")
	}
}

// TestSourceSetGatePushApplied: after the integration push applied, the default
// branch is the candidate. Reconciliation must not invalidate the consumed
// approval or emit gate.invalidated, and a replay is answered "already
// integrated". A revoked reviewer's approval yields none, on real SQL.
func TestSourceSetGatePushApplied(t *testing.T) {
	s := newSourceSetGate(t)
	ctx := context.Background()
	scope := s.Run.Scope
	g := s.gate(t)
	g, err := approve(t, s.E, g, gatestest.Reviewer)
	if err != nil {
		t.Fatal(err)
	}
	if rep, err := s.E.Gates.ProcessGate(ctx, scope, g.ID); err != nil || rep.Completed != 1 {
		t.Fatalf("progress: %+v %v", rep, err)
	}
	if _, err := s.E.Gates.IntegrationApproval(ctx, scope, s.Run.ID, g.ID, s.Backend.ID); err != nil {
		t.Fatalf("before the push: %v", err)
	}

	t.Run("a revoked reviewer's approval yields none", func(t *testing.T) {
		s.E.X.Revoke(scope, gatestest.Reviewer)
		if _, err := s.E.Gates.IntegrationApproval(ctx, scope, s.Run.ID, g.ID, s.Backend.ID); !errors.Is(err, gates.ErrNoApproval) {
			t.Fatalf("a revoked reviewer: %v", err)
		}
		s.E.X.Grant(scope, gatestest.Reviewer, identity.RoleReviewer)
		if _, err := s.E.Gates.IntegrationApproval(ctx, scope, s.Run.ID, g.ID, s.Backend.ID); err != nil {
			t.Fatalf("the reviewer holds the authority again: %v", err)
		}
	})

	t.Run("the applied push is not a moved base", func(t *testing.T) {
		s.E.Bases.Set(s.Backend.ID, "refs/heads/main", s.Commit)
		if rep, err := s.E.Gates.Reconcile(ctx, scope.TenantID); err != nil || rep.Invalidated != 0 {
			t.Fatalf("reconcile: %+v %v", rep, err)
		}
		if cur := gateByID(t, s.E, g); cur.Invalidation != nil {
			t.Fatalf("a successful integration invalidated the approval: %+v", cur.Invalidation)
		}
		if n := outboxCount(s.E, "gate.invalidated", g.ID); n != 0 {
			t.Fatalf("a spurious gate.invalidated event: %d", n)
		}
		got, err := s.E.Gates.IntegrationApproval(ctx, scope, s.Run.ID, g.ID, s.Backend.ID)
		var done *gates.AlreadyIntegratedError
		if !errors.Is(err, gates.ErrAlreadyIntegrated) || !errors.As(err, &done) || got != (gates.IntegrationApproval{}) ||
			done.Approval.Candidate != s.Commit || done.Approval.Base != sqlBase || done.Approval.CurrentTip != s.Commit {
			t.Fatalf("a replay after the push: %+v %v", got, err)
		}
	})
}
