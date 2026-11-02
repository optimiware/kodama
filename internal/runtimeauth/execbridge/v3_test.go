package execbridge_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/execbridge"
)

// The v3 half of the bridge (T-150): the committed resources of a source-set run
// map to ContextV3 states, the lease of an attempt, the v3 recorders and the
// version-mismatch classification.

func sourceSetAggregate(t *testing.T) *execution.Aggregate {
	t.Helper()
	a := aggregate(t) // preparation + service.db of a run_shared run
	a.Run.Sources = &execution.RunSources{
		BaseURL: "https://git.example.invalid/git",
		Entries: []execution.RunSource{{RepositoryID: "repo_01", Path: "backend"}, {RepositoryID: "repo_02", Path: "frontend"}},
	}
	prep := a.Resource("preparation")
	prep.Spec.Preparation = &execution.PreparationSpec{
		Image: prep.Spec.Preparation.Image, TimeoutSecs: 600, MaxBytes: 1 << 30, MaxAttempts: 2,
		Sources: []execution.PreparationSourceSpec{
			{RepositoryID: "repo_01", RepositoryURL: "https://git.example.invalid/git/t_acme/p_inventory/backend.git", Commit: strings.Repeat("1", 40), Path: "backend"},
			{RepositoryID: "repo_02", RepositoryURL: "https://git.example.invalid/git/t_acme/p_inventory/frontend.git", Commit: strings.Repeat("2", 40), Path: "frontend"},
			{RepositoryID: "repo_01", RepositoryURL: "https://git.example.invalid/git/t_acme/p_inventory/backend.git", Commit: strings.Repeat("1", 40), Path: "tools"},
		},
	}
	return a
}

func TestStateOfServesASourceSetRunOnTheV3WireOnly(t *testing.T) {
	a := sourceSetAggregate(t)
	st, err := execbridge.StateOf(a, "preparation", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Validate(); err != nil {
		t.Fatalf("the state is not a valid authority state: %v", err)
	}
	if st.Wire() != runtimeauth.WireV3 || st.ContextV3 == nil || st.Context.Preparation != nil {
		t.Fatalf("a source-set run is served on the v3 wire only: %+v", st)
	}
	p := st.ContextV3.Preparation
	if len(p.Sources) != 3 || p.Sources[2].Path != "tools" || p.MaxAttempts != 2 || p.MountPath != "/workspace" {
		t.Fatalf("preparation: %+v", p)
	}
	if len(p.Git.Repositories) != 2 || p.Git.Repositories[0] != "repo_01" || p.Git.Repositories[1] != "repo_02" ||
		p.Git.BaseURL != "https://git.example.invalid/git" || p.Git.CredentialFile != "/kodama/git/credential" || p.Git.CAFile != nil ||
		p.Git.GrantID != execbridge.PreparationGrantRef(scope, "run_01") {
		t.Fatalf("the Git delivery names the files and holds no secret: %+v", p.Git)
	}
	if strings.Contains(p.Git.GrantID, "gitc_") || !strings.HasPrefix(p.Git.GrantID, "gg_") {
		t.Fatalf("grant reference: %s", p.Git.GrantID)
	}
	if other := execbridge.PreparationGrantRef(scope, "run_02"); other == p.Git.GrantID || execbridge.PreparationGrantRef(domain.Scope{TenantID: "t_globex", ProjectID: scope.ProjectID}, "run_01") == p.Git.GrantID {
		t.Fatal("the reference is not bound to tenant and run")
	}
	ca := "/kodama/git/ca.pem"
	a.Run.Sources.CAFile = &ca
	if st, err := execbridge.StateOf(a, "preparation", now); err != nil || st.ContextV3.Preparation.Git.CAFile == nil || *st.ContextV3.Preparation.Git.CAFile != ca || st.Validate() != nil {
		t.Fatalf("ca file: %+v %v", st, err)
	}
	svc, err := execbridge.StateOf(a, "service.db", now)
	if err != nil || svc.Validate() != nil || svc.ContextV3 == nil || svc.ContextV3.Service == nil || svc.ContextV3.Service.Name != "db" {
		t.Fatalf("a service of a source-set run: %+v %v", svc, err)
	}
	// Ending the resource ends the state exactly as on the v2 wire.
	d := now.Add(time.Minute)
	a.Run.DeadlineAt = &d
	if st, err := execbridge.StateOf(a, "preparation", now.Add(5*time.Minute)); err != nil || st.Phase != runtimeauth.ResourceEnded {
		t.Fatalf("run deadline: %+v %v", st.Phase, err)
	}
	// A spec without the sources of a source-set run is not mapped.
	a = sourceSetAggregate(t)
	a.Resource("preparation").Spec.Preparation.Sources = nil
	if _, err := execbridge.StateOf(a, "preparation", now); !errors.Is(err, runtimeauth.ErrUnavailable) {
		t.Fatalf("a source-set preparation without sources: %v", err)
	}
	// An earlier run keeps its v2 state.
	if st, err := execbridge.StateOf(aggregate(t), "preparation", now); err != nil || st.Wire() != runtimeauth.WireV2 || st.ContextV3 != nil {
		t.Fatalf("v2 run: %+v %v", st, err)
	}
}

func attemptAggregate(deadline *time.Time) *execution.Aggregate {
	return &execution.Aggregate{
		Run:      &execution.Run{ID: "run_01", Scope: scope, Status: execution.RunRunning, DeadlineAt: deadline},
		Attempts: []*execution.Attempt{{Scope: scope, ID: "att_01", RunID: "run_01", Status: execution.AttemptRunning}},
		Controls: map[domain.AttemptID]*execution.Control{"att_01": execution.NewControl("att_01")},
	}
}

func TestLeaseOfIsTheAttemptsOwnState(t *testing.T) {
	deadline := now.Add(time.Hour)
	live := func(mut func(a *execution.Aggregate), at time.Time) execbridge.AttemptLease {
		a := attemptAggregate(&deadline)
		if mut != nil {
			mut(a)
		}
		l, err := execbridge.LeaseOf(a, "att_01", at)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	if l := live(nil, now); !l.Live || !l.Deadline.Equal(deadline) {
		t.Fatalf("a running attempt: %+v", l)
	}
	revoked := now
	for name, mut := range map[string]func(*execution.Aggregate){
		"revoked":   func(a *execution.Aggregate) { a.Control("att_01").RevokedAt = &revoked },
		"cancelled": func(a *execution.Aggregate) { a.Attempts[0].Status = execution.AttemptCancelled },
		"succeeded": func(a *execution.Aggregate) { a.Attempts[0].Status = execution.AttemptSucceeded },
		"failed":    func(a *execution.Aggregate) { a.Attempts[0].Status = execution.AttemptFailed },
		"terminal":  func(a *execution.Aggregate) { a.Run.Status = execution.RunFailed },
		"cancel":    func(a *execution.Aggregate) { a.Run.CancelRequestedAt = &revoked },
		"failure":   func(a *execution.Aggregate) { a.Run.FailureIntentAt = &revoked },
	} {
		if l := live(mut, now); l.Live || !l.Deadline.Equal(deadline) {
			t.Errorf("%s: %+v", name, l)
		}
	}
	if l := live(nil, deadline); l.Live {
		t.Errorf("at the run's deadline: %+v", l)
	}
	if l, err := execbridge.LeaseOf(func() *execution.Aggregate { a := attemptAggregate(nil); return a }(), "att_01", now); err != nil || !l.Live || !l.Deadline.IsZero() {
		t.Errorf("a run without a deadline: %+v %v", l, err)
	}
	for _, c := range []struct {
		a  *execution.Aggregate
		id domain.AttemptID
	}{{nil, "att_01"}, {attemptAggregate(&deadline), "att_nowhere"}} {
		if _, err := execbridge.LeaseOf(c.a, c.id, now); !errors.Is(err, runtimeauth.ErrNotFound) {
			t.Errorf("unknown: %v", err)
		}
	}
}

type fakeExecV3 struct {
	beginErr, receiptErr error
	claim                execution.GrantClaim
	calls                int
}

func (f *fakeExecV3) RecordBeginV3(_ context.Context, _ domain.Scope, attempt domain.AttemptID, claim execution.GrantClaim, _ runtimev3.BeginRequestV3) (execution.BeginAck, error) {
	f.calls++
	f.claim = claim
	return execution.BeginAck{AttemptID: attempt, BeganAt: now, WorkspaceState: runtimev3.StateModified}, f.beginErr
}

func (f *fakeExecV3) RecordPreparationReceiptV3(_ context.Context, _ domain.Scope, _ domain.RunID, claim execution.GrantClaim, _ runtimev3.PreparationReceiptRequestV3) (execution.ReceiptAck, error) {
	f.calls++
	f.claim = claim
	return execution.ReceiptAck{Digest: "sha256:" + hex64, RecordedAt: now}, f.receiptErr
}

func TestV3RecordersServeExecutionsAnswers(t *testing.T) {
	ctx := context.Background()
	sess := runtimeauth.SessionID("ses_" + strings.Repeat("a", 22))
	att := runtimeauth.Authorization{Scope: scope, AttemptID: "att_1", Session: sess, TokenGeneration: 7}
	ex := &fakeExecV3{}
	res, err := execbridge.BeginsV3{Exec: ex, Recheck: recheck{}}.RecordBeginV3(ctx, att, runtimev3.BeginRequestV3{})
	if err != nil || res.AttemptID != "att_1" || res.WorkspaceState != runtimev3.StateModified || ex.claim != (execution.GrantClaim{Session: string(sess), Generation: 7}) {
		t.Fatalf("begin: %+v %v claim %+v", res, err, ex.claim)
	}
	ra := runtimeauth.ResourceAuthorization{Ref: runtimeauth.ResourceRef{Scope: scope, RunID: "run_01", Key: "preparation"}, Session: sess, TokenGeneration: 9}
	rec, err := execbridge.ReceiptsV3{Exec: ex, Recheck: recheck{}}.RecordPreparationReceiptV3(ctx, ra, runtimev3.PreparationReceiptRequestV3{})
	if err != nil || rec.Digest != "sha256:"+hex64 || ex.claim != (execution.GrantClaim{Session: string(sess), Generation: 9}) {
		t.Fatalf("receipt: %+v %v", rec, err)
	}
	// A revoked session never reaches execution; an unconfigured bridge refuses.
	idle := &fakeExecV3{}
	if _, err := (execbridge.BeginsV3{Exec: idle, Recheck: recheck{err: runtimeauth.ErrAttemptRevoked}}).RecordBeginV3(ctx, att, runtimev3.BeginRequestV3{}); !errors.Is(err, runtimeauth.ErrAttemptRevoked) || idle.calls != 0 {
		t.Fatalf("a revoked session reached execution: %v", err)
	}
	if _, err := (execbridge.ReceiptsV3{Exec: idle, Recheck: recheck{err: runtimeauth.ErrResourceRevoked}}).RecordPreparationReceiptV3(ctx, ra, runtimev3.PreparationReceiptRequestV3{}); !errors.Is(err, runtimeauth.ErrResourceRevoked) || idle.calls != 0 {
		t.Fatalf("a revoked resource reached execution: %v", err)
	}
	if _, err := (execbridge.BeginsV3{Exec: idle}).RecordBeginV3(ctx, att, runtimev3.BeginRequestV3{}); !errors.Is(err, runtimeauth.ErrUnavailable) {
		t.Fatalf("a bridge without a recheck: %v", err)
	}
	if _, err := (execbridge.ReceiptsV3{}).RecordPreparationReceiptV3(ctx, ra, runtimev3.PreparationReceiptRequestV3{}); !errors.Is(err, runtimeauth.ErrUnavailable) {
		t.Fatalf("an unconfigured bridge: %v", err)
	}
	// Execution's failures are classified as the runtime authority's, and a version
	// mismatch is neither a plain conflict nor an outage.
	for _, tc := range []struct{ in, want error }{
		{execution.ErrContextVersionMismatch, runtimeauth.ErrContextVersionMismatch},
		{execution.ErrBeginConflict, runtimeauth.ErrIdempotencyConflict},
		{execution.ErrReceiptConflict, runtimeauth.ErrReceiptConflict},
		{execution.ErrAttemptRevoked, runtimeauth.ErrAttemptRevoked},
	} {
		if _, err := (execbridge.BeginsV3{Exec: &fakeExecV3{beginErr: tc.in}, Recheck: recheck{}}).RecordBeginV3(ctx, att, runtimev3.BeginRequestV3{}); !errors.Is(err, tc.want) {
			t.Errorf("%v: %v", tc.in, err)
		}
	}
	if errors.Is(execbridge.Classify(execution.ErrContextVersionMismatch), runtimeauth.ErrUnavailable) {
		t.Error("a version mismatch read as unavailable")
	}
}
