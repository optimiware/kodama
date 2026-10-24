package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

// verifyUse decides, inside tx and in the module-wide lock order, whether a
// session may be used now. It is the one protected-use check: Admit runs it
// before charging a call or recording dispatch intent, and CheckSessionUse runs
// it for discovery and for the periodic check of an in-flight call.
//
// Lock order: runtime authority (execution attempt first, grant second), the
// registration row FOR SHARE, the session row FOR SHARE, then the credential:
// secret_versions row FOR SHARE and the run's secret_pins row FOR SHARE, the
// order the secrets module's resolution finalization uses (version, then pin).
// Revocation takes the version row FOR UPDATE and pin release updates the pin
// row, so each is totally ordered against this transaction: revocation or
// release that committed first is observed here and nothing is admitted; an
// admission that committed first is attributable and its effect is settled or
// recovered as unknown.
//
// The check reads metadata only. It never reads ciphertext, decrypts, calls a
// key provider or the network, and writes nothing. The returned session is the
// locked row.
func (s *Store) verifyUse(ctx context.Context, tx pgx.Tx, u mcpproxy.SessionUse) (mcpproxy.RemoteSession, error) {
	c := u.Caller
	if err := s.opt.Recheck(ctx, tx, c, u.Now); err != nil {
		return mcpproxy.RemoteSession{}, err
	}
	var sv int64
	var deleted *time.Time
	if err := tx.QueryRow(ctx, `SELECT version, deleted_at FROM mcp_servers WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR SHARE`,
		string(c.Scope.TenantID), string(c.Scope.ProjectID), string(u.Registration)).Scan(&sv, &deleted); err != nil {
		return mcpproxy.RemoteSession{}, noRows(err, mcpproxy.ErrNotFound)
	}
	sess, err := scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM mcp_sessions WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR SHARE`,
		string(c.Scope.TenantID), string(c.Scope.ProjectID), string(u.Session)))
	if err != nil {
		return mcpproxy.RemoteSession{}, noRows(err, mcpproxy.ErrSessionInvalid)
	}
	if !sess.Owns(c) || sess.Registration != u.Registration {
		return mcpproxy.RemoteSession{}, mcpproxy.ErrSessionInvalid
	}
	if err := sess.Usable(u.Now, u.Incarnation); err != nil {
		return mcpproxy.RemoteSession{}, err
	}
	if deleted != nil || domain.Version(sv) != sess.RegistrationVersion {
		return mcpproxy.RemoteSession{}, mcpproxy.ErrBindingInvalid
	}
	if err := s.checkCredentialUse(ctx, tx, sess); err != nil {
		return mcpproxy.RemoteSession{}, err
	}
	return sess, nil
}

// checkCredentialUse verifies, for the session's frozen run binding, that the
// exact credential version is still available to the run: the binding still
// names what the session froze, the version exists and is not revoked, and the
// run's pin of it exists and is unreleased. Secret deletion alone does not end
// use: deletion closes new pins, and a retained pinned version stays available
// to its existing consumers (the secrets module's resolution makes the same
// distinction). Anything else is ErrCredential; a binding that differs from the
// session is ErrBindingInvalid. Predicates carry tenant and project explicitly.
func (s *Store) checkCredentialUse(ctx context.Context, tx pgx.Tx, sess mcpproxy.RemoteSession) error {
	c := sess.Caller
	tenant, project := string(c.Scope.TenantID), string(c.Scope.ProjectID)
	var bv, bsv int64
	var bname string
	if err := tx.QueryRow(ctx, `SELECT server_version, secret_name, secret_version FROM mcp_run_bindings
		WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 AND server_id = $4`,
		tenant, project, string(c.RunID), string(sess.Registration)).Scan(&bv, &bname, &bsv); err != nil {
		return noRows(err, mcpproxy.ErrBindingInvalid)
	}
	if domain.Version(bv) != sess.RegistrationVersion || bname != sess.Credential.Name || bsv != sess.Credential.Version {
		return mcpproxy.ErrBindingInvalid
	}
	var secretID string
	if err := tx.QueryRow(ctx, `SELECT id FROM secrets WHERE tenant_id = $1 AND project_id = $2 AND name = $3`,
		tenant, project, sess.Credential.Name).Scan(&secretID); err != nil {
		return noRows(err, mcpproxy.ErrCredential)
	}
	var revoked bool
	if err := tx.QueryRow(ctx, `SELECT revoked_at IS NOT NULL FROM secret_versions
		WHERE tenant_id = $1 AND project_id = $2 AND secret_id = $3 AND version = $4 FOR SHARE`,
		tenant, project, secretID, sess.Credential.Version).Scan(&revoked); err != nil {
		return noRows(err, mcpproxy.ErrCredential)
	}
	if revoked {
		return mcpproxy.ErrCredential
	}
	var released bool
	if err := tx.QueryRow(ctx, `SELECT released_at IS NOT NULL FROM secret_pins
		WHERE tenant_id = $1 AND project_id = $2 AND consumer_kind = 'run' AND consumer_id = $3 AND secret_id = $4 AND secret_version = $5 FOR SHARE`,
		tenant, project, string(c.RunID), secretID, sess.Credential.Version).Scan(&released); err != nil {
		return noRows(err, mcpproxy.ErrCredential)
	}
	if released {
		return mcpproxy.ErrCredential
	}
	return nil
}

// CheckSessionUse implements mcpproxy.Store: verifyUse in its own short
// transaction. The authority check takes row locks, so it cannot run in a
// read-only transaction.
func (s *Store) CheckSessionUse(ctx context.Context, u mcpproxy.SessionUse) error {
	if err := u.Caller.Validate(); err != nil {
		return err
	}
	if err := u.Session.Validate(); err != nil {
		return mcpproxy.ErrSessionInvalid
	}
	_, err := s.do(ctx, u.Caller.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.verifyUse(ctx, tx, u)
		return err
	})
	return classify(err)
}
