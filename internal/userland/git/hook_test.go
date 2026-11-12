package git_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	git "github.com/optimiware/kodama/internal/userland/git"
)

var zeroObjectID = strings.Repeat("0", 40)

var (
	hookBase      = strings.Repeat("a", 40)
	hookCandidate = strings.Repeat("b", 40)
	hookMoved     = strings.Repeat("c", 40)
	hookMain      = strings.Repeat("d", 40)
	hookA         = strings.Repeat("1", 40)
	hookB         = strings.Repeat("2", 40)
)

type hookFixture struct {
	*fixture
	handles *fakeActorHandles
	env     map[string]string
	hook    *git.Hook
}

func newHookFixture(t *testing.T) *hookFixture {
	t.Helper()
	base := newFixture(t)
	handles := newFakeActorHandles()
	handle, err := handles.IssueActor(context.Background(), testCredential)
	if err != nil {
		t.Fatalf("IssueActor: %v", err)
	}
	environment := map[string]string{
		git.EnvActorRef:    handle,
		git.EnvRepository:  "shop/web",
		git.EnvResourceID:  testResourceID,
		git.EnvSourceEvent: "push-1",
	}
	hook, err := git.NewHook(git.HookConfig{
		Service: base.service,
		Actors:  handles,
		Environment: func(key string) (string, bool) {
			value, ok := environment[key]
			return value, ok
		},
	})
	if err != nil {
		t.Fatalf("NewHook: %v", err)
	}
	return &hookFixture{fixture: base, handles: handles, env: environment, hook: hook}
}

func lastJournalState(t *testing.T, f *hookFixture) git.JournalState {
	t.Helper()
	entries := f.journal.recorded()
	if len(entries) == 0 {
		t.Fatal("no journal entries recorded")
	}
	return entries[len(entries)-1].State
}

// assertNoSettledEffect proves a rejected or pre-effect batch recorded no
// acceptance and no report.
func assertNoSettledEffect(t *testing.T, f *hookFixture) {
	t.Helper()
	for _, entry := range f.journal.current() {
		if entry.State == git.JournalAccepted || entry.State == git.JournalReported {
			t.Fatalf("journal entry settled as %q; want no acceptance or report", entry.State)
		}
	}
}

func TestHookUpdateAcceptsVerifiedUpdate(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, hookBase)
	f.executor.setAncestor("shop/web", hookBase, hookCandidate, true)

	result, err := f.hook.RunUpdate(context.Background(), testRef, hookBase, hookCandidate)
	if err != nil {
		t.Fatalf("RunUpdate: %v", err)
	}
	if result.Decision != git.DecisionAllow || result.ExitCode != 0 {
		t.Fatalf("result = %+v; want accept/0", result)
	}
	if got := f.executor.commit("shop/web", testRef); got != hookCandidate {
		t.Fatalf("ref = %q; want %q", got, hookCandidate)
	}
	if calls := f.verifier.verifyRequests(); len(calls) != 1 {
		t.Fatalf("verification calls = %d; want 1", len(calls))
	}
	if state := lastJournalState(t, f); state != git.JournalReported {
		t.Fatalf("journal state = %q; want reported", state)
	}
}

func TestHookUpdateRejectsMovedRef(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, hookMoved)
	f.executor.setAncestor("shop/web", hookBase, hookCandidate, true)

	result, err := f.hook.RunUpdate(context.Background(), testRef, hookBase, hookCandidate)
	if err == nil {
		t.Fatal("RunUpdate accepted a moved ref")
	}
	if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
		t.Fatalf("result = %+v; want reject/1", result)
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatalf("update-ref commands = %d; want 0", f.executor.updateRefCount())
	}
	if state := lastJournalState(t, f); state != git.JournalRefused {
		t.Fatalf("journal state = %q; want refused", state)
	}
}

func TestHookUpdateRejectsDelete(t *testing.T) {
	f := newHookFixture(t)
	result, err := f.hook.RunUpdate(context.Background(), testRef, hookBase, zeroObjectID)
	if err == nil {
		t.Fatal("RunUpdate accepted a delete the journal cannot represent")
	}
	if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
		t.Fatalf("result = %+v; want reject/1", result)
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatal("delete reached the executor")
	}
	if calls := f.verifier.verifyRequests(); len(calls) != 0 {
		t.Fatal("delete reached verification")
	}
}

func TestHookUpdateRejectsMalformedInput(t *testing.T) {
	f := newHookFixture(t)
	cases := []struct {
		name string
		ref  string
		old  string
		new  string
	}{
		{"unanchored ref", "heads/main", hookBase, hookCandidate},
		{"notes ref", "refs/notes/x", hookBase, hookCandidate},
		{"bad old oid", testRef, "not-an-oid", hookCandidate},
		{"bad new oid", testRef, hookBase, "not-an-oid"},
		{"both zero", testRef, zeroObjectID, zeroObjectID},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := f.hook.RunUpdate(context.Background(), testCase.ref, testCase.old, testCase.new)
			if err == nil {
				t.Fatal("malformed update was accepted")
			}
			if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
				t.Fatalf("result = %+v; want reject/1", result)
			}
			if f.executor.updateRefCount() != 0 {
				t.Fatal("malformed update reached the executor")
			}
		})
	}
}

func TestHookUpdateRejectsDependencyFailure(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.fail(errors.New("verification unavailable"))
	f.executor.setRef("shop/web", testRef, hookBase)
	f.executor.setAncestor("shop/web", hookBase, hookCandidate, true)

	result, err := f.hook.RunUpdate(context.Background(), testRef, hookBase, hookCandidate)
	if err == nil {
		t.Fatal("RunUpdate accepted an unverified update")
	}
	if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
		t.Fatalf("result = %+v; want reject/1", result)
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatal("unverified update reached the executor")
	}
}

func TestHookUpdateRejectsUncertainOutcome(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, hookBase)
	f.executor.setAncestor("shop/web", hookBase, hookCandidate, true)
	f.executor.failAfter("update-ref", errors.New("response lost after the ref command"))

	result, err := f.hook.RunUpdate(context.Background(), testRef, hookBase, hookCandidate)
	if err == nil {
		t.Fatal("RunUpdate accepted an uncertain effect")
	}
	if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
		t.Fatalf("result = %+v; want reject/1", result)
	}
	if state := lastJournalState(t, f); state != git.JournalUncertain {
		t.Fatalf("journal state = %q; want uncertain", state)
	}
}

func TestHookPreReceiveBatchAccepted(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	stdin := strings.Join([]string{
		zeroObjectID + " " + hookA + " refs/heads/work/a",
		zeroObjectID + " " + hookB + " refs/heads/work/b",
		"",
	}, "\n")

	result, err := f.hook.RunPreReceive(context.Background(), strings.NewReader(stdin))
	if err != nil {
		t.Fatalf("RunPreReceive: %v", err)
	}
	if result.Decision != git.DecisionAllow || result.ExitCode != 0 {
		t.Fatalf("result = %+v; want accept/0", result)
	}
	// The pre-receive adapter takes no ref effect and sends no report: the
	// native receive-pack transaction is the atomic boundary.
	if f.executor.updateRefCount() != 0 {
		t.Fatalf("update-ref commands = %d; want 0 before the native transaction", f.executor.updateRefCount())
	}
	if reports := f.verifier.reportRequests(); len(reports) != 0 {
		t.Fatalf("reports = %d; want 0 before the native transaction", len(reports))
	}
	if calls := f.verifier.verifyRequests(); len(calls) != 2 {
		t.Fatalf("verification calls = %d; want every ref verified", len(calls))
	}
	for _, entry := range f.journal.current() {
		if entry.State != git.JournalIntended {
			t.Fatalf("journal state = %q; want intended", entry.State)
		}
	}

	// The native receive-pack transaction applies the whole batch; the
	// post-receive adapter then records acceptance and reports.
	f.executor.setRef("shop/web", "refs/heads/work/a", hookA)
	f.executor.setRef("shop/web", "refs/heads/work/b", hookB)
	result, err = f.hook.RunPostReceive(context.Background(), strings.NewReader(stdin))
	if err != nil {
		t.Fatalf("RunPostReceive: %v", err)
	}
	if result.Decision != git.DecisionAllow || result.ExitCode != 0 {
		t.Fatalf("post-receive result = %+v; want accept/0", result)
	}
	if reports := f.verifier.reportRequests(); len(reports) != 2 {
		t.Fatalf("reports = %d; want 2 after acceptance", len(reports))
	}
	if got := f.executor.commit("shop/web", "refs/heads/work/a"); got != hookA {
		t.Fatalf("work/a = %q", got)
	}
	if got := f.executor.commit("shop/web", "refs/heads/work/b"); got != hookB {
		t.Fatalf("work/b = %q", got)
	}
	for _, entry := range f.journal.current() {
		if entry.State != git.JournalReported {
			t.Fatalf("journal state = %q; want reported", entry.State)
		}
	}
}

func TestHookPreReceiveBatchIsAtomic(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, hookBase)
	f.executor.setAncestor("shop/web", hookBase, hookMain, false)
	stdin := strings.Join([]string{
		zeroObjectID + " " + hookA + " refs/heads/work/a",
		hookBase + " " + hookMain + " refs/heads/main",
		"",
	}, "\n")

	result, err := f.hook.RunPreReceive(context.Background(), strings.NewReader(stdin))
	if err == nil {
		t.Fatal("RunPreReceive accepted a batch with a refused ref")
	}
	if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
		t.Fatalf("result = %+v; want reject/1", result)
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatalf("update-ref commands = %d; want 0 (atomic batch)", f.executor.updateRefCount())
	}
	if reports := f.verifier.reportRequests(); len(reports) != 0 {
		t.Fatalf("reports = %d; want 0", len(reports))
	}
	assertNoSettledEffect(t, f)
	for _, entry := range f.journal.current() {
		if entry.State != git.JournalRefused {
			t.Fatalf("rejected batch left journal entry in state %q; want refused", entry.State)
		}
	}
}

func TestHookPreReceiveBatchRejectsStaleRefWithoutEffects(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", "refs/heads/work/b", hookMoved)
	stdin := strings.Join([]string{
		zeroObjectID + " " + hookA + " refs/heads/work/a",
		zeroObjectID + " " + hookB + " refs/heads/work/b",
		"",
	}, "\n")

	result, err := f.hook.RunPreReceive(context.Background(), strings.NewReader(stdin))
	if err == nil {
		t.Fatal("RunPreReceive accepted a batch with a moved ref")
	}
	if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
		t.Fatalf("result = %+v; want reject/1", result)
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatalf("update-ref commands = %d; want 0", f.executor.updateRefCount())
	}
	if reports := f.verifier.reportRequests(); len(reports) != 0 {
		t.Fatalf("reports = %d; want 0", len(reports))
	}
	assertNoSettledEffect(t, f)
	for _, entry := range f.journal.current() {
		if entry.State != git.JournalRefused {
			t.Fatalf("stale-base precheck left journal entry in state %q; want refused", entry.State)
		}
	}
}

func TestHookPostReceiveContinuesPastFirstRefFailure(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	stdin := strings.Join([]string{
		zeroObjectID + " " + hookA + " refs/heads/work/a",
		zeroObjectID + " " + hookB + " refs/heads/work/b",
		"",
	}, "\n")

	if result, err := f.hook.RunPreReceive(context.Background(), strings.NewReader(stdin)); err != nil || !result.Accepted() {
		t.Fatalf("RunPreReceive = %+v, %v; want accept", result, err)
	}
	f.executor.setRef("shop/web", "refs/heads/work/a", hookA)
	f.executor.setRef("shop/web", "refs/heads/work/b", hookB)
	f.verifier.failNextReports(1)

	result, err := f.hook.RunPostReceive(context.Background(), strings.NewReader(stdin))
	if err == nil {
		t.Fatal("RunPostReceive hid the first ref report failure")
	}
	if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
		t.Fatalf("result = %+v; want reject/1", result)
	}
	if reports := f.verifier.reportRequests(); len(reports) != 2 {
		t.Fatalf("report attempts = %d; want 2 (must continue past first ref failure)", len(reports))
	}
	byRef := make(map[string]git.JournalState)
	for _, entry := range f.journal.current() {
		byRef[entry.Ref] = entry.State
	}
	if byRef["refs/heads/work/a"] != git.JournalAccepted {
		t.Fatalf("work/a state = %q; want accepted for later retry", byRef["refs/heads/work/a"])
	}
	if byRef["refs/heads/work/b"] != git.JournalReported {
		t.Fatalf("work/b state = %q; want reported", byRef["refs/heads/work/b"])
	}
}

func TestHookPreReceiveDuplicateDeliverySettlesReportedEntry(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	stdin := strings.Join([]string{
		zeroObjectID + " " + hookA + " refs/heads/work/a",
		zeroObjectID + " " + hookB + " refs/heads/work/b",
		"",
	}, "\n")

	if result, err := f.hook.RunPreReceive(context.Background(), strings.NewReader(stdin)); err != nil || !result.Accepted() {
		t.Fatalf("first pre-receive = %+v, %v; want accept", result, err)
	}
	f.executor.setRef("shop/web", "refs/heads/work/a", hookA)
	f.executor.setRef("shop/web", "refs/heads/work/b", hookB)
	if result, err := f.hook.RunPostReceive(context.Background(), strings.NewReader(stdin)); err != nil || !result.Accepted() {
		t.Fatalf("post-receive = %+v, %v; want accept", result, err)
	}
	verifyCalls := len(f.verifier.verifyRequests())
	reportCalls := len(f.verifier.reportRequests())

	// A redelivery of the same push keeps the same identity and settles the
	// already-reported entry without verifying or reporting twice.
	result, err := f.hook.RunPreReceive(context.Background(), strings.NewReader(stdin))
	if err != nil || !result.Accepted() {
		t.Fatalf("duplicate pre-receive = %+v, %v; want accept", result, err)
	}
	if calls := len(f.verifier.verifyRequests()); calls != verifyCalls {
		t.Fatalf("verification calls = %d; want %d", calls, verifyCalls)
	}
	if calls := len(f.verifier.reportRequests()); calls != reportCalls {
		t.Fatalf("report calls = %d; want %d", calls, reportCalls)
	}
}

func TestHookPreReceiveStoredIntentIsReconciledFromObservation(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	stdin := zeroObjectID + " " + hookA + " refs/heads/work/a\n"

	if result, err := f.hook.RunPreReceive(context.Background(), strings.NewReader(stdin)); err != nil || !result.Accepted() {
		t.Fatalf("first pre-receive = %+v, %v; want accept", result, err)
	}
	verifyCalls := len(f.verifier.verifyRequests())
	// The refs are still at the expected base, but the credential is revoked.
	// A redelivery must reconcile the stored intent from observation only and
	// must never apply its stored compare-and-swap from stale verification.
	f.verifier.fail(errors.New("credential revoked"))

	result, err := f.hook.RunPreReceive(context.Background(), strings.NewReader(stdin))
	if err == nil {
		t.Fatal("a stored intent was accepted from stale verification")
	}
	if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
		t.Fatalf("result = %+v; want reject/1", result)
	}
	if calls := len(f.verifier.verifyRequests()); calls != verifyCalls {
		t.Fatalf("verification calls = %d; want %d", calls, verifyCalls)
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatalf("update-ref commands = %d; want 0", f.executor.updateRefCount())
	}
	if reports := len(f.verifier.reportRequests()); reports != 0 {
		t.Fatalf("reports = %d; want 0", reports)
	}
	for _, entry := range f.journal.current() {
		if entry.State != git.JournalRefused {
			t.Fatalf("journal state = %q; want refused", entry.State)
		}
	}
}

func TestHookPreReceiveStoredIntentObservedCandidateIsAccepted(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	stdin := strings.Join([]string{
		zeroObjectID + " " + hookA + " refs/heads/work/a",
		zeroObjectID + " " + hookB + " refs/heads/work/b",
		"",
	}, "\n")

	if result, err := f.hook.RunPreReceive(context.Background(), strings.NewReader(stdin)); err != nil || !result.Accepted() {
		t.Fatalf("first pre-receive = %+v, %v; want accept", result, err)
	}
	verifyCalls := len(f.verifier.verifyRequests())
	// The native transaction applied the batch, but post-receive has not run.
	// A redelivery reconciles from the observed candidate only; the revoked
	// credential is never consulted.
	f.executor.setRef("shop/web", "refs/heads/work/a", hookA)
	f.executor.setRef("shop/web", "refs/heads/work/b", hookB)
	f.verifier.fail(errors.New("credential revoked"))

	result, err := f.hook.RunPreReceive(context.Background(), strings.NewReader(stdin))
	if err != nil || !result.Accepted() {
		t.Fatalf("duplicate pre-receive = %+v, %v; want accept from observation", result, err)
	}
	if calls := len(f.verifier.verifyRequests()); calls != verifyCalls {
		t.Fatalf("verification calls = %d; want %d", calls, verifyCalls)
	}
	if reports := len(f.verifier.reportRequests()); reports != 2 {
		t.Fatalf("reports = %d; want 2", reports)
	}
	for _, entry := range f.journal.current() {
		if entry.State != git.JournalReported {
			t.Fatalf("journal state = %q; want reported", entry.State)
		}
	}
}

func TestHookPreReceiveRejectsMalformedLines(t *testing.T) {
	f := newHookFixture(t)
	cases := map[string]string{
		"missing field":      hookA + " refs/heads/work/a\n",
		"extra field":        zeroObjectID + " " + hookA + " refs/heads/work/a extra\n",
		"bad oid":            "not-an-oid " + hookA + " refs/heads/work/a\n",
		"bad ref":            zeroObjectID + " " + hookA + " heads/work/a\n",
		"outside heads/tags": zeroObjectID + " " + hookA + " refs/notes/x\n",
		"control ref":        zeroObjectID + " " + hookA + " refs/heads/bad\x00ref\n",
		"empty line":         "\n",
	}
	for name, stdin := range cases {
		t.Run(name, func(t *testing.T) {
			result, err := f.hook.RunPreReceive(context.Background(), strings.NewReader(stdin))
			if err == nil {
				t.Fatal("malformed batch was accepted")
			}
			if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
				t.Fatalf("result = %+v; want reject/1", result)
			}
			if f.executor.updateRefCount() != 0 {
				t.Fatal("malformed batch reached the executor")
			}
		})
	}
}

func TestHookPreReceiveRejectsDelete(t *testing.T) {
	f := newHookFixture(t)
	stdin := hookBase + " " + zeroObjectID + " " + testRef + "\n"
	result, err := f.hook.RunPreReceive(context.Background(), strings.NewReader(stdin))
	if err == nil {
		t.Fatal("delete batch was accepted")
	}
	if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
		t.Fatalf("result = %+v; want reject/1", result)
	}
	if f.executor.updateRefCount() != 0 {
		t.Fatal("delete reached the executor")
	}
}

func TestHookRejectsMalformedEnvironment(t *testing.T) {
	cases := []map[string]string{
		{},
		{git.EnvActorRef: "unknown-handle", git.EnvRepository: "shop/web", git.EnvResourceID: testResourceID},
		{git.EnvActorRef: "handle-1", git.EnvRepository: "../escape", git.EnvResourceID: testResourceID},
		{git.EnvActorRef: "handle-1", git.EnvRepository: "shop/web", git.EnvResourceID: "bad id"},
	}
	for index, environment := range cases {
		base := newFixture(t)
		handles := newFakeActorHandles()
		if _, err := handles.IssueActor(context.Background(), testCredential); err != nil {
			t.Fatalf("case %d: IssueActor: %v", index, err)
		}
		hook, err := git.NewHook(git.HookConfig{
			Service: base.service,
			Actors:  handles,
			Environment: func(key string) (string, bool) {
				value, ok := environment[key]
				return value, ok
			},
		})
		if err != nil {
			t.Fatalf("case %d: NewHook: %v", index, err)
		}
		result, err := hook.RunUpdate(context.Background(), testRef, hookBase, hookCandidate)
		if err == nil {
			t.Fatalf("case %d: malformed environment was accepted", index)
		}
		if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
			t.Fatalf("case %d: result = %+v; want reject/1", index, result)
		}
		if base.executor.updateRefCount() != 0 {
			t.Fatalf("case %d: malformed environment reached the executor", index)
		}
	}
}

func TestHookNoCredentialInEnvironmentOrMessage(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, hookMoved)
	f.executor.setAncestor("shop/web", hookBase, hookCandidate, true)

	result, _ := f.hook.RunUpdate(context.Background(), testRef, hookBase, hookCandidate)
	if strings.Contains(result.Message, testCredential) {
		t.Fatal("credential leaked in the hook message")
	}
	if strings.Contains(result.Message, "\n") {
		t.Fatalf("hook message is not one line: %q", result.Message)
	}
	if f.env[git.EnvActorRef] == testCredential {
		t.Fatal("raw credential placed in the backend environment")
	}
	for _, entry := range f.journal.recorded() {
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal journal entry: %v", err)
		}
		if strings.Contains(string(encoded), testCredential) {
			t.Fatalf("credential persisted in the journal: %s", encoded)
		}
	}
}

func TestHookConcurrentUpdatesToOneRefOneWins(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, hookBase)
	f.executor.setAncestor("shop/web", hookBase, hookA, true)
	f.executor.setAncestor("shop/web", hookBase, hookB, true)

	candidates := []string{hookA, hookB}
	results := make([]git.HookResult, len(candidates))
	errs := make([]error, len(candidates))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range candidates {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			results[index], errs[index] = f.hook.RunUpdate(context.Background(), testRef, hookBase, candidates[index])
		}(index)
	}
	close(start)
	wait.Wait()

	accepted := -1
	for index, result := range results {
		if result.Decision == git.DecisionAllow {
			if accepted >= 0 {
				t.Fatalf("both concurrent updates were accepted: %+v", results)
			}
			if errs[index] != nil {
				t.Fatalf("accepted update returned error: %v", errs[index])
			}
			accepted = index
		}
	}
	if accepted < 0 {
		t.Fatalf("no concurrent update was accepted: %+v", results)
	}
	if got := f.executor.commit("shop/web", testRef); got != candidates[accepted] {
		t.Fatalf("ref = %q; want the accepted candidate %q", got, candidates[accepted])
	}
	if f.executor.updateRefCount() != 1 {
		t.Fatalf("update-ref commands = %d; want 1", f.executor.updateRefCount())
	}
}

func TestNewHookRequiresBoundaries(t *testing.T) {
	if _, err := git.NewHook(git.HookConfig{}); err == nil {
		t.Fatal("NewHook accepted an empty config")
	}
}
