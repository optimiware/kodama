package execbridge_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/execbridge"
)

var (
	scope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	now   = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	hex64 = strings.Repeat("a", 64)
)

func spec(kind execution.ResourceKind, key string) execution.ResourceSpec {
	s := execution.ResourceSpec{
		Scope: scope, RunID: "run_01", Backend: "process", Kind: kind, Key: key,
		Name:   execution.ResourceName(scope, "run_01", kind, key),
		Policy: execution.PolicyPin{ID: "run-v1", Digest: "sha256:" + hex64}, RoleShape: "sha256:" + hex64,
		DeadlineAt: now.Add(time.Hour).Format(time.RFC3339),
	}
	switch kind {
	case execution.KindPreparation:
		s.Preparation = &execution.PreparationSpec{Image: "registry.invalid/prep@sha256:" + hex64, RepositoryURL: "https://git.example.invalid/acme/inventory.git",
			Commit: strings.Repeat("1", 40), TimeoutSecs: 600, MaxBytes: 1 << 30, MaxAttempts: 2}
	case execution.KindService:
		s.Service = &execution.ServiceSpec{Name: "db", Role: "general", Image: "registry.invalid/db@sha256:" + hex64,
			Command: []string{"/entry.sh", "postgres"}, Args: []string{"-c", "x=y"}, Env: map[string]string{"POSTGRES_DB": "app"},
			Secrets: []execution.SecretPin{{Ref: "secret://project/pw", Version: 3, Name: "pw", Delivery: runtimev1.DeliveryFile}}}
	}
	return s
}

func aggregate(t *testing.T) *execution.Aggregate {
	t.Helper()
	run := &execution.Run{ID: "run_01", Scope: scope, Status: execution.RunRunning}
	a := &execution.Aggregate{Run: run}
	for i, s := range []execution.ResourceSpec{spec(execution.KindPreparation, "preparation"), spec(execution.KindService, "service.db")} {
		r, err := execution.NewRunResource(s, i, now)
		if err != nil {
			t.Fatal(err)
		}
		r.UID = "uid-" + string(rune('a'+i)) + "-0123456789"
		a.Resources = append(a.Resources, r)
	}
	return a
}

func TestStateOfMapsOnlyCommittedSpecAndEndsWithTheResource(t *testing.T) {
	a := aggregate(t)
	prep, err := execbridge.StateOf(a, "preparation", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := prep.Validate(); err != nil {
		t.Fatalf("the state is not a valid authority state: %v", err)
	}
	r := a.Resource("preparation")
	if prep.Phase != runtimeauth.ResourceActive || prep.WorkloadName != r.Name || prep.WorkloadUID != r.UID || prep.Fingerprint != r.Fingerprint ||
		prep.Context.Preparation.Commit != runtimev2.GitCommit(strings.Repeat("1", 40)) || prep.Context.Preparation.MaxAttempts != 2 {
		t.Fatalf("state %+v", prep)
	}
	svc, err := execbridge.StateOf(a, "service.db", now)
	if err != nil || svc.Validate() != nil {
		t.Fatalf("service state: %v %v", err, svc.Validate())
	}
	c := svc.Context.Service
	if c.Name != "db" || c.Role != runtimev2.RoleGeneral || len(c.Secrets) != 1 || c.Secrets[0].Version != 3 || c.Command.Command[0] != "/entry.sh" || c.Command.Env["POSTGRES_DB"] != "app" {
		t.Fatalf("service context %+v", c)
	}
	// A defaults-only service has a null command.
	a2 := aggregate(t)
	a2.Resource("service.db").Spec.Service.Command, a2.Resource("service.db").Spec.Service.Secrets = nil, nil
	if st, err := execbridge.StateOf(a2, "service.db", now); err != nil || st.Context.Service.Command.Command != nil || st.Context.Service.Secrets == nil {
		t.Fatalf("defaults-only service: %+v %v", st, err)
	}

	// Everything that ends a resource's authority ends the state.
	ends := map[string]func(a *execution.Aggregate, at *time.Time){
		"stop intent":  func(a *execution.Aggregate, _ *time.Time) { t := now; a.Resource("preparation").StopRequestedAt = &t },
		"stopped":      func(a *execution.Aggregate, _ *time.Time) { t := now; a.Resource("preparation").StoppedAt = &t },
		"removed":      func(a *execution.Aggregate, _ *time.Time) { t := now; a.Resource("preparation").RemovedAt = &t },
		"cancellation": func(a *execution.Aggregate, _ *time.Time) { t := now; a.Run.CancelRequestedAt = &t },
		"failure":      func(a *execution.Aggregate, _ *time.Time) { t := now; a.Run.FailureIntentAt = &t },
		"terminal run": func(a *execution.Aggregate, _ *time.Time) { a.Run.Status = execution.RunFailed },
		"run deadline": func(a *execution.Aggregate, _ *time.Time) { d := now.Add(time.Minute); a.Run.DeadlineAt = &d },
		"own deadline": func(_ *execution.Aggregate, at *time.Time) { *at = now.Add(2 * time.Hour) },
	}
	for name, mutate := range ends {
		a := aggregate(t)
		at := now.Add(5 * time.Minute)
		mutate(a, &at)
		st, err := execbridge.StateOf(a, "preparation", at)
		if err != nil || st.Phase != runtimeauth.ResourceEnded {
			t.Errorf("%s: %+v %v", name, st.Phase, err)
		}
	}

	// Resources without a runtime identity, and keys that are not resources, are refused.
	for _, key := range []string{"workspace", "network", "service.missing", "nonsense", ""} {
		if _, err := execbridge.StateOf(a, key, now); err == nil {
			t.Errorf("key %q was mapped", key)
		}
	}
	if _, err := execbridge.StateOf(nil, "preparation", now); !errors.Is(err, runtimeauth.ErrNotFound) {
		t.Errorf("no aggregate: %v", err)
	}
}

type fakeExec struct {
	beginErr, receiptErr error
}

func (f fakeExec) RecordBegin(_ context.Context, _ domain.Scope, attempt domain.AttemptID, _ execution.GrantClaim, _ runtimev2.BeginRequest) (execution.BeginAck, error) {
	return execution.BeginAck{AttemptID: attempt, BeganAt: now, WorkspaceState: runtimev2.StateClean, IgnoredPresent: true}, f.beginErr
}

func (f fakeExec) RecordPreparationReceipt(context.Context, domain.Scope, domain.RunID, execution.GrantClaim, runtimev2.PreparationReceiptRequest) (execution.ReceiptAck, error) {
	return execution.ReceiptAck{Digest: "sha256:" + hex64, RecordedAt: now}, f.receiptErr
}

func TestFailuresAreClassifiedAsTheRuntimeAuthoritys(t *testing.T) {
	for _, tc := range []struct {
		in   error
		want error
	}{
		{execution.ErrBeginConflict, runtimeauth.ErrIdempotencyConflict},
		{execution.ErrReceiptConflict, runtimeauth.ErrReceiptConflict},
		{execution.ErrResourceUnbound, runtimeauth.ErrResourceUnbound},
		{execution.ErrResourceRevoked, runtimeauth.ErrResourceRevoked},
		{execution.ErrAttemptRevoked, runtimeauth.ErrAttemptRevoked},
		{execution.ErrNotFound, runtimeauth.ErrNotFound},
		{execution.ErrHidden, runtimeauth.ErrNotFound},
		{execution.ErrDenied, runtimeauth.ErrForbidden},
		{execution.ErrInvalid, runtimeauth.ErrInvalidRequest},
		{errors.New("driver exploded"), runtimeauth.ErrUnavailable},
		{execution.ErrConflict, runtimeauth.ErrUnavailable}, // an unclassified conflict never reads as a decision
	} {
		if got := execbridge.Classify(tc.in); !errors.Is(got, tc.want) {
			t.Errorf("%v classified as %v, want %v", tc.in, got, tc.want)
		}
	}
	if execbridge.Classify(nil) != nil {
		t.Fatal("nil")
	}
	// Specific before general: a revoked resource is not a plain denial and a begin
	// conflict is not an unavailable dependency.
	if errors.Is(execbridge.Classify(execution.ErrResourceRevoked), runtimeauth.ErrForbidden) {
		t.Error("resource_revoked read as forbidden")
	}
}

func TestBeginsAndReceiptsServeExecutionsAnswers(t *testing.T) {
	ctx := context.Background()
	att := runtimeauth.Authorization{Scope: scope, AttemptID: "att_1"}
	rc := recheck{}
	res, err := execbridge.Begins{Exec: fakeExec{}, Recheck: rc}.RecordBegin(ctx, att, runtimev2.BeginRequest{})
	if err != nil || res.AttemptID != "att_1" || res.WorkspaceState != runtimev2.StateClean || !res.IgnoredPresent {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := (execbridge.Begins{Exec: fakeExec{beginErr: execution.ErrAttemptRevoked}, Recheck: rc}).RecordBegin(ctx, att, runtimev2.BeginRequest{}); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
		t.Fatalf("a revoked attempt: %v", err)
	}
	ra := runtimeauth.ResourceAuthorization{Ref: runtimeauth.ResourceRef{Scope: scope, RunID: "run_01", Key: "preparation"}}
	rec, err := execbridge.Receipts{Exec: fakeExec{}, Recheck: rc}.RecordPreparationReceipt(ctx, ra, runtimev2.PreparationReceiptRequest{})
	if err != nil || rec.Digest != "sha256:"+hex64 {
		t.Fatalf("%+v %v", rec, err)
	}
	if _, err := (execbridge.Receipts{Exec: fakeExec{receiptErr: execution.ErrReceiptConflict}, Recheck: rc}).RecordPreparationReceipt(ctx, ra, runtimev2.PreparationReceiptRequest{}); !errors.Is(err, runtimeauth.ErrReceiptConflict) {
		t.Fatalf("a receipt conflict: %v", err)
	}
	if _, err := (execbridge.Begins{}).RecordBegin(ctx, att, runtimev2.BeginRequest{}); !errors.Is(err, runtimeauth.ErrUnavailable) {
		t.Fatalf("an unconfigured bridge must be unavailable: %v", err)
	}
	// Review finding 7: without the session recheck the bridge records nothing, and a
	// revoked session never reaches execution.
	if _, err := (execbridge.Begins{Exec: fakeExec{}}).RecordBegin(ctx, att, runtimev2.BeginRequest{}); !errors.Is(err, runtimeauth.ErrUnavailable) {
		t.Fatalf("a bridge without a recheck must refuse: %v", err)
	}
	if _, err := (execbridge.Receipts{Exec: fakeExec{}}).RecordPreparationReceipt(ctx, ra, runtimev2.PreparationReceiptRequest{}); !errors.Is(err, runtimeauth.ErrUnavailable) {
		t.Fatalf("a bridge without a recheck must refuse: %v", err)
	}
	counted := &countingExec{}
	revoked := recheck{err: runtimeauth.ErrAttemptRevoked}
	if _, err := (execbridge.Begins{Exec: counted, Recheck: revoked}).RecordBegin(ctx, att, runtimev2.BeginRequest{}); !errors.Is(err, runtimeauth.ErrAttemptRevoked) || counted.n != 0 {
		t.Fatalf("a revoked session reached execution (%d calls): %v", counted.n, err)
	}
	// The verified session and signing generation travel to execution as the grant
	// claim it checks in its own transaction.
	sess := runtimeauth.SessionID("ses_" + strings.Repeat("a", 22))
	claimed := &countingExec{}
	if _, err := (execbridge.Begins{Exec: claimed, Recheck: rc}).RecordBegin(ctx, runtimeauth.Authorization{Scope: scope, AttemptID: "att_1", Session: sess, TokenGeneration: 7}, runtimev2.BeginRequest{}); err != nil ||
		claimed.claim != (execution.GrantClaim{Session: string(sess), Generation: 7}) {
		t.Fatalf("begin claim %+v %v", claimed.claim, err)
	}
	if _, err := (execbridge.Receipts{Exec: claimed, Recheck: rc}).RecordPreparationReceipt(ctx, runtimeauth.ResourceAuthorization{Ref: ra.Ref, Session: sess, TokenGeneration: 9}, runtimev2.PreparationReceiptRequest{}); err != nil ||
		claimed.claim != (execution.GrantClaim{Session: string(sess), Generation: 9}) {
		t.Fatalf("receipt claim %+v %v", claimed.claim, err)
	}
	revokedRes := recheck{err: runtimeauth.ErrResourceRevoked}
	if _, err := (execbridge.Receipts{Exec: counted, Recheck: revokedRes}).RecordPreparationReceipt(ctx, ra, runtimev2.PreparationReceiptRequest{}); !errors.Is(err, runtimeauth.ErrResourceRevoked) || counted.n != 0 {
		t.Fatalf("a revoked resource reached execution (%d calls): %v", counted.n, err)
	}
}

type recheck struct{ err error }

func (r recheck) Reauthorize(context.Context, runtimeauth.Authorization) error { return r.err }
func (r recheck) ReauthorizeResource(context.Context, runtimeauth.ResourceAuthorization) error {
	return r.err
}

// countingExec fails the test's expectation if execution is ever called.
type countingExec struct {
	n     int
	claim execution.GrantClaim
}

func (c *countingExec) RecordBegin(_ context.Context, _ domain.Scope, _ domain.AttemptID, claim execution.GrantClaim, _ runtimev2.BeginRequest) (execution.BeginAck, error) {
	c.n++
	c.claim = claim
	return execution.BeginAck{}, nil
}

func (c *countingExec) RecordPreparationReceipt(_ context.Context, _ domain.Scope, _ domain.RunID, claim execution.GrantClaim, _ runtimev2.PreparationReceiptRequest) (execution.ReceiptAck, error) {
	c.n++
	c.claim = claim
	return execution.ReceiptAck{}, nil
}
