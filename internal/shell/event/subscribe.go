package event

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// subscribeOperation is the declared registry operation every event.subscribe
// setup commits against.
const subscribeOperation = authorize.OperationName("event.subscribe")

// SubscribeCommand is one authorized event.subscribe setup invocation. Actor is
// the verified principal supplied by the authentication boundary; Scope is the
// requested subscription scope. The handler applies the authorized scope to the
// setup, so a caller can never subscribe to another scope.
type SubscribeCommand struct {
	Actor identity.Principal `json:"actor"`
	Scope tenancy.Scope      `json:"scope"`
	Setup SubscribeSetup     `json:"setup"`
}

// SubscribeConfig is the complete event.subscribe setup composition. Cursor
// reads the event-table resume point and Validity bounds the grant by the
// principal's current credential.
type SubscribeConfig struct {
	Authorizer authorize.Authorizer
	Authority  authorize.AuthorityReader
	Principals identity.PrincipalReader
	Cursor     CursorPort
	Validity   CredentialValidity
	Clock      Clock
}

// Subscriber orchestrates one authorized subscription setup. It returns a
// validated grant value only; it never opens a broker connection or stores a
// credential.
type Subscriber struct {
	authorizer authorize.Authorizer
	authority  authorize.AuthorityReader
	principals identity.PrincipalReader
	cursor     CursorPort
	validity   CredentialValidity
	clock      Clock
}

// NewSubscriber validates the composition and returns a subscriber.
func NewSubscriber(config SubscribeConfig) (*Subscriber, error) {
	switch {
	case config.Authorizer == nil:
		return nil, fault.New(fault.InvalidInput, "event subscriber requires an authorizer")
	case config.Authority == nil:
		return nil, fault.New(fault.InvalidInput, "event subscriber requires an authority reader")
	case config.Principals == nil:
		return nil, fault.New(fault.InvalidInput, "event subscriber requires a current-principal reader")
	case config.Cursor == nil:
		return nil, fault.New(fault.InvalidInput, "event subscriber requires a cursor reader")
	case config.Validity == nil:
		return nil, fault.New(fault.InvalidInput, "event subscriber requires a credential-validity reader")
	case config.Clock == nil:
		return nil, fault.New(fault.InvalidInput, "event subscriber requires a clock")
	}
	return &Subscriber{
		authorizer: config.Authorizer,
		authority:  config.Authority,
		principals: config.Principals,
		cursor:     config.Cursor,
		validity:   config.Validity,
		clock:      config.Clock,
	}, nil
}

// Setup authorizes event.subscribe, confirms the current principal and the
// registry's current-authority reads, applies the authorized scope, derives the
// scope-qualified event subjects, reads the event-table resume point, bounds the
// grant by the current credential validity and returns the validated grant. The
// grant carries no credential or token material.
func (subscriber *Subscriber) Setup(ctx context.Context, command SubscribeCommand) (SubscribeGrant, error) {
	operation, err := registryOperation(subscribeOperation)
	if err != nil {
		return SubscribeGrant{}, err
	}
	if err := authorizeGrant(ctx, subscriber.authorizer, command.Actor, operation, command.Scope); err != nil {
		return SubscribeGrant{}, err
	}
	principal, err := confirmCurrentPrincipal(ctx, subscriber.principals, command.Actor, operation, command.Scope)
	if err != nil {
		return SubscribeGrant{}, err
	}
	if command.Setup.Scope != (tenancy.Scope{}) && command.Setup.Scope != command.Scope {
		return SubscribeGrant{}, fault.New(fault.Forbidden, "event subscribe setup names a foreign scope")
	}
	setup := command.Setup
	setup.Scope = command.Scope
	if err := setup.Validate(); err != nil {
		return SubscribeGrant{}, err
	}
	reads := operationAuthorityReads(operation, command.Scope, principal.ID)
	if err := confirmAuthority(ctx, subscriber.authority, reads); err != nil {
		return SubscribeGrant{}, err
	}

	now := subscriber.clock.Now()
	validUntil, err := subscriber.validity.ValidUntil(ctx, principal)
	if err != nil {
		return SubscribeGrant{}, err
	}
	if validUntil.IsZero() || !validUntil.After(now) {
		return SubscribeGrant{}, fault.New(fault.Forbidden, "principal credential has no remaining validity")
	}
	current, err := subscriber.cursor.Cursor(ctx, command.Scope)
	if err != nil {
		return SubscribeGrant{}, err
	}
	if setup.Cursor > current {
		return SubscribeGrant{}, fault.New(fault.InvalidInput, "event subscribe resume cursor is ahead of the committed event table")
	}
	cursor := setup.Cursor
	if cursor == 0 {
		cursor = current
	}
	subject, err := EventSubject(command.Scope)
	if err != nil {
		return SubscribeGrant{}, err
	}
	grant := SubscribeGrant{
		Subjects:  []string{subject},
		Cursor:    cursor,
		ExpiresAt: validUntil,
	}
	if err := grant.Validate(); err != nil {
		return SubscribeGrant{}, fault.Wrap(fault.InternalFailure, "built event subscribe grant is invalid", err)
	}
	if err := grant.ValidateFor(command.Scope); err != nil {
		return SubscribeGrant{}, fault.Wrap(fault.InternalFailure, "built event subscribe grant names a foreign scope", err)
	}
	return grant, nil
}
