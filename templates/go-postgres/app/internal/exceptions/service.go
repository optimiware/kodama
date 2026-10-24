package exceptions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"example.invalid/inventory/internal/tenancy"
)

// Config wires Service. Store is required; ERP is required only for Process.
type Config struct {
	Store Store
	ERP   ERP
	Clock func() time.Time
	// Lease bounds how long a worker owns an intent before it may be reclaimed.
	Lease time.Duration
	// MaxAttempts bounds automatic ERP attempts; afterwards the outcome stays
	// visibly unknown for an operator to reconcile.
	MaxAttempts int
	// RetryBase and RetryMax bound the reconciliation backoff.
	RetryBase, RetryMax time.Duration
}

// Service coordinates exception use cases over its ports.
type Service struct {
	cfg Config
}

// NewService fails closed on a missing required collaborator.
func NewService(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("exceptions: a store is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Lease <= 0 {
		cfg.Lease = 30 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 8
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 5 * time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 10 * time.Minute
	}
	return &Service{cfg: cfg}, nil
}

// NewID returns a random lowercase UUID (version 4).
func NewID() ID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return ID(h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:])
}

// CreateInput describes a new exception.
type CreateInput struct {
	ERPRef          string
	Description     string
	ERPQuantity     int64
	CountedQuantity int64
}

// Create opens an exception.
func (s *Service) Create(ctx context.Context, a tenancy.Access, in CreateInput) (Exception, error) {
	if err := a.Require(tenancy.ExceptionCreate); err != nil {
		return Exception{}, err
	}
	e, err := NewException(a.Tenant(), NewID(), in.ERPRef, in.Description, in.ERPQuantity, in.CountedQuantity, a.Subject(), s.cfg.Clock())
	if err != nil {
		return Exception{}, err
	}
	if err := s.cfg.Store.Create(ctx, a, e); err != nil {
		return Exception{}, err
	}
	return e, nil
}

// Get reads one exception in the access's tenant.
func (s *Service) Get(ctx context.Context, a tenancy.Access, id ID) (Exception, error) {
	if err := a.Require(tenancy.ExceptionRead); err != nil {
		return Exception{}, err
	}
	return s.cfg.Store.Get(ctx, a, id)
}

// List reads a bounded page.
func (s *Service) List(ctx context.Context, a tenancy.Access, q ListQuery) (Page, error) {
	if err := a.Require(tenancy.ExceptionRead); err != nil {
		return Page{}, err
	}
	if q.State != "" && !q.State.Valid() {
		return Page{}, fmt.Errorf("%w: unknown state filter", ErrInvalid)
	}
	if q.Limit <= 0 || q.Limit > 100 {
		q.Limit = 50
	}
	return s.cfg.Store.List(ctx, a, q)
}

// History reads the append-only audit trail of one exception.
func (s *Service) History(ctx context.Context, a tenancy.Access, id ID) ([]AuditEntry, error) {
	if err := a.Require(tenancy.ExceptionRead); err != nil {
		return nil, err
	}
	return s.cfg.Store.History(ctx, a, id)
}

// Resolve accepts a reviewed adjustment: the exception becomes pending with a
// durable ERP intent. It never reports resolved; only an ERP acknowledgment
// does. The actor comes from the verified access, never from the payload.
func (s *Service) Resolve(ctx context.Context, a tenancy.Access, id ID, adjusted int64, reason, key string, expectedVersion int64) (Receipt, error) {
	if err := a.Require(tenancy.ExceptionResolve); err != nil {
		return Receipt{}, err
	}
	req := ResolveRequest{Actor: a.Subject(), AdjustedQuantity: adjusted, Reason: reason, IdempotencyKey: key, ExpectedVersion: expectedVersion}
	if err := req.Validate(); err != nil {
		return Receipt{}, err
	}
	now := s.cfg.Clock()
	return s.cfg.Store.Submit(ctx, a, id, req, func(e *Exception) (Intent, error) { return e.RequestResolution(req, now) })
}

// Process claims and executes at most one intent for the worker's tenant.
// It returns whether work was found. The ERP call happens outside any SQL
// transaction; a crash or lost response leaves the intent reclaimable and
// reconciled by the same operation key.
func (s *Service) Process(ctx context.Context, a tenancy.Access, owner string) (bool, error) {
	if err := a.Require(tenancy.ExceptionProcess); err != nil {
		return false, err
	}
	if s.cfg.ERP == nil {
		return false, errors.New("exceptions: an ERP adapter is required to process intents")
	}
	claim, err := s.cfg.Store.Claim(ctx, a, owner, s.cfg.Lease, s.cfg.Clock(), s.cfg.MaxAttempts)
	if err != nil || claim == nil {
		return false, err
	}
	in := claim.Intent
	op := Operation{Key: in.OperationKey, ERPRef: in.ERPRef, AdjustedQuantity: in.AdjustedQuantity, Reason: in.Reason, PayloadHash: in.PayloadHash}

	var out Outcome
	if claim.Reconcile {
		out, err = s.cfg.ERP.Lookup(ctx, a.Tenant(), op.Key)
		if err == nil && out.Status == OutcomeNotFound {
			out, err = s.cfg.ERP.Apply(ctx, a.Tenant(), op) // same key: the ERP deduplicates
		}
	} else {
		out, err = s.cfg.ERP.Apply(ctx, a.Tenant(), op)
	}
	if ctx.Err() != nil {
		return true, ctx.Err() // keep the lease; a later claim reconciles by the same key
	}
	obs := Observation{IntentID: in.ID, OperationKey: in.OperationKey, Owner: claim.Owner, At: s.cfg.Clock()}
	switch {
	case err == nil && out.Status == OutcomeAcknowledged:
		obs.Kind, obs.Reference = ObservedAcknowledged, out.Reference
	case err == nil && out.Status == OutcomeRejected:
		obs.Kind, obs.Detail = ObservedRejected, truncate(out.Reason, 300)
		if obs.Detail == "" {
			obs.Detail = "rejected by the ERP"
		}
	default:
		obs.Kind, obs.Detail = ObservedUnknown, "ERP outcome could not be established"
		if err != nil {
			obs.Detail = truncate("ERP outcome unknown: "+err.Error(), 300)
		}
		if in.Attempts < s.cfg.MaxAttempts {
			obs.RetryAt = obs.At.Add(s.backoff(in.Attempts))
		}
	}
	if _, oerr := s.cfg.Store.Observe(ctx, a, obs); oerr != nil {
		return true, oerr
	}
	return true, nil
}

func (s *Service) backoff(attempts int) time.Duration {
	d := s.cfg.RetryBase
	for i := 1; i < attempts && d < s.cfg.RetryMax; i++ {
		d *= 2
	}
	if d > s.cfg.RetryMax {
		d = s.cfg.RetryMax
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
