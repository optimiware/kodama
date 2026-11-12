package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
)

// DevAuthenticator is the explicitly named development/test authenticator.
//
// It maps static configured bearer tokens to principals and exposes the
// configured session expiry. It is constructed only by the development and test
// profiles; production refuses it.
type DevAuthenticator struct {
	clock   Clock
	mu      sync.RWMutex
	byToken map[string]devSession
	byID    map[string]time.Time
}

type devSession struct {
	principal identity.Principal
	expiresAt time.Time
}

// NewDevAuthenticator validates the configured tokens and builds the
// development authenticator.
func NewDevAuthenticator(clock Clock, tokens []DevToken) (*DevAuthenticator, error) {
	if clock == nil {
		return nil, fault.New(fault.InvalidInput, "development authenticator requires a clock")
	}
	authenticator := &DevAuthenticator{
		clock:   clock,
		byToken: make(map[string]devSession, len(tokens)),
		byID:    make(map[string]time.Time, len(tokens)),
	}
	for _, token := range tokens {
		if token.Credential == "" {
			return nil, fault.New(fault.InvalidInput, "development authenticator token is empty")
		}
		if err := token.Principal.Validate(); err != nil {
			return nil, fault.Wrap(fault.InvalidInput, "development authenticator principal is invalid", err)
		}
		if token.ExpiresAt.IsZero() {
			return nil, fault.New(fault.InvalidInput, "development authenticator token has no expiry")
		}
		if _, exists := authenticator.byToken[token.Credential]; exists {
			return nil, fault.New(fault.InvalidInput, "development authenticator token is configured twice")
		}
		authenticator.byToken[token.Credential] = devSession{
			principal: token.Principal,
			expiresAt: token.ExpiresAt,
		}
		if token.Principal.Kind == identity.KindHuman {
			if existing, ok := authenticator.byID[string(token.Principal.ID)]; !ok || token.ExpiresAt.After(existing) {
				authenticator.byID[string(token.Principal.ID)] = token.ExpiresAt
			}
		}
	}
	return authenticator, nil
}

// Resolve authenticates one static development credential.
func (authenticator *DevAuthenticator) Resolve(ctx context.Context, credential string) (identity.Principal, error) {
	if err := ctx.Err(); err != nil {
		return identity.Principal{}, fault.Wrap(fault.Unauthenticated, "development authentication was canceled", err)
	}
	authenticator.mu.RLock()
	session, ok := authenticator.byToken[credential]
	authenticator.mu.RUnlock()
	if !ok {
		return identity.Principal{}, fault.New(fault.Unauthenticated, "presented credential is invalid")
	}
	if !authenticator.clock.Now().Before(session.expiresAt) {
		return identity.Principal{}, fault.New(fault.Unauthenticated, "presented credential has expired")
	}
	return session.principal, nil
}

// ValidUntil returns the configured human session expiry. A non-human
// principal has no development session expiry here.
func (authenticator *DevAuthenticator) ValidUntil(ctx context.Context, principal identity.Principal) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, fault.Wrap(fault.DeadlineExpired, "credential validity read was canceled", err)
	}
	if principal.Kind != identity.KindHuman {
		return time.Time{}, fault.New(fault.NotFound, "development session expiry is human-only")
	}
	authenticator.mu.RLock()
	expiry, ok := authenticator.byID[string(principal.ID)]
	authenticator.mu.RUnlock()
	if !ok {
		return time.Time{}, fault.New(fault.NotFound, "development session is not configured")
	}
	return expiry, nil
}

// bindingAuthenticator resolves the main credentials and a request-local
// composition credential that binds an already authenticated principal to a
// capability that still authenticates its own input (event ingress, model
// gateway, MCP proxy).
type bindingAuthenticator struct {
	base  authorize.CurrentPrincipal
	clock Clock

	mu    sync.Mutex
	bound map[string]identity.Principal
}

func newBindingAuthenticator(base authorize.CurrentPrincipal, clock Clock) (*bindingAuthenticator, error) {
	if base == nil {
		return nil, fault.New(fault.InvalidInput, "binding authenticator requires a base principal resolver")
	}
	if clock == nil {
		return nil, fault.New(fault.InvalidInput, "binding authenticator requires a clock")
	}
	return &bindingAuthenticator{base: base, clock: clock, bound: make(map[string]identity.Principal)}, nil
}

// Resolve authenticates one credential, preferring a request-local binding.
func (authenticator *bindingAuthenticator) Resolve(ctx context.Context, credential string) (identity.Principal, error) {
	authenticator.mu.Lock()
	principal, bound := authenticator.bound[credential]
	authenticator.mu.Unlock()
	if bound {
		return principal, nil
	}
	return authenticator.base.Resolve(ctx, credential)
}

// withPrincipal runs fn with a fresh credential bound to principal. The
// credential never leaves the composition call and is removed afterwards.
func (authenticator *bindingAuthenticator) withPrincipal(principal identity.Principal, fn func(string) error) error {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fault.Wrap(fault.InternalFailure, "composition credential allocation failed", err)
	}
	credential := "compose-" + hex.EncodeToString(raw[:])
	authenticator.mu.Lock()
	authenticator.bound[credential] = principal
	authenticator.mu.Unlock()
	defer func() {
		authenticator.mu.Lock()
		delete(authenticator.bound, credential)
		authenticator.mu.Unlock()
	}()
	return fn(credential)
}

// composedCredentialValidity supplies the human session expiry from the
// development authenticator and delegates execution/service expiry to the
// store-backed reader.
type composedCredentialValidity struct {
	dev   *DevAuthenticator
	store event.CredentialValidity
}

var _ event.CredentialValidity = (*composedCredentialValidity)(nil)

func (validity *composedCredentialValidity) ValidUntil(ctx context.Context, principal identity.Principal) (time.Time, error) {
	if principal.Kind == identity.KindHuman {
		if validity.dev == nil {
			return time.Time{}, fault.New(fault.NotFound, "human session expiry is not configured")
		}
		return validity.dev.ValidUntil(ctx, principal)
	}
	if validity.store == nil {
		return time.Time{}, fault.New(fault.DependencyUnavailable, "store credential validity is not configured")
	}
	return validity.store.ValidUntil(ctx, principal)
}
