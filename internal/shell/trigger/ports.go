package trigger

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// SelectPort applies one complete push-selection commit unit.
//
// Select must check the expected pipeline/binding versions and the supplied
// current authority inside the same transaction as the receipt and selection
// write. Image resolution and the shared start operation run outside it; a
// recorded selection is never permission for a later effect. Select never
// commits a permanent refusal: a refusal is committed and published only
// through StartPort.Start, so a StatusRefused result (or a permanent refusal
// fault class) is routed through the start refusal path and is never left as an
// unpublished durable decision.
type SelectPort interface {
	Select(context.Context, SelectUnit) (Result, error)
}

// EventReader reads committed accepted event rows from the Shell event table in
// durable sequence order. The zero sequence means "from the beginning"; the
// processor always rescans from zero so a committed row can never be skipped.
//
// Pending is the one deliberately cross-scope read of the domain: the trigger
// processor scans accepted events for every project, so implementations run it
// under the store controller's read authority (role kodama_controller with
// events SELECT only) and never under a caller scope. The returned scope
// travels with each row and every subsequent selection, start or refusal write
// still commits under that owning scope; the scan is a read capability, never a
// durable permission.
type EventReader interface {
	Pending(context.Context, event.Sequence, int) ([]event.PendingEvent, error)
}

// CandidateReader returns the current pipeline definitions and bindings for one
// project scope. The processor matches only candidates in the event scope.
type CandidateReader interface {
	Candidates(context.Context, tenancy.Scope) ([]Candidate, error)
}

// SelectionReader reads the persisted first selections of one event. Lookup is
// consulted before any current definition, binding or service is used, so an
// edited definition can never rebuild an existing selection. Unfinished lists
// every persisted selection for the event that still has no durable result,
// including a selection whose current pipeline definition or binding no longer
// exists; the processor resumes or permanently refuses each one before it
// advances the event cursor.
type SelectionReader interface {
	Lookup(context.Context, tenancy.Scope, pipeline.PipelineID, id.EventID) (Result, bool, error)
	Unfinished(context.Context, tenancy.Scope, id.EventID) ([]Result, error)
}

// CompletionPort settles one fully processed accepted event row. It advances
// the durable event-table cursor and writes the event_consumptions marker keyed
// by the event id under CompletionConsumer, in one atomic unit: the marker's
// scope is CompletionUnit.Owner, its completed_at comes from the adapter's
// injected clock and its version is the completed event's durable sequence.
// Completing the same event twice applies nothing and is not an error. It is
// called only after every relevant selection has a durable result.
type CompletionPort interface {
	Complete(context.Context, CompletionUnit) error
}

// AdmissionPort is the shared admission seam. The processor supplies the
// persisted exact YAML bytes and inputs inline; admission never re-reads an
// edited definition.
type AdmissionPort interface {
	Admit(context.Context, pipeline.AdmitRequest) (pipeline.AdmissionResult, error)
}

// StartPort is the shared start seam. Start commits either the queued execution
// or a permanent refusal together with the common receipt and returns the
// persisted result. It is idempotent for one (pipeline, event) pair, so a lost
// response is retried without creating a second execution. A refusal for a
// persisted selection whose current definition or binding no longer exists
// carries zero expected versions and must be recorded without a version check.
// When Start records a refusal for a StatusSelected unit it must not commit the
// queued-execution event or its dispatch effect; it commits and publishes the
// canonical pipeline.start_refused event built by NewStartRefusedEvent with
// RefusalReasonAuthorityRefused instead. A unit that already carries
// StatusRefused carries that event directly.
type StartPort interface {
	Start(context.Context, StartUnit) (Result, error)
}

// Clock supplies the commit time.
type Clock interface {
	Now() time.Time
}

// IDSource supplies durable identities. A retry that finds a persisted result
// must never consume a fresh execution identity.
type IDSource interface {
	NewEventID() (id.EventID, error)
	NewReceiptID() (id.ReceiptID, error)
	NewExecutionID() (id.ExecutionID, error)
}
