package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
)

// EnsureSubjectToken returns the tenant's server-assigned subject token inside
// tx, creating it on first use. The token is random and never derived from
// tenant-supplied text; it is immutable once assigned (no UPDATE grant).
func (s *Store) EnsureSubjectToken(ctx context.Context, tx pgx.Tx, tenant domain.TenantID) (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		var raw [12]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", fmt.Errorf("events: subject token: %w", err)
		}
		token := hex.EncodeToString(raw[:])
		// DO NOTHING without a target also absorbs a (practically impossible)
		// token collision with another tenant without aborting the transaction.
		if _, err := tx.Exec(ctx, `INSERT INTO event_tenant_subjects (tenant_id, subject_token) VALUES ($1, $2) ON CONFLICT DO NOTHING`, string(tenant), token); err != nil {
			return "", fmt.Errorf("events: assign subject token: %w", err)
		}
		var existing string
		err := tx.QueryRow(ctx, `SELECT subject_token FROM event_tenant_subjects WHERE tenant_id = $1`, string(tenant)).Scan(&existing)
		if err == nil {
			if verr := events.ValidateSubjectToken(existing); verr != nil {
				return "", verr
			}
			return existing, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("events: read subject token: %w", err)
		}
	}
	return "", errors.New("events: could not assign a subject token")
}

// SubjectToken returns the tenant's token, assigning one if needed.
func (s *Store) SubjectToken(ctx context.Context, tenant domain.TenantID) (string, error) {
	var token string
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) (err error) {
		token, err = s.EnsureSubjectToken(ctx, tx, tenant)
		return err
	})
	return token, err
}

// ResolveSubjectToken maps a subject token from a received message to its
// tenant through the narrow definer function. An unknown or malformed token is
// events.ErrUnknownSubject; the result is the trusted scope of the delivery.
func (s *Store) ResolveSubjectToken(ctx context.Context, token string) (domain.TenantID, error) {
	if err := events.ValidateSubjectToken(token); err != nil {
		return "", fmt.Errorf("%w: %v", events.ErrUnknownSubject, err)
	}
	rows, err := s.db.Query(ctx, `SELECT kodama_event_resolve_subject($1)`, token)
	if err != nil {
		return "", fmt.Errorf("events: resolve subject: %w", err)
	}
	defer rows.Close()
	var tenant *string
	for rows.Next() {
		if err := rows.Scan(&tenant); err != nil {
			return "", fmt.Errorf("events: resolve subject: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("events: resolve subject: %w", err)
	}
	if tenant == nil {
		return "", events.ErrUnknownSubject
	}
	return domain.TenantID(*tenant), nil
}
