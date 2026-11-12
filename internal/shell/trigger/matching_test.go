package trigger_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

func TestParseReportMapsExactRefAndCommits(t *testing.T) {
	envelope := pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1").Event
	report, err := trigger.ParseReport(envelope)
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if report.Operation != "git.ref.update" {
		t.Fatalf("operation = %q; want git.ref.update", report.Operation)
	}
	if string(report.Resource) != "repo-1" {
		t.Fatalf("resource = %q; want repo-1", report.Resource)
	}
	if report.Ref != "refs/heads/main" {
		t.Fatalf("ref = %q; want refs/heads/main", report.Ref)
	}
	if report.Commit != "cand-1" {
		t.Fatalf("commit = %q; want candidate-1", report.Commit)
	}
	if report.PreviousCommit != "base-1" {
		t.Fatalf("previous commit = %q; want base-1", report.PreviousCommit)
	}
	if report.Deleted {
		t.Fatal("ordinary ref update parsed as a deletion")
	}
}

func TestParseReportMarksDeletion(t *testing.T) {
	cases := []struct {
		name      string
		deleted   bool
		candidate string
	}{
		{name: "explicit deletion", deleted: true, candidate: ""},
		{name: "empty candidate", deleted: false, candidate: ""},
		{name: "deleted flag with candidate", deleted: true, candidate: "cand-1"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			row := pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", testCase.candidate)
			row.Event.Data = reportData("repo-1", "refs/heads/main", "base-1", testCase.candidate, testCase.deleted)
			report, err := trigger.ParseReport(row.Event)
			if err != nil {
				t.Fatalf("ParseReport: %v", err)
			}
			if !report.Deleted {
				t.Fatal("deletion payload was not marked deleted")
			}
		})
	}
}

func TestParseReportRefusesMalformedPayload(t *testing.T) {
	valid := pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1").Event
	cases := []struct {
		name  string
		event event.Envelope
	}{
		{
			name: "malformed json",
			event: func() event.Envelope {
				broken := valid
				broken.Data = []byte("{")
				return broken
			}(),
		},
		{
			name: "missing ref",
			event: func() event.Envelope {
				missing := valid
				missing.Data = reportData("repo-1", "", "base-1", "cand-1", false)
				return missing
			}(),
		},
		{
			name: "wrong operation",
			event: func() event.Envelope {
				wrong := valid
				wrong.Data = []byte(`{"operation":"git.ref.delete","resource":"repo-1","ref":"refs/heads/main","expected_base":"base-1","candidate":"cand-1"}`)
				return wrong
			}(),
		},
		{
			name: "resource disagrees with subject",
			event: func() event.Envelope {
				mismatch := valid
				mismatch.Data = reportData("repo-2", "refs/heads/main", "base-1", "cand-1", false)
				return mismatch
			}(),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := trigger.ParseReport(testCase.event)
			if err == nil {
				t.Fatal("malformed report payload accepted")
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("error class = %v (ok=%v); want invalid_input", class, ok)
			}
		})
	}
}

func TestExactPushMatchesRefResourceAndReportingSource(t *testing.T) {
	scope := testScope()
	mainYAML := pushYAML("repo-1", "start-svc-1", []string{"refs/heads/main"}, []string{"repository", "ref", "commit", "previous_commit"}, "")
	releaseYAML := pushYAML("repo-1", "start-svc-2", []string{"refs/heads/release"}, []string{"repository", "ref"}, "")
	otherRepoYAML := pushYAML("repo-2", "start-svc-1", []string{"refs/heads/main"}, []string{"repository", "ref"}, "")
	otherSourceYAML := pushYAML("repo-1", "start-svc-4", []string{"refs/heads/main"}, []string{"repository", "ref"}, "")

	main := testDefinition("pipeline-main", mainYAML, 1, "repo-1", "start-svc-1", []string{"refs/heads/main"})
	release := testDefinition("pipeline-release", releaseYAML, 1, "repo-1", "start-svc-2", []string{"refs/heads/release"})
	otherRepo := testDefinition("pipeline-other-repo", otherRepoYAML, 1, "repo-2", "start-svc-1", []string{"refs/heads/main"})
	otherSource := testDefinition("pipeline-other-source", otherSourceYAML, 1, "repo-1", "start-svc-4", []string{"refs/heads/main"})

	candidates := &fakeCandidates{}
	candidates.set(
		testCandidate(main, testBinding(scope, "repo-1", "git-svc", 1)),
		testCandidate(release, testBinding(scope, "repo-1", "git-svc", 1)),
		testCandidate(otherRepo, testBinding(scope, "repo-2", "git-svc", 1)),
		testCandidate(otherSource, testBinding(scope, "repo-1", "git-other", 1)),
	)
	events := &fakeEvents{}
	events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	selections := newFakeSelections()
	admission := &fakeAdmission{}
	start := newFakeStart(selections)
	completion := &fakeCompletion{}
	processor := newProcessor(t, events, candidates, selections, admission, start, completion)

	result, err := processor.Process(context.Background())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if selections.selectCount() != 1 {
		t.Fatalf("Select called %d times; want only the exact pipeline", selections.selectCount())
	}
	selected := selections.selectCall(0)
	if selected.PipelineID != pipeline.PipelineID("pipeline-main") {
		t.Fatalf("selected pipeline = %q; want pipeline-main", selected.PipelineID)
	}
	if string(selected.Selection.YAML) != mainYAML {
		t.Fatalf("selection yaml = %q; want the exact authored bytes", selected.Selection.YAML)
	}
	if selected.Selection.ServiceID != service.ServiceID("start-svc-1") {
		t.Fatalf("selection service = %q; want start-svc-1", selected.Selection.ServiceID)
	}
	wantInputs := map[id.InputKey]string{
		"repository":      "repo-1",
		"ref":             "refs/heads/main",
		"commit":          "cand-1",
		"previous_commit": "base-1",
	}
	if !reflect.DeepEqual(selected.Selection.Inputs, wantInputs) {
		t.Fatalf("selection inputs = %v; want %v", selected.Selection.Inputs, wantInputs)
	}
	if selected.Selection.EventContentHash == ([32]byte{}) {
		t.Fatal("selection has no event content fingerprint")
	}
	if selected.Receipt.Key.Operation != "pipeline.start" {
		t.Fatalf("receipt operation = %q; want pipeline.start", selected.Receipt.Key.Operation)
	}
	if start.callCount() != 1 {
		t.Fatalf("Start called %d times; want one execution", start.callCount())
	}
	started := start.call(0)
	if started.Selection.ServiceID != service.ServiceID("start-svc-1") {
		t.Fatalf("start service = %q; want the first-selected service", started.Selection.ServiceID)
	}
	// The automatic service initiates; the execution runs as its own
	// capability principal, never as the service account (the shared start
	// gives every execution a fresh principal that resolves to that execution).
	if string(started.Initiator) != "start-svc-1" || string(started.ExecutionPrincipal) != "principal-exec-1" {
		t.Fatalf("start principals = %q/%q; want the automatic service and the execution's own principal", started.Initiator, started.ExecutionPrincipal)
	}
	if got := string(started.Admission.Snapshot.YAMLBytes()); got != mainYAML {
		t.Fatalf("admitted yaml = %q; want exact first-selection bytes", got)
	}
	if len(result.Started) != 1 || result.Started[0] != id.ExecutionID("exec-1") {
		t.Fatalf("started executions = %v; want [exec-1]", result.Started)
	}
	if !completion.has(1) {
		t.Fatal("completed event was not recorded")
	}
}

func TestPushRefMustMatchExactly(t *testing.T) {
	scope := testScope()
	yaml := pushYAML("repo-1", "start-svc-1", []string{"refs/heads/main"}, []string{"ref"}, "")
	def := testDefinition("pipeline-main", yaml, 1, "repo-1", "start-svc-1", []string{"refs/heads/main"})
	candidates := &fakeCandidates{}
	candidates.set(testCandidate(def, testBinding(scope, "repo-1", "git-svc", 1)))
	events := &fakeEvents{}
	events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/mainline", "base-1", "cand-1"))
	selections := newFakeSelections()
	processor := newProcessor(t, events, candidates, selections, &fakeAdmission{}, newFakeStart(selections), &fakeCompletion{})

	if _, err := processor.Process(context.Background()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if selections.selectCount() != 0 {
		t.Fatalf("Select called %d times for a non-exact ref; want zero", selections.selectCount())
	}
}

func TestPushIgnoresDeletionsAndExecutionOriginatedReports(t *testing.T) {
	scope := testScope()
	yaml := pushYAML("repo-1", "start-svc-1", []string{"refs/heads/main"}, []string{"repository", "ref"}, "")
	first := testDefinition("pipeline-a", yaml, 1, "repo-1", "start-svc-1", []string{"refs/heads/main"})
	second := testDefinition("pipeline-b", pushYAML("repo-1", "start-svc-2", []string{"refs/heads/main"}, []string{"ref"}, ""), 1, "repo-1", "start-svc-2", []string{"refs/heads/main"})
	candidates := &fakeCandidates{}
	candidates.set(
		testCandidate(first, testBinding(scope, "repo-1", "git-svc", 1)),
		testCandidate(second, testBinding(scope, "repo-1", "git-svc", 1)),
	)

	cases := []struct {
		name string
		row  event.PendingEvent
	}{
		{name: "deletion report", row: deletionEvent(1, "event-delete", "repo-1", "git-svc", "refs/heads/main")},
		{name: "execution correlation", row: executionOriginatedEvent(1, "event-caused", "repo-1", "git-svc", "refs/heads/main")},
		{
			name: "execution correlation without causation",
			row: func() event.PendingEvent {
				row := executionOriginatedEvent(1, "event-uncaused", "repo-1", "git-svc", "refs/heads/main")
				row.Event.CausationID = ""
				return row
			}(),
		},
		{
			name: "execution source",
			row: func() event.PendingEvent {
				row := pushEvent(1, "event-execution", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1")
				row.Event.Source.Kind = event.SourceExecution
				return row
			}(),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			events := &fakeEvents{}
			events.setRows(testCase.row)
			selections := newFakeSelections()
			processor := newProcessor(t, events, candidates, selections, &fakeAdmission{}, newFakeStart(selections), &fakeCompletion{})
			if _, err := processor.Process(context.Background()); err != nil {
				t.Fatalf("Process: %v", err)
			}
			if selections.selectCount() != 0 {
				t.Fatalf("Select called %d times; want zero across every pipeline", selections.selectCount())
			}
		})
	}
}

// TestHumanPushReportWithVerificationCausationStartsItsPipeline pins the real
// report shape of a human push: the verified-report ingress always sets the
// causation to the cited verification, and a human pusher has no execution to
// correlate. That report must start its subscribed pipeline; only the execution
// correlation marks a push as execution-originated.
func TestHumanPushReportWithVerificationCausationStartsItsPipeline(t *testing.T) {
	scope := testScope()
	yaml := pushYAML("repo-1", "start-svc-1", []string{"refs/heads/main"}, []string{"commit"}, "")
	def := testDefinition("pipeline-main", yaml, 1, "repo-1", "start-svc-1", []string{"refs/heads/main"})
	candidates := &fakeCandidates{}
	candidates.set(testCandidate(def, testBinding(scope, "repo-1", "git-svc", 1)))
	row := pushEvent(1, "event-human", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1")
	row.Event.CausationID = "verification-human"
	row.Event.CorrelationID = ""
	events := &fakeEvents{}
	events.setRows(row)
	selections := newFakeSelections()
	start := newFakeStart(selections)
	completion := &fakeCompletion{}
	processor := newProcessor(t, events, candidates, selections, &fakeAdmission{}, start, completion)

	result, err := processor.Process(context.Background())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if selections.selectCount() != 1 {
		t.Fatalf("Select called %d times for a human push with verification causation; want 1", selections.selectCount())
	}
	if got := selections.selectCall(0).Selection.Inputs; !reflect.DeepEqual(got, map[id.InputKey]string{"commit": "cand-1"}) {
		t.Fatalf("selection inputs = %v; want the pushed commit", got)
	}
	if start.callCount() != 1 || len(result.Started) != 1 {
		t.Fatalf("Start called %d times, started %v; want one execution", start.callCount(), result.Started)
	}
	if !completion.has(1) {
		t.Fatal("the human push event was not completed")
	}
}

func TestPushIgnoresUnacceptedEventTypes(t *testing.T) {
	scope := testScope()
	yaml := pushYAML("repo-1", "start-svc-1", []string{"refs/heads/main"}, []string{"ref"}, "")
	def := testDefinition("pipeline-main", yaml, 1, "repo-1", "start-svc-1", []string{"refs/heads/main"})
	candidates := &fakeCandidates{}
	candidates.set(testCandidate(def, testBinding(scope, "repo-1", "git-svc", 1)))

	custom := pushEvent(1, "event-custom", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1")
	custom.Event.Type = event.TypeCustomObservation
	custom.Event.ReportType = ""
	custom.Event.Data = nil
	deletedType := pushEvent(2, "event-deleted-type", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1")
	deletedType.Event.ReportType = event.ReportType("git.ref.deleted")
	task := pushEvent(3, "event-task", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1")
	task.Event.Type = event.TypeTaskCreated
	task.Event.ReportType = ""
	task.Event.Data = nil

	events := &fakeEvents{}
	events.setRows(custom, deletedType, task)
	selections := newFakeSelections()
	completion := &fakeCompletion{}
	processor := newProcessor(t, events, candidates, selections, &fakeAdmission{}, newFakeStart(selections), completion)

	if _, err := processor.Process(context.Background()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if selections.selectCount() != 0 {
		t.Fatalf("Select called %d times for unaccepted event types; want zero", selections.selectCount())
	}
	if completion.count() != 3 {
		t.Fatalf("completed %d events; want all three irrelevant events advanced", completion.count())
	}
}

func TestPushScopeIsolation(t *testing.T) {
	eventScope := testScope()
	otherScope := eventScope
	otherScope.ProjectID = "project-2"
	yaml := pushYAML("repo-1", "start-svc-1", []string{"refs/heads/main"}, []string{"ref"}, "")
	def := testDefinition("pipeline-other", yaml, 1, "repo-1", "start-svc-1", []string{"refs/heads/main"})
	candidates := &fakeCandidates{}
	candidates.set(testCandidate(def, testBinding(otherScope, "repo-1", "git-svc", 1)))
	events := &fakeEvents{}
	events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	selections := newFakeSelections()
	processor := newProcessor(t, events, candidates, selections, &fakeAdmission{}, newFakeStart(selections), &fakeCompletion{})

	if _, err := processor.Process(context.Background()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if selections.selectCount() != 0 {
		t.Fatalf("Select called %d times across scopes; want zero", selections.selectCount())
	}
}

func TestPushInputsComeFromRepositoryRefCommitAndPreviousCommit(t *testing.T) {
	scope := testScope()
	all := testDefinition(
		"pipeline-all",
		pushYAML("repo-1", "start-svc-1", []string{"refs/heads/main"}, []string{"repository", "ref", "commit", "previous_commit"}, ""),
		1, "repo-1", "start-svc-1", []string{"refs/heads/main"},
	)
	refOnly := testDefinition(
		"pipeline-ref",
		pushYAML("repo-1", "start-svc-2", []string{"refs/heads/main"}, []string{"ref"}, ""),
		1, "repo-1", "start-svc-2", []string{"refs/heads/main"},
	)
	commitOnly := testDefinition(
		"pipeline-commit",
		pushYAML("repo-1", "start-svc-3", []string{"refs/heads/main"}, []string{"commit"}, ""),
		1, "repo-1", "start-svc-3", []string{"refs/heads/main"},
	)
	candidates := &fakeCandidates{}
	candidates.set(
		testCandidate(all, testBinding(scope, "repo-1", "git-svc", 1)),
		testCandidate(refOnly, testBinding(scope, "repo-1", "git-svc", 1)),
		testCandidate(commitOnly, testBinding(scope, "repo-1", "git-svc", 1)),
	)
	events := &fakeEvents{}
	events.setRows(pushEvent(1, "event-1", "repo-1", "git-svc", "refs/heads/main", "base-1", "cand-1"))
	selections := newFakeSelections()
	processor := newProcessor(t, events, candidates, selections, &fakeAdmission{}, newFakeStart(selections), &fakeCompletion{})

	if _, err := processor.Process(context.Background()); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if selections.selectCount() != 3 {
		t.Fatalf("Select called %d times; want every matching pipeline", selections.selectCount())
	}
	want := map[pipeline.PipelineID]map[id.InputKey]string{
		"pipeline-all": {
			"repository":      "repo-1",
			"ref":             "refs/heads/main",
			"commit":          "cand-1",
			"previous_commit": "base-1",
		},
		"pipeline-ref":    {"ref": "refs/heads/main"},
		"pipeline-commit": {"commit": "cand-1"},
	}
	for index := 0; index < selections.selectCount(); index++ {
		unit := selections.selectCall(index)
		if !reflect.DeepEqual(unit.Selection.Inputs, want[unit.PipelineID]) {
			t.Fatalf("%s inputs = %v; want %v", unit.PipelineID, unit.Selection.Inputs, want[unit.PipelineID])
		}
	}
}

var _ = resource.ResourceID("")
var _ = trigger.StatusSelected
