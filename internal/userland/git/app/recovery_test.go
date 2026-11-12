package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/git"
)

// recoveryEntry returns one accepted, reportable journal entry. The identity
// fields are valid fixed values: the sweep test never mints or guesses one.
func recoveryEntry(sourceEventID string) git.JournalEntry {
	return git.JournalEntry{
		Version:        git.JournalVersion,
		ResourceID:     "resource-1",
		Repository:     "repo",
		Ref:            "refs/heads/main",
		ExpectedBase:   "base-1",
		Candidate:      "candidate-1",
		SourceEventID:  sourceEventID,
		VerificationID: "verification-1",
		State:          git.JournalAccepted,
		At:             time.Unix(1700000000, 0).UTC(),
	}
}

// seedJournalEntry records one durable entry before the app is constructed, as
// a crash between acceptance and reporting leaves it.
func seedJournalEntry(t *testing.T, directory string, entry git.JournalEntry) {
	t.Helper()
	journal, err := git.NewFileJournal(directory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	if err := journal.Record(context.Background(), entry); err != nil {
		t.Fatalf("record journal entry: %v", err)
	}
}

// loadJournalEntry reads one durable entry through a fresh store, so the test
// observes the persisted state rather than the service's in-memory view.
func loadJournalEntry(t *testing.T, directory, sourceEventID string) git.JournalEntry {
	t.Helper()
	journal, err := git.NewFileJournal(directory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	entry, ok, err := journal.Load(context.Background(), sourceEventID)
	if err != nil || !ok {
		t.Fatalf("load journal entry %q = %v,%v", sourceEventID, ok, err)
	}
	return entry
}

// recoveryVerifier counts report calls and fails a scripted number of them.
type recoveryVerifier struct {
	mu          sync.Mutex
	reportCalls int
	failures    int
	reportErr   error
}

func (verifier *recoveryVerifier) Verify(context.Context, api.VerifyRequest) (api.VerifyResponse, error) {
	return api.VerifyResponse{}, nil
}

func (verifier *recoveryVerifier) Report(_ context.Context, request api.ReportRequest) (api.ReportResponse, error) {
	verifier.mu.Lock()
	verifier.reportCalls++
	fail := verifier.failures > 0
	if fail {
		verifier.failures--
	}
	reportErr := verifier.reportErr
	verifier.mu.Unlock()
	if fail {
		if reportErr == nil {
			reportErr = errors.New("report unavailable")
		}
		return api.ReportResponse{}, reportErr
	}
	return api.ReportResponse{
		VerificationID: request.VerificationID,
		EventID:        request.SourceEventID,
	}, nil
}

func (verifier *recoveryVerifier) calls() int {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	return verifier.reportCalls
}

// countingObserver records every ref observation and returns one scripted
// observed state. The sweep must never consult it: a timer has no evidence that
// an in-flight receive-pack transaction has finished.
type countingObserver struct {
	mu    sync.Mutex
	state git.RefState
	calls int
}

func (observer *countingObserver) Observe(context.Context, string, string) (git.RefState, error) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.calls++
	return observer.state, nil
}

func (observer *countingObserver) observed() int {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.calls
}

// blockingReceiveBackend blocks a git-receive-pack exchange at an explicit
// barrier so tests can exercise RecoverPending while a push is in flight and
// observe post-Serve cleanup when it finishes.
type blockingReceiveBackend struct {
	entered chan git.GitBackendRequest
	release chan struct{}
	onServe func(context.Context, git.GitBackendRequest)
}

func newBlockingReceiveBackend() *blockingReceiveBackend {
	return &blockingReceiveBackend{
		entered: make(chan git.GitBackendRequest, 1),
		release: make(chan struct{}),
	}
}

func (backend *blockingReceiveBackend) Serve(ctx context.Context, request git.GitBackendRequest, body io.Reader, response git.GitResponseWriter) error {
	if body != nil {
		_, _ = io.Copy(io.Discard, body)
	}
	if backend.onServe != nil {
		backend.onServe(ctx, request)
	}
	select {
	case backend.entered <- request:
	default:
	}
	select {
	case <-backend.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	response.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte("ok"))
	return nil
}

// TestRecoverySweepLeavesInFlightIntendedUntouchedAndRecoversIdleIntended pins
// that RecoverPending skips JournalIntended entries while a git-receive-pack
// exchange is active for their repository, reconciles idle JournalIntended
// entries once no push is in flight, and always leaves JournalUncertain entries
// untouched on a timer.
func TestRecoverySweepLeavesInFlightIntendedUntouchedAndRecoversIdleIntended(t *testing.T) {
	config := committedPolicyConfig(t, "commit-one", "repo repo\n    RW = @engineer\n")
	config.Actors = &fakeActors{identity: git.Identity{Roles: []string{"engineer"}}}
	config.Clock = fakeClock{now: time.Unix(1700000001, 0).UTC()}
	config.SourceEvent = func() (string, error) { return "push-inflight-other", nil }
	verifier := &recoveryVerifier{}
	config.Verify = verifier
	observer := &countingObserver{state: git.RefState{Ref: "refs/heads/main", Commit: "base-1"}}
	config.Observer = observer
	backend := newBlockingReceiveBackend()
	config.Backend = backend

	intended := recoveryEntry("event-sweep-intended")
	intended.State = git.JournalIntended
	// A slow push can outlast any fixed age bound, so an old intent whose
	// receive-pack is still in flight must stay untouched too: no age bound may
	// settle it and mark a push refused that receive-pack then applies.
	oldIntended := recoveryEntry("event-sweep-old-intended")
	oldIntended.State = git.JournalIntended
	oldIntended.At = time.Unix(1600000000, 0).UTC()
	uncertain := recoveryEntry("event-sweep-uncertain")
	uncertain.State = git.JournalUncertain
	accepted := recoveryEntry("event-sweep-accepted")
	for _, entry := range []git.JournalEntry{intended, oldIntended, uncertain, accepted} {
		seedJournalEntry(t, config.JournalDirectory, entry)
	}

	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = server.Close() }()

	pushDone := make(chan int, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/repo/git-receive-pack", strings.NewReader("pack"))
		request.Header.Set("Content-Type", "application/x-git-receive-pack-request")
		request.SetBasicAuth("kodama", "credential-1")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		pushDone <- recorder.Code
	}()

	select {
	case <-backend.entered:
	case <-time.After(5 * time.Second):
		close(backend.release)
		t.Fatal("git-receive-pack never reached the backend")
	}

	// While git-receive-pack is in flight on "repo", intended and uncertain
	// entries are untouched and only accepted is reported.
	if err := server.RecoverPending(context.Background()); err != nil {
		close(backend.release)
		t.Fatalf("RecoverPending while in flight: %v", err)
	}
	for _, entry := range []git.JournalEntry{intended, oldIntended, uncertain} {
		state := loadJournalEntry(t, config.JournalDirectory, entry.SourceEventID).State
		if state != entry.State {
			close(backend.release)
			t.Fatalf("journal entry %q state = %q; want %q while push is in flight", entry.SourceEventID, state, entry.State)
		}
	}
	if got := observer.observed(); got != 0 {
		close(backend.release)
		t.Fatalf("ref observations while in flight = %d; want 0", got)
	}
	if got := verifier.calls(); got != 1 {
		close(backend.release)
		t.Fatalf("report calls = %d; want 1 for the accepted entry only", got)
	}
	if state := loadJournalEntry(t, config.JournalDirectory, accepted.SourceEventID).State; state != git.JournalReported {
		close(backend.release)
		t.Fatalf("accepted journal state = %q; want reported", state)
	}

	// Finish the in-flight push; now no git-receive-pack is active on "repo".
	close(backend.release)
	if status := <-pushDone; status != http.StatusOK {
		t.Fatalf("push status = %d; want 200", status)
	}

	// With no active git-receive-pack on "repo", RecoverPending reconciles the
	// orphaned intended entry (ref == ExpectedBase -> refused, and another
	// orphaned intended entry whose ref == Candidate -> reported), while
	// uncertain stays uncertain.
	appliedIntended := recoveryEntry("event-sweep-intended-applied")
	appliedIntended.State = git.JournalIntended
	appliedIntended.Ref = "refs/heads/feature"
	appliedIntended.ExpectedBase = "base-feature"
	appliedIntended.Candidate = "candidate-feature"
	seedJournalEntry(t, config.JournalDirectory, appliedIntended)

	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("RecoverPending after push finished: %v", err)
	}
	if state := loadJournalEntry(t, config.JournalDirectory, intended.SourceEventID).State; state != git.JournalRefused {
		t.Fatalf("idle intended (at base) state = %q; want refused", state)
	}
	// appliedIntended observed "base-1" (neither base-feature nor candidate-feature) -> uncertain;
	// verify a clean candidate match settles to reported as well.
	observer.mu.Lock()
	observer.state = git.RefState{Ref: "refs/heads/release", Commit: "candidate-release"}
	observer.mu.Unlock()
	releaseIntended := recoveryEntry("event-sweep-intended-release")
	releaseIntended.State = git.JournalIntended
	releaseIntended.Ref = "refs/heads/release"
	releaseIntended.ExpectedBase = "base-release"
	releaseIntended.Candidate = "candidate-release"
	seedJournalEntry(t, config.JournalDirectory, releaseIntended)

	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("RecoverPending for applied idle intended: %v", err)
	}
	if state := loadJournalEntry(t, config.JournalDirectory, releaseIntended.SourceEventID).State; state != git.JournalReported {
		t.Fatalf("idle intended (at candidate) state = %q; want reported", state)
	}
	if got := verifier.calls(); got != 2 {
		t.Fatalf("report calls = %d; want 2 (accepted + applied idle intended)", got)
	}
	if state := loadJournalEntry(t, config.JournalDirectory, uncertain.SourceEventID).State; state != git.JournalUncertain {
		t.Fatalf("uncertain journal state = %q; want uncertain", state)
	}
}

// TestRecoverySweepPrunesExpiredTerminalEntries proves RecoverPending removes
// terminal (reported and refused) journal files older than 24h while keeping
// recent terminal entries and recovering pending ones.
func TestRecoverySweepPrunesExpiredTerminalEntries(t *testing.T) {
	now := time.Unix(1700200000, 0).UTC()
	config := wiredConfig(t)
	config.Clock = fakeClock{now: now}
	verifier := &recoveryVerifier{}
	config.Verify = verifier

	oldReported := recoveryEntry("event-prune-old-reported")
	oldReported.State = git.JournalReported
	oldReported.At = now.Add(-25 * time.Hour)

	oldRefused := recoveryEntry("event-prune-old-refused")
	oldRefused.State = git.JournalRefused
	oldRefused.At = now.Add(-25 * time.Hour)

	recentReported := recoveryEntry("event-prune-recent-reported")
	recentReported.State = git.JournalReported
	recentReported.At = now.Add(-time.Hour)

	accepted := recoveryEntry("event-prune-accepted")
	accepted.State = git.JournalAccepted
	accepted.At = now.Add(-time.Minute)

	for _, entry := range []git.JournalEntry{oldReported, oldRefused, recentReported, accepted} {
		seedJournalEntry(t, config.JournalDirectory, entry)
	}

	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = server.Close() }()

	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("RecoverPending: %v", err)
	}
	journal, err := git.NewFileJournal(config.JournalDirectory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	for _, prunedID := range []string{oldReported.SourceEventID, oldRefused.SourceEventID} {
		if _, ok, loadErr := journal.Load(context.Background(), prunedID); loadErr != nil || ok {
			t.Fatalf("expired terminal entry %q still present: ok=%v err=%v", prunedID, ok, loadErr)
		}
	}
	if state := loadJournalEntry(t, config.JournalDirectory, recentReported.SourceEventID).State; state != git.JournalReported {
		t.Fatalf("recent reported state = %q; want reported", state)
	}
	if state := loadJournalEntry(t, config.JournalDirectory, accepted.SourceEventID).State; state != git.JournalReported {
		t.Fatalf("accepted state = %q; want reported", state)
	}
}

// TestRecoverySweepReportsAcceptedEntryExactlyOnce pins the core recovery
// sweep: an accepted-but-unreported durable entry is reported by the sweep, and
// a second sweep is a settled no-op that never reports twice.
func TestRecoverySweepReportsAcceptedEntryExactlyOnce(t *testing.T) {
	config := wiredConfig(t)
	config.Clock = fakeClock{now: time.Unix(1700000001, 0).UTC()}
	verifier := &recoveryVerifier{}
	config.Verify = verifier
	entry := recoveryEntry("event-sweep-once")
	seedJournalEntry(t, config.JournalDirectory, entry)

	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = server.Close() }()

	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("RecoverPending: %v", err)
	}
	if got := verifier.calls(); got != 1 {
		t.Fatalf("report calls = %d; want 1", got)
	}
	reported := loadJournalEntry(t, config.JournalDirectory, entry.SourceEventID)
	if reported.State != git.JournalReported {
		t.Fatalf("journal state = %q; want reported", reported.State)
	}

	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("second RecoverPending: %v", err)
	}
	if got := verifier.calls(); got != 1 {
		t.Fatalf("report calls after the second sweep = %d; want 1", got)
	}
	if state := loadJournalEntry(t, config.JournalDirectory, entry.SourceEventID).State; state != git.JournalReported {
		t.Fatalf("journal state after the second sweep = %q; want reported", state)
	}
}

// TestRecoverySweepRetriesAFailedReportOnALaterPass pins that a failed report
// leaves the entry accepted and the next bounded sweep retries the same stable
// report identity.
func TestRecoverySweepRetriesAFailedReportOnALaterPass(t *testing.T) {
	config := wiredConfig(t)
	config.Clock = fakeClock{now: time.Unix(1700000001, 0).UTC()}
	verifier := &recoveryVerifier{failures: 1}
	config.Verify = verifier
	entry := recoveryEntry("event-sweep-retry")
	seedJournalEntry(t, config.JournalDirectory, entry)

	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = server.Close() }()

	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("first RecoverPending: %v", err)
	}
	if state := loadJournalEntry(t, config.JournalDirectory, entry.SourceEventID).State; state != git.JournalAccepted {
		t.Fatalf("journal state after a failed report = %q; want accepted", state)
	}
	if got := verifier.calls(); got != 1 {
		t.Fatalf("report calls after the failed report = %d; want 1", got)
	}

	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("second RecoverPending: %v", err)
	}
	if state := loadJournalEntry(t, config.JournalDirectory, entry.SourceEventID).State; state != git.JournalReported {
		t.Fatalf("journal state after the retry = %q; want reported", state)
	}
	if got := verifier.calls(); got != 2 {
		t.Fatalf("report calls after the retry = %d; want 2", got)
	}
}

// TestRecoverySweepIsBoundedPerPass pins the bounded work per tick: with a
// batch of one, one sweep reports exactly one pending entry and a later sweep
// reaches the next one instead of starving it behind the same first key.
func TestRecoverySweepIsBoundedPerPass(t *testing.T) {
	config := wiredConfig(t)
	config.Clock = fakeClock{now: time.Unix(1700000001, 0).UTC()}
	verifier := &recoveryVerifier{}
	config.Verify = verifier
	first := recoveryEntry("event-sweep-a")
	second := recoveryEntry("event-sweep-b")
	seedJournalEntry(t, config.JournalDirectory, first)
	seedJournalEntry(t, config.JournalDirectory, second)

	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = server.Close() }()
	server.recoveryBatch = 1

	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("first RecoverPending: %v", err)
	}
	if got := verifier.calls(); got != 1 {
		t.Fatalf("report calls after one bounded sweep = %d; want 1", got)
	}
	states := []git.JournalState{
		loadJournalEntry(t, config.JournalDirectory, first.SourceEventID).State,
		loadJournalEntry(t, config.JournalDirectory, second.SourceEventID).State,
	}
	reported := 0
	for _, state := range states {
		if state == git.JournalReported {
			reported++
		}
	}
	if reported != 1 {
		t.Fatalf("reported entries after one bounded sweep = %d; want 1", reported)
	}

	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("second RecoverPending: %v", err)
	}
	if got := verifier.calls(); got != 2 {
		t.Fatalf("report calls after the second bounded sweep = %d; want 2", got)
	}
	for _, entry := range []git.JournalEntry{first, second} {
		if state := loadJournalEntry(t, config.JournalDirectory, entry.SourceEventID).State; state != git.JournalReported {
			t.Fatalf("journal entry %q state = %q; want reported", entry.SourceEventID, state)
		}
	}
}

// seedSettledJournalEntries records count already settled (reported) entries
// through one journal handle. They are the noise a large receive journal
// accumulates: the sweep must skip them cheaply and never spend its recovery
// batch on them.
func seedSettledJournalEntries(t *testing.T, directory, prefix string, count int) {
	t.Helper()
	journal, err := git.NewFileJournal(directory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	for index := range count {
		entry := recoveryEntry(fmt.Sprintf("%s-%04d", prefix, index))
		entry.State = git.JournalReported
		if err := journal.Record(context.Background(), entry); err != nil {
			t.Fatalf("record settled journal entry %d: %v", index, err)
		}
	}
}

// TestRecoverySweepReachesAcceptedEntryBehindSettledEntries pins that settled
// (reported) entries must not consume the recovery batch. Otherwise an
// accepted entry whose key sorts behind more settled entries than one batch is
// unreachable for many passes, and a crash-truncated report waits hours
// instead of being retried at startup.
func TestRecoverySweepReachesAcceptedEntryBehindSettledEntries(t *testing.T) {
	config := wiredConfig(t)
	config.Clock = fakeClock{now: time.Unix(1700000001, 0).UTC()}
	verifier := &recoveryVerifier{}
	config.Verify = verifier
	// The settled keys sort before "event-sweep-target", so the accepted entry
	// sits behind more entries than the default batch.
	seedSettledJournalEntries(t, config.JournalDirectory, "event-sweep-settled", defaultRecoveryBatch+64)
	target := recoveryEntry("event-sweep-target")
	seedJournalEntry(t, config.JournalDirectory, target)

	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = server.Close() }()

	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("RecoverPending: %v", err)
	}
	if got := verifier.calls(); got != 1 {
		t.Fatalf("report calls after one sweep = %d; want 1: settled entries must not consume the recovery batch", got)
	}
	if state := loadJournalEntry(t, config.JournalDirectory, target.SourceEventID).State; state != git.JournalReported {
		t.Fatalf("accepted journal state = %q; want reported", state)
	}
}

// TestRecoverySweepRecoversSparseAcceptedEntriesOnFirstPass pins that the
// first pass after startup reaches accepted entries spread across a journal
// full of settled noise; one pass must cover the whole directory.
func TestRecoverySweepRecoversSparseAcceptedEntriesOnFirstPass(t *testing.T) {
	config := wiredConfig(t)
	config.Clock = fakeClock{now: time.Unix(1700000001, 0).UTC()}
	verifier := &recoveryVerifier{}
	config.Verify = verifier
	journal, err := git.NewFileJournal(config.JournalDirectory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	// Sparse accepted entries interleaved with settled noise; the last one is
	// behind more settled entries than one default batch.
	var accepted []git.JournalEntry
	for index := range 500 {
		entry := recoveryEntry(fmt.Sprintf("event-sweep-scan-%04d", index))
		if index%100 == 40 {
			accepted = append(accepted, entry)
		} else {
			entry.State = git.JournalReported
		}
		if err := journal.Record(context.Background(), entry); err != nil {
			t.Fatalf("record journal entry %d: %v", index, err)
		}
	}
	if len(accepted) != 5 {
		t.Fatalf("seeded accepted entries = %d; want 5", len(accepted))
	}

	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = server.Close() }()
	server.recoveryBatch = len(accepted)

	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("RecoverPending: %v", err)
	}
	if got := verifier.calls(); got != len(accepted) {
		t.Fatalf("report calls after the first sweep = %d; want %d", got, len(accepted))
	}
	for _, entry := range accepted {
		if state := loadJournalEntry(t, config.JournalDirectory, entry.SourceEventID).State; state != git.JournalReported {
			t.Fatalf("accepted journal entry %q state = %q; want reported", entry.SourceEventID, state)
		}
	}
}

// TestRecoverySweepBoundsLaterPassesToTheScanLimit pins that a periodic pass
// examines only a bounded number of entries: after the startup pass, the sweep
// must not scan an unbounded directory on every tick. It keeps progressing
// through the journal instead, so no accepted entry is starved.
func TestRecoverySweepBoundsLaterPassesToTheScanLimit(t *testing.T) {
	config := wiredConfig(t)
	config.Clock = fakeClock{now: time.Unix(1700000001, 0).UTC()}
	verifier := &recoveryVerifier{}
	config.Verify = verifier
	journal, err := git.NewFileJournal(config.JournalDirectory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	// event-a sits first and is recovered by the startup pass. event-b sorts
	// behind settled entries so the second pass must not reach it in one tick.
	first := recoveryEntry("event-a")
	if err := journal.Record(context.Background(), first); err != nil {
		t.Fatalf("record first accepted entry: %v", err)
	}
	for index := range 12 {
		entry := recoveryEntry(fmt.Sprintf("event-settled-%02d", index))
		entry.State = git.JournalReported
		if err := journal.Record(context.Background(), entry); err != nil {
			t.Fatalf("record settled entry %d: %v", index, err)
		}
	}
	second := recoveryEntry("event-z")
	if err := journal.Record(context.Background(), second); err != nil {
		t.Fatalf("record second accepted entry: %v", err)
	}

	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = server.Close() }()
	server.recoveryBatch = 1
	server.recoveryScanLimit = 3

	// The startup pass is unbounded: it reaches event-a.
	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("startup RecoverPending: %v", err)
	}
	if got := verifier.calls(); got != 1 {
		t.Fatalf("report calls after the startup sweep = %d; want 1", got)
	}
	if state := loadJournalEntry(t, config.JournalDirectory, first.SourceEventID).State; state != git.JournalReported {
		t.Fatalf("first accepted state = %q; want reported", state)
	}

	// The next periodic pass may examine only recoveryScanLimit entries, so it
	// must not reach event-z behind the settled block.
	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("bounded RecoverPending: %v", err)
	}
	if got := verifier.calls(); got != 1 {
		t.Fatalf("report calls after one bounded sweep = %d; want 1: later passes must honor the scan bound", got)
	}
	if state := loadJournalEntry(t, config.JournalDirectory, second.SourceEventID).State; state != git.JournalAccepted {
		t.Fatalf("second accepted state after one bounded sweep = %q; want accepted", state)
	}

	// Progress continues across passes until the whole journal has been swept,
	// and no accepted entry is reported twice.
	for pass := 0; pass < 8 && verifier.calls() < 2; pass++ {
		if err := server.RecoverPending(context.Background()); err != nil {
			t.Fatalf("progress RecoverPending %d: %v", pass, err)
		}
	}
	if got := verifier.calls(); got != 2 {
		t.Fatalf("report calls after the journal was swept = %d; want 2", got)
	}
	if state := loadJournalEntry(t, config.JournalDirectory, second.SourceEventID).State; state != git.JournalReported {
		t.Fatalf("second accepted state = %q; want reported", state)
	}
}

// TestRecoverySweepRecoversEveryAcceptedEntryExactlyOnceAcrossPasses pins the
// offset's round-robin fairness when more accepted entries exist than one
// batch: each later pass continues where the previous stopped.
func TestRecoverySweepRecoversEveryAcceptedEntryExactlyOnceAcrossPasses(t *testing.T) {
	config := wiredConfig(t)
	config.Clock = fakeClock{now: time.Unix(1700000001, 0).UTC()}
	verifier := &recoveryVerifier{}
	config.Verify = verifier
	journal, err := git.NewFileJournal(config.JournalDirectory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	var entries []git.JournalEntry
	for index := range 5 {
		entry := recoveryEntry(fmt.Sprintf("event-many-%d", index))
		entries = append(entries, entry)
		if err := journal.Record(context.Background(), entry); err != nil {
			t.Fatalf("record accepted entry %d: %v", index, err)
		}
	}

	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = server.Close() }()
	server.recoveryBatch = 2

	for pass := range 5 {
		before := verifier.calls()
		if err := server.RecoverPending(context.Background()); err != nil {
			t.Fatalf("RecoverPending %d: %v", pass, err)
		}
		if delta := verifier.calls() - before; delta > 2 {
			t.Fatalf("pass %d reported %d entries; want at most the batch of 2", pass, delta)
		}
	}
	if got := verifier.calls(); got != len(entries) {
		t.Fatalf("report calls after all passes = %d; want %d", got, len(entries))
	}
	for _, entry := range entries {
		if state := loadJournalEntry(t, config.JournalDirectory, entry.SourceEventID).State; state != git.JournalReported {
			t.Fatalf("accepted journal entry %q state = %q; want reported", entry.SourceEventID, state)
		}
	}
}

// cancelingVerifier cancels the sweep context on the first report, so the test
// observes that a mid-pass cancellation stops the pass before the next entry.
type cancelingVerifier struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	calls  int
}

func (verifier *cancelingVerifier) Verify(context.Context, api.VerifyRequest) (api.VerifyResponse, error) {
	return api.VerifyResponse{}, nil
}

func (verifier *cancelingVerifier) Report(_ context.Context, request api.ReportRequest) (api.ReportResponse, error) {
	verifier.mu.Lock()
	verifier.calls++
	verifier.mu.Unlock()
	verifier.cancel()
	return api.ReportResponse{VerificationID: request.VerificationID, EventID: request.SourceEventID}, nil
}

func (verifier *cancelingVerifier) count() int {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	return verifier.calls
}

// TestRecoverySweepStopsMidPassOnCancellation pins cooperative scheduling: an
// unbounded startup pass still checks the context between entries and stops at
// once when the service shuts down.
func TestRecoverySweepStopsMidPassOnCancellation(t *testing.T) {
	config := wiredConfig(t)
	config.Clock = fakeClock{now: time.Unix(1700000001, 0).UTC()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	verifier := &cancelingVerifier{cancel: cancel}
	config.Verify = verifier
	journal, err := git.NewFileJournal(config.JournalDirectory)
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	var entries []git.JournalEntry
	for index := range 5 {
		entry := recoveryEntry(fmt.Sprintf("event-cancel-%d", index))
		entries = append(entries, entry)
		if err := journal.Record(context.Background(), entry); err != nil {
			t.Fatalf("record accepted entry %d: %v", index, err)
		}
	}

	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = server.Close() }()
	server.recoveryBatch = len(entries)

	if err := server.RecoverPending(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RecoverPending error = %v; want context.Canceled", err)
	}
	if got := verifier.count(); got != 1 {
		t.Fatalf("report calls after mid-pass cancellation = %d; want 1", got)
	}
	// The canceled report may complete its server call but must not settle a
	// second entry: cancellation is observed before the next journal load.
	settled := 0
	for _, entry := range entries {
		if state := loadJournalEntry(t, config.JournalDirectory, entry.SourceEventID).State; state != git.JournalAccepted {
			settled++
		}
	}
	if settled > 1 {
		t.Fatalf("settled entries after mid-pass cancellation = %d; want at most 1", settled)
	}
}

// blockingVerifier blocks the report until its context is canceled, so the
// test can observe whether shutdown joins the sweep goroutine.
type blockingVerifier struct {
	started chan struct{}
	ended   chan struct{}
	once    sync.Once
	endOnce sync.Once
}

func (verifier *blockingVerifier) Verify(context.Context, api.VerifyRequest) (api.VerifyResponse, error) {
	return api.VerifyResponse{}, nil
}

func (verifier *blockingVerifier) Report(ctx context.Context, _ api.ReportRequest) (api.ReportResponse, error) {
	verifier.once.Do(func() { close(verifier.started) })
	<-ctx.Done()
	verifier.endOnce.Do(func() { close(verifier.ended) })
	return api.ReportResponse{}, ctx.Err()
}

// TestStartRecoverySweepsAtStartupAndJoinsOnCancellation pins the composed
// lifecycle: starting the sweep performs an immediate pass, and a canceled
// context is joined before CloseRecovery returns, even while a report is in
// flight.
func TestStartRecoverySweepsAtStartupAndJoinsOnCancellation(t *testing.T) {
	config := wiredConfig(t)
	config.Clock = fakeClock{now: time.Unix(1700000001, 0).UTC()}
	verifier := &blockingVerifier{started: make(chan struct{}), ended: make(chan struct{})}
	config.Verify = verifier
	entry := recoveryEntry("event-sweep-join")
	seedJournalEntry(t, config.JournalDirectory, entry)

	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := server.StartRecovery(ctx); err != nil {
		cancel()
		_ = server.Close()
		t.Fatalf("StartRecovery: %v", err)
	}
	select {
	case <-verifier.started:
	case <-time.After(5 * time.Second):
		cancel()
		_ = server.Close()
		t.Fatal("the startup sweep never attempted the accepted entry")
	}
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- server.CloseRecovery() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("CloseRecovery: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CloseRecovery did not join the canceled sweep")
	}
	select {
	case <-verifier.ended:
	default:
		t.Fatal("CloseRecovery returned before the in-flight report observed cancellation")
	}
	_ = server.Close()
}

// TestRecoverySweepLogsByFaultClassWithoutRawText pins the logging contract:
// a failed recovery is logged by its public fault class and never carries the
// private cause text, which may contain secrets.
func TestRecoverySweepLogsByFaultClassWithoutRawText(t *testing.T) {
	config := wiredConfig(t)
	config.Clock = fakeClock{now: time.Unix(1700000001, 0).UTC()}
	verifier := &recoveryVerifier{
		failures:  1,
		reportErr: fault.New(fault.DependencyUnavailable, "credential super-secret-token rejected"),
	}
	config.Verify = verifier
	var logs strings.Builder
	config.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	entry := recoveryEntry("event-sweep-log")
	seedJournalEntry(t, config.JournalDirectory, entry)

	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = server.Close() }()
	if err := server.RecoverPending(context.Background()); err != nil {
		t.Fatalf("RecoverPending: %v", err)
	}
	text := logs.String()
	if !strings.Contains(text, "dependency_unavailable") {
		t.Fatalf("recovery log = %q; want the dependency_unavailable fault class", text)
	}
	for _, forbidden := range []string{"super-secret-token", "credential"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("recovery log leaks %q: %q", forbidden, text)
		}
	}
}

// TestRunSweepsAcceptedJournalAtStartup proves the production Run path starts
// the recovery sweep and joins it on cancellation.
func TestRunSweepsAcceptedJournalAtStartup(t *testing.T) {
	config := wiredConfig(t)
	config.Clock = fakeClock{now: time.Unix(1700000001, 0).UTC()}
	socketBase, err := os.MkdirTemp("", "k-")
	if err != nil {
		t.Fatalf("create socket base: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketBase) })
	config.HookSocketDirectory = socketBase
	verifier := &recoveryVerifier{}
	config.Verify = verifier
	entry := recoveryEntry("event-run-startup")
	seedJournalEntry(t, config.JournalDirectory, entry)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	config.Listener = listener
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, config) }()

	deadline := time.Now().Add(5 * time.Second)
	for verifier.calls() != 1 {
		select {
		case err := <-done:
			t.Fatalf("Run returned before sweeping the accepted journal entry: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never swept the accepted journal entry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not join after cancellation")
	}
	if state := loadJournalEntry(t, config.JournalDirectory, entry.SourceEventID).State; state != git.JournalReported {
		t.Fatalf("journal state = %q; want reported", state)
	}
}

// TestNewRefusesJournalDirectoryInsideServedRepository pins the configuration
// refusal: the receive journal may never be a served repository path or lie
// inside one. The refusal happens before the journal directory is created, so a
// misconfiguration cannot wedge a later provisioning rename.
func TestNewRefusesJournalDirectoryInsideServedRepository(t *testing.T) {
	cases := []struct {
		name    string
		journal func(root string) string
		repos   map[string]string
	}{
		{
			name:    "admin repository itself",
			journal: func(root string) string { return filepath.Join(root, "tenant-1", "admin") },
		},
		{
			name:    "inside the admin repository",
			journal: func(root string) string { return filepath.Join(root, "tenant-1", "admin", "journal") },
		},
		{
			name:    "served repository itself",
			journal: func(root string) string { return filepath.Join(root, "tenant-1", "repo") },
		},
		{
			name:    "inside a nested served repository",
			journal: func(root string) string { return filepath.Join(root, "tenant-1", "shop", "web", "journal") },
			repos:   map[string]string{"shop/web": "resource-2"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			config := wiredConfig(t)
			root := config.StorageRoot
			config.JournalDirectory = test.journal(root)
			if test.repos != nil {
				config.ResourceIDs = test.repos
			}
			_, err := New(config)
			if !errors.Is(err, ErrJournalInRepository) {
				t.Fatalf("New error = %v; want ErrJournalInRepository", err)
			}
			if _, statErr := os.Stat(config.JournalDirectory); !os.IsNotExist(statErr) {
				t.Fatalf("refused journal directory state = %v; want it not created", statErr)
			}
		})
	}
}

// TestNewAllowsJournalBesideTheTenantRepositories is the positive control: a
// journal directory that is a sibling of the served repositories is accepted.
func TestNewAllowsJournalBesideTheTenantRepositories(t *testing.T) {
	config := wiredConfig(t)
	config.JournalDirectory = filepath.Join(config.StorageRoot, "tenant-1", "journal")
	server, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = server.Close() }()
	if info, statErr := os.Stat(config.JournalDirectory); statErr != nil || !info.IsDir() {
		t.Fatalf("journal directory state = %v; want the directory New created", statErr)
	}
}
