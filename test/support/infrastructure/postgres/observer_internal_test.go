package pgtest

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestDiffUnitSnapshotsDetectsEveryChangeKind(t *testing.T) {
	before := map[string]map[string]string{
		"tasks": {"t1": "a", "t2": "b"},
		"audit": {"a1": "x"},
	}
	after := map[string]map[string]string{
		"tasks": {"t1": "a", "t2": "changed", "t3": "new"},
		"audit": {},
	}
	changed := diffUnitSnapshots(before, after)
	want := map[string]map[string]bool{
		"tasks": {"t2": true, "t3": true},
		"audit": {"a1": true},
	}
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("diff = %v; want %v", changed, want)
	}
}

func TestDiffUnitSnapshotsIgnoresUnchangedRows(t *testing.T) {
	snapshot := map[string]map[string]string{"tasks": {"t1": "a"}}
	if changed := diffUnitSnapshots(snapshot, snapshot); len(changed) != 0 {
		t.Fatalf("unchanged snapshot produced changes: %v", changed)
	}
}

func TestUnitChangeGapsRequiresTheMatchingAssertion(t *testing.T) {
	changed := map[string]map[string]bool{"tasks": {"t1": true}, "audit": {"a1": true}}
	asserted := map[string]bool{"tasks\x00t1": true}
	gaps := unitChangeGaps(changed, asserted, map[string]bool{})
	if !reflect.DeepEqual(gaps, []string{"audit/a1"}) {
		t.Fatalf("gaps = %v; want only audit/a1", gaps)
	}
	// Assertions are held per observer, so an empty observer is never satisfied
	// by another observer's map.
	if gaps := unitChangeGaps(changed, map[string]bool{}, map[string]bool{}); len(gaps) != 2 {
		t.Fatalf("empty observer had satisfied gaps: %v", gaps)
	}
}

func TestUnitChangeGapsAcceptsExplicitAbsence(t *testing.T) {
	changed := map[string]map[string]bool{"tasks": {"t1": true}}
	absent := map[string]bool{"tasks\x00t1": true}
	if gaps := unitChangeGaps(changed, map[string]bool{}, absent); len(gaps) != 0 {
		t.Fatalf("explicit absence did not satisfy a deleted row: %v", gaps)
	}
}

func TestUnitChangeGapsRejectsUnassertedRow(t *testing.T) {
	changed := map[string]map[string]bool{"events": {"e1": true}}
	if gaps := unitChangeGaps(changed, map[string]bool{}, map[string]bool{}); len(gaps) != 1 {
		t.Fatalf("unasserted changed row produced no gap: %v", gaps)
	}
}

func TestUnitChangeCountMismatchRequiresExactCounts(t *testing.T) {
	if mismatch := unitChangeCountMismatch(map[string]int{"audit": 1, "events": 2}, map[string]int{"audit": 1, "events": 2}); len(mismatch) != 0 {
		t.Fatalf("equal counts mismatched: %v", mismatch)
	}
	if mismatch := unitChangeCountMismatch(map[string]int{"events": 2}, map[string]int{"events": 2, "audit": 1}); len(mismatch) != 1 || mismatch[0] != "missing audit" {
		t.Fatalf("missing table not reported: %v", mismatch)
	}
	if mismatch := unitChangeCountMismatch(map[string]int{"events": 1}, map[string]int{"events": 2}); len(mismatch) != 1 {
		t.Fatalf("dropped row not reported: %v", mismatch)
	}
	if mismatch := unitChangeCountMismatch(map[string]int{"events": 2, "tasks": 1}, map[string]int{"events": 2}); len(mismatch) != 1 || mismatch[0] != "unexpected tasks" {
		t.Fatalf("extra table not reported: %v", mismatch)
	}
}

// fakeTB records failures instead of failing the real test, so the observer's
// failure paths can be asserted without a database.
type fakeTB struct {
	testing.TB
	name     string
	failed   bool
	errors   []string
	cleanups []func()
}

func newFakeTB(t *testing.T, name string) *fakeTB { return &fakeTB{TB: t, name: name} }

func (f *fakeTB) Helper()      {}
func (f *fakeTB) Name() string { return f.name }
func (f *fakeTB) Failed() bool { return f.failed }
func (f *fakeTB) Errorf(format string, args ...any) {
	f.failed = true
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}
func (f *fakeTB) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }
func (f *fakeTB) runCleanups() {
	for _, v := range slices.Backward(f.cleanups) {
		v()
	}
}

func withCleanObserverState(t *testing.T) {
	t.Helper()
	savedActive, savedDerived := activeObservers, derivedUnitStates
	activeObservers, derivedUnitStates = nil, map[string][]string{}
	t.Cleanup(func() { activeObservers, derivedUnitStates = savedActive, savedDerived })
}

func openObserver(tb testing.TB, unit, state string, before map[string]map[string]string) *UnitObserver {
	observer := newUnitObserver(tb, nil, unit, state, before)
	activeObservers = append(activeObservers, observer)
	tb.Cleanup(observer.cleanup)
	return observer
}

func TestObserverAssertionsAreNotSharedBetweenObservers(t *testing.T) {
	withCleanObserverState(t)
	tb := newFakeTB(t, "TestA")
	first := openObserver(tb, "task", "one", map[string]map[string]string{"tasks": {}})
	recordGoldenAssertion(tb, "tasks", "t1")
	if !first.asserted["tasks\x00t1"] {
		t.Fatal("the open observer did not record the assertion")
	}
	second := newUnitObserver(tb, nil, "task", "two", map[string]map[string]string{"tasks": {}})
	if len(second.asserted) != 0 || len(second.absent) != 0 {
		t.Fatalf("a new observer inherited assertions: %v %v", second.asserted, second.absent)
	}
	after := map[string]map[string]string{"tasks": {"t1": "h"}}
	err := second.finishWith(after, map[string]int{"tasks": 1})
	if err == nil || !strings.Contains(err.Error(), "without whole-row assertions") {
		t.Fatalf("second observer finished = %v; want a missing-assertion failure", err)
	}
}

func TestRecordGoldenAssertionMarksExactlyOneKeyForTheAssertingTest(t *testing.T) {
	withCleanObserverState(t)
	mine := newFakeTB(t, "TestMine")
	other := newFakeTB(t, "TestOther")
	observer := openObserver(mine, "task", "state", map[string]map[string]string{})
	recordGoldenAssertion(mine, "tasks", "t1")
	if len(observer.asserted) != 1 || !observer.asserted["tasks\x00t1"] {
		t.Fatalf("asserted = %v; want exactly tasks/t1", observer.asserted)
	}
	recordGoldenAssertion(other, "audit", "a1")
	recordRowAbsence(other, "events", "e1")
	if len(observer.asserted) != 1 || len(observer.absent) != 0 {
		t.Fatalf("another test's assertions leaked: asserted=%v absent=%v", observer.asserted, observer.absent)
	}
	sub := newFakeTB(t, "TestMine/sub")
	recordGoldenAssertion(sub, "audit", "a2")
	recordRowAbsence(sub, "events", "e2")
	if !observer.asserted["audit\x00a2"] || !observer.absent["events\x00e2"] {
		t.Fatalf("a subtest's assertions were not attributed: %v %v", observer.asserted, observer.absent)
	}
	lookalike := newFakeTB(t, "TestMineToo")
	recordGoldenAssertion(lookalike, "audit", "a3")
	if observer.asserted["audit\x00a3"] {
		t.Fatal("a test whose name merely starts with the observer's test name was attributed")
	}
}

func TestFinishWithRequiresCountsAndAssertions(t *testing.T) {
	withCleanObserverState(t)
	tb := newFakeTB(t, "TestF")
	before := map[string]map[string]string{"tasks": {"t1": "a"}, "audit": {}}
	after := map[string]map[string]string{"tasks": {"t1": "b"}, "audit": {"a1": "x"}}

	observer := openObserver(tb, "task", "ok", before)
	recordGoldenAssertion(tb, "tasks", "t1")
	recordGoldenAssertion(tb, "audit", "a1")
	if err := observer.finishWith(after, map[string]int{"tasks": 1, "audit": 1}); err != nil {
		t.Fatalf("a fully asserted unit failed: %v", err)
	}
	if got := derivedUnitStates["task.ok"]; len(got) != 2 || got[0] != "audit" || got[1] != "tasks" {
		t.Fatalf("derived tables = %v; want [audit tasks]", got)
	}
	if len(activeObservers) != 0 {
		t.Fatalf("finishing left %d active observers", len(activeObservers))
	}
	if err := observer.finishWith(after, map[string]int{"tasks": 1, "audit": 1}); err == nil {
		t.Fatal("a finished observer finished twice")
	}

	wrongCount := openObserver(tb, "task", "count", before)
	recordGoldenAssertion(tb, "tasks", "t1")
	recordGoldenAssertion(tb, "audit", "a1")
	if err := wrongCount.finishWith(after, map[string]int{"tasks": 1, "audit": 2}); err == nil ||
		!strings.Contains(err.Error(), "expected-set mismatch") {
		t.Fatalf("wrong expected count finished = %v; want a mismatch", err)
	}

	unasserted := openObserver(tb, "task", "gap", before)
	recordGoldenAssertion(tb, "tasks", "t1")
	if err := unasserted.finishWith(after, map[string]int{"tasks": 1, "audit": 1}); err == nil ||
		!strings.Contains(err.Error(), "audit/a1") {
		t.Fatalf("unasserted audit row finished = %v; want it named", err)
	}
	if _, registered := derivedUnitStates["task.gap"]; registered {
		t.Fatal("a failed finish registered its unit×state")
	}
}

func TestObserverNeverFinishedFailsAtCleanup(t *testing.T) {
	withCleanObserverState(t)
	tb := newFakeTB(t, "TestUnfinished")
	openObserver(tb, "task", "dangling", map[string]map[string]string{})
	if len(activeObservers) != 1 {
		t.Fatalf("active observers = %d; want 1", len(activeObservers))
	}
	tb.runCleanups()
	if len(tb.errors) != 1 || !strings.Contains(tb.errors[0], "task.dangling was observed but never finished") {
		t.Fatalf("cleanup errors = %v; want the never-finished failure", tb.errors)
	}
	if len(activeObservers) != 0 {
		t.Fatalf("cleanup left %d active observers", len(activeObservers))
	}
}

func TestFinishedObserverCleanupIsSilentAndFailedTestIsNotDoubleReported(t *testing.T) {
	withCleanObserverState(t)
	done := newFakeTB(t, "TestDone")
	observer := openObserver(done, "task", "done", map[string]map[string]string{})
	if err := observer.finishWith(map[string]map[string]string{}, map[string]int{}); err != nil {
		t.Fatalf("empty unit failed: %v", err)
	}
	done.runCleanups()
	if len(done.errors) != 0 {
		t.Fatalf("a finished observer reported %v at cleanup", done.errors)
	}

	failing := newFakeTB(t, "TestFailing")
	openObserver(failing, "task", "aborted", map[string]map[string]string{})
	failing.failed = true
	failing.runCleanups()
	if len(failing.errors) != 0 {
		t.Fatalf("an already-failed test was reported again: %v", failing.errors)
	}
	if len(activeObservers) != 0 {
		t.Fatalf("cleanups left %d active observers", len(activeObservers))
	}
}

func TestIndexSnapshotRowsRefusesDuplicateIDs(t *testing.T) {
	indexed, err := indexSnapshotRows("tasks", [][]string{{"t1", "a"}, {"t2", "b"}})
	if err != nil || len(indexed) != 2 || indexed["t1"] != "a" || indexed["t2"] != "b" {
		t.Fatalf("indexSnapshotRows = %v, %v", indexed, err)
	}
	if _, err := indexSnapshotRows("tasks", [][]string{{"t1", "a"}, {"t1", "b"}}); err == nil ||
		!strings.Contains(err.Error(), `duplicate row id "t1"`) {
		t.Fatalf("duplicate ids = %v; want a duplicate-id refusal", err)
	}
	if _, err := indexSnapshotRows("tasks", [][]string{{"t1"}}); err == nil {
		t.Fatal("a one-column row was accepted")
	}
	if indexed, err := indexSnapshotRows("tasks", nil); err != nil || len(indexed) != 0 {
		t.Fatalf("no rows = %v, %v", indexed, err)
	}
}
