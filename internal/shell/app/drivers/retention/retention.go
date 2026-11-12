// Package retention implements the installation.RetentionStore seam over the
// PostgreSQL scoped store. It lives in the composition layer because it is the
// production caller of an installation port: shell adapters may not depend on
// installation declarations.
package retention

// PostgreSQL retention adapter. It offers the sweeper only the rows whose
// creation time is at or before a family's boundary, reports whether a live
// execution still references an evidence row, and re-checks both conditions
// inside the deleting statement. Every statement is scope-predicated and runs
// in a transaction-local tenancy scope, so the runtime role's forced row-level
// security is a second, independent guard.
//
// The adapter deliberately does not manage operation receipts (kept until
// project deletion) or retained log chunks (the log FileSink enforces its
// 30-day bound). Raw model/tool content is swept only when the sweeper's
// RetentionPolicy enables RawContentOptIn, and holds rows whose retain_until
// deadline has not elapsed or whose linked execution is still active.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// RetentionStore is the PostgreSQL implementation of
// installation.RetentionStore.
type RetentionStore struct {
	store *postgres.Store
}

// NewRetentionStore builds the retention adapter over one role pool. The pool
// is normally the runtime role, so the same least-privilege session that serves
// requests also proves the retention delete.
func NewRetentionStore(db postgres.DB) *RetentionStore {
	return &RetentionStore{store: postgres.New(db)}
}

var _ installation.RetentionStore = (*RetentionStore)(nil)

// Candidates returns the rows of class in scope older than before. The evidence
// family reports an active reference when the row names an execution that is
// still queued or running; terminal executions do not extend the deadline.
// Tenant-wide audit rows (project_id NULL) are reached by a tenant scope.
func (adapter *RetentionStore) Candidates(ctx context.Context, scope installation.RetentionScope, class installation.RetentionClass, before time.Time) ([]installation.RetentionCandidate, error) {
	if ctx == nil {
		return nil, fault.New(fault.InvalidInput, "retention candidate scan requires a context")
	}
	if before.IsZero() {
		return nil, fault.New(fault.InvalidInput, "retention candidate scan requires a boundary time")
	}
	typed, err := retentionScope(scope)
	if err != nil {
		return nil, err
	}
	switch class {
	case installation.RetentionEvidence:
		if typed.TenantScoped() {
			// verifications are project-scoped; a tenant scope cannot read or
			// delete them without naming the project.
			return nil, nil
		}
		return adapter.queryCandidates(ctx, typed, `
			SELECT v.id, v.created_at,
			       EXISTS (
			           SELECT 1 FROM executions e
			           WHERE e.tenant_id = v.tenant_id
			             AND e.project_id = v.project_id
			             AND e.id = v.execution_id
			             AND e.state IN ('queued', 'running')
			       )
			FROM verifications v
			WHERE v.tenant_id = $1 AND v.project_id = $2 AND v.created_at < $3
			ORDER BY v.created_at, v.id`,
			string(typed.TenantID), string(typed.ProjectID), before)
	case installation.RetentionAudit:
		if typed.TenantScoped() {
			return adapter.queryCandidates(ctx, typed, `
				SELECT a.id, a.at, false
				FROM audit a
				WHERE a.tenant_id = $1 AND a.project_id IS NULL AND a.at < $2
				ORDER BY a.at, a.id`,
				string(typed.TenantID), before)
		}
		return adapter.queryCandidates(ctx, typed, `
			SELECT a.id, a.at, false
			FROM audit a
			WHERE a.tenant_id = $1 AND a.project_id = $2 AND a.at < $3
			ORDER BY a.at, a.id`,
			string(typed.TenantID), string(typed.ProjectID), before)
	case installation.RetentionRawContent:
		if typed.TenantScoped() {
			// content_records are project-scoped; a tenant scope cannot read or
			// delete them without naming the project.
			return nil, nil
		}
		now, err := rawContentNow(before)
		if err != nil {
			return nil, err
		}
		return adapter.queryCandidates(ctx, typed, `
			SELECT c.id, c.recorded_at,
			       (c.retain_until > $4 OR (
			           c.execution_id IS NOT NULL AND EXISTS (
			               SELECT 1 FROM executions e
			               WHERE e.tenant_id = c.tenant_id
			                 AND e.project_id = c.project_id
			                 AND e.id = c.execution_id
			                 AND e.state IN ('queued', 'running')
			           )
			       ))
			FROM content_records c
			WHERE c.tenant_id = $1 AND c.project_id = $2 AND c.recorded_at < $3
			ORDER BY c.recorded_at, c.id`,
			string(typed.TenantID), string(typed.ProjectID), before, now)
	case installation.RetentionLogs, installation.RetentionReceipts:
		// Retained logs are bounded by the log sink and receipts live until
		// project deletion.
		return nil, nil
	default:
		return nil, fault.New(fault.InvalidInput, fmt.Sprintf("retention candidate class %q is unknown", class))
	}
}

// Delete removes one candidate only when the same scope, identity, creation
// time, expiry boundary and (for evidence) absence of an active reference still
// hold. It reports whether a row was removed. A row already gone and a
// concurrent change that restores a live reference both report false, so a
// retried sweep is idempotent and only proven deletions are counted.
func (adapter *RetentionStore) Delete(ctx context.Context, scope installation.RetentionScope, class installation.RetentionClass, candidate installation.RetentionCandidate, before time.Time) (bool, error) {
	if ctx == nil {
		return false, fault.New(fault.InvalidInput, "retention delete requires a context")
	}
	if candidate.ID == "" {
		return false, fault.New(fault.InvalidInput, "retention delete requires a row identity")
	}
	if candidate.CreatedAt.IsZero() || before.IsZero() {
		return false, fault.New(fault.InvalidInput, "retention delete requires a proven creation and boundary time")
	}
	typed, err := retentionScope(scope)
	if err != nil {
		return false, err
	}
	var statement store.Statement
	switch class {
	case installation.RetentionEvidence:
		if typed.TenantScoped() {
			return false, nil
		}
		statement = store.Statement{
			SQL: `
				DELETE FROM verifications v
				WHERE v.id = $1
				  AND v.tenant_id = $2
				  AND v.project_id = $3
				  AND v.created_at = $4
				  AND v.created_at < $5
				  AND NOT EXISTS (
				      SELECT 1 FROM executions e
				      WHERE e.tenant_id = v.tenant_id
				        AND e.project_id = v.project_id
				        AND e.id = v.execution_id
				        AND e.state IN ('queued', 'running')
				  )`,
			Args: []any{candidate.ID, string(typed.TenantID), string(typed.ProjectID), candidate.CreatedAt, before},
		}
	case installation.RetentionAudit:
		if typed.TenantScoped() {
			statement = store.Statement{
				SQL: `
					DELETE FROM audit
					WHERE id = $1
					  AND tenant_id = $2
					  AND project_id IS NULL
					  AND at = $3
					  AND at < $4`,
				Args: []any{candidate.ID, string(typed.TenantID), candidate.CreatedAt, before},
			}
		} else {
			statement = store.Statement{
				SQL: `
					DELETE FROM audit
					WHERE id = $1
					  AND tenant_id = $2
					  AND project_id = $3
					  AND at = $4
					  AND at < $5`,
				Args: []any{candidate.ID, string(typed.TenantID), string(typed.ProjectID), candidate.CreatedAt, before},
			}
		}
	case installation.RetentionRawContent:
		if typed.TenantScoped() {
			return false, nil
		}
		now, err := rawContentNow(before)
		if err != nil {
			return false, err
		}
		statement = store.Statement{
			SQL: `
				DELETE FROM content_records c
				WHERE c.id = $1
				  AND c.tenant_id = $2
				  AND c.project_id = $3
				  AND c.recorded_at = $4
				  AND c.recorded_at < $5
				  AND c.retain_until <= $6
				  AND (c.execution_id IS NULL OR NOT EXISTS (
				      SELECT 1 FROM executions e
				      WHERE e.tenant_id = c.tenant_id
				        AND e.project_id = c.project_id
				        AND e.id = c.execution_id
				        AND e.state IN ('queued', 'running')
				  ))`,
			Args: []any{candidate.ID, string(typed.TenantID), string(typed.ProjectID), candidate.CreatedAt, before, now},
		}
	case installation.RetentionLogs, installation.RetentionReceipts:
		return false, nil
	default:
		return false, fault.New(fault.InvalidInput, fmt.Sprintf("retention delete class %q is unknown", class))
	}

	tx, err := adapter.store.Begin(ctx, typed)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	result, err := tx.Exec(ctx, statement)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return result.RowsAffected > 0, nil
}

// RetainedCommits returns the distinct Git candidate commits named by
// git.ref.update verification rows that the evidence rule still retains in
// one project scope. It applies the same installation.RetentionPolicy the
// sweeper deletes through; it never carries a second window and never deletes a
// row. A tenant-wide scope has no project-scoped evidence and returns nothing.
func (adapter *RetentionStore) RetainedCommits(ctx context.Context, scope installation.RetentionScope, now time.Time) ([]string, error) {
	if ctx == nil {
		return nil, fault.New(fault.InvalidInput, "retained commit scan requires a context")
	}
	if now.IsZero() {
		return nil, fault.New(fault.InvalidInput, "retained commit scan requires a boundary time")
	}
	typed, err := retentionScope(scope)
	if err != nil {
		return nil, err
	}
	if typed.TenantScoped() {
		return nil, nil
	}
	policy := installation.RetentionPolicy{}
	window, err := policy.Window(installation.RetentionEvidence)
	if err != nil {
		return nil, err
	}
	before := now.Add(-window.Duration)
	tx, err := adapter.store.Begin(ctx, typed)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, store.Query{SQL: `
		SELECT COALESCE((
		           SELECT field->>'value'
		           FROM jsonb_array_elements(v.subject->'fields') field
		           WHERE field->>'name' = 'candidate'
		           LIMIT 1
		       ), ''), v.created_at,
		       EXISTS (
		           SELECT 1 FROM executions e
		           WHERE e.tenant_id = v.tenant_id
		             AND e.project_id = v.project_id
		             AND e.id = v.execution_id
		             AND e.state IN ('queued', 'running')
		       )
		FROM verifications v
		WHERE v.tenant_id = $1
		  AND v.project_id = $2
		  AND v.operation = 'git.ref.update'
		  AND (v.created_at >= $3 OR EXISTS (
		          SELECT 1 FROM executions e
		          WHERE e.tenant_id = v.tenant_id
		            AND e.project_id = v.project_id
		            AND e.id = v.execution_id
		            AND e.state IN ('queued', 'running')
		  ))
		ORDER BY v.created_at, v.id`, Args: []any{string(typed.TenantID), string(typed.ProjectID), before}})
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[string]bool)
	commits := make([]string, 0)
	for rows.Next() {
		var candidate string
		var createdAt time.Time
		var active bool
		if err := rows.Scan(&candidate, &createdAt, &active); err != nil {
			return nil, err
		}
		if candidate == "" || seen[candidate] {
			continue
		}
		retained, err := retainedCommit(policy, candidate, createdAt, now, active)
		if err != nil {
			return nil, err
		}
		if !retained {
			continue
		}
		seen[candidate] = true
		commits = append(commits, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	sort.Strings(commits)
	return commits, nil
}

// retainedCommit re-applies the evidence rule to one candidate
// row. It is the single decision point shared by the scan, so keep refs cannot
// drift into a second retention window.
func retainedCommit(policy installation.RetentionPolicy, candidate string, createdAt, now time.Time, active bool) (bool, error) {
	if candidate == "" {
		return false, nil
	}
	decision, err := policy.MayDelete(installation.RetentionEvidence, createdAt, now, active)
	if err != nil {
		return false, err
	}
	return !decision.Delete, nil
}

func (adapter *RetentionStore) queryCandidates(ctx context.Context, scope tenancy.Scope, query string, args ...any) ([]installation.RetentionCandidate, error) {
	tx, err := adapter.store.Begin(ctx, scope)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, store.Query{SQL: query, Args: args})
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	candidates := make([]installation.RetentionCandidate, 0)
	for rows.Next() {
		var candidate installation.RetentionCandidate
		if err := rows.Scan(&candidate.ID, &candidate.CreatedAt, &candidate.ActiveReferences); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return candidates, nil
}

func rawContentNow(before time.Time) (time.Time, error) {
	window, err := (installation.RetentionPolicy{RawContentOptIn: true}).Window(installation.RetentionRawContent)
	if err != nil {
		return time.Time{}, err
	}
	return before.Add(window.Duration), nil
}

func retentionScope(scope installation.RetentionScope) (tenancy.Scope, error) {
	if err := scope.Validate(); err != nil {
		return tenancy.Scope{}, err
	}
	typed := tenancy.Scope{
		TenantID:  id.TenantID(scope.TenantID),
		ProjectID: id.ProjectID(scope.ProjectID),
	}
	if err := typed.Validate(); err != nil {
		return tenancy.Scope{}, err
	}
	return typed, nil
}
