// Package execution is Kodama's durable run orchestration: pinned run
// admission, DAG progression, transactional dispatch with tenant/project
// capacity and the single workspace writer slot, observation, bounded retry,
// fail-fast draining, cancellation, validated result commit and
// reconciliation (docs/specs/pipeline-execution.md).
//
// The package holds entities (Run, StepRun, Attempt and their Aggregate), pure
// planning, use cases and small consumer-owned ports. It imports neither pgx,
// net/http, Kubernetes nor Git: the PostgreSQL store (postgres/), HTTP (http/)
// and peer-module bridges (adapters/) are adapters wired by the composition
// root. PostgreSQL owns desired state; the executor only acts on committed
// intent, outside any transaction.
package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/optimiware/kodama/internal/diag"
	"github.com/optimiware/kodama/internal/domain"
)

// Options are explicit, validated settings of a Service. There are no hidden
// defaults for policy; the zero value selects only mechanical batch sizes.
type Options struct {
	// ClaimLease is how long a dispatch intent stays claimed by one driver
	// before another may redrive it with the same identity.
	ClaimLease time.Duration
	// Batch bounds each scan page.
	Batch  int
	Logger *slog.Logger
	// TerminationDelay is how long after an accepted result is committed its workload is stopped, so that
	// the entrypoint still waiting for the acknowledgment receives it first (default 2s; negative stops at
	// once, as a late or refused result always is).
	TerminationDelay time.Duration
	// ResourceFreshness is how old a resource observation may be for the
	// dispatch barrier to trust it; ResourceRecheck is the age after which
	// dispatch observes again before claiming. ResourceBackoffBase/Max bound the
	// restart-safe retry of Ensure, Stop and Remove.
	ResourceFreshness   time.Duration
	ResourceRecheck     time.Duration
	ResourceBackoffBase time.Duration
	ResourceBackoffMax  time.Duration
}

const defaultTerminationDelay = 2 * time.Second

func (o Options) withDefaults() (Options, error) {
	if o.ClaimLease == 0 {
		o.ClaimLease = 30 * time.Second
	}
	if o.Batch == 0 {
		o.Batch = 50
	}
	if o.TerminationDelay == 0 {
		o.TerminationDelay = defaultTerminationDelay
	}
	if o.ResourceFreshness == 0 {
		o.ResourceFreshness = 30 * time.Second
	}
	if o.ResourceRecheck == 0 {
		o.ResourceRecheck = 10 * time.Second
	}
	if o.ResourceBackoffBase == 0 {
		o.ResourceBackoffBase = 2 * time.Second
	}
	if o.ResourceBackoffMax == 0 {
		o.ResourceBackoffMax = 5 * time.Minute
	}
	if o.ClaimLease < time.Second || o.Batch < 1 || o.Batch > 1000 {
		return o, invalidf("claim lease and batch are out of range")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return o, nil
}

// Service implements the execution use cases.
type Service struct {
	d            Dependencies
	o            Options
	now          func() time.Time
	environments EnvironmentResolver
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
	case d.Definitions == nil:
		return nil, missing("definitions")
	case d.Capabilities == nil:
		return nil, missing("capabilities")
	case d.Work == nil:
		return nil, missing("work content")
	case d.Configuration == nil:
		return nil, missing("configuration")
	case d.Sources == nil:
		return nil, missing("sources")
	case d.Limits == nil:
		return nil, missing("limits")
	case d.Hooks == nil:
		return nil, missing("hooks")
	}
	if err := d.Shared.validate(d.Runtime); err != nil {
		return nil, err
	}
	o, err := o.withDefaults()
	if err != nil {
		return nil, err
	}
	if d.Planner == nil {
		d.Planner = DefaultPlanner{}
	}
	s := &Service{d: d, o: o, now: d.Clock}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if d.NewID == nil {
		s.d.NewID = newID
	}
	return s, nil
}

func newID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
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
	case errors.Is(err, ErrHidden):
		return ErrHidden
	case errors.Is(err, ErrDenied):
		return ErrDenied
	case errors.Is(err, ErrNotFound):
		return ErrHidden
	}
	return Unavailablef(err, "the authority is unavailable")
}

// Authorize checks run:read or run:manage for a caller without loading
// anything; transports use it for list endpoints.
func (s *Service) Authorize(ctx context.Context, c Caller, scope domain.Scope, perm Permission) error {
	_, err := s.authorize(ctx, c, scope, perm)
	return err
}

func (s *Service) log(ctx context.Context, op string, err error, attrs ...any) {
	if err == nil {
		return
	}
	head := []slog.Attr{slog.String("error", errClass(err))}
	if detail := diag.Describe(err); detail != "" {
		head = append(head, slog.String("detail", detail))
	}
	s.o.Logger.LogAttrs(ctx, slog.LevelWarn, "execution: "+op, append(head, toAttrs(attrs)...)...)
}

func toAttrs(kv []any) []slog.Attr {
	var out []slog.Attr
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			out = append(out, slog.Any(k, kv[i+1]))
		}
	}
	return out
}

// errClass renders only the failure class, never its message: messages may
// wrap peer errors.
func errClass(err error) string {
	for _, c := range []struct {
		e error
		n string
	}{{ErrQuotaExceeded, "quota"}, {ErrBusy, "busy"}, {ErrStale, "stale"}, {ErrDenied, "denied"}, {ErrHidden, "hidden"},
		{ErrNotFound, "not_found"}, {ErrConflict, "conflict"}, {ErrUnavailable, "unavailable"}, {ErrAttemptRevoked, "revoked"},
		{ErrOutcomeUnknown, "outcome_unknown"}} {
		if errors.Is(err, c.e) {
			return c.n
		}
	}
	return "error"
}
