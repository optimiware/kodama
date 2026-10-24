package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters"
	"github.com/optimiware/kodama/internal/storage"
)

// Admitter creates a run inside a caller-owned tenant transaction. It is
// satisfied by *execution/postgres.Store, whose AdmitInTx is the same path the
// keyed HTTP create uses.
type Admitter interface {
	AdmitInTx(ctx context.Context, tx pgx.Tx, plan *execution.AdmissionPlan) (*execution.Run, bool, error)
}

// Ledger implements adapters.Ledger: the admission ledger and run creation in
// one tenant transaction per key.
//
// Atomicity. Admit and Decide both take the key's advisory lock first (a
// transaction-scoped lock, so they queue behind each other and it is released
// by commit or rollback). Under the lock the key is read; only an undecided key
// proceeds: Admit creates the run and inserts the ledger row in the same
// transaction, Decide inserts the refusal row. The row is the decision: its
// check constraint forbids a row that names both a run and a refusal, its
// primary key forbids a second decision, and no role may update or delete it.
// A refusal therefore commits only if no run did, and once it has committed
// every later Admit finds it under the lock and creates nothing, however long
// the caller stalled between planning and calling.
type Ledger struct {
	db       storage.TxBeginner
	admitter Admitter
}

var _ adapters.Ledger = (*Ledger)(nil)

// NewLedger requires the runtime database handle and the run admitter.
func NewLedger(db storage.TxBeginner, admitter Admitter) (*Ledger, error) {
	if db == nil || admitter == nil {
		return nil, errors.New("gitwebhook/postgres: a database and a run admitter are required")
	}
	return &Ledger{db: db, admitter: admitter}, nil
}

// keyLockTimeout (milliseconds) bounds how long a caller waits for the key's
// lock (and for any row lock run admission takes) before reporting an outage.
const keyLockTimeout = "10000"

func (l *Ledger) inScope(ctx context.Context, op string, scope domain.Scope, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("%w: %v", gw.ErrInvalid, err)
	}
	err := storage.WithTenant(ctx, l.db, scope.TenantID, fn)
	if err != nil && execution.IsBusinessError(err) {
		return err // run admission's own classification passes through
	}
	return classify(op, err)
}

func lockKey(ctx context.Context, tx pgx.Tx, scope domain.Scope, key string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config('lock_timeout', $1, true)`, keyLockTimeout); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1, 0))`,
		"git-trigger-admission:"+string(scope.TenantID)+"/"+string(scope.ProjectID)+"/"+key)
	return err
}

func validKey(key string) error {
	if key == "" || len(key) > 512 {
		return fmt.Errorf("%w: the idempotency key must be 1..512 bytes", gw.ErrInvalid)
	}
	return nil
}

func readEntry(ctx context.Context, tx pgx.Tx, scope domain.Scope, key string) (adapters.Entry, bool, error) {
	var run, reason *string
	err := tx.QueryRow(ctx, `SELECT run_id, refusal_reason FROM git_trigger_admissions
WHERE tenant_id = $1 AND project_id = $2 AND idempotency_key = $3`, string(scope.TenantID), string(scope.ProjectID), key).Scan(&run, &reason)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return adapters.Entry{}, false, nil
	case err != nil:
		return adapters.Entry{}, false, err
	}
	var e adapters.Entry
	if run != nil {
		e.RunID = domain.RunID(*run)
	}
	if reason != nil {
		e.Refusal = *reason
	}
	return e, true, nil
}

// Lookup implements adapters.Ledger.
func (l *Ledger) Lookup(ctx context.Context, scope domain.Scope, key string) (adapters.Entry, bool, error) {
	if err := validKey(key); err != nil {
		return adapters.Entry{}, false, err
	}
	var e adapters.Entry
	var found bool
	err := l.inScope(ctx, "look up admission", scope, func(ctx context.Context, tx pgx.Tx) (err error) {
		e, found, err = readEntry(ctx, tx, scope, key)
		return err
	})
	return e, found, err
}

// Admit implements adapters.Ledger.
func (l *Ledger) Admit(ctx context.Context, scope domain.Scope, key string, plan *execution.AdmissionPlan) (adapters.Admission, error) {
	if err := validKey(key); err != nil {
		return adapters.Admission{}, err
	}
	if plan == nil || plan.Aggregate == nil || plan.Aggregate.Run == nil || plan.Aggregate.Run.Scope != scope {
		return adapters.Admission{}, fmt.Errorf("%w: the admission plan is not for this scope", gw.ErrInvalid)
	}
	var out adapters.Admission
	err := l.inScope(ctx, "admit run", scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockKey(ctx, tx, scope, key); err != nil {
			return err
		}
		e, found, err := readEntry(ctx, tx, scope, key)
		if err != nil {
			return err
		}
		if found {
			if e.RunID == "" {
				return gw.ErrAdmissionClosed
			}
			out = adapters.Admission{RunID: e.RunID}
			return nil
		}
		run, _, err := l.admitter.AdmitInTx(ctx, tx, plan)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO git_trigger_admissions (tenant_id, project_id, idempotency_key, run_id) VALUES ($1, $2, $3, $4)`,
			string(scope.TenantID), string(scope.ProjectID), key, string(run.ID)); err != nil {
			return err
		}
		out = adapters.Admission{RunID: run.ID, Created: true}
		return nil
	})
	if err != nil {
		return adapters.Admission{}, err
	}
	return out, nil
}

// Decide implements adapters.Ledger.
func (l *Ledger) Decide(ctx context.Context, scope domain.Scope, key, reason string) (adapters.Entry, error) {
	if err := validKey(key); err != nil {
		return adapters.Entry{}, err
	}
	if reason == "" || len(reason) > 128 {
		return adapters.Entry{}, fmt.Errorf("%w: a refusal reason must be 1..128 bytes", gw.ErrInvalid)
	}
	var out adapters.Entry
	err := l.inScope(ctx, "decide refusal", scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockKey(ctx, tx, scope, key); err != nil {
			return err
		}
		e, found, err := readEntry(ctx, tx, scope, key)
		if err != nil {
			return err
		}
		if found {
			out = e
			return nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO git_trigger_admissions (tenant_id, project_id, idempotency_key, refusal_reason) VALUES ($1, $2, $3, $4)`,
			string(scope.TenantID), string(scope.ProjectID), key, reason); err != nil {
			return err
		}
		out = adapters.Entry{Refusal: reason}
		return nil
	})
	if err != nil {
		return adapters.Entry{}, err
	}
	return out, nil
}
