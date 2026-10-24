// Package gates implements Kodama's canonical human gates: one attributable
// human decision about one identified subject (source commit, content
// revision, definition, evidence and deployment configuration), with the gate
// step waiting in durable records and consuming no compute
// (docs/specs/delivery-and-quality-gates.md, docs/specs/pipeline-execution.md).
//
// The package holds the gate entity and its subject, pure applicability rules,
// use cases and small consumer-owned ports. It imports neither pgx, net/http,
// S3 nor Git: the PostgreSQL store (postgres/), HTTP (http/) and peer-module
// bridges (adapters/) are adapters wired by the composition root. Execution
// owns the step and run transitions; this module owns the subject, the
// decision, applicability and the gate events.
package gates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Options are explicit, validated settings of a Service. The gate deadline has
// no default: no merged contract defines a gate duration, so the composition
// root must inject one (never an execution timeout).
type Options struct {
	// GateTTL is how long a gate waits for its decision, snapshotted once into
	// the gate's deadline. Required and positive.
	GateTTL time.Duration
	// MaxReportBytes bounds the conformance report read from the object store.
	MaxReportBytes int64
	// Batch bounds each reconciliation page.
	Batch  int
	Logger *slog.Logger
}

func (o Options) withDefaults() (Options, error) {
	if o.GateTTL <= 0 {
		return o, fmt.Errorf("%w: a positive gate deadline duration is required", ErrNotConfigured)
	}
	if o.MaxReportBytes == 0 {
		o.MaxReportBytes = 1 << 20
	}
	if o.Batch == 0 {
		o.Batch = 50
	}
	if o.MaxReportBytes < 1 || o.MaxReportBytes > 16<<20 || o.Batch < 1 || o.Batch > 1000 {
		return o, invalidf("report bound and batch are out of range")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return o, nil
}

// Dependencies are the collaborators of a Service. Required ports must be set;
// optional ones disable the behavior that needs them and never loosen a check:
// a gate that needs a missing optional port is not created.
type Dependencies struct {
	Store     Store
	Authority Authority
	Runs      RunReader
	Evidence  Evidence
	Pins      Pins
	// Candidates verifies the candidate commit against workspace history.
	Candidates Candidates
	Tenants    Tenants
	// Optional.
	Deployments Deployments
	Conformance Conformance
	Clock       func() time.Time
}

// Service implements the gate use cases.
type Service struct {
	d   Dependencies
	o   Options
	now func() time.Time
}

// NewService requires the ports whose absence would make a check unsafe; any
// missing one is ErrNotConfigured, never an allow-all default.
func NewService(d Dependencies, o Options) (*Service, error) {
	missing := func(name string) error { return fmt.Errorf("%w: %s is required", ErrNotConfigured, name) }
	switch {
	case d.Store == nil:
		return nil, missing("store")
	case d.Authority == nil:
		return nil, missing("authority")
	case d.Runs == nil:
		return nil, missing("run reader")
	case d.Evidence == nil:
		return nil, missing("evidence reader")
	case d.Pins == nil:
		return nil, missing("evidence pins")
	case d.Candidates == nil:
		return nil, missing("candidate verifier")
	}
	o, err := o.withDefaults()
	if err != nil {
		return nil, err
	}
	s := &Service{d: d, o: o, now: d.Clock}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

// authorize resolves the caller and checks a permission, hiding foreign scopes.
func (s *Service) authorize(ctx context.Context, c Caller, scope domain.Scope, perm Permission) (Actor, error) {
	if err := scope.Validate(); err != nil {
		return Actor{}, ErrNotFound
	}
	actor, err := s.d.Authority.ResolveActor(ctx, scope, c.PrincipalID)
	if err != nil {
		return Actor{}, classifyAuthority(err)
	}
	if err := actor.Validate(); err != nil {
		return Actor{}, ErrDenied
	}
	if err := s.d.Authority.Authorize(ctx, actor, scope, perm); err != nil {
		return Actor{}, classifyAuthority(err)
	}
	return actor, nil
}

func classifyAuthority(err error) error {
	switch {
	case errors.Is(err, ErrHidden), errors.Is(err, ErrNotFound):
		return ErrHidden
	case errors.Is(err, ErrDenied):
		return ErrDenied
	}
	return Unavailablef(err, "the authority is unavailable")
}

// Authorize checks a permission for a caller without loading anything; the
// keyed replay path uses it so a stored response is never released on a stale
// grant.
func (s *Service) Authorize(ctx context.Context, c Caller, scope domain.Scope, perm Permission) error {
	_, err := s.authorize(ctx, c, scope, perm)
	return err
}

// Get returns a gate the caller may read. A missing, foreign or hidden gate is
// indistinguishable.
func (s *Service) Get(ctx context.Context, c Caller, scope domain.Scope, id ID) (*Gate, error) {
	if _, err := s.authorize(ctx, c, scope, PermRead); err != nil {
		return nil, err
	}
	if err := id.Validate(); err != nil {
		return nil, ErrNotFound
	}
	g, err := s.d.Store.Get(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	return g, nil
}

// List pages the gates of a project.
func (s *Service) List(ctx context.Context, c Caller, scope domain.Scope, q ListQuery) ([]*Gate, error) {
	if _, err := s.authorize(ctx, c, scope, PermRead); err != nil {
		return nil, err
	}
	if q.Limit < 1 || q.Limit > 201 {
		return nil, invalidf("limit is out of range")
	}
	out, err := s.d.Store.List(ctx, scope, q)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PrepareDecision derives the human actor from the verified principal and
// requires the reviewer authority now. Admin, engineer, workload and service
// identities cannot substitute for it. The transaction that records the
// decision checks the authority again, including after any lock wait.
func (s *Service) PrepareDecision(ctx context.Context, c Caller, scope domain.Scope, id ID, cmd DecideCommand) (*DecisionPlan, error) {
	if err := id.Validate(); err != nil {
		return nil, ErrNotFound
	}
	actor, err := s.authorize(ctx, c, scope, PermDecide)
	if err != nil {
		return nil, err
	}
	if actor.Type != ActorUser {
		return nil, ErrDenied
	}
	cmd.Actor = actor
	if err := cmd.Validate(); err != nil {
		return nil, err
	}
	return &DecisionPlan{Scope: scope, GateID: id, Command: cmd, RequestID: c.RequestID}, nil
}

// Decide records one decision. A definitive staleness found by the decision is
// recorded as an invalidation (without touching any earlier decision) and still
// refuses the request.
func (s *Service) Decide(ctx context.Context, c Caller, scope domain.Scope, id ID, cmd DecideCommand) (*Gate, error) {
	plan, err := s.PrepareDecision(ctx, c, scope, id, cmd)
	if err != nil {
		return nil, err
	}
	g, err := s.d.Store.Decide(ctx, plan)
	if err != nil {
		return nil, s.AfterRefusal(ctx, plan, err)
	}
	return g, nil
}

// AfterRefusal classifies a refused decision and records a definitive
// staleness. It returns the error the caller should report.
func (s *Service) AfterRefusal(ctx context.Context, plan *DecisionPlan, err error) error {
	var se *StaleError
	if errors.As(err, &se) && se.Definitive {
		if _, _, ierr := s.d.Store.Invalidate(context.WithoutCancel(ctx), plan.Scope, plan.GateID, se, plan.RequestID); ierr != nil {
			s.log(ctx, "invalidate", ierr, "gate", string(plan.GateID))
		}
	}
	return err
}

func (s *Service) log(ctx context.Context, op string, err error, attrs ...any) {
	if err == nil {
		return
	}
	a := []slog.Attr{slog.String("error", errClass(err))}
	for i := 0; i+1 < len(attrs); i += 2 {
		if k, ok := attrs[i].(string); ok {
			a = append(a, slog.Any(k, attrs[i+1]))
		}
	}
	s.o.Logger.LogAttrs(ctx, slog.LevelWarn, "gates: "+op, a...)
}

// errClass renders only the failure class, never its message.
func errClass(err error) string {
	for _, c := range []struct {
		e error
		n string
	}{{ErrStaleSubject, "stale"}, {ErrDenied, "denied"}, {ErrHidden, "hidden"}, {ErrNotFound, "not_found"}, {ErrVersionConflict, "version_conflict"},
		{ErrConformance, "conformance"}, {ErrEvidence, "evidence"}, {ErrUnavailable, "unavailable"}, {ErrNotConfigured, "not_configured"},
		{ErrReviewerAuthority, "reviewer_authority"}} {
		if errors.Is(err, c.e) {
			return c.n
		}
	}
	return "error"
}
