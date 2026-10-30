package execbridge_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/execution/memory"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/execbridge"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
)

// The runtime grant is checked IN the transaction that records a begin or a
// preparation receipt, after the run lock. These tests pin the ordering the
// handlers and the bridge cannot close alone: the bridge's final recheck commits,
// a standalone revocation (its own transaction, no execution state) commits, and
// only then does execution record. Over memory and over real PostgreSQL.

var fenceScope = executiontest.SharedScope

const (
	fenceSession = "ses_fencefencefencefencefence"
	fenceOther   = "ses_otherotherotherotherother1"
)

var fenceClaim = execution.GrantClaim{Session: fenceSession, Generation: 1}

// grantOps are the runtime authority's side of a fixture: issue consumed grants
// and revoke them in standalone transactions.
type grantOps struct {
	issueAttempt   func(attempt *execution.AttemptSpec, c execution.GrantClaim)
	issueResource  func(run domain.RunID, key string, c execution.GrantClaim)
	revokeAttempt  func(attempt domain.AttemptID)
	revokeResource func(run domain.RunID, key string)
}

// recheckThen is the bridge's final recheck: it passes, and then (the window
// between the recheck and the record) runs a hook such as a revocation commit.
type recheckThen struct {
	then  func()
	calls atomic.Int32
}

func (r *recheckThen) Reauthorize(context.Context, runtimeauth.Authorization) error {
	r.calls.Add(1)
	if r.then != nil {
		r.then()
	}
	return nil
}

func (r *recheckThen) ReauthorizeResource(context.Context, runtimeauth.ResourceAuthorization) error {
	r.calls.Add(1)
	if r.then != nil {
		r.then()
	}
	return nil
}

type fixture struct {
	t   *testing.T
	s   executiontest.SharedSubject
	g   grantOps
	get func(run domain.RunID) *execution.Aggregate
}

func memFixture(t *testing.T, mod ...func(*executiontest.MemOptions)) *fixture {
	t.Helper()
	var grants *executiontest.MemGrants
	m, s := executiontest.NewSharedMem(t, append([]func(*executiontest.MemOptions){func(o *executiontest.MemOptions) {
		o.GrantFenceOf = func(db *memdb.Store) memory.GrantFencer { grants = executiontest.NewMemGrants(db); return grants }
	}}, mod...)...)
	ctx := context.Background()
	return &fixture{t: t, s: s, get: func(run domain.RunID) *execution.Aggregate {
		agg, err := m.Mem.Get(ctx, fenceScope, run)
		if err != nil {
			t.Fatal(err)
		}
		return agg
	}, g: grantOps{
		issueAttempt: func(a *execution.AttemptSpec, c execution.GrantClaim) {
			if grants == nil {
				return
			}
			if err := grants.IssueAttempt(ctx, fenceScope, a.AttemptID, c); err != nil {
				t.Fatal(err)
			}
		},
		issueResource: func(run domain.RunID, key string, c execution.GrantClaim) {
			if grants == nil {
				return
			}
			if err := grants.IssueResource(ctx, fenceScope, run, key, c); err != nil {
				t.Fatal(err)
			}
		},
		revokeAttempt: func(a domain.AttemptID) {
			if err := grants.RevokeAttempt(ctx, fenceScope, a); err != nil {
				t.Fatal(err)
			}
		},
		revokeResource: func(run domain.RunID, key string) {
			if err := grants.RevokeResource(ctx, fenceScope, run, key); err != nil {
				t.Fatal(err)
			}
		},
	}}
}

func pgFixture(t *testing.T, o executiontest.Options) *fixture {
	t.Helper()
	o.RuntimeGrants = true
	e, s := executiontest.NewSharedPG(t, o)
	ctx := context.Background()
	return &fixture{t: t, s: s, get: func(run domain.RunID) *execution.Aggregate {
		agg, err := e.Store.Get(ctx, fenceScope, run)
		if err != nil {
			t.Fatal(err)
		}
		return agg
	}, g: grantOps{
		issueAttempt: func(a *execution.AttemptSpec, c execution.GrantClaim) {
			e.OwnerExec(`INSERT INTO runtime_grants (tenant_id, project_id, run_id, attempt_id, job_namespace, job_name, credential_digest, signing_generation,
				actions, authority_revision, context_digest, context_doc, created_at, expires_at,
				session_id, nonce_digest, bound_job_uid, bound_pod_name, bound_pod_uid, consumed_at, max_expires_at)
				VALUES ($1::text, $2::text, $3::text, $4::text, 'ns-t113', 'job-t113', sha256(convert_to($4::text, 'UTF8')), $5, ARRAY['context.read'], 1,
				sha256(convert_to($3::text, 'UTF8')), '\x7b7d', now(), now() + interval '1 hour',
				$6, sha256(convert_to('nonce' || $4::text, 'UTF8')), '6f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f', 'pod-t113', '7f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f', now(), now() + interval '1 hour')`,
				string(a.Scope.TenantID), string(a.Scope.ProjectID), string(a.RunID), string(a.AttemptID), c.Generation, c.Session)
		},
		issueResource: func(run domain.RunID, key string, c execution.GrantClaim) {
			kind := "service"
			if key == "preparation" {
				kind = "preparation"
			}
			e.OwnerExec(`INSERT INTO runtime_resource_grants (tenant_id, project_id, run_id, resource_key, resource_kind, credential_digest, signing_generation,
				spec_fingerprint, deadline_at, created_at, expires_at, session_id, nonce_digest, bound_workload_name, bound_workload_uid, consumed_at, max_expires_at)
				VALUES ($1::text, $2::text, $3::text, $4::text, $5, sha256(convert_to($3::text || $4::text, 'UTF8')), $6,
				'sha256:' || repeat('a', 64), now() + interval '2 hours', now(), now() + interval '1 hour',
				$7, sha256(convert_to('nonce' || $3::text, 'UTF8')), 'wl-t113', '8f1c1d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f', now(), now() + interval '90 minutes')`,
				string(fenceScope.TenantID), string(fenceScope.ProjectID), string(run), key, kind, c.Generation, c.Session)
		},
		revokeAttempt: func(a domain.AttemptID) {
			if err := e.RuntimeTx(fenceScope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				n, err := rtpg.RevokeAttempt(ctx, tx, fenceScope, a, e.Clock.Now())
				if err == nil && n != 1 {
					err = errors.New("the revocation touched no grant")
				}
				return err
			}); err != nil {
				t.Error(err)
			}
		},
		revokeResource: func(run domain.RunID, key string) {
			if err := e.RuntimeTx(fenceScope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				n, err := rtpg.RevokeResource(ctx, tx, runtimeauth.ResourceRef{Scope: fenceScope, RunID: run, Key: key}, e.Clock.Now())
				if err == nil && n != 1 {
					err = errors.New("the revocation touched no grant")
				}
				return err
			}); err != nil {
				t.Error(err)
			}
		},
	}}
}

func receiptReq() runtimev2.PreparationReceiptRequest {
	return runtimev2.PreparationReceiptRequest{Commit: executiontest.CommitA, TreeID: executiontest.CommitC, HooksDisabled: true,
		Submodules: runtimev2.SubmodulesSkipped, CredentialsRemoved: true, Attempts: 1}
}

func beginReq() runtimev2.BeginRequest {
	return runtimev2.BeginRequest{Workspace: &runtimev2.BeginWorkspace{Head: executiontest.CommitA, TrackedClean: true}}
}

func (f *fixture) newRun() *executiontest.SharedRun {
	f.t.Helper()
	return executiontest.NewSharedRun(f.t, f.s, executiontest.SharedSpecDoc(true, executiontest.SharedStep("job")))
}

// prepared drives a run to the point where its preparation workload would report:
// the resources are up and the receipt is the next durable step.
func (f *fixture) prepared(c *execution.GrantClaim) (*executiontest.SharedRun, domain.RunID) {
	f.t.Helper()
	sr := f.newRun()
	run := sr.Run().ID
	f.s.Resources.AutoReady = true
	if c != nil {
		f.g.issueResource(run, execution.KeyPreparation, *c)
		sr.Grant(*c)
	}
	sr.Start()
	sr.Pass()
	return sr, run
}

func (f *fixture) receiptRecorded(run domain.RunID) bool {
	f.t.Helper()
	prep := f.get(run).Resource(execution.KeyPreparation)
	return prep != nil && prep.ReceiptAt != nil
}

// runningAttempt drives a run to its first claimed attempt under a resource grant
// and an attempt grant, both live.
func (f *fixture) runningAttempt(c execution.GrantClaim) (*execution.AttemptSpec, domain.RunID) {
	f.t.Helper()
	sr := f.newRun()
	run := sr.Run().ID
	f.g.issueResource(run, execution.KeyPreparation, c)
	sr.Grant(c)
	sr.Up()
	in, err := sr.Claim()
	if err != nil || in == nil {
		f.t.Fatalf("claim: %v", err)
	}
	f.g.issueAttempt(in, c)
	return in, run
}

func (f *fixture) begins(run domain.RunID) int { return len(f.get(run).Begins) }

func authFor(in *execution.AttemptSpec, c execution.GrantClaim) runtimeauth.Authorization {
	return runtimeauth.Authorization{Scope: in.Scope, RunID: in.RunID, AttemptID: in.AttemptID, Session: runtimeauth.SessionID(c.Session), TokenGeneration: c.Generation}
}

func resAuth(run domain.RunID, c execution.GrantClaim) runtimeauth.ResourceAuthorization {
	return runtimeauth.ResourceAuthorization{Ref: runtimeauth.ResourceRef{Scope: fenceScope, RunID: run, Key: execution.KeyPreparation},
		Session: runtimeauth.SessionID(c.Session), TokenGeneration: c.Generation}
}

func suites(t *testing.T, run func(t *testing.T, mk func(t *testing.T, mod ...func(*executiontest.MemOptions)) *fixture)) {
	t.Run("memory", func(t *testing.T) {
		run(t, memFixture)
	})
	t.Run("postgres", func(t *testing.T) {
		run(t, func(t *testing.T, _ ...func(*executiontest.MemOptions)) *fixture {
			return pgFixture(t, executiontest.Options{})
		})
	})
}

// Test_begin_revoked_after_the_recheck_records_nothing: pause AFTER the bridge's
// final recheck, commit a standalone revocation, resume: execution refuses and no
// begin is recorded. The control without the revocation records.
func Test_begin_revoked_after_the_recheck_records_nothing(t *testing.T) {
	suites(t, func(t *testing.T, mk func(*testing.T, ...func(*executiontest.MemOptions)) *fixture) {
		ctx := context.Background()
		t.Run("control: no revocation records the begin", func(t *testing.T) {
			f := mk(t)
			in, run := f.runningAttempt(fenceClaim)
			rc := &recheckThen{}
			res, err := execbridge.Begins{Exec: f.s.Svc, Recheck: rc}.RecordBegin(ctx, authFor(in, fenceClaim), beginReq())
			if err != nil || res.AttemptID != in.AttemptID || f.begins(run) != 1 || rc.calls.Load() != 1 {
				t.Fatalf("%+v %v (%d begins)", res, err, f.begins(run))
			}
		})
		t.Run("a standalone revocation after the final recheck refuses and records nothing", func(t *testing.T) {
			f := mk(t)
			in, run := f.runningAttempt(fenceClaim)
			rc := &recheckThen{then: func() { f.g.revokeAttempt(in.AttemptID) }}
			_, err := execbridge.Begins{Exec: f.s.Svc, Recheck: rc}.RecordBegin(ctx, authFor(in, fenceClaim), beginReq())
			if !errors.Is(err, runtimeauth.ErrAttemptRevoked) || !errors.Is(err, execution.ErrAttemptRevoked) {
				t.Fatalf("a begin on a revoked grant: %v", err)
			}
			if rc.calls.Load() != 1 {
				t.Fatalf("the recheck ran %d times", rc.calls.Load())
			}
			if n := f.begins(run); n != 0 {
				t.Fatalf("%d begins were recorded for a revoked session", n)
			}
			// The refusal is stable: a retry is refused too and records nothing.
			if _, err := (execbridge.Begins{Exec: f.s.Svc, Recheck: &recheckThen{}}).RecordBegin(ctx, authFor(in, fenceClaim), beginReq()); !errors.Is(err, runtimeauth.ErrAttemptRevoked) || f.begins(run) != 0 {
				t.Fatalf("retry: %v (%d begins)", err, f.begins(run))
			}
		})
		t.Run("another session or generation is not this grant's authority", func(t *testing.T) {
			f := mk(t)
			in, run := f.runningAttempt(fenceClaim)
			for name, c := range map[string]execution.GrantClaim{
				"another session":    {Session: fenceOther, Generation: 1},
				"another generation": {Session: fenceSession, Generation: 2},
			} {
				if _, err := (execbridge.Begins{Exec: f.s.Svc, Recheck: &recheckThen{}}).RecordBegin(ctx, authFor(in, c), beginReq()); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
					t.Errorf("%s: %v", name, err)
				}
			}
			if f.begins(run) != 0 {
				t.Fatal("a begin was recorded under a session that is not the grant's")
			}
		})
		t.Run("an attempt with no grant is refused", func(t *testing.T) {
			f := mk(t)
			in, run := f.runningAttempt(fenceClaim)
			other := *in
			other.AttemptID = "att_unknown"
			if _, err := (execbridge.Begins{Exec: f.s.Svc, Recheck: &recheckThen{}}).RecordBegin(ctx, authFor(&other, fenceClaim), beginReq()); err == nil {
				t.Fatal("a begin for an attempt with no grant was accepted")
			}
			if f.begins(run) != 0 {
				t.Fatal("recorded")
			}
		})
	})
}

// Test_receipt_revoked_after_the_recheck_records_nothing is the same ordering for
// the preparation receipt and its resource grant.
func Test_receipt_revoked_after_the_recheck_records_nothing(t *testing.T) {
	suites(t, func(t *testing.T, mk func(*testing.T, ...func(*executiontest.MemOptions)) *fixture) {
		ctx := context.Background()
		t.Run("control: no revocation records the receipt", func(t *testing.T) {
			f := mk(t)
			_, run := f.prepared(&fenceClaim)
			rc := &recheckThen{}
			res, err := execbridge.Receipts{Exec: f.s.Svc, Recheck: rc}.RecordPreparationReceipt(ctx, resAuth(run, fenceClaim), receiptReq())
			if err != nil || res.Digest == "" || !f.receiptRecorded(run) {
				t.Fatalf("%+v %v", res, err)
			}
		})
		t.Run("a standalone revocation after the final recheck refuses and records nothing", func(t *testing.T) {
			f := mk(t)
			_, run := f.prepared(&fenceClaim)
			rc := &recheckThen{then: func() { f.g.revokeResource(run, execution.KeyPreparation) }}
			_, err := execbridge.Receipts{Exec: f.s.Svc, Recheck: rc}.RecordPreparationReceipt(ctx, resAuth(run, fenceClaim), receiptReq())
			if !errors.Is(err, runtimeauth.ErrResourceRevoked) || !errors.Is(err, execution.ErrResourceRevoked) {
				t.Fatalf("a receipt on a revoked grant: %v", err)
			}
			if f.receiptRecorded(run) {
				t.Fatal("a receipt was recorded for a revoked resource")
			}
		})
		t.Run("another session or generation is refused", func(t *testing.T) {
			f := mk(t)
			_, run := f.prepared(&fenceClaim)
			for name, c := range map[string]execution.GrantClaim{
				"another session":    {Session: fenceOther, Generation: 1},
				"another generation": {Session: fenceSession, Generation: 2},
			} {
				if _, err := (execbridge.Receipts{Exec: f.s.Svc, Recheck: &recheckThen{}}).RecordPreparationReceipt(ctx, resAuth(run, c), receiptReq()); !errors.Is(err, runtimeauth.ErrResourceRevoked) {
					t.Errorf("%s: %v", name, err)
				}
			}
			if f.receiptRecorded(run) {
				t.Fatal("recorded")
			}
		})
	})
}

// Test_record_without_a_grant_fence_refuses: a store composed without the fence
// records nothing and says so; it never skips the check.
func Test_record_without_a_grant_fence_refuses(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, f *fixture) {
		t.Helper()
		sr := f.newRun()
		run := sr.Run().ID
		f.s.Resources.AutoReady = true
		sr.Start()
		sr.Pass()
		if _, err := (execbridge.Receipts{Exec: f.s.Svc, Recheck: &recheckThen{}}).RecordPreparationReceipt(ctx, resAuth(run, fenceClaim), receiptReq()); !errors.Is(err, runtimeauth.ErrUnavailable) || !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("a receipt without a fence: %v", err)
		}
		if f.receiptRecorded(run) {
			t.Fatal("a receipt was recorded without a fence")
		}
		// And a begin: a claimed attempt with no way to check its grant.
		if _, err := f.s.Svc.RecordBegin(ctx, fenceScope, "att_whatever", fenceClaim, beginReq()); err == nil {
			t.Fatal("a begin for an unknown attempt was accepted")
		}
	}
	t.Run("memory", func(t *testing.T) {
		check(t, memFixture(t, func(o *executiontest.MemOptions) {
			o.GrantFenceOf = nil
			o.NoGrantFence = true
		}))
	})
	t.Run("postgres component store", func(t *testing.T) {
		e, s := executiontest.NewSharedPG(t, executiontest.Options{NoGrantFence: true})
		f := &fixture{t: t, s: s, get: func(run domain.RunID) *execution.Aggregate {
			agg, err := e.Store.Get(ctx, fenceScope, run)
			if err != nil {
				t.Fatal(err)
			}
			return agg
		}}
		check(t, f)
	})
	t.Run("an empty claim is invalid before anything is read", func(t *testing.T) {
		f := memFixture(t)
		if _, err := f.s.Svc.RecordBegin(ctx, fenceScope, "att_x", execution.GrantClaim{}, beginReq()); !errors.Is(err, execution.ErrInvalid) {
			t.Fatalf("%v", err)
		}
		if _, err := f.s.Svc.RecordPreparationReceipt(ctx, fenceScope, "run_x", execution.GrantClaim{}, receiptReq()); !errors.Is(err, execution.ErrInvalid) {
			t.Fatalf("%v", err)
		}
	})
}

// pausingFence runs the real SQL fence and then holds the record's transaction
// open until released, with the grant row's share lock taken.
type pausingFence struct {
	inner   execpg.GrantFencer
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
}

func (p *pausingFence) pause() {
	if !p.armed.Load() {
		return
	}
	p.reached <- struct{}{}
	<-p.release
}

func (p *pausingFence) AttemptGrantLive(ctx context.Context, tx pgx.Tx, scope domain.Scope, a domain.AttemptID, c execution.GrantClaim) error {
	if err := p.inner.AttemptGrantLive(ctx, tx, scope, a, c); err != nil {
		return err
	}
	p.pause()
	return nil
}

func (p *pausingFence) ResourceGrantLive(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID, key string, c execution.GrantClaim) error {
	if err := p.inner.ResourceGrantLive(ctx, tx, scope, run, key, c); err != nil {
		return err
	}
	p.pause()
	return nil
}

// Test_revocation_waits_for_an_inflight_record_and_cannot_deadlock: the other
// order. The record's transaction holds the run row and (after the fence) the
// grant row's share lock; a standalone revocation, which locks only the grant row,
// waits for it, does not deadlock and lands after the record commits. Real
// PostgreSQL.
func Test_revocation_waits_for_an_inflight_record_and_cannot_deadlock(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []string{"begin", "receipt"} {
		t.Run(kind, func(t *testing.T) {
			p := &pausingFence{reached: make(chan struct{}, 1), release: make(chan struct{})}
			f := pgFixture(t, executiontest.Options{GrantFenceWrap: func(in execpg.GrantFencer) execpg.GrantFencer { p.inner = in; return p }})
			var record func() error
			var recorded func() bool
			var revoke func()
			switch kind {
			case "begin":
				in, run := f.runningAttempt(fenceClaim)
				record = func() error {
					_, err := execbridge.Begins{Exec: f.s.Svc, Recheck: &recheckThen{}}.RecordBegin(ctx, authFor(in, fenceClaim), beginReq())
					return err
				}
				recorded = func() bool { return f.begins(run) == 1 }
				revoke = func() { f.g.revokeAttempt(in.AttemptID) }
			default:
				_, run := f.prepared(&fenceClaim)
				record = func() error {
					_, err := execbridge.Receipts{Exec: f.s.Svc, Recheck: &recheckThen{}}.RecordPreparationReceipt(ctx, resAuth(run, fenceClaim), receiptReq())
					return err
				}
				recorded = func() bool { return f.receiptRecorded(run) }
				revoke = func() { f.g.revokeResource(run, execution.KeyPreparation) }
			}
			// Only the record under test pauses; the fixture's own receipt did not.
			p.armed.Store(true)
			recordErr := make(chan error, 1)
			go func() { recordErr <- record() }()
			select {
			case <-p.reached:
			case <-time.After(20 * time.Second):
				t.Fatal("the record never reached its fence")
			}
			revoked := make(chan struct{})
			go func() { revoke(); close(revoked) }()
			select {
			case <-revoked:
				t.Fatal("the revocation committed while the record held the grant's share lock")
			case <-time.After(500 * time.Millisecond):
			}
			close(p.release)
			select {
			case err := <-recordErr:
				if err != nil {
					t.Fatalf("the in-flight record: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the record did not finish")
			}
			select {
			case <-revoked:
			case <-time.After(20 * time.Second):
				t.Fatal("the revocation never completed (deadlock?)")
			}
			if !recorded() {
				t.Fatal("the record that won the lock was not committed")
			}
		})
	}
}
