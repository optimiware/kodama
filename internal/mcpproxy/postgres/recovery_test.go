package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest"
	"github.com/optimiware/kodama/internal/storage"
)

type commitMode int

const (
	commitNormal     commitMode = iota
	commitLostReply             // the commit happens, the caller sees an error
	commitNeverLands            // the commit is rolled back, the caller sees an error
)

var errLostCommit = errors.New("connection reset while committing")

// flakyDB injects commit faults into write transactions only. skip lets the
// first writes through so a test can target the Nth write of a use case.
type flakyDB struct {
	inner storage.TxBeginner
	mu    sync.Mutex
	mode  commitMode
	skip  int
}

func (f *flakyDB) arm(mode commitMode, skip int) {
	f.mu.Lock()
	f.mode, f.skip = mode, skip
	f.mu.Unlock()
}

func (f *flakyDB) take() commitMode {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mode == commitNormal {
		return commitNormal
	}
	if f.skip > 0 {
		f.skip--
		return commitNormal
	}
	m := f.mode
	f.mode = commitNormal // one fault per arming
	return m
}

func (f *flakyDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := f.inner.BeginTx(ctx, opts)
	if err != nil || opts.AccessMode == pgx.ReadOnly {
		return tx, err
	}
	return &flakyTx{Tx: tx, db: f}, nil
}

type flakyTx struct {
	pgx.Tx
	db *flakyDB
}

func (t *flakyTx) Commit(ctx context.Context) error {
	switch t.db.take() {
	case commitLostReply:
		if err := t.Tx.Commit(ctx); err != nil {
			return err
		}
		return errLostCommit
	case commitNeverLands:
		_ = t.Tx.Rollback(ctx)
		return errLostCommit
	}
	return t.Tx.Commit(ctx)
}

// Test_audit_and_commit_failures: an audit rollback and an uncertain admission
// commit prevent dispatch; an uncertain outcome commit stays recoverable without
// a resend.
func Test_audit_and_commit_failures(t *testing.T) {
	setup := func(t *testing.T) (*mcptest.Stack, mcptest.Ready, *mcpproxy.Service, *flakyDB, mcpproxy.SessionID) {
		s := mcptest.New(t, mcptest.Config{})
		r := s.Setup([]string{"mutate"}, []string{"mutate"}, nil)
		db := &flakyDB{inner: s.RT.Runtime}
		svc, _ := s.ServiceOver(db, "inc-t033-flaky")
		id, err := svc.OpenSession(bg, r.W.Caller, r.Reg.ID)
		if err != nil {
			t.Fatal(err)
		}
		return s, r, svc, db, id
	}
	inv := func(svc *mcpproxy.Service, r mcptest.Ready, id mcpproxy.SessionID, req string) error {
		_, err := svc.Invoke(bg, r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: mcpproxy.RequestID(req), Tool: "mutate", Arguments: []byte(`{"key":"k"}`)})
		return err
	}

	t.Run("an audit failure rolls the admission back and nothing is dispatched or charged", func(t *testing.T) {
		s, r, svc, _, id := setup(t)
		s.RT.OwnerExec(`CREATE FUNCTION t033_fail_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
			IF NEW.action = 'mcp_tool.admitted' THEN RAISE EXCEPTION 'audit unavailable'; END IF; RETURN NEW; END $$`)
		s.RT.OwnerExec(`CREATE TRIGGER t033_fail_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION t033_fail_audit()`)
		if err := inv(svc, r, id, "req_t033_audit"); !errors.Is(err, mcpproxy.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if n := s.Fixture.Mutations.Load(); n != 0 {
			t.Fatalf("a call was dispatched without durable audit: %d", n)
		}
		if n := count(t, s, `SELECT count(*) FROM mcp_invocations`); n != 0 {
			t.Fatalf("%d receipts survived the rollback", n)
		}
		if n := count(t, s, `SELECT count(*) FROM mcp_run_calls`); n != 0 {
			t.Fatalf("the rolled-back call was charged: %d", n)
		}
		s.RT.OwnerExec(`DROP TRIGGER t033_fail_audit ON audit`)
		if err := inv(svc, r, id, "req_t033_audit"); err != nil {
			t.Fatalf("the same request after audit recovered: %v", err)
		}
		if n := s.Fixture.Mutations.Load(); n != 1 {
			t.Fatalf("mutations %d", n)
		}
	})

	t.Run("an admission that never lands prevents dispatch", func(t *testing.T) {
		s, r, svc, db, id := setup(t)
		db.arm(commitNeverLands, 0)
		err := inv(svc, r, id, "req_t033_never")
		if !errors.Is(err, mcpproxy.ErrAdmissionUncertain) || !errors.Is(err, mcpproxy.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if n := s.Fixture.Mutations.Load(); n != 0 {
			t.Fatalf("dispatched on an uncommitted admission: %d", n)
		}
		if n := count(t, s, `SELECT count(*) FROM mcp_invocations`); n != 0 {
			t.Fatalf("receipt count %d", n)
		}
	})

	t.Run("an admission whose commit reply is lost prevents dispatch and recovers as unknown", func(t *testing.T) {
		s, r, svc, db, id := setup(t)
		db.arm(commitLostReply, 0)
		err := inv(svc, r, id, "req_t033_lost")
		if !errors.Is(err, mcpproxy.ErrAdmissionUncertain) {
			t.Fatalf("got %v", err)
		}
		if n := s.Fixture.Mutations.Load(); n != 0 {
			t.Fatalf("dispatched on an uncertain admission: %d", n)
		}
		// The receipt did commit: a retry of the same request is a duplicate and
		// is never sent; recovery turns the orphaned receipt into unknown.
		if err := inv(svc, r, id, "req_t033_lost"); !errors.Is(err, mcpproxy.ErrDuplicate) {
			t.Fatalf("retry: %v", err)
		}
		n, err := s.Store.RecoverStale(bg, r.W.Scope.TenantID, s.RT.Clock.Now().AddDate(1, 0, 0), 10)
		if err != nil || n != 1 {
			t.Fatalf("recovered %d err %v", n, err)
		}
		if st := count(t, s, `SELECT count(*) FROM mcp_invocations WHERE state = 'unknown'`); st != 1 {
			t.Fatalf("unknown receipts: %d", st)
		}
		if n := s.Fixture.Mutations.Load(); n != 0 {
			t.Fatalf("recovery dispatched: %d", n)
		}
	})

	for _, mode := range []struct {
		name string
		m    commitMode
	}{{"an outcome commit that never lands", commitNeverLands}, {"an outcome commit whose reply is lost", commitLostReply}} {
		t.Run(mode.name+" stays recoverable without a resend", func(t *testing.T) {
			s, r, svc, db, id := setup(t)
			db.arm(mode.m, 1) // let the admission through, fail the outcome write
			if err := inv(svc, r, id, "req_t033_out"); err != nil {
				t.Fatalf("the call itself succeeded and returns its result: %v", err)
			}
			if n := s.Fixture.Mutations.Load(); n != 1 {
				t.Fatalf("mutations %d", n)
			}
			// A retry of the same request is never sent again.
			if err := inv(svc, r, id, "req_t033_out"); !errors.Is(err, mcpproxy.ErrDuplicate) {
				t.Fatalf("retry: %v", err)
			}
			if _, err := s.Store.RecoverStale(bg, r.W.Scope.TenantID, s.RT.Clock.Now().AddDate(1, 0, 0), 10); err != nil {
				t.Fatal(err)
			}
			var st string
			if err := s.RT.Owner.QueryRow(bg, `SELECT state FROM mcp_invocations WHERE id = 'req_t033_out'`).Scan(&st); err != nil {
				t.Fatal(err)
			}
			if st != "unknown" && st != "succeeded" {
				t.Fatalf("receipt state %s", st)
			}
			if mode.m == commitNeverLands && st != "unknown" {
				t.Fatalf("an unrecorded outcome must recover as unknown, got %s", st)
			}
			if n := s.Fixture.Mutations.Load(); n != 1 {
				t.Fatalf("a retry or recovery re-sent the call: %d", n)
			}
		})
	}
}
