package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/optimiware/kodama/internal/domain"
)

// Authority answers the current effective-access question for any module: may
// the actor perform the permission at the target right now? It is the one
// consumer-owned authorization seam. Logs, streams, source, artifacts and human
// gates ask it the same question as work and configuration do; the resource
// owner remains responsible for scoped lookup of the resource itself.
//
// Outcomes: nil allows. ErrHidden means the caller must not learn whether the
// target exists. ErrDenied means the caller may know but is not permitted.
// Anything else (ErrUnavailable or an unclassified error) is an outage and
// never access. Implementations read current state on every call: no positive
// result is cached, and events are notifications, not authority.
type Authority interface {
	Authorize(ctx context.Context, actor Actor, target Target, perm Permission) error
}

// AccessReader reads an actor's current bindings in one tenant. It is the
// persistence port of the standalone authority.
type AccessReader interface {
	CurrentBindings(ctx context.Context, tenant domain.TenantID, user UserID) ([]Binding, error)
}

// StandaloneAuthority is the standalone/on-premises authority profile: Kodama's
// own bindings are the source of truth. It is never a fallback for another
// profile.
type StandaloneAuthority struct {
	reader AccessReader
}

var _ Authority = (*StandaloneAuthority)(nil)

// NewStandaloneAuthority requires the reader of current bindings.
func NewStandaloneAuthority(reader AccessReader) (*StandaloneAuthority, error) {
	if reader == nil {
		return nil, errors.New("identity: a standalone authority requires an access reader")
	}
	return &StandaloneAuthority{reader: reader}, nil
}

// Authorize implements Authority from the current bindings.
func (a *StandaloneAuthority) Authorize(ctx context.Context, actor Actor, target Target, perm Permission) error {
	if err := target.TenantID.Validate(); err != nil {
		return ErrHidden
	}
	if actor.Type != ActorUser {
		return ErrDenied
	}
	if err := actor.Validate(); err != nil {
		return ErrDenied
	}
	bindings, err := a.reader.CurrentBindings(ctx, target.TenantID, UserID(actor.ID))
	if err != nil {
		return Unavailablef(err, "reading current bindings")
	}
	return Decide(bindings, actor, target, perm)
}

// Profile names an authority profile. The installation selects exactly one.
type Profile string

const (
	// ProfileStandalone uses Kodama-local OIDC membership (this module).
	ProfileStandalone Profile = "standalone"
	// ProfileHosted delegates to the platform core through an adapter selected
	// by O-18/T-061; this module provides none.
	ProfileHosted Profile = "hosted"
)

// AuthorityConfig explicitly selects the authority.
type AuthorityConfig struct {
	Profile    Profile
	Standalone *StandaloneAuthority
	// Hosted is the adapter of the hosted profile. It is consulted only for
	// ProfileHosted.
	Hosted Authority
}

// SelectAuthority returns the authority of the selected profile, wrapped so
// that an outage or an unclassified failure reports ErrUnavailable. The unselected
// profile is never consulted: a hosted denial, outage or missing adapter does
// not select the standalone grants, and standalone mode never reaches a hosted
// adapter. An empty or unknown profile is a configuration error.
func SelectAuthority(cfg AuthorityConfig) (Authority, error) {
	switch cfg.Profile {
	case ProfileStandalone:
		if cfg.Standalone == nil {
			return nil, errors.New("identity: the standalone profile requires a standalone authority")
		}
		return failClosed{cfg.Standalone}, nil
	case ProfileHosted:
		if cfg.Hosted == nil {
			return nil, errors.New("identity: the hosted profile requires a hosted authority adapter (O-18/T-061)")
		}
		return failClosed{cfg.Hosted}, nil
	case "":
		return nil, errors.New("identity: the authority profile must be selected explicitly")
	}
	return nil, fmt.Errorf("identity: unknown authority profile %q", cfg.Profile)
}

// failClosed normalizes an authority's result to the three outcomes.
type failClosed struct{ inner Authority }

func (f failClosed) Authorize(ctx context.Context, actor Actor, target Target, perm Permission) error {
	if f.inner == nil {
		return Unavailablef(nil, "no authority is configured")
	}
	err := f.inner.Authorize(ctx, actor, target, perm)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrHidden):
		return ErrHidden
	case errors.Is(err, ErrDenied):
		return ErrDenied
	case errors.Is(err, ErrUnavailable):
		return err
	}
	return Unavailablef(err, "authority failed")
}
