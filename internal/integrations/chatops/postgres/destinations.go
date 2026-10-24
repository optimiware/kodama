package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/integrations/chatops"
)

const destinationColumns = `tenant_id, project_id, id, name, endpoint, events, credential, enabled, binding_revision, version, created_at, updated_at`

func scanDestination(row pgx.Row) (chatops.Destination, error) {
	var d chatops.Destination
	var tenant, project, id string
	if err := row.Scan(&tenant, &project, &id, &d.Name, &d.Endpoint, &d.Events, &d.Credential, &d.Enabled, &d.BindingRevision, &d.Version,
		&d.CreatedAt, &d.UpdatedAt); err != nil {
		return chatops.Destination{}, err
	}
	d.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	d.ID = chatops.DestinationID(id)
	d.CreatedAt, d.UpdatedAt = d.CreatedAt.UTC(), d.UpdatedAt.UTC()
	if err := d.Validate(); err != nil {
		return chatops.Destination{}, err
	}
	return d, nil
}

// CreateDestination implements chatops.Store. The project lock serializes
// creators, so the count and the insert are exact against the limit.
func (s *Store) CreateDestination(ctx context.Context, c chatops.Caller, d chatops.Destination, limit int) (out chatops.Destination, err error) {
	if err := d.Validate(); err != nil {
		return chatops.Destination{}, err
	}
	err = s.protected(ctx, c, d.Scope, chatops.PermSettings, true, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM chatops_destinations WHERE tenant_id = $1 AND project_id = $2`,
			string(d.Scope.TenantID), string(d.Scope.ProjectID)).Scan(&n); err != nil {
			return err
		}
		if n >= limit {
			return chatops.Fail(chatops.ErrLimit, nil, "the project has the maximum number of destinations")
		}
		_, err := tx.Exec(ctx, `INSERT INTO chatops_destinations (`+destinationColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			string(d.Scope.TenantID), string(d.Scope.ProjectID), string(d.ID), d.Name, d.Endpoint, d.Events, d.Credential, d.Enabled,
			d.BindingRevision, d.Version, d.CreatedAt, d.UpdatedAt)
		if err != nil {
			if constraint(err) == "chatops_destinations_name_unique" {
				return chatops.Fail(chatops.ErrNameTaken, err, "the destination name is used in this project")
			}
			return err
		}
		out = d
		return audit(ctx, tx, callerAudit(c, d.Scope, d.CreatedAt, "chatops.destination.create", "chatops_destination", string(d.ID), d.Version, "",
			map[string]any{"name": d.Name, "enabled": d.Enabled, "events": d.Events, "authenticated": d.Credential != ""}))
	})
	return out, err
}

// GetDestination implements chatops.Store.
func (s *Store) GetDestination(ctx context.Context, c chatops.Caller, scope domain.Scope, id chatops.DestinationID) (out chatops.Destination, err error) {
	err = s.protected(ctx, c, scope, chatops.PermSettings, false, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		out, e = scanDestination(tx.QueryRow(ctx, `SELECT `+destinationColumns+` FROM chatops_destinations WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(id)))
		if errors.Is(e, pgx.ErrNoRows) {
			return chatops.Fail(chatops.ErrNotFound, e, "not found")
		}
		return e
	})
	return out, err
}

// ListDestinations implements chatops.Store.
func (s *Store) ListDestinations(ctx context.Context, c chatops.Caller, scope domain.Scope, after chatops.DestinationID, limit int) (out []chatops.Destination, err error) {
	err = s.protected(ctx, c, scope, chatops.PermSettings, false, func(ctx context.Context, tx pgx.Tx) error {
		rows, e := tx.Query(ctx, `SELECT `+destinationColumns+` FROM chatops_destinations
WHERE tenant_id = $1 AND project_id = $2 AND id > $3 ORDER BY id LIMIT $4`, string(scope.TenantID), string(scope.ProjectID), string(after), limit)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			d, e := scanDestination(rows)
			if e != nil {
				return e
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	return out, err
}

// UpdateDestination implements chatops.Store. The row is locked, compared by
// the entity against the expected version and written with its audit entry in
// the same transaction; the version comparison is part of the write.
func (s *Store) UpdateDestination(ctx context.Context, c chatops.Caller, scope domain.Scope, id chatops.DestinationID, expected int64, edit chatops.DestinationEdit, now time.Time) (out chatops.Destination, err error) {
	err = s.protected(ctx, c, scope, chatops.PermSettings, true, func(ctx context.Context, tx pgx.Tx) error {
		d, e := scanDestination(tx.QueryRow(ctx, `SELECT `+destinationColumns+` FROM chatops_destinations
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`, string(scope.TenantID), string(scope.ProjectID), string(id)))
		if errors.Is(e, pgx.ErrNoRows) {
			return chatops.Fail(chatops.ErrNotFound, e, "not found")
		}
		if e != nil {
			return e
		}
		before := d
		changed, e := d.Edit(expected, edit, now)
		if e != nil {
			return e
		}
		out = d
		if !changed {
			return nil
		}
		tag, e := tx.Exec(ctx, `UPDATE chatops_destinations SET name = $4, endpoint = $5, events = $6, credential = $7, enabled = $8,
binding_revision = $9, version = $10, updated_at = $11 WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $12`,
			string(scope.TenantID), string(scope.ProjectID), string(id), d.Name, d.Endpoint, d.Events, d.Credential, d.Enabled,
			d.BindingRevision, d.Version, d.UpdatedAt, expected)
		if e != nil {
			if constraint(e) == "chatops_destinations_name_unique" {
				return chatops.Fail(chatops.ErrNameTaken, e, "the destination name is used in this project")
			}
			return e
		}
		if tag.RowsAffected() != 1 {
			return chatops.Fail(chatops.ErrVersionConflict, nil, "the destination changed")
		}
		return audit(ctx, tx, callerAudit(c, scope, now, "chatops.destination.update", "chatops_destination", string(id), d.Version, "",
			map[string]any{"fromVersion": before.Version, "enabled": d.Enabled, "bindingRevision": d.BindingRevision,
				"rebound": d.BindingRevision != before.BindingRevision, "authenticated": d.Credential != ""}))
	})
	if err != nil {
		return chatops.Destination{}, err
	}
	return out, nil
}
