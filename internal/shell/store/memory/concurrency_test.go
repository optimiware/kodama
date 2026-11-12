// Concurrency and isolation invariants: conflicting units serialize to exactly
// one winner, duplicate receipts apply once and replay afterwards, terminal
// schedules settle once, opposite lock listings cannot deadlock, caller input
// and inspection results never alias stored state, and each New() store is an
// independent ephemeral instance.
package memory

import (
	"bytes"
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
)

func TestConcurrentConflictingUnitsHaveExactlyOneWinner(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))

	states := []string{"running", "succeeded"}
	units := make([]store.Unit, len(states))
	for index, state := range states {
		record := receiptRecord(t,
			fmt.Sprintf("receipt-%d", index),
			keyFor(testTenantID, testProjectID, testPrincipalID, "execution.transition", fmt.Sprintf("caller-%d", index)),
			fmt.Sprintf(`{"state":%q}`, state),
			fmt.Sprintf(`{"winner":%d}`, index))
		units[index] = store.Unit{
			Writes:  []store.Write{{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": state}}},
			Receipt: &record,
			Events:  []event.Envelope{defaultEvent(scope, fmt.Sprintf("event-%d", index), event.TypeExecutionStarted, testExecutionID)},
		}
	}

	errs := runConcurrently(t, len(units), func(index int) error {
		_, err := s.Execute(context.Background(), scope, units[index])
		return err
	})
	if got := countNil(errs); got != 1 {
		t.Fatalf("concurrent settlement successes = %d; want exactly 1 (%s)", got, describeErrors(errs))
	}
	if got := countFault(t, errs, fault.VersionConflict); got != 1 {
		t.Fatalf("concurrent settlement conflicts = %d; want exactly 1 (%s)", got, describeErrors(errs))
	}

	winner := 0
	if errs[1] == nil {
		winner = 1
	}
	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowString(t, execution, "state"); got != states[winner] {
		t.Errorf("settled state = %q; want the winner's %q", got, states[winner])
	}
	if got := rowVersion(t, execution, "version"); got != 2 {
		t.Errorf("settled version = %d; want exactly one applied update (2)", got)
	}
	if _, found := s.Receipt(units[winner].Receipt.Key); !found {
		t.Error("the winner's receipt is missing")
	}
	if _, found := s.Receipt(units[1-winner].Receipt.Key); found {
		t.Error("the loser's receipt was committed")
	}
	events := s.Events(scope)
	if len(events) != 1 {
		t.Fatalf("settled events = %d; want exactly the winner's event", len(events))
	}
	if want := fmt.Sprintf("event-%d", winner); string(events[0].Envelope.ID) != want {
		t.Errorf("settled event = %q; want the winner's %q", events[0].Envelope.ID, want)
	}
}

func TestConcurrentDuplicateReceiptsApplyOnce(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
	record := defaultReceipt(t)
	unit := store.Unit{
		Writes:  []store.Write{{Table: "executions", ID: testExecutionID, ExpectVersion: 1, Values: map[string]any{"state": "running"}}},
		Receipt: &record,
		Audit:   []event.Audit{defaultAudit(scope, "audit-1", "execution.start")},
		Events:  []event.Envelope{defaultEvent(scope, "event-1", event.TypeExecutionStarted, testExecutionID)},
	}

	const workers = 4
	results := make([]store.UnitResult, workers)
	errs := runConcurrently(t, workers, func(index int) error {
		result, err := s.Execute(context.Background(), scope, unit)
		results[index] = result
		return err
	})
	for index, err := range errs {
		if err != nil {
			t.Fatalf("duplicate receipt worker %d: %v", index, err)
		}
	}

	replayed := 0
	for _, result := range results {
		if result.Replayed {
			replayed++
		}
		if string(result.Receipt.Result) != string(record.Result) {
			t.Errorf("worker result = %q; want the original %q", result.Receipt.Result, record.Result)
		}
	}
	if replayed != workers-1 {
		t.Fatalf("replayed results = %d; want %d (exactly one application)", replayed, workers-1)
	}
	execution := mustSnapshot(t, s, scope, "executions", testExecutionID)
	if got := rowVersion(t, execution, "version"); got != 2 {
		t.Errorf("execution version = %d; want one applied update (2)", got)
	}
	if events := s.Events(scope); len(events) != 1 {
		t.Errorf("duplicate receipts published %d events; want 1", len(events))
	}
	if audits := s.Audit(scope); len(audits) != 1 {
		t.Errorf("duplicate receipts wrote %d audit rows; want 1", len(audits))
	}
}

func TestConcurrentTerminalSchedulesSettleOnce(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "actions", testActionID, actionValues(1, "running"))

	terminals := []struct {
		name      string
		state     string
		decision  string
		eventType event.Type
	}{
		{name: "output", state: "succeeded", eventType: event.TypeActionTransition},
		{name: "resolve", state: "succeeded", decision: "pass", eventType: event.TypeRequestResolved},
		{name: "cancel", state: "cancelled", eventType: event.TypeExecutionCancelled},
	}
	units := make([]store.Unit, len(terminals))
	for index, terminal := range terminals {
		record := receiptRecord(t,
			fmt.Sprintf("receipt-%d", index),
			keyFor(testTenantID, testProjectID, testPrincipalID, "action.settle", fmt.Sprintf("caller-%d", index)),
			fmt.Sprintf(`{"terminal":%q}`, terminal.name),
			fmt.Sprintf(`{"winner":%q}`, terminal.name))
		values := map[string]any{"state": terminal.state}
		if terminal.decision != "" {
			values["request_decision"] = terminal.decision
		}
		units[index] = store.Unit{
			Writes:  []store.Write{{Table: "actions", ID: testActionID, ExpectVersion: 1, Values: values}},
			Receipt: &record,
			Events:  []event.Envelope{defaultEvent(scope, fmt.Sprintf("event-%d", index), terminal.eventType, testActionID)},
		}
	}

	errs := runConcurrently(t, len(units), func(index int) error {
		_, err := s.Execute(context.Background(), scope, units[index])
		return err
	})
	if got := countNil(errs); got != 1 {
		t.Fatalf("terminal settlement successes = %d; want exactly 1 (%s)", got, describeErrors(errs))
	}
	if got := countFault(t, errs, fault.VersionConflict); got != len(units)-1 {
		t.Fatalf("terminal settlement conflicts = %d; want %d (%s)", got, len(units)-1, describeErrors(errs))
	}

	winner := -1
	for index, err := range errs {
		if err == nil {
			winner = index
		}
	}
	action := mustSnapshot(t, s, scope, "actions", testActionID)
	if got := rowString(t, action, "state"); got != terminals[winner].state {
		t.Errorf("settled action state = %q; want the winner's %q", got, terminals[winner].state)
	}
	if got := rowVersion(t, action, "version"); got != 2 {
		t.Errorf("settled action version = %d; want 2", got)
	}
	events := s.Events(scope)
	if len(events) != 1 {
		t.Fatalf("settled events = %d; want exactly one", len(events))
	}
	if want := fmt.Sprintf("event-%d", winner); string(events[0].Envelope.ID) != want {
		t.Errorf("settled event = %q; want the winner's %q", events[0].Envelope.ID, want)
	}
	if _, found := s.Receipt(units[winner].Receipt.Key); !found {
		t.Error("the winning terminal receipt is missing")
	}
}

func TestOppositeLockListingDoesNotDeadlock(t *testing.T) {
	s := New()
	scope := projectScope()
	seedRow(t, s, scope, "executions", testExecutionID, executionValues(1, "queued"))
	seedRow(t, s, scope, "actions", testActionID, actionValues(1, "pending"))

	unitA := store.Unit{Locks: []store.Lock{
		{Table: "executions", IDs: []string{testExecutionID}},
		{Table: "actions", IDs: []string{testActionID}},
	}}
	unitB := store.Unit{Locks: []store.Lock{
		{Table: "actions", IDs: []string{testActionID}},
		{Table: "executions", IDs: []string{testExecutionID}},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errs := runConcurrently(t, 2, func(index int) error {
		unit := unitA
		if index == 1 {
			unit = unitB
		}
		_, err := s.Execute(ctx, scope, unit)
		return err
	})
	for index, err := range errs {
		if err != nil {
			t.Fatalf("unit %d with an opposite lock listing: %v", index, err)
		}
	}
}

func TestNoMutableAliasesBetweenInputAndStorage(t *testing.T) {
	s := New()
	scope := projectScope()

	originalYAML := []byte("name: build\non: push\n")
	yaml := append([]byte(nil), originalYAML...)
	values := map[string]any{"name": "build", "yaml": yaml, "yaml_sha256": sha256Sum(yaml), "version": uint64(1)}
	mustExecute(t, s, scope, store.Unit{Writes: []store.Write{
		{Table: "pipelines", ID: "pipeline-1", Insert: true, Values: values},
	}})
	yaml[0] = 'X'
	values["name"] = "mutated"
	values["yaml"] = []byte("replaced")

	pipeline := mustSnapshot(t, s, scope, "pipelines", "pipeline-1")
	if got, _ := pipeline.Values["yaml"].([]byte); !bytes.Equal(got, originalYAML) {
		t.Errorf("mutating the input values changed stored YAML to %q", got)
	}
	if got := pipeline.Values["name"]; got != "build" {
		t.Errorf("mutating the input map changed the stored name to %v", got)
	}

	originalData := []byte(`{"a":1}`)
	data := append([]byte(nil), originalData...)
	envelope := defaultEvent(scope, "event-1", event.TypeTaskCreated, testTaskID)
	envelope.Data = data
	mustExecute(t, s, scope, store.Unit{Events: []event.Envelope{envelope}})
	data[0] = 'X'

	events := s.Events(scope)
	if len(events) != 1 {
		t.Fatalf("committed events = %d; want 1", len(events))
	}
	if !bytes.Equal(events[0].Envelope.Data, originalData) {
		t.Errorf("mutating input event data changed stored data to %q", events[0].Envelope.Data)
	}
	events[0].Envelope.Data[0] = 'Y'
	events[0].Envelope.ID = "mutated"
	eventsAgain := s.Events(scope)
	if string(eventsAgain[0].Envelope.ID) != "event-1" {
		t.Errorf("mutating a returned event changed the stored id to %q", eventsAgain[0].Envelope.ID)
	}
	if !bytes.Equal(eventsAgain[0].Envelope.Data, originalData) {
		t.Errorf("mutating a returned event changed the stored data to %q", eventsAgain[0].Envelope.Data)
	}

	audit := defaultAudit(scope, "audit-1", "task.create")
	mustExecute(t, s, scope, store.Unit{Audit: []event.Audit{audit}})
	audits := s.Audit(scope)
	if len(audits) != 1 {
		t.Fatalf("committed audit rows = %d; want 1", len(audits))
	}
	audits[0].Detail = "mutated"
	audits[0].Operation = "mutated"
	auditsAgain := s.Audit(scope)
	if auditsAgain[0].Detail != audit.Detail || auditsAgain[0].Operation != audit.Operation {
		t.Errorf("mutating a returned audit row changed stored state: %+v", auditsAgain[0])
	}

	originalResult := []byte(`{"execution_id":"execution-1"}`)
	record := defaultReceipt(t)
	record.Result = append([]byte(nil), originalResult...)
	mustExecute(t, s, scope, store.Unit{Receipt: &record})
	record.Result[0] = 'X'
	stored, found := s.Receipt(record.Key)
	if !found {
		t.Fatal("the committed receipt is missing")
	}
	if !bytes.Equal(stored.Result, originalResult) {
		t.Errorf("mutating the input receipt result changed stored result to %q", stored.Result)
	}
	stored.Result[0] = 'Y'
	storedAgain, _ := s.Receipt(record.Key)
	if !bytes.Equal(storedAgain.Result, originalResult) {
		t.Errorf("mutating a returned receipt changed stored result to %q", storedAgain.Result)
	}
}

func TestStoresAreEphemeralAndIndependent(t *testing.T) {
	first := New()
	second := New()
	seedRow(t, first, projectScope(), "executions", testExecutionID, executionValues(1, "queued"))

	if _, found := second.Snapshot(projectScope(), "executions", testExecutionID); found {
		t.Error("a second store saw the first store's committed row")
	}
	if _, err := second.Read(context.Background(), projectScope(), "executions", testExecutionID); err == nil {
		t.Error("a second store read the first store's committed row")
	} else {
		assertFault(t, err, fault.NotFound)
	}
	if events := second.Events(projectScope()); len(events) != 0 {
		t.Errorf("a second store saw %d events from the first", len(events))
	}
}

func TestProductionPackageDocumentsTheEphemeralDevelopmentAdapter(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	documented := false
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, source, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if file.Doc == nil {
			continue
		}
		doc := strings.ToLower(file.Doc.Text())
		if strings.Contains(doc, "ephemeral") && strings.Contains(doc, "development") {
			documented = true
		}
	}
	if !documented {
		t.Fatal("the production package doc must state that this is an ephemeral development adapter")
	}
}
