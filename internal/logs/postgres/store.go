// Package postgres is the PostgreSQL adapter of the log module: tenant-scoped
// persistence of capture records, their fenced collection state and verified
// artifact linkage.
//
// Every statement runs inside storage.WithTenant under the non-owner runtime
// role, carries explicit tenant and project predicates and is additionally
// confined by forced row level security and composite foreign keys. Claims are
// serialized per capture by the row lock and per (attempt, source, generation)
// by a unique key. No object store, Kubernetes or registry call happens in a
// transaction: the controller records intent first, performs the external
// effect, then commits the outcome under its fence.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/storage"
)

// Config configures a Store.
type Config struct {
	// DB begins transactions; use the non-owner runtime pool.
	DB storage.TxBeginner
}

// Store implements logs.Store.
type Store struct{ db storage.TxBeginner }

var _ logs.Store = (*Store)(nil)

// NewStore validates cfg.
func NewStore(cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, errors.New("logs/postgres: DB is required")
	}
	return &Store{db: cfg.DB}, nil
}

func (s *Store) write(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenant(ctx, s.db, tenant, fn))
}

func (s *Store) read(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenantOptions(ctx, s.db, tenant, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn))
}

// classify maps database conditions to module classes. Entity sentinels pass
// through unchanged; constraint violations of the log invariants are conflicts.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "40001", "40P01", "55P03", "57014", "53300":
			return fmt.Errorf("%w: %v", logs.ErrUnavailable, err)
		case "23503": // foreign key: the attempt or artifact is not this scope's
			return logs.ErrNotFound
		case "23514", "23505":
			return fmt.Errorf("%w: %s", logs.ErrConflict, pg.ConstraintName)
		}
	}
	return err
}

func newAuditID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("logs/postgres: random source failed: %v", err))
	}
	return "aud_" + hex.EncodeToString(b)
}

// audit records a transition with safe metadata only: identifiers, states and
// classes, never digests of content, keys, URLs or log text.
func audit(ctx context.Context, tx pgx.Tx, r *logs.Record, a logs.Audit, now time.Time) error {
	meta := map[string]any{"attemptId": string(r.Attempt), "source": string(r.Source), "generation": r.Generation, "state": string(r.State)}
	if r.Failure != "" {
		meta["failure"] = string(r.Failure)
	}
	if r.Link != nil {
		meta["completeness"] = string(r.Link.Completeness)
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	var req *string
	if a.RequestID != "" {
		req = &a.RequestID
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, request_id, metadata)
VALUES ($1, $2, $3, $4, 'system', 'logs-controller', $5, 'log_record', $6, $7, $8, $9)`,
		string(r.Scope.TenantID), newAuditID(), string(r.Scope.ProjectID), now, a.Action, string(r.ID), int64(r.Version), req, b)
	return err
}

const columns = `id, run_id, attempt_id, step_id, source, generation, state, claim_token, claim_fence, claim_until, claims, failure_class,
snap_log_name, snap_meta_name, snap_digest, snap_size, snap_completeness, snap_reasons,
log_artifact_id, log_digest, log_size, meta_artifact_id, meta_digest, meta_size, completeness, reasons,
removed_at, version, created_at, updated_at`

func scan(row pgx.Row, scope domain.Scope) (*logs.Record, error) {
	var (
		id, run, attempt, step, source, state              string
		generation, claims                                 int
		claimToken, failure                                *string
		claimFence, version                                int64
		claimUntil, removedAt                              *time.Time
		snapLog, snapMeta, snapDigest, snapComp            *string
		snapSize                                           *int64
		snapReasons                                        []string
		logID, logDigest, metaID, metaDigest, completeness *string
		logSize, metaSize                                  *int64
		reasons                                            []string
		created, updated                                   time.Time
	)
	if err := row.Scan(&id, &run, &attempt, &step, &source, &generation, &state, &claimToken, &claimFence, &claimUntil, &claims, &failure,
		&snapLog, &snapMeta, &snapDigest, &snapSize, &snapComp, &snapReasons,
		&logID, &logDigest, &logSize, &metaID, &metaDigest, &metaSize, &completeness, &reasons,
		&removedAt, &version, &created, &updated); err != nil {
		return nil, err
	}
	r := &logs.Record{Scope: scope, ID: logs.RecordID(id), Attempt: domain.AttemptID(attempt), Run: domain.RunID(run), Step: step,
		Source: logs.Source(source), Generation: generation, State: logs.State(state), Claims: claims,
		Claim:     logs.Claim{Fence: claimFence},
		Failure:   logs.FailureClass(deref(failure)),
		RemovedAt: utc(removedAt), Version: domain.Version(version), CreatedAt: created.UTC(), UpdatedAt: updated.UTC()}
	if claimToken != nil {
		r.Claim.Token = *claimToken
	}
	if claimUntil != nil {
		r.Claim.Until = claimUntil.UTC()
	}
	if snapDigest != nil {
		r.Snapshot = &logs.Snapshot{LogName: deref(snapLog), MetaName: deref(snapMeta), Digest: *snapDigest, Size: derefInt(snapSize),
			Statement: logs.Statement{Completeness: logs.Completeness(deref(snapComp)), Reasons: toReasons(snapReasons)}}
	}
	if logID != nil {
		l := &logs.Link{Log: logs.ArtifactRef{ID: domain.ArtifactID(*logID), Digest: deref(logDigest), Size: derefInt(logSize)},
			Statement: logs.Statement{Completeness: logs.Completeness(deref(completeness)), Reasons: toReasons(reasons)}}
		if metaID != nil {
			l.Meta = &logs.ArtifactRef{ID: domain.ArtifactID(*metaID), Digest: deref(metaDigest), Size: derefInt(metaSize)}
		}
		r.Link = l
	}
	return r, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func toReasons(in []string) []logs.Reason {
	out := make([]logs.Reason, 0, len(in))
	for _, s := range in {
		out = append(out, logs.Reason(s))
	}
	return out
}

func fromReasons(in []logs.Reason) []string {
	out := make([]string, 0, len(in))
	for _, r := range in {
		out = append(out, string(r))
	}
	return out
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Ensure implements logs.Store.
func (s *Store) Ensure(ctx context.Context, tmpl logs.Record) (*logs.Record, error) {
	var out *logs.Record
	err := s.write(ctx, tmpl.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO log_records (tenant_id, project_id, id, run_id, attempt_id, step_id, source, generation, state, version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, 1, 'pending', 1, $8, $8) ON CONFLICT ON CONSTRAINT log_records_capture_unique DO NOTHING`,
			string(tmpl.Scope.TenantID), string(tmpl.Scope.ProjectID), string(tmpl.ID), string(tmpl.Run), string(tmpl.Attempt), tmpl.Step,
			string(tmpl.Source), tmpl.CreatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			if err := audit(ctx, tx, &tmpl, logs.Audit{Action: "log.created"}, tmpl.CreatedAt); err != nil {
				return err
			}
		}
		r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM log_records WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3 AND source = $4
ORDER BY generation DESC LIMIT 1`, string(tmpl.Scope.TenantID), string(tmpl.Scope.ProjectID), string(tmpl.Attempt), string(tmpl.Source)), tmpl.Scope)
		if err != nil {
			return err
		}
		out = r
		return nil
	})
	return out, err
}

// Get implements logs.Store.
func (s *Store) Get(ctx context.Context, scope domain.Scope, id logs.RecordID) (*logs.Record, error) {
	var out *logs.Record
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM log_records WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(id)), scope)
		if errors.Is(err, pgx.ErrNoRows) {
			return logs.ErrNotFound
		}
		out = r
		return err
	})
	return out, err
}

// ForAttempt implements logs.Store.
func (s *Store) ForAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) ([]*logs.Record, error) {
	var out []*logs.Record
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+columns+` FROM log_records WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3
ORDER BY source, generation`, string(scope.TenantID), string(scope.ProjectID), string(attempt))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scan(rows, scope)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// Create implements logs.Store. The guard trigger admits a generation above 1
// only after the previous one lost its frozen bytes.
func (s *Store) Create(ctx context.Context, r *logs.Record) (*logs.Record, error) {
	var out *logs.Record
	err := s.write(ctx, r.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO log_records (tenant_id, project_id, id, run_id, attempt_id, step_id, source, generation, state, version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', 1, $9, $9) ON CONFLICT ON CONSTRAINT log_records_capture_unique DO NOTHING`,
			string(r.Scope.TenantID), string(r.Scope.ProjectID), string(r.ID), string(r.Run), string(r.Attempt), r.Step,
			string(r.Source), r.Generation, r.CreatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			if err := audit(ctx, tx, r, logs.Audit{Action: "log.created"}, r.CreatedAt); err != nil {
				return err
			}
		}
		got, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM log_records WHERE tenant_id = $1 AND project_id = $2 AND attempt_id = $3 AND source = $4 AND generation = $5`,
			string(r.Scope.TenantID), string(r.Scope.ProjectID), string(r.Attempt), string(r.Source), r.Generation), r.Scope)
		out = got
		return err
	})
	return out, err
}

// Update implements logs.Store: lock the row, run the pure mutation, save the
// result with its audit entry in the same transaction. An unchanged result
// writes nothing. A mutation error (a fenced-out collector, an invalid
// transition) rolls everything back.
func (s *Store) Update(ctx context.Context, scope domain.Scope, id logs.RecordID, fn logs.Mutation) (*logs.Record, error) {
	var out *logs.Record
	err := s.write(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM log_records WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`,
			string(scope.TenantID), string(scope.ProjectID), string(id)), scope)
		if errors.Is(err, pgx.ErrNoRows) {
			return logs.ErrNotFound
		}
		if err != nil {
			return err
		}
		before := r.Version
		changed, a, err := fn(r)
		if err != nil {
			return err
		}
		out = r
		if !changed || r.Version == before {
			return nil
		}
		return save(ctx, tx, r, a)
	})
	return out, err
}

func save(ctx context.Context, tx pgx.Tx, r *logs.Record, a logs.Audit) error {
	var (
		snapLog, snapMeta, snapDigest, snapComp            *string
		snapSize                                           *int64
		snapReasons                                        []string
		logID, logDigest, metaID, metaDigest, completeness *string
		logSize, metaSize                                  *int64
		reasons                                            []string
	)
	if r.Snapshot != nil {
		s := r.Snapshot
		snapLog, snapMeta, snapDigest, snapComp = &s.LogName, &s.MetaName, &s.Digest, nullable(string(s.Completeness))
		snapSize, snapReasons = &s.Size, fromReasons(s.Reasons)
	}
	if r.Link != nil {
		l := r.Link
		id, dg, sz, comp := string(l.Log.ID), l.Log.Digest, l.Log.Size, string(l.Completeness)
		logID, logDigest, logSize, completeness, reasons = &id, &dg, &sz, &comp, fromReasons(l.Reasons)
		if l.Meta != nil {
			mid, mdg, msz := string(l.Meta.ID), l.Meta.Digest, l.Meta.Size
			metaID, metaDigest, metaSize = &mid, &mdg, &msz
		}
	}
	var failure *string
	if r.Failure != "" {
		f := string(r.Failure)
		failure = &f
	}
	_, err := tx.Exec(ctx, `UPDATE log_records SET state = $4, claim_token = $5, claim_fence = $6, claim_until = $7, claims = $8, failure_class = $9,
 snap_log_name = $10, snap_meta_name = $11, snap_digest = $12, snap_size = $13, snap_completeness = $14, snap_reasons = $15,
 log_artifact_id = $16, log_digest = $17, log_size = $18, meta_artifact_id = $19, meta_digest = $20, meta_size = $21,
 completeness = $22, reasons = $23, removed_at = $24, version = $25, updated_at = $26
WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(r.Scope.TenantID), string(r.Scope.ProjectID), string(r.ID),
		string(r.State), nullable(r.Claim.Token), r.Claim.Fence, claimUntil(r), r.Claims, failure,
		snapLog, snapMeta, snapDigest, snapSize, snapComp, snapReasons,
		logID, logDigest, logSize, metaID, metaDigest, metaSize, completeness, reasons, r.RemovedAt, int64(r.Version), r.UpdatedAt)
	if err != nil {
		return err
	}
	return audit(ctx, tx, r, a, r.UpdatedAt)
}

func claimUntil(r *logs.Record) *time.Time {
	if r.Claim.Token == "" {
		return nil
	}
	t := r.Claim.Until
	return &t
}
