package trigger_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// twoBindingCandidates returns one pipeline as two candidates, one per binding
// of repo-1, in reader order: the binding of another enforcing service sorts
// first, the binding of the reporting Git service second.
func twoBindingCandidates() (trigger.Candidate, trigger.Candidate) {
	scope := testScope()
	definition := testDefinition("pipeline-a", pushYAML("repo-1", "svc-a", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "svc-a", []string{"refs/heads/main"})
	other := testBinding(scope, "repo-1", "git-other", 7)
	other.ID = resource.BindingID("binding-a-other")
	reporting := testBinding(scope, "repo-1", "git-svc", 3)
	reporting.ID = resource.BindingID("binding-b-reporting")
	return testCandidate(definition, other), testCandidate(definition, reporting)
}

func bindingVersionRead(t *testing.T, unit trigger.StartUnit) authorize.AuthorityRead {
	t.Helper()
	for _, read := range unit.Authority {
		if read.Check == authorize.CheckBindingVersion {
			return read
		}
	}
	t.Fatalf("start unit carries no binding_version authority read: %+v", unit.Authority)
	return authorize.AuthorityRead{}
}

// TestRetriedStartUsesOnlyTheReportingBinding pins that a persisted selection is
// resumed only through the binding whose enforcing service reported the push.
// Another binding of the same repository must never authorize, lock or
// duplicate the retried start.
func TestRetriedStartUsesOnlyTheReportingBinding(t *testing.T) {
	fixture := newPushFixture(t)
	other, reporting := twoBindingCandidates()
	fixture.candidates.set(other, reporting)
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	attempts := 0
	fixture.start.setFn(func(unit trigger.StartUnit) (trigger.Result, error) {
		attempts++
		if attempts == 1 {
			return trigger.Result{}, fault.New(fault.DependencyUnavailable, "start store unavailable")
		}
		return fixture.start.defaultStart(unit)
	})

	first := fixture.process(t)
	if !hasEvent(first.Deferred, id.EventID("event-1")) {
		t.Fatalf("deferred = %v; a transient start failure must retain the event", first.Deferred)
	}
	second := fixture.process(t)

	if fixture.start.callCount() != 2 {
		t.Fatalf("start calls = %d; want the failed attempt and one retry", fixture.start.callCount())
	}
	for index := 0; index < fixture.start.callCount(); index++ {
		unit := fixture.start.call(index)
		read := bindingVersionRead(t, unit)
		if read.TargetID != string(reporting.Binding.ID) || read.ExpectedVersion != reporting.Binding.Version ||
			unit.ExpectedBindingVersion != reporting.Binding.Version {
			t.Fatalf("start %d authority = %s@%d (unit expects %d); want the reporting binding %s@%d",
				index, read.TargetID, read.ExpectedVersion, unit.ExpectedBindingVersion, reporting.Binding.ID, reporting.Binding.Version)
		}
	}
	retried := fixture.start.call(1).NewExecutionID
	if len(second.Started) != 1 || second.Started[0] != retried {
		t.Fatalf("started = %v; want the retried execution %s exactly once", second.Started, retried)
	}
	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; the settled event must advance", fixture.completion.sequences())
	}
}

// TestSelectionWhoseReportingBindingIsGoneIsRefused pins that a persisted
// selection never resumes through another binding of the repository: when the
// reporting binding is deleted before the retry, the selection is refused like
// any selection without a current candidate, and nothing starts.
func TestSelectionWhoseReportingBindingIsGoneIsRefused(t *testing.T) {
	fixture := newPushFixture(t)
	other, reporting := twoBindingCandidates()
	fixture.candidates.set(other, reporting)
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	attempts := 0
	fixture.start.setFn(func(unit trigger.StartUnit) (trigger.Result, error) {
		attempts++
		if attempts == 1 {
			return trigger.Result{}, fault.New(fault.DependencyUnavailable, "start store unavailable")
		}
		return fixture.start.defaultStart(unit)
	})
	first := fixture.process(t)
	if !hasEvent(first.Deferred, id.EventID("event-1")) {
		t.Fatalf("deferred = %v; a transient start failure must retain the event", first.Deferred)
	}

	fixture.candidates.set(other)
	second := fixture.process(t)

	if len(second.Started) != 0 {
		t.Fatalf("started = %v; a selection must never start through another binding", second.Started)
	}
	if !hasEvent(second.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v; the orphaned selection must be refused", second.Refused)
	}
	for index := 1; index < fixture.start.callCount(); index++ {
		unit := fixture.start.call(index)
		if unit.Status != trigger.StatusRefused {
			t.Fatalf("start %d status = %q; want only the refusal", index, unit.Status)
		}
		if unit.ExpectedBindingVersion == other.Binding.Version {
			t.Fatalf("start %d is guarded by the other binding's version", index)
		}
	}
	key := keyFor(testScope(), pipeline.PipelineID("pipeline-a"), id.EventID("event-1"))
	if stored, ok := fixture.selections.get(key); !ok || stored.Status != trigger.StatusRefused {
		t.Fatalf("stored result = %+v (ok=%v); want the durable refusal", stored, ok)
	}
	if !fixture.completion.has(1) {
		t.Fatalf("completed = %v; the refused event must advance", fixture.completion.sequences())
	}
}
