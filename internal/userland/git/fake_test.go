package git_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	git "github.com/optimiware/kodama/internal/userland/git"
)

var testNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

const testTenant id.TenantID = "tenant-acme"

const (
	testResourceID     = "res-web"
	testCredential     = "actor-credential-do-not-persist"
	testSourceEventID  = "git-event-1"
	testVerificationID = "ver-1"
	testBase           = "base-1"
	testCandidate      = "cand-1"
	testRef            = "refs/heads/main"
)

// testGitConfig mirrors the contract rule vector and adds a pipeline service
// account on the guarded repository so a short-lived pipeline identity can be
// exercised as a service account.
const testGitConfig = `
repo admin
    RW                              = @admin

repo shop/web
    R                               = @all
    RW+  refs/heads/work/           = @engineer @execution
    RW   refs/heads/main            = @execution
    option approve.refs/heads/main  = reviewer

repo shop/deploy
    R                               = @operator @execution
    RW+  refs/heads/release/        = @execution
    RW   refs/heads/main            = @execution
    option approve.refs/heads/main  = operator

repo shop/guarded
    -    refs/heads/secret          = @all
    RW   refs/heads/                = @engineer
    RW   refs/heads/svc             = shop-svc ci-pipeline
    option approve.refs/heads/release/ = reviewer
`

func mustConfig(t *testing.T, body string) git.Config {
	t.Helper()
	config, err := git.ParseConfig([]byte(body))
	if err != nil {
		t.Fatalf("ParseConfig refused the fixture: %v", err)
	}
	return config
}

func humanIdentity(roles ...string) git.Identity {
	return git.Identity{Roles: roles}
}

func executionIdentity() git.Identity {
	return git.Identity{Execution: true}
}

func serviceIdentity(id string) git.Identity {
	return git.Identity{ServiceID: id}
}

// callLog is the shared ordering evidence across the fakes. It lets a test
// assert intent-before-effect, verify-before-command and report-after-
// acceptance without inspecting private implementation steps.
type callLog struct {
	mu      sync.Mutex
	entries []string
}

func (log *callLog) add(entry string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.entries = append(log.entries, entry)
}

func (log *callLog) snapshot() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.entries...)
}

func (log *callLog) index(entry string) int {
	log.mu.Lock()
	defer log.mu.Unlock()
	for index, candidate := range log.entries {
		if candidate == entry {
			return index
		}
	}
	return -1
}

type fakeClock struct {
	now time.Time
}

func (clock *fakeClock) Now() time.Time { return clock.now }

// fakePolicySource is the mutable tenant git.conf boundary. A test swaps the
// config to prove the service reads it per attempt instead of caching.
type fakePolicySource struct {
	mu     sync.Mutex
	config git.Config
	err    error
	calls  int
}

func (source *fakePolicySource) Current(context.Context) (git.Config, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.calls++
	if source.err != nil {
		return git.Config{}, source.err
	}
	return source.config, nil
}

func (source *fakePolicySource) set(config git.Config) {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.config = config
}

func (source *fakePolicySource) callCount() int {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.calls
}

// verificationScript is one deterministic verification fact for one credential.
type verificationScript struct {
	principal      api.VerifiedPrincipal
	roles          []string
	approval       bool
	verificationID string
}

func humanScript(principalID string, roles ...string) verificationScript {
	return verificationScript{
		principal:      api.VerifiedPrincipal{ID: principalID, Kind: "human"},
		roles:          roles,
		verificationID: testVerificationID,
	}
}

func executionScript(principalID string) verificationScript {
	return verificationScript{
		principal:      api.VerifiedPrincipal{ID: principalID, Kind: "execution"},
		approval:       true,
		verificationID: testVerificationID,
	}
}

func serviceScript(principalID string) verificationScript {
	return verificationScript{
		principal:      api.VerifiedPrincipal{ID: principalID, Kind: "service"},
		verificationID: testVerificationID,
	}
}

// fakeVerifier is the public verification/report boundary. It never echoes a
// credential and records every request so the test can pin the canonical
// subject and the report-only retry.
type fakeVerifier struct {
	mu            sync.Mutex
	scripts       map[string]verificationScript
	verifyCalls   []api.VerifyRequest
	reportCalls   []api.ReportRequest
	verifyErr     error
	reportErr     error
	reportFailure int
	log           *callLog
}

func newFakeVerifier(log *callLog) *fakeVerifier {
	return &fakeVerifier{scripts: map[string]verificationScript{}, log: log}
}

func (verifier *fakeVerifier) script(credential string, script verificationScript) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	verifier.scripts[credential] = script
}

func (verifier *fakeVerifier) Verify(_ context.Context, request api.VerifyRequest) (api.VerifyResponse, error) {
	verifier.mu.Lock()
	verifier.verifyCalls = append(verifier.verifyCalls, request)
	script, ok := verifier.scripts[request.ActorCredential]
	err := verifier.verifyErr
	verifier.mu.Unlock()

	verifier.log.add("verify")
	if err != nil {
		return api.VerifyResponse{}, err
	}
	if !ok {
		return api.VerifyResponse{}, errors.New("fake verifier: unknown actor credential")
	}
	return api.VerifyResponse{
		Principal:      script.principal,
		Roles:          append([]string(nil), script.roles...),
		Approval:       script.approval,
		VerificationID: script.verificationID,
	}, nil
}

func (verifier *fakeVerifier) Report(_ context.Context, request api.ReportRequest) (api.ReportResponse, error) {
	verifier.mu.Lock()
	verifier.reportCalls = append(verifier.reportCalls, request)
	call := len(verifier.reportCalls)
	fail := verifier.reportFailure > 0
	if fail {
		verifier.reportFailure--
	}
	err := verifier.reportErr
	verifier.mu.Unlock()

	verifier.log.add("report")
	if fail {
		return api.ReportResponse{}, errors.New("fake verifier: report transport unavailable")
	}
	if err != nil {
		return api.ReportResponse{}, err
	}
	return api.ReportResponse{
		VerificationID: request.VerificationID,
		EventID:        "evt-" + strconv.Itoa(call),
		Replayed:       call > 1,
	}, nil
}

func (verifier *fakeVerifier) verifyRequests() []api.VerifyRequest {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	return append([]api.VerifyRequest(nil), verifier.verifyCalls...)
}

func (verifier *fakeVerifier) reportRequests() []api.ReportRequest {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	return append([]api.ReportRequest(nil), verifier.reportCalls...)
}

func (verifier *fakeVerifier) failNextReports(count int) {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	verifier.reportFailure = count
}

// fakeJournal is the durable-journal boundary with explicit fault injection. A
// record failure happens before the write, so the prior durable state remains
// loadable, exactly like a lost journal write after an effect.
type fakeJournal struct {
	mu        sync.Mutex
	entries   map[string]git.JournalEntry
	records   []git.JournalEntry
	log       *callLog
	recordErr func(git.JournalEntry) error
	loadErr   error
}

func newFakeJournal(log *callLog) *fakeJournal {
	return &fakeJournal{entries: map[string]git.JournalEntry{}, log: log}
}

func (journal *fakeJournal) Load(_ context.Context, key string) (git.JournalEntry, bool, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.loadErr != nil {
		return git.JournalEntry{}, false, journal.loadErr
	}
	entry, ok := journal.entries[key]
	return entry, ok, nil
}

func (journal *fakeJournal) Record(_ context.Context, entry git.JournalEntry) error {
	if err := entry.Validate(); err != nil {
		return fmt.Errorf("fake journal: %w", err)
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.recordErr != nil {
		if err := journal.recordErr(entry); err != nil {
			journal.log.add("journal:record-failed:" + string(entry.State))
			return err
		}
	}
	journal.entries[entry.SourceEventID] = entry
	journal.records = append(journal.records, entry)
	journal.log.add("journal:record:" + string(entry.State))
	return nil
}

func (journal *fakeJournal) state(key string) (git.JournalState, bool) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	entry, ok := journal.entries[key]
	return entry.State, ok
}

func (journal *fakeJournal) entry(key string) (git.JournalEntry, bool) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	entry, ok := journal.entries[key]
	return entry, ok
}

func (journal *fakeJournal) recorded() []git.JournalEntry {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return append([]git.JournalEntry(nil), journal.records...)
}

// current returns the latest durable entry per source event.
func (journal *fakeJournal) current() []git.JournalEntry {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	entries := make([]git.JournalEntry, 0, len(journal.entries))
	for _, entry := range journal.entries {
		entries = append(entries, entry)
	}
	return entries
}

func (journal *fakeJournal) failRecordWhen(match func(git.JournalEntry) bool) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.recordErr = func(entry git.JournalEntry) error {
		if match(entry) {
			return errors.New("fake journal: durable write lost")
		}
		return nil
	}
}

// fakeExecutor is a deterministic in-memory Git. It only accepts `git -C
// <path>` commands for a registered repository path, so a cross-tenant path bug
// fails loudly instead of silently touching another tenant's refs.
type fakeExecutor struct {
	mu       sync.Mutex
	paths    map[string]string
	refs     map[string]string
	objects  map[string]bool
	ancestry map[string]bool
	commands []git.Command
	after    map[string]error
	gate     func(git.Command) bool
	gateIn   chan struct{}
	gateOut  chan struct{}
	gateOnce sync.Once
	log      *callLog
}

func newFakeExecutor(log *callLog) *fakeExecutor {
	return &fakeExecutor{
		paths:    map[string]string{},
		refs:     map[string]string{},
		objects:  map[string]bool{},
		ancestry: map[string]bool{},
		after:    map[string]error{},
		log:      log,
	}
}

func (executor *fakeExecutor) registerRepository(path, repository string) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.paths[path] = repository
}

func (executor *fakeExecutor) setRef(repository, ref, commit string) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.refs[repository+"|"+ref] = commit
}

func (executor *fakeExecutor) commit(repository, ref string) string {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.refs[repository+"|"+ref]
}

func (executor *fakeExecutor) setObject(repository, commit string, exists bool) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.objects[repository+"|"+commit] = exists
}

func (executor *fakeExecutor) setAncestor(repository, old, next string, ancestor bool) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.ancestry[repository+"|"+old+"|"+next] = ancestor
}

func (executor *fakeExecutor) failAfter(subcommand string, err error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.after[subcommand] = err
}

// gateOn holds the first command matching match at an explicit barrier. The
// returned release function is idempotent.
func (executor *fakeExecutor) gateOn(match func(git.Command) bool) (<-chan struct{}, func()) {
	entered := make(chan struct{})
	release := make(chan struct{})
	executor.mu.Lock()
	executor.gate = match
	executor.gateIn = entered
	executor.gateOut = release
	executor.gateOnce = sync.Once{}
	executor.mu.Unlock()
	var once sync.Once
	return entered, func() { once.Do(func() { close(release) }) }
}

func (executor *fakeExecutor) commandsSnapshot() []git.Command {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]git.Command(nil), executor.commands...)
}

func (executor *fakeExecutor) updateRefCount() int {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	total := 0
	for _, command := range executor.commands {
		if commandSubcommand(command) == "update-ref" {
			total++
		}
	}
	return total
}

func commandSubcommand(command git.Command) string {
	if len(command.Args) >= 3 && command.Args[0] == "-C" {
		return command.Args[2]
	}
	return ""
}

func (executor *fakeExecutor) Run(ctx context.Context, command git.Command) (git.CommandResult, error) {
	if len(command.Args) < 3 || command.Args[0] != "-C" {
		return git.CommandResult{}, fmt.Errorf("fake executor: unsupported command %+v", command)
	}
	path := command.Args[1]
	subcommand := command.Args[2]

	executor.mu.Lock()
	executor.commands = append(executor.commands, command)
	repository := executor.paths[path]
	gate := executor.gate
	gateIn := executor.gateIn
	gateOut := executor.gateOut
	executor.mu.Unlock()

	executor.log.add("exec:" + subcommand)
	if repository == "" {
		return git.CommandResult{}, fmt.Errorf("fake executor: unregistered repository path %q", path)
	}
	if gate != nil && gate(command) {
		executor.gateOnce.Do(func() { close(gateIn) })
		select {
		case <-gateOut:
		case <-ctx.Done():
			return git.CommandResult{}, ctx.Err()
		}
	}

	result, err := executor.execute(repository, subcommand, command.Args[3:])
	if err != nil {
		return git.CommandResult{}, err
	}

	executor.mu.Lock()
	after := executor.after[subcommand]
	executor.mu.Unlock()
	if after != nil {
		return result, after
	}
	return result, nil
}

func (executor *fakeExecutor) execute(repository, subcommand string, rest []string) (git.CommandResult, error) {
	switch subcommand {
	case "merge-base":
		if len(rest) != 3 || rest[0] != "--is-ancestor" {
			return git.CommandResult{}, fmt.Errorf("fake executor: bad merge-base %v", rest)
		}
		executor.mu.Lock()
		ancestor := executor.ancestry[repository+"|"+rest[1]+"|"+rest[2]]
		executor.mu.Unlock()
		if ancestor {
			return git.CommandResult{ExitCode: 0}, nil
		}
		return git.CommandResult{ExitCode: 1}, nil
	case "update-ref":
		executor.mu.Lock()
		defer executor.mu.Unlock()
		switch {
		case len(rest) == 2:
			executor.refs[repository+"|"+rest[0]] = rest[1]
			return git.CommandResult{ExitCode: 0}, nil
		case len(rest) == 3 && rest[0] == "-d":
			key := repository + "|" + rest[1]
			if executor.refs[key] != rest[2] {
				return git.CommandResult{ExitCode: 1, Stderr: []byte("stale ref")}, nil
			}
			delete(executor.refs, key)
			return git.CommandResult{ExitCode: 0}, nil
		case len(rest) == 3:
			ref, next, expected := rest[0], rest[1], rest[2]
			current := executor.refs[repository+"|"+ref]
			if current != expected {
				return git.CommandResult{ExitCode: 1, Stderr: []byte("stale ref")}, nil
			}
			executor.refs[repository+"|"+ref] = next
			return git.CommandResult{ExitCode: 0}, nil
		default:
			return git.CommandResult{}, fmt.Errorf("fake executor: bad update-ref %v", rest)
		}
	case "rev-parse":
		if len(rest) != 3 || rest[0] != "--verify" || rest[1] != "--quiet" {
			return git.CommandResult{}, fmt.Errorf("fake executor: bad rev-parse %v", rest)
		}
		commit := strings.TrimSuffix(rest[2], "^{commit}")
		executor.mu.Lock()
		exists := executor.objects[repository+"|"+commit]
		executor.mu.Unlock()
		if exists {
			return git.CommandResult{ExitCode: 0, Stdout: []byte(commit + "\n")}, nil
		}
		return git.CommandResult{ExitCode: 1}, nil
	case "for-each-ref":
		executor.mu.Lock()
		lines := make([]string, 0)
		prefix := repository + "|"
		for key, commit := range executor.refs {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			lines = append(lines, strings.TrimPrefix(key, prefix)+" "+commit)
		}
		executor.mu.Unlock()
		sort.Strings(lines)
		stdout := []byte(strings.Join(lines, "\n"))
		if len(lines) > 0 {
			stdout = append(stdout, '\n')
		}
		return git.CommandResult{ExitCode: 0, Stdout: stdout}, nil
	default:
		return git.CommandResult{}, fmt.Errorf("fake executor: unsupported subcommand %q", subcommand)
	}
}

// fakeObserver observes the same ref state the executor mutates, with an
// optional per-ref override to model a concurrent writer between observation
// and compare-and-swap.
type fakeObserver struct {
	executor  *fakeExecutor
	overrides map[string]string
	err       error
	log       *callLog
}

func (observer *fakeObserver) Observe(_ context.Context, repository, ref string) (git.RefState, error) {
	observer.log.add("observe:" + repository + "|" + ref)
	if observer.err != nil {
		return git.RefState{}, observer.err
	}
	if observer.overrides != nil {
		if commit, ok := observer.overrides[repository+"|"+ref]; ok {
			return git.RefState{Ref: ref, Commit: commit}, nil
		}
	}
	return git.RefState{Ref: ref, Commit: observer.executor.commit(repository, ref)}, nil
}

// fakeRetention serves ordered pages of referenced commits through the
// authorized query boundary.
type fakeRetention struct {
	mu       sync.Mutex
	pages    []git.CommitRefs
	err      error
	calls    int
	requests []api.Page
	log      *callLog
}

func (query *fakeRetention) ReferencedCommits(_ context.Context, page api.Page) (git.CommitRefs, error) {
	query.mu.Lock()
	defer query.mu.Unlock()
	query.calls++
	query.requests = append(query.requests, page)
	query.log.add("retention:" + page.Cursor)
	if query.err != nil {
		return git.CommitRefs{}, query.err
	}
	if query.calls > len(query.pages) {
		if len(query.pages) == 0 {
			return git.CommitRefs{}, errors.New("fake retention: no pages")
		}
		// Keep serving the last page so an unbounded caller cannot hang the
		// test; a correct service refuses the repeated cursor long before this.
		if query.calls > 1000 {
			return git.CommitRefs{}, errors.New("fake retention: page overflow")
		}
		return query.pages[len(query.pages)-1], nil
	}
	return query.pages[query.calls-1], nil
}

func (query *fakeRetention) pageRequests() []api.Page {
	query.mu.Lock()
	defer query.mu.Unlock()
	return append([]api.Page(nil), query.requests...)
}

func (query *fakeRetention) callCount() int {
	query.mu.Lock()
	defer query.mu.Unlock()
	return query.calls
}

// fixture is one isolated tenant service plus its deterministic boundaries.
type fixture struct {
	t         *testing.T
	tenant    id.TenantID
	root      string
	storage   *git.LocalStorage
	policies  *fakePolicySource
	executor  *fakeExecutor
	observer  *fakeObserver
	journal   *fakeJournal
	verifier  *fakeVerifier
	retention *fakeRetention
	clock     *fakeClock
	service   *git.Service
	log       *callLog
}

func newFixture(t *testing.T) *fixture {
	return newFixtureWithConfig(t, testGitConfig)
}

func newFixtureWithConfig(t *testing.T, body string) *fixture {
	t.Helper()
	root := t.TempDir()
	storage, err := git.NewLocalStorage(root)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	log := &callLog{}
	fixture := &fixture{
		t:         t,
		tenant:    testTenant,
		root:      root,
		storage:   storage,
		policies:  &fakePolicySource{config: mustConfig(t, body)},
		executor:  newFakeExecutor(log),
		journal:   newFakeJournal(log),
		verifier:  newFakeVerifier(log),
		retention: &fakeRetention{log: log},
		clock:     &fakeClock{now: testNow},
		log:       log,
	}
	fixture.observer = &fakeObserver{executor: fixture.executor, log: log}
	fixture.service = fixture.newService()
	for _, name := range fixture.policies.config.PolicyNames() {
		fixture.register(name)
	}
	return fixture
}

func (f *fixture) newService() *git.Service {
	f.t.Helper()
	service, err := git.NewService(git.ServiceConfig{
		TenantID:  f.tenant,
		Storage:   f.storage,
		Policies:  f.policies,
		Executor:  f.executor,
		Observer:  f.observer,
		Journal:   f.journal,
		Verify:    f.verifier,
		Retention: f.retention,
		Clock:     f.clock,
	})
	if err != nil {
		f.t.Fatalf("NewService: %v", err)
	}
	return service
}

func (f *fixture) register(repository string) {
	f.t.Helper()
	path, err := f.storage.RepositoryPath(f.tenant, repository)
	if err != nil {
		f.t.Fatalf("RepositoryPath(%q): %v", repository, err)
	}
	f.executor.registerRepository(path, repository)
}

func (f *fixture) repositoryPath(repository string) string {
	f.t.Helper()
	path, err := f.storage.RepositoryPath(f.tenant, repository)
	if err != nil {
		f.t.Fatalf("RepositoryPath(%q): %v", repository, err)
	}
	return path
}

func (f *fixture) updateRequest() git.UpdateRequest {
	return git.UpdateRequest{
		ResourceID:      testResourceID,
		Repository:      "shop/web",
		Ref:             testRef,
		ExpectedBase:    testBase,
		Candidate:       testCandidate,
		ActorCredential: testCredential,
		SourceEventID:   testSourceEventID,
	}
}
