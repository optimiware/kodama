// Package event_test pins the event surface these tests rely on: the durable
// Envelope, Audit, Source, Type and PostCommitEffect values, the envelope
// constructor, the custom-observation ingress commit unit/port/orchestrator and
// the durable publication worker. The event table is the durable stream; the
// worker only reads already-committed rows and can never touch domain state.
//
// Envelope constructor:
//
//	type event.EnvelopeInput struct {
//	    Type           event.Type
//	    ReportType     event.ReportType
//	    SubjectID      string
//	    SubjectVersion uint64
//	    CorrelationID  string
//	    CausationID    string
//	    Data           []byte
//	}
//	func event.NewEnvelope(eventID id.EventID, source event.Source, scope tenancy.Scope, at time.Time, input event.EnvelopeInput) (event.Envelope, error)
//
// NewEnvelope applies the trusted source, scope and time supplied by the
// caller, never from EnvelopeInput. It canonicalizes an input lifecycle alias
// through api.CanonicalEventType (request.activated -> action.requested) and
// refuses an unknown type. It keeps the supplied stable event ID. Report-type
// separation: only resource.verified may carry a ReportType, the report type
// must pass api.ReportTypeValid and must not be a lifecycle type or alias, and
// every other type must leave ReportType empty. Data is bounded by
// event.DefaultBounds.MaxDataBytes (64 KiB): the exact bound is accepted and one
// byte more is refused with fault.InvalidInput.
//
// Custom-observation ingress (registry operation event.publish):
//
//	type event.IngressUnit struct {
//	    Owner       tenancy.Scope
//	    ExecutionID id.ExecutionID
//	    Event       event.Envelope
//	    Receipt     receipt.Record
//	    Audit       []event.Audit
//	    Authority   []authorize.AuthorityRead
//	    PostCommit  []event.PostCommitEffect
//	}
//	func (event.IngressUnit) Validate() error
//	type event.IngressResult struct {
//	    EventID   id.EventID
//	    ReceiptID id.ReceiptID
//	    Replayed  bool
//	}
//	type event.IngressPort interface {
//	    Ingest(context.Context, event.IngressUnit) (event.IngressResult, error)
//	}
//	type event.Clock interface{ Now() time.Time }
//	type event.PublishAttempt struct {
//	    Credential     string
//	    CallerKey      string
//	    Content        receipt.Content
//	    Type           string
//	    Scope          tenancy.Scope
//	    Source         event.Source
//	    SubjectID      string
//	    SubjectVersion uint64
//	    CorrelationID  string
//	    CausationID    string
//	    Data           []byte
//	}
//	type event.IngressConfig struct {
//	    Credentials  authorize.CurrentPrincipal
//	    Authority    authorize.AuthorityReader
//	    Authorizer   authorize.Authorizer
//	    Port         event.IngressPort
//	    Clock        event.Clock
//	    NewEventID   func() (id.EventID, error)
//	    NewReceiptID func() (id.ReceiptID, error)
//	}
//	func event.NewIngress(event.IngressConfig) (*event.Ingress, error)
//	func (*event.Ingress) Publish(context.Context, event.PublishAttempt) (event.IngressResult, error)
//
// The unit contains no action or execution state write: a custom observation
// can never settle or advance a request. Its only post-commit effect is
// publishing the committed observation. IngressUnit.Validate refuses a unit
// whose event is not observation.custom, whose report type is set, whose source
// is not a declared authenticated source kind matching the actor, whose
// owner/scope disagree, whose execution source lacks its execution id or whose
// non-execution source carries one, whose receipt operation is not event.publish
// or lacks a content hash, whose authority reads are not the applicable
// event.publish checks (membership always; execution_running only for an
// execution source), whose audit is empty or names another operation, or whose
// post-commit effect is not a publish.
//
// Durable publication worker:
//
//	type event.Sequence uint64
//	type event.PendingEvent struct {
//	    Sequence event.Sequence
//	    Event    event.Envelope
//	}
//	type event.Outbox interface {
//	    Pending(context.Context, event.Sequence, int) ([]event.PendingEvent, error)
//	    MarkPublished(context.Context, id.EventID, event.Sequence) error
//	}
//	type event.DispatcherConfig struct {
//	    Outbox    event.Outbox
//	    Publisher event.Publisher
//	    BatchSize int
//	    Idle      func(context.Context) error
//	}
//	func event.NewDispatcher(event.DispatcherConfig) (*event.Dispatcher, error)
//	type event.DispatchResult struct {
//	    Published []id.EventID
//	    Failed    *id.EventID
//	    More      bool
//	}
//	func (*event.Dispatcher) Dispatch(context.Context) (event.DispatchResult, error)
//	func (*event.Dispatcher) Run(context.Context) error
//
// Dispatch performs one bounded pass: it reads at most BatchSize committed
// unacknowledged rows from the start of the outbox, publishes them in sequence
// order, and calls MarkPublished with the row's own identity and sequence only
// after Publish succeeds. It keeps no in-memory high-water mark, so a row whose
// transaction commits late with a lower sequence than an already-acknowledged row
// is still returned and published; MarkPublished, not a cursor, is what removes a
// settled row. A publish or acknowledgement failure stops the pass without
// skipping the row, reports it in Failed and leaves More true, and the next pass
// re-publishes the same event ID (at-least-once). Published lists only fully
// settled rows. More is true for a full batch or a failed pass. Run repeats
// Dispatch, calls Idle between passes when supplied, tolerates broker and read
// failures, and returns ctx.Err() after cancellation while joining with no leaked
// goroutine. The dispatcher holds no domain writer: broker failure cannot roll
// back domain progress, and only committed rows returned by the outbox can ever
// be published.
package event_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/event"
)

// Compile-time port shapes.
var (
	_ event.Outbox      = (*fakeOutbox)(nil)
	_ event.IngressPort = (*fakeIngressPort)(nil)
	_ event.Publisher   = (*fakePublisher)(nil)
	_ event.Clock       = fakeClock{}
)

// TestEventEnvelopeInputCarriesNoSourceOrScope asserts the untrusted envelope
// input cannot carry the authenticated source or ownership scope: both are
// trusted constructor parameters.
func TestEventEnvelopeInputCarriesNoSourceOrScope(t *testing.T) {
	assertFieldNames(t, reflect.TypeFor[event.EnvelopeInput](), []string{
		"Type", "ReportType", "SubjectID", "SubjectVersion",
		"CorrelationID", "CausationID", "Data",
	})
}

// TestEventIngressUnitCarriesNoDomainStateWrites asserts a custom observation
// commit unit contains only its event, receipt, audit, authority reads and
// publication intent. There is no action/execution state write, so an
// observation can never settle an action.
func TestEventIngressUnitCarriesNoDomainStateWrites(t *testing.T) {
	assertFieldNames(t, reflect.TypeFor[event.IngressUnit](), []string{
		"Owner", "ExecutionID", "Event", "Receipt", "Audit", "Authority", "PostCommit",
	})
}

// TestEventIngressResultCarriesNoSettlement asserts the ingress result reports
// only the committed identities and replay fact, never a decision or state
// transition.
func TestEventIngressResultCarriesNoSettlement(t *testing.T) {
	assertFieldNames(t, reflect.TypeFor[event.IngressResult](), []string{
		"EventID", "ReceiptID", "Replayed",
	})
}

// TestEventDispatcherConfigHasNoDomainWriter asserts the publication worker is
// composed only from the durable read seam and the broker publisher. It has no
// domain store, so a broker failure cannot roll back domain progress.
func TestEventDispatcherConfigHasNoDomainWriter(t *testing.T) {
	assertFieldNames(t, reflect.TypeFor[event.DispatcherConfig](), []string{
		"Outbox", "Publisher", "BatchSize", "Idle",
	})
}

// TestEventPortsAreSmall asserts the publication seam exposes only a bounded
// read and an acknowledgement, and the ingress seam only the atomic commit.
func TestEventPortsAreSmall(t *testing.T) {
	assertMethodNames(t, reflect.TypeFor[event.Outbox](), []string{
		"MarkPublished", "Pending",
	})
	assertMethodNames(t, reflect.TypeFor[event.IngressPort](), []string{
		"Ingest",
	})
	assertMethodNames(t, reflect.TypeFor[event.Publisher](), []string{
		"Publish",
	})
}

// TestEventGeneratedMetadataIsSecretFree asserts no generated envelope, audit,
// unit, result or publication value exposes a credential, token, secret,
// password or plaintext field.
func TestEventGeneratedMetadataIsSecretFree(t *testing.T) {
	roots := []reflect.Type{
		reflect.TypeFor[event.Envelope](),
		reflect.TypeFor[event.Audit](),
		reflect.TypeFor[event.EnvelopeInput](),
		reflect.TypeFor[event.IngressUnit](),
		reflect.TypeFor[event.IngressResult](),
		reflect.TypeFor[event.PostCommitEffect](),
		reflect.TypeFor[event.PendingEvent](),
		reflect.TypeFor[event.DispatchResult](),
	}
	for _, root := range roots {
		assertNoSecretFields(t, root, root.Name(), 0, map[reflect.Type]bool{})
	}
}

var secretFieldMarkers = []string{"credential", "token", "secret", "password", "plaintext"}

func assertNoSecretFields(t *testing.T, typ reflect.Type, path string, depth int, seen map[reflect.Type]bool) {
	t.Helper()
	if depth > 6 {
		return
	}
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || seen[typ] {
		return
	}
	seen[typ] = true
	for field := range typ.Fields() {
		if field.PkgPath != "" {
			continue
		}
		lower := strings.ToLower(field.Name)
		for _, marker := range secretFieldMarkers {
			if strings.Contains(lower, marker) {
				t.Errorf("%s.%s exposes %q in generated metadata", path, field.Name, marker)
			}
		}
		assertNoSecretFields(t, field.Type, path+"."+field.Name, depth+1, seen)
	}
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
