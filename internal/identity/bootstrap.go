package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// BootstrapConfig is the explicit operator configuration of the one-time
// standalone bootstrap. There is no first-request-wins enrollment: only the
// configured identity, verified through the normal credential verifier, may
// complete it.
type BootstrapConfig struct {
	// Profile must be ProfileStandalone; bootstrap provisions local authority.
	Profile Profile
	// Eligible is the (issuer, subject) of the initial administrator.
	Eligible Principal
	// OperatorID is the audit/event actor identifier of the operator running the
	// bootstrap (an opaque ID, not a credential).
	OperatorID string
	// TenantID, TenantKey and DisplayName describe the initial tenant.
	TenantID    domain.TenantID
	TenantKey   string
	DisplayName string
}

// Validate checks the configuration.
func (c BootstrapConfig) Validate() error {
	if c.Profile != ProfileStandalone {
		return errors.New("identity: bootstrap requires the explicitly selected standalone profile")
	}
	if err := c.Eligible.Validate(); err != nil {
		return err
	}
	if err := (Actor{ID: c.OperatorID, Type: ActorOperator}).Validate(); err != nil {
		return err
	}
	if err := c.TenantID.Validate(); err != nil {
		return invalid("%v", err)
	}
	if !keyPattern.MatchString(c.TenantKey) {
		return invalid("tenant key must match %s", keyPattern)
	}
	return validateDisplayName(c.DisplayName)
}

// BootstrapPlan is everything the store needs to provision the initial tenant
// in one administrative transaction. IDs are generated before the transaction;
// a retry that finds a completed bootstrap returns the committed IDs instead.
type BootstrapPlan struct {
	Tenant      domain.TenantID
	TenantKey   string
	DisplayName string
	Principal   Principal
	OperatorID  string
	UserID      UserID
	BindingID   BindingID
	ChangeID    string
	// Digest fingerprints the parameters; an identical retry observes the
	// committed result and changed parameters conflict.
	Digest     string
	OccurredAt time.Time
}

// BootstrapResult is the committed outcome.
type BootstrapResult struct {
	Tenant    domain.TenantID
	UserID    UserID
	BindingID BindingID
	// Created is true for the invocation that committed; false for an identical
	// retry that observed the earlier commit.
	Created bool
}

// BootstrapStore provisions the plan atomically using the administrative
// identity: tenant, initial user mapping, tenant-wide admin binding, audit,
// membership.changed event and the durable completion record. It must report an
// unknown commit outcome as ErrUnavailable and must never grant twice.
type BootstrapStore interface {
	Complete(ctx context.Context, plan BootstrapPlan) (BootstrapResult, error)
}

// BootstrapService runs the operator-only bootstrap. It is not wired into
// request handling: the composition root exposes it only through an explicitly
// invoked operator command and gives it a separate administrative database.
type BootstrapService struct {
	cfg      BootstrapConfig
	verifier CredentialVerifier
	store    BootstrapStore
	now      func() time.Time
	newID    func(string) (string, error)
}

// NewBootstrapService validates the configuration and collaborators.
func NewBootstrapService(cfg BootstrapConfig, v CredentialVerifier, store BootstrapStore) (*BootstrapService, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if v == nil || store == nil {
		return nil, errors.New("identity: bootstrap requires a credential verifier and a store")
	}
	return &BootstrapService{cfg: cfg, verifier: v, store: store,
		now: func() time.Time { return time.Now().UTC() }, newID: RandomID}, nil
}

// Run verifies the administrator's access token with the normal verifier,
// before any administrative transaction opens, and provisions the tenant.
func (s *BootstrapService) Run(ctx context.Context, accessToken string) (BootstrapResult, error) {
	p, err := s.verifier.Verify(ctx, accessToken)
	if err != nil {
		if errors.Is(err, ErrUnauthenticated) {
			return BootstrapResult{}, err
		}
		return BootstrapResult{}, Unavailablef(err, "verifying the bootstrap credential")
	}
	if p != s.cfg.Eligible {
		return BootstrapResult{}, ErrBootstrapNotEligible
	}
	uid, err := s.newID("u_")
	if err != nil {
		return BootstrapResult{}, Unavailablef(err, "identifier generation failed")
	}
	bid, err := s.newID("mb_")
	if err != nil {
		return BootstrapResult{}, Unavailablef(err, "identifier generation failed")
	}
	chg, err := s.newID("chg_")
	if err != nil {
		return BootstrapResult{}, Unavailablef(err, "identifier generation failed")
	}
	plan := BootstrapPlan{
		Tenant: s.cfg.TenantID, TenantKey: s.cfg.TenantKey, DisplayName: s.cfg.DisplayName, Principal: p,
		OperatorID: s.cfg.OperatorID, UserID: UserID(uid), BindingID: BindingID(bid), ChangeID: chg,
		Digest: BootstrapDigest(s.cfg.TenantID, s.cfg.TenantKey, s.cfg.DisplayName, p), OccurredAt: s.now(),
	}
	res, err := s.store.Complete(ctx, plan)
	if err != nil && !IsBusinessError(err) {
		return BootstrapResult{}, Unavailablef(err, "the bootstrap outcome is unknown; observe the completion record before retrying")
	}
	return res, err
}

// BootstrapDigest fingerprints the parameters that define a bootstrap.
func BootstrapDigest(tenant domain.TenantID, key, displayName string, p Principal) string {
	h := sha256.New()
	for _, f := range []string{string(tenant), key, displayName, p.Issuer, p.Subject} {
		fmt.Fprintf(h, "%d:%s;", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}
