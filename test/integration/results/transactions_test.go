package results_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/storage"
)

var errInjected = errors.New("injected failure")

// failingAppender fails the append of events while armed.
type failingAppender struct {
	inner execpg.EventAppender
	armed atomic.Bool
}

func (f *failingAppender) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	if f.armed.Load() {
		return eventspg.AppendResult{}, errInjected
	}
	return f.inner.Append(ctx, tx, env)
}

// lossyDB loses the acknowledgment of commits while armed: either after the
// commit applied (outcome unknown to the caller, committed) or instead of it
// (rolled back).
type lossyDB struct {
	inner storage.TxBeginner
	mode  atomic.Int32 // 0 off, 1 lose the acknowledgment, 2 fail the commit
}

func (d *lossyDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := d.inner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &lossyTx{Tx: tx, db: d}, nil
}

type lossyTx struct {
	pgx.Tx
	db *lossyDB
}

func (t *lossyTx) Commit(ctx context.Context) error {
	switch t.db.mode.Load() {
	case 1:
		if err := t.Tx.Commit(ctx); err != nil {
			return err
		}
		return errors.New("connection reset by peer while waiting for the commit acknowledgment")
	case 2:
		_ = t.Tx.Rollback(ctx)
		return errors.New("server closed the connection unexpectedly during commit")
	}
	return t.Tx.Commit(ctx)
}

// evidenceDouble is an artifact-evidence port that can go down.
type evidenceDouble struct {
	down atomic.Bool
	refs map[domain.ArtifactID]results.ArtifactEvidence
}

func (d *evidenceDouble) References(_ context.Context, _ domain.Scope, _ domain.AttemptID, ids []domain.ArtifactID) (map[domain.ArtifactID]results.ArtifactEvidence, error) {
	if d.down.Load() {
		return nil, results.Fail(results.ErrUnavailable, errInjected, "artifact store outage")
	}
	out := map[domain.ArtifactID]results.ArtifactEvidence{}
	for _, id := range ids {
		if r, ok := d.refs[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}
func (d *evidenceDouble) Protect(context.Context, domain.Scope, domain.AttemptID, []domain.ArtifactID) error {
	if d.down.Load() {
		return results.Fail(results.ErrUnavailable, errInjected, "artifact store outage")
	}
	return nil
}

// TestResultAtomicityAndIsolation: PX-05, PX-12, IT-02, IT-04, IT-06 and SD-04.
// Pooled tenant isolation, revoked-grant races, audit/outbox rollback, commit
// uncertainty, artifact-validation outages and concurrent replay after response
// loss. Real PostgreSQL through the real adapters; no NATS, S3 or Git (the
// artifact store is a controlled double only in the outage case).
func TestResultAtomicityAndIsolation(t *testing.T) {
	ctx := context.Background()

	t.Run("pooled connections never carry a tenant's authority to another", func(t *testing.T) {
		e := New(t, Options{})
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		b := e.Run(e.NewProject(ScopeB), Job("build", readRun, labelJob))
		// A token only authorizes its own attempt, in either direction.
		for _, c := range []struct{ route, token *Attempt }{{a, b}, {b, a}} {
			if code, _ := e.doToken(c.route.ID(), c.token.Token, "result", mustJSON(t, Request(t, Completed(outputsOf("x"))))); code != 401 {
				t.Fatalf("a foreign token on a route: %d", code)
			}
		}
		// Interleaved results of both tenants over the shared pool each land in their own tenant.
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			for _, at := range []*Attempt{a, b} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := e.Submit(at, Request(t, Completed(outputsOf(string(at.Scope.TenantID))))); err != nil {
						t.Error(err)
					}
				}()
			}
		}
		wg.Wait()
		for _, at := range []*Attempt{a, b} {
			if got := e.Aggregate(at).Step("build").Outputs["label"].String(); got != string(at.Scope.TenantID) {
				t.Fatalf("tenant %s accepted %q", at.Scope.TenantID, got)
			}
		}
		// Row-level security: a tenant sees only its own grants and attempts.
		for _, tenant := range []domain.TenantID{ScopeA.TenantID, ScopeB.TenantID} {
			err := e.TenantTx(tenant, func(ctx context.Context, tx pgx.Tx) error {
				var n, foreign int
				if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE tenant_id <> $1) FROM runtime_grants`, string(tenant)).Scan(&n, &foreign); err != nil {
					return err
				}
				if n != 1 || foreign != 0 {
					t.Errorf("tenant %s sees %d grants (%d foreign)", tenant, n, foreign)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		// A guard configured with another tenant's session is refused inside the transaction.
		_, _, err := e.Guarded.Update(rpgSession(ctx, b), a.Scope, a.Run.ID, execution.UpdateOptions{}, func(context.Context, execution.Env, *execution.Aggregate) (execution.Effects, error) {
			t.Error("a foreign session reached the mutation")
			return execution.Effects{}, nil
		})
		if err == nil {
			t.Fatal("a foreign session mutated the run")
		}
	})

	t.Run("an audit or outbox failure rolls the acceptance back and the retry succeeds once", func(t *testing.T) {
		var fa *failingAppender
		e := New(t, Options{WrapAppender: func(in execpg.EventAppender) execpg.EventAppender { fa = &failingAppender{inner: in}; return fa }})
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		req := Request(t, Completed(outputsOf("v1")))
		history, events := e.HistoryRows(a), outbox(e, ScopeA.TenantID)
		fa.armed.Store(true)
		if _, err := e.Submit(a, req); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		fa.armed.Store(false)
		if at := e.Persisted(a); at.ResultHash != "" || at.Status != execution.AttemptRunning || at.FinishedAt != nil {
			t.Fatalf("a failed acceptance left state behind: %s %q", at.Status, at.ResultHash)
		}
		if e.HistoryRows(a) != history || outbox(e, ScopeA.TenantID) != events {
			t.Fatal("a failed acceptance left history or outbox rows")
		}
		ack, err := e.Submit(a, req)
		if err != nil || ack.AttemptStatus != runtimev1.AttemptSucceeded {
			t.Fatalf("%+v %v", ack, err)
		}
		if n := e.Count(`SELECT count(*) FROM run_history WHERE run_id = $1 AND subject_kind = 'attempt' AND to_status = 'succeeded'`, string(a.Run.ID)); n != 1 {
			t.Fatalf("%d success transitions", n)
		}
	})

	t.Run("an unknown commit outcome is unavailable, and the retry observes the same receipt", func(t *testing.T) {
		var db *lossyDB
		e := New(t, Options{WrapDB: func(in storage.TxBeginner) storage.TxBeginner { db = &lossyDB{inner: in}; return db }})
		req := func(l string) runtimev1.ResultRequest { return Request(t, Completed(outputsOf(l))) }

		// The commit applied but its acknowledgment was lost.
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		db.mode.Store(1)
		if _, err := e.Submit(a, req("v1")); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("a lost commit acknowledgment must not read as success: %v", err)
		}
		db.mode.Store(0)
		persisted := e.Persisted(a)
		if persisted.ResultHash != string(req("v1").ResultHash) || persisted.FinishedAt == nil {
			t.Fatalf("the commit should have applied: %+v", persisted)
		}
		ack, err := e.Submit(a, req("v1"))
		if err != nil || string(ack.AcceptedAt) != persisted.FinishedAt.UTC().Format(time.RFC3339Nano) {
			t.Fatalf("the retry must observe the original receipt: %+v %v", ack, err)
		}
		if _, err := e.Submit(a, req("v2")); !errors.Is(err, results.ErrResultConflict) {
			t.Fatalf("%v", err)
		}

		// The commit failed and rolled back.
		b := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		db.mode.Store(2)
		if _, err := e.Submit(b, req("v1")); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		db.mode.Store(0)
		if e.Persisted(b).ResultHash != "" {
			t.Fatal("a rolled-back commit left an acceptance")
		}
		if _, err := e.Submit(b, req("v1")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("an artifact outage fails a new result closed and never replaces a receipt decision", func(t *testing.T) {
		d := &evidenceDouble{}
		e := New(t, Options{Evidence: func(results.Evidence) results.Evidence { return d }})
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, reportJob))
		d.refs = map[domain.ArtifactID]results.ArtifactEvidence{"art_report": {
			ID: "art_report", Scope: a.Scope, Attempt: a.ID(), Kind: runtimev1.KindFile, Role: runtimev1.RoleOutput, Usable: true,
			Provenance: results.Provenance{RepositoryURL: a.Context.Workspace.Source.RepositoryURL, Commit: string(a.Context.Workspace.Source.Commit)},
		}}
		req := Request(t, Completed(reportOutput("art_report")))

		d.down.Store(true)
		before := e.Commits.Count()
		if code, body := e.SubmitHTTP(a, req); code != 503 || errCode(t, body) != domain.CodeUnavailable {
			t.Fatalf("%d %s", code, body)
		}
		if e.Commits.Count() != before || e.Persisted(a).ResultHash != "" {
			t.Fatal("an artifact outage reached execution or changed the attempt")
		}
		d.down.Store(false)
		ack, err := e.Submit(a, req)
		if err != nil || ack.AttemptStatus != runtimev1.AttemptSucceeded {
			t.Fatalf("%+v %v", ack, err)
		}
		// Later outage, or later removal, cannot replace the receipt decision.
		d.down.Store(true)
		for range 3 {
			if again, err := e.Submit(a, req); err != nil || again != ack {
				t.Fatalf("a replay depended on the artifact store: %+v %v", again, err)
			}
		}
		other := Request(t, Completed(reportOutput("art_other")))
		if _, err := e.Submit(a, other); !errors.Is(err, results.ErrResultConflict) {
			t.Fatalf("a conflict depended on the artifact store: %v", err)
		}
		d.refs = nil
		d.down.Store(false)
		if again, err := e.Submit(a, req); err != nil || again != ack {
			t.Fatalf("a replay depended on retained artifacts: %+v %v", again, err)
		}
	})

	t.Run("revocation racing concurrent acceptance never acknowledges what was not persisted", func(t *testing.T) {
		e := New(t, Options{})
		for round := 0; round < 6; round++ {
			a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
			req := Request(t, Completed(outputsOf("v1")))
			var wg sync.WaitGroup
			var mu sync.Mutex
			var acks []runtimev1.ResultResponse
			start := make(chan struct{})
			for range 6 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					ack, err := e.Submit(a, req)
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err == nil:
						acks = append(acks, ack)
					case errors.Is(err, results.ErrAttemptRevoked), errors.Is(err, results.ErrUnavailable):
					default:
						t.Errorf("round %d: %v", round, err)
					}
				}()
			}
			wg.Add(1)
			go func() { defer wg.Done(); <-start; _ = e.Auth.Revoke(ctx, a.Scope, a.ID()) }()
			close(start)
			wg.Wait()
			at := e.Persisted(a)
			for _, ack := range acks {
				if at.ResultHash != string(ack.ResultHash) || at.Status != execution.AttemptSucceeded {
					t.Fatalf("round %d: an acknowledgment without a durable acceptance: %+v vs %s %q", round, ack, at.Status, at.ResultHash)
				}
			}
			if n := e.Count(`SELECT count(*) FROM run_history WHERE run_id = $1 AND subject_kind = 'attempt' AND to_status = 'succeeded'`, string(a.Run.ID)); n > 1 {
				t.Fatalf("round %d: %d acceptances", round, n)
			}
			// After the revocation every callback is denied, whatever was accepted.
			if _, err := e.Submit(a, req); !errors.Is(err, results.ErrAttemptRevoked) {
				t.Fatalf("round %d: after revocation: %v", round, err)
			}
		}
	})

	t.Run("concurrent replays after response loss converge on one acceptance", func(t *testing.T) {
		var db *lossyDB
		e := New(t, Options{WrapDB: func(in storage.TxBeginner) storage.TxBeginner { db = &lossyDB{inner: in}; return db }})
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		req := Request(t, Completed(outputsOf("v1")))
		db.mode.Store(1) // every response is lost after the commit applied
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() { defer wg.Done(); _, _ = e.Submit(a, req) }()
		}
		wg.Wait()
		db.mode.Store(0)
		first, err := e.Submit(a, req)
		if err != nil {
			t.Fatal(err)
		}
		for range 4 {
			if again, err := e.Submit(a, req); err != nil || again != first {
				t.Fatalf("%+v %v", again, err)
			}
		}
		if n := e.Count(`SELECT count(*) FROM run_history WHERE run_id = $1 AND subject_kind = 'attempt' AND to_status = 'succeeded'`, string(a.Run.ID)); n != 1 {
			t.Fatalf("%d acceptances", n)
		}
	})
}
