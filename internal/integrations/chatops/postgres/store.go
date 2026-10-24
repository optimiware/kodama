// Package postgres persists the ChatOps module over PostgreSQL with forced row
// level security: project-scoped destinations, durable deliveries with their
// attempt history, replay audit metadata and the per-tenant restore fence.
//
// Every transaction runs under storage.WithTenant, which sets the
// transaction-local tenant context RLS reads, and every statement also names the
// tenant and project explicitly.
//
// Lock ordering (see the module README for the full table):
//
//   - protected management mutations (destination create/update, replay):
//     project row (FOR NO KEY UPDATE) -> authorization revision (FOR SHARE, taken
//     by the transaction-bound authority) -> ChatOps rows;
//   - the event-bus inbox handler: inbox row (T-005) -> ChatOps destination and
//     delivery rows; it takes neither the project lock nor an authorization
//     revision, and no management mutation takes an inbox row, so the two cannot
//     form a cycle;
//   - the worker: delivery rows (FOR UPDATE SKIP LOCKED) -> destination row
//     (FOR SHARE) in claim transactions; finalization locks one delivery row.
//     Network effects never happen inside any of these transactions.
package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	"github.com/optimiware/kodama/internal/storage"
)

// Config configures a Store.
type Config struct {
	// DB begins transactions; use the non-owner runtime pool.
	DB storage.TxBeginner
	// Authorizer is the transaction-bound permission check. It is required:
	// without it every management operation is refused (no allow-all fallback).
	Authorizer TxAuthorizer
	// Clock defaults to the UTC wall clock.
	Clock func() time.Time
}

// Store implements chatops.Store, chatops.WorkStore and chatops.FenceStore.
type Store struct {
	db   storage.TxBeginner
	auth TxAuthorizer
	now  func() time.Time
}

var (
	_ chatops.Store      = (*Store)(nil)
	_ chatops.WorkStore  = (*Store)(nil)
	_ chatops.FenceStore = (*Store)(nil)
)

// NewStore validates cfg.
func NewStore(cfg Config) (*Store, error) {
	if cfg.DB == nil || cfg.Authorizer == nil {
		return nil, errors.New("chatops/postgres: DB and Authorizer are required")
	}
	s := &Store{db: cfg.DB, auth: cfg.Authorizer, now: cfg.Clock}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

// ---- transaction helpers -----------------------------------------------------

// system runs a tenant transaction with no caller (worker, consumer, fence).
func (s *Store) system(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenant(ctx, s.db, tenant, fn))
}

// protected runs one operation of a caller: for a mutation the project lock
// first, then the transaction-bound permission check, then fn.
func (s *Store) protected(ctx context.Context, c chatops.Caller, scope domain.Scope, perm chatops.Permission, mutate bool, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenant(ctx, s.db, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if mutate {
			var one int
			err := tx.QueryRow(ctx, `SELECT 1 FROM projects WHERE tenant_id = $1 AND id = $2 FOR NO KEY UPDATE`,
				string(scope.TenantID), string(scope.ProjectID)).Scan(&one)
			if errors.Is(err, pgx.ErrNoRows) {
				return chatops.Fail(chatops.ErrNotFound, err, "not found")
			}
			if err != nil {
				return err
			}
		}
		if err := s.auth.Authorize(ctx, tx, c.Actor, scope, perm); err != nil {
			return err
		}
		return fn(ctx, tx)
	}))
}

// CheckAccess implements chatops.Store.
func (s *Store) CheckAccess(ctx context.Context, c chatops.Caller, scope domain.Scope, perm chatops.Permission) error {
	return s.protected(ctx, c, scope, perm, false, func(context.Context, pgx.Tx) error { return nil })
}

// classify keeps classified errors and maps infrastructure failures, including
// an unknown commit outcome, to the module's classes: anything unclassified is
// "unavailable", never success.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if isClassified(err) {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return chatops.Fail(chatops.ErrUnavailable, err, "the request ended before the outcome was known")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23503":
			return chatops.Fail(chatops.ErrNotFound, err, "a referenced resource was not found")
		case "23514":
			return chatops.Fail(chatops.ErrInvalidTransition, err, "the database refused the change")
		case "23505":
			return chatops.Fail(chatops.ErrConflict, err, "the record already exists")
		}
	}
	return chatops.Fail(chatops.ErrUnavailable, err, "the database could not complete the request")
}

func isClassified(err error) bool {
	for _, k := range []error{chatops.ErrInvalid, chatops.ErrNotFound, chatops.ErrDenied, chatops.ErrVersionConflict, chatops.ErrInvalidTransition,
		chatops.ErrStaleClaim, chatops.ErrConflict, chatops.ErrNameTaken, chatops.ErrLimit, chatops.ErrNotConfigured, chatops.ErrPolicy,
		chatops.ErrCredentialUnavailable, chatops.ErrFenced, chatops.ErrUnavailable} {
		if errors.Is(err, k) {
			return true
		}
	}
	return false
}

func constraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// ---- audit ---------------------------------------------------------------------

type auditEntry struct {
	Scope        domain.Scope
	ActorType    string
	ActorID      string
	RequestID    string
	Reason       string
	Action       string
	ResourceType string
	ResourceID   string
	Version      int64
	At           time.Time
	Metadata     map[string]any
}

func newAuditID() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return "aud_" + hex.EncodeToString(b[:])
}

// audit appends one safe-metadata row: identifiers, versions and operation
// names only, never an endpoint, a credential reference value or a payload.
func audit(ctx context.Context, tx pgx.Tx, e auditEntry) error {
	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	mj, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	var project, reason, request *string
	if e.Scope.ProjectID != "" {
		p := string(e.Scope.ProjectID)
		project = &p
	}
	if e.Reason != "" {
		reason = &e.Reason
	}
	if e.RequestID != "" {
		request = &e.RequestID
	}
	var version *int64
	if e.Version > 0 {
		version = &e.Version
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit
(tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, reason, request_id, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb)`,
		string(e.Scope.TenantID), newAuditID(), project, e.At.UTC(), e.ActorType, e.ActorID, e.Action, e.ResourceType, e.ResourceID,
		version, reason, request, string(mj))
	return err
}

func callerAudit(c chatops.Caller, scope domain.Scope, at time.Time, action, resourceType, resourceID string, version int64, reason string, meta map[string]any) auditEntry {
	return auditEntry{Scope: scope, ActorType: "user", ActorID: c.Actor.ID, RequestID: c.RequestID, Reason: reason, Action: action,
		ResourceType: resourceType, ResourceID: resourceID, Version: version, At: at, Metadata: meta}
}
