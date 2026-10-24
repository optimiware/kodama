package chatops

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Config holds the consumer-owned collaborators of a Service. Store and Policy
// are required. Credentials is required to activate a destination that names a
// credential reference: without it such a destination is refused (fail closed),
// and credential-free destinations still work. Fences and Operators are
// required only by the restore operations.
type Config struct {
	Store       Store
	Policy      EndpointPolicy
	Credentials CredentialSource
	Fences      FenceStore
	Operators   OperatorAuthorizer
	// MaxDestinationsPerProject bounds configured destinations; it is
	// installation configuration, not an environment variable.
	MaxDestinationsPerProject int
	Clock                     func() time.Time
	Logger                    *slog.Logger
}

// Service is the management use-case layer: destination configuration, sanitized
// delivery inspection, manual replay and the restore fence. Delivery itself is
// the Worker's; this layer holds no transport or SQL.
type Service struct {
	cfg Config
	now func() time.Time
	log *slog.Logger
}

// NewService validates the configuration.
func NewService(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("chatops: a store is required")
	case cfg.Policy == nil:
		return nil, errors.New("chatops: an endpoint policy is required (no allow-all fallback)")
	case cfg.MaxDestinationsPerProject < 1 || cfg.MaxDestinationsPerProject > 1000:
		return nil, errors.New("chatops: the destination limit must be 1 to 1000")
	}
	s := &Service{cfg: cfg, now: cfg.Clock, log: cfg.Logger}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// MaxPage bounds a listing's page (the HTTP adapter asks for at most 201 rows).
const MaxPage = 1000

func (s *Service) check(c Caller, scope domain.Scope) error {
	if err := c.Actor.Validate(); err != nil {
		return Fail(ErrDenied, nil, "the caller cannot be attributed")
	}
	if err := scope.Validate(); err != nil {
		return invalidf("the scope is not valid")
	}
	return nil
}

// approve applies the installation policy and, for an enabled destination that
// names a credential, proves the credential resolves for exactly this recipient.
// The value is discarded; send time resolves again and is authoritative.
func (s *Service) approve(ctx context.Context, d Destination) error {
	if err := s.cfg.Policy.Approve(d.Scope, d.Endpoint); err != nil {
		if errors.Is(err, ErrPolicy) {
			return Fail(ErrPolicy, err, "the endpoint is not approved by the installation policy")
		}
		return Fail(ErrUnavailable, err, "the endpoint policy could not decide")
	}
	if d.Credential == "" {
		return nil
	}
	if s.cfg.Credentials == nil {
		return Fail(ErrNotConfigured, nil, "this installation has no credential source for authenticated destinations")
	}
	if !d.Enabled {
		return nil
	}
	r, err := d.Recipient()
	if err != nil {
		return err
	}
	switch _, err := s.cfg.Credentials.Resolve(ctx, d.Scope, d.Credential, r); {
	case err == nil:
		return nil
	case errors.Is(err, ErrCredentialUnavailable):
		return Fail(ErrCredentialUnavailable, err, "the credential is not available for this destination")
	default:
		return Fail(ErrUnavailable, err, "the credential source could not answer")
	}
}

// CreateDestination configures a destination (project:settings). Authorization
// is checked before any policy or credential lookup so a caller learns nothing
// from those, and again inside the store's transaction.
func (s *Service) CreateDestination(ctx context.Context, c Caller, scope domain.Scope, in DestinationInput) (Destination, error) {
	if err := s.check(c, scope); err != nil {
		return Destination{}, err
	}
	id, err := NewDestinationID()
	if err != nil {
		return Destination{}, Classify(err)
	}
	d, err := NewDestination(scope, id, in, s.now())
	if err != nil {
		return Destination{}, err
	}
	if err := s.cfg.Store.CheckAccess(ctx, c, scope, PermSettings); err != nil {
		return Destination{}, Classify(err)
	}
	if err := s.approve(ctx, d); err != nil {
		return Destination{}, err
	}
	out, err := s.cfg.Store.CreateDestination(ctx, c, d, s.cfg.MaxDestinationsPerProject)
	return out, Classify(err)
}

// GetDestination reads a destination's configuration (project:settings).
func (s *Service) GetDestination(ctx context.Context, c Caller, scope domain.Scope, id DestinationID) (Destination, error) {
	if err := s.check(c, scope); err != nil {
		return Destination{}, err
	}
	if err := id.Validate(); err != nil {
		return Destination{}, Fail(ErrNotFound, err, "not found")
	}
	out, err := s.cfg.Store.GetDestination(ctx, c, scope, id)
	return out, Classify(err)
}

// ListDestinations lists configuration in identifier order (project:settings).
func (s *Service) ListDestinations(ctx context.Context, c Caller, scope domain.Scope, after DestinationID, limit int) ([]Destination, error) {
	if err := s.check(c, scope); err != nil {
		return nil, err
	}
	if limit < 1 || limit > MaxPage {
		return nil, invalidf("a page is 1 to %d rows", MaxPage)
	}
	out, err := s.cfg.Store.ListDestinations(ctx, c, scope, after, limit)
	return out, Classify(err)
}

// UpdateDestination edits a destination under If-Match semantics
// (project:settings). Disabling is an ordinary versioned edit. A change of the
// endpoint or the credential reference advances the binding revision, which
// fences intents created earlier from the new binding.
func (s *Service) UpdateDestination(ctx context.Context, c Caller, scope domain.Scope, id DestinationID, expected int64, edit DestinationEdit) (Destination, error) {
	if err := s.check(c, scope); err != nil {
		return Destination{}, err
	}
	if err := id.Validate(); err != nil {
		return Destination{}, Fail(ErrNotFound, err, "not found")
	}
	if edit.Empty() {
		return Destination{}, invalidf("an edit changes at least one member")
	}
	current, err := s.cfg.Store.GetDestination(ctx, c, scope, id)
	if err != nil {
		return Destination{}, Classify(err)
	}
	next := current
	changed, err := next.Edit(expected, edit, s.now())
	if err != nil {
		return Destination{}, err
	}
	if changed {
		endpointChanged := next.Endpoint != current.Endpoint
		activating := next.Enabled && (!current.Enabled || next.Credential != current.Credential || endpointChanged)
		if endpointChanged || activating {
			if err := s.approve(ctx, next); err != nil {
				return Destination{}, err
			}
		}
	}
	out, err := s.cfg.Store.UpdateDestination(ctx, c, scope, id, expected, edit, s.now())
	return out, Classify(err)
}

// ListDeliveries lists sanitized delivery status, newest first (project:read).
func (s *Service) ListDeliveries(ctx context.Context, c Caller, scope domain.Scope, f DeliveryFilter, after DeliveryPosition, limit int) ([]Delivery, error) {
	if err := s.check(c, scope); err != nil {
		return nil, err
	}
	switch f.State {
	case "", StatePending, StateClaimed, StateDelivered, StateFailed:
	default:
		return nil, invalidf("the state filter is not known")
	}
	if f.Destination != "" {
		if err := f.Destination.Validate(); err != nil {
			return nil, err
		}
	}
	if limit < 1 || limit > MaxPage {
		return nil, invalidf("a page is 1 to %d rows", MaxPage)
	}
	out, err := s.cfg.Store.ListDeliveries(ctx, c, scope, f, after, limit)
	return out, Classify(err)
}

// GetDelivery returns one delivery with its attempt history (project:read).
func (s *Service) GetDelivery(ctx context.Context, c Caller, scope domain.Scope, id DeliveryID) (DeliveryView, error) {
	if err := s.check(c, scope); err != nil {
		return DeliveryView{}, err
	}
	if err := id.Validate(); err != nil {
		return DeliveryView{}, Fail(ErrNotFound, err, "not found")
	}
	out, err := s.cfg.Store.GetDelivery(ctx, c, scope, id)
	return out, Classify(err)
}

// ReplayDelivery reopens a failed delivery for a new cycle (project:settings).
// It needs the delivery's current version and a reason; delivered records are
// never replayed. The payload and the receiver idempotency key are unchanged, so
// a receiver that deduplicates applies one effect.
func (s *Service) ReplayDelivery(ctx context.Context, c Caller, scope domain.Scope, id DeliveryID, expected int64, reason string) (Delivery, error) {
	if err := s.check(c, scope); err != nil {
		return Delivery{}, err
	}
	if err := id.Validate(); err != nil {
		return Delivery{}, Fail(ErrNotFound, err, "not found")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > MaxReasonBytes {
		return Delivery{}, invalidf("a replay carries a reason of 1 to %d bytes", MaxReasonBytes)
	}
	out, err := s.cfg.Store.ReplayDelivery(ctx, c, scope, id, expected, reason, s.now())
	return out, Classify(err)
}

// FenceDeliveries stops every delivery attempt of a tenant until ClearFence. A
// restore operator calls it before workers restart: T-005's consumer fence
// stops event replay but does not stop deliveries that were already created,
// which a restored worker would otherwise send again to receivers that may have
// accepted them before the backup point.
func (s *Service) FenceDeliveries(ctx context.Context, op Operator, tenant domain.TenantID, reason string) error {
	return s.setFence(ctx, op, tenant, true, reason)
}

// ClearDeliveryFence resumes deliveries once the operator has reconciled
// external effects and fenced older workers.
func (s *Service) ClearDeliveryFence(ctx context.Context, op Operator, tenant domain.TenantID, reason string) error {
	return s.setFence(ctx, op, tenant, false, reason)
}

func (s *Service) setFence(ctx context.Context, op Operator, tenant domain.TenantID, fenced bool, reason string) error {
	if s.cfg.Fences == nil || s.cfg.Operators == nil {
		return Fail(ErrNotConfigured, nil, "restore operations are not configured")
	}
	reason = strings.TrimSpace(reason)
	if op.ID == "" || len(op.ID) > 256 || reason == "" || len(reason) > MaxReasonBytes {
		return invalidf("a fence names an operator and a reason")
	}
	if err := tenant.Validate(); err != nil {
		return invalidf("the tenant is not valid")
	}
	action := "chatops.fence"
	if !fenced {
		action = "chatops.fence_clear"
	}
	if err := s.cfg.Operators.AuthorizeOperator(ctx, op, tenant, action); err != nil {
		return Fail(ErrDenied, err, "the operator is not authorized")
	}
	return Classify(s.cfg.Fences.SetDeliveryFence(ctx, tenant, fenced, op, reason, s.now()))
}
