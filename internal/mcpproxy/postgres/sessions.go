package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

// EnsureBinding freezes, on first authorized use, the registration revision and
// exact credential version for the caller's run, and registers the secret pin
// in the same transaction. Authority is rechecked first, in the module-wide
// lock order, so a revoked attempt creates no binding. The registration row is
// locked FOR SHARE: an edit or deletion either committed first (the binding is
// refused) or waits for this transaction.
func (s *Store) EnsureBinding(ctx context.Context, req mcpproxy.BindingRequest) (mcpproxy.Registration, error) {
	c := req.Caller
	if err := c.Validate(); err != nil {
		return mcpproxy.Registration{}, err
	}
	var out mcpproxy.Registration
	unknown, err := s.do(ctx, c.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.opt.Recheck(ctx, tx, c, req.Now); err != nil {
			return err
		}
		r, err := scanServer(tx.QueryRow(ctx, `SELECT `+serverColumns+` FROM mcp_servers WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR SHARE`,
			string(c.Scope.TenantID), string(c.Scope.ProjectID), string(req.Registration)))
		if err != nil {
			return noRows(err, mcpproxy.ErrNotFound)
		}
		if r.Deleted() {
			return mcpproxy.ErrNotFound
		}
		var version int64
		var secretName string
		var secretVersion int64
		err = tx.QueryRow(ctx, `SELECT server_version, secret_name, secret_version FROM mcp_run_bindings
			WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND server_id = $4`,
			string(c.Scope.TenantID), string(c.Scope.ProjectID), string(c.RunID), string(r.ID)).Scan(&version, &secretName, &secretVersion)
		switch {
		case err == nil:
			// A registration change after the first use invalidates the binding
			// rather than silently replacing the credential.
			if domain.Version(version) != r.Version || secretName != r.Secret.Name || secretVersion != r.Secret.Version {
				return mcpproxy.ErrBindingInvalid
			}
			out = r
			return nil
		case !isNoRows(err):
			return err
		}
		if err := s.opt.Pin(ctx, tx, c.Scope, c.RunID, r.Secret.Name, r.Secret.Version); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO mcp_run_bindings (tenant_id, project_id, run_id, server_id, server_version, secret_name, secret_version, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, string(c.Scope.TenantID), string(c.Scope.ProjectID), string(c.RunID), string(r.ID),
			int64(r.Version), r.Secret.Name, r.Secret.Version, req.Now.UTC()); err != nil {
			return err
		}
		out = r
		return s.audit(ctx, tx, auditEntry{Scope: c.Scope, ActorType: "workload", ActorID: string(c.AttemptID), Action: "mcp_server.credential_bound",
			Resource: string(r.ID), Version: int64(r.Version), At: req.Now,
			Metadata: map[string]any{"runId": string(c.RunID), "attemptId": string(c.AttemptID), "secretName": r.Secret.Name, "secretVersion": r.Secret.Version}})
	})
	if unknown {
		return mcpproxy.Registration{}, mcpproxy.Unavailablef(err, "the binding commit outcome is unknown")
	}
	if err != nil {
		return mcpproxy.Registration{}, classify(err)
	}
	return out, nil
}

func isNoRows(err error) bool { return err == pgx.ErrNoRows }

const sessionColumns = `tenant_id, project_id, id, run_id, attempt_id, runtime_session, server_id, server_version, secret_name, secret_version,
	protocol, incarnation, state, created_at, updated_at, expires_at`

func scanSession(row pgx.Row) (mcpproxy.RemoteSession, error) {
	var (
		s                                                  mcpproxy.RemoteSession
		tenant, project, id, run, attempt, runtime, server string
		serverVersion                                      int64
		state                                              string
	)
	if err := row.Scan(&tenant, &project, &id, &run, &attempt, &runtime, &server, &serverVersion, &s.Credential.Name, &s.Credential.Version,
		&s.Protocol, &s.Incarnation, &state, &s.CreatedAt, &s.UpdatedAt, &s.ExpiresAt); err != nil {
		return mcpproxy.RemoteSession{}, err
	}
	s.ID, s.State = mcpproxy.SessionID(id), mcpproxy.SessionState(state)
	s.Caller = mcpproxy.Caller{Scope: domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)},
		RunID: domain.RunID(run), AttemptID: domain.AttemptID(attempt), Session: runtime}
	s.Registration, s.RegistrationVersion = mcpproxy.RegistrationID(server), domain.Version(serverVersion)
	s.CreatedAt, s.UpdatedAt, s.ExpiresAt = s.CreatedAt.UTC(), s.UpdatedAt.UTC(), s.ExpiresAt.UTC()
	return s, nil
}

// CreateSession inserts an initializing session. The composite foreign key to
// the run binding means a session can only exist for a binding frozen at the
// same registration.
func (s *Store) CreateSession(ctx context.Context, sess mcpproxy.RemoteSession) error {
	c := sess.Caller
	if sess.State != mcpproxy.SessionInitializing || c.Validate() != nil {
		return fmt.Errorf("%w: a new session must be initializing", mcpproxy.ErrInvalid)
	}
	unknown, err := s.do(ctx, c.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		// The binding must still be the frozen revision this session names.
		var v int64
		var name string
		var sv int64
		if err := tx.QueryRow(ctx, `SELECT server_version, secret_name, secret_version FROM mcp_run_bindings
			WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND server_id = $4`,
			string(c.Scope.TenantID), string(c.Scope.ProjectID), string(c.RunID), string(sess.Registration)).Scan(&v, &name, &sv); err != nil {
			return noRows(err, mcpproxy.ErrBindingInvalid)
		}
		if domain.Version(v) != sess.RegistrationVersion || name != sess.Credential.Name || sv != sess.Credential.Version {
			return mcpproxy.ErrBindingInvalid
		}
		_, err := tx.Exec(ctx, `INSERT INTO mcp_sessions (tenant_id, project_id, id, run_id, attempt_id, runtime_session, server_id, server_version,
			secret_name, secret_version, protocol, incarnation, state, created_at, updated_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'initializing', $13, $13, $14)`,
			string(c.Scope.TenantID), string(c.Scope.ProjectID), string(sess.ID), string(c.RunID), string(c.AttemptID), c.Session,
			string(sess.Registration), int64(sess.RegistrationVersion), sess.Credential.Name, sess.Credential.Version,
			sess.Protocol, sess.Incarnation, sess.CreatedAt.UTC(), sess.ExpiresAt.UTC())
		return err
	})
	if unknown {
		return mcpproxy.Unavailablef(err, "the session commit outcome is unknown")
	}
	return classify(err)
}

// GetSession reads one session of the scope.
func (s *Store) GetSession(ctx context.Context, scope domain.Scope, id mcpproxy.SessionID) (mcpproxy.RemoteSession, error) {
	var out mcpproxy.RemoteSession
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM mcp_sessions WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(id)))
		if err != nil {
			return noRows(err, mcpproxy.ErrNotFound)
		}
		out = r
		return nil
	})
	return out, classify(err)
}

// SaveSession persists a state transition from the given state.
func (s *Store) SaveSession(ctx context.Context, sess mcpproxy.RemoteSession, from mcpproxy.SessionState) (bool, error) {
	c := sess.Caller
	var changed bool
	_, err := s.do(ctx, c.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE mcp_sessions SET state = $5, updated_at = $6
			WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND state = $4`,
			string(c.Scope.TenantID), string(c.Scope.ProjectID), string(sess.ID), string(from), string(sess.State), sess.UpdatedAt.UTC())
		changed = err == nil && tag.RowsAffected() == 1
		return err
	})
	if err != nil {
		return false, classify(err)
	}
	return changed, nil
}

// ExpireSessions marks the tenant's live sessions past their expiry as expired.
func (s *Store) ExpireSessions(ctx context.Context, tenant domain.TenantID, now time.Time, limit int) (int, error) {
	var n int
	_, err := s.do(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE mcp_sessions SET state = 'expired', updated_at = $2 WHERE tenant_id = $1 AND (tenant_id, id) IN (
			SELECT tenant_id, id FROM mcp_sessions WHERE tenant_id = $1 AND state IN ('initializing', 'active') AND expires_at <= $2
			ORDER BY expires_at LIMIT $3)`, string(tenant), now.UTC(), limit)
		n = int(tag.RowsAffected())
		return err
	})
	return n, classify(err)
}
