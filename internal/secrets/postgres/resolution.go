package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/envelope"
	"github.com/optimiware/kodama/internal/storage"
)

// VersionSource is the standalone encrypted-value adapter behind
// secrets.VersionSource: it reads this module's encrypted store, unwraps the
// scope key and decrypts the exact pinned version. A hosted adapter (O-19/T-061)
// replaces it without changing the Resolver.
type VersionSource struct {
	store        *Store
	wrapper      secrets.KeyWrapper
	installation string
}

var _ secrets.VersionSource = (*VersionSource)(nil)

// NewVersionSource wires the adapter. The key wrapper and installation identity
// must equal the ones the Service used to seal the values.
func NewVersionSource(store *Store, wrapper secrets.KeyWrapper, installation string) (*VersionSource, error) {
	if store == nil || wrapper == nil || installation == "" {
		return nil, errors.New("secrets/postgres: store, key wrapper and installation ID are required")
	}
	return &VersionSource{store: store, wrapper: wrapper, installation: installation}, nil
}

// Open reads the encrypted version and its scope key in one read-only
// transaction, then, outside any transaction, unwraps the key and decrypts. It
// never falls back to another version: a version that does not exist is
// ErrNotFound, a version the run did not pin is ErrNotPinned, and a revoked one
// is ErrRevoked.
func (v *VersionSource) Open(ctx context.Context, req secrets.VersionRequest) (secrets.PendingValue, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if err := req.RunID.Validate(); err != nil {
		return nil, err
	}
	var (
		sec secrets.Secret
		sv  secrets.SecretVersion
		sk  secrets.ScopeKey
	)
	err := v.store.read(ctx, req.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if sec, err = getSecretByName(ctx, tx, req.Scope, req.SecretName, ""); err != nil {
			return err
		}
		if sv, err = getVersion(ctx, tx, req.Scope, sec.ID(), req.Version, ""); err != nil {
			return err
		}
		var released bool
		err = tx.QueryRow(ctx, `SELECT released_at IS NOT NULL FROM secret_pins
WHERE tenant_id = $1 AND project_id = $2 AND consumer_kind = $3 AND consumer_id = $4 AND secret_id = $5 AND secret_version = $6`,
			string(req.Scope.TenantID), string(req.Scope.ProjectID), secrets.ConsumerKindRun, string(req.RunID), sec.ID(), req.Version).Scan(&released)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return secrets.ErrNotPinned
		case err != nil:
			return err
		case released:
			return fmt.Errorf("%w: the run released its pins", secrets.ErrAttemptInactive)
		}
		if sv.Revoked() {
			return secrets.ErrRevoked
		}
		sk, err = getScopeKey(ctx, tx, req.Scope.TenantID, sv.Record().ScopeKeyID, false)
		return err
	})
	if err != nil {
		return nil, err
	}

	material, err := secrets.UnwrapScopeKey(ctx, v.wrapper, v.installation, sk)
	if err != nil {
		return nil, err
	}
	defer clear(material)
	plain, err := envelope.Open(material, sv.EnvelopeContext(v.installation, sec.Name()), sv.Envelope())
	if err != nil {
		return nil, fmt.Errorf("%w: stored value failed authentication", secrets.ErrInconsistent)
	}
	return &pending{src: v, req: req, secretID: sec.ID(), generation: sv.Record().KeyGeneration, value: plain}, nil
}

type pending struct {
	src        *VersionSource
	req        secrets.VersionRequest
	secretID   string
	generation int64
	value      []byte
}

func (p *pending) Value() []byte { return p.value }

func (p *pending) Discard() { clear(p.value) }

// Finalize rechecks, in one short transaction, that the version is still not
// revoked and that the consumer's pin is still unreleased, and commits the
// resolution audit entry. The version row is locked FOR SHARE and the pin row
// FOR UPDATE (in that order): revocation takes the version row FOR UPDATE and
// pin release updates the pin row, so each is ordered against this commit. If
// the audit insert fails, or the commit's outcome is unknown, the caller
// releases nothing.
func (p *pending) Finalize(ctx context.Context, info secrets.FinalizeInfo) error {
	s, req := p.src.store, p.req
	if info.RunID != req.RunID {
		return fmt.Errorf("%w: finalization for another run", secrets.ErrInvalid)
	}
	now := s.now()
	m := secrets.Mutation{
		Actor:     secrets.Actor{Type: secrets.ActorWorkload, ID: string(info.AttemptID)},
		RequestID: info.RequestID, Now: now,
	}
	if err := m.Validate(); err != nil {
		return err
	}
	callbackDone := false
	err := storage.WithTenant(ctx, s.db, req.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		sv, err := getVersion(ctx, tx, req.Scope, p.secretID, req.Version, " FOR SHARE")
		if err != nil {
			return err
		}
		if sv.Revoked() {
			return secrets.ErrRevoked
		}
		var released bool
		err = tx.QueryRow(ctx, `SELECT released_at IS NOT NULL FROM secret_pins
WHERE tenant_id = $1 AND project_id = $2 AND consumer_kind = $3 AND consumer_id = $4 AND secret_id = $5 AND secret_version = $6
FOR UPDATE`, string(req.Scope.TenantID), string(req.Scope.ProjectID), secrets.ConsumerKindRun, string(req.RunID), p.secretID, req.Version).Scan(&released)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return secrets.ErrNotPinned
		case err != nil:
			return err
		case released:
			return fmt.Errorf("%w: the run released its pins", secrets.ErrAttemptInactive)
		}
		if _, err := tx.Exec(ctx, `UPDATE secret_pins SET first_resolved_at = COALESCE(first_resolved_at, $7), last_resolved_at = $7,
resolution_count = resolution_count + 1
WHERE tenant_id = $1 AND project_id = $2 AND consumer_kind = $3 AND consumer_id = $4 AND secret_id = $5 AND secret_version = $6`,
			string(req.Scope.TenantID), string(req.Scope.ProjectID), secrets.ConsumerKindRun, string(req.RunID), p.secretID, req.Version, now); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, m, auditEntry{Scope: req.Scope, Action: "secret.resolve", ResourceType: "secret", ResourceID: p.secretID,
			ResourceVersion: req.Version, Metadata: map[string]any{
				"name": req.SecretName, "version": req.Version, "delivery": string(info.Delivery),
				"runId": string(info.RunID), "attemptId": string(info.AttemptID), "keyGeneration": p.generation,
			}}); err != nil {
			return err
		}
		callbackDone = true
		return nil
	})
	switch {
	case err == nil:
		return nil
	case callbackDone:
		// Everything succeeded and only the commit failed: the audit may or may
		// not be durable, so no plaintext is released; a retry is a new,
		// separately audited resolution.
		return fmt.Errorf("%w: %v", secrets.ErrFinalizationUncertain, err)
	}
	return classify(err)
}

// RunConsumer is the consumer reference of a pipeline run.
func RunConsumer(id domain.RunID) secrets.ConsumerRef {
	return secrets.ConsumerRef{Kind: secrets.ConsumerKindRun, ID: string(id)}
}
