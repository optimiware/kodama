//go:build integration

package recovery_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
	recoveryfix "github.com/optimiware/kodama/test/support/boundaries/recovery"
)

// TestRecoverJournalLoadFailureKeepsTheEntry proves a corrupt durable entry is a
// load error, not a missing entry: recovery reports nothing and a repaired
// journal reports exactly once.
func TestRecoverJournalLoadFailureKeepsTheEntry(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "corrupt-journal")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no durable intent")
	}
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	restore := harness.CorruptJournalEntry(t, entry.SourceEventID)
	if _, err := harness.Service().Recover(context.Background(), entry.SourceEventID); err == nil {
		t.Fatal("recovery with a corrupt journal entry returned no error")
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}
	restore()

	final := recoverEntry(t, harness, entry.SourceEventID)
	if final.Entry.State != git.JournalReported || !final.Reported {
		t.Fatalf("recovery after repair = %+v; want reported", final)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after repair = %d; want exactly %d", got, baselineEvents+1)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestRecoverAcceptanceWriteFailureStaysUncertain proves a failed acceptance
// write is uncertainty, never a claimed acceptance or report.
func TestRecoverAcceptanceWriteFailureStaysUncertain(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "unwritable-acceptance")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no durable intent")
	}
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	restore := harness.MakeJournalUnwritable(t)
	if _, err := harness.Service().Recover(context.Background(), entry.SourceEventID); !errors.Is(err, git.ErrUncertain) {
		t.Fatalf("recovery with an unwritable journal = %v; want ErrUncertain", err)
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "intended" {
		t.Fatalf("raw journal state after the failed acceptance write = %q; want intended", state)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}
	restore()

	final := recoverEntry(t, harness, entry.SourceEventID)
	if final.Entry.State != git.JournalReported || !final.Reported {
		t.Fatalf("recovery after the journal returned = %+v; want reported", final)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after recovery = %d; want exactly %d", got, baselineEvents+1)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestRecoverReportSaveFailureKeepsAccepted proves a committed report whose
// durable `reported` write fails leaves the raw entry accepted; the next pass
// replays the original result with exactly one event.
func TestRecoverReportSaveFailureKeepsAccepted(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()

	// The original push records acceptance and then its report fails.
	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureStatus, Status: 503})
	candidate := harness.Commit(t, directory, "accepted-report-write")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil || entry.State != git.JournalAccepted {
		t.Fatalf("journal entry = %+v; want accepted", entry)
	}
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	restore := harness.MakeJournalUnwritable(t)
	if _, err := harness.Service().Recover(context.Background(), entry.SourceEventID); err == nil {
		t.Fatal("recovery with an unwritable journal returned no error")
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "accepted" {
		t.Fatalf("raw journal state = %q; want accepted", state)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count = %d; want %d: the report committed before the write failed", got, baselineEvents+1)
	}
	restore()

	final := recoverEntry(t, harness, entry.SourceEventID)
	if final.Entry.State != git.JournalReported || !final.Reported || !final.Report.Replayed {
		t.Fatalf("recovery after the journal returned = %+v; want the original result replayed", final)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after the replay = %d; want exactly %d", got, baselineEvents+1)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestRecoverMalformedReportResponseKeepsAccepted proves a malformed public
// report response is a dependency failure: the entry stays accepted, nothing is
// reported, and a later pass reports exactly once.
func TestRecoverMalformedReportResponseKeepsAccepted(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "malformed-report")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no durable intent")
	}
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	harness.Contract.QueueReportFailure(idfix.Failure{Mode: idfix.FailureExtraField})
	if _, err := harness.Service().Recover(context.Background(), entry.SourceEventID); err == nil {
		t.Fatal("recovery with a malformed report response returned no error")
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "accepted" {
		t.Fatalf("raw journal state = %q; want accepted", state)
	}
	// The fixture commits the report before corrupting the response, so exactly
	// one event exists even though the client refused it.
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents+1)
	}
	events := harness.Contract.EventIDs()
	committed := events[len(events)-1]

	final := recoverEntry(t, harness, entry.SourceEventID)
	if final.Entry.State != git.JournalReported || !final.Reported {
		t.Fatalf("recovery after the malformed response = %+v; want reported", final)
	}
	if !final.Report.Replayed || final.Report.EventID != committed {
		t.Fatalf("recovery response = %+v; want the original committed result %q replayed", final.Report, committed)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after recovery = %d; want exactly %d", got, baselineEvents+1)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestPreReceiveVerificationFailureRejectsWithoutEffect proves a verification
// dependency failure rejects the push before any journal or ref effect.
func TestPreReceiveVerificationFailureRejectsWithoutEffect(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, first := seedMain(t, harness)
	baselineVerifications := harness.Contract.VerificationCount()
	baselineEvents := harness.Contract.EventCount()
	baselineEntries := len(harness.JournalEntries(t))

	harness.Contract.QueueVerifyFailure(idfix.Failure{Mode: idfix.FailureStatus, Status: 503})
	candidate := harness.Commit(t, directory, "verify-failure")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != first {
		t.Fatalf("remote main = %q; want %q: a verification failure never applies", observed, first)
	}
	if got := len(harness.JournalEntries(t)); got != baselineEntries {
		t.Fatalf("journal entries = %d; want %d", got, baselineEntries)
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications {
		t.Fatalf("verification count = %d; want %d", got, baselineVerifications)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}

	// The failure is consumed; the same push now verifies and applies.
	if stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main"); code != 0 {
		t.Fatalf("push after recovery: %s", stderr)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want %q", observed, candidate)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestPreReceiveJournalWriteFailureRejectsWithoutEffect proves a failed durable
// intent write rejects the push before the native apply.
func TestPreReceiveJournalWriteFailureRejectsWithoutEffect(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, first := seedMain(t, harness)
	baselineEntries := len(harness.JournalEntries(t))

	restore := harness.MakeJournalUnwritable(t)
	candidate := harness.Commit(t, directory, "unwritable-intent")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != first {
		t.Fatalf("remote main = %q; want %q: a failed intent write never applies", observed, first)
	}
	if got := len(harness.JournalEntries(t)); got != baselineEntries {
		t.Fatalf("journal entries = %d; want %d", got, baselineEntries)
	}
	restore()

	if stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main"); code != 0 {
		t.Fatalf("push after the journal returned: %s", stderr)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want %q", observed, candidate)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestPostReceiveHookChannelUnreachableKeepsIntentRecoverable proves the hook
// process fails closed when the private channel is unreachable: the native
// apply still happens, the intent stays recoverable and a later pass reports it.
func TestPostReceiveHookChannelUnreachableKeepsIntentRecoverable(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, _ := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()

	script := "#!/bin/sh\n" +
		"set -u\n" +
		"export KODAMA_HOOK_SOCKET=/nonexistent/kodama-hook.sock\n" +
		"exec " + shellQuote(harness.BinaryPath) + " git hook post-receive\n"
	harness.WriteHook(t, recoveryfix.Repository, "post-receive", script)

	candidate := harness.Commit(t, directory, "unreachable-post-receive")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != candidate {
		t.Fatalf("remote main = %q; want the applied %q", observed, candidate)
	}
	if !strings.Contains(stderr, nativeRejectionMessage) {
		t.Fatalf("push stderr = %q; want the fail-closed post-receive rejection", stderr)
	}
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil || entry.State != git.JournalIntended {
		t.Fatalf("journal entry = %+v; want intended", entry)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}

	harness.RestoreProductHooks(t, recoveryfix.Repository)
	final := recoverEntry(t, harness, entry.SourceEventID)
	if final.Entry.State != git.JournalReported || !final.Reported {
		t.Fatalf("recovery = %+v; want reported", final)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents+1 {
		t.Fatalf("report event count after recovery = %d; want exactly %d", got, baselineEvents+1)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestApplyObservationFailureStaysUncertain proves Apply's observer error branch
// is uncertainty, not a refusal or acceptance.
func TestApplyObservationFailureStaysUncertain(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	_, first := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	candidate := harness.CreateCommitOnTop(t, recoveryfix.Repository, first, "apply-unobservable")
	entry, err := harness.Service().Prepare(context.Background(), git.UpdateRequest{
		ResourceID:      recoveryfix.ResourceID,
		Repository:      recoveryfix.Repository,
		Ref:             "refs/heads/main",
		ExpectedBase:    first,
		Candidate:       candidate,
		ActorCredential: recoveryfix.ExecutionToken,
		SourceEventID:   "applyUnobservable1",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	restore := harness.HideRepository(t, recoveryfix.Repository)
	if _, err := harness.Service().Apply(context.Background(), entry); !errors.Is(err, git.ErrUncertain) {
		t.Fatalf("Apply without repository evidence = %v; want ErrUncertain", err)
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "uncertain" {
		t.Fatalf("raw journal state = %q; want uncertain", state)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}
	restore()

	// The CAS was never attempted, so the ref is still the base: reconciliation
	// proves the effect never happened and refuses without reporting.
	final := recoverEntry(t, harness, entry.SourceEventID)
	if final.Outcome != git.RecoveryNotApplied || final.Entry.State != git.JournalRefused || final.Reported {
		t.Fatalf("recovery after the repository returned = %+v; want refused and unreported", final)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != first {
		t.Fatalf("remote main = %q; want the base %q", observed, first)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count after reconciliation = %d; want %d", got, baselineEvents)
	}
	harness.RequireNoSecrets(t)
}

// TestApplyObservedOtherRefRefuses proves Apply refuses a ref that is neither
// the expected base nor the candidate instead of claiming acceptance.
func TestApplyObservedOtherRefRefuses(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	_, first := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	candidate := harness.CreateCommitOnTop(t, recoveryfix.Repository, first, "apply-other")
	third := harness.CreateCommitOnTop(t, recoveryfix.Repository, candidate, "apply-other-third")
	entry, err := harness.Service().Prepare(context.Background(), git.UpdateRequest{
		ResourceID:      recoveryfix.ResourceID,
		Repository:      recoveryfix.Repository,
		Ref:             "refs/heads/main",
		ExpectedBase:    first,
		Candidate:       candidate,
		ActorCredential: recoveryfix.ExecutionToken,
		SourceEventID:   "applyOther1",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", third)

	applied, err := harness.Service().Apply(context.Background(), entry)
	if !errors.Is(err, git.ErrStaleBase) {
		t.Fatalf("Apply on a third observed state = %v; want ErrStaleBase", err)
	}
	if applied.State != git.JournalRefused {
		t.Fatalf("applied state = %q; want refused", applied.State)
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "refused" {
		t.Fatalf("raw journal state = %q; want refused", state)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}
	harness.RequireNoSecrets(t)
}

// TestRecoverRefusalWriteFailureKeepsIntent proves a failed refusal write does
// not claim a terminal refusal; the intent is refused only once durable.
func TestRecoverRefusalWriteFailureKeepsIntent(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	directory, first := seedMain(t, harness)
	baselineEvents := harness.Contract.EventCount()
	unreachablePostReceive(t, harness)

	candidate := harness.Commit(t, directory, "unwritable-refusal")
	stderr, code := harness.Push(t, directory, recoveryfix.ExecutionToken, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	if entry == nil {
		t.Fatal("the push left no durable intent")
	}
	harness.SetRemoteRef(t, recoveryfix.Repository, "refs/heads/main", first)
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	restore := harness.MakeJournalUnwritable(t)
	if _, err := harness.Service().Recover(context.Background(), entry.SourceEventID); err == nil {
		t.Fatal("recovery with an unwritable journal returned no error")
	}
	if state := rawJournalState(t, harness, entry.SourceEventID); state != "intended" {
		t.Fatalf("raw journal state = %q; want intended", state)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}
	restore()

	recovery := recoverEntry(t, harness, entry.SourceEventID)
	if recovery.Outcome != git.RecoveryNotApplied || recovery.Entry.State != git.JournalRefused || recovery.Reported {
		t.Fatalf("recovery after the journal returned = %+v; want refused and unreported", recovery)
	}
	harness.RequireNoSecrets(t, stderr)
}

// TestPrepareJournalLoadFailureDoesNotVerify proves a corrupt durable entry is
// refused before verification: a failed load must not fall through to a fresh
// verification.
func TestPrepareJournalLoadFailureDoesNotVerify(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	_, first := seedMain(t, harness)
	candidate := harness.CreateCommitOnTop(t, recoveryfix.Repository, first, "prepare-corrupt")
	baselineVerifications := harness.Contract.VerificationCount()

	sourceEventID := "corruptPrepare1"
	path := filepath.Join(harness.JournalDir, sourceEventID+".json")
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatalf("write corrupt journal: %v", err)
	}
	if _, err := harness.Service().Prepare(context.Background(), git.UpdateRequest{
		ResourceID:      recoveryfix.ResourceID,
		Repository:      recoveryfix.Repository,
		Ref:             "refs/heads/main",
		ExpectedBase:    first,
		Candidate:       candidate,
		ActorCredential: recoveryfix.ExecutionToken,
		SourceEventID:   sourceEventID,
	}); err == nil {
		t.Fatal("Prepare with a corrupt journal entry returned no error")
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications {
		t.Fatalf("verification count = %d; want %d: a corrupt journal must not verify", got, baselineVerifications)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "{corrupt" {
		t.Fatalf("corrupt journal was rewritten: %q, %v", data, err)
	}
	harness.RequireNoSecrets(t)
}

// failingObserver always reports an unavailable repository.
type failingObserver struct{}

func (failingObserver) Observe(context.Context, string, string) (git.RefState, error) {
	return git.RefState{}, errors.New("repository unavailable")
}

// failingExecutor always fails before running a command.
type failingExecutor struct{}

func (failingExecutor) Run(context.Context, git.Command) (git.CommandResult, error) {
	return git.CommandResult{}, errors.New("executor unavailable")
}

// fixedStorage resolves every repository to one path.
type fixedStorage struct{}

func (fixedStorage) RepositoryPath(id.TenantID, string) (string, error) {
	return "/nonexistent/repository", nil
}

func (fixedStorage) EnsureRepository(id.TenantID, string) (git.RepositoryLocation, error) {
	return git.RepositoryLocation{Path: "/nonexistent/repository"}, nil
}

// TestObserverExecutorFailureIsAnError proves the observer's executor error
// branch is an error, never an empty observed commit.
func TestObserverExecutorFailureIsAnError(t *testing.T) {
	observer, err := gitapp.NewObserver(gitapp.ObserverConfig{
		TenantID: recoveryfix.TenantID,
		Storage:  fixedStorage{},
		Executor: failingExecutor{},
	})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	if _, err := observer.Observe(context.Background(), recoveryfix.Repository, "refs/heads/main"); err == nil {
		t.Fatal("Observe returned no error when the executor failed")
	}
}

// TestPreReceiveObservationFailureRejectsWithoutApply proves the pre-receive
// batch pre-check fails closed when the ref cannot be observed: the batch is
// rejected even though the durable intent was already journaled.
func TestPreReceiveObservationFailureRejectsWithoutApply(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	_, first := seedMain(t, harness)
	candidate := harness.CreateCommitOnTop(t, recoveryfix.Repository, first, "prereceive-unobservable")
	baselineEvents := harness.Contract.EventCount()

	localStorage, err := git.NewLocalStorage(harness.StorageRoot)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	storage, err := native.NewSafeStorage(localStorage, harness.StorageRoot, harness.Tenant)
	if err != nil {
		t.Fatalf("NewSafeStorage: %v", err)
	}
	journal, err := git.NewFileJournal(harness.JournalDir)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	executor, err := gitapp.NewExecutor(gitapp.ExecutorConfig{})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	policies, err := native.NewCommittedPolicy(native.CommittedPolicyConfig{
		RepositoryPath: harness.RepositoryPath(native.AdminRepository),
		Executor:       executor,
	})
	if err != nil {
		t.Fatalf("NewCommittedPolicy: %v", err)
	}
	verify, err := gitapp.NewVerificationClient(harness.Transport)
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}
	service, err := git.NewService(git.ServiceConfig{
		TenantID:    harness.Tenant,
		Storage:     storage,
		Policies:    policies,
		Executor:    executor,
		Observer:    failingObserver{},
		Journal:     journal,
		Verify:      verify,
		Retention:   &gitapp.RetentionClient{},
		Clock:       gitapp.SystemClock{},
		ResourceIDs: map[string]string{recoveryfix.Repository: recoveryfix.ResourceID},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	ctx := t.Context()
	handle, err := harness.Registry().IssueActor(ctx, recoveryfix.ExecutionToken)
	if err != nil {
		t.Fatalf("IssueActor: %v", err)
	}
	environment := map[string]string{
		git.EnvActorRef:    handle,
		git.EnvRepository:  recoveryfix.Repository,
		git.EnvResourceID:  recoveryfix.ResourceID,
		git.EnvSourceEvent: "source-precheck-1",
	}
	hook, err := git.NewHook(git.HookConfig{
		Service: service,
		Actors:  harness.Registry(),
		Environment: func(key string) (string, bool) {
			value, ok := environment[key]
			return value, ok
		},
	})
	if err != nil {
		t.Fatalf("NewHook: %v", err)
	}
	lines := first + " " + candidate + " refs/heads/main\n"
	result, err := hook.RunPreReceive(ctx, strings.NewReader(lines))
	if err == nil || result.Accepted() {
		t.Fatalf("pre-receive with an unobservable ref = %+v, %v; want a fail-closed rejection", result, err)
	}
	if observed := harness.RemoteRef(t, recoveryfix.Repository, "refs/heads/main"); observed != first {
		t.Fatalf("remote main = %q; want %q", observed, first)
	}
	if got := harness.Contract.EventCount(); got != baselineEvents {
		t.Fatalf("report event count = %d; want %d", got, baselineEvents)
	}
	entry := entryForCandidate(t, harness, "refs/heads/main", candidate)
	// The pre-receive rejection means git applies nothing, so the intent this
	// call created is settled as refused instead of lingering as an orphan.
	if entry == nil || entry.State != git.JournalRefused {
		t.Fatalf("journal entry = %+v; want the rejected pre-receive intent settled as refused", entry)
	}
	harness.RequireNoSecrets(t)
}

// cancelingPolicy returns the current rules and then cancels the request
// context, so the next verification step observes a canceled request.
type cancelingPolicy struct {
	inner  git.PolicySource
	cancel context.CancelFunc
}

func (policy cancelingPolicy) Current(ctx context.Context) (git.Config, error) {
	config, err := policy.inner.Current(ctx)
	if err == nil {
		policy.cancel()
	}
	return config, err
}

// TestPrepareCancellationBeforeVerificationStopsVerification proves the
// verification client honors the request context: cancellation after the policy
// read and before verification must not create a verification record.
func TestPrepareCancellationBeforeVerificationStopsVerification(t *testing.T) {
	harness := newHarness(t, recoveryfix.Options{})
	_, first := seedMain(t, harness)
	candidate := harness.CreateCommitOnTop(t, recoveryfix.Repository, first, "cancel-before-verify")
	baselineVerifications := harness.Contract.VerificationCount()
	baselineEntries := len(harness.JournalEntries(t))

	localStorage, err := git.NewLocalStorage(harness.StorageRoot)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	storage, err := native.NewSafeStorage(localStorage, harness.StorageRoot, harness.Tenant)
	if err != nil {
		t.Fatalf("NewSafeStorage: %v", err)
	}
	journal, err := git.NewFileJournal(harness.JournalDir)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	executor, err := gitapp.NewExecutor(gitapp.ExecutorConfig{})
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	policies, err := native.NewCommittedPolicy(native.CommittedPolicyConfig{
		RepositoryPath: harness.RepositoryPath(native.AdminRepository),
		Executor:       executor,
	})
	if err != nil {
		t.Fatalf("NewCommittedPolicy: %v", err)
	}
	observer, err := gitapp.NewObserver(gitapp.ObserverConfig{
		TenantID: harness.Tenant, Storage: storage, Executor: executor,
	})
	if err != nil {
		t.Fatalf("NewObserver: %v", err)
	}
	verify, err := gitapp.NewVerificationClient(harness.Transport)
	if err != nil {
		t.Fatalf("NewVerificationClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service, err := git.NewService(git.ServiceConfig{
		TenantID:    harness.Tenant,
		Storage:     storage,
		Policies:    cancelingPolicy{inner: policies, cancel: cancel},
		Executor:    executor,
		Observer:    observer,
		Journal:     journal,
		Verify:      verify,
		Retention:   &gitapp.RetentionClient{},
		Clock:       gitapp.SystemClock{},
		ResourceIDs: map[string]string{recoveryfix.Repository: recoveryfix.ResourceID},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := service.Prepare(ctx, git.UpdateRequest{
		ResourceID:      recoveryfix.ResourceID,
		Repository:      recoveryfix.Repository,
		Ref:             "refs/heads/main",
		ExpectedBase:    first,
		Candidate:       candidate,
		ActorCredential: recoveryfix.ExecutionToken,
		SourceEventID:   "cancelAfterPolicy1",
	}); err == nil {
		t.Fatal("Prepare with a request canceled before verification returned no error")
	}
	if got := harness.Contract.VerificationCount(); got != baselineVerifications {
		t.Fatalf("verification count = %d; want %d: a canceled request must not verify", got, baselineVerifications)
	}
	if got := len(harness.JournalEntries(t)); got != baselineEntries {
		t.Fatalf("journal entries = %d; want %d", got, baselineEntries)
	}
	harness.RequireNoSecrets(t)
}
