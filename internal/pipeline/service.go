package pipeline

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

// Format is the syntax of a submitted document.
type Format string

const (
	FormatYAML Format = "yaml"
	FormatJSON Format = "json"
)

// Caller is the verified identity of a request as the transport saw it. The
// principal is never read from request content.
type Caller struct {
	PrincipalID string
	RequestID   string
}

func (c Caller) validate() error {
	if c.PrincipalID == "" || len(c.PrincipalID) > 512 {
		return fmt.Errorf("%w: principal must be 1..512 bytes", domain.ErrInvalidValue)
	}
	if len(c.RequestID) > 256 {
		return fmt.Errorf("%w: request id must be at most 256 bytes", domain.ErrInvalidValue)
	}
	return nil
}

// ServiceConfig wires a Service. Repository, Authority and Validator are
// required: there is no default that grants access or skips validation.
type ServiceConfig struct {
	Repository Repository
	Authority  Authority
	Validator  *Validator
	Clock      func() time.Time
	NewID      func() (string, error)
}

// Service holds the pipeline-definition use cases. It authorizes the scope
// before any parsing or persistence, validates through the shared Validator
// outside any database transaction, and delegates the atomic, content-addressed
// write to the Repository.
type Service struct {
	repo      Repository
	authority Authority
	validator *Validator
	now       func() time.Time
	newID     func() (string, error)
}

// NewService validates cfg.
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Repository == nil || cfg.Authority == nil || cfg.Validator == nil {
		return nil, errors.New("pipeline: a repository, an authority and a validator are required")
	}
	s := &Service{repo: cfg.Repository, authority: cfg.Authority, validator: cfg.Validator, now: cfg.Clock, newID: cfg.NewID}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.newID == nil {
		s.newID = randomDefinitionID
	}
	return s, nil
}

func randomDefinitionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "pd_" + hex.EncodeToString(b[:]), nil
}

// authorize resolves the caller and requires the permission, before any
// persistence or parsing.
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

// RegisterCommand submits one document.
type RegisterCommand struct {
	Scope  domain.Scope
	Caller Caller
	Format Format
	Body   []byte
}

// RegisterResult is the outcome of a registration.
type RegisterResult struct {
	Definition Definition
	// Created is true when this call created a new version and false when the
	// identical content was already registered.
	Created bool
}

// Register validates the document and registers it as an immutable version, or
// returns the existing version of identical content. Authorization precedes
// every other step, so deduplication discloses nothing to an unauthorized
// caller.
func (s *Service) Register(ctx context.Context, cmd RegisterCommand) (RegisterResult, error) {
	actor, err := s.authorize(ctx, cmd.Scope, cmd.Caller, PermRegister)
	if err != nil {
		return RegisterResult{}, err
	}
	var doc Document
	switch cmd.Format {
	case FormatYAML:
		doc, err = s.validator.ParseYAML(cmd.Body)
	case FormatJSON:
		doc, err = s.validator.ParseJSON(cmd.Body)
	default:
		return RegisterResult{}, fmt.Errorf("%w: unsupported document format", ErrInvalid)
	}
	if err != nil {
		return RegisterResult{}, err
	}
	content, err := s.validator.Validate(ctx, cmd.Scope, actor, doc)
	if err != nil {
		return RegisterResult{}, err
	}
	id, err := s.newID()
	if err != nil {
		return RegisterResult{}, Unavailablef(err, "identifier generation failed")
	}
	cand := Candidate{
		Scope: cmd.Scope, ID: domain.DefinitionID(id), Content: content, CreatedBy: actor,
		CreatedAt: s.now().UTC().Truncate(time.Microsecond), RequestID: cmd.Caller.RequestID,
	}
	if err := cand.Validate(); err != nil {
		return RegisterResult{}, Unavailablef(err, "the candidate definition is unusable")
	}
	def, created, err := s.repo.RegisterOrGet(ctx, cand)
	if err != nil {
		return RegisterResult{}, err
	}
	if err := def.Validate(); err != nil || def.Scope != cmd.Scope || def.Digest != content.Digest() {
		return RegisterResult{}, fmt.Errorf("%w: the repository returned a different definition", ErrConflict)
	}
	return RegisterResult{Definition: def, Created: created}, nil
}

// Get returns one definition of the scope.
func (s *Service) Get(ctx context.Context, scope domain.Scope, caller Caller, id domain.DefinitionID) (Definition, error) {
	if _, err := s.authorize(ctx, scope, caller, PermRead); err != nil {
		return Definition{}, err
	}
	if err := id.Validate(); err != nil {
		return Definition{}, fmt.Errorf("%w: the definition was not found", ErrNotFound)
	}
	def, err := s.repo.Get(ctx, scope, id)
	if err != nil {
		return Definition{}, err
	}
	if def.Scope != scope || def.ID != id {
		return Definition{}, fmt.Errorf("%w: the repository returned a foreign definition", ErrConflict)
	}
	return def, nil
}

// List returns up to q.Limit definitions of the scope in (created_at, id) order.
func (s *Service) List(ctx context.Context, scope domain.Scope, caller Caller, q ListQuery) ([]Definition, error) {
	if _, err := s.authorize(ctx, scope, caller, PermList); err != nil {
		return nil, err
	}
	if q.Limit < 1 || q.Limit > MaxPageRows {
		return nil, fmt.Errorf("%w: invalid page size", ErrInvalid)
	}
	if q.Name != "" && !nameRE.MatchString(q.Name) {
		return nil, fmt.Errorf("%w: the name filter is not a valid pipeline name", ErrInvalid)
	}
	defs, err := s.repo.List(ctx, scope, q)
	if err != nil {
		return nil, err
	}
	for _, d := range defs {
		if d.Scope != scope {
			return nil, fmt.Errorf("%w: the repository returned a foreign definition", ErrConflict)
		}
	}
	return defs, nil
}
