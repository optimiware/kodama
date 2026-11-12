package event

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// readOperation is the declared registry operation every authorized event read
// commits against.
const readOperation = authorize.OperationName("event.read")

// ReadCommand is one authorized event.read invocation. Actor is the verified
// principal supplied by the authentication boundary; Scope is the requested
// read scope. The handler applies the authorized scope to the query, so a
// caller can never choose another scope.
type ReadCommand struct {
	Actor identity.Principal `json:"actor"`
	Scope tenancy.Scope      `json:"scope"`
	Query ReadQuery          `json:"query"`
}

// ReadConfig is the complete authorized event-read composition.
type ReadConfig struct {
	Authorizer authorize.Authorizer
	Authority  authorize.AuthorityReader
	Principals identity.PrincipalReader
	Port       ReadPort
}

// Reader orchestrates one authorized, cursor-based committed-event read. It
// holds only immutable dependencies, so concurrent reads share no per-request
// state and every read re-applies the current grant.
type Reader struct {
	authorizer authorize.Authorizer
	authority  authorize.AuthorityReader
	principals identity.PrincipalReader
	port       ReadPort
}

// NewReader validates the composition and returns a reader.
func NewReader(config ReadConfig) (*Reader, error) {
	switch {
	case config.Authorizer == nil:
		return nil, fault.New(fault.InvalidInput, "event reader requires an authorizer")
	case config.Authority == nil:
		return nil, fault.New(fault.InvalidInput, "event reader requires an authority reader")
	case config.Principals == nil:
		return nil, fault.New(fault.InvalidInput, "event reader requires a current-principal reader")
	case config.Port == nil:
		return nil, fault.New(fault.InvalidInput, "event reader requires a read port")
	}
	return &Reader{
		authorizer: config.Authorizer,
		authority:  config.Authority,
		principals: config.Principals,
		port:       config.Port,
	}, nil
}

// Read authorizes event.read, confirms the current principal and the registry's
// current-authority reads, applies the authorized scope, validates the bounded
// query and returns only committed events in durable sequence order. It never
// returns a credential or an internal field, and a port page naming a foreign
// scope or an unrequested type is a dependency-contract failure.
func (reader *Reader) Read(ctx context.Context, command ReadCommand) (ReadPage, error) {
	operation, err := registryOperation(readOperation)
	if err != nil {
		return ReadPage{}, err
	}
	if err := authorizeGrant(ctx, reader.authorizer, command.Actor, operation, command.Scope); err != nil {
		return ReadPage{}, err
	}
	principal, err := confirmCurrentPrincipal(ctx, reader.principals, command.Actor, operation, command.Scope)
	if err != nil {
		return ReadPage{}, err
	}
	reads := operationAuthorityReads(operation, command.Scope, principal.ID)
	if err := confirmAuthority(ctx, reader.authority, reads); err != nil {
		return ReadPage{}, err
	}
	if command.Query.Scope != (tenancy.Scope{}) && command.Query.Scope != command.Scope {
		return ReadPage{}, fault.New(fault.Forbidden, "event read query names a foreign scope")
	}
	query := command.Query
	query.Scope = command.Scope
	if err := query.Validate(); err != nil {
		return ReadPage{}, err
	}
	page, err := reader.port.Read(ctx, query)
	if err != nil {
		return ReadPage{}, err
	}
	if err := validateReadPage(query, page); err != nil {
		return ReadPage{}, err
	}
	return page, nil
}

// validateReadPage enforces the consumer-owned read contract: the cursor never
// regresses, every returned event is a valid committed row in the authorized
// scope and every event matches the authorized type filter.
func validateReadPage(query ReadQuery, page ReadPage) error {
	if page.Next < query.AfterSequence {
		return fault.New(fault.InternalFailure, "event read page cursor regressed")
	}
	var allowed map[Type]bool
	if len(query.Types) > 0 {
		allowed = make(map[Type]bool, len(query.Types))
		for _, eventType := range query.Types {
			allowed[eventType] = true
		}
	}
	for _, envelope := range page.Events {
		if envelope.Scope != query.Scope {
			return fault.New(fault.InternalFailure, "event read page returned a foreign scope")
		}
		if err := envelope.Validate(); err != nil {
			return fault.Wrap(fault.InternalFailure, "event read page returned an invalid committed event", err)
		}
		if allowed != nil && !allowed[envelope.Type] {
			return fault.New(fault.InternalFailure, "event read page returned an unrequested event type")
		}
	}
	return nil
}
