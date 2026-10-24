package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gateway"
)

const settingsColumns = `tenant_id, project_id, revision, enabled, endpoint, credential_ref, credential_version, models,
	max_requests, token_allowance, max_request_bytes, max_input_tokens, max_output_tokens, recording, updated_at, updated_by`

func scanSettings(r rowScanner) (gateway.ModelSettings, error) {
	var (
		s               gateway.ModelSettings
		tenant, project string
		endpoint        string
		l               gateway.Limits
	)
	if err := r.Scan(&tenant, &project, &s.Revision, &s.Enabled, &endpoint, &s.Credential.Ref, &s.Credential.Version, &s.Models,
		&l.MaxRequests, &l.TokenAllowance, &l.MaxRequestBytes, &l.MaxInputTokens, &l.MaxOutputTokens, &s.Recording, &s.UpdatedAt, &s.UpdatedBy); err != nil {
		return gateway.ModelSettings{}, err
	}
	ep, err := gateway.ParseEndpoint(endpoint)
	if err != nil {
		return gateway.ModelSettings{}, gateway.FailWith(gateway.ErrUnavailable, err, "stored settings are inconsistent")
	}
	s.Scope = domain.Scope{TenantID: domain.TenantID(tenant), ProjectID: domain.ProjectID(project)}
	s.Endpoint, s.Limits, s.UpdatedAt = ep, l, s.UpdatedAt.UTC()
	return s, nil
}

func loadSettings(ctx context.Context, tx pgx.Tx, scope domain.Scope, lock string) (gateway.ModelSettings, error) {
	s, err := scanSettings(tx.QueryRow(ctx, `SELECT `+settingsColumns+` FROM model_settings WHERE tenant_id = $1 AND project_id = $2`+lock,
		string(scope.TenantID), string(scope.ProjectID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.Disabled(scope), nil
	}
	return s, err
}

// Settings implements gateway.Store.
func (s *Store) Settings(ctx context.Context, scope domain.Scope) (gateway.ModelSettings, error) {
	if err := scope.Validate(); err != nil {
		return gateway.ModelSettings{}, gateway.Failf(gateway.ErrInvalid, "the scope is malformed")
	}
	var out gateway.ModelSettings
	err := s.read(ctx, scope.TenantID, func(ctx context.Context, tx pgx.Tx) (err error) {
		out, err = loadSettings(ctx, tx, scope, "")
		return err
	})
	return out, err
}

func insertSettings(ctx context.Context, tx pgx.Tx, table string, s gateway.ModelSettings) (int64, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO `+table+` (`+settingsColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		string(s.Scope.TenantID), string(s.Scope.ProjectID), s.Revision, s.Enabled, s.Endpoint.String(), s.Credential.Ref, s.Credential.Version, s.Models,
		s.Limits.MaxRequests, s.Limits.TokenAllowance, s.Limits.MaxRequestBytes, s.Limits.MaxInputTokens, s.Limits.MaxOutputTokens, s.Recording, s.UpdatedAt, s.UpdatedBy)
	return tag.RowsAffected(), err
}

// UpdateSettings implements gateway.Store. The actor's permission is rechecked
// inside this transaction, the expected revision is compared with the locked
// row, and the revision, the audit entry and project.settings_changed commit
// together or not at all.
func (s *Store) UpdateSettings(ctx context.Context, cmd gateway.UpdateSettings) (gateway.SettingsResult, error) {
	if err := cmd.Scope.Validate(); err != nil {
		return gateway.SettingsResult{}, gateway.Failf(gateway.ErrInvalid, "the scope is malformed")
	}
	var res gateway.SettingsResult
	err := s.atomically(ctx, cmd.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.management(tx).Authorize(ctx, cmd.Actor, cmd.Scope, gateway.PermSettings); err != nil {
			return err
		}
		cur, err := loadSettings(ctx, tx, cmd.Scope, " FOR UPDATE")
		if err != nil {
			return err
		}
		next, changed, err := cur.Apply(cmd.Expected, cmd.Edit, cmd.Actor.ID, cmd.Now)
		if err != nil {
			return err
		}
		if len(changed) == 0 {
			res = gateway.SettingsResult{Settings: cur}
			return nil
		}
		if _, err := insertSettings(ctx, tx, "model_settings_revisions", next); err != nil {
			return err
		}
		if cur.Stored() {
			tag, err := tx.Exec(ctx, `UPDATE model_settings SET revision = $3, enabled = $4, endpoint = $5, credential_ref = $6, credential_version = $7,
				models = $8, max_requests = $9, token_allowance = $10, max_request_bytes = $11, max_input_tokens = $12, max_output_tokens = $13,
				recording = $14, updated_at = $15, updated_by = $16
				WHERE tenant_id = $1 AND project_id = $2 AND revision = $17`,
				string(next.Scope.TenantID), string(next.Scope.ProjectID), next.Revision, next.Enabled, next.Endpoint.String(), next.Credential.Ref,
				next.Credential.Version, next.Models, next.Limits.MaxRequests, next.Limits.TokenAllowance, next.Limits.MaxRequestBytes,
				next.Limits.MaxInputTokens, next.Limits.MaxOutputTokens, next.Recording, next.UpdatedAt, next.UpdatedBy, cur.Revision)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return gateway.Failf(gateway.ErrVersionConflict, "the settings changed; reload and retry")
			}
		} else if _, err := insertSettings(ctx, tx, "model_settings", next); err != nil {
			return err
		}
		if err := s.auditSettings(ctx, tx, cmd, next, changed); err != nil {
			return err
		}
		if err := s.emitSettings(ctx, tx, cmd, next, changed); err != nil {
			return err
		}
		res = gateway.SettingsResult{Settings: next, Changed: true}
		return nil
	})
	if err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			// Two first writers raced: the loser's expected revision is stale.
			return gateway.SettingsResult{}, gateway.Failf(gateway.ErrVersionConflict, "the settings changed; reload and retry")
		}
		return gateway.SettingsResult{}, err
	}
	return res, nil
}

func (s *Store) auditSettings(ctx context.Context, tx pgx.Tx, cmd gateway.UpdateSettings, next gateway.ModelSettings, changed []string) error {
	meta, err := json.Marshal(map[string]any{
		"changed": changed, "enabled": next.Enabled, "origin": next.Endpoint.Origin(), "credentialRef": next.Credential.Ref,
		"credentialVersion": next.Credential.Version, "models": next.Models, "recording": next.Recording,
	})
	if err != nil {
		return err
	}
	var request *string
	if cmd.RequestID != "" {
		request = &cmd.RequestID
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, request_id, metadata)
		VALUES ($1, $2, $3, $4, 'user', $5, 'model_settings.update', 'model_settings', $3, $6, $7, $8::jsonb)`,
		string(next.Scope.TenantID), newID("aud"), string(next.Scope.ProjectID), next.UpdatedAt, cmd.Actor.ID, next.Revision, request, string(meta))
	return err
}

func (s *Store) emitSettings(ctx context.Context, tx pgx.Tx, cmd gateway.UpdateSettings, next gateway.ModelSettings, changed []string) error {
	data, err := json.Marshal(eventsv1.ProjectSettingsChanged{ChangedSettings: changed, Version: next.Revision})
	if err != nil {
		return err
	}
	id := newID("evt")
	corr := cmd.RequestID
	if corr == "" {
		corr = id
	}
	return s.appendEvent(ctx, tx, eventsv1.Envelope{
		ID: id, SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeProjectSettingsChanged, OccurredAt: next.UpdatedAt,
		TenantID: next.Scope.TenantID, ProjectID: next.Scope.ProjectID,
		Resource: eventsv1.Resource{Type: "project", ID: string(next.Scope.ProjectID), Version: next.Revision},
		Actor:    eventsv1.Actor{Type: eventsv1.ActorUser, ID: cmd.Actor.ID}, CorrelationID: corr, Data: data,
	})
}

func (s *Store) appendEvent(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
	if err := s.events(ctx, tx, env); err != nil {
		if errors.Is(err, gateway.ErrBackpressure) {
			return err
		}
		return fmt.Errorf("gateway/postgres: append event: %w", err)
	}
	return nil
}
