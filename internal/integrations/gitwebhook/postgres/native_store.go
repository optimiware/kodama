package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/storage"
)

// NativeStore implements gitwebhook.NativeStore over PostgreSQL: the native
// trigger and task route configuration and the inbox that deduplicates
// admissions (migration 012). Every statement runs in storage.WithTenant under
// the non-owner runtime role and names tenant and project explicitly. No
// network call is made inside a transaction.
type NativeStore struct {
	db    storage.TxBeginner
	newID func() (string, error)
}

var _ gw.NativeStore = (*NativeStore)(nil)

// NewNativeStore requires a database handle (use the non-owner runtime pool).
func NewNativeStore(db storage.TxBeginner, opts Options) (*NativeStore, error) {
	if db == nil {
		return nil, errors.New("gitwebhook/postgres: a database is required")
	}
	if opts.NewID == nil {
		opts.NewID = randomAuditID
	}
	return &NativeStore{db: db, newID: opts.NewID}, nil
}

func (s *NativeStore) inTenant(ctx context.Context, op string, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(op, storage.WithTenant(ctx, s.db, tenant, fn))
}

// writeAudit appends the record to the shared audit table inside tx.
func (s *NativeStore) writeAudit(ctx context.Context, tx pgx.Tx, a gw.AuditRecord) error {
	id, err := s.newID()
	if err != nil {
		return fmt.Errorf("%w: audit identifier: %w", gw.ErrUnavailable, err)
	}
	resource := "git_push_trigger"
	if strings.HasPrefix(a.Action, "git_task_route.") {
		resource = "git_task_route"
	}
	meta := map[string]string{}
	for k, v := range a.Metadata {
		meta[k] = v
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("%w: audit metadata: %v", gw.ErrInvalid, err)
	}
	actorType, actorID := "system", "git_trigger"
	if a.Principal != "" {
		actorType, actorID = "user", a.Principal
	}
	var at *time.Time
	if !a.At.IsZero() {
		u := a.At.UTC()
		at = &u
	}
	reason := a.Reason
	if len(reason) > maxAuditReason {
		reason = reason[:maxAuditReason]
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, reason, metadata)
VALUES ($1, $2, $3, COALESCE($4, now()), $5, $6, $7, $8, $9, $10, $11::jsonb)`,
		string(a.Scope.TenantID), id, string(a.Scope.ProjectID), at, actorType, actorID, a.Action, resource, string(a.TriggerID), nullable(reason), string(raw))
	return err
}

func checkAudit(scope domain.Scope, a gw.AuditRecord) error {
	if a.Scope != scope {
		return fmt.Errorf("%w: the audit record names another scope", gw.ErrInvalid)
	}
	return nil
}

// ---- push triggers ----

const pushTriggerColumns = `repository_id, ref_patterns, exclude_origin_runs, definition_id, enabled, enabled_at, configured_by, version, created_at, updated_at`

func scanPushTrigger(scope domain.Scope, id string, row pgx.Row) (gw.PushTrigger, error) {
	var (
		t                gw.PushTrigger
		exclude, enabled bool
		created, updated time.Time
		enabledAt        time.Time
		repo, def        string
	)
	if err := row.Scan(&repo, &t.Config.RefPatterns, &exclude, &def, &enabled, &enabledAt, &t.ConfiguredBy, &t.Version, &created, &updated); err != nil {
		return gw.PushTrigger{}, err
	}
	t.Scope = scope
	t.Config = gitv1.TriggerConfig{TriggerID: id, RepositoryID: repo, RefPatterns: t.Config.RefPatterns, ExcludeOriginRuns: &exclude, DefinitionID: def, Enabled: enabled}
	t.CreatedAt, t.UpdatedAt, t.EnabledAt = created.UTC(), updated.UTC(), enabledAt.UTC()
	return t, nil
}

// InsertPushTrigger implements gitwebhook.NativeStore.
func (s *NativeStore) InsertPushTrigger(ctx context.Context, t gw.PushTrigger, audit gw.AuditRecord) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if t.Version != 1 {
		return fmt.Errorf("%w: a new trigger starts at version 1", gw.ErrInvalid)
	}
	if err := checkAudit(t.Scope, audit); err != nil {
		return err
	}
	return s.inTenant(ctx, "insert push trigger", t.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO git_push_triggers (tenant_id, project_id, id, `+pushTriggerColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			string(t.Scope.TenantID), string(t.Scope.ProjectID), t.ID(), t.Config.RepositoryID, t.Config.RefPatterns, *t.Config.ExcludeOriginRuns,
			t.Config.DefinitionID, t.Config.Enabled, t.EnabledAt.UTC(), t.ConfiguredBy, t.Version, t.CreatedAt.UTC(), t.UpdatedAt.UTC()); err != nil {
			return err
		}
		return s.writeAudit(ctx, tx, audit)
	})
}

// PushTrigger implements gitwebhook.NativeStore.
func (s *NativeStore) PushTrigger(ctx context.Context, scope domain.Scope, id string) (gw.PushTrigger, error) {
	if err := scope.Validate(); err != nil {
		return gw.PushTrigger{}, gw.ErrNotFound
	}
	var out gw.PushTrigger
	err := s.inTenant(ctx, "read push trigger", scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		t, err := scanPushTrigger(scope, id, tx.QueryRow(ctx, `SELECT `+pushTriggerColumns+` FROM git_push_triggers WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), id))
		if errors.Is(err, pgx.ErrNoRows) {
			return gw.ErrNotFound
		}
		out = t
		return err
	})
	return out, err
}

// UpdatePushTrigger implements gitwebhook.NativeStore: optimistic and atomic
// with its audit. A stale version and a changed repository binding are both an
// ErrConflict that changes nothing.
func (s *NativeStore) UpdatePushTrigger(ctx context.Context, t gw.PushTrigger, expected int64, audit gw.AuditRecord) (gw.PushTrigger, error) {
	if err := t.Validate(); err != nil {
		return gw.PushTrigger{}, err
	}
	if err := checkAudit(t.Scope, audit); err != nil {
		return gw.PushTrigger{}, err
	}
	var out gw.PushTrigger
	err := s.inTenant(ctx, "update push trigger", t.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		next, err := scanPushTriggerReturning(t.Scope, t.ID(), tx.QueryRow(ctx, `UPDATE git_push_triggers
SET ref_patterns = $4, exclude_origin_runs = $5, definition_id = $6, enabled = $7, enabled_at = $8, configured_by = $9, version = version + 1, updated_at = $10
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $11 AND repository_id = $12
RETURNING `+pushTriggerColumns,
			string(t.Scope.TenantID), string(t.Scope.ProjectID), t.ID(), t.Config.RefPatterns, *t.Config.ExcludeOriginRuns, t.Config.DefinitionID, t.Config.Enabled,
			t.EnabledAt.UTC(), t.ConfiguredBy, t.UpdatedAt.UTC(), expected, t.Config.RepositoryID))
		if errors.Is(err, pgx.ErrNoRows) {
			return s.missingOrConflict(ctx, tx, t.Scope, t.ID())
		}
		if err != nil {
			return err
		}
		out = next
		return s.writeAudit(ctx, tx, audit)
	})
	return out, err
}

func scanPushTriggerReturning(scope domain.Scope, id string, row pgx.Row) (gw.PushTrigger, error) {
	return scanPushTrigger(scope, id, row)
}

// missingOrConflict tells an absent trigger (ErrNotFound) from one whose
// version or binding did not match (ErrConflict).
func (s *NativeStore) missingOrConflict(ctx context.Context, tx pgx.Tx, scope domain.Scope, id string) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM git_push_triggers WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return gw.ErrNotFound
	}
	return gw.ErrConflict
}

func (s *NativeStore) queryTriggers(ctx context.Context, op string, scope domain.Scope, sql string, args ...any) ([]gw.PushTrigger, error) {
	var out []gw.PushTrigger
	err := s.inTenant(ctx, op, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var (
				repo, def        string
				patterns         []string
				exclude, enabled bool
				by               string
				version          int64
				created, updated time.Time
				enabledAt        time.Time
			)
			if err := rows.Scan(&id, &repo, &patterns, &exclude, &def, &enabled, &enabledAt, &by, &version, &created, &updated); err != nil {
				return err
			}
			out = append(out, gw.PushTrigger{
				Scope: scope, ConfiguredBy: by, Version: version, EnabledAt: enabledAt.UTC(), CreatedAt: created.UTC(), UpdatedAt: updated.UTC(),
				Config: gitv1.TriggerConfig{TriggerID: id, RepositoryID: repo, RefPatterns: patterns, ExcludeOriginRuns: &exclude, DefinitionID: def, Enabled: enabled},
			})
		}
		return rows.Err()
	})
	return out, err
}

// ListPushTriggers implements gitwebhook.NativeStore. The order is byte order
// (collation "C"), the order the memory store and the consumers use.
func (s *NativeStore) ListPushTriggers(ctx context.Context, scope domain.Scope, after string, limit int) ([]gw.PushTrigger, error) {
	if err := scope.Validate(); err != nil {
		return nil, gw.ErrNotFound
	}
	return s.queryTriggers(ctx, "list push triggers", scope, `SELECT id, `+pushTriggerColumns+` FROM git_push_triggers
WHERE tenant_id = $1 AND project_id = $2 AND id COLLATE "C" > $3 COLLATE "C" ORDER BY id COLLATE "C" LIMIT $4`,
		string(scope.TenantID), string(scope.ProjectID), after, limit)
}

// EnabledPushTriggers implements gitwebhook.NativeStore.
func (s *NativeStore) EnabledPushTriggers(ctx context.Context, scope domain.Scope, repositoryID string) ([]gw.PushTrigger, error) {
	if err := scope.Validate(); err != nil {
		return nil, gw.ErrNotFound
	}
	return s.queryTriggers(ctx, "select push triggers", scope, `SELECT id, `+pushTriggerColumns+` FROM git_push_triggers
WHERE tenant_id = $1 AND project_id = $2 AND repository_id = $3 AND enabled ORDER BY id COLLATE "C"`,
		string(scope.TenantID), string(scope.ProjectID), repositoryID)
}

// ---- task route ----

const taskRouteColumns = `definition_id, enabled, enabled_at, configured_by, version, created_at, updated_at`

func scanTaskRoute(scope domain.Scope, row pgx.Row) (gw.TaskRoute, error) {
	var (
		r                gw.TaskRoute
		def              string
		created, updated time.Time
		enabledAt        time.Time
	)
	if err := row.Scan(&def, &r.Enabled, &enabledAt, &r.ConfiguredBy, &r.Version, &created, &updated); err != nil {
		return gw.TaskRoute{}, err
	}
	r.Scope, r.DefinitionID = scope, domain.DefinitionID(def)
	r.CreatedAt, r.UpdatedAt, r.EnabledAt = created.UTC(), updated.UTC(), enabledAt.UTC()
	return r, nil
}

// PutTaskRoute implements gitwebhook.NativeStore.
func (s *NativeStore) PutTaskRoute(ctx context.Context, r gw.TaskRoute, expected int64, audit gw.AuditRecord) (gw.TaskRoute, error) {
	if err := r.Validate(); err != nil {
		return gw.TaskRoute{}, err
	}
	if err := checkAudit(r.Scope, audit); err != nil {
		return gw.TaskRoute{}, err
	}
	var out gw.TaskRoute
	err := s.inTenant(ctx, "put task route", r.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			row  pgx.Row
			sqlS = `RETURNING ` + taskRouteColumns
		)
		if expected == 0 {
			row = tx.QueryRow(ctx, `INSERT INTO git_task_routes (tenant_id, project_id, definition_id, enabled, enabled_at, configured_by, version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $7, $5, 1, $6, $6) `+sqlS,
				string(r.Scope.TenantID), string(r.Scope.ProjectID), string(r.DefinitionID), r.Enabled, r.ConfiguredBy, r.UpdatedAt.UTC(), r.EnabledAt.UTC())
		} else {
			row = tx.QueryRow(ctx, `UPDATE git_task_routes SET definition_id = $3, enabled = $4, enabled_at = $8, configured_by = $5, version = version + 1, updated_at = $6
WHERE tenant_id = $1 AND project_id = $2 AND version = $7 `+sqlS,
				string(r.Scope.TenantID), string(r.Scope.ProjectID), string(r.DefinitionID), r.Enabled, r.ConfiguredBy, r.UpdatedAt.UTC(), expected, r.EnabledAt.UTC())
		}
		next, err := scanTaskRoute(r.Scope, row)
		if errors.Is(err, pgx.ErrNoRows) {
			return gw.ErrConflict // a stale version, or an alteration of a route that does not exist
		}
		if err != nil {
			return err
		}
		out = next
		return s.writeAudit(ctx, tx, audit)
	})
	return out, err
}

// TaskRoute implements gitwebhook.NativeStore.
func (s *NativeStore) TaskRoute(ctx context.Context, scope domain.Scope) (gw.TaskRoute, error) {
	if err := scope.Validate(); err != nil {
		return gw.TaskRoute{}, gw.ErrNotFound
	}
	var out gw.TaskRoute
	err := s.inTenant(ctx, "read task route", scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := scanTaskRoute(scope, tx.QueryRow(ctx, `SELECT `+taskRouteColumns+` FROM git_task_routes WHERE tenant_id = $1 AND project_id = $2`,
			string(scope.TenantID), string(scope.ProjectID)))
		if errors.Is(err, pgx.ErrNoRows) {
			return gw.ErrNotFound
		}
		out = r
		return err
	})
	return out, err
}

// ---- inbox ----

const inboxColumns = `COALESCE(lease_holder, ''), lease_until, done, COALESCE(outcome, ''), COALESCE(reason, ''), COALESCE(run_id, ''), received_at, completed_at`

func scanInbox(key gw.InboxKey, row pgx.Row) (gw.InboxRecord, error) {
	var (
		rec               gw.InboxRecord
		leaseUntil, doneT *time.Time
		received          time.Time
		outcome, run      string
	)
	if err := row.Scan(&rec.LeaseHolder, &leaseUntil, &rec.Done, &outcome, &rec.Reason, &run, &received, &doneT); err != nil {
		return gw.InboxRecord{}, err
	}
	rec.Key, rec.Outcome, rec.RunID, rec.ReceivedAt = key, gw.Outcome(outcome), domain.RunID(run), received.UTC()
	if leaseUntil != nil {
		rec.LeaseUntil = leaseUntil.UTC()
	}
	if doneT != nil {
		rec.CompletedAt = doneT.UTC()
	}
	return rec, nil
}

func keyArgs(k gw.InboxKey) []any {
	return []any{string(k.Scope.TenantID), string(k.Scope.ProjectID), string(k.Kind), k.Source, k.Subject, k.Ref}
}

const inboxWhere = `tenant_id = $1 AND project_id = $2 AND kind = $3 AND source = $4 AND subject = $5 AND ref = $6`

// ClaimInbox implements gitwebhook.NativeStore: the primary key is the claim,
// inserted ON CONFLICT DO NOTHING; an existing undecided key is leased with a
// conditional UPDATE under the row lock, so concurrent claims of one key have
// exactly one holder.
func (s *NativeStore) ClaimInbox(ctx context.Context, key gw.InboxKey, holder string, now time.Time, ttl time.Duration) (gw.InboxRecord, bool, error) {
	if err := key.Validate(); err != nil {
		return gw.InboxRecord{}, false, err
	}
	if holder == "" || len(holder) > 256 || ttl <= 0 {
		return gw.InboxRecord{}, false, fmt.Errorf("%w: a claim needs a holder and a positive lease", gw.ErrInvalid)
	}
	now, until := now.UTC(), now.UTC().Add(ttl)
	var (
		rec      gw.InboxRecord
		acquired bool
	)
	err := s.inTenant(ctx, "claim inbox", key.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO git_trigger_inbox (tenant_id, project_id, kind, source, subject, ref, lease_holder, lease_until, received_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT DO NOTHING`, append(keyArgs(key), holder, until, now)...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			acquired = true
			rec, err = scanInbox(key, tx.QueryRow(ctx, `SELECT `+inboxColumns+` FROM git_trigger_inbox WHERE `+inboxWhere, keyArgs(key)...))
			return err
		}
		rec, err = scanInbox(key, tx.QueryRow(ctx, `UPDATE git_trigger_inbox SET lease_holder = $7, lease_until = $8
WHERE `+inboxWhere+` AND NOT done AND (lease_holder IS NULL OR lease_holder = $7 OR lease_until <= $9)
RETURNING `+inboxColumns, append(keyArgs(key), holder, until, now)...))
		switch {
		case err == nil:
			acquired = true
			return nil
		case errors.Is(err, pgx.ErrNoRows):
			rec, err = scanInbox(key, tx.QueryRow(ctx, `SELECT `+inboxColumns+` FROM git_trigger_inbox WHERE `+inboxWhere, keyArgs(key)...))
			return err
		}
		return err
	})
	return rec, acquired, err
}

// FinishInbox implements gitwebhook.NativeStore: the outcome is write-once and
// recorded with its audit in one transaction; the lease fence is evaluated
// under the row lock.
func (s *NativeStore) FinishInbox(ctx context.Context, key gw.InboxKey, fin gw.InboxFinish, now time.Time, audit gw.AuditRecord) (gw.InboxRecord, error) {
	if err := key.Validate(); err != nil {
		return gw.InboxRecord{}, err
	}
	if err := checkAudit(key.Scope, audit); err != nil {
		return gw.InboxRecord{}, err
	}
	switch {
	case fin.Outcome == gw.OutcomeStarted && fin.RunID == "",
		fin.Outcome != gw.OutcomeStarted && (fin.RunID != "" || fin.Reason == ""),
		fin.Outcome != gw.OutcomeStarted && fin.Outcome != gw.OutcomeIgnored && fin.Outcome != gw.OutcomeRejected:
		return gw.InboxRecord{}, fmt.Errorf("%w: a started outcome names its run and any other names its reason", gw.ErrInvalid)
	}
	var out gw.InboxRecord
	err := s.inTenant(ctx, "finish inbox", key.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := scanInbox(key, tx.QueryRow(ctx, `SELECT `+inboxColumns+` FROM git_trigger_inbox WHERE `+inboxWhere+` FOR UPDATE`, keyArgs(key)...))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return gw.ErrNotFound
		case err != nil:
			return err
		case cur.Done:
			out = cur
			return nil
		case fin.Outcome != gw.OutcomeStarted && (fin.Holder == "" || fin.Holder != cur.LeaseHolder || !now.UTC().Before(cur.LeaseUntil)):
			return gw.ErrConflict
		}
		out, err = scanInbox(key, tx.QueryRow(ctx, `UPDATE git_trigger_inbox
SET done = true, outcome = $7, reason = $8, run_id = $9, completed_at = $10, lease_holder = NULL, lease_until = NULL
WHERE `+inboxWhere+` RETURNING `+inboxColumns, append(keyArgs(key), string(fin.Outcome), nullable(fin.Reason), nullable(string(fin.RunID)), now.UTC())...))
		if err != nil {
			return err
		}
		return s.writeAudit(ctx, tx, audit)
	})
	return out, err
}

// LatestInbox implements gitwebhook.NativeStore: the latest completion, ties
// broken by subject and ref in byte order descending (the memory store's order).
func (s *NativeStore) LatestInbox(ctx context.Context, scope domain.Scope, kind gw.InboxKind, source string, outcome gw.Outcome) (gw.InboxRecord, bool, error) {
	if err := scope.Validate(); err != nil {
		return gw.InboxRecord{}, false, gw.ErrNotFound
	}
	var (
		rec gw.InboxRecord
		ok  bool
	)
	err := s.inTenant(ctx, "latest inbox", scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var subject, ref string
		row := tx.QueryRow(ctx, `SELECT subject, ref, `+inboxColumns+` FROM git_trigger_inbox
WHERE tenant_id = $1 AND project_id = $2 AND kind = $3 AND source = $4 AND done AND ($5::text = '' OR outcome = $5::text)
ORDER BY completed_at DESC, subject COLLATE "C" DESC, ref COLLATE "C" DESC LIMIT 1`,
			string(scope.TenantID), string(scope.ProjectID), string(kind), source, string(outcome))
		var (
			leaseUntil, doneT *time.Time
			received          time.Time
			oc, run           string
		)
		err := row.Scan(&subject, &ref, &rec.LeaseHolder, &leaseUntil, &rec.Done, &oc, &rec.Reason, &run, &received, &doneT)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rec.Key = gw.InboxKey{Scope: scope, Kind: kind, Source: source, Subject: subject, Ref: ref}
		rec.Outcome, rec.RunID, rec.ReceivedAt = gw.Outcome(oc), domain.RunID(run), received.UTC()
		if doneT != nil {
			rec.CompletedAt = doneT.UTC()
		}
		ok = true
		return nil
	})
	return rec, ok, err
}

// ReleaseInbox implements gitwebhook.NativeStore.
func (s *NativeStore) ReleaseInbox(ctx context.Context, key gw.InboxKey, holder string) error {
	if err := key.Validate(); err != nil {
		return err
	}
	return s.inTenant(ctx, "release inbox", key.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE git_trigger_inbox SET lease_holder = NULL, lease_until = NULL WHERE `+inboxWhere+` AND NOT done AND lease_holder = $7`,
			append(keyArgs(key), holder)...)
		return err
	})
}
