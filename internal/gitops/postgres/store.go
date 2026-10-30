// Package postgres is the PostgreSQL adapter of the durable state of
// internal/gitops (migration 016): backup runs and their leases, the selected
// recovery with its completed steps and operator resolutions, and the prune
// intents shared with hold acquisition. Every statement runs inside
// storage.WithTenant under the non-owner runtime role, carries an explicit
// tenant predicate and is further constrained by forced row level security.
// A lease and an intent expire on the clock of Options.Now when one is given and
// on the database's now() otherwise (the default), which is the one clock every
// control-plane instance shares: with it instances need no synchronized clocks.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/storage"
)

// DB is satisfied by *storage.Pool and *pgxpool.Pool.
type DB interface {
	storage.TxBeginner
}

// Options configure a Store.
type Options struct {
	// Now is the clock of leases and prune intents. Nil (the default) uses the
	// database's now(); a test injects one. Instances sharing a store with Now set
	// must agree to within a small fraction of the lease and intent lifetimes.
	Now func() time.Time
}

// Store implements the durable ports of gitops over PostgreSQL.
type Store struct {
	db   DB
	opts Options
}

// NewStore requires a database.
func NewStore(db DB, opts Options) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("%w: a database is required", gitops.ErrNotConfigured)
	}
	return &Store{db: db, opts: opts}, nil
}

// at is the instant statements use: nil selects the database's now() through
// COALESCE($n::timestamptz, now()).
func (s *Store) at() *time.Time {
	if s.opts.Now == nil {
		return nil
	}
	t := s.opts.Now().UTC()
	return &t
}

// ts is the SQL spelling of an instant parameter.
func ts(n int) string { return fmt.Sprintf("COALESCE($%d::timestamptz, now())", n) }

// Leases is the gitops.BackupLeases of the store.
func (s *Store) Leases() gitops.BackupLeases { return leases{s} }

// Recoveries is the gitops.RecoveryStore of the store.
func (s *Store) Recoveries() gitops.RecoveryStore { return recoveries{s} }

// Intents is the gitops.PruneIntents of the store.
func (s *Store) Intents() gitops.PruneIntents { return intents{s} }

func (s *Store) do(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(ctx, s.db, tenant, fn)
}

func (s *Store) view(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenantOptions(ctx, s.db, tenant, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn)
}

func constraint(err error, code string) (name string, ok bool) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == code {
		return pe.ConstraintName, true
	}
	return "", false
}

func nstr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func sval(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ---- backup leases ----

type leases struct{ s *Store }

var _ gitops.BackupLeases = leases{}

// Acquire abandons the tenant's active run whose lease expired and starts this
// one in the same transaction. The partial unique index admits one active run
// per tenant, so two instances racing for the lease cannot both win.
func (l leases) Acquire(ctx context.Context, tenant domain.TenantID, backupID string, ttl time.Duration) error {
	now := l.s.at()
	err := l.s.do(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE git_backup_runs SET state = 'failed', updated_at = GREATEST(`+ts(2)+`, started_at)
			WHERE tenant_id = $1 AND state = 'active' AND lease_expires_at <= `+ts(2), string(tenant), now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO git_backup_runs (tenant_id, backup_id, lease_expires_at, started_at, updated_at)
			VALUES ($1, $2, `+ts(3)+` + $4::bigint * interval '1 microsecond', `+ts(3)+`, `+ts(3)+`)`,
			string(tenant), backupID, now, ttl.Microseconds())
		return err
	})
	if name, ok := constraint(err, "23505"); ok {
		if name == "git_backup_runs_one_active" {
			return gitops.ErrBackupInProgress
		}
		return fmt.Errorf("gitops/postgres: backup %s already has a run: %w", backupID, err)
	}
	return err
}

func (l leases) Renew(ctx context.Context, tenant domain.TenantID, backupID string, ttl time.Duration) error {
	now := l.s.at()
	return l.s.do(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE git_backup_runs SET lease_expires_at = `+ts(3)+` + $4::bigint * interval '1 microsecond', updated_at = GREATEST(`+ts(3)+`, started_at)
			WHERE tenant_id = $1 AND backup_id = $2 AND state = 'active'`, string(tenant), backupID, now, ttl.Microseconds())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return gitops.ErrLeaseLost
		}
		return nil
	})
}

func (l leases) finish(ctx context.Context, tenant domain.TenantID, backupID, state string) (int64, error) {
	now := l.s.at()
	var n int64
	err := l.s.do(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE git_backup_runs SET state = $3, updated_at = GREATEST(`+ts(4)+`, started_at)
			WHERE tenant_id = $1 AND backup_id = $2 AND state = 'active'`, string(tenant), backupID, state, now)
		n = tag.RowsAffected()
		return err
	})
	return n, err
}

func (l leases) Complete(ctx context.Context, tenant domain.TenantID, backupID string) error {
	n, err := l.finish(ctx, tenant, backupID, "complete")
	if err == nil && n != 1 {
		return gitops.ErrLeaseLost
	}
	return err
}

func (l leases) Fail(ctx context.Context, tenant domain.TenantID, backupID string) error {
	_, err := l.finish(ctx, tenant, backupID, "failed")
	return err
}

func (l leases) State(ctx context.Context, tenant domain.TenantID, backupID string) (gitops.BackupState, bool, error) {
	var state string
	err := l.s.view(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM git_backup_runs WHERE tenant_id = $1 AND backup_id = $2`, string(tenant), backupID).Scan(&state)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return gitops.BackupState(state), true, nil
}

// ---- prune intents ----

type intents struct{ s *Store }

var _ gitops.PruneIntents = intents{}

// Declare reserves the ref at its tip for the owner. The row is keyed by
// (repository, ref): the owner's repeat extends it (and may name a new tip), and
// an expired or released reservation of anyone is replaced. Another owner's live
// reservation is ErrPruneBusy; a project other than the repository's is an error,
// never a silent success.
func (i intents) Declare(ctx context.Context, scope domain.Scope, repositoryID, ref, tip, owner string, ttl time.Duration) error {
	now := i.s.at()
	return i.s.do(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO git_prune_intents (tenant_id, project_id, repository_id, ref, tip, owner, expires_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, `+ts(7)+` + $8::bigint * interval '1 microsecond', `+ts(7)+`, `+ts(7)+`)
			ON CONFLICT (tenant_id, repository_id, ref) DO UPDATE
			SET tip = EXCLUDED.tip, owner = EXCLUDED.owner, expires_at = EXCLUDED.expires_at, released_at = NULL,
			    updated_at = GREATEST(EXCLUDED.updated_at, git_prune_intents.created_at)
			WHERE git_prune_intents.project_id = EXCLUDED.project_id
			  AND (git_prune_intents.released_at IS NOT NULL OR git_prune_intents.expires_at <= `+ts(7)+` OR git_prune_intents.owner = EXCLUDED.owner)`,
			string(scope.TenantID), string(scope.ProjectID), repositoryID, ref, tip, owner, now, ttl.Microseconds())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		var project string
		if err := tx.QueryRow(ctx, `SELECT project_id FROM git_prune_intents WHERE tenant_id = $1 AND repository_id = $2 AND ref = $3`,
			string(scope.TenantID), repositoryID, ref).Scan(&project); err != nil {
			return err
		}
		if project != string(scope.ProjectID) {
			return fmt.Errorf("gitops/postgres: repository %s belongs to another project", repositoryID)
		}
		return gitops.ErrPruneBusy
	})
}

// Release drops the owner's reservation; another owner's is untouched.
func (i intents) Release(ctx context.Context, scope domain.Scope, repositoryID, ref, owner string) error {
	now := i.s.at()
	return i.s.do(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE git_prune_intents SET released_at = `+ts(6)+`, updated_at = GREATEST(`+ts(6)+`, created_at)
			WHERE tenant_id = $1 AND project_id = $2 AND repository_id = $3 AND ref = $4 AND owner = $5 AND released_at IS NULL`,
			string(scope.TenantID), string(scope.ProjectID), repositoryID, ref, owner, now)
		return err
	})
}

func (i intents) Conflicts(ctx context.Context, scope domain.Scope, repositoryID, ref, commit string) (bool, error) {
	now := i.s.at()
	var conflict bool
	err := i.s.view(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM git_prune_intents
			WHERE tenant_id = $1 AND project_id = $2 AND repository_id = $3 AND released_at IS NULL AND expires_at > `+ts(6)+`
			  AND (ref = $4 OR tip = $5))`, string(scope.TenantID), string(scope.ProjectID), repositoryID, ref, commit, now).Scan(&conflict)
	})
	return conflict, err
}

// ---- recoveries ----

type recoveries struct{ s *Store }

var _ gitops.RecoveryStore = recoveries{}

type conflictJSON struct {
	Ref      string `json:"ref"`
	Have     string `json:"have"`
	Want     string `json:"want"`
	Relation string `json:"relation"`
}

const recoveryColumns = `id, snapshot_id, backup_id, status, volume_restored`

func scanRecovery(tenant domain.TenantID, row pgx.Row) (gitops.RecoveryRecord, error) {
	var id, status string
	var snapshot, backup *string
	var restored bool
	if err := row.Scan(&id, &snapshot, &backup, &status, &restored); err != nil {
		return gitops.RecoveryRecord{}, err
	}
	return gitops.RecoveryRecord{ID: id, Tenant: tenant, SnapshotID: sval(snapshot), BackupID: sval(backup), Status: gitops.RecoveryStatus(status),
		VolumeRestored: restored, Replays: map[string]gitops.ReplayResult{}}, nil
}

func loadChildren(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, rec *gitops.RecoveryRecord) error {
	rows, err := tx.Query(ctx, `SELECT repository_id, created, unchanged, conflicts FROM git_recovery_replays
		WHERE tenant_id = $1 AND recovery_id = $2 ORDER BY repository_id`, string(tenant), rec.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var repo string
		var created, unchanged int
		var raw []byte
		if err := rows.Scan(&repo, &created, &unchanged, &raw); err != nil {
			rows.Close()
			return err
		}
		var cs []conflictJSON
		if err := json.Unmarshal(raw, &cs); err != nil {
			rows.Close()
			return fmt.Errorf("gitops/postgres: stored replay conflicts: %w", err)
		}
		res := gitops.ReplayResult{Created: created, Unchanged: unchanged}
		for _, c := range cs {
			res.Conflicts = append(res.Conflicts, gitops.RefConflict{Ref: c.Ref, Have: c.Have, Want: c.Want, Relation: gitops.ConflictRelation(c.Relation)})
		}
		rec.Replays[repo] = res
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT repository_id, ref, kind, expected, actual, resolved_by, reason, resolved_at FROM git_recovery_resolutions
		WHERE tenant_id = $1 AND recovery_id = $2 ORDER BY resolved_at, repository_id, ref, kind, actual`, string(tenant), rec.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r gitops.DiscrepancyResolution
		var kind string
		if err := rows.Scan(&r.Repository, &r.Ref, &kind, &r.Expected, &r.Actual, &r.By, &r.Reason, &r.At); err != nil {
			return err
		}
		r.Kind, r.At = gitops.DiscrepancyKind(kind), r.At.UTC()
		rec.Resolutions = append(rec.Resolutions, r)
	}
	return rows.Err()
}

// Begin records the recovery or returns the active one for the identical
// source. The partial unique index admits one active recovery per tenant; an
// instance that loses the insert race re-reads the winner.
func (r recoveries) Begin(ctx context.Context, tenant domain.TenantID, id string, src gitops.RestoreSource, now time.Time) (gitops.RecoveryRecord, error) {
	for range 3 {
		var rec gitops.RecoveryRecord
		err := r.s.do(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
			cur, err := scanRecovery(tenant, tx.QueryRow(ctx, `SELECT `+recoveryColumns+` FROM git_recoveries
				WHERE tenant_id = $1 AND status = 'active' FOR UPDATE`, string(tenant)))
			switch {
			case err == nil:
				if cur.SnapshotID != src.SnapshotID || cur.BackupID != src.BackupID {
					return gitops.ErrRecoveryActive
				}
				rec = cur
			case errors.Is(err, pgx.ErrNoRows):
				if _, err := tx.Exec(ctx, `INSERT INTO git_recoveries (tenant_id, id, snapshot_id, backup_id, volume_restored, started_at, updated_at)
					VALUES ($1, $2, $3, $4, $5, $6, $6)`, string(tenant), id, nstr(src.SnapshotID), nstr(src.BackupID), src.SnapshotID == "", now.UTC()); err != nil {
					return err
				}
				rec = gitops.RecoveryRecord{ID: id, Tenant: tenant, SnapshotID: src.SnapshotID, BackupID: src.BackupID, Status: gitops.RecoveryActive,
					VolumeRestored: src.SnapshotID == "", Replays: map[string]gitops.ReplayResult{}}
				return nil
			default:
				return err
			}
			return loadChildren(ctx, tx, tenant, &rec)
		})
		if name, ok := constraint(err, "23505"); ok && name == "git_recoveries_one_active" {
			continue // another instance began a recovery between the read and the insert
		}
		return rec, err
	}
	return gitops.RecoveryRecord{}, gitops.ErrRecoveryActive
}

func (r recoveries) Current(ctx context.Context, tenant domain.TenantID) (gitops.RecoveryRecord, bool, error) {
	var rec gitops.RecoveryRecord
	err := r.s.view(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = scanRecovery(tenant, tx.QueryRow(ctx, `SELECT `+recoveryColumns+` FROM git_recoveries WHERE tenant_id = $1 ORDER BY seq DESC LIMIT 1`, string(tenant)))
		if err != nil {
			return err
		}
		return loadChildren(ctx, tx, tenant, &rec)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gitops.RecoveryRecord{}, false, nil
	}
	if err != nil {
		return gitops.RecoveryRecord{}, false, err
	}
	return rec, true, nil
}

var errNoRecovery = errors.New("gitops/postgres: no such recovery")

func (r recoveries) MarkVolumeRestored(ctx context.Context, tenant domain.TenantID, id string, now time.Time) error {
	return r.s.do(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE git_recoveries SET volume_restored = true, updated_at = GREATEST($3, started_at)
			WHERE tenant_id = $1 AND id = $2 AND status = 'active'`, string(tenant), id, now.UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errNoRecovery
		}
		return nil
	})
}

// MarkReplayed records the repository's completed initialization and replay. The
// first record stands: a repeat (a crash after the replay, before the mark) keeps
// it. The database writes it only while the recovery is active.
func (r recoveries) MarkReplayed(ctx context.Context, tenant domain.TenantID, id, repositoryID string, res gitops.ReplayResult, now time.Time) error {
	cs := make([]conflictJSON, 0, len(res.Conflicts))
	for _, c := range res.Conflicts {
		cs = append(cs, conflictJSON{Ref: c.Ref, Have: c.Have, Want: c.Want, Relation: string(c.Relation)})
	}
	raw, err := json.Marshal(cs)
	if err != nil {
		return err
	}
	return r.s.do(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO git_recovery_replays (tenant_id, recovery_id, repository_id, created, unchanged, conflicts, completed_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (tenant_id, recovery_id, repository_id) DO NOTHING`,
			string(tenant), id, repositoryID, res.Created, res.Unchanged, raw, now.UTC())
		return err
	})
}

// AuditResolution is the audit action of an accepted discrepancy.
const AuditResolution = "git.recovery.discrepancy_resolved"

// Resolve records the operator's resolution and its audit row in one
// transaction. A repeat of the same resolution changes nothing and audits
// nothing.
func (r recoveries) Resolve(ctx context.Context, tenant domain.TenantID, id string, res gitops.DiscrepancyResolution) error {
	return r.s.do(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO git_recovery_resolutions (tenant_id, recovery_id, repository_id, ref, kind, expected, actual, resolved_by, reason, resolved_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (tenant_id, recovery_id, repository_id, ref, kind, actual) DO NOTHING`,
			string(tenant), id, res.Repository, res.Ref, string(res.Kind), res.Expected, res.Actual, res.By, res.Reason, res.At.UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		md, err := json.Marshal(map[string]any{"repositoryId": res.Repository, "ref": res.Ref, "kind": string(res.Kind), "expected": res.Expected, "actual": res.Actual})
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%s|%s", tenant, id, res.Repository, res.Ref, res.Kind, res.Actual)))
		_, err = tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, reason, metadata)
			VALUES ($1, $2, $3, 'user', $4, $5, 'git_recovery', $6, $7, $8)`,
			string(tenant), "rsl_"+hex.EncodeToString(sum[:])[:40], res.At.UTC(), res.By, AuditResolution, id, res.Reason, md)
		return err
	})
}

// Complete ends the recovery; a repeat on a complete recovery succeeds.
func (r recoveries) Complete(ctx context.Context, tenant domain.TenantID, id string, now time.Time) error {
	return r.s.do(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE git_recoveries SET status = 'complete', completed_at = GREATEST($3, started_at), updated_at = GREATEST($3, started_at)
			WHERE tenant_id = $1 AND id = $2 AND status = 'active'`, string(tenant), id, now.UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM git_recoveries WHERE tenant_id = $1 AND id = $2`, string(tenant), id).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errNoRecovery
			}
			return err
		}
		return nil
	})
}
