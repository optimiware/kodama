package adapters

import (
	"context"
	"errors"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Profiles is the narrow read port of the agents service the adapters consume.
// *agents.Service satisfies it.
type Profiles interface {
	Resolve(ctx context.Context, scope domain.Scope, ref string) (agents.Resolved, error)
}

// Lookup implements pipeline.ProfileLookup. A profile reference is the
// server-returned immutable "name@version"; the pipeline module explicitly
// leaves that syntax to the profile owner.
//
// A reference is immutable only when it names one stored version of this
// project that currently meets its compatibility contract. A bare name (a moving
// alias), a malformed reference, and a stored version whose wrapper is no longer
// qualified are reported as not immutable, so validation rejects them. An
// unknown or foreign reference is pipeline.ErrReferenceNotFound, and the two are
// indistinguishable. Infrastructure failure is an outage, never a verdict.
type Lookup struct{ profiles Profiles }

var _ pipeline.ProfileLookup = Lookup{}

// NewLookup requires the profile reader.
func NewLookup(p Profiles) (Lookup, error) {
	if p == nil {
		return Lookup{}, errors.New("agents/adapters: a profile reader is required")
	}
	return Lookup{profiles: p}, nil
}

// LookupProfile implements pipeline.ProfileLookup.
func (l Lookup) LookupProfile(ctx context.Context, scope domain.Scope, name string) (pipeline.ProfileInfo, error) {
	_, err := l.profiles.Resolve(ctx, scope, name)
	switch {
	case err == nil:
		return pipeline.ProfileInfo{Immutable: true}, nil
	case errors.Is(err, agents.ErrInvalid), errors.Is(err, agents.ErrUnsupported):
		return pipeline.ProfileInfo{Immutable: false}, nil
	case errors.Is(err, agents.ErrNotFound), errors.Is(err, agents.ErrHidden):
		return pipeline.ProfileInfo{}, pipeline.ErrReferenceNotFound
	}
	return pipeline.ProfileInfo{}, pipeline.Unavailablef(err, "the profile lookup failed")
}
