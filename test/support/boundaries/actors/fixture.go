// Package actors provides test support for the Git actor-handle boundary.
//
// This is fixture code, not product code. The executor, observer, journal and
// actor-verifier stand in for real Shell/Kernel facts and are labelled as such.
// The real boundaries under test are git.SmartHTTP, git.Hook, git.Service and
// the identity contract verification fixture.
package actors

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
)

// Fixture identities. The tokens are opaque fixture values; the contract
// server fixture is the only component that authenticates them.
const (
	TenantID id.TenantID = "tenant-i03b"

	ProjectID = "project-i03b"

	ServiceID    = "service-i03b"
	ServiceToken = "kodsvc.i03b.fixture-token"

	ExecutionID    = "execution-i03b"
	ExecutionToken = "kodex.i03b.fixture-token"

	ResourceID = "res-i03b"
	Repository = "shop/web"
)

// DefaultTime is the deterministic fixture time.
func DefaultTime() time.Time {
	return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
}

// Clock is a deterministic, fixed time boundary.
type Clock struct {
	now time.Time
}

// NewClock returns a fixed clock.
func NewClock(now time.Time) *Clock { return &Clock{now: now} }

// Now returns the fixture time.
func (clock *Clock) Now() time.Time {
	if clock == nil {
		return time.Time{}
	}
	return clock.now
}

// Policy is the mutable git.conf boundary.
type Policy struct {
	mu     sync.Mutex
	config git.Config
}

// NewPolicy parses one git.conf fixture in the declared grammar.
func NewPolicy(body string) (*Policy, error) {
	config, err := git.ParseConfig([]byte(body))
	if err != nil {
		return nil, fmt.Errorf("parse fixture git.conf: %w", err)
	}
	return &Policy{config: config}, nil
}

// Current returns the fixture policy.
func (policy *Policy) Current(context.Context) (git.Config, error) {
	policy.mu.Lock()
	defer policy.mu.Unlock()
	return policy.config, nil
}

// Storage is an in-memory tenant storage path boundary. No directory is
// created; the fixture executor ignores the path.
type Storage struct {
	Root string
}

// RepositoryPath returns a deterministic tenant-scoped path.
func (storage Storage) RepositoryPath(tenant id.TenantID, repository string) (string, error) {
	if storage.Root == "" {
		return "", fmt.Errorf("fixture storage root is empty")
	}
	return storage.Root + "/" + string(tenant) + "/" + repository, nil
}

// EnsureRepository returns the same deterministic path.
func (storage Storage) EnsureRepository(tenant id.TenantID, repository string) (git.RepositoryLocation, error) {
	path, err := storage.RepositoryPath(tenant, repository)
	return git.RepositoryLocation{Path: path}, err
}

// Executor is the native Git command fixture. It records update-ref effects and
// treats every merge-base as an ancestor.
type Executor struct {
	mu   sync.Mutex
	refs map[string]string
}

// NewExecutor returns an empty fixture executor.
func NewExecutor() *Executor { return &Executor{refs: map[string]string{}} }

// Run implements git.CommandExecutor.
func (executor *Executor) Run(ctx context.Context, command git.Command) (git.CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return git.CommandResult{}, err
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	args := command.Args
	if command.Name != "git" || len(args) != 6 {
		return git.CommandResult{}, fmt.Errorf("fixture executor: unexpected command %+v", command)
	}
	switch args[2] {
	case "update-ref":
		executor.refs[args[3]] = args[4]
		return git.CommandResult{ExitCode: 0}, nil
	case "merge-base":
		return git.CommandResult{ExitCode: 0}, nil
	default:
		return git.CommandResult{}, fmt.Errorf("fixture executor: unexpected command %+v", command)
	}
}

// SetRef records one observed ref.
func (executor *Executor) SetRef(ref, commit string) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.refs[ref] = commit
}

// Ref returns the recorded commit for one ref.
func (executor *Executor) Ref(ref string) string {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.refs[ref]
}

// Observer is the ref observer fixture backed by the executor.
type Observer struct {
	Executor *Executor
}

// Observe implements git.RefObserver.
func (observer *Observer) Observe(_ context.Context, _ string, ref string) (git.RefState, error) {
	return git.RefState{Ref: ref, Commit: observer.Executor.Ref(ref)}, nil
}

// Journal is the in-memory durable-journal fixture.
type Journal struct {
	mu      sync.Mutex
	entries map[string]git.JournalEntry
}

// NewJournal returns an empty journal.
func NewJournal() *Journal { return &Journal{entries: map[string]git.JournalEntry{}} }

// Load implements git.JournalStore.
func (journal *Journal) Load(_ context.Context, key string) (git.JournalEntry, bool, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	entry, ok := journal.entries[key]
	return entry, ok, nil
}

// Record implements git.JournalStore.
func (journal *Journal) Record(_ context.Context, entry git.JournalEntry) error {
	if err := entry.Validate(); err != nil {
		return err
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.entries[entry.SourceEventID] = entry
	return nil
}

// Retention is the retention-query fixture. The Git service never calls it on
// the handle/verification path; it refuses rather than inventing references.
type Retention struct{}

// ReferencedCommits implements git.RetentionQuery.
func (Retention) ReferencedCommits(context.Context, api.Page) (git.CommitRefs, error) {
	return git.CommitRefs{}, errors.New("fixture retention is unavailable")
}

// ActorVerifier is the smart-HTTP actor-verification fixture. It stands in for
// the current-principal identity boundary; the hook's per-update verification
// still goes through the real public client and the identity contract server.
type ActorVerifier struct {
	mu         sync.Mutex
	identities map[string]git.Identity
}

// NewActorVerifier returns an empty fixture verifier.
func NewActorVerifier() *ActorVerifier {
	return &ActorVerifier{identities: map[string]git.Identity{}}
}

// Allow records one credential-to-identity fact.
func (verifier *ActorVerifier) Allow(credential string, identity git.Identity) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	verifier.identities[credential] = identity
}

// VerifyReadActor implements git.ReadActorVerifier.
func (verifier *ActorVerifier) VerifyReadActor(ctx context.Context, _, credential string) (git.Identity, error) {
	if err := ctx.Err(); err != nil {
		return git.Identity{}, err
	}
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	identity, ok := verifier.identities[credential]
	if !ok {
		return git.Identity{}, fault.New(fault.Unauthenticated, "fixture verifier: unknown actor credential")
	}
	return identity, nil
}

// Fixture is one real git.Service wired to deterministic fixtures.
type Fixture struct {
	TenantID id.TenantID
	Service  *git.Service
	Executor *Executor
	Observer *Observer
	Journal  *Journal
	Verifier *ActorVerifier
	Clock    git.Clock
}

// ServiceConfig is the fixture Service input.
type ServiceConfig struct {
	TenantID id.TenantID
	Policy   string
	Verify   git.VerificationClient
	Clock    git.Clock
}

// NewFixture builds one real git.Service over fixture boundaries.
func NewFixture(config ServiceConfig) (*Fixture, error) {
	tenant := config.TenantID
	if tenant == "" {
		tenant = TenantID
	}
	policy, err := NewPolicy(config.Policy)
	if err != nil {
		return nil, err
	}
	clock := config.Clock
	if clock == nil {
		clock = NewClock(DefaultTime())
	}
	executor := NewExecutor()
	observer := &Observer{Executor: executor}
	journal := NewJournal()
	verifier := NewActorVerifier()
	service, err := git.NewService(git.ServiceConfig{
		TenantID:  tenant,
		Storage:   Storage{Root: "/kodama-i03b-fixture"},
		Policies:  policy,
		Executor:  executor,
		Observer:  observer,
		Journal:   journal,
		Verify:    config.Verify,
		Retention: Retention{},
		Clock:     clock,
	})
	if err != nil {
		return nil, err
	}
	return &Fixture{
		TenantID: tenant,
		Service:  service,
		Executor: executor,
		Observer: observer,
		Journal:  journal,
		Verifier: verifier,
		Clock:    clock,
	}, nil
}

// Handler builds the real smart-HTTP handler with the fixture service, the
// injected handle issuer and the fixture actor verifier.
func (fixture *Fixture) Handler(backend git.GitBackend, handles git.ActorHandleIssuer, sourceEvent func() (string, error), resources map[string]string) (*git.SmartHTTP, error) {
	if sourceEvent == nil {
		sourceEvent = git.NewSourceEventID()
	}
	return git.NewSmartHTTP(git.SmartHTTPConfig{
		Service:      fixture.Service,
		Backend:      backend,
		Actors:       fixture.Verifier,
		ActorHandles: handles,
		ResourceID: func(repository string) (string, error) {
			resourceID, ok := resources[repository]
			if !ok || resourceID == "" {
				return "", fmt.Errorf("fixture has no resource identity for %q", repository)
			}
			return resourceID, nil
		},
		SourceEvent: sourceEvent,
	})
}

// Hook builds the real native hook with the fixture service and the injected
// actor source.
func (fixture *Fixture) Hook(source git.ActorSource, environment func(string) (string, bool)) (*git.Hook, error) {
	return git.NewHook(git.HookConfig{
		Service:     fixture.Service,
		Actors:      source,
		Environment: environment,
	})
}

// Backend is a capturing git.GitBackend. It records every validated request and
// can block an exchange so a test can cancel the request context mid-flight.
type Backend struct {
	mu       sync.Mutex
	requests []git.GitBackendRequest
	blocked  bool
	captured chan struct{}
	status   int
	body     []byte
}

// NewBackend returns a capturing backend.
func NewBackend() *Backend {
	return &Backend{
		captured: make(chan struct{}, 32),
		status:   http.StatusOK,
		body:     []byte("fixture-backend-response"),
	}
}

// Serve implements git.GitBackend.
func (backend *Backend) Serve(ctx context.Context, request git.GitBackendRequest, body io.Reader, response git.GitResponseWriter) error {
	var readErr error
	if body != nil {
		_, readErr = io.ReadAll(body)
	}
	backend.mu.Lock()
	backend.requests = append(backend.requests, cloneRequest(request))
	blocked := backend.blocked
	backend.mu.Unlock()

	if readErr != nil {
		return readErr
	}
	select {
	case backend.captured <- struct{}{}:
	default:
	}
	if blocked {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	backend.mu.Lock()
	status := backend.status
	out := append([]byte(nil), backend.body...)
	backend.mu.Unlock()
	response.WriteHeader(status)
	if len(out) > 0 {
		_, err := response.Write(out)
		return err
	}
	return nil
}

// Block makes every later exchange wait until its context is cancelled.
func (backend *Backend) Block() {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	backend.blocked = true
}

// Captured signals that a request has been recorded. It is buffered, so a
// non-blocking send never stalls the handler.
func (backend *Backend) Captured() <-chan struct{} { return backend.captured }

// Requests returns a copy of every recorded exchange.
func (backend *Backend) Requests() []git.GitBackendRequest {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	requests := make([]git.GitBackendRequest, 0, len(backend.requests))
	for _, request := range backend.requests {
		requests = append(requests, cloneRequest(request))
	}
	return requests
}

// Last returns the most recent exchange.
func (backend *Backend) Last() (git.GitBackendRequest, bool) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.requests) == 0 {
		return git.GitBackendRequest{}, false
	}
	return cloneRequest(backend.requests[len(backend.requests)-1]), true
}

func cloneRequest(request git.GitBackendRequest) git.GitBackendRequest {
	request.Environment = append([]string(nil), request.Environment...)
	request.Headers = request.Headers.Clone()
	return request
}

// Environment maps one backend environment slice.
func Environment(environment []string) map[string]string {
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		if key, value, ok := strings.Cut(entry, "="); ok {
			values[key] = value
		}
	}
	return values
}
