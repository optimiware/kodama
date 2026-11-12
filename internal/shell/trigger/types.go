// Package trigger declares the exact push selection and its atomic commit
// unit.
//
// The push processor scans accepted events in the Shell event table, not broker
// delivery. For each (pipeline id, event id) one common receipt freezes the
// event content identity, the first selected YAML, the automatic-start service,
// the supplied inputs and the eventual result. Definition/binding edits cannot
// change a replay selection, and a stored selection never becomes a permission
// cache: current authority is rechecked before any new effect.
package trigger

import (
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Selection is the immutable first selection for one (pipeline, event) pair.
type Selection struct {
	PipelineID       pipeline.PipelineID    `json:"pipeline_id"`
	EventID          id.EventID             `json:"event_id"`
	YAML             []byte                 `json:"-"`
	ServiceID        service.ServiceID      `json:"service_id"`
	Inputs           map[id.InputKey]string `json:"inputs,omitempty"`
	EventContentHash [32]byte               `json:"event_content_hash"`
}

// Status is the recorded state of one push selection result.
type Status string

const (
	// StatusSelected means the selection was committed and the shared start
	// operation may run.
	StatusSelected Status = "selected"
	// StatusRefused means a permanent refusal was recorded and published.
	StatusRefused Status = "refused"
	// StatusDeferred means a transient dependency failed; the selection is
	// resumable and the cursor must not advance past it.
	StatusDeferred Status = "deferred"
)

var statuses = [...]Status{StatusSelected, StatusRefused, StatusDeferred}

// Valid reports whether the status is one of the declared statuses.
func (status Status) Valid() bool {
	for _, candidate := range statuses {
		if status == candidate {
			return true
		}
	}
	return false
}

// RefusalReason is the stable reason class published with a permanent push
// start refusal. It is either one of the explicit trigger classes below or the
// stable fault class of the permanent admission/selection fault that caused the
// refusal. It never carries free-form error text.
type RefusalReason string

const (
	// RefusalReasonDefinitionMissing reports a persisted first selection whose
	// current pipeline definition or binding no longer exists. It is the only
	// reason published with subject version 0.
	RefusalReasonDefinitionMissing RefusalReason = "definition_missing"
	// RefusalReasonSelectionRefused reports a permanent refusal returned by the
	// selection commit. Select never commits a refusal, so the processor
	// records it through StartPort.Start.
	RefusalReasonSelectionRefused RefusalReason = "selection_refused"
	// RefusalReasonAuthorityRefused reports a current membership, service
	// enablement, pipeline/binding version or event-read authority refusal
	// detected inside the start commit.
	RefusalReasonAuthorityRefused RefusalReason = "authority_refused"
)

// Valid reports whether the reason is a stable, permanent refusal class.
func (reason RefusalReason) Valid() bool {
	switch reason {
	case RefusalReasonDefinitionMissing, RefusalReasonSelectionRefused, RefusalReasonAuthorityRefused:
		return true
	}
	return permanentFaultClass(fault.Class(reason))
}

// ReceiptMode names how one trigger commit unit uses the common receipt of its
// (pipeline, event) pair. The push selection inserts it; the shared start (or
// its permanent refusal) completes the same key under an explicit completion
// mode, guarded on the receipt's prior state and version. An equal-key replay
// of a completed receipt applies nothing and returns the original result, and
// completion with different content is refused with
// changed_idempotency_content.
type ReceiptMode string

const (
	// ReceiptInsert inserts the first common receipt for the pair.
	ReceiptInsert ReceiptMode = "insert"
	// ReceiptComplete completes the previously inserted common receipt.
	ReceiptComplete ReceiptMode = "complete"
)

// Valid reports whether the mode is one of the declared modes.
func (mode ReceiptMode) Valid() bool {
	return mode == ReceiptInsert || mode == ReceiptComplete
}

// CompletionConsumer is the stable consumer identity written to the
// event_consumptions marker row. The marker's id is the event id, its scope
// comes from CompletionUnit.Owner, its completed_at comes from the adapter's
// injected clock and its version is the completed event's durable sequence
// (CompletionUnit.Sequence), so a replayed completion is idempotent.
const CompletionConsumer = "trigger"

// SelectUnit is the complete atomic push-selection commit unit.
type SelectUnit struct {
	Owner                   tenancy.Scope             `json:"owner"`
	PipelineID              pipeline.PipelineID       `json:"pipeline_id"`
	EventID                 id.EventID                `json:"event_id"`
	ExpectedPipelineVersion uint64                    `json:"expected_pipeline_version"`
	ExpectedBindingVersion  uint64                    `json:"expected_binding_version"`
	Selection               Selection                 `json:"selection"`
	Receipt                 receipt.Record            `json:"receipt"`
	Audit                   []event.Audit             `json:"audit"`
	Events                  []event.Envelope          `json:"events"`
	Authority               []authorize.AuthorityRead `json:"authority"`
	PostCommit              []event.PostCommitEffect  `json:"post_commit"`
}

// ReceiptMode reports that the selection inserts the common receipt. The shared
// start completes that same key, so the two units never insert two receipts for
// one (pipeline, event) pair.
func (SelectUnit) ReceiptMode() ReceiptMode {
	return ReceiptInsert
}

// Result is the committed push selection result. Selection is the immutable
// first selection that was persisted with Receipt, the common receipt shared
// by the selection and its eventual start or refusal.
type Result struct {
	ExecutionID id.ExecutionID `json:"execution_id,omitempty"`
	ReceiptID   id.ReceiptID   `json:"receipt_id,omitempty"`
	Status      Status         `json:"status"`
	Replayed    bool           `json:"replayed"`
	Selection   Selection      `json:"selection"`
	Receipt     receipt.Record `json:"receipt"`
}

// Candidate is one current pipeline definition together with its current
// binding. The definition carries the exact authored YAML bytes; the binding
// carries the current version, scope, resource and reporting service.
type Candidate struct {
	Definition pipeline.Definition `json:"definition"`
	Binding    resource.Binding    `json:"binding"`
}

// StartUnit is the complete atomic push start commit unit. It carries the
// persisted first selection, its common receipt, the admission result, the
// current pipeline/binding versions and the current automatic-service
// authority reads. Status is StatusSelected for a normal start or
// StatusRefused for a permanent admission/selection/orphan refusal; a refused
// unit carries a zero Admission result and its Events entry is the canonical
// pipeline.start_refused envelope. A StatusSelected unit carries the
// execution.queued event and dispatch effect; if the start commit refuses on
// current authority it must not commit those, and instead commits the
// pipeline.start_refused envelope built by NewStartRefusedEvent with
// RefusalReasonAuthorityRefused.
type StartUnit struct {
	Owner                   tenancy.Scope             `json:"owner"`
	PipelineID              pipeline.PipelineID       `json:"pipeline_id"`
	EventID                 id.EventID                `json:"event_id"`
	Selection               Selection                 `json:"selection"`
	Admission               pipeline.AdmissionResult  `json:"admission"`
	ExpectedPipelineVersion uint64                    `json:"expected_pipeline_version"`
	ExpectedBindingVersion  uint64                    `json:"expected_binding_version"`
	Initiator               id.PrincipalID            `json:"initiator"`
	ExecutionPrincipal      id.PrincipalID            `json:"execution_principal"`
	NewExecutionID          id.ExecutionID            `json:"new_execution_id,omitempty"`
	Status                  Status                    `json:"status"`
	At                      time.Time                 `json:"at"`
	Receipt                 receipt.Record            `json:"receipt"`
	Audit                   []event.Audit             `json:"audit"`
	Events                  []event.Envelope          `json:"events"`
	Authority               []authorize.AuthorityRead `json:"authority"`
	PostCommit              []event.PostCommitEffect  `json:"post_commit"`
}

// ReceiptMode reports that the start completes the common receipt inserted by
// the selection (or by the earlier refusal path) under the same key. A
// completed-receipt replay applies nothing and returns the original result; a
// changed-content completion is refused with changed_idempotency_content.
func (StartUnit) ReceiptMode() ReceiptMode {
	return ReceiptComplete
}

// CompletionUnit settles one fully processed accepted event row: it advances
// the durable cursor and writes the event_consumptions marker keyed by the
// event id under the CompletionConsumer identity. The adapter derives
// completed_at from its injected clock and stores Sequence as the marker
// version, so completing the same event twice is idempotent.
type CompletionUnit struct {
	Owner    tenancy.Scope  `json:"owner"`
	Sequence event.Sequence `json:"sequence"`
	EventID  id.EventID     `json:"event_id"`
}

// ProcessResult reports one bounded processor pass. Started lists durable
// queued executions, Refused lists durable permanent refusals, Deferred lists
// events retained for a later pass, Completed lists advanced event sequences
// and More reports that another pass may have work.
type ProcessResult struct {
	Started   []id.ExecutionID `json:"started,omitempty"`
	Refused   []id.EventID     `json:"refused,omitempty"`
	Deferred  []id.EventID     `json:"deferred,omitempty"`
	Completed []event.Sequence `json:"completed,omitempty"`
	More      bool             `json:"more"`
}
