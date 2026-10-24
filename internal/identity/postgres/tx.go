package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/identity"
)

// txImpl is the identity.Tx bound to one tenant transaction.
type txImpl struct {
	tx     pgx.Tx
	tenant domain.TenantID
	events EventAppender

	projectLocked bool
	authLocked    bool
	recorded      bool
}

var _ identity.Tx = (*txImpl)(nil)

// errOrder reports a violation of the documented lock order; it is a
// programming error, not a business failure.
func errOrder(what string) error {
	return fmt.Errorf("identity/postgres: %s violates the lock order", what)
}

func (t *txImpl) guardWrite(what string) error {
	if t.recorded {
		return errOrder(what + " after Record")
	}
	return nil
}

func isPg(err error, code string, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

// ---- locks --------------------------------------------------------------

func (t *txImpl) LockProject(ctx context.Context, id domain.ProjectID) (identity.Project, error) {
	if t.projectLocked || t.authLocked || t.recorded {
		return identity.Project{}, errOrder("LockProject")
	}
	t.projectLocked = true
	return t.readProject(ctx, id, " FOR NO KEY UPDATE")
}

func (t *txImpl) LockAuthorization(ctx context.Context, changed []identity.UserID, observed []identity.UserID) error {
	if t.authLocked || t.recorded {
		return errOrder("LockAuthorization")
	}
	t.authLocked = true
	exclusive := map[string]bool{}
	for _, u := range changed {
		exclusive[string(u)] = true
	}
	users := make([]string, 0, len(changed)+len(observed))
	for u := range exclusive {
		users = append(users, u)
	}
	for _, u := range observed {
		if !exclusive[string(u)] {
			users = append(users, string(u))
			exclusive[string(u)] = false
		}
	}
	slices.SortFunc(users, strings.Compare) // byte order: every transaction locks in the same order
	for _, u := range users {
		if exclusive[u] {
			if _, err := t.tx.Exec(ctx, `INSERT INTO identity_authz_revisions (tenant_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, string(t.tenant), u); err != nil {
				if isPg(err, "23503", "") {
					return identity.ErrNotFound
				}
				return err
			}
			var rev int64
			if err := t.tx.QueryRow(ctx, `SELECT revision FROM identity_authz_revisions WHERE tenant_id = $1 AND user_id = $2 FOR UPDATE`, string(t.tenant), u).Scan(&rev); err != nil {
				return err
			}
			continue
		}
		// A user without a revision row has never held a binding: nothing to
		// serialize with, and the binding read below finds none.
		var rev int64
		err := t.tx.QueryRow(ctx, `SELECT revision FROM identity_authz_revisions WHERE tenant_id = $1 AND user_id = $2 FOR SHARE`, string(t.tenant), u).Scan(&rev)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}

func (t *txImpl) BumpAuthorization(ctx context.Context, user identity.UserID) (int64, error) {
	if err := t.guardWrite("BumpAuthorization"); err != nil {
		return 0, err
	}
	var rev int64
	err := t.tx.QueryRow(ctx, `UPDATE identity_authz_revisions SET revision = revision + 1, updated_at = now()
		WHERE tenant_id = $1 AND user_id = $2 RETURNING revision`, string(t.tenant), string(user)).Scan(&rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, errOrder("BumpAuthorization without LockAuthorization")
	}
	return rev, err
}

// ---- bindings -----------------------------------------------------------

func (t *txImpl) Bindings(ctx context.Context, user identity.UserID) ([]identity.Binding, error) {
	return readBindings(ctx, t.tx, t.tenant, user)
}

func (t *txImpl) Binding(ctx context.Context, id identity.BindingID) (identity.Binding, error) {
	b, err := scanBinding(t.tenant, t.tx.QueryRow(ctx, `SELECT `+bindingColumns+` FROM memberships WHERE tenant_id = $1 AND id = $2`, string(t.tenant), string(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.Binding{}, identity.ErrNotFound
	}
	return b, err
}

func (t *txImpl) ListBindings(ctx context.Context, q identity.BindingQuery) ([]identity.Binding, error) {
	sql := `SELECT ` + bindingColumns + ` FROM memberships WHERE tenant_id = $1`
	args := []any{string(t.tenant)}
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if !q.AllScopes {
		if len(q.Projects) == 0 {
			return nil, nil
		}
		ps := make([]string, len(q.Projects))
		for i, p := range q.Projects {
			ps[i] = string(p)
		}
		sql += ` AND project_id = ANY(` + arg(ps) + `)`
	}
	if q.Project != "" {
		sql += ` AND project_id = ` + arg(string(q.Project))
	}
	if q.User != "" {
		sql += ` AND user_id = ` + arg(string(q.User))
	}
	if q.After != "" {
		sql += ` AND id COLLATE "C" > ` + arg(string(q.After)) + ` COLLATE "C"`
	}
	sql += ` ORDER BY id COLLATE "C" LIMIT ` + arg(int32(q.Limit))
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []identity.Binding
	for rows.Next() {
		b, err := scanBinding(t.tenant, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (t *txImpl) InsertBinding(ctx context.Context, b identity.Binding) error {
	if err := t.guardWrite("InsertBinding"); err != nil {
		return err
	}
	if b.Tenant != t.tenant {
		return fmt.Errorf("%w: the binding belongs to another tenant", identity.ErrInvalid)
	}
	var project *string
	if b.Project != "" {
		p := string(b.Project)
		project = &p
	}
	_, err := t.tx.Exec(ctx, `INSERT INTO memberships (tenant_id, id, user_id, project_id, role, created_at, version) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		string(b.Tenant), string(b.ID), string(b.User), project, string(b.Role), micros(b.CreatedAt), int64(b.Version))
	switch {
	case isPg(err, "23505", ""):
		return identity.ErrConflict
	case isPg(err, "23503", ""):
		return identity.ErrNotFound
	}
	return err
}

func (t *txImpl) UpdateBinding(ctx context.Context, next identity.Binding, expected domain.Version) error {
	if err := t.guardWrite("UpdateBinding"); err != nil {
		return err
	}
	tag, err := t.tx.Exec(ctx, `UPDATE memberships SET role = $4, version = $5 WHERE tenant_id = $1 AND id = $2 AND version = $3`,
		string(t.tenant), string(next.ID), int64(expected), string(next.Role), int64(next.Version))
	if isPg(err, "23505", "") {
		return identity.ErrConflict
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return identity.ErrVersionConflict
	}
	return nil
}

func (t *txImpl) DeleteBinding(ctx context.Context, b identity.Binding) error {
	if err := t.guardWrite("DeleteBinding"); err != nil {
		return err
	}
	tag, err := t.tx.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND id = $2 AND version = $3`, string(t.tenant), string(b.ID), int64(b.Version))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return identity.ErrVersionConflict
	}
	return nil
}

// ---- projects -----------------------------------------------------------

const projectColumns = `id, key, display_name, settings, active_wip_limit, created_at, updated_at, version`

func (t *txImpl) scanProject(row pgx.Row) (identity.Project, error) {
	var p identity.Project
	var id string
	var settings []byte
	var version int64
	if err := row.Scan(&id, &p.Key, &p.DisplayName, &settings, &p.ActiveWIPLimit, &p.CreatedAt, &p.UpdatedAt, &version); err != nil {
		return identity.Project{}, err
	}
	p.Scope = domain.Scope{TenantID: t.tenant, ProjectID: domain.ProjectID(id)}
	p.Settings, p.Version = settings, domain.Version(version)
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	return p, nil
}

func (t *txImpl) readProject(ctx context.Context, id domain.ProjectID, lock string) (identity.Project, error) {
	p, err := t.scanProject(t.tx.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE tenant_id = $1 AND id = $2`+lock, string(t.tenant), string(id)))
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.Project{}, identity.ErrNotFound
	}
	return p, err
}

func (t *txImpl) Project(ctx context.Context, id domain.ProjectID) (identity.Project, error) {
	return t.readProject(ctx, id, "")
}

func (t *txImpl) ListProjects(ctx context.Context, q identity.ProjectQuery) ([]identity.Project, error) {
	sql := `SELECT ` + projectColumns + ` FROM projects WHERE tenant_id = $1`
	args := []any{string(t.tenant)}
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if !q.All {
		if len(q.Only) == 0 {
			return nil, nil
		}
		ps := make([]string, len(q.Only))
		for i, p := range q.Only {
			ps[i] = string(p)
		}
		sql += ` AND id = ANY(` + arg(ps) + `)`
	}
	if q.After != "" {
		sql += ` AND id COLLATE "C" > ` + arg(string(q.After)) + ` COLLATE "C"`
	}
	sql += ` ORDER BY id COLLATE "C" LIMIT ` + arg(int32(q.Limit))
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []identity.Project
	for rows.Next() {
		p, err := t.scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (t *txImpl) InsertProject(ctx context.Context, p identity.Project) error {
	if err := t.guardWrite("InsertProject"); err != nil {
		return err
	}
	if p.Scope.TenantID != t.tenant {
		return fmt.Errorf("%w: the project belongs to another tenant", identity.ErrInvalid)
	}
	_, err := t.tx.Exec(ctx, `INSERT INTO projects (tenant_id, id, key, display_name, settings, active_wip_limit, created_at, updated_at, version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		string(p.Scope.TenantID), string(p.Scope.ProjectID), p.Key, p.DisplayName, []byte(p.Settings), p.ActiveWIPLimit, micros(p.CreatedAt), micros(p.UpdatedAt), int64(p.Version))
	if isPg(err, "23505", "") {
		return identity.ErrConflict
	}
	return err
}

func (t *txImpl) UpdateProject(ctx context.Context, next identity.Project, expected domain.Version) error {
	if err := t.guardWrite("UpdateProject"); err != nil {
		return err
	}
	tag, err := t.tx.Exec(ctx, `UPDATE projects SET display_name = $4, settings = $5, active_wip_limit = $6, updated_at = $7, version = $8
		WHERE tenant_id = $1 AND id = $2 AND version = $3`,
		string(t.tenant), string(next.Scope.ProjectID), int64(expected), next.DisplayName, []byte(next.Settings), next.ActiveWIPLimit, micros(next.UpdatedAt), int64(next.Version))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return identity.ErrVersionConflict
	}
	return nil
}
