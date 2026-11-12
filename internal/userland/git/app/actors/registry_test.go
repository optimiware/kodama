package actors

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
)

const testTenant = id.TenantID("tenant-i03b")

var testNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

// streamRand is a deterministic, non-repeating random source. It lets tests
// assert handle independence without touching crypto/rand.
type streamRand struct {
	mu   sync.Mutex
	next byte
}

func (source *streamRand) Read(buffer []byte) (int, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	for index := range buffer {
		buffer[index] = source.next
		source.next++
	}
	return len(buffer), nil
}

type registryFixture struct {
	registry *Registry
	clock    *fakeClock
}

func newRegistryFixture(t *testing.T, config Config) *registryFixture {
	t.Helper()
	clock := newFakeClock(testNow)
	if config.Clock == nil {
		config.Clock = clock
	}
	if config.TenantID == "" {
		config.TenantID = testTenant
	}
	if config.Rand == nil {
		config.Rand = &streamRand{}
	}
	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return &registryFixture{registry: registry, clock: clock}
}

func mustIssue(t *testing.T, registry *Registry, ctx context.Context, credential string) string {
	t.Helper()
	handle, err := registry.IssueActor(ctx, credential)
	if err != nil {
		t.Fatalf("IssueActor: %v", err)
	}
	return handle
}

func requireClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want class %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("fault class = %v (ok=%v); want %s (err=%v)", class, ok, want, err)
	}
}

func requireUnauthenticated(t *testing.T, err error) {
	t.Helper()
	requireClass(t, err, fault.Unauthenticated)
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("error %v does not wrap ErrUnauthenticated", err)
	}
}

func TestIssueResolveRoundTripAndHandleShape(t *testing.T) {
	fixture := newRegistryFixture(t, Config{MaxHandles: 8, TTL: time.Minute})
	ctx := t.Context()
	const credential = "execution-credential-do-not-leak"
	handle := mustIssue(t, fixture.registry, ctx, credential)

	if len(handle) == 0 || len(handle) > 256 {
		t.Fatalf("handle length = %d; want a bounded non-empty handle", len(handle))
	}
	for index := 0; index < len(handle); index++ {
		character := handle[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.', character == ':':
		default:
			t.Fatalf("handle %q contains %q outside the validActorHandle alphabet", handle, character)
		}
	}
	if strings.Contains(handle, credential) {
		t.Fatal("handle contains the credential")
	}
	if strings.Contains(handle, "execution") || strings.Contains(handle, "credential") {
		t.Fatalf("handle %q appears derived from the credential", handle)
	}

	// A handle is not single-use: a hook may resolve it several times.
	for attempt := range 3 {
		credentialOut, err := fixture.registry.ResolveActor(context.Background(), handle)
		if err != nil {
			t.Fatalf("ResolveActor attempt %d: %v", attempt, err)
		}
		if credentialOut != credential {
			t.Fatalf("resolved credential = %q; want the issued credential", credentialOut)
		}
	}

	// Distinct issues mint distinct handles even from a deterministic stream.
	other := mustIssue(t, fixture.registry, ctx, credential)
	if other == handle {
		t.Fatal("two issues returned the same handle")
	}
}

func TestIssueActorRefusesInvalidCredential(t *testing.T) {
	fixture := newRegistryFixture(t, Config{})
	ctx := context.Background()
	cases := []struct {
		name       string
		credential string
	}{
		{name: "empty", credential: ""},
		{name: "oversized", credential: strings.Repeat("x", api.MaxCredentialBytes+1)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handle, err := fixture.registry.IssueActor(ctx, testCase.credential)
			if err == nil {
				t.Fatalf("IssueActor accepted %s credential and returned %q", testCase.name, handle)
			}
			requireClass(t, err, fault.InvalidInput)
			if !errors.Is(err, ErrCredential) {
				t.Fatalf("error %v does not wrap ErrCredential", err)
			}
			if testCase.credential != "" && strings.Contains(err.Error(), testCase.credential) {
				t.Fatal("issuance error echoed the credential")
			}
		})
	}

	var nilContext context.Context
	if _, err := fixture.registry.IssueActor(nilContext, "credential"); err == nil {
		t.Fatal("IssueActor accepted a nil context")
	}

	// The exact documented bound is accepted.
	atBound := strings.Repeat("b", api.MaxCredentialBytes)
	if _, err := fixture.registry.IssueActor(ctx, atBound); err != nil {
		t.Fatalf("IssueActor refused a credential at the bound: %v", err)
	}
}

func TestHandleExpiresWithIssuingContext(t *testing.T) {
	fixture := newRegistryFixture(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	const credential = "context-bound-credential"
	handle := mustIssue(t, fixture.registry, ctx, credential)

	secret := capturedSecret(t, fixture.registry, handle)
	if _, err := fixture.registry.ResolveActor(context.Background(), handle); err != nil {
		t.Fatalf("ResolveActor before cancellation: %v", err)
	}

	cancel()
	if _, err := fixture.registry.ResolveActor(context.Background(), handle); err == nil {
		t.Fatal("ResolveActor accepted a handle after the issuing context ended")
	} else {
		requireUnauthenticated(t, err)
	}
	requireZeroed(t, secret)
}

func TestHandleExpiresAtHardTTL(t *testing.T) {
	fixture := newRegistryFixture(t, Config{TTL: time.Minute})
	ctx := context.Background()
	const credential = "ttl-bound-credential"
	handle := mustIssue(t, fixture.registry, ctx, credential)
	secret := capturedSecret(t, fixture.registry, handle)

	fixture.clock.Advance(time.Minute - time.Nanosecond)
	if _, err := fixture.registry.ResolveActor(context.Background(), handle); err != nil {
		t.Fatalf("ResolveActor before TTL: %v", err)
	}

	// At the exact expiry instant the handle is no longer live.
	fixture.clock.Advance(time.Nanosecond)
	if _, err := fixture.registry.ResolveActor(context.Background(), handle); err == nil {
		t.Fatal("ResolveActor accepted a handle at its TTL")
	} else {
		requireUnauthenticated(t, err)
	}
	requireZeroed(t, secret)
}

func TestRevokeInvalidatesHandleAndZeroesCredential(t *testing.T) {
	fixture := newRegistryFixture(t, Config{})
	ctx := context.Background()
	handle := mustIssue(t, fixture.registry, ctx, "revoked-credential")
	secret := capturedSecret(t, fixture.registry, handle)

	fixture.registry.Revoke(handle)
	if _, err := fixture.registry.ResolveActor(context.Background(), handle); err == nil {
		t.Fatal("ResolveActor accepted a revoked handle")
	} else {
		requireUnauthenticated(t, err)
	}
	requireZeroed(t, secret)

	// Revoking an unknown handle is a no-op.
	fixture.registry.Revoke("kah_unknown")
	fixture.registry.Revoke("")
}

func TestMaxHandlesBoundAndRelease(t *testing.T) {
	fixture := newRegistryFixture(t, Config{MaxHandles: 2, TTL: time.Minute})
	ctx := context.Background()
	first := mustIssue(t, fixture.registry, ctx, "first-credential")
	mustIssue(t, fixture.registry, ctx, "second-credential")

	handle, err := fixture.registry.IssueActor(ctx, "third-credential")
	if err == nil {
		t.Fatalf("IssueActor beyond the bound returned %q", handle)
	}
	requireClass(t, err, fault.InternalFailure)
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("error %v does not wrap ErrCapacity", err)
	}

	fixture.registry.Revoke(first)
	if _, err := fixture.registry.IssueActor(ctx, "third-credential"); err != nil {
		t.Fatalf("IssueActor after releasing capacity: %v", err)
	}

	// An elapsed TTL also frees capacity even before its callback runs.
	fixture.clock.Advance(2 * time.Minute)
	if _, err := fixture.registry.IssueActor(ctx, "fourth-credential"); err != nil {
		t.Fatalf("IssueActor after TTL pruning: %v", err)
	}
}

func TestUniformHandleRefusal(t *testing.T) {
	clock := newFakeClock(testNow)
	first, err := NewRegistry(Config{TenantID: testTenant, Clock: clock, MaxHandles: 8, TTL: time.Minute, Rand: &streamRand{}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	other, err := NewRegistry(Config{TenantID: "tenant-other", Clock: clock, MaxHandles: 8, TTL: time.Minute, Rand: &streamRand{}})
	if err != nil {
		t.Fatalf("NewRegistry other: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	closed, err := NewRegistry(Config{TenantID: testTenant, Clock: clock, MaxHandles: 8, TTL: time.Minute, Rand: &streamRand{}})
	if err != nil {
		t.Fatalf("NewRegistry closed: %v", err)
	}

	ctx := context.Background()
	const credential = "uniform-credential"

	live := mustIssue(t, first, ctx, credential)

	cancelled, cancel := context.WithCancel(context.Background())
	cancelledHandle := mustIssue(t, first, cancelled, credential)
	cancel()

	ttlClock := newFakeClock(testNow)
	ttlRegistry, err := NewRegistry(Config{TenantID: testTenant, Clock: ttlClock, MaxHandles: 8, TTL: time.Minute, Rand: &streamRand{}})
	if err != nil {
		t.Fatalf("NewRegistry ttl: %v", err)
	}
	t.Cleanup(func() { _ = ttlRegistry.Close() })
	expired := mustIssue(t, ttlRegistry, ctx, credential)
	ttlClock.Advance(2 * time.Minute)

	revoked := mustIssue(t, first, ctx, credential)
	first.Revoke(revoked)

	crossRegistry := mustIssue(t, first, ctx, credential)

	closedHandle := mustIssue(t, closed, ctx, credential)
	if err := closed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	cases := []struct {
		name    string
		handle  string
		resolve func() (string, error)
	}{
		{name: "unknown", handle: "kah_00000000000000000000000000000000", resolve: func() (string, error) {
			return first.ResolveActor(ctx, "kah_00000000000000000000000000000000")
		}},
		{name: "malformed empty", handle: "", resolve: func() (string, error) {
			return first.ResolveActor(ctx, "")
		}},
		{name: "malformed characters", handle: "not a handle!", resolve: func() (string, error) {
			return first.ResolveActor(ctx, "not a handle!")
		}},
		{name: "credential itself", handle: credential, resolve: func() (string, error) {
			return first.ResolveActor(ctx, credential)
		}},
		{name: "expired by context", handle: cancelledHandle, resolve: func() (string, error) {
			return first.ResolveActor(ctx, cancelledHandle)
		}},
		{name: "expired by TTL", handle: expired, resolve: func() (string, error) {
			return ttlRegistry.ResolveActor(ctx, expired)
		}},
		{name: "revoked", handle: revoked, resolve: func() (string, error) {
			return first.ResolveActor(ctx, revoked)
		}},
		{name: "cross registry", handle: crossRegistry, resolve: func() (string, error) {
			return other.ResolveActor(ctx, crossRegistry)
		}},
		{name: "closed registry", handle: closedHandle, resolve: func() (string, error) {
			return closed.ResolveActor(ctx, closedHandle)
		}},
	}

	var wantText string
	for _, testCase := range cases {
		credentialOut, err := testCase.resolve()
		if err == nil {
			t.Fatalf("%s: ResolveActor returned credential %q", testCase.name, credentialOut)
		}
		requireUnauthenticated(t, err)
		if strings.Contains(err.Error(), credential) {
			t.Fatalf("%s: refusal echoed the credential", testCase.name)
		}
		if wantText == "" {
			wantText = err.Error()
		} else if err.Error() != wantText {
			t.Fatalf("%s: refusal text %q differs from %q; refusals must be uniform", testCase.name, err.Error(), wantText)
		}
	}

	// The live handle still resolves and is not affected by the probes.
	if credentialOut, err := first.ResolveActor(ctx, live); err != nil || credentialOut != credential {
		t.Fatalf("live handle = (%q, %v); want the issued credential", credentialOut, err)
	}
}

func TestFormattingNeverRevealsCredential(t *testing.T) {
	fixture := newRegistryFixture(t, Config{})
	const credential = "formatting-credential"
	handle := mustIssue(t, fixture.registry, context.Background(), credential)
	if handle == "" {
		t.Fatal("empty handle")
	}

	formats := []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X"}
	for _, format := range formats {
		rendered := fmt.Sprintf(format, fixture.registry)
		if strings.Contains(rendered, credential) {
			t.Fatalf("%s rendered the credential: %q", format, rendered)
		}
	}
	if !strings.Contains(fmt.Sprintf("%v", fixture.registry), string(testTenant)) {
		t.Fatalf("registry String() lost its safe metadata: %q", fmt.Sprintf("%v", fixture.registry))
	}

	_, err := fixture.registry.ResolveActor(context.Background(), "forged")
	if err == nil {
		t.Fatal("forged handle resolved")
	}
	for _, format := range formats {
		if strings.Contains(fmt.Sprintf(format, err), credential) {
			t.Fatalf("%s of the refusal rendered the credential", format)
		}
	}

	// A nil registry is also safe to render.
	var nilRegistry *Registry
	for _, format := range formats {
		if strings.Contains(fmt.Sprintf(format, nilRegistry), credential) {
			t.Fatalf("%s of a nil registry rendered the credential", format)
		}
	}
}

func TestCloseRevokesEverythingAndRefusesFurtherIssuance(t *testing.T) {
	fixture := newRegistryFixture(t, Config{})
	ctx := t.Context()
	first := mustIssue(t, fixture.registry, ctx, "first-credential")
	second := mustIssue(t, fixture.registry, ctx, "second-credential")
	firstSecret := capturedSecret(t, fixture.registry, first)
	secondSecret := capturedSecret(t, fixture.registry, second)

	if err := fixture.registry.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requireZeroed(t, firstSecret)
	requireZeroed(t, secondSecret)
	for _, handle := range []string{first, second} {
		if _, err := fixture.registry.ResolveActor(context.Background(), handle); err == nil {
			t.Fatalf("ResolveActor accepted %q after Close", handle)
		} else {
			requireUnauthenticated(t, err)
		}
	}
	if _, err := fixture.registry.IssueActor(context.Background(), "credential"); err == nil {
		t.Fatal("IssueActor accepted work after Close")
	} else {
		requireClass(t, err, fault.InternalFailure)
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("error %v does not wrap ErrClosed", err)
		}
	}
	if err := fixture.registry.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestConcurrentUseIsRaceSafe(t *testing.T) {
	fixture := newRegistryFixture(t, Config{MaxHandles: 64, TTL: 10 * time.Minute})
	ctx := t.Context()

	var wait sync.WaitGroup
	for worker := range 8 {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for iteration := range 50 {
				credential := fmt.Sprintf("credential-%d-%d", worker, iteration)
				handle, err := fixture.registry.IssueActor(ctx, credential)
				if err != nil {
					t.Errorf("IssueActor: %v", err)
					return
				}
				resolved, err := fixture.registry.ResolveActor(ctx, handle)
				if err != nil || resolved != credential {
					t.Errorf("ResolveActor = (%q, %v); want the issued credential", resolved, err)
					return
				}
				fixture.registry.Revoke(handle)
			}
		}(worker)
	}
	wait.Wait()
}

func TestNoGoroutineLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()
	fixture := newRegistryFixture(t, Config{MaxHandles: 128})
	for iteration := range 64 {
		ctx, cancel := context.WithCancel(context.Background())
		if _, err := fixture.registry.IssueActor(ctx, fmt.Sprintf("credential-%d", iteration)); err != nil {
			t.Fatalf("IssueActor: %v", err)
		}
		cancel()
	}
	if err := fixture.registry.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		current := runtime.NumGoroutine()
		if current <= baseline+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d; want at most %d after Close", current, baseline+2)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// capturedSecret returns the registry's credential storage for a live handle so
// a test can prove best-effort zeroing without exporting it.
func capturedSecret(t *testing.T, registry *Registry, handle string) []byte {
	t.Helper()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry, ok := registry.entries[handle]
	if !ok {
		t.Fatalf("handle %q is not live", handle)
	}
	return entry.secret
}

func requireZeroed(t *testing.T, secret []byte) {
	t.Helper()
	if len(secret) == 0 {
		t.Fatal("captured secret is empty")
	}
	for index, value := range secret {
		if value != 0 {
			t.Fatalf("secret byte %d = %d; want zeroed storage", index, value)
		}
	}
}

// Hook-boundary fakes. The hook's only job here is to prove the minted handle
// passes the real validActorHandle check and resolves through the registry.

type hookStorage struct{}

func (hookStorage) RepositoryPath(tenant id.TenantID, repository string) (string, error) {
	return "/fixture/" + string(tenant) + "/" + repository, nil
}

func (hookStorage) EnsureRepository(tenant id.TenantID, repository string) (git.RepositoryLocation, error) {
	path, err := hookStorage{}.RepositoryPath(tenant, repository)
	return git.RepositoryLocation{Path: path}, err
}

type hookPolicy struct{ config git.Config }

func (policy hookPolicy) Current(context.Context) (git.Config, error) { return policy.config, nil }

type hookExecutor struct {
	mu   sync.Mutex
	refs map[string]string
}

func (executor *hookExecutor) Run(ctx context.Context, command git.Command) (git.CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return git.CommandResult{}, err
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	args := command.Args
	if command.Name != "git" || len(args) != 6 {
		return git.CommandResult{}, fmt.Errorf("unexpected fixture command %+v", command)
	}
	switch args[2] {
	case "update-ref":
		executor.refs[args[3]] = args[4]
		return git.CommandResult{ExitCode: 0}, nil
	case "merge-base":
		return git.CommandResult{ExitCode: 0}, nil
	default:
		return git.CommandResult{}, fmt.Errorf("unexpected fixture command %+v", command)
	}
}

func (executor *hookExecutor) ref(ref string) string {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.refs[ref]
}

type hookObserver struct{ executor *hookExecutor }

func (observer hookObserver) Observe(_ context.Context, _ string, ref string) (git.RefState, error) {
	return git.RefState{Ref: ref, Commit: observer.executor.ref(ref)}, nil
}

type hookJournal struct {
	mu      sync.Mutex
	entries map[string]git.JournalEntry
}

func (journal *hookJournal) Load(_ context.Context, key string) (git.JournalEntry, bool, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	entry, ok := journal.entries[key]
	return entry, ok, nil
}

func (journal *hookJournal) Record(_ context.Context, entry git.JournalEntry) error {
	if err := entry.Validate(); err != nil {
		return err
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.entries[entry.SourceEventID] = entry
	return nil
}

type hookVerifier struct {
	mu          sync.Mutex
	credentials []string
}

func (verifier *hookVerifier) Verify(_ context.Context, request api.VerifyRequest) (api.VerifyResponse, error) {
	verifier.mu.Lock()
	verifier.credentials = append(verifier.credentials, request.ActorCredential)
	verifier.mu.Unlock()
	return api.VerifyResponse{
		Principal:      api.VerifiedPrincipal{ID: "execution-1", Kind: "execution"},
		VerificationID: "verification-1",
	}, nil
}

func (verifier *hookVerifier) Report(_ context.Context, request api.ReportRequest) (api.ReportResponse, error) {
	return api.ReportResponse{VerificationID: request.VerificationID, EventID: "event-1"}, nil
}

func (verifier *hookVerifier) seen() []string {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	return append([]string(nil), verifier.credentials...)
}

type hookRetention struct{}

func (hookRetention) ReferencedCommits(context.Context, api.Page) (git.CommitRefs, error) {
	return git.CommitRefs{}, errors.New("fixture retention is unavailable")
}

func TestHandleAcceptedByRealHook(t *testing.T) {
	config, err := git.ParseConfig([]byte("repo shop/web\n    RW  refs/heads/main = @execution\n"))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	executor := &hookExecutor{refs: map[string]string{}}
	journal := &hookJournal{entries: map[string]git.JournalEntry{}}
	verifier := &hookVerifier{}
	service, err := git.NewService(git.ServiceConfig{
		TenantID:  testTenant,
		Storage:   hookStorage{},
		Policies:  hookPolicy{config: config},
		Executor:  executor,
		Observer:  hookObserver{executor: executor},
		Journal:   journal,
		Verify:    verifier,
		Retention: hookRetention{},
		Clock:     newFakeClock(testNow),
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	fixture := newRegistryFixture(t, Config{})
	ctx := t.Context()
	const credential = "hook-credential"
	handle := mustIssue(t, fixture.registry, ctx, credential)

	environment := map[string]string{
		git.EnvActorRef:    handle,
		git.EnvRepository:  "shop/web",
		git.EnvResourceID:  "res-i03b",
		git.EnvSourceEvent: "push-fixture-1",
	}
	hook, err := git.NewHook(git.HookConfig{
		Service: service,
		Actors:  fixture.registry,
		Environment: func(key string) (string, bool) {
			value, ok := environment[key]
			return value, ok
		},
	})
	if err != nil {
		t.Fatalf("NewHook: %v", err)
	}
	result, err := hook.RunUpdate(ctx, "refs/heads/main", strings.Repeat("0", 40), strings.Repeat("2", 40))
	if err != nil {
		t.Fatalf("RunUpdate through the real hook: %v", err)
	}
	if !result.Accepted() {
		t.Fatalf("hook result = %+v; want accept", result)
	}
	if got := executor.ref("refs/heads/main"); got != strings.Repeat("2", 40) {
		t.Fatalf("ref = %q; want the candidate", got)
	}
	if credentials := verifier.seen(); len(credentials) != 1 || credentials[0] != credential {
		t.Fatalf("verification credentials = %q; want the resolved credential", credentials)
	}
}

func TestNewRegistryValidation(t *testing.T) {
	if _, err := NewRegistry(Config{}); err == nil {
		t.Fatal("NewRegistry accepted an empty tenant")
	}
	if _, err := NewRegistry(Config{TenantID: testTenant, TTL: MaxTTL + time.Second}); err == nil {
		t.Fatal("NewRegistry accepted a TTL above the hard maximum")
	}
	registry, err := NewRegistry(Config{TenantID: testTenant})
	if err != nil {
		t.Fatalf("NewRegistry defaults: %v", err)
	}
	if err := registry.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
