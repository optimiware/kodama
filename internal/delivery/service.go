package delivery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Config holds the consumer-owned collaborators of a Service. Store, Access and
// Credentials are required. The runtime collaborators are required by the
// operations that use them: without Authority no runtime callback is served;
// without Contexts every protected callback is denied (a missing pin never falls
// back to the environment's current settings); without Producers and Artifacts
// no deployment can be registered; without Cleanups no cleanup callback is
// authorized.
type Config struct {
	Store       Store
	Access      Access
	Credentials Credentials
	// Targets are the installation's target profiles. Nil selects the builtin
	// Kubernetes profile only; an environment naming another profile is refused.
	Targets *TargetRegistry

	Contexts  PinnedContexts
	Producers Producers
	Artifacts Artifacts
	Authority Authority
	Cleanups  CleanupBindings

	IDs   IDs
	Clock func() time.Time
	// DefaultPreviewTTL is the preview TTL of a preview environment created
	// without one (O-11: 24 hours unless configured).
	DefaultPreviewTTL time.Duration
	// Annotations are the reconciliation annotation names published in
	// receipts. The zero value selects DefaultAnnotations.
	Annotations AnnotationNames
	// Logger receives operation names, safe identifiers and classified
	// outcomes only.
	Logger *slog.Logger
}

// Service is the delivery use-case layer. It owns environment, snapshot and
// deployment transitions; peers are reached only through the ports.
type Service struct {
	cfg Config
	now func() time.Time
	log *slog.Logger
}

// NewService validates the configuration.
func NewService(cfg Config) (*Service, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("delivery: a store is required")
	case cfg.Access == nil:
		return nil, errors.New("delivery: an access authority is required (no allow-all fallback)")
	case cfg.Credentials == nil:
		return nil, errors.New("delivery: a credential resolver is required")
	}
	if cfg.IDs == nil {
		cfg.IDs = RandomIDs{}
	}
	if cfg.DefaultPreviewTTL == 0 {
		cfg.DefaultPreviewTTL = DefaultPreviewTTL
	}
	if cfg.DefaultPreviewTTL < MinPreviewTTL || cfg.DefaultPreviewTTL > MaxPreviewTTL || cfg.DefaultPreviewTTL%time.Second != 0 {
		return nil, errors.New("delivery: the default preview TTL is out of range")
	}
	if cfg.Annotations == (AnnotationNames{}) {
		cfg.Annotations = DefaultAnnotations
	}
	if err := cfg.Annotations.Validate(); err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, now: cfg.Clock, log: cfg.Logger}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return s, nil
}

// Annotations returns the configured reconciliation annotation names.
func (s *Service) Annotations() AnnotationNames { return s.cfg.Annotations }

// Receipt is the durable answer to a registration or outcome: the deployment's
// identity and state, and the annotation values a script writes on its objects.
type Receipt struct {
	Deployment  Deployment
	Annotations map[string]string
	// Created is true only for the call that created the record.
	Created bool
}

func (s *Service) receipt(d Deployment, created bool) Receipt {
	return Receipt{Deployment: d, Annotations: d.Annotations(s.cfg.Annotations), Created: created}
}

func (s *Service) mutation(c Caller, reason string) (Mutation, error) {
	m := Mutation{Actor: c.Actor, RequestID: c.RequestID, Reason: reason, Now: s.now().UTC().Truncate(time.Microsecond)}
	return m, m.Validate()
}

// authorize checks a human caller's current permission. A trusted system actor
// (a scheduler built by the composition root, never by a transport) skips the
// role check.
func (s *Service) authorize(ctx context.Context, c Caller, scope domain.Scope, perm Permission) error {
	if err := scope.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := c.Actor.Validate(); err != nil {
		return Fail(ErrDenied, err, "the caller cannot be attributed")
	}
	if c.Actor.Type == ActorSystem {
		return nil
	}
	if c.Actor.Type != ActorUser {
		return Fail(ErrDenied, nil, "only users hold roles")
	}
	if err := s.cfg.Access.Authorize(ctx, c.Actor, scope, perm); err != nil {
		switch {
		case errors.Is(err, ErrDenied), errors.Is(err, ErrNotFound):
			return err
		}
		return Fail(ErrUnavailable, err, "the authority could not decide")
	}
	return nil
}

// runtimeCall is a verified runtime callback: the principal, its bound context
// and its immutable pinned context.
type runtimeCall struct {
	ctx    context.Context
	p      Principal
	pinned runtimev1.Context
	// snapshot is the retained configuration the pinned context names, already
	// checked against the context's credential bindings.
	snapshot ConfigurationSnapshot
}

// beginRuntime authenticates deployment.call for the route's attempt, binds the
// session for the transaction-guarded store and reads the attempt's pinned
// context. Everything is checked against the pin; the environment's current
// settings are never consulted.
func (s *Service) beginRuntime(ctx context.Context, attempt domain.AttemptID, bearer string) (runtimeCall, error) {
	if s.cfg.Authority == nil {
		return runtimeCall{}, Fail(ErrNotConfigured, nil, "runtime deployment calls are not configured")
	}
	if err := attempt.Validate(); err != nil {
		return runtimeCall{}, Fail(ErrUnauthenticated, err, "the token is not valid for this attempt")
	}
	p, err := s.cfg.Authority.Authenticate(ctx, attempt, bearer)
	if err != nil {
		return runtimeCall{}, err
	}
	// The grant, not the request, names the attempt.
	if p.Attempt != attempt || p.Scope.Validate() != nil || p.Run.Validate() != nil {
		return runtimeCall{}, Fail(ErrUnauthenticated, nil, "the token is not valid for this attempt")
	}
	if s.cfg.Contexts == nil {
		return runtimeCall{}, Fail(ErrPinRequired, nil, "no pinned-context reader is configured")
	}
	if ctx, err = s.cfg.Authority.Bind(ctx, p); err != nil {
		return runtimeCall{}, err
	}
	pinned, err := s.cfg.Contexts.Pinned(ctx, p.Scope, p.Attempt)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return runtimeCall{}, Fail(ErrPinRequired, err, "the attempt has no pinned context")
		}
		return runtimeCall{}, err
	}
	if err := CheckPinned(pinned, p); err != nil {
		return runtimeCall{}, err
	}
	snap, err := s.retainedPin(ctx, pinned)
	if err != nil {
		return runtimeCall{}, err
	}
	return runtimeCall{ctx: ctx, p: p, pinned: pinned, snapshot: snap}, nil
}

// retainedPin reads the retained configuration snapshot the context pins and
// verifies the context carries exactly its credential bindings. An absent,
// foreign, invalid or mismatched snapshot is ErrPinRequired: protected callbacks
// work only from the immutable pin, never from the environment's current
// settings or current credential versions, so there is no fallback to resolve.
func (s *Service) retainedPin(ctx context.Context, pinned runtimev1.Context) (ConfigurationSnapshot, error) {
	scope := pinned.Scope()
	snap, err := s.cfg.Store.GetSnapshot(ctx, scope, pinned.Environment.ID, pinned.Environment.ConfigDigest)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ConfigurationSnapshot{}, Fail(ErrPinRequired, err, "the pinned configuration is not retained")
		}
		return ConfigurationSnapshot{}, err
	}
	if err := snap.Validate(); err != nil {
		return ConfigurationSnapshot{}, Fail(ErrPinRequired, err, "the retained configuration is not valid")
	}
	if err := CheckEnvironment(pinned, snap); err != nil {
		return ConfigurationSnapshot{}, err
	}
	return snap, nil
}

func (c runtimeCall) mutation(now time.Time, reason string) Mutation {
	return Mutation{Actor: Actor{Type: ActorWorkload, ID: string(c.p.Attempt)}, RequestID: "", Reason: reason, Now: now.UTC().Truncate(time.Microsecond), Runtime: true}
}

func (s *Service) logOp(ctx context.Context, op string, err error, attrs ...slog.Attr) {
	level, outcome := slog.LevelInfo, "ok"
	if err != nil {
		outcome = "denied"
		if errors.Is(err, ErrUnavailable) {
			level, outcome = slog.LevelWarn, "unavailable"
		}
	}
	s.log.LogAttrs(ctx, level, "delivery", append([]slog.Attr{slog.String("op", op), slog.String("outcome", outcome)}, attrs...)...)
}
