// Credential lifecycle orchestration.
//
// Every mutation re-reads the current principal/service state and the current
// tenant derivation key before it asks MutationPort to commit one complete
// per-principal unit. The nonce/expiry pair, receipt, audit/event intent and
// current-authority reads commit atomically; token plaintext exists only on the
// post-commit Delivery value and is never persisted, serialized into the unit or
// rendered by an error.
//
// A failed commit produces no token and no delivery. A commit that succeeds but
// whose delivery/invalidation effect is lost returns the same retryable command
// without mutating again or restarting work. The durable receipt is consulted
// before the credential-presence and expected-version preflights, so a
// lost-response retry replays the original result instead of conflicting on the
// state its own commit advanced. A replayed commit (an equal receipt already
// committed by a lost-response attempt) returns the committed state/version
// with no token and no delivery: the nonce derived by the replay was never
// committed, so it must never be handed out.
package token

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// DerivationKeyResolver unwraps and decrypts one tenant derivation key. The
// returned plaintext exists only in memory and is never logged or persisted.
type DerivationKeyResolver interface {
	ResolveDerivationKey(context.Context, TenantKey) ([]byte, error)
}

// Clock is the injected authoritative time source for credential mutations.
type Clock interface {
	Now() time.Time
}

// Delivery is one post-commit credential delivery command. Token is plaintext
// and deliberately has no serialized form; String redacts it for logs.
type Delivery struct {
	Scope       tenancy.Scope     `json:"scope"`
	PrincipalID id.PrincipalID    `json:"principal_id"`
	ExecutionID id.ExecutionID    `json:"execution_id,omitempty"`
	ServiceID   service.ServiceID `json:"service_id,omitempty"`
	Kind        MutationKind      `json:"kind"`
	Token       string            `json:"-"`
	Target      string            `json:"target"`
	Refresh     bool              `json:"refresh"`
}

// Validate checks the delivery identity, exactly one target, the declared
// mutation kind and the presence of a token and target. It never renders the
// token.
func (delivery Delivery) Validate() error {
	if err := delivery.Scope.Validate(); err != nil {
		return err
	}
	if _, err := id.ParsePrincipalID(string(delivery.PrincipalID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "delivery principal id is invalid", err)
	}
	switch {
	case delivery.ExecutionID != "" && delivery.ServiceID != "":
		return fault.New(fault.InvalidInput, "delivery names both an execution and a service")
	case delivery.ExecutionID != "":
		if _, err := id.ParseExecutionID(string(delivery.ExecutionID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "delivery execution id is invalid", err)
		}
	case delivery.ServiceID != "":
		if _, err := service.ParseServiceID(string(delivery.ServiceID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "delivery service id is invalid", err)
		}
	default:
		return fault.New(fault.InvalidInput, "delivery names neither an execution nor a service")
	}
	if !delivery.Kind.Valid() {
		return fault.New(fault.InvalidInput, "delivery mutation kind is not frozen")
	}
	if delivery.Token == "" {
		return fault.New(fault.InvalidInput, "delivery token is empty")
	}
	if delivery.Target == "" {
		return fault.New(fault.InvalidInput, "delivery target is empty")
	}
	return nil
}

// String renders a redacted delivery for logs. The plaintext token is never
// part of the output.
func (delivery Delivery) String() string {
	return fmt.Sprintf(
		"credential delivery scope=%+v principal=%s execution=%s service=%s kind=%s target=%s refresh=%t token=[REDACTED]",
		delivery.Scope, delivery.PrincipalID, delivery.ExecutionID, delivery.ServiceID,
		delivery.Kind, delivery.Target, delivery.Refresh)
}

// Deliverer delivers one committed credential to its consumer. It is a
// post-commit retrying effect, never part of the mutation transaction.
type Deliverer interface {
	Deliver(context.Context, Delivery) error
}

// ManagerConfig binds every credential lifecycle dependency.
type ManagerConfig struct {
	Keys          TenantKeyReader
	Resolver      DerivationKeyResolver
	Credentials   CredentialReader
	Principals    identity.PrincipalReader
	Services      service.ServiceReader
	Mutations     MutationPort
	Receipts      ReceiptLookup
	Deliverer     Deliverer
	Invalidations InvalidationPort
	Clock         Clock
	Random        io.Reader
	Bounds        Bounds
}

// Manager orchestrates credential issue/rotate/revoke and derived-credential
// authentication. It holds no cached authority and no mutable state, so
// concurrent calls re-read current state independently.
type Manager struct {
	keys          TenantKeyReader
	resolver      DerivationKeyResolver
	credentials   CredentialReader
	principals    identity.PrincipalReader
	services      service.ServiceReader
	mutations     MutationPort
	receipts      ReceiptLookup
	deliverer     Deliverer
	invalidations InvalidationPort
	clock         Clock
	random        io.Reader
	bounds        Bounds
}

// NewManager builds a manager with every dependency present and a usable bound
// set.
func NewManager(config ManagerConfig) (*Manager, error) {
	switch {
	case config.Keys == nil:
		return nil, fault.New(fault.InvalidInput, "credential manager requires a tenant key reader")
	case config.Resolver == nil:
		return nil, fault.New(fault.InvalidInput, "credential manager requires a derivation key resolver")
	case config.Credentials == nil:
		return nil, fault.New(fault.InvalidInput, "credential manager requires a credential reader")
	case config.Principals == nil:
		return nil, fault.New(fault.InvalidInput, "credential manager requires a principal reader")
	case config.Services == nil:
		return nil, fault.New(fault.InvalidInput, "credential manager requires a service reader")
	case config.Mutations == nil:
		return nil, fault.New(fault.InvalidInput, "credential manager requires a mutation port")
	case config.Receipts == nil:
		return nil, fault.New(fault.InvalidInput, "credential manager requires a receipt lookup")
	case config.Deliverer == nil:
		return nil, fault.New(fault.InvalidInput, "credential manager requires a deliverer")
	case config.Invalidations == nil:
		return nil, fault.New(fault.InvalidInput, "credential manager requires an invalidation port")
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "credential manager requires a clock")
	case config.Random == nil:
		return nil, fault.New(fault.InvalidInput, "credential manager requires a randomness source")
	}
	bounds := config.Bounds
	if bounds == (Bounds{}) {
		bounds = DefaultBounds
	}
	if bounds.MinNonceBytes < MinNonceBytes {
		return nil, fault.New(fault.InvalidInput, "credential bounds require at least 256-bit nonces")
	}
	if bounds.MinTTL <= 0 {
		return nil, fault.New(fault.InvalidInput, "credential bounds require a positive minimum ttl")
	}
	if bounds.MaxTTL < bounds.MinTTL {
		return nil, fault.New(fault.InvalidInput, "credential bounds maximum ttl is below the minimum")
	}
	return &Manager{
		keys:          config.Keys,
		resolver:      config.Resolver,
		credentials:   config.Credentials,
		principals:    config.Principals,
		services:      config.Services,
		mutations:     config.Mutations,
		receipts:      config.Receipts,
		deliverer:     config.Deliverer,
		invalidations: config.Invalidations,
		clock:         config.Clock,
		random:        config.Random,
		bounds:        bounds,
	}, nil
}

// IssueRequest is one credential issue or rotation input.
type IssueRequest struct {
	Scope           tenancy.Scope             `json:"scope"`
	PrincipalID     id.PrincipalID            `json:"principal_id"`
	ExecutionID     id.ExecutionID            `json:"execution_id,omitempty"`
	ServiceID       service.ServiceID         `json:"service_id,omitempty"`
	TTL             time.Duration             `json:"ttl"`
	Deadline        time.Time                 `json:"deadline,omitempty"`
	ExpectedVersion uint64                    `json:"expected_version"`
	Service         *ServiceChange            `json:"service,omitempty"`
	Receipt         receipt.Record            `json:"receipt"`
	Audit           []event.Audit             `json:"audit"`
	Events          []event.Envelope          `json:"events"`
	Authority       []authorize.AuthorityRead `json:"authority"`
}

// Validate checks the scope, principal, exactly one target, the expected
// version, the positive ttl and the optional service companion.
func (request IssueRequest) Validate() error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if _, err := id.ParsePrincipalID(string(request.PrincipalID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "credential request principal id is invalid", err)
	}
	switch {
	case request.ExecutionID != "" && request.ServiceID != "":
		return fault.New(fault.InvalidInput, "credential request names both an execution and a service")
	case request.ExecutionID != "":
		if _, err := id.ParseExecutionID(string(request.ExecutionID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "credential request execution id is invalid", err)
		}
	case request.ServiceID != "":
		if _, err := service.ParseServiceID(string(request.ServiceID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "credential request service id is invalid", err)
		}
	default:
		return fault.New(fault.InvalidInput, "credential request names neither an execution nor a service")
	}
	if request.ExpectedVersion == 0 {
		return fault.New(fault.InvalidInput, "credential request expected version is zero")
	}
	if request.TTL <= 0 {
		return fault.New(fault.InvalidInput, "credential request ttl is not positive")
	}
	if request.ExecutionID != "" && request.Service != nil {
		return fault.New(fault.InvalidInput, "execution credential request carries a service change")
	}
	if request.Service != nil {
		if err := request.Service.Validate(); err != nil {
			return err
		}
		if request.Service.State != nil && *request.Service.State == service.StateDisabled {
			// Issuing or rotating a credential while disabling the service would
			// commit a non-empty pair that can never authenticate and leave a
			// dormant credential behind. Disablement is the clearing path: an
			// empty credential plus token.InvalidationDisabled.
			return fault.New(fault.InvalidInput, "credential issue or rotation cannot disable a service; disablement clears the credential pair")
		}
	}
	return nil
}

// IssueResult is one committed issue/rotate result. Delivery is present only for
// an execution credential; a service token is returned for secure delivery to
// its owner and is never mounted.
type IssueResult struct {
	State     State     `json:"state"`
	Version   uint64    `json:"version"`
	Replayed  bool      `json:"replayed"`
	Token     string    `json:"-"`
	Delivery  *Delivery `json:"-"`
	Delivered bool      `json:"delivered"`
}

// EnsureResult is the container-activation result. A reused credential, or a
// replayed commit whose plaintext was never stored, returns no token and no
// delivery.
type EnsureResult struct {
	State     State     `json:"state"`
	Version   uint64    `json:"version"`
	Reused    bool      `json:"reused"`
	Token     string    `json:"-"`
	Delivery  *Delivery `json:"-"`
	Delivered bool      `json:"delivered"`
}

// RevokeRequest is one credential revocation input.
type RevokeRequest struct {
	Scope           tenancy.Scope             `json:"scope"`
	PrincipalID     id.PrincipalID            `json:"principal_id"`
	ExecutionID     id.ExecutionID            `json:"execution_id,omitempty"`
	ServiceID       service.ServiceID         `json:"service_id,omitempty"`
	ExpectedVersion uint64                    `json:"expected_version"`
	Reason          InvalidationReason        `json:"reason"`
	Receipt         receipt.Record            `json:"receipt"`
	Audit           []event.Audit             `json:"audit"`
	Events          []event.Envelope          `json:"events"`
	Authority       []authorize.AuthorityRead `json:"authority"`
}

// Validate checks the scope, principal, exactly one target, the expected
// version and the declared invalidation reason.
func (request RevokeRequest) Validate() error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if _, err := id.ParsePrincipalID(string(request.PrincipalID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "revocation principal id is invalid", err)
	}
	switch {
	case request.ExecutionID != "" && request.ServiceID != "":
		return fault.New(fault.InvalidInput, "revocation names both an execution and a service")
	case request.ExecutionID != "":
		if _, err := id.ParseExecutionID(string(request.ExecutionID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "revocation execution id is invalid", err)
		}
	case request.ServiceID != "":
		if _, err := service.ParseServiceID(string(request.ServiceID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "revocation service id is invalid", err)
		}
	default:
		return fault.New(fault.InvalidInput, "revocation names neither an execution nor a service")
	}
	if request.ExpectedVersion == 0 {
		return fault.New(fault.InvalidInput, "revocation expected version is zero")
	}
	if !request.Reason.Valid() {
		return fault.New(fault.InvalidInput, "revocation reason is not frozen")
	}
	return nil
}

// RevokeResult is one committed revocation and its post-commit invalidation.
type RevokeResult struct {
	Mutation     MutationResult `json:"mutation"`
	Invalidation Invalidation   `json:"invalidation"`
	Invalidated  bool           `json:"invalidated"`
}

// AuthenticateRequest is one derived-credential authentication input. The
// principal/target identifiers are lookup hints supplied by the caller; the
// presented token is authoritative only after its MAC and the current state
// agree.
type AuthenticateRequest struct {
	Scope       tenancy.Scope     `json:"scope"`
	Presented   string            `json:"-"`
	PrincipalID id.PrincipalID    `json:"principal_id"`
	ExecutionID id.ExecutionID    `json:"execution_id,omitempty"`
	ServiceID   service.ServiceID `json:"service_id,omitempty"`
	Now         time.Time         `json:"-"`
}

// Validate checks the scope, token, authoritative time, principal and exactly
// one target before any read.
func (request AuthenticateRequest) Validate() error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if request.Presented == "" {
		return fault.New(fault.InvalidInput, "presented credential is empty")
	}
	if request.Now.IsZero() {
		return fault.New(fault.InvalidInput, "authentication time is zero")
	}
	if _, err := id.ParsePrincipalID(string(request.PrincipalID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "authentication principal id is invalid", err)
	}
	switch {
	case request.ExecutionID != "" && request.ServiceID != "":
		return fault.New(fault.InvalidInput, "authentication names both an execution and a service")
	case request.ExecutionID != "":
		if _, err := id.ParseExecutionID(string(request.ExecutionID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "authentication execution id is invalid", err)
		}
	case request.ServiceID != "":
		if _, err := service.ParseServiceID(string(request.ServiceID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "authentication service id is invalid", err)
		}
	default:
		return fault.New(fault.InvalidInput, "authentication names neither an execution nor a service")
	}
	return nil
}

// Authenticated is the current authenticated identity of one derived
// credential. Roles and Version are the current committed values, never the
// values carried by the presented token.
type Authenticated struct {
	PrincipalID id.PrincipalID         `json:"principal_id"`
	Kind        identity.PrincipalKind `json:"kind"`
	Scope       tenancy.Scope          `json:"scope"`
	Roles       []tenancy.Role         `json:"roles,omitempty"`
	Enabled     bool                   `json:"enabled"`
	Version     uint64                 `json:"version"`
	ExecutionID id.ExecutionID         `json:"execution_id,omitempty"`
	ServiceID   service.ServiceID      `json:"service_id,omitempty"`
}

// Issue commits a first credential pair and, for an execution, delivers the
// token after the commit.
func (manager *Manager) Issue(ctx context.Context, request IssueRequest) (IssueResult, error) {
	return manager.mutate(ctx, request, MutationIssue, false)
}

// Rotate replaces an existing credential pair and refreshes an execution mount
// after the commit. An absent pair is refused rather than silently issued.
func (manager *Manager) Rotate(ctx context.Context, request IssueRequest) (IssueResult, error) {
	return manager.mutate(ctx, request, MutationRotate, true)
}

// EnsureExecution reuses a still-current execution credential without mutating
// and issues a fresh one when the pair is absent or expired.
func (manager *Manager) EnsureExecution(ctx context.Context, request IssueRequest) (EnsureResult, error) {
	if err := request.Validate(); err != nil {
		return EnsureResult{}, err
	}
	if request.ExecutionID == "" || request.ServiceID != "" {
		return EnsureResult{}, fault.New(fault.InvalidInput, "execution activation requires exactly one execution target")
	}
	if request.Service != nil {
		return EnsureResult{}, fault.New(fault.InvalidInput, "execution activation carries a service change")
	}
	now := manager.clock.Now()
	if now.IsZero() {
		return EnsureResult{}, fault.New(fault.InvalidInput, "credential clock returned zero time")
	}
	if _, err := manager.expiry(now, request); err != nil {
		return EnsureResult{}, err
	}
	current, err := identity.Current(ctx, manager.principals, identity.PrincipalRequest{
		Scope:       request.Scope,
		PrincipalID: request.PrincipalID,
	})
	if err != nil {
		return EnsureResult{}, publicFault("current execution principal is unavailable", err, fault.Unauthenticated)
	}
	if current.Kind != identity.KindExecution || current.ExecutionID != request.ExecutionID {
		return EnsureResult{}, fault.New(fault.Unauthenticated, "current principal is not the target execution")
	}
	if current.Version != request.ExpectedVersion {
		return EnsureResult{}, versionConflict(current.Version, request.ExpectedVersion)
	}
	fact, err := manager.readFact(ctx, request)
	if err != nil {
		return EnsureResult{}, err
	}
	if fact.Credential.State(now) == StateCurrent {
		return EnsureResult{State: StateCurrent, Version: fact.Version, Reused: true}, nil
	}
	result, err := manager.mutate(ctx, request, MutationIssue, false)
	if err != nil {
		return EnsureResult{
			State:     result.State,
			Version:   result.Version,
			Reused:    result.Replayed,
			Token:     result.Token,
			Delivery:  result.Delivery,
			Delivered: result.Delivered,
		}, err
	}
	return EnsureResult{
		State:     result.State,
		Version:   result.Version,
		Reused:    result.Replayed,
		Token:     result.Token,
		Delivery:  result.Delivery,
		Delivered: result.Delivered,
	}, nil
}

// Revoke clears the current pair atomically and only then invalidates live
// consumers. A failed commit never invalidates; a lost invalidation is returned
// as a retryable command without mutating again.
func (manager *Manager) Revoke(ctx context.Context, request RevokeRequest) (RevokeResult, error) {
	if err := request.Validate(); err != nil {
		return RevokeResult{}, err
	}
	now := manager.clock.Now()
	if now.IsZero() {
		return RevokeResult{}, fault.New(fault.InvalidInput, "credential clock returned zero time")
	}
	// Refuse a disabled principal or service on every attempt, including a
	// replay, but defer the expected-version comparison until the durable
	// receipt has been consulted. The original commit already advanced the
	// version, so comparing first would hide the committed result and skip the
	// post-commit invalidation.
	var currentVersion uint64
	var err error
	if request.ExecutionID != "" {
		currentVersion, err = manager.executionAuthorityVersion(ctx, request.Scope, request.PrincipalID, request.ExecutionID)
	} else {
		currentVersion, err = manager.serviceAuthorityVersion(ctx, request.Scope, request.ServiceID)
	}
	if err != nil {
		return RevokeResult{}, err
	}
	if replayed, found, replayErr := manager.replayRevoke(ctx, request, now); found {
		return replayed, replayErr
	} else if replayErr != nil {
		return RevokeResult{}, replayErr
	}
	if currentVersion != request.ExpectedVersion {
		return RevokeResult{}, versionConflict(currentVersion, request.ExpectedVersion)
	}
	unit := MutationUnit{
		Owner:           request.Scope,
		PrincipalID:     request.PrincipalID,
		ExecutionID:     request.ExecutionID,
		ServiceID:       request.ServiceID,
		Kind:            MutationRevoke,
		ExpectedVersion: request.ExpectedVersion,
		Credential:      Credential{},
		Receipt:         request.Receipt,
		Audit:           append([]event.Audit(nil), request.Audit...),
		Events:          append([]event.Envelope(nil), request.Events...),
		Authority:       append([]authorize.AuthorityRead(nil), request.Authority...),
	}
	result, err := manager.mutations.Mutate(ctx, unit)
	if err != nil {
		return RevokeResult{}, publicFault("credential mutation failed", err, fault.DependencyUnavailable)
	}
	invalidation := Invalidation{
		Scope:       request.Scope,
		PrincipalID: request.PrincipalID,
		ExecutionID: request.ExecutionID,
		ServiceID:   request.ServiceID,
		Reason:      request.Reason,
		At:          now,
	}
	if err := invalidation.Validate(); err != nil {
		return RevokeResult{Mutation: result, Invalidation: invalidation}, err
	}
	revoked := RevokeResult{Mutation: result, Invalidation: invalidation}
	if err := manager.invalidations.Invalidate(ctx, invalidation); err != nil {
		return revoked, publicFault("credential invalidation failed", err, fault.UncertainExternalEffect)
	}
	revoked.Invalidated = true
	return revoked, nil
}

// Authenticate verifies one derived credential against the current tenant key,
// the current nonce/expiry pair and the current principal/service state. Every
// call re-reads current state; nothing is cached.
func (manager *Manager) Authenticate(ctx context.Context, request AuthenticateRequest) (Authenticated, error) {
	if err := request.Validate(); err != nil {
		return Authenticated{}, err
	}
	execution := request.ExecutionID != ""
	prefix := ServicePrefix
	if execution {
		prefix = ExecutionPrefix
	}
	if !strings.HasPrefix(request.Presented, prefix) {
		return Authenticated{}, fault.New(fault.Unauthenticated, "presented credential has the wrong kind")
	}
	credentialRequest := CredentialRequest{
		Scope:       request.Scope,
		PrincipalID: request.PrincipalID,
		ExecutionID: request.ExecutionID,
		ServiceID:   request.ServiceID,
	}
	if err := credentialRequest.Validate(); err != nil {
		return Authenticated{}, err
	}
	fact, err := manager.credentials.ReadCredential(ctx, credentialRequest)
	if err != nil {
		return Authenticated{}, authReadFault("credential state is unavailable", err)
	}
	if err := checkFactIdentity(fact, credentialRequest); err != nil {
		return Authenticated{}, err
	}
	if err := checkFactCurrent(fact, request.Now); err != nil {
		return Authenticated{}, err
	}
	key, err := manager.authenticateKey(ctx, request.Scope.TenantID)
	if err != nil {
		return Authenticated{}, err
	}
	if execution {
		claims := ExecutionClaims{
			TenantID:    request.Scope.TenantID,
			ExecutionID: request.ExecutionID,
			Nonce:       fact.Credential.Nonce,
			Expiry:      fact.Credential.Expiry,
		}
		if err := VerifyExecutionToken(key, request.Presented, claims, request.Now); err != nil {
			return Authenticated{}, fault.Wrap(fault.Unauthenticated, "presented credential is invalid", err)
		}
	} else {
		claims := ServiceClaims{
			TenantID:  request.Scope.TenantID,
			ServiceID: request.ServiceID,
			Nonce:     fact.Credential.Nonce,
			Expiry:    fact.Credential.Expiry,
		}
		if err := VerifyServiceToken(key, request.Presented, claims, request.Now); err != nil {
			return Authenticated{}, fault.Wrap(fault.Unauthenticated, "presented credential is invalid", err)
		}
	}
	if execution {
		current, err := manager.currentPrincipal(ctx, identity.PrincipalRequest{
			Scope:       request.Scope,
			PrincipalID: request.PrincipalID,
		})
		if err != nil {
			return Authenticated{}, err
		}
		verified := identity.Principal{
			ID:          request.PrincipalID,
			Kind:        identity.KindExecution,
			Scope:       request.Scope,
			Enabled:     true,
			ExecutionID: request.ExecutionID,
		}
		if err := identity.CheckCurrent(verified, current); err != nil {
			return Authenticated{}, fault.Wrap(fault.Unauthenticated, "current principal does not match the credential", err)
		}
		return Authenticated{
			PrincipalID: current.ID,
			Kind:        current.Kind,
			Scope:       current.Scope,
			Roles:       append([]tenancy.Role(nil), current.Roles...),
			Enabled:     current.Enabled,
			Version:     current.Version,
			ExecutionID: current.ExecutionID,
		}, nil
	}
	currentService, err := manager.services.ReadService(ctx, service.ServiceReadRequest{
		Scope:     request.Scope,
		ServiceID: request.ServiceID,
	})
	if err != nil {
		return Authenticated{}, authReadFault("current service is unavailable", err)
	}
	if err := currentService.Validate(); err != nil {
		return Authenticated{}, fault.Wrap(fault.Unauthenticated, "current service is invalid", err)
	}
	if currentService.ID != request.ServiceID {
		return Authenticated{}, fault.New(fault.Unauthenticated, "current service does not match the credential")
	}
	verified := identity.Principal{
		ID:      request.PrincipalID,
		Kind:    identity.KindService,
		Scope:   request.Scope,
		Enabled: true,
	}
	current := identity.Principal{
		ID:      request.PrincipalID,
		Kind:    identity.KindService,
		Scope:   currentService.Scope,
		Roles:   append([]tenancy.Role(nil), currentService.Roles...),
		Enabled: currentService.State == service.StateEnabled,
		Version: currentService.Version,
	}
	if err := identity.CheckCurrent(verified, current); err != nil {
		return Authenticated{}, fault.Wrap(fault.Unauthenticated, "current service does not match the credential", err)
	}
	return Authenticated{
		PrincipalID: current.ID,
		Kind:        current.Kind,
		Scope:       current.Scope,
		Roles:       current.Roles,
		Enabled:     current.Enabled,
		Version:     current.Version,
		ServiceID:   request.ServiceID,
	}, nil
}

func (manager *Manager) mutate(ctx context.Context, request IssueRequest, kind MutationKind, requireCurrent bool) (IssueResult, error) {
	if err := request.Validate(); err != nil {
		return IssueResult{}, err
	}
	now := manager.clock.Now()
	if now.IsZero() {
		return IssueResult{}, fault.New(fault.InvalidInput, "credential clock returned zero time")
	}
	expiry, err := manager.expiry(now, request)
	if err != nil {
		return IssueResult{}, err
	}
	// Re-read the current principal/service identity and enablement first: a
	// disabled principal or service is still refused on a retry. The expected
	// version is compared only after the durable receipt has been consulted,
	// because the original attempt already advanced it.
	var currentVersion uint64
	if request.ExecutionID != "" {
		currentVersion, err = manager.executionAuthorityVersion(ctx, request.Scope, request.PrincipalID, request.ExecutionID)
	} else {
		currentVersion, err = manager.serviceAuthorityVersion(ctx, request.Scope, request.ServiceID)
	}
	if err != nil {
		return IssueResult{}, err
	}
	if replayed, found, err := manager.replayIssue(ctx, request); err != nil {
		return IssueResult{}, err
	} else if found {
		return replayed, nil
	}
	if requireCurrent {
		fact, err := manager.readFact(ctx, request)
		if err != nil {
			return IssueResult{}, err
		}
		if !fact.Credential.Present() {
			return IssueResult{}, fault.New(fault.Unauthenticated, "no current credential to rotate")
		}
	}
	if currentVersion != request.ExpectedVersion {
		return IssueResult{}, versionConflict(currentVersion, request.ExpectedVersion)
	}
	key, err := manager.readKey(ctx, request.Scope.TenantID)
	if err != nil {
		return IssueResult{}, err
	}
	nonce, err := manager.newNonce()
	if err != nil {
		return IssueResult{}, err
	}
	presented, err := deriveCredentialToken(key, request, nonce, expiry)
	if err != nil {
		return IssueResult{}, err
	}
	unit := MutationUnit{
		Owner:           request.Scope,
		PrincipalID:     request.PrincipalID,
		ExecutionID:     request.ExecutionID,
		ServiceID:       request.ServiceID,
		Kind:            kind,
		ExpectedVersion: request.ExpectedVersion,
		Credential:      Credential{Nonce: nonce, Expiry: expiry},
		Service:         cloneServiceChange(request.Service),
		Receipt:         request.Receipt,
		Audit:           append([]event.Audit(nil), request.Audit...),
		Events:          append([]event.Envelope(nil), request.Events...),
		Authority:       append([]authorize.AuthorityRead(nil), request.Authority...),
	}
	if request.ExecutionID != "" {
		postCommit := event.PostDeliverCredential
		if kind == MutationRotate {
			postCommit = event.PostRefreshCredential
		}
		unit.PostCommit = []event.PostCommitEffect{{Kind: postCommit, Target: string(request.ExecutionID)}}
	}
	result, err := manager.mutations.Mutate(ctx, unit)
	if err != nil {
		return IssueResult{}, publicFault("credential mutation failed", err, fault.DependencyUnavailable)
	}
	issueResult := IssueResult{
		State:    result.State,
		Version:  result.Version,
		Replayed: result.Replayed,
		Token:    presented,
	}
	if result.Replayed {
		// An equal receipt was already committed by an earlier attempt. That
		// attempt owns the committed nonce/expiry pair and its delivery; the
		// nonce derived here was never committed, so returning or delivering
		// this token would hand out a credential that cannot authenticate.
		// Report the committed result with no token and no delivery.
		issueResult.Token = ""
		issueResult.Delivery = nil
		issueResult.Delivered = false
		return issueResult, nil
	}
	if request.ExecutionID == "" {
		return issueResult, nil
	}
	delivery := &Delivery{
		Scope:       request.Scope,
		PrincipalID: request.PrincipalID,
		ExecutionID: request.ExecutionID,
		Kind:        kind,
		Token:       presented,
		Target:      string(request.ExecutionID),
		Refresh:     kind == MutationRotate,
	}
	if err := delivery.Validate(); err != nil {
		return issueResult, err
	}
	issueResult.Delivery = delivery
	if err := manager.deliverer.Deliver(ctx, *delivery); err != nil {
		return issueResult, publicFault("credential delivery failed", err, fault.UncertainExternalEffect)
	}
	issueResult.Delivered = true
	return issueResult, nil
}

func (manager *Manager) expiry(now time.Time, request IssueRequest) (time.Time, error) {
	if !manager.bounds.ValidTTL(request.TTL) {
		return time.Time{}, fault.New(fault.InvalidInput, "credential ttl is outside the allowed bounds")
	}
	expiry := now.Add(request.TTL)
	if !request.Deadline.IsZero() {
		if !now.Before(request.Deadline) {
			return time.Time{}, fault.New(fault.DeadlineExpired, "action deadline has passed")
		}
		if request.Deadline.Before(expiry) {
			expiry = request.Deadline
		}
	}
	if expiry.Sub(now) < manager.bounds.MinTTL {
		return time.Time{}, fault.New(fault.InvalidInput, "credential lifetime is shorter than the minimum")
	}
	return expiry, nil
}

func (manager *Manager) readFact(ctx context.Context, request IssueRequest) (CredentialFact, error) {
	credentialRequest := CredentialRequest{
		Scope:       request.Scope,
		PrincipalID: request.PrincipalID,
		ExecutionID: request.ExecutionID,
		ServiceID:   request.ServiceID,
	}
	if err := credentialRequest.Validate(); err != nil {
		return CredentialFact{}, err
	}
	fact, err := manager.credentials.ReadCredential(ctx, credentialRequest)
	if err != nil {
		return CredentialFact{}, publicFault("credential state is unavailable", err, fault.DependencyUnavailable)
	}
	if err := checkFactIdentity(fact, credentialRequest); err != nil {
		return CredentialFact{}, err
	}
	return fact, nil
}

func (manager *Manager) executionAuthorityVersion(ctx context.Context, scope tenancy.Scope, principalID id.PrincipalID, executionID id.ExecutionID) (uint64, error) {
	current, err := identity.Current(ctx, manager.principals, identity.PrincipalRequest{
		Scope:       scope,
		PrincipalID: principalID,
	})
	if err != nil {
		return 0, publicFault("current execution principal is unavailable", err, fault.Unauthenticated)
	}
	if current.Kind != identity.KindExecution || current.ExecutionID != executionID {
		return 0, fault.New(fault.Unauthenticated, "current principal is not the target execution")
	}
	return current.Version, nil
}

func (manager *Manager) serviceAuthorityVersion(ctx context.Context, scope tenancy.Scope, serviceID service.ServiceID) (uint64, error) {
	current, err := manager.services.ReadService(ctx, service.ServiceReadRequest{Scope: scope, ServiceID: serviceID})
	if err != nil {
		return 0, publicFault("current service is unavailable", err, fault.DependencyUnavailable)
	}
	if err := current.Validate(); err != nil {
		return 0, publicFault("current service is invalid", err, fault.InvalidInput)
	}
	if current.ID != serviceID || current.Scope != scope {
		return 0, fault.New(fault.Unauthenticated, "current service is outside the caller scope")
	}
	if current.State != service.StateEnabled {
		return 0, fault.New(fault.Unauthenticated, "current service is disabled")
	}
	return current.Version, nil
}

func (manager *Manager) readKey(ctx context.Context, tenantID id.TenantID) ([]byte, error) {
	key, err := manager.keys.ReadTenantKey(ctx, TenantKeyRequest{TenantID: tenantID})
	if err != nil {
		return nil, publicFault("tenant derivation key is unavailable", err, fault.DependencyUnavailable)
	}
	if key.TenantID != tenantID {
		return nil, fault.New(fault.Unauthenticated, "tenant derivation key is outside the caller scope")
	}
	if err := key.Validate(); err != nil {
		return nil, publicFault("tenant derivation key is invalid", err, fault.Unauthenticated)
	}
	plaintext, err := manager.resolver.ResolveDerivationKey(ctx, key)
	if err != nil {
		return nil, publicFault("tenant derivation key cannot be resolved", err, fault.DependencyUnavailable)
	}
	if len(plaintext) < manager.bounds.MinNonceBytes {
		return nil, fault.New(fault.Unauthenticated, "tenant derivation key is shorter than the minimum")
	}
	return plaintext, nil
}

// authReadFault classifies one current-state read failure inside Authenticate.
// A genuinely absent row is an authentication refusal; every other failure
// (transport, dependency, context or an unavailable store) is a retryable
// dependency outage, so a store outage never reads to a client as a revoked
// credential. The private cause stays reachable for diagnostics but is never
// rendered by the public error.
func authReadFault(message string, err error) error {
	if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
		return fault.Wrap(fault.Unauthenticated, message, err)
	}
	return fault.Wrap(fault.DependencyUnavailable, message, err)
}

// currentPrincipal re-reads one current principal for authentication. Unlike
// identity.Current, whose every read failure is unauthenticated, it keeps a
// genuine not-found as a refusal and reports any other read failure as an
// unavailable dependency.
func (manager *Manager) currentPrincipal(ctx context.Context, request identity.PrincipalRequest) (identity.Principal, error) {
	current, err := manager.principals.ReadPrincipal(ctx, request)
	if err != nil {
		return identity.Principal{}, authReadFault("current principal is unavailable", err)
	}
	if err := current.Validate(); err != nil {
		return identity.Principal{}, fault.Wrap(fault.Unauthenticated, "current principal is invalid", err)
	}
	if current.Scope != request.Scope || current.ID != request.PrincipalID {
		return identity.Principal{}, fault.New(fault.Unauthenticated, "current principal does not match the request")
	}
	if !current.Enabled {
		return identity.Principal{}, fault.New(fault.Unauthenticated, "current principal is disabled")
	}
	return current, nil
}

func (manager *Manager) authenticateKey(ctx context.Context, tenantID id.TenantID) ([]byte, error) {
	key, err := manager.keys.ReadTenantKey(ctx, TenantKeyRequest{TenantID: tenantID})
	if err != nil {
		return nil, authReadFault("tenant derivation key is unavailable", err)
	}
	if key.TenantID != tenantID {
		return nil, fault.New(fault.Unauthenticated, "tenant derivation key is outside the caller scope")
	}
	if err := key.Validate(); err != nil {
		return nil, fault.Wrap(fault.Unauthenticated, "tenant derivation key is invalid", err)
	}
	plaintext, err := manager.resolver.ResolveDerivationKey(ctx, key)
	if err != nil {
		return nil, authReadFault("tenant derivation key cannot be resolved", err)
	}
	if len(plaintext) < manager.bounds.MinNonceBytes {
		return nil, fault.New(fault.Unauthenticated, "tenant derivation key is shorter than the minimum")
	}
	return plaintext, nil
}

func (manager *Manager) newNonce() ([]byte, error) {
	nonce := make([]byte, manager.bounds.MinNonceBytes)
	if _, err := io.ReadFull(manager.random, nonce); err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "credential nonce generation failed", err)
	}
	return nonce, nil
}

func deriveCredentialToken(key []byte, request IssueRequest, nonce []byte, expiry time.Time) (string, error) {
	if request.ExecutionID != "" {
		return DeriveExecutionToken(key, ExecutionClaims{
			TenantID:    request.Scope.TenantID,
			ExecutionID: request.ExecutionID,
			Nonce:       nonce,
			Expiry:      expiry,
		})
	}
	return DeriveServiceToken(key, ServiceClaims{
		TenantID:  request.Scope.TenantID,
		ServiceID: request.ServiceID,
		Nonce:     nonce,
		Expiry:    expiry,
	})
}

func checkFactIdentity(fact CredentialFact, request CredentialRequest) error {
	if fact.Scope != request.Scope {
		return fault.New(fault.Unauthenticated, "credential fact is outside the caller scope")
	}
	if fact.PrincipalID != request.PrincipalID {
		return fault.New(fault.Unauthenticated, "credential fact is for another principal")
	}
	if request.ExecutionID != "" {
		if fact.ExecutionID != request.ExecutionID || fact.ServiceID != "" {
			return fault.New(fault.Unauthenticated, "credential fact is for another target")
		}
	} else {
		if fact.ServiceID != request.ServiceID || fact.ExecutionID != "" {
			return fault.New(fault.Unauthenticated, "credential fact is for another target")
		}
	}
	if !fact.Enabled {
		return fault.New(fault.Unauthenticated, "credential principal is disabled")
	}
	return nil
}

func checkFactCurrent(fact CredentialFact, now time.Time) error {
	if !fact.Credential.Present() {
		return fault.New(fault.Unauthenticated, "no current credential is stored")
	}
	if fact.Credential.State(now) != StateCurrent {
		return fault.New(fault.Unauthenticated, "credential is not current")
	}
	return nil
}

func versionConflict(current, expected uint64) error {
	return fault.New(fault.VersionConflict, fmt.Sprintf(
		"current version %d does not match expected version %d", current, expected))
}

func cloneServiceChange(change *ServiceChange) *ServiceChange {
	if change == nil {
		return nil
	}
	cloned := &ServiceChange{}
	if change.Roles != nil {
		cloned.Roles = make([]tenancy.Role, len(change.Roles))
		copy(cloned.Roles, change.Roles)
	}
	if change.State != nil {
		state := *change.State
		cloned.State = &state
	}
	return cloned
}

func publicFault(message string, err error, fallback fault.Class) error {
	if class, ok := fault.ClassOf(err); ok {
		return fault.Wrap(class, message, err)
	}
	return fault.Wrap(fallback, message, err)
}
