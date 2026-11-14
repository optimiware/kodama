package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
)

type fakeStorage struct{}

func (fakeStorage) RepositoryPath(id.TenantID, string) (string, error) { return "/tmp/repo", nil }
func (fakeStorage) EnsureRepository(id.TenantID, string) (git.RepositoryLocation, error) {
	return git.RepositoryLocation{Path: "/tmp/repo"}, nil
}

type fakePolicy struct{}

func (fakePolicy) Current(context.Context) (git.Config, error) {
	return git.ParseConfig([]byte("repo shop/web\noption resource = res-1\nRW = @all\n"))
}

type fakeExecutor struct{}

func (fakeExecutor) Run(context.Context, git.Command) (git.CommandResult, error) {
	return git.CommandResult{ExitCode: 0}, nil
}

type fakeObserver struct{}

func (fakeObserver) Observe(_ context.Context, _, ref string) (git.RefState, error) {
	return git.RefState{Ref: ref}, nil
}

type fakeJournal struct {
	entries map[string]git.JournalEntry
}

func (journal fakeJournal) Load(_ context.Context, key string) (git.JournalEntry, bool, error) {
	entry, ok := journal.entries[key]
	return entry, ok, nil
}

func (journal fakeJournal) Record(_ context.Context, entry git.JournalEntry) error {
	if journal.entries != nil {
		journal.entries[entry.SourceEventID] = entry
	}
	return nil
}

type fakeVerification struct{}

func (fakeVerification) Verify(context.Context, api.VerifyRequest) (api.VerifyResponse, error) {
	return api.VerifyResponse{}, errors.New("not configured")
}
func (fakeVerification) Report(context.Context, api.ReportRequest) (api.ReportResponse, error) {
	return api.ReportResponse{}, errors.New("not configured")
}

// countingVerification records whether the delegated hook ran at all. A
// pre-receive policy refusal must happen before the service observes the
// candidate.
type countingVerification struct {
	verifyCalls int
}

func (verification *countingVerification) Verify(context.Context, api.VerifyRequest) (api.VerifyResponse, error) {
	verification.verifyCalls++
	return api.VerifyResponse{}, errors.New("not configured")
}
func (*countingVerification) Report(context.Context, api.ReportRequest) (api.ReportResponse, error) {
	return api.ReportResponse{}, errors.New("not configured")
}

type fakeRetention struct{}

func (fakeRetention) ReferencedCommits(context.Context, api.Page) (git.CommitRefs, error) {
	return git.CommitRefs{}, errors.New("not configured")
}

type fakeClock struct{}

func (fakeClock) Now() time.Time { return time.Now() }

type fakeActors struct{ valid string }

func (actors fakeActors) ResolveActor(_ context.Context, handle string) (string, error) {
	if handle == actors.valid {
		return "credential", nil
	}
	return "", errors.New("actor handle is not valid")
}

func testHookHandler(t *testing.T) *HookHandler {
	t.Helper()
	return testHookHandlerWithJournal(t, fakeJournal{})
}

func testHookHandlerWithJournal(t *testing.T, journal git.JournalStore) *HookHandler {
	t.Helper()
	return testHookHandlerWithConfig(t, journal, HookHandlerConfig{})
}

func testGitService(t *testing.T, journal git.JournalStore, verify git.VerificationClient) *git.Service {
	t.Helper()
	service, err := git.NewService(git.ServiceConfig{
		TenantID:  "tenant-1",
		Storage:   fakeStorage{},
		Policies:  fakePolicy{},
		Executor:  fakeExecutor{},
		Observer:  fakeObserver{},
		Journal:   journal,
		Verify:    verify,
		Retention: fakeRetention{},
		Clock:     fakeClock{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

func testHookHandlerWithConfig(t *testing.T, journal git.JournalStore, config HookHandlerConfig) *HookHandler {
	t.Helper()
	config.Service = testGitService(t, journal, fakeVerification{})
	config.Actors = fakeActors{valid: "kah_valid"}
	if config.Policy == nil {
		config.Policy = fakePolicy{}
	}
	if config.RepositoryPath == nil {
		config.RepositoryPath = func(string) (string, error) {
			return "/tmp/repo", nil
		}
	}
	handler, err := NewHookHandler(config)
	if err != nil {
		t.Fatalf("NewHookHandler: %v", err)
	}
	return handler
}

func TestHookHandlerRefusesForgedHandlesUniformly(t *testing.T) {
	handler := testHookHandler(t)
	var want string
	for _, handle := range []string{"", "kah_forged", "not a handle!", "kah_valid_but_expired"} {
		request := validRequest(PhasePreReceive)
		request.ActorRef = handle
		response := handler.Handle(context.Background(), request)
		if response.Accepted() {
			t.Fatalf("handle %q was accepted", handle)
		}
		if want == "" {
			want = response.Message
		} else if response.Message != want {
			t.Fatalf("refusal %q differs from %q; refusals must be uniform", response.Message, want)
		}
	}
	if want != RejectionMessage {
		t.Fatalf("message = %q; want %q", want, RejectionMessage)
	}
}

func TestHookHandlerRefusesUnknownPhaseAndMissingEnvironment(t *testing.T) {
	handler := testHookHandler(t)
	request := validRequest(PhasePreReceive)
	request.Phase = "evil"
	if response := handler.Handle(context.Background(), request); response.Accepted() {
		t.Fatal("unknown phase was accepted")
	}
	request = validRequest(PhasePreReceive)
	request.SourceEvent = ""
	if response := handler.Handle(context.Background(), request); response.Accepted() {
		t.Fatal("missing environment was accepted")
	}
}

func TestHookHandlerRefusesUpdateLines(t *testing.T) {
	handler := testHookHandler(t)
	request := validRequest(PhaseUpdate)
	request.Lines = []string{"extra"}
	if response := handler.Handle(context.Background(), request); response.Accepted() {
		t.Fatal("update with stdin lines was accepted")
	}
}

// recordingPolicyValidator records every proposed policy commit and returns
// its configured decision.
type recordingPolicyValidator struct {
	commits []string
	err     error
}

func (validator *recordingPolicyValidator) ValidateCommit(_ context.Context, commit string) error {
	validator.commits = append(validator.commits, commit)
	return validator.err
}

func TestHookHandlerValidatesProposedTrustedPolicyBeforeAcceptance(t *testing.T) {
	validator := &recordingPolicyValidator{err: errors.New("malformed proposed policy")}
	handler := testHookHandlerWithConfig(t, fakeJournal{}, HookHandlerConfig{
		PolicyRepository: "shop/web",
		PolicyValidator:  validator,
	})
	request := validRequest(PhasePreReceive)
	request.ActorRef = "kah_valid"
	response := handler.Handle(context.Background(), request)
	if response.Accepted() || response.Message != RejectionMessage {
		t.Fatalf("response = %+v; want the uniform rejection", response)
	}
	candidate := strings.Repeat("2", 40)
	if len(validator.commits) != 1 || validator.commits[0] != candidate {
		t.Fatalf("validated commits = %v; want [%s]", validator.commits, candidate)
	}
}

// TestHookHandlerStopsBeforeTheServiceWhenThePolicyIsInvalid proves the
// proposal validator's error is honored: the delegated hook never observes the
// candidate, so an ignored validator cannot fall through to another decision.
func TestHookHandlerStopsBeforeTheServiceWhenThePolicyIsInvalid(t *testing.T) {
	verification := &countingVerification{}
	validator := &recordingPolicyValidator{err: errors.New("malformed proposed policy")}
	handler, err := NewHookHandler(HookHandlerConfig{
		Service:          testGitService(t, fakeJournal{}, verification),
		Actors:           fakeActors{valid: "kah_valid"},
		Policy:           fakePolicy{},
		PolicyRepository: "shop/web",
		PolicyValidator:  validator,
		RepositoryPath:   func(string) (string, error) { return "/tmp/repo", nil },
	})
	if err != nil {
		t.Fatalf("NewHookHandler: %v", err)
	}
	request := validRequest(PhasePreReceive)
	request.ActorRef = "kah_valid"
	response := handler.Handle(context.Background(), request)
	if response.Accepted() || response.Message != RejectionMessage {
		t.Fatalf("response = %+v; want the uniform rejection", response)
	}
	if len(validator.commits) != 1 {
		t.Fatalf("validated commits = %v; want one validation", validator.commits)
	}
	if verification.verifyCalls != 0 {
		t.Fatalf("service verification calls = %d; want none after a policy refusal", verification.verifyCalls)
	}
}

func TestHookHandlerValidatesMainEvenWhenItIsNotTheFirstLine(t *testing.T) {
	validator := &recordingPolicyValidator{err: errors.New("malformed proposed policy")}
	handler := testHookHandlerWithConfig(t, fakeJournal{}, HookHandlerConfig{
		PolicyRepository: "shop/web",
		PolicyValidator:  validator,
	})
	old := strings.Repeat("1", 40)
	first := strings.Repeat("2", 40)
	candidate := strings.Repeat("3", 40)
	request := validRequest(PhasePreReceive)
	request.ActorRef = "kah_valid"
	// refs/heads/aaa sorts and arrives before the trusted ref. Validating only
	// the first batch line would skip the proposed policy.
	request.Lines = []string{
		old + " " + first + " refs/heads/aaa",
		old + " " + candidate + " refs/heads/main",
	}
	response := handler.Handle(context.Background(), request)
	if response.Accepted() || response.Message != RejectionMessage {
		t.Fatalf("response = %+v; want the uniform rejection", response)
	}
	if len(validator.commits) != 1 || validator.commits[0] != candidate {
		t.Fatalf("validated commits = %v; want [%s]", validator.commits, candidate)
	}
}

func TestHookHandlerDoesNotValidateUntrustedRefsOrRepositories(t *testing.T) {
	validator := &recordingPolicyValidator{}
	handler := testHookHandlerWithConfig(t, fakeJournal{}, HookHandlerConfig{
		PolicyRepository: "shop/web",
		PolicyValidator:  validator,
	})
	old := strings.Repeat("1", 40)
	candidate := strings.Repeat("2", 40)

	// A work branch is never policy.
	request := validRequest(PhasePreReceive)
	request.ActorRef = "kah_valid"
	request.Lines = []string{old + " " + candidate + " refs/heads/work/candidate"}
	_ = handler.Handle(context.Background(), request)

	// Another repository's main is not this tenant's trusted policy ref.
	request = validRequest(PhasePreReceive)
	request.ActorRef = "kah_valid"
	request.Repository = "shop/linked"
	_ = handler.Handle(context.Background(), request)

	if len(validator.commits) != 0 {
		t.Fatalf("validated commits = %v; want none", validator.commits)
	}
}

// scriptedPolicyExecutor scripts the trusted ref, its object type and the
// committed policy blob for one policy source.
type scriptedPolicyExecutor struct {
	commit string
	kind   string
	blob   string
}

func (executor scriptedPolicyExecutor) Run(_ context.Context, command git.Command) (git.CommandResult, error) {
	joined := strings.Join(command.Args, " ")
	switch {
	case strings.Contains(joined, "for-each-ref"):
		if executor.commit == "" {
			return git.CommandResult{}, nil
		}
		return git.CommandResult{Stdout: []byte(CommittedPolicyRef + " " + executor.commit + "\n")}, nil
	case strings.Contains(joined, "cat-file"):
		for _, argument := range command.Args {
			if argument == "-t" {
				return git.CommandResult{Stdout: []byte(executor.kind + "\n")}, nil
			}
			if strings.Contains(argument, ":") {
				return git.CommandResult{Stdout: []byte(executor.blob)}, nil
			}
		}
	}
	return git.CommandResult{}, nil
}

func TestCommittedPolicyRefusesANonCommitTrustedRef(t *testing.T) {
	policy, err := NewCommittedPolicy(CommittedPolicyConfig{
		RepositoryPath: t.TempDir(),
		Executor: scriptedPolicyExecutor{
			commit: "commit-one",
			kind:   "tree",
			blob:   "repo repo\nRW = @all\n",
		},
	})
	if err != nil {
		t.Fatalf("NewCommittedPolicy: %v", err)
	}
	if _, err := policy.Current(context.Background()); err == nil {
		t.Fatal("Current accepted a trusted ref that does not point at a commit")
	}
}

func TestCommittedPolicyValidatesProposedCommitPolicy(t *testing.T) {
	malformed, err := NewCommittedPolicy(CommittedPolicyConfig{
		RepositoryPath: t.TempDir(),
		Executor: scriptedPolicyExecutor{
			commit: "commit-one",
			kind:   "commit",
			blob:   "repo repo\nRW = @wizard\n",
		},
	})
	if err != nil {
		t.Fatalf("NewCommittedPolicy: %v", err)
	}
	if err := malformed.ValidateCommit(context.Background(), strings.Repeat("2", 40)); err == nil {
		t.Fatal("ValidateCommit accepted a malformed proposed policy")
	}

	// A proposal is untrusted input: only a concrete object id is a candidate,
	// never a revision expression that would read the accepted policy. The
	// parseable blob also keeps the admin repository writable by the admin
	// role, which every accepted proposal must.
	valid, err := NewCommittedPolicy(CommittedPolicyConfig{
		RepositoryPath: t.TempDir(),
		Executor: scriptedPolicyExecutor{
			commit: "commit-one",
			kind:   "commit",
			blob:   "repo admin\nRW = @admin\n",
		},
	})
	if err != nil {
		t.Fatalf("NewCommittedPolicy: %v", err)
	}
	if err := valid.ValidateCommit(context.Background(), strings.Repeat("2", 40)); err != nil {
		t.Fatalf("ValidateCommit refused a valid proposed policy: %v", err)
	}
	if err := valid.ValidateCommit(context.Background(), "HEAD"); err == nil {
		t.Fatal("ValidateCommit accepted a revision expression as a proposed commit")
	}
}

func TestValidateObjectEnvironmentAllowsOnlyRepositoryObjectPaths(t *testing.T) {
	repository := "/repos/shop/web"
	objects := "/repos/shop/web/objects"
	valid := []string{
		EnvObjectDirectory + "=" + objects + "/tmp_objdir-incoming-abc",
		EnvAlternateObjectDirectories + "=" + objects,
		EnvQuarantinePath + "=" + objects + "/tmp_objdir-incoming-abc",
	}
	if err := validateObjectEnvironment(repository, valid); err != nil {
		t.Fatalf("valid object environment refused: %v", err)
	}
	invalid := [][]string{
		{EnvObjectDirectory + "=objects"},
		{EnvObjectDirectory + "=" + objects + "/../other"},
		{EnvObjectDirectory + "=/repos/shop/web/other"},
		{EnvAlternateObjectDirectories + "=/repos/other/objects"},
		{EnvAlternateObjectDirectories + "=" + objects + ":/repos/other/objects"},
		{EnvQuarantinePath + "=/etc"},
		{"GIT_DIR=/repos/shop/web"},
	}
	for _, environment := range invalid {
		if err := validateObjectEnvironment(repository, environment); err == nil {
			t.Fatalf("object environment %v was accepted", environment)
		}
	}
}

func TestRequestRefusesOverBoundObjectEnvironment(t *testing.T) {
	request := validRequest(PhasePreReceive)
	request.ObjectEnvironment = []string{
		EnvObjectDirectory + "=/repos/shop/web/objects/a",
		EnvAlternateObjectDirectories + "=/repos/shop/web/objects",
		EnvQuarantinePath + "=/repos/shop/web/objects/a",
		EnvObjectDirectory + "=/repos/shop/web/objects/b",
	}
	if err := request.Validate(); err == nil {
		t.Fatal("an over-bound object environment was accepted")
	}
}

func TestHookHandlerRefusesOutOfBoundsObjectEnvironment(t *testing.T) {
	handler := testHookHandler(t)
	request := validRequest(PhasePreReceive)
	request.ActorRef = "kah_valid"
	request.ObjectEnvironment = []string{EnvObjectDirectory + "=/repos/other/objects/quarantine"}
	response := handler.Handle(context.Background(), request)
	if response.Accepted() || response.Message != RejectionMessage {
		t.Fatalf("out-of-bounds object environment response = %+v; want the uniform rejection", response)
	}
}

// derivedEventID mirrors the hook's stable per-ref receive identity so
// the post-receive forged-handle test can seed an existing journal entry.
func derivedEventID(pushID, repository, ref, old, new string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"kodama-git-hook-v2", pushID, repository, ref, old, new,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func postReceiveRequest(handle string) Request {
	old := strings.Repeat("1", 40)
	candidate := strings.Repeat("2", 40)
	return Request{
		Phase:       PhasePostReceive,
		ActorRef:    handle,
		Repository:  "shop/web",
		ResourceID:  "res-1",
		SourceEvent: "event-1",
		Lines:       []string{old + " " + candidate + " refs/heads/main"},
	}
}

func TestHookHandlerRefusesForgedPostReceiveHandle(t *testing.T) {
	old := strings.Repeat("1", 40)
	candidate := strings.Repeat("2", 40)
	entry := git.JournalEntry{
		Version:       git.JournalVersion,
		ResourceID:    "res-1",
		Repository:    "shop/web",
		Ref:           "refs/heads/main",
		ExpectedBase:  old,
		Candidate:     candidate,
		SourceEventID: derivedEventID("event-1", "shop/web", "refs/heads/main", old, candidate),
		State:         git.JournalReported,
	}
	journal := fakeJournal{entries: map[string]git.JournalEntry{entry.SourceEventID: entry}}
	handler := testHookHandlerWithJournal(t, journal)

	// The valid handle proves the seeded journal would otherwise accept.
	accepted := handler.Handle(context.Background(), postReceiveRequest("kah_valid"))
	if !accepted.Accepted() {
		t.Fatalf("valid post-receive = %+v; want accept", accepted)
	}
	forged := handler.Handle(context.Background(), postReceiveRequest("kah_forged"))
	if forged.Accepted() || forged.Message != RejectionMessage {
		t.Fatalf("forged post-receive = %+v; want the uniform rejection", forged)
	}
}

// mutablePolicy is a policy source whose committed rules change between
// deliveries, standing in for an accepted admin push.
type mutablePolicy struct {
	mu   sync.Mutex
	body string
	err  error
}

func (policy *mutablePolicy) Current(context.Context) (git.Config, error) {
	policy.mu.Lock()
	defer policy.mu.Unlock()
	if policy.err != nil {
		return git.Config{}, policy.err
	}
	return git.ParseConfig([]byte(policy.body))
}

func (policy *mutablePolicy) set(body string) {
	policy.mu.Lock()
	defer policy.mu.Unlock()
	policy.body = body
	policy.err = nil
}

func (policy *mutablePolicy) fail(err error) {
	policy.mu.Lock()
	defer policy.mu.Unlock()
	policy.err = err
}

// countingJournal records how often the delegated hook reached durable state,
// so a policy gate can be proved to run before any settlement.
type countingJournal struct {
	mu      sync.Mutex
	entries map[string]git.JournalEntry
	loads   int
	records int
}

func (journal *countingJournal) Load(_ context.Context, key string) (git.JournalEntry, bool, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.loads++
	entry, ok := journal.entries[key]
	return entry, ok, nil
}

func (journal *countingJournal) Record(_ context.Context, entry git.JournalEntry) error {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.records++
	journal.entries[entry.SourceEventID] = entry
	return nil
}

func (journal *countingJournal) loadCount() int {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return journal.loads
}

func (journal *countingJournal) recordCount() int {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return journal.records
}

// TestHookHandlerGatesEveryPhaseOnTheCommittedPolicy pins that each hook phase,
// including a duplicate post-receive delivery of an already accepted entry,
// reads the committed policy before acting. A policy that stops declaring the
// repository, or one that is unavailable, refuses the delivery instead of
// settling a stale entry.
func TestHookHandlerGatesEveryPhaseOnTheCommittedPolicy(t *testing.T) {
	old := strings.Repeat("1", 40)
	candidate := strings.Repeat("2", 40)
	sourceEvent := derivedEventID("event-1", "shop/web", "refs/heads/main", old, candidate)
	journal := &countingJournal{entries: map[string]git.JournalEntry{
		sourceEvent: {
			Version:        git.JournalVersion,
			ResourceID:     "res-1",
			Repository:     "shop/web",
			Ref:            "refs/heads/main",
			ExpectedBase:   old,
			Candidate:      candidate,
			SourceEventID:  sourceEvent,
			VerificationID: "verify-1",
			State:          git.JournalReported,
			At:             time.Now(),
		},
	}}
	policy := &mutablePolicy{body: "repo admin\n    RW = @admin\n\nrepo shop/web\n    option resource = res-1\n    RW = @all\n"}
	handler := testHookHandlerWithConfig(t, journal, HookHandlerConfig{Policy: policy})

	// The duplicate delivery is otherwise acceptable: the accepted entry
	// settles from the journal.
	accepted := handler.Handle(context.Background(), postReceiveRequest("kah_valid"))
	if !accepted.Accepted() {
		t.Fatalf("post-receive with the committed policy = %+v; want accept", accepted)
	}
	loads := journal.loadCount()
	if loads != 1 {
		t.Fatalf("journal loads = %d; want one for the first delivery", loads)
	}

	// The accepted admin policy no longer declares the pushed repository. The
	// duplicate delivery must not bypass the changed policy.
	policy.set("repo admin\n    RW = @admin\n")
	refused := handler.Handle(context.Background(), postReceiveRequest("kah_valid"))
	if refused.Accepted() || refused.Message != RejectionMessage {
		t.Fatalf("duplicate delivery after the policy changed = %+v; want the uniform rejection", refused)
	}
	if got := journal.loadCount(); got != loads {
		t.Fatalf("journal loads = %d; want %d: the gate must refuse before settlement", got, loads)
	}
	if got := journal.recordCount(); got != 0 {
		t.Fatalf("journal records = %d; want none from the gated deliveries", got)
	}

	// An unavailable policy gates every phase, not only post-receive.
	policy.fail(fault.New(fault.DependencyUnavailable, "the tenant Git policy is unavailable"))
	for _, request := range []Request{
		validRequest(PhaseUpdate),
		validRequest(PhasePreReceive),
		postReceiveRequest("kah_valid"),
	} {
		request.ActorRef = "kah_valid"
		response := handler.Handle(context.Background(), request)
		if response.Accepted() || response.Message != RejectionMessage {
			t.Fatalf("phase %q with an unavailable policy = %+v; want the uniform rejection", request.Phase, response)
		}
	}
	if got := journal.loadCount(); got != loads {
		t.Fatalf("journal loads = %d; want %d after the unavailable-policy refusals", got, loads)
	}
}
