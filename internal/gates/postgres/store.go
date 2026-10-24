// Package postgres is the PostgreSQL adapter of the gate module: the gate and
// evidence-reference tables, the transactional store, the keyed (idempotent)
// decision helper, the execution-store decorator that protects claims behind
// gates, and the reconciliation scans.
//
// Every statement runs inside storage.WithTenant under the non-owner runtime
// role, carries explicit tenant and project predicates and is further
// constrained by forced row level security. State, audit and outbox rows
// commit together; an appender failure rolls the business mutation back. No
// network call is ever made inside a transaction: evidence is read, pinned and
// verified before, and only immutable identity and current database facts are
// rechecked inside.
//
// Lock order, extending the execution module's (see execution/postgres):
//
//  1. execution quota scope rows (dispatch only);
//  2. the work item admission lock;
//  3. the run row (FOR UPDATE), then its step, attempt and bookkeeping rows;
//  4. the gate rows (FOR UPDATE for a decision or invalidation, FOR SHARE for
//     consumption and dispatch, several in ascending identifier order);
//  5. the identity authorization revision (read FOR SHARE) and secret rows;
//  6. the workspace row;
//  7. the event backlog and scope rows inside events/postgres Append, last.
//
// A decision takes only steps 4, 5 and 7 and never the run row. A claim takes
// 1-3, then locks every gate bound to the run FOR SHARE before the claim
// mutation runs (ExecutionStore.guard), so the authorization revision the
// claim reads (step 5) is always taken after the gates; completion takes 2-5.
// No path takes a later lock before an earlier one, so the orders cannot cycle.
// The idempotency advisory lock of a keyed decision is taken before everything
// else.
package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/storage"
)

// EventAppender is the transaction-local event append of the event bus.
type EventAppender interface {
	Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error)
}

// ExecutionTx is the execution store's transaction-joining entry point
// (execution/postgres.Store.UpdateInTx).
type ExecutionTx interface {
	UpdateInTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error)
}

// AuthorityBinder binds current-authorization checks to one open transaction.
type AuthorityBinder func(tx pgx.Tx) gates.TxAuthority

// DeploymentBinder binds the deployment reader to one open transaction, so the
// configuration digest is read in the transaction that decides or consumes.
type DeploymentBinder func(tx pgx.Tx) gates.Deployments

// Config wires a Store. DB, Events, Execution and Authority are required: a
// gate transition without its audit/outbox records, its execution binding or
// its authority check must not be constructible.
type Config struct {
	DB        storage.TxBeginner
	Events    EventAppender
	Execution ExecutionTx
	Authority AuthorityBinder
	// Deployments is optional. Without it a deployment-bearing gate cannot be
	// decided or consumed.
	Deployments DeploymentBinder
	Clock       func() time.Time
	NewID       func(prefix string) string
}

// Store implements gates.Store.
type Store struct {
	db   storage.TxBeginner
	ev   EventAppender
	exec ExecutionTx
	auth AuthorityBinder
	dep  DeploymentBinder
	now  func() time.Time
	id   func(prefix string) string
}

var _ gates.Store = (*Store)(nil)

// NewStore validates the configuration.
func NewStore(c Config) (*Store, error) {
	if c.DB == nil || c.Events == nil || c.Execution == nil || c.Authority == nil {
		return nil, fmt.Errorf("%w: database, event appender, execution binding and authority are required", gates.ErrNotConfigured)
	}
	s := &Store{db: c.DB, ev: c.Events, exec: c.Execution, auth: c.Authority, dep: c.Deployments, now: c.Clock, id: c.NewID}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.id == nil {
		s.id = func(prefix string) string {
			b := make([]byte, 12)
			if _, err := rand.Read(b); err != nil {
				panic(err)
			}
			return prefix + "_" + hex.EncodeToString(b)
		}
	}
	return s, nil
}

func (s *Store) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// inTenant runs fn in a tenant transaction and classifies its failure.
func (s *Store) inTenant(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenant(ctx, s.db, tenant, fn))
}

func (s *Store) inTenantRead(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return classify(storage.WithTenantOptions(ctx, s.db, tenant, pgx.TxOptions{AccessMode: pgx.ReadOnly}, fn))
}

// classify maps infrastructure failures to the module's classes. Business
// errors pass through; unknown ones stay unclassified (a 500).
func classify(err error) error {
	if err == nil || gates.IsBusinessError(err) || execution.IsBusinessError(err) {
		return err
	}
	switch {
	case errors.Is(err, events.ErrBacklogFull):
		return gates.Unavailablef(err, "the event backlog is full")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return gates.Unavailablef(err, "the request ended before the outcome was known")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23503":
			return fmt.Errorf("%w: a referenced resource was not found", gates.ErrNotFound)
		case pgErr.Code == "23505":
			return fmt.Errorf("%w: the record already exists", gates.ErrConflict)
		case pgErr.Code == "23514":
			return fmt.Errorf("%w: the database refused the change (%s)", gates.ErrInvalidTransition, pgErr.ConstraintName)
		case pgErr.Code == "55P03":
			return gates.Unavailablef(err, "the gate is busy")
		case len(pgErr.Code) >= 2 && (pgErr.Code[:2] == "40" || pgErr.Code[:2] == "53" || pgErr.Code[:2] == "57" || pgErr.Code[:2] == "08"):
			return gates.Unavailablef(err, "the database could not complete the request")
		}
	}
	var connErr interface{ Timeout() bool }
	if errors.As(err, &connErr) {
		return gates.Unavailablef(err, "the database could not be reached")
	}
	return err
}

// ---- records ----

const gateColumns = `id, run_id, step_id, subject, required, status, version, requested_at, deadline, decided_by_type, decided_by_id,
	decision, decision_reason, decided_at, invalidated_at, invalidation_reason, superseding_commit, superseding_revision, created_at, updated_at`

func scanGate(row pgx.Row, scope domain.Scope) (*gates.Gate, error) {
	var (
		id, run, step, subject, status        string
		required                              []byte
		version                               int64
		requested, deadline, created, updated time.Time
		byType, byID, decision, reason        *string
		decidedAt, invalidatedAt              *time.Time
		invReason, supCommit                  *string
		supRevision                           *int64
	)
	if err := row.Scan(&id, &run, &step, &subject, &required, &status, &version, &requested, &deadline, &byType, &byID, &decision, &reason,
		&decidedAt, &invalidatedAt, &invReason, &supCommit, &supRevision, &created, &updated); err != nil {
		return nil, err
	}
	rec := gates.Record{
		ID: gates.ID(id), Scope: scope, RunID: domain.RunID(run), StepID: step, Subject: []byte(subject), Status: gates.Status(status),
		Version: domain.Version(version), RequestedAt: requested.UTC(), Deadline: deadline.UTC(), CreatedAt: created.UTC(), UpdatedAt: updated.UTC(),
	}
	if err := json.Unmarshal(required, &rec.Required); err != nil {
		return nil, fmt.Errorf("gates: stored evidence snapshot: %w", err)
	}
	if rec.Required.Evidence == nil {
		rec.Required.Evidence = []gates.RequiredEvidence{}
	}
	if decision != nil && byType != nil && byID != nil && reason != nil && decidedAt != nil {
		rec.Decision = &gates.DecisionRecord{By: gates.Actor{Type: gates.ActorType(*byType), ID: *byID}, Decision: gates.Decision(*decision), Reason: *reason, At: decidedAt.UTC()}
	}
	if invalidatedAt != nil && invReason != nil {
		inv := &gates.Invalidation{At: invalidatedAt.UTC(), Reason: *invReason}
		if supCommit != nil {
			inv.SupersedingCommit = *supCommit
		}
		if supRevision != nil {
			inv.SupersedingRevision = *supRevision
		}
		rec.Invalidation = inv
	}
	return gates.Restore(rec)
}

type lockMode int

const (
	lockNone lockMode = iota
	lockShare
	lockUpdate
)

func (m lockMode) sql() string {
	switch m {
	case lockShare:
		return " FOR SHARE"
	case lockUpdate:
		return " FOR UPDATE"
	}
	return ""
}

func (s *Store) loadGate(ctx context.Context, tx pgx.Tx, scope domain.Scope, id gates.ID, mode lockMode) (*gates.Gate, error) {
	g, err := scanGate(tx.QueryRow(ctx, `SELECT `+gateColumns+` FROM gates WHERE tenant_id = $1 AND project_id = $2 AND id = $3`+mode.sql(),
		string(scope.TenantID), string(scope.ProjectID), string(id)), scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, gates.ErrNotFound
	}
	return g, err
}

// Get implements gates.Store.
func (s *Store) Get(ctx context.Context, scope domain.Scope, id gates.ID) (*gates.Gate, error) {
	if err := scope.Validate(); err != nil || id.Validate() != nil {
		return nil, gates.ErrNotFound
	}
	var g *gates.Gate
	err := s.inTenantRead(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		g, err = s.loadGate(ctx, tx, scope, id, lockNone)
		return err
	})
	return g, err
}

// ByStep implements gates.Store.
func (s *Store) ByStep(ctx context.Context, scope domain.Scope, run domain.RunID, step string) (*gates.Gate, error) {
	if err := scope.Validate(); err != nil || run.Validate() != nil {
		return nil, gates.ErrNotFound
	}
	var g *gates.Gate
	err := s.inTenantRead(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		g, err = s.byStep(ctx, tx, scope, run, step, lockNone)
		return err
	})
	return g, err
}

func (s *Store) byStep(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID, step string, mode lockMode) (*gates.Gate, error) {
	g, err := scanGate(tx.QueryRow(ctx, `SELECT `+gateColumns+` FROM gates WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND step_id = $4`+mode.sql(),
		string(scope.TenantID), string(scope.ProjectID), string(run), step), scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, gates.ErrNotFound
	}
	return g, err
}

// List implements gates.Store.
func (s *Store) List(ctx context.Context, scope domain.Scope, q gates.ListQuery) ([]*gates.Gate, error) {
	if err := scope.Validate(); err != nil {
		return nil, gates.ErrNotFound
	}
	if q.Limit < 1 || q.Limit > 1000 {
		return nil, gates.Invalidf("limit is out of range")
	}
	args := []any{string(scope.TenantID), string(scope.ProjectID)}
	where := `tenant_id = $1 AND project_id = $2`
	if len(q.Statuses) > 0 {
		var st []string
		for _, v := range q.Statuses {
			st = append(st, string(v))
		}
		args = append(args, st)
		where += fmt.Sprintf(` AND status = ANY($%d)`, len(args))
	}
	if q.RunID != "" {
		args = append(args, string(q.RunID))
		where += fmt.Sprintf(` AND run_id = $%d`, len(args))
	}
	if q.After != nil {
		args = append(args, micros(q.After.CreatedAt), string(q.After.ID))
		where += fmt.Sprintf(` AND (created_at, id) > ($%d, $%d)`, len(args)-1, len(args))
	}
	args = append(args, q.Limit)
	var out []*gates.Gate
	err := s.inTenantRead(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+gateColumns+` FROM gates WHERE `+where+fmt.Sprintf(` ORDER BY created_at, id LIMIT $%d`, len(args)), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			g, err := scanGate(rows, scope)
			if err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	return out, err
}

func micros(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func nstr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---- audit and events ----

func auditActorType(t gates.ActorType) string {
	if t == gates.ActorService {
		return "system"
	}
	return string(t)
}

func eventID(g *gates.Gate, kind string, version domain.Version) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "gates/%s/%s/%s/%s/%d", g.Scope.TenantID, g.Scope.ProjectID, g.ID, kind, version))
	return "evt_gt_" + hex.EncodeToString(sum[:20])
}

// record writes the audit row and the catalog event of one gate transition in
// the transaction that saved the gate. An error rolls the transition back.
func (s *Store) record(ctx context.Context, tx pgx.Tx, g *gates.Gate, kind string, actor gates.Actor, reqID, reason string, data any, md map[string]any) error {
	now := s.clock()
	md["runId"], md["stepId"], md["subjectDigest"] = string(g.RunID), g.StepID, g.SubjectDigest()
	rawMD, err := json.Marshal(md)
	if err != nil {
		return err
	}
	if len(reqID) > 256 {
		reqID = reqID[:256]
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, reason, request_id, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'gate',$8,$9,$10,$11,$12)`,
		string(g.Scope.TenantID), s.id("aud"), string(g.Scope.ProjectID), now, auditActorType(actor.Type), actor.ID, "gate."+kind, string(g.ID),
		int64(g.Version), nstr(reason), nstr(reqID), rawMD); err != nil {
		return err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	corr := reqID
	if corr == "" {
		corr = s.id("corr")
	}
	typ := map[string]string{"requested": eventsv1.TypeGateRequested, "decided": eventsv1.TypeGateDecided, "invalidated": eventsv1.TypeGateInvalidated}[kind]
	env := eventsv1.Envelope{
		ID: eventID(g, kind, g.Version), SchemaVersion: eventsv1.SchemaVersion, Type: typ, OccurredAt: now,
		TenantID: g.Scope.TenantID, ProjectID: g.Scope.ProjectID,
		Resource:      eventsv1.Resource{Type: "gate", ID: string(g.ID), Version: int64(g.Version)},
		Actor:         eventsv1.Actor{Type: eventsv1.ActorType(actor.Type), ID: actor.ID},
		CorrelationID: corr, Data: raw,
	}
	if _, err := s.ev.Append(ctx, tx, env); err != nil {
		return fmt.Errorf("gates: append event %s: %w", typ, err)
	}
	return nil
}

const timestampLayout = "2006-01-02T15:04:05.999999999Z"
