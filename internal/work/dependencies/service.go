package dependencies

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/work"
)

// Config wires a Service. Repository and Authority are required: there is no
// default that grants access.
type Config struct {
	Repository Repository
	Authority  work.Authority
	Clock      func() time.Time
	NewID      func(prefix string) (string, error)
}

// Service holds the dependency use cases. It resolves the actor and authorizes
// before persistence; the repository re-evaluates the permission inside the
// mutating transaction.
type Service struct {
	repo      Repository
	authority work.Authority
	now       func() time.Time
	newID     func(string) (string, error)
}

// NewService validates cfg.
func NewService(cfg Config) (*Service, error) {
	if cfg.Repository == nil || cfg.Authority == nil {
		return nil, errors.New("dependencies: a repository and an authority are required")
	}
	s := &Service{repo: cfg.Repository, authority: cfg.Authority, now: cfg.Clock, newID: cfg.NewID}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.newID == nil {
		s.newID = work.RandomID
	}
	return s, nil
}

func (s *Service) authorize(ctx context.Context, scope domain.Scope, caller Caller, perm work.Permission) (work.Actor, error) {
	if err := scope.Validate(); err != nil {
		return work.Actor{}, fmt.Errorf("%w: %v", work.ErrInvalid, err)
	}
	if caller.PrincipalID == "" {
		return work.Actor{}, fmt.Errorf("%w: a verified caller is required", work.ErrDenied)
	}
	actor, err := s.authority.ResolveActor(ctx, scope, caller.PrincipalID)
	if err != nil {
		return work.Actor{}, authorityError(err, "actor")
	}
	if err := actor.Validate(); err != nil {
		return work.Actor{}, work.Unavailablef(err, "the authority returned an unusable actor")
	}
	if err := s.authority.Authorize(ctx, actor, scope, perm); err != nil {
		return work.Actor{}, authorityError(err, "permission")
	}
	return actor, nil
}

// authorityError keeps denial classes and turns every other authority failure
// into an outage: an unknown answer is never access.
func authorityError(err error, what string) error {
	if errors.Is(err, work.ErrDenied) || errors.Is(err, work.ErrHidden) {
		return err
	}
	return work.Unavailablef(err, "%s authority failed", what)
}

// List returns one page of the direct prerequisites of a work item. The caller
// needs work_item:read, and every page is authorized again.
func (s *Service) List(ctx context.Context, cmd ListCommand) (Page, error) {
	if _, err := s.authorize(ctx, cmd.Scope, cmd.Caller, PermList); err != nil {
		return Page{}, err
	}
	if err := cmd.Dependent.Validate(); err != nil {
		return Page{}, fmt.Errorf("%w: %v", work.ErrInvalid, err)
	}
	if cmd.After != "" {
		if err := cmd.After.Validate(); err != nil {
			return Page{}, fmt.Errorf("%w: %v", work.ErrInvalid, err)
		}
	}
	if cmd.Limit < 1 || cmd.Limit > MaxPageRows {
		return Page{}, fmt.Errorf("%w: limit must be between 1 and %d", work.ErrInvalid, MaxPageRows)
	}
	return s.repo.List(ctx, cmd.Scope, ListQuery{Dependent: cmd.Dependent, After: cmd.After, Limit: cmd.Limit})
}

// Add adds the link cmd.Dependent -> cmd.Prerequisite. Adding an existing link
// is a no-op that writes nothing.
func (s *Service) Add(ctx context.Context, cmd AddCommand) (Outcome, error) {
	return s.mutate(ctx, cmd, OpAdd)
}

// Remove removes the link. Removing an absent link is a no-op that writes
// nothing. Removing a link removes a prerequisite; it is the only way a
// cancelled prerequisite stops blocking its dependent.
func (s *Service) Remove(ctx context.Context, cmd RemoveCommand) (Outcome, error) {
	return s.mutate(ctx, cmd, OpRemove)
}

func (s *Service) mutate(ctx context.Context, cmd AddCommand, op Operation) (Outcome, error) {
	actor, err := s.authorize(ctx, cmd.Scope, cmd.Caller, PermMutate)
	if err != nil {
		return Outcome{}, err
	}
	link, err := NewLink(cmd.Scope, cmd.Dependent, cmd.Prerequisite)
	if err != nil {
		return Outcome{}, err
	}
	if err := cmd.Expected.Validate(); err != nil {
		return Outcome{}, fmt.Errorf("%w: %v", work.ErrInvalid, err)
	}
	hid, err := s.newID("wih_")
	if err != nil {
		return Outcome{}, work.Unavailablef(err, "identifier generation failed")
	}
	req := MutationRequest{Scope: cmd.Scope, Dependent: cmd.Dependent, Expected: cmd.Expected, Actor: actor, Permission: PermMutate}
	res, err := s.repo.Mutate(ctx, req, func(ctx context.Context, tx Tx, owner Owner) (Change, error) {
		return s.plan(ctx, tx, owner, link, op, actor, hid, cmd.Caller.RequestID)
	})
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Link: link, Changed: res.Changed, Owner: res.Owner}, nil
}

// plan gathers the graph facts the entity needs, cheapest and least revealing
// first: the owner's editability, then both endpoints in scope (so no graph
// diagnostic is ever produced for a foreign or missing reference), then the
// link, and only for an insertion of a new link the cycle and size checks.
func (s *Service) plan(ctx context.Context, tx Tx, owner Owner, l Link, op Operation, actor work.Actor, hid, requestID string) (Change, error) {
	if err := owner.CheckEditable(); err != nil {
		return Change{}, err
	}
	if err := tx.PrerequisiteExists(ctx, l.Scope, l.PrerequisiteID); err != nil {
		return Change{}, err
	}
	var f Facts
	var err error
	if f.LinkExists, err = tx.LinkExists(ctx, l); err != nil {
		return Change{}, err
	}
	now := s.now()
	if op == OpRemove {
		return owner.Remove(l, f, actor, now, hid, requestID)
	}
	if !f.LinkExists {
		if f.WouldCycle, err = tx.DependsOn(ctx, l.Scope, l.PrerequisiteID, l.DependentID); err != nil {
			return Change{}, err
		}
		if f.PrerequisiteCount, err = tx.PrerequisiteCount(ctx, l.Scope, l.DependentID); err != nil {
			return Change{}, err
		}
	}
	return owner.Add(l, f, actor, now, hid, requestID)
}
