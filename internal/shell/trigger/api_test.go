// Package trigger_test pins the exact-push surface these tests rely on.
//
// The package declares the durable values Selection, SelectUnit,
// SelectPort.Select and Status, and the event-table-driven processor with its
// consumer-owned ports. The tests compile only against this exact surface.
//
// # Processor and ports
//
//	type EventReader interface {
//	    Pending(context.Context, event.Sequence, int) ([]event.PendingEvent, error)
//	}
//
//	type Candidate struct {
//	    Definition pipeline.Definition
//	    Binding    resource.Binding
//	}
//	type CandidateReader interface {
//	    Candidates(context.Context, tenancy.Scope) ([]Candidate, error)
//	}
//
//	type SelectionReader interface {
//	    Lookup(context.Context, tenancy.Scope, pipeline.PipelineID, id.EventID) (Result, bool, error)
//	    Unfinished(context.Context, tenancy.Scope, id.EventID) ([]Result, error)
//	}
//
//	type CompletionUnit struct {
//	    Owner    tenancy.Scope
//	    Sequence event.Sequence
//	    EventID  id.EventID
//	}
//	type CompletionPort interface {
//	    Complete(context.Context, CompletionUnit) error
//	}
//
//	type AdmissionPort interface {
//	    Admit(context.Context, pipeline.AdmitRequest) (pipeline.AdmissionResult, error)
//	}
//
//	type StartUnit struct {
//	    Owner                   tenancy.Scope
//	    PipelineID              pipeline.PipelineID
//	    EventID                 id.EventID
//	    Selection               Selection
//	    Admission               pipeline.AdmissionResult
//	    ExpectedPipelineVersion uint64
//	    ExpectedBindingVersion  uint64
//	    Initiator               id.PrincipalID
//	    ExecutionPrincipal      id.PrincipalID
//	    NewExecutionID          id.ExecutionID
//	    Status                  Status
//	    At                      time.Time
//	    Receipt                 receipt.Record
//	    Audit                   []event.Audit
//	    Events                  []event.Envelope
//	    Authority               []authorize.AuthorityRead
//	    PostCommit              []event.PostCommitEffect
//	}
//	type StartPort interface {
//	    Start(context.Context, StartUnit) (Result, error)
//	}
//
//	type Clock interface {
//	    Now() time.Time
//	}
//	type IDSource interface {
//	    NewEventID() (id.EventID, error)
//	    NewReceiptID() (id.ReceiptID, error)
//	    NewExecutionID() (id.ExecutionID, error)
//	}
//
//	type ProcessorConfig struct {
//	    Events         EventReader
//	    Candidates     CandidateReader
//	    Selections     SelectionReader
//	    Select         SelectPort
//	    Admission      AdmissionPort
//	    Start          StartPort
//	    Completion     CompletionPort
//	    Clock          Clock
//	    IDs            IDSource
//	    BatchSize      int
//	    DefaultTimeout time.Duration
//	    Idle           func(context.Context) error
//	}
//	func NewProcessor(ProcessorConfig) (*Processor, error)
//	func (*Processor) Process(context.Context) (ProcessResult, error)
//	func (*Processor) Run(context.Context) error
//
//	type ProcessResult struct {
//	    Started   []id.ExecutionID
//	    Refused   []id.EventID
//	    Deferred  []id.EventID
//	    Completed []event.Sequence
//	    More      bool
//	}
//
// Result gains the persisted first selection and its common receipt:
//
//	type Result struct {
//	    ExecutionID id.ExecutionID
//	    ReceiptID   id.ReceiptID
//	    Status      Status
//	    Replayed    bool
//	    Selection   Selection
//	    Receipt     receipt.Record
//	}
//
// # Verified report payload
//
// A push is an accepted event row with Type resource.verified,
// ReportType git.ref.updated, Source.Kind service, SubjectID equal to the
// resource identity, and Data holding one JSON object:
//
//	{"operation":"git.ref.update","resource":"<resource-id>",
//	 "ref":"refs/heads/main","expected_base":"<sha>",
//	 "candidate":"<sha>","deleted":false}
//
//	type Report struct {
//	    Operation      string
//	    Resource       resource.ResourceID
//	    Ref            string
//	    Commit         string
//	    PreviousCommit string
//	    Deleted        bool
//	}
//	func ParseReport(event.Envelope) (Report, error)
//
// ParseReport maps candidate to Commit and expected_base to PreviousCommit. A
// deletion sets deleted true or leaves candidate empty. Malformed JSON, a
// missing ref, an operation other than git.ref.update, or a resource that
// disagrees with Envelope.SubjectID is refused with fault.InvalidInput.
//
// # Behavior contract
//
// Process scans committed rows from the event table through EventReader.Pending
// (starting at the zero sequence each pass) and never depends on NATS or any
// other broker. It accepts only verified git.ref.updated reports whose parsed
// report is not a deletion and whose envelope has no CorrelationID and is not
// execution-sourced. The verified-report ingress derives CorrelationID from the
// verified pusher's execution and always sets CausationID to the cited
// verification, so a human push matches while an execution-originated push
// (including a late report) starts nothing for any pipeline. Deletions start
// nothing either.
//
// For each candidate in the event scope it consults SelectionReader.Lookup
// before looking at the candidate's current YAML, binding or service. A stored
// selection is the first selection and is used verbatim: edits can change a
// later selection but never an existing one. SelectionReader.Unfinished lists
// the persisted selections of the event that still have no durable result. After
// the current candidates, the processor resumes each listed selection whose
// pipeline is no longer a candidate and records a permanent refusal through
// StartPort.Start, so a committed selection can never be skipped when its
// definition or binding is deleted. A new selection matches the
// binding scope, the binding resource, the binding's enforcing (reporting)
// service, the binding's declared git.ref.updated report type, and an exact
// on.push ref. Declared inputs are filled only from repository, ref, commit and
// previous_commit; undeclared inputs are never invented.
//
// SelectPort.Select commits the first selection and its common receipt. On
// success the processor calls AdmissionPort.Admit with an inline
// pipeline.AdmitRequest carrying the persisted exact YAML bytes, inputs and the
// configured installation default action timeout, so admission never re-reads
// an edited definition and an action without an explicit timeout resolves from
// the installation default rather than a zero deadline. It then calls
// StartPort.Start with StartUnit.Status == StatusSelected and the current
// automatic-service authority reads, including service enablement and the
// event.read grant for the accepted event. StartPort.Start commits either the queued execution or a
// permanent refusal and returns the persisted Result; the queued execution
// remains the sole dispatch record. A selected unit carries a canonical
// execution.queued event with publish and dispatch effects; a refused unit
// carries the canonical Shell-sourced pipeline.start_refused event (stable
// reason class plus the pipeline/event selection identity) with a publish
// effect. Select never commits a refusal, so a StatusRefused result or a
// permanent refusal fault class is routed through StartPort.Start and published
// exactly once.
//
// A permanent refusal (StatusRefused) is durable and is reported in
// ProcessResult.Refused. A permanent admission fault (Forbidden, InvalidInput,
// NotFound, VersionConflict, ChangedIdempotencyContent, UnsupportedCapability)
// is recorded through StartPort.Start with StatusRefused and no Admission
// result. Every other error is transient or uncertain: the selection is
// retained, the event is reported in ProcessResult.Deferred, and no completion
// is recorded. A lost start/admission response is therefore retried from the
// persisted receipt and returns the original execution; it never creates a
// second execution and is never replaced by a refusal.
//
// Completion advances only after every relevant selection for an event has a
// durable result. Process scans in durable sequence order and stops at the
// first event that still has an unfinished relevant selection, so a later
// event is never marked complete while an earlier result is unfinished. An
// event with no matching push pipeline is completed without creating work.
// Concurrent Process calls produce one execution per (pipeline, event).
//
// There is no generic trigger engine, command subscription, separate dispatch
// queue, Git port or YAML/repository discovery in this package.
package trigger_test

import (
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// Compile-time port shapes.
var (
	_ trigger.EventReader     = (*fakeEvents)(nil)
	_ trigger.CandidateReader = (*fakeCandidates)(nil)
	_ trigger.SelectionReader = (*fakeSelections)(nil)
	_ trigger.SelectPort      = (*fakeSelections)(nil)
	_ trigger.AdmissionPort   = (*fakeAdmission)(nil)
	_ trigger.StartPort       = (*fakeStart)(nil)
	_ trigger.CompletionPort  = (*fakeCompletion)(nil)
	_ trigger.Clock           = fakeClock{}
	_ trigger.IDSource        = (*fakeIDs)(nil)
)

func TestProcessorConfigHasNoBrokerOrDispatchQueue(t *testing.T) {
	assertFieldNames(t, reflect.TypeFor[trigger.ProcessorConfig](), []string{
		"Events", "Candidates", "Selections", "Select", "Admission", "Start",
		"Completion", "Clock", "IDs", "BatchSize", "DefaultTimeout", "Idle",
	})
	for _, field := range []string{"Broker", "NATS", "Publisher", "Queue", "Dispatch", "Subscribe"} {
		if hasFieldName(reflect.TypeFor[trigger.ProcessorConfig](), field) {
			t.Fatalf("ProcessorConfig exposes %q; the trigger processor must not own a broker or dispatch queue", field)
		}
	}
}

func TestProcessorHasOnlyProcessAndRun(t *testing.T) {
	assertMethodNames(t, reflect.TypeFor[*trigger.Processor](), []string{"Process", "Run"})
}

func TestTriggerPortsAreSmallAndSpecific(t *testing.T) {
	assertMethodNames(t, reflect.TypeFor[trigger.EventReader](), []string{"Pending"})
	assertMethodNames(t, reflect.TypeFor[trigger.CandidateReader](), []string{"Candidates"})
	assertMethodNames(t, reflect.TypeFor[trigger.SelectionReader](), []string{"Lookup", "Unfinished"})
	assertMethodNames(t, reflect.TypeFor[trigger.CompletionPort](), []string{"Complete"})
	assertMethodNames(t, reflect.TypeFor[trigger.AdmissionPort](), []string{"Admit"})
	assertMethodNames(t, reflect.TypeFor[trigger.StartPort](), []string{"Start"})
	assertMethodNames(t, reflect.TypeFor[trigger.Clock](), []string{"Now"})
	assertMethodNames(t, reflect.TypeFor[trigger.IDSource](), []string{
		"NewEventID", "NewExecutionID", "NewReceiptID",
	})
}

func TestResultCarriesPersistedSelectionAndReceipt(t *testing.T) {
	assertFieldNames(t, reflect.TypeFor[trigger.Result](), []string{
		"ExecutionID", "ReceiptID", "Status", "Replayed", "Selection", "Receipt",
	})
}

func TestStartUnitCarriesSelectionAdmissionAndAuthority(t *testing.T) {
	assertFieldNames(t, reflect.TypeFor[trigger.StartUnit](), []string{
		"Owner", "PipelineID", "EventID", "Selection", "Admission",
		"ExpectedPipelineVersion", "ExpectedBindingVersion", "Initiator",
		"ExecutionPrincipal", "NewExecutionID", "Status", "At", "Receipt",
		"Audit", "Events", "Authority", "PostCommit",
	})
}

func TestCandidateCarriesDefinitionAndCurrentBinding(t *testing.T) {
	assertFieldNames(t, reflect.TypeFor[trigger.Candidate](), []string{"Definition", "Binding"})
}

func TestCompletionUnitCarriesEventIdentity(t *testing.T) {
	assertFieldNames(t, reflect.TypeFor[trigger.CompletionUnit](), []string{
		"Owner", "Sequence", "EventID",
	})
}

func TestProcessResultCarriesOnlyDurableOutcomes(t *testing.T) {
	assertFieldNames(t, reflect.TypeFor[trigger.ProcessResult](), []string{
		"Started", "Refused", "Deferred", "Completed", "More",
	})
}

func TestReportCarriesExactRefAndCommits(t *testing.T) {
	assertFieldNames(t, reflect.TypeFor[trigger.Report](), []string{
		"Operation", "Resource", "Ref", "Commit", "PreviousCommit", "Deleted",
	})
}

func TestTriggerProductionImportsNoGitOrCommandExecution(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imported := range file.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			if strings.Contains(path, "userland/git") || path == "os/exec" {
				t.Errorf("%s imports %q; trigger must not discover Git or run commands", name, path)
			}
		}
	}
}

// TestTriggerAPINoteIsConsistent keeps the compile-time shapes above honest:
// every port value is also usable through its declared interface value.
func TestTriggerAPINoteIsConsistent(t *testing.T) {
	var _ trigger.EventReader = (*fakeEvents)(nil)
	var _ trigger.CandidateReader = (*fakeCandidates)(nil)
	var _ trigger.SelectionReader = (*fakeSelections)(nil)
	var _ trigger.AdmissionPort = (*fakeAdmission)(nil)
	var _ trigger.StartPort = (*fakeStart)(nil)
	var _ trigger.CompletionPort = (*fakeCompletion)(nil)
	var _ trigger.Clock = fakeClock{}
	var _ trigger.IDSource = (*fakeIDs)(nil)
	var _ trigger.SelectPort = (*fakeSelections)(nil)
	_ = event.Sequence(0)
	_ = pipeline.PipelineID("")
	_ = receipt.Record{}
	_ = resource.Binding{}
	_ = tenancy.Scope{}
	_ = authorize.AuthorityRead{}
}

func hasFieldName(typ reflect.Type, name string) bool {
	for field := range typ.Fields() {
		if field.Name == name {
			return true
		}
	}
	return false
}

func assertFieldNames(t *testing.T, typ reflect.Type, want []string) {
	t.Helper()
	got := make([]string, 0, typ.NumField())
	for field := range typ.Fields() {
		got = append(got, field.Name)
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(got, sorted) {
		t.Fatalf("%s fields = %v, want exactly %v", typ.Name(), got, sorted)
	}
}

func assertMethodNames(t *testing.T, typ reflect.Type, want []string) {
	t.Helper()
	got := make([]string, 0, typ.NumMethod())
	for method := range typ.Methods() {
		got = append(got, method.Name)
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(got, sorted) {
		t.Fatalf("%s methods = %v, want exactly %v", typ.Name(), got, sorted)
	}
}
