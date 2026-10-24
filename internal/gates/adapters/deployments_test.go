package adapters_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/adapters"
)

var (
	depScope   = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	otherScope = domain.Scope{TenantID: "t_acme", ProjectID: "p_other"}
	otherTen   = domain.Scope{TenantID: "t_other", ProjectID: "p_inventory"}
	t0         = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	digestA    = runtimev1.Digest("sha256:" + strings.Repeat("a", 64))
)

func record(t *testing.T, sc domain.Scope, mutate func(*delivery.DeploymentRecord)) delivery.Deployment {
	t.Helper()
	exp := t0.Add(time.Hour)
	d, err := delivery.NewDeployment(delivery.Registration{
		Scope: sc, Environment: "env_staging", ConfigDigest: digestA, Namespace: "staging",
		Source:   delivery.Source{RepositoryURL: "https://git.example.invalid/r.git", Commit: commitA},
		Image:    delivery.Image{ArtifactID: "art_1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("b", 64))},
		Producer: delivery.Producer{Attempt: "att_build", Run: "run_build", Step: "build", ToolImage: "registry.example.invalid/tools@sha256:" + strings.Repeat("c", 64)},
		Run:      "run_deploy", Step: "deploy", ActionKey: "act:0123456789abcdef", Attempt: "att_deploy", ExpiresAt: &exp,
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		rec := d.Record()
		mutate(&rec)
		if d, err = delivery.RestoreDeployment(rec); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func cleanupRequested(rec *delivery.DeploymentRecord) {
	at := t0.Add(time.Minute)
	rec.Status = delivery.StatusSucceeded
	rec.CleanupRequestedAt, rec.CleanupState = &at, delivery.CleanupRequested
}

// ---- committed reader ----------------------------------------------------------

type records struct {
	byScope map[domain.Scope]map[delivery.DeploymentID]delivery.Deployment
	err     error
	// answerAnyScope makes the double answer whatever scope was recorded, like a
	// peer that does not filter: the adapter's own scope check must hold.
	answerAnyScope bool
	reads          []domain.Scope
}

func (r *records) GetDeployment(_ context.Context, sc domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error) {
	r.reads = append(r.reads, sc)
	if r.err != nil {
		return delivery.Deployment{}, r.err
	}
	if d, ok := r.byScope[sc][id]; ok {
		return d, nil
	}
	if r.answerAnyScope {
		for _, m := range r.byScope {
			if d, ok := m[id]; ok {
				return d, nil
			}
		}
	}
	return delivery.Deployment{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
}

func recordsOf(sc domain.Scope, d delivery.Deployment) *records {
	return &records{byScope: map[domain.Scope]map[delivery.DeploymentID]delivery.Deployment{sc: {d.ID(): d}}}
}

// ---- transaction ----------------------------------------------------------------

type depRow struct {
	values []any
	err    error
}

func (r depRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("column count mismatch")
	}
	for i, d := range dest {
		target := reflect.ValueOf(d).Elem()
		if r.values[i] == nil {
			target.Set(reflect.Zero(target.Type()))
			continue
		}
		target.Set(reflect.ValueOf(r.values[i]))
	}
	return nil
}

type depTx struct {
	rows    map[string]depRow
	failure error
	queries [][]any
	sql     []string
}

func (x *depTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	x.sql = append(x.sql, sql)
	x.queries = append(x.queries, args)
	if x.failure != nil {
		return depRow{err: x.failure}
	}
	if r, ok := x.rows[args[0].(string)+"/"+args[1].(string)+"/"+args[2].(string)]; ok {
		return r
	}
	return depRow{err: pgx.ErrNoRows}
}

func rowOf(d delivery.Deployment) depRow {
	r := d.Record()
	return depRow{values: []any{string(r.Scope.TenantID), string(r.Scope.ProjectID), string(r.ID), string(r.ConfigDigest), r.Source.Commit,
		r.CleanupRequestedAt, string(r.CleanupState), r.DeletedAt}}
}

func rowKey(sc domain.Scope, id delivery.DeploymentID) string {
	return string(sc.TenantID) + "/" + string(sc.ProjectID) + "/" + string(id)
}

// fakeTx is an open transaction whose only used capability is QueryRow.
type fakeTx struct {
	pgx.Tx
	q *depTx
}

func (f fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return f.q.QueryRow(ctx, sql, args...)
}

// both runs one assertion against the committed-reader adapter and against the
// transaction-bound binder, which must answer identically.
func both(t *testing.T, dep delivery.Deployment, recordScope domain.Scope, check func(t *testing.T, d gates.Deployments)) {
	t.Helper()
	t.Run("committed", func(t *testing.T) {
		d, err := adapters.NewDeployments(recordsOf(recordScope, dep))
		if err != nil {
			t.Fatal(err)
		}
		check(t, d)
	})
	t.Run("transaction", func(t *testing.T) {
		tx := &depTx{rows: map[string]depRow{rowKey(recordScope, dep.ID()): rowOf(dep)}}
		check(t, adapters.BindDeployments(fakeTx{q: tx}))
	})
}

// Test_gate_deployment_binder proves the deployment adapter behind the gate
// store's DeploymentBinder: the recorded digest, scope and candidate are bound,
// a foreign, absent or cleaned-up deployment is refused as not found, an
// unreadable record is an outage, and the binder reads through the deciding
// transaction.
func Test_gate_deployment_binder(t *testing.T) {
	ctx := context.Background()
	live := record(t, depScope, nil)
	id := string(live.ID())

	t.Run("binds the recorded digest, scope and candidate", func(t *testing.T) {
		both(t, live, depScope, func(t *testing.T, d gates.Deployments) {
			snap, err := d.Snapshot(ctx, depScope, id)
			if err != nil {
				t.Fatalf("a live deployment of the scope: %v", err)
			}
			want := gates.DeploymentSnapshot{ID: id, ConfigDigest: string(digestA), Scope: depScope, Candidate: commitA}
			if snap != want {
				t.Fatalf("snapshot %+v, want the recorded %+v", snap, want)
			}
		})
	})

	t.Run("a foreign or absent deployment is not found", func(t *testing.T) {
		for name, asked := range map[string]domain.Scope{"another project": otherScope, "another tenant": otherTen} {
			both(t, live, depScope, func(t *testing.T, d gates.Deployments) {
				if _, err := d.Snapshot(ctx, asked, id); !errors.Is(err, gates.ErrNotFound) {
					t.Errorf("%s: %v, want not found", name, err)
				}
			})
		}
		both(t, live, depScope, func(t *testing.T, d gates.Deployments) {
			if _, err := d.Snapshot(ctx, depScope, "dep_absent"); !errors.Is(err, gates.ErrNotFound) {
				t.Errorf("an absent deployment: %v, want not found", err)
			}
			if _, err := d.Snapshot(ctx, depScope, ""); !errors.Is(err, gates.ErrNotFound) {
				t.Errorf("an empty id: %v, want not found", err)
			}
			if _, err := d.Snapshot(ctx, domain.Scope{}, id); !errors.Is(err, gates.ErrNotFound) {
				t.Errorf("an empty scope: %v, want not found", err)
			}
		})
	})

	t.Run("a peer that answers another scope's row is not trusted", func(t *testing.T) {
		foreign := record(t, otherScope, nil)
		committed := &records{byScope: map[domain.Scope]map[delivery.DeploymentID]delivery.Deployment{otherScope: {foreign.ID(): foreign}}, answerAnyScope: true}
		d, err := adapters.NewDeployments(committed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Snapshot(ctx, depScope, string(foreign.ID())); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("a foreign row answered a gate: %v", err)
		}
		// The transaction row is filtered by the statement, and a row that
		// nevertheless carries another scope is refused as well.
		row := rowOf(foreign)
		tx := &depTx{rows: map[string]depRow{rowKey(depScope, foreign.ID()): row}}
		if _, err := adapters.BindDeployments(fakeTx{q: tx}).Snapshot(ctx, depScope, string(foreign.ID())); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("a foreign row answered a gate in the transaction: %v", err)
		}
	})

	t.Run("a cleaned-up deployment is refused", func(t *testing.T) {
		gone := t0.Add(time.Hour)
		for name, dep := range map[string]delivery.Deployment{
			"cleanup requested": record(t, depScope, cleanupRequested),
			"cleanup done": record(t, depScope, func(r *delivery.DeploymentRecord) {
				cleanupRequested(r)
				r.CleanupState = delivery.CleanupDone
			}),
			"cleanup failed": record(t, depScope, func(r *delivery.DeploymentRecord) {
				cleanupRequested(r)
				r.CleanupState = delivery.CleanupFailed
			}),
			"deleted": record(t, depScope, func(r *delivery.DeploymentRecord) { r.DeletedAt = &gone }),
		} {
			both(t, dep, depScope, func(t *testing.T, d gates.Deployments) {
				if _, err := d.Snapshot(ctx, depScope, string(dep.ID())); !errors.Is(err, gates.ErrNotFound) {
					t.Errorf("%s: %v, want not found", name, err)
				}
			})
		}
	})

	t.Run("an unreadable record is unavailable and never a grant", func(t *testing.T) {
		boom := errors.New("connection reset")
		d, err := adapters.NewDeployments(&records{err: boom})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Snapshot(ctx, depScope, id); !errors.Is(err, gates.ErrUnavailable) || errors.Is(err, gates.ErrNotFound) {
			t.Errorf("committed outage: %v, want unavailable", err)
		}
		tx := &depTx{failure: boom}
		if _, err := adapters.BindDeployments(fakeTx{q: tx}).Snapshot(ctx, depScope, id); !errors.Is(err, gates.ErrUnavailable) || errors.Is(err, gates.ErrNotFound) {
			t.Errorf("transaction outage: %v, want unavailable", err)
		}
		// A record without its digest is inconsistent, not a binding.
		row := rowOf(live)
		row.values[3] = ""
		tx = &depTx{rows: map[string]depRow{rowKey(depScope, live.ID()): row}}
		if _, err := adapters.BindDeployments(fakeTx{q: tx}).Snapshot(ctx, depScope, id); !errors.Is(err, gates.ErrUnavailable) {
			t.Errorf("a record without a digest: %v, want unavailable", err)
		}
	})

	t.Run("the binder reads through the deciding transaction, scoped by tenant and project", func(t *testing.T) {
		tx := &depTx{rows: map[string]depRow{rowKey(depScope, live.ID()): rowOf(live)}}
		binder := func(tx pgx.Tx) gates.Deployments { return adapters.BindDeployments(tx) }
		if _, err := binder(fakeTx{q: tx}).Snapshot(ctx, depScope, id); err != nil {
			t.Fatal(err)
		}
		if len(tx.queries) != 1 {
			t.Fatalf("%d statements, want the one scoped read in the transaction", len(tx.queries))
		}
		if q := tx.queries[0]; q[0] != string(depScope.TenantID) || q[1] != string(depScope.ProjectID) || q[2] != id {
			t.Fatalf("read args %v, want tenant, project and id of the gate", q)
		}
		if s := tx.sql[0]; strings.Contains(strings.ToUpper(s), "FOR UPDATE") || strings.Contains(strings.ToUpper(s), "FOR SHARE") {
			t.Fatalf("the read takes a row lock that could precede the execution locks: %s", s)
		}
		// Each transaction is read separately: a cleanup committed after the
		// first is seen by the next one.
		cleaned := record(t, depScope, cleanupRequested)
		next := &depTx{rows: map[string]depRow{rowKey(depScope, live.ID()): rowOf(cleaned)}}
		if _, err := binder(fakeTx{q: next}).Snapshot(ctx, depScope, id); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("a later transaction bound a cleaned-up deployment: %v", err)
		}
	})

	t.Run("without its collaborators nothing is built or granted", func(t *testing.T) {
		if _, err := adapters.NewDeployments(nil); err == nil {
			t.Error("a deployment adapter without a reader was built")
		}
		if _, err := adapters.NewTxDeployments(nil); err == nil {
			t.Error("a transaction reader without a transaction was built")
		}
		closed := adapters.BindDeployments(nil)
		if _, err := closed.Snapshot(ctx, depScope, id); err == nil {
			t.Error("a binder over no transaction answered a snapshot")
		}
	})
}
