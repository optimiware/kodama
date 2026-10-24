// Package agents implements agent profiles: immutable, versioned defaults that
// select a harness image, wrapper, instructions, approved model, tool allow-list
// and limits for an agent step (docs/specs/agent-runtime.md). An agent step is a
// container step resolved from one exact profile version; it uses the ordinary
// attempt, entrypoint, publication, result and cancellation machinery. There is
// no agent-session state machine, continuation or scheduler here.
//
// The package holds the profile entity, pure expansion into a container command
// and a canonical wrapper payload, use cases and small consumer-owned ports. It
// imports neither pgx, net/http nor the execution module: the PostgreSQL store,
// HTTP handlers and execution/pipeline bridges are adapters in sub-packages,
// wired by the composition root.
package agents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// MaxPageRows bounds the rows one list read may request (page size + 1).
const MaxPageRows = 1001

// ServiceConfig wires a Service. Repository and Authority are required, and a
// missing Compatibility or Approvals port makes registration and resolution
// fail closed: there is no default that grants support or approval.
type ServiceConfig struct {
	Repository    Repository
	Authority     Authority
	Compatibility Compatibility
	Approvals     Approvals
	Clock         func() time.Time
	NewID         func() (string, error)
}

// Service holds the profile use cases. It authorizes the scope before any
// parsing or persistence, qualifies the wrapper and checks approvals outside any
// database transaction, and delegates the atomic, content-addressed write to
// the Repository.
type Service struct {
	repo      Repository
	authority Authority
	compat    Compatibility
	approvals Approvals
	now       func() time.Time
	newID     func() (string, error)
}

// NewService validates cfg.
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Repository == nil || cfg.Authority == nil {
		return nil, errors.New("agents: a repository and an authority are required")
	}
	s := &Service{repo: cfg.Repository, authority: cfg.Authority, compat: cfg.Compatibility, approvals: cfg.Approvals, now: cfg.Clock, newID: cfg.NewID}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.newID == nil {
		s.newID = randomProfileID
	}
	return s, nil
}

func randomProfileID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ap_" + hex.EncodeToString(b[:]), nil
}

func (s *Service) authorize(ctx context.Context, scope domain.Scope, caller Caller, perm Permission) (Actor, error) {
	if err := scope.Validate(); err != nil {
		return Actor{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := caller.validate(); err != nil {
		return Actor{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	actor, err := s.authority.ResolveActor(ctx, scope, caller.PrincipalID)
	if err != nil {
		return Actor{}, authorityError(err, "actor")
	}
	if err := actor.Validate(); err != nil {
		return Actor{}, Unavailablef(err, "the authority returned an unusable actor")
	}
	if err := s.authority.Authorize(ctx, actor, scope, perm); err != nil {
		return Actor{}, authorityError(err, "permission")
	}
	return actor, nil
}

func authorityError(err error, what string) error {
	if errors.Is(err, ErrDenied) || errors.Is(err, ErrHidden) {
		return err
	}
	return Unavailablef(err, "%s authority failed", what)
}

// qualify requires compatibility evidence for the tuple and for every control
// the content claims.
func (s *Service) qualify(ctx context.Context, c Content) (Qualification, error) {
	if s.compat == nil {
		return Qualification{}, Unavailablef(nil, "no compatibility catalogue is configured")
	}
	d := c.draft
	q, err := s.compat.Qualify(ctx, Tuple{
		Image: d.Image, WrapperID: d.Wrapper.ID, WrapperVersion: d.Wrapper.Version, Command: d.Wrapper.Command,
		HarnessID: d.Wrapper.Harness.ID, HarnessVersion: d.Wrapper.Harness.Version,
	})
	switch {
	case err == nil:
	case errors.Is(err, ErrUnsupported):
		return Qualification{}, Unsupportedf("the wrapper, harness and image combination has no compatibility evidence")
	default:
		return Qualification{}, Unavailablef(err, "compatibility lookup failed")
	}
	if q.Evidence == "" {
		return Qualification{}, Unsupportedf("the combination has no compatibility evidence")
	}
	for _, claim := range c.Claims() {
		if !q.Supports(claim) {
			return Qualification{}, Unsupportedf("the control %s is not enforced for this combination", claim)
		}
	}
	return q, nil
}

// approve checks the model and tool references against the approved-reference
// policy of the scope.
func (s *Service) approve(ctx context.Context, scope domain.Scope, c Content) error {
	if s.approvals == nil {
		return Unavailablef(nil, "no approval policy is configured")
	}
	d := c.draft
	var dg diagnostics
	switch err := s.approvals.ApproveModel(ctx, scope, d.Model); {
	case err == nil:
	case errors.Is(err, ErrDenied), errors.Is(err, ErrHidden):
		dg.add(CodeUnapprovedModel, "model", "the model configuration is not approved for this project")
	default:
		return Unavailablef(err, "model approval lookup failed")
	}
	if len(d.Tools) > 0 {
		switch err := s.approvals.ApproveTools(ctx, scope, d.Tools); {
		case err == nil:
		case errors.Is(err, ErrDenied), errors.Is(err, ErrHidden):
			dg.add(CodeUnapprovedTool, "tools", "a listed tool is not approved for this project")
		default:
			return Unavailablef(err, "tool approval lookup failed")
		}
	}
	return dg.err()
}

// RegisterCommand submits one draft.
type RegisterCommand struct {
	Scope  domain.Scope
	Caller Caller
	Draft  Draft
}

// RegisterResult is the outcome of a registration.
type RegisterResult struct {
	Profile ProfileVersion
	// Created is true when this call created a new version and false when the
	// identical content was already registered.
	Created bool
}

// Register validates the draft and registers it as an immutable version, or
// returns the existing version of identical content. Authorization precedes
// every other step, so deduplication discloses nothing to an unauthorized
// caller. An unqualified wrapper, an unsupported claimed control or an
// unapproved reference rejects the registration before anything is written.
func (s *Service) Register(ctx context.Context, cmd RegisterCommand) (RegisterResult, error) {
	actor, err := s.authorize(ctx, cmd.Scope, cmd.Caller, PermRegister)
	if err != nil {
		return RegisterResult{}, err
	}
	content, err := NewContent(cmd.Draft)
	if err != nil {
		return RegisterResult{}, err
	}
	if _, err := s.qualify(ctx, content); err != nil {
		return RegisterResult{}, err
	}
	if err := s.approve(ctx, cmd.Scope, content); err != nil {
		return RegisterResult{}, err
	}
	id, err := s.newID()
	if err != nil {
		return RegisterResult{}, Unavailablef(err, "identifier generation failed")
	}
	cand := Candidate{
		Scope: cmd.Scope, ID: ProfileID(id), Content: content, CreatedBy: actor,
		CreatedAt: s.now().UTC().Truncate(time.Microsecond), RequestID: cmd.Caller.RequestID,
	}
	if err := cand.Validate(); err != nil {
		return RegisterResult{}, Unavailablef(err, "the candidate profile is unusable")
	}
	p, created, err := s.repo.RegisterOrGet(ctx, cand)
	if err != nil {
		return RegisterResult{}, err
	}
	if err := p.Validate(); err != nil || p.Scope != cmd.Scope || p.Digest != content.Digest() {
		return RegisterResult{}, fmt.Errorf("%w: the repository returned a different profile", ErrConflict)
	}
	return RegisterResult{Profile: p, Created: created}, nil
}

// List returns up to q.Limit versions of the scope in (created_at, id) order.
func (s *Service) List(ctx context.Context, scope domain.Scope, caller Caller, q ListQuery) ([]ProfileVersion, error) {
	if _, err := s.authorize(ctx, scope, caller, PermList); err != nil {
		return nil, err
	}
	if q.Limit < 1 || q.Limit > MaxPageRows {
		return nil, fmt.Errorf("%w: invalid page size", ErrInvalid)
	}
	if q.Name != "" && !nameRE.MatchString(q.Name) {
		return nil, fmt.Errorf("%w: the name filter is not a valid profile name", ErrInvalid)
	}
	out, err := s.repo.List(ctx, scope, q)
	if err != nil {
		return nil, err
	}
	for _, p := range out {
		if p.Scope != scope {
			return nil, fmt.Errorf("%w: the repository returned a foreign profile", ErrConflict)
		}
	}
	return out, nil
}

// Stored reads one exact version by its immutable reference without any
// capability judgement. It is scoped: a foreign, unknown or moving reference is
// ErrNotFound or ErrInvalid. It is an internal read for adapters, not an API.
func (s *Service) Stored(ctx context.Context, scope domain.Scope, ref string) (ProfileVersion, error) {
	if err := scope.Validate(); err != nil {
		return ProfileVersion{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	name, version, err := ParseReference(ref)
	if err != nil {
		return ProfileVersion{}, err
	}
	p, err := s.repo.Get(ctx, scope, name, version)
	if err != nil {
		return ProfileVersion{}, err
	}
	if p.Scope != scope || p.Name != name || p.Version != version {
		return ProfileVersion{}, fmt.Errorf("%w: the repository returned a different profile", ErrConflict)
	}
	return p, nil
}

// Resolved is a stored version that currently meets its compatibility contract.
type Resolved struct {
	Profile       ProfileVersion
	Qualification Qualification
}

// Resolve reads one exact stored version and requires that it still meets the
// compatibility and approval contract now: a wrapper dropped from the catalogue
// or a withdrawn approval fails closed for new runs without changing the stored
// profile. It performs no write.
func (s *Service) Resolve(ctx context.Context, scope domain.Scope, ref string) (Resolved, error) {
	p, err := s.Stored(ctx, scope, ref)
	if err != nil {
		return Resolved{}, err
	}
	q, err := s.qualify(ctx, p.Content)
	if err != nil {
		return Resolved{}, err
	}
	if err := s.approve(ctx, scope, p.Content); err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			return Resolved{}, Unsupportedf("the profile's model or tools are no longer approved")
		}
		return Resolved{}, err
	}
	return Resolved{Profile: p, Qualification: q}, nil
}
