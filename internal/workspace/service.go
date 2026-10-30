package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Dependencies are the injected ports. Every required one must be non-nil:
// there is no default that grants access or reaches a repository.
type Dependencies struct {
	Store        Store
	Tenants      TenantSource
	Authorizer   Authorizer
	WorkItems    WorkItems
	Repositories Repositories
	Attempts     Attempts
	Bundles      Bundles
	Secrets      SecretNeedles
	Refs         RefReader
	Source       SourcePreparer
	Publisher    RefPublisher

	// Deletion and Rotation are optional: without them removal is always
	// denied and publication resumption is refused.
	Deletion DeletionPolicy
	Rotation PublisherRotation

	// The internally hosted model (ADR 0011). Each is optional and independent
	// of the external publisher path: a use case that needs a missing one fails
	// closed with ErrNotConfigured and never falls back to another model.
	// Hosts creates task branches and retained refs; InternalRepos resolves the
	// project's repositories; Pushes reloads push records; Gate reports unknown
	// push outcomes; RefPushes reads the push records that touched one ref
	// (in-flight pushes, applied entries); Inspector reads a pushed commit's
	// content server-side.
	Hosts         Hosts
	InternalRepos InternalRepositories
	Pushes        PushRecords
	Gate          WriterGate
	RefPushes     RefPushes
	Inspector     PushInspector

	Clock  func() time.Time
	NewID  func(prefix string) string
	Logger *slog.Logger
}

// Options are the explicit policies of a Service. Every duration and size is
// required to be positive; nothing has a hidden default except the retained
// reference prefix.
type Options struct {
	// GitDeadline bounds each remote Git interaction made by a use case.
	GitDeadline time.Duration
	// ArchiveWait bounds how long Archive waits for settlement.
	ArchiveWait time.Duration
	// ArchivePoll is the interval between settlement checks while waiting.
	ArchivePoll time.Duration
	// DriverStaleAfter is how long a publisher claim blocks another driver
	// from resuming the same publication.
	DriverStaleAfter time.Duration
	// RedriveAfter is how long after a push attempt an uncertain publication
	// may be pushed again (same intent, same expected-old-ref CAS).
	RedriveAfter time.Duration
	// ReconcileBatch bounds the workspaces handled per tenant per pass.
	ReconcileBatch int
	// RetainedRefPrefix is the ordinary-branch namespace of retained refs.
	RetainedRefPrefix string
}

func (o Options) validate() (Options, error) {
	if o.RetainedRefPrefix == "" {
		o.RetainedRefPrefix = DefaultRetainedRefPrefix
	}
	switch {
	case o.GitDeadline <= 0, o.ArchiveWait <= 0, o.ArchivePoll <= 0, o.DriverStaleAfter <= 0, o.RedriveAfter <= 0, o.ReconcileBatch < 1:
		return o, fmt.Errorf("%w: durations and batch sizes must be positive", ErrInvalidConfiguration)
	case len(o.RetainedRefPrefix) < 12 || o.RetainedRefPrefix[:11] != "refs/heads/" || o.RetainedRefPrefix[len(o.RetainedRefPrefix)-1] != '/':
		return o, fmt.Errorf("%w: retained ref prefix", ErrInvalidConfiguration)
	}
	return o, nil
}

// Service coordinates the workspace entities through the injected ports.
type Service struct {
	d Dependencies
	o Options
}

// NewService validates the capabilities and options.
func NewService(d Dependencies, o Options) (*Service, error) {
	missing := func(name string) error { return fmt.Errorf("%w: %s", ErrNotConfigured, name) }
	switch {
	case d.Store == nil:
		return nil, missing("Store")
	case d.Tenants == nil:
		return nil, missing("Tenants")
	case d.Authorizer == nil:
		return nil, missing("Authorizer")
	case d.WorkItems == nil:
		return nil, missing("WorkItems")
	case d.Repositories == nil:
		return nil, missing("Repositories")
	case d.Attempts == nil:
		return nil, missing("Attempts")
	case d.Bundles == nil:
		return nil, missing("Bundles")
	case d.Secrets == nil:
		return nil, missing("Secrets")
	case d.Refs == nil:
		return nil, missing("Refs")
	case d.Source == nil:
		return nil, missing("Source")
	case d.Publisher == nil:
		return nil, missing("Publisher")
	}
	o, err := o.validate()
	if err != nil {
		return nil, err
	}
	if d.Clock == nil {
		d.Clock = func() time.Time { return time.Now().UTC() }
	}
	if d.NewID == nil {
		d.NewID = randomID
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &Service{d: d, o: o}, nil
}

func randomID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// system is the actor of background reconciliation.
var system = Actor{Type: ActorService, ID: "workspace-reconciler"}

func (s *Service) now() time.Time { return s.d.Clock().UTC() }

func (s *Service) repoRef(w *Workspace) RepoRef {
	return RepoRef{Scope: w.Scope, URL: w.RepositoryURL, CheckoutSecretRef: w.CheckoutSecretRef, PublisherSecretRef: w.PublisherSecretRef}
}

func (s *Service) authorize(ctx context.Context, actor Actor, action Action, scope domain.Scope, res Resource) error {
	if err := actor.Validate(); err != nil {
		return err
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	err := s.d.Authorizer.Authorize(ctx, actor, action, scope, res)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrDenied), errors.Is(err, ErrNotFound):
		return err
	default:
		return unavailable("authorization", err)
	}
}

// gitCtx bounds one remote interaction.
func (s *Service) gitCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.o.GitDeadline)
}

func stateEvent(actor Actor, from, to, reason string) Event {
	return Event{Kind: EventStateChanged, Actor: actor, From: from, To: to, Reason: reason}
}

func audit(actor Actor, action, reason string, md map[string]any) Audit {
	return Audit{Action: action, Actor: actor, Reason: reason, Metadata: md}
}

// retry runs fn up to n times while it fails with ErrStale.
func retry(n int, fn func() error) error {
	var err error
	for range n {
		if err = fn(); !errors.Is(err, ErrStale) {
			return err
		}
	}
	return err
}

// Get returns the workspace after an authorization check.
func (s *Service) Get(ctx context.Context, actor Actor, scope domain.Scope, id ID) (*Workspace, error) {
	if err := id.Validate(); err != nil {
		return nil, ErrNotFound
	}
	if err := s.authorize(ctx, actor, ActionRead, scope, Resource{"workspace", string(id)}); err != nil {
		return nil, err
	}
	return s.d.Store.Get(ctx, scope, id)
}

// Recovery is what a failed attempt's recovery reports: the last published
// commit, and that uncommitted work may be lost.
type Recovery struct {
	LastPublishedCommit      Commit
	UncommittedWorkMayBeLost bool
}

// RecoveryPoint returns the commit recovery starts from (WS-08).
func (s *Service) RecoveryPoint(ctx context.Context, actor Actor, scope domain.Scope, id ID) (Recovery, error) {
	w, err := s.Get(ctx, actor, scope, id)
	if err != nil {
		return Recovery{}, err
	}
	if err := w.requireExternal("a single recovery point (use InternalRecovery)"); err != nil {
		return Recovery{}, err
	}
	if w.AcceptedCommit == "" {
		return Recovery{}, fmt.Errorf("%w: workspace has no published commit", ErrInvalidTransition)
	}
	return Recovery{LastPublishedCommit: w.AcceptedCommit, UncommittedWorkMayBeLost: true}, nil
}
