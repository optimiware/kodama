// Package postgres is the PostgreSQL adapter of gitpush: the push-record tables
// (migration 009) behind gitpush.Store. Every statement runs inside
// storage.WithTenant under the non-owner runtime role, carries an explicit
// tenant predicate and is further constrained by forced row level security.
// Audit rows and the git.refs_updated outbox event are written in the same
// transaction as the record change, and the grant transaction of gitauth
// (seed consumption) joins the same pgx transaction. No network or Git call is
// ever made inside a transaction.
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

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/gitauth"
	gitauthpg "github.com/optimiware/kodama/internal/gitauth/postgres"
	"github.com/optimiware/kodama/internal/gitpush"
	"github.com/optimiware/kodama/internal/storage"
)

// DB is satisfied by *storage.Pool and *pgxpool.Pool.
type DB interface {
	storage.TxBeginner
}

// EventAppender is the transaction-local event append of the event bus.
type EventAppender interface {
	Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error)
}

// Options configure a Store.
type Options struct {
	NewID func(prefix string) string
}

// Store implements gitpush.Store.
type Store struct {
	db     DB
	events EventAppender
	opts   Options
}

var _ gitpush.Store = (*Store)(nil)

// NewStore requires a database and an event appender: a settlement without its
// audit and outbox records would be unobservable, so there is no construction
// without them.
func NewStore(db DB, events EventAppender, opts Options) (*Store, error) {
	if db == nil || events == nil {
		return nil, fmt.Errorf("%w: a database and an event appender are required", gitpush.ErrNotConfigured)
	}
	if opts.NewID == nil {
		opts.NewID = func(prefix string) string {
			b := make([]byte, 12)
			if _, err := rand.Read(b); err != nil {
				panic(err)
			}
			return prefix + "_" + hex.EncodeToString(b)
		}
	}
	return &Store{db: db, events: events, opts: opts}, nil
}

// Do implements gitpush.Store. A failure after fn succeeded (the commit) has an
// unknown outcome and is reported as ErrUnavailable.
func (s *Store) Do(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx gitpush.Tx) error) error {
	ran := false
	err := storage.WithTenant(ctx, s.db, tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, s.Bind(tx, tenant)); err != nil {
			return err
		}
		ran = true
		return nil
	})
	if err != nil && ran {
		return errors.Join(gitpush.ErrUnavailable, err)
	}
	return err
}

// View implements gitpush.Store over a read-only transaction.
func (s *Store) View(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx gitpush.Tx) error) error {
	return storage.WithTenantOptions(ctx, s.db, tenant, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, s.Bind(tx, tenant))
	})
}

// Bind adapts a transaction that already carries the tenant context to
// gitpush.Tx: the integration seam of a caller that owns the transaction.
func (s *Store) Bind(tx pgx.Tx, tenant domain.TenantID) gitpush.Tx {
	return &pgTx{s: s, tx: tx, tenant: tenant}
}

type pgTx struct {
	s      *Store
	tx     pgx.Tx
	tenant domain.TenantID
}

var _ gitpush.Tx = (*pgTx)(nil)

// RepositoryState reads the repositories row of 007 under row level security
// with the runtime role's grants (SELECT, and UPDATE on the lifecycle columns,
// which FOR SHARE requires) and holds it against concurrent change.
func (t *pgTx) RepositoryState(ctx context.Context, repositoryID string) (gitpush.RepositoryState, error) {
	var hosting, status, branch string
	var direct bool
	err := t.tx.QueryRow(ctx, `SELECT hosting, status, default_branch, direct_push_to_default FROM repositories
		WHERE tenant_id = $1 AND id = $2 FOR SHARE`, string(t.tenant), repositoryID).Scan(&hosting, &status, &branch, &direct)
	if errors.Is(err, pgx.ErrNoRows) {
		return gitpush.RepositoryState{}, gitpush.ErrNotFound
	}
	if err != nil {
		return gitpush.RepositoryState{}, err
	}
	return gitpush.RepositoryState{Hosting: gitv1.Hosting(hosting), Status: gitv1.RepositoryStatus(status), DefaultBranch: branch, DirectPushToDefault: direct}, nil
}

func (t *pgTx) Grants() gitauth.GrantTx { return gitauthpg.Bind(t.tx, t.tenant) }

func mapErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return fmt.Errorf("%w: %s", gitpush.ErrConflict, pe.ConstraintName)
	}
	return err
}

const recordColumns = `project_id, id, repository_id, receive_id, grant_id, grant_kind, actor_type, actor_id, run_id, attempt_id,
	push_options, repository_empty, empty_report_pending, request_digest, deadline, version, created_at, updated_at`

func (t *pgTx) scanRecords(ctx context.Context, sql string, args ...any) ([]*gitpush.Record, error) {
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*gitpush.Record
	for rows.Next() {
		var (
			project, id, repo, receive, grant, kind, actorType, actorID string
			run, attempt                                                *string
			options                                                     []byte
			empty, pending                                              bool
			digest                                                      string
			deadline, created, updated                                  time.Time
			version                                                     int64
		)
		if err := rows.Scan(&project, &id, &repo, &receive, &grant, &kind, &actorType, &actorID, &run, &attempt,
			&options, &empty, &pending, &digest, &deadline, &version, &created, &updated); err != nil {
			return nil, err
		}
		r := &gitpush.Record{
			ID: id, Scope: domain.Scope{TenantID: t.tenant, ProjectID: domain.ProjectID(project)}, RepositoryID: repo, ReceiveID: receive,
			GrantID: grant, GrantKind: gitv1.GrantKind(kind),
			Actor:           gitv1.Actor{Type: gitv1.ActorType(actorType), ID: actorID, RunID: run, AttemptID: attempt},
			RepositoryEmpty: empty, EmptyReportPending: pending, RequestDigest: digest, Deadline: deadline.UTC(),
			Version: version, CreatedAt: created.UTC(), UpdatedAt: updated.UTC(),
		}
		if err := json.Unmarshal(options, &r.PushOptions); err != nil {
			return nil, fmt.Errorf("gitpush/postgres: stored push options: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for _, r := range out {
		if err := t.loadEntries(ctx, r); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (t *pgTx) loadEntries(ctx context.Context, r *gitpush.Record) error {
	rows, err := t.tx.Query(ctx, `SELECT ref, old_value, new_value, kind, state, reason FROM git_push_entries
		WHERE tenant_id = $1 AND push_id = $2 ORDER BY ordinal`, string(t.tenant), r.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var e gitpush.Entry
		var kind, state string
		var reason *string
		if err := rows.Scan(&e.Ref, &e.Old, &e.New, &kind, &state, &reason); err != nil {
			return err
		}
		e.Kind, e.State = gitv1.UpdateKind(kind), gitv1.EntryState(state)
		if reason != nil {
			e.Reason = *reason
		}
		r.Entries = append(r.Entries, e)
	}
	return rows.Err()
}

func (t *pgTx) one(ctx context.Context, sql string, args ...any) (*gitpush.Record, error) {
	rs, err := t.scanRecords(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	if len(rs) == 0 {
		return nil, gitpush.ErrNotFound
	}
	return rs[0], nil
}

func (t *pgTx) ByID(ctx context.Context, pushID string, lock bool) (*gitpush.Record, error) {
	q := `SELECT ` + recordColumns + ` FROM git_push_records WHERE tenant_id = $1 AND id = $2`
	if lock {
		q += ` FOR UPDATE`
	}
	return t.one(ctx, q, string(t.tenant), pushID)
}

func (t *pgTx) ByReceive(ctx context.Context, repositoryID, receiveID string) (*gitpush.Record, error) {
	return t.one(ctx, `SELECT `+recordColumns+` FROM git_push_records WHERE tenant_id = $1 AND repository_id = $2 AND receive_id = $3`,
		string(t.tenant), repositoryID, receiveID)
}

func status(r *gitpush.Record) string { return string(r.Status()) }

func (t *pgTx) Insert(ctx context.Context, r *gitpush.Record, fx gitpush.Effects) error {
	if r.Scope.TenantID != t.tenant {
		return gitpush.ErrNotFound
	}
	options, err := json.Marshal(append([]string{}, r.PushOptions...))
	if err != nil {
		return err
	}
	if _, err := t.tx.Exec(ctx, `INSERT INTO git_push_records
		(tenant_id, project_id, id, repository_id, receive_id, grant_id, grant_kind, actor_type, actor_id, run_id, attempt_id,
		 push_options, repository_empty, empty_report_pending, request_digest, deadline, status, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		string(t.tenant), string(r.Scope.ProjectID), r.ID, r.RepositoryID, r.ReceiveID, r.GrantID, string(r.GrantKind), string(r.Actor.Type), r.Actor.ID,
		r.Actor.RunID, r.Actor.AttemptID, options, r.RepositoryEmpty, r.EmptyReportPending, r.RequestDigest, r.Deadline, status(r),
		r.Version, r.CreatedAt, r.UpdatedAt); err != nil {
		return mapErr(err)
	}
	n := len(r.Entries)
	positions, refs, olds, news, kinds, states := make([]int32, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	reasons := make([]*string, n)
	for i, e := range r.Entries {
		positions[i], refs[i], olds[i], news[i], kinds[i], states[i] = int32(i), e.Ref, e.Old, e.New, string(e.Kind), string(e.State)
		if e.Reason != "" {
			reason := e.Reason
			reasons[i] = &reason
		}
	}
	if _, err := t.tx.Exec(ctx, `INSERT INTO git_push_entries (tenant_id, push_id, ordinal, project_id, repository_id, ref, old_value, new_value, kind, state, reason)
		SELECT $1, $2, u.ordinal, $3, $4, u.ref, u.old_value, u.new_value, u.kind, u.state, u.reason
		FROM unnest($5::int[], $6::text[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[]) AS u(ordinal, ref, old_value, new_value, kind, state, reason)`,
		string(t.tenant), r.ID, string(r.Scope.ProjectID), r.RepositoryID, positions, refs, olds, news, kinds, states, reasons); err != nil {
		return mapErr(err)
	}
	return t.writeEffects(ctx, r, fx)
}

func (t *pgTx) Save(ctx context.Context, r *gitpush.Record, fx gitpush.Effects) error {
	tag, err := t.tx.Exec(ctx, `UPDATE git_push_records SET status = $3, empty_report_pending = $4, version = $5, updated_at = $6
		WHERE tenant_id = $1 AND id = $2 AND version = $7`,
		string(t.tenant), r.ID, status(r), r.EmptyReportPending, r.Version, r.UpdatedAt, r.Version-1)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("gitpush/postgres: push record %s changed concurrently", r.ID)
	}
	// An operator resolution is recorded before its entry leaves unknown, in this
	// transaction: the entry guard of migration 015 accepts the move only then.
	if err := t.writeResolutions(ctx, r, fx.Resolutions); err != nil {
		return err
	}
	n := len(r.Entries)
	positions, states := make([]int32, n), make([]string, n)
	reasons := make([]*string, n)
	for i, e := range r.Entries {
		positions[i], states[i] = int32(i), string(e.State)
		if e.Reason != "" {
			reason := e.Reason
			reasons[i] = &reason
		}
	}
	if _, err := t.tx.Exec(ctx, `UPDATE git_push_entries e SET state = u.state, reason = u.reason
		FROM unnest($3::int[], $4::text[], $5::text[]) AS u(ordinal, state, reason)
		WHERE e.tenant_id = $1 AND e.push_id = $2 AND e.ordinal = u.ordinal AND (e.state, e.reason) IS DISTINCT FROM (u.state, u.reason)`,
		string(t.tenant), r.ID, positions, states, reasons); err != nil {
		return mapErr(err)
	}
	return t.writeEffects(ctx, r, fx)
}

func (t *pgTx) List(ctx context.Context, scope domain.Scope, repositoryID string, q gitpush.ListQuery) ([]*gitpush.Record, error) {
	var (
		at *time.Time
		id *string
	)
	if q.After != nil {
		a, i := q.After.CreatedAt.UTC(), q.After.ID
		at, id = &a, &i
	}
	return t.scanRecords(ctx, `SELECT `+recordColumns+` FROM git_push_records
		WHERE tenant_id = $1 AND project_id = $2 AND repository_id = $3
		  AND ($4::timestamptz IS NULL OR (created_at, id) < ($4::timestamptz, $5::text))
		ORDER BY created_at DESC, id DESC LIMIT $6`, string(t.tenant), string(scope.ProjectID), repositoryID, at, id, q.Limit)
}

func (t *pgTx) Unsettled(ctx context.Context, repositoryID, afterID string, limit int) ([]*gitpush.Record, error) {
	return t.scanRecords(ctx, `SELECT `+recordColumns+` FROM git_push_records r
		WHERE tenant_id = $1 AND repository_id = $2 AND id > $3
		  AND EXISTS (SELECT 1 FROM git_push_entries e WHERE e.tenant_id = r.tenant_id AND e.push_id = r.id AND e.state IN ('authorized', 'committing'))
		ORDER BY id LIMIT $4`, string(t.tenant), repositoryID, afterID, limit)
}

func (t *pgTx) HasUnsettled(ctx context.Context, scope domain.Scope, repositoryID, exceptReceive string) (bool, error) {
	var busy bool
	err := t.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM git_push_records
		WHERE tenant_id = $1 AND project_id = $2 AND repository_id = $3 AND status IN ('authorized', 'unknown') AND receive_id <> $4)`,
		string(t.tenant), string(scope.ProjectID), repositoryID, exceptReceive).Scan(&busy)
	return busy, err
}

func (t *pgTx) UnsettledOnRef(ctx context.Context, scope domain.Scope, repositoryID, ref string) (bool, error) {
	var busy bool
	err := t.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM git_push_entries
		WHERE tenant_id = $1 AND project_id = $2 AND repository_id = $3 AND ref = $4 AND state IN ('authorized', 'committing', 'unknown'))`,
		string(t.tenant), string(scope.ProjectID), repositoryID, ref).Scan(&busy)
	return busy, err
}

func (t *pgTx) AppliedOnRef(ctx context.Context, scope domain.Scope, repositoryID, ref string, limit int) ([]*gitpush.Record, error) {
	return t.scanRecords(ctx, `SELECT `+recordColumns+` FROM git_push_records r
		WHERE tenant_id = $1 AND project_id = $2 AND repository_id = $3
		  AND EXISTS (SELECT 1 FROM git_push_entries e WHERE e.tenant_id = r.tenant_id AND e.push_id = r.id AND e.ref = $4 AND e.state = 'applied')
		ORDER BY (SELECT e.applied_seq FROM git_push_entries e WHERE e.tenant_id = r.tenant_id AND e.push_id = r.id AND e.ref = $4) DESC, id DESC LIMIT $5`, string(t.tenant), string(scope.ProjectID), repositoryID, ref, limit)
}

func (t *pgTx) HasUnknownEntry(ctx context.Context, scope domain.Scope, repositoryID, ref string) (bool, error) {
	var blocked bool
	err := t.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM git_push_entries
		WHERE tenant_id = $1 AND project_id = $2 AND repository_id = $3 AND ref = $4 AND state = 'unknown')`,
		string(t.tenant), string(scope.ProjectID), repositoryID, ref).Scan(&blocked)
	return blocked, err
}

func (t *pgTx) EmptyReportPending(ctx context.Context, repositoryID string, limit int) ([]*gitpush.Record, error) {
	return t.scanRecords(ctx, `SELECT `+recordColumns+` FROM git_push_records
		WHERE tenant_id = $1 AND repository_id = $2 AND empty_report_pending ORDER BY id LIMIT $3`, string(t.tenant), repositoryID, limit)
}

func (t *pgTx) ClearEmptyReport(ctx context.Context, pushID string) error {
	_, err := t.tx.Exec(ctx, `UPDATE git_push_records SET empty_report_pending = false WHERE tenant_id = $1 AND id = $2`, string(t.tenant), pushID)
	return mapErr(err)
}

func (t *pgTx) KnownHead(ctx context.Context, repositoryID string) (int64, error) {
	var head int64
	err := t.tx.QueryRow(ctx, `SELECT head FROM git_journal_heads WHERE tenant_id = $1 AND repository_id = $2`, string(t.tenant), repositoryID).Scan(&head)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return head, err
}

func (t *pgTx) NoteHead(ctx context.Context, repositoryID string, head int64, now time.Time) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO git_journal_heads (tenant_id, repository_id, head, updated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, repository_id) DO UPDATE SET head = GREATEST(git_journal_heads.head, EXCLUDED.head), updated_at = EXCLUDED.updated_at`,
		string(t.tenant), repositoryID, head, now)
	return mapErr(err)
}

func (t *pgTx) writeResolutions(ctx context.Context, r *gitpush.Record, rs []gitpush.Resolution) error {
	for _, res := range rs {
		ordinal := -1
		for i, e := range r.Entries {
			if e.Ref == res.Ref {
				ordinal = i
			}
		}
		if ordinal < 0 {
			return fmt.Errorf("gitpush/postgres: resolution of %s names no entry", res.Ref)
		}
		if _, err := t.tx.Exec(ctx, `INSERT INTO git_push_resolutions
			(tenant_id, push_id, ordinal, project_id, repository_id, ref, resolution, reason, resolved_by, observed, resolved_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			string(t.tenant), r.ID, ordinal, string(r.Scope.ProjectID), r.RepositoryID, res.Ref, string(res.State), res.Reason, res.By,
			nstr(res.Observed), res.At); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

func (t *pgTx) EntryCounts(ctx context.Context, repositoryID string) (gitpush.EntryCounts, error) {
	var c gitpush.EntryCounts
	err := t.tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state IN ('authorized', 'committing')), count(*) FILTER (WHERE state = 'unknown')
		FROM git_push_entries WHERE tenant_id = $1 AND repository_id = $2 AND state IN ('authorized', 'committing', 'unknown')`,
		string(t.tenant), repositoryID).Scan(&c.InFlight, &c.Unknown)
	return c, err
}

// ResetHead lowers the known journal head in a transaction that has first
// recorded the reset (git_journal_resets): the head guard of migration 015
// refuses a decrease without it.
func (t *pgTx) ResetHead(ctx context.Context, scope domain.Scope, repositoryID string, head int64, reset gitpush.HeadReset) error {
	if scope.TenantID != t.tenant {
		return gitpush.ErrNotFound
	}
	var from int64
	err := t.tx.QueryRow(ctx, `SELECT head FROM git_journal_heads WHERE tenant_id = $1 AND repository_id = $2 FOR UPDATE`,
		string(t.tenant), repositoryID).Scan(&from)
	if err != nil {
		return err
	}
	if head >= from {
		return fmt.Errorf("gitpush/postgres: a head reset lowers the known head (%d >= %d)", head, from)
	}
	if _, err := t.tx.Exec(ctx, `INSERT INTO git_journal_resets (tenant_id, repository_id, from_head, to_head, reset_by, reason, reset_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, string(t.tenant), repositoryID, from, head, reset.By, reset.Reason, reset.At); err != nil {
		return mapErr(err)
	}
	if _, err := t.tx.Exec(ctx, `UPDATE git_journal_heads SET head = $3, updated_at = $4 WHERE tenant_id = $1 AND repository_id = $2`,
		string(t.tenant), repositoryID, head, reset.At); err != nil {
		return mapErr(err)
	}
	md, _ := json.Marshal(map[string]any{"repositoryId": repositoryID, "from": from, "to": head})
	reason := reset.Reason
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, reason, request_id, metadata)
		VALUES ($1,$2,$3,$4,'user',$5,$6,'repository',$7,$8,$9,$10)`,
		string(t.tenant), gitpush.HeadResetAuditID(scope.TenantID, repositoryID, from, head, reset.At), string(scope.ProjectID), reset.At, reset.By,
		gitpush.AuditHeadReset, repositoryID, nstr(reason), nstr(gitpush.RequestID(ctx)), md)
	return mapErr(err)
}

// writeEffects writes the audit rows and the outbox event of a change whose
// record is already current at r.Version.
func (t *pgTx) writeEffects(ctx context.Context, r *gitpush.Record, fx gitpush.Effects) error {
	for i, a := range fx.Audit {
		md := a.Metadata
		if md == nil {
			md = map[string]any{}
		}
		raw, err := json.Marshal(md)
		if err != nil {
			return fmt.Errorf("gitpush: audit metadata: %w", err)
		}
		reason, reqID := a.AuditFields(ctx)
		if _, err := t.tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, reason, request_id, metadata)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			string(t.tenant), gitpush.AuditID(r, i), string(r.Scope.ProjectID), r.OccurredAt(), a.Actor.Type, a.Actor.ID, a.Action,
			gitpush.ResourceType, r.ID, r.Version, nstr(reason), nstr(reqID), raw); err != nil {
			return fmt.Errorf("gitpush: write audit: %w", err)
		}
	}
	if fx.Event {
		env, err := r.Envelope(gitpush.RequestID(ctx), t.s.opts.NewID)
		if err != nil {
			return err
		}
		if _, err := t.s.events.Append(ctx, t.tx, env); err != nil {
			return fmt.Errorf("gitpush: append event %s: %w", env.Type, err)
		}
	}
	return nil
}

func nstr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
