package snapshotdoc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/request"
)

// documentVersion is the only document schema this codec reads or writes.
const documentVersion = 1

// document is the exact persisted executions.snapshot shape. See doc.go.
type document struct {
	Version         int                       `json:"version"`
	YAML            []byte                    `json:"yaml"`
	Snapshot        pipeline.Snapshot         `json:"snapshot"`
	Actions         []pipeline.AdmittedAction `json:"actions"`
	PipelineID      pipeline.PipelineID       `json:"pipeline_id,omitempty"`
	PipelineVersion uint64                    `json:"pipeline_version,omitempty"`
}

// Admitted is the minimal admitted input one start path owns: the immutable
// snapshot, its ordered admitted actions and the pipeline revision they came
// from. Encode and Decode both use it, so a writer and a reader agree on the
// whole admitted set.
type Admitted struct {
	PipelineID      pipeline.PipelineID
	PipelineVersion uint64
	Snapshot        pipeline.Snapshot
	Actions         []pipeline.AdmittedAction
}

// Encode renders the authoritative executions.snapshot document. It validates
// the admitted action list so a writer cannot persist a document a reader must
// refuse.
func Encode(admitted Admitted) ([]byte, error) {
	if len(admitted.Snapshot.YAMLBytes()) == 0 {
		return nil, fault.New(fault.InvalidInput, "admitted snapshot yaml is empty")
	}
	if err := validateActions(admitted.Actions); err != nil {
		return nil, err
	}
	document := document{
		Version:         documentVersion,
		YAML:            admitted.Snapshot.YAMLBytes(),
		Snapshot:        admitted.Snapshot,
		Actions:         append([]pipeline.AdmittedAction(nil), admitted.Actions...),
		PipelineID:      admitted.PipelineID,
		PipelineVersion: admitted.PipelineVersion,
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "execution snapshot could not be encoded", err)
	}
	return encoded, nil
}

// Decode rebuilds the admitted input from a stored document. Malformed,
// truncated, tampered or unknown-version documents are internal_failure: a
// stored snapshot is never silently dropped or partially accepted.
func Decode(data []byte) (Admitted, error) {
	if len(data) == 0 {
		return Admitted{}, fault.New(fault.InternalFailure, "execution snapshot row is empty")
	}
	var document document
	if err := json.Unmarshal(data, &document); err != nil {
		return Admitted{}, fault.Wrap(fault.InternalFailure, "execution snapshot row is malformed", err)
	}
	if document.Version != documentVersion {
		return Admitted{}, fault.New(fault.InternalFailure, fmt.Sprintf("execution snapshot version %d is not supported", document.Version))
	}
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:         document.YAML,
		Inputs:       document.Snapshot.Inputs,
		Env:          document.Snapshot.Env,
		Images:       document.Snapshot.Images,
		SecretNames:  document.Snapshot.SecretNames,
		Requirements: document.Snapshot.Requirements,
		TaskID:       document.Snapshot.TaskID,
		Conversation: document.Snapshot.Conversation,
		References:   document.Snapshot.References,
		Metadata:     document.Snapshot.Metadata,
	})
	if err != nil {
		return Admitted{}, fault.Wrap(fault.InternalFailure, "execution snapshot could not be rebuilt", err)
	}
	if snapshot.YAMLSHA256 != document.Snapshot.YAMLSHA256 {
		return Admitted{}, fault.New(fault.InternalFailure, "execution snapshot does not match its yaml digest")
	}
	if err := validateActions(document.Actions); err != nil {
		return Admitted{}, fault.Wrap(fault.InternalFailure, "execution snapshot actions are malformed", err)
	}
	return Admitted{
		PipelineID:      document.PipelineID,
		PipelineVersion: document.PipelineVersion,
		Snapshot:        snapshot,
		Actions:         append([]pipeline.AdmittedAction(nil), document.Actions...),
	}, nil
}

// validateActions refuses an empty, non-contiguous or invalid admitted action
// list. A started execution always has at least one admitted action.
func validateActions(actions []pipeline.AdmittedAction) error {
	if len(actions) == 0 {
		return fault.New(fault.InvalidInput, "admitted action list is empty")
	}
	for position, action := range actions {
		if err := action.Validate(); err != nil {
			return err
		}
		if action.Index != position {
			return fault.New(fault.InvalidInput, "admitted actions must be contiguous and ordered from index zero")
		}
	}
	return nil
}

// ActionRow is the actions-table projection of one admitted action.
type ActionRow struct {
	ID     string
	Values map[string]any
}

// ActionRows renders the ordered actions rows one start writes beside the
// snapshot document. The container or request definition has no actions
// column, so it is not projected here: it lives in the snapshot document and
// Load pairs the row with the admitted action at the same index.
//
// A request row carries its immutable admitted assignment from the moment it is
// inserted. The Kernel reconciler activates a request by committing a
// flow.Commit through the state port rather than through the Shell activation
// port, so this insert is the only writer that can persist the assignment
// before the row becomes answerable; a later activation must never be the first
// writer of an immutable admitted fact.
func ActionRows(executionID id.ExecutionID, actions []pipeline.AdmittedAction) ([]ActionRow, error) {
	if err := validateActions(actions); err != nil {
		return nil, err
	}
	rows := make([]ActionRow, len(actions))
	for position, action := range actions {
		values := map[string]any{
			"execution_id":       string(executionID),
			"index":              int64(action.Index),
			"name":               string(action.Name),
			"kind":               string(action.Kind),
			"state":              string(kernel.ActionPending),
			"version":            int64(1),
			"timeout_ms":         action.Timeout.Milliseconds(),
			"staged_outputs_set": false,
			"launch_state":       string(kernel.LaunchNotIssued),
		}
		assignment, err := RequestAssignment(action)
		if err != nil {
			return nil, err
		}
		if assignment != nil {
			values["request_assignment"] = assignment
		}
		rows[position] = ActionRow{
			ID:     string(DeriveActionID(executionID, action.Index)),
			Values: values,
		}
	}
	return rows, nil
}

// RequestAssignment encodes the immutable assignment of one admitted request
// action into the exact actions.request_assignment JSON shape. A container
// action has no request assignment and returns nil; the caller persists SQL
// NULL by omitting the column.
func RequestAssignment(action pipeline.AdmittedAction) ([]byte, error) {
	if action.Kind != definition.ActionKindRequest {
		return nil, nil
	}
	if action.Request == nil {
		return nil, fault.New(fault.InvalidInput, "admitted request action has no request declaration")
	}
	assignment, err := request.ParseAssignment(action.Request.Assignment)
	if err != nil {
		return nil, err
	}
	return EncodeAssignment(assignment)
}

// EncodeAssignment is the single encoder of the actions.request_assignment
// column. The start insert, the Kernel activation commit and the Shell
// activation port all use it, so every writer persists byte-identical JSON for
// the same immutable assignment.
func EncodeAssignment(assignment request.Assignment) ([]byte, error) {
	if !assignment.Valid() {
		return nil, fault.New(fault.InvalidInput, "request assignment is invalid")
	}
	encoded, err := json.Marshal(assignment)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "request assignment could not be encoded", err)
	}
	return encoded, nil
}

// DeriveActionID is the durable actions.id of one admitted action. See doc.go
// for the exact derivation.
func DeriveActionID(executionID id.ExecutionID, index int) id.ActionID {
	sum := sha256.Sum256([]byte(string(executionID) + "\x00action\x00" + fmt.Sprintf("%d", index)))
	return id.ActionID("act-" + hex.EncodeToString(sum[:16]))
}

// DeriveWorkspaceID is the durable executions.workspace_id of one execution.
// See doc.go for the exact derivation.
func DeriveWorkspaceID(executionID id.ExecutionID) id.WorkspaceID {
	sum := sha256.Sum256([]byte(string(executionID) + "\x00workspace"))
	return id.WorkspaceID("ws-" + hex.EncodeToString(sum[:16]))
}
