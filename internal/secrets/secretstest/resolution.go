package secretstest

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/envelope"
)

// MemVersionSource is the standalone encrypted-value adapter behind
// secrets.VersionSource over memory: it reads the memory store, unwraps the scope
// key through the key wrapper and decrypts the exact pinned version. The key
// wrapper and installation identity must equal the ones the Service used to
// seal the values.
type MemVersionSource struct {
	store        *MemStore
	wrapper      secrets.KeyWrapper
	installation string
}

var _ secrets.VersionSource = (*MemVersionSource)(nil)

// NewMemVersionSource wires the adapter.
func NewMemVersionSource(store *MemStore, wrapper secrets.KeyWrapper, installation string) (*MemVersionSource, error) {
	if store == nil || wrapper == nil || installation == "" {
		return nil, errors.New("secretstest: store, key wrapper and installation ID are required")
	}
	return &MemVersionSource{store: store, wrapper: wrapper, installation: installation}, nil
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*MemVersionSource) DevelopmentOnly() bool { return true }

// Open reads the encrypted version and its scope key in one read-only unit,
// then, outside any unit, unwraps the key and decrypts. It never falls back to
// another version: a version that does not exist is ErrNotFound, one the run did
// not pin is ErrNotPinned, a released pin is ErrAttemptInactive and a revoked
// version is ErrRevoked.
func (v *MemVersionSource) Open(ctx context.Context, req secrets.VersionRequest) (secrets.PendingValue, error) {
	return v.open(ctx, req, false)
}

var _ secrets.ScanSource = (*MemVersionSource)(nil)

// OpenForScan is Open for comparison only (secrets.ScanSource): a revoked version
// and a released pin are still read, the run must have pinned the version, and the
// read is audited as secret.scan before any value is returned.
func (v *MemVersionSource) OpenForScan(ctx context.Context, req secrets.VersionRequest) (secrets.PendingValue, error) {
	return v.open(ctx, req, true)
}

func (v *MemVersionSource) open(ctx context.Context, req secrets.VersionRequest, scan bool) (secrets.PendingValue, error) {
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
	err := v.store.view(ctx, req.Scope.TenantID, func(tx memdb.Tx) error {
		var err error
		if sec, err = getSecretByName(tx, req.Scope, req.SecretName); err != nil {
			return err
		}
		if sv, err = getVersion(tx, req.Scope, sec.ID(), req.Version); err != nil {
			return err
		}
		pin, ok := pinsT.Get(tx, pinKey(secrets.ConsumerRef{Kind: secrets.ConsumerKindRun, ID: string(req.RunID)}, sec.ID(), req.Version))
		switch {
		case !ok || pin.Project != req.Scope.ProjectID:
			return secrets.ErrNotPinned
		case pin.ReleasedAt != nil && !scan:
			return fmt.Errorf("%w: the run released its pins", secrets.ErrAttemptInactive)
		}
		if sv.Revoked() && !scan {
			return secrets.ErrRevoked
		}
		sk, err = getScopeKey(tx, sv.Record().ScopeKeyID)
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
	if scan {
		s := v.store
		m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorSystem, ID: "disclosure-scan"}, Reason: "scan", Now: s.now()}
		err := s.cfg.Store.Update(ctx, req.Scope.TenantID, func(tx memdb.Tx) error {
			return s.audit(tx, m, auditEntry{Scope: req.Scope, Action: "secret.scan", ResourceType: "secret", ResourceID: sec.ID(),
				ResourceVersion: req.Version, Metadata: map[string]any{"name": req.SecretName, "version": req.Version, "runId": string(req.RunID)}})
		})
		if err != nil {
			clear(plain)
			return nil, fmt.Errorf("%w: the scan could not be audited: %v", secrets.ErrUnavailable, err)
		}
	}
	return &pending{src: v, req: req, secretID: sec.ID(), generation: sv.Record().KeyGeneration, value: plain}, nil
}

type pending struct {
	src        *MemVersionSource
	req        secrets.VersionRequest
	secretID   string
	generation int64
	value      []byte
}

func (p *pending) Value() []byte { return p.value }

func (p *pending) Discard() { clear(p.value) }

// Finalize rechecks, in one Update, that the version is still not revoked and
// that the consumer's pin is still unreleased, and commits the resolution audit
// entry. If the audit fails, or the commit's outcome is unknown, the caller
// releases nothing.
func (p *pending) Finalize(ctx context.Context, info secrets.FinalizeInfo) error {
	s, req := p.src.store, p.req
	if info.RunID != req.RunID {
		return fmt.Errorf("%w: finalization for another run", secrets.ErrInvalid)
	}
	now := s.now()
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorWorkload, ID: info.Subject()}, RequestID: info.RequestID, Now: now}
	if err := m.Validate(); err != nil {
		return err
	}
	callbackDone := false
	err := s.cfg.Store.Update(ctx, req.Scope.TenantID, func(tx memdb.Tx) error {
		sv, err := getVersion(tx, req.Scope, p.secretID, req.Version)
		if err != nil {
			return err
		}
		if sv.Revoked() {
			return secrets.ErrRevoked
		}
		consumer := secrets.ConsumerRef{Kind: secrets.ConsumerKindRun, ID: string(req.RunID)}
		pin, ok := pinsT.Get(tx, pinKey(consumer, p.secretID, req.Version))
		switch {
		case !ok || pin.Project != req.Scope.ProjectID:
			return secrets.ErrNotPinned
		case pin.ReleasedAt != nil:
			return fmt.Errorf("%w: the run released its pins", secrets.ErrAttemptInactive)
		}
		if pin.FirstResolvedAt == nil {
			pin.FirstResolvedAt = &now
		}
		pin.LastResolvedAt = &now
		pin.Resolutions++
		if err := pinsT.Put(tx, pin); err != nil {
			return err
		}
		if err := s.audit(tx, m, auditEntry{Scope: req.Scope, Action: "secret.resolve", ResourceType: "secret", ResourceID: p.secretID,
			ResourceVersion: req.Version, Metadata: auditMeta{
				"name": req.SecretName, "version": req.Version, "delivery": string(info.Delivery),
				"runId": string(info.RunID), "attemptId": string(info.AttemptID), "keyGeneration": p.generation,
			}.withResource(info.ResourceKey)}); err != nil {
			return err
		}
		callbackDone = true
		return nil
	})
	switch {
	case err == nil:
		return nil
	case callbackDone:
		return fmt.Errorf("%w: %v", secrets.ErrFinalizationUncertain, err)
	}
	return err
}

// RunConsumer is the consumer reference of a pipeline run.
func RunConsumer(id domain.RunID) secrets.ConsumerRef {
	return secrets.ConsumerRef{Kind: secrets.ConsumerKindRun, ID: string(id)}
}

// auditMeta is the metadata of a resolution audit entry.
type auditMeta map[string]any

// withResource names the run service a value was delivered to; an attempt's
// entry keeps exactly its earlier shape.
func (m auditMeta) withResource(key string) map[string]any {
	if key != "" {
		m["resourceKey"] = key
	}
	return m
}
