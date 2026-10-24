package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

// Recheck verifies current authority in its own short transaction.
func (s *Store) Recheck(ctx context.Context, c mcpproxy.Caller, now time.Time) error {
	if err := c.Validate(); err != nil {
		return err
	}
	// The authority check takes row locks (attempt FOR SHARE), so it cannot run in
	// a read-only transaction.
	_, err := s.do(ctx, c.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.opt.Recheck(ctx, tx, c, now)
	})
	return classify(err)
}

// Admit atomically admits one call. In one transaction and in the module-wide
// lock order it rechecks current runtime authority (attempt first, grant
// second), locks the registration FOR SHARE so an edit or deletion is ordered
// against it, locks and validates the session, inserts the dispatching receipt
// (a duplicate request returns the stored receipt and charges nothing), charges
// the run-wide counter against the claimed ceiling and appends the audit entry.
//
// A commit whose outcome is unknown is reported as ErrAdmissionUncertain: the
// service dispatches only on a nil error, so an uncertain admission never
// sends.
func (s *Store) Admit(ctx context.Context, req mcpproxy.AdmitRequest) (mcpproxy.Admission, error) {
	inv := req.Invocation
	c := inv.Caller
	if err := c.Validate(); err != nil {
		return mcpproxy.Admission{}, err
	}
	if inv.State != mcpproxy.InvocationDispatching {
		return mcpproxy.Admission{}, fmt.Errorf("%w: only a dispatching receipt is admitted", mcpproxy.ErrInvalid)
	}
	var out mcpproxy.Admission
	unknown, err := s.do(ctx, c.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Authority, registration, session and the exact credential version are
		// verified in one lock order before anything is charged or recorded.
		if _, err := s.verifyUse(ctx, tx, mcpproxy.SessionUse{Caller: c, Registration: inv.Registration, Session: inv.Session,
			Incarnation: req.Incarnation, Now: req.Now}); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO mcp_invocations (tenant_id, project_id, session_id, id, run_id, attempt_id, server_id, tool, state, code, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'dispatching', '', $9, $9) ON CONFLICT (tenant_id, session_id, id) DO NOTHING`,
			string(c.Scope.TenantID), string(c.Scope.ProjectID), string(inv.Session), string(inv.ID), string(c.RunID), string(c.AttemptID),
			string(inv.Registration), inv.Tool, req.Now.UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			existing, err := getInvocation(ctx, tx, c.Scope, inv.Session, inv.ID)
			if err != nil {
				return err
			}
			out = mcpproxy.Admission{Invocation: existing, Duplicate: true}
			return nil
		}
		var calls int64
		err = tx.QueryRow(ctx, `INSERT INTO mcp_run_calls (tenant_id, project_id, run_id, calls) VALUES ($1, $2, $3, 1)
			ON CONFLICT (tenant_id, run_id) DO UPDATE SET calls = mcp_run_calls.calls + 1
			WHERE $4::bigint IS NULL OR mcp_run_calls.calls < $4::bigint RETURNING calls`,
			string(c.Scope.TenantID), string(c.Scope.ProjectID), string(c.RunID), req.MaxCalls).Scan(&calls)
		if errors.Is(err, pgx.ErrNoRows) {
			return mcpproxy.ErrLimitReached
		}
		if err != nil {
			return err
		}
		if req.MaxCalls != nil && calls > *req.MaxCalls {
			return mcpproxy.ErrLimitReached
		}
		out = mcpproxy.Admission{Invocation: inv}
		return s.audit(ctx, tx, auditEntry{Scope: c.Scope, ActorType: "workload", ActorID: string(c.AttemptID), Action: "mcp_tool.admitted",
			Resource: string(inv.Registration), RequestID: string(inv.ID), At: req.Now,
			Metadata: map[string]any{"runId": string(c.RunID), "attemptId": string(c.AttemptID), "tool": inv.Tool, "callNumber": calls}})
	})
	if unknown {
		return mcpproxy.Admission{}, fmt.Errorf("%w: %w: %v", mcpproxy.ErrUnavailable, mcpproxy.ErrAdmissionUncertain, err)
	}
	if err != nil {
		return mcpproxy.Admission{}, classify(err)
	}
	return out, nil
}

const invocationColumns = `tenant_id, project_id, session_id, id, run_id, attempt_id, server_id, tool, state, code, created_at, updated_at`

func getInvocation(ctx context.Context, tx pgx.Tx, scope domain.Scope, session mcpproxy.SessionID, id mcpproxy.RequestID) (mcpproxy.Invocation, error) {
	var (
		i                                         mcpproxy.Invocation
		tenant, project, sess, rid, run, att, srv string
		state                                     string
	)
	err := tx.QueryRow(ctx, `SELECT `+invocationColumns+` FROM mcp_invocations WHERE tenant_id = $1 AND project_id = $2 AND session_id = $3 AND id = $4`,
		string(scope.TenantID), string(scope.ProjectID), string(session), string(id)).
		Scan(&tenant, &project, &sess, &rid, &run, &att, &srv, &i.Tool, &state, &i.Code, &i.CreatedAt, &i.UpdatedAt)
	if err != nil {
		return mcpproxy.Invocation{}, noRows(err, mcpproxy.ErrNotFound)
	}
	i.ID, i.Session, i.Registration, i.State = mcpproxy.RequestID(rid), mcpproxy.SessionID(sess), mcpproxy.RegistrationID(srv), mcpproxy.InvocationState(state)
	i.Caller = mcpproxy.Caller{Scope: scope, RunID: domain.RunID(run), AttemptID: domain.AttemptID(att)}
	i.CreatedAt, i.UpdatedAt = i.CreatedAt.UTC(), i.UpdatedAt.UTC()
	return i, nil
}

// GetInvocation reads one receipt.
func (s *Store) GetInvocation(ctx context.Context, scope domain.Scope, session mcpproxy.SessionID, id mcpproxy.RequestID) (mcpproxy.Invocation, error) {
	var out mcpproxy.Invocation
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = getInvocation(ctx, tx, scope, session, id)
		return err
	})
	return out, classify(err)
}

// Complete records the terminal outcome of a dispatching receipt with its audit
// entry. The database guard makes the outcome final: an unknown receipt is never
// rewritten.
func (s *Store) Complete(ctx context.Context, inv mcpproxy.Invocation) error {
	c := inv.Caller
	if !inv.State.Terminal() {
		return fmt.Errorf("%w: only a terminal outcome is recorded", mcpproxy.ErrInvalid)
	}
	unknown, err := s.do(ctx, c.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE mcp_invocations SET state = $5, code = $6, updated_at = $7
			WHERE tenant_id = $1 AND project_id = $2 AND session_id = $3 AND id = $4 AND state = 'dispatching'`,
			string(c.Scope.TenantID), string(c.Scope.ProjectID), string(inv.Session), string(inv.ID), string(inv.State), inv.Code, inv.UpdatedAt.UTC())
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return mcpproxy.ErrInvalidTransition
		}
		return s.audit(ctx, tx, auditEntry{Scope: c.Scope, ActorType: "workload", ActorID: string(c.AttemptID), Action: "mcp_tool.completed",
			Resource: string(inv.Registration), RequestID: string(inv.ID), At: inv.UpdatedAt,
			Metadata: map[string]any{"runId": string(c.RunID), "attemptId": string(c.AttemptID), "tool": inv.Tool, "outcome": string(inv.State), "code": inv.Code}})
	})
	if unknown {
		return mcpproxy.Unavailablef(err, "the outcome commit is uncertain; the receipt is recoverable")
	}
	return classify(err)
}

// RecoverStale turns dispatching receipts older than the cutoff into unknown,
// with an audit entry each. It never requeues a call.
func (s *Store) RecoverStale(ctx context.Context, tenant domain.TenantID, before time.Time, limit int) (int, error) {
	var n int
	_, err := s.do(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE mcp_invocations SET state = 'unknown', code = 'recovered', updated_at = now()
			WHERE tenant_id = $1 AND (tenant_id, session_id, id) IN (
				SELECT tenant_id, session_id, id FROM mcp_invocations WHERE tenant_id = $1 AND state = 'dispatching' AND updated_at < $2
				ORDER BY updated_at LIMIT $3)
			RETURNING project_id, server_id, id, run_id, attempt_id, tool`, string(tenant), before.UTC(), limit)
		if err != nil {
			return err
		}
		type rec struct{ project, server, id, run, attempt, tool string }
		var recs []rec
		for rows.Next() {
			var r rec
			if err := rows.Scan(&r.project, &r.server, &r.id, &r.run, &r.attempt, &r.tool); err != nil {
				rows.Close()
				return err
			}
			recs = append(recs, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range recs {
			if err := s.audit(ctx, tx, auditEntry{Scope: domain.Scope{TenantID: tenant, ProjectID: domain.ProjectID(r.project)}, ActorType: "system",
				ActorID: "mcp-proxy-recovery", Action: "mcp_tool.recovered", Resource: r.server, RequestID: r.id, At: time.Now(),
				Metadata: map[string]any{"runId": r.run, "attemptId": r.attempt, "tool": r.tool, "outcome": "unknown", "code": "recovered"}}); err != nil {
				return err
			}
		}
		n = len(recs)
		return nil
	})
	return n, classify(err)
}
