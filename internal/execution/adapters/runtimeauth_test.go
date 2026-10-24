package adapters

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
)

// recordingTx is a transaction that only records Exec calls.
type recordingTx struct {
	pgx.Tx
	sql  []string
	args [][]any
	tag  pgconn.CommandTag
	err  error
}

func (r *recordingTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.sql = append(r.sql, sql)
	r.args = append(r.args, args)
	return r.tag, r.err
}

// Test_revoke_attempt_grants_bridge: SD-04, IT-03. The bridge runs the runtime
// authority's revocation on the transaction it is handed (never another one),
// scoped by tenant, project and attempt, and reports failures unchanged so the
// owning transaction rolls back.
func Test_revoke_attempt_grants_bridge(t *testing.T) {
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	tx := &recordingTx{tag: pgconn.NewCommandTag("UPDATE 2")}
	n, err := RevokeAttemptGrants(context.Background(), tx, scope, "att_one", now)
	if err != nil || n != 2 {
		t.Fatalf("%d %v", n, err)
	}
	if len(tx.sql) != 1 {
		t.Fatalf("%d statements ran on the supplied transaction", len(tx.sql))
	}
	args := tx.args[0]
	if len(args) != 4 || args[0] != "t_acme" || args[1] != "p_inventory" || args[2] != "att_one" || !args[3].(time.Time).Equal(now) {
		t.Fatalf("the statement is not scoped by tenant, project and attempt: %v", args)
	}
	if args[3].(time.Time).Location() != time.UTC {
		t.Fatal("the revocation time is not UTC")
	}

	failing := &recordingTx{err: errors.New("connection lost")}
	if _, err := RevokeAttemptGrants(context.Background(), failing, scope, "att_one", now); err == nil || !errors.Is(err, failing.err) {
		t.Fatalf("a failure must reach the owning transaction unchanged: %v", err)
	}
}
