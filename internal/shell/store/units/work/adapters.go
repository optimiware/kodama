package work

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Task adapts the task commit-unit ports and the task reader.
type Task struct {
	executor store.Executor
	scanner  store.Scanner
}

// NewTask validates the neutral seams and returns the task adapter.
func NewTask(executor store.Executor, scanner store.Scanner) (*Task, error) {
	if executor == nil || scanner == nil {
		return nil, fault.New(fault.InternalFailure, "task store adapter requires an executor and a scanner")
	}
	return &Task{executor: executor, scanner: scanner}, nil
}

// Inbox adapts the read-only inbox query.
type Inbox struct {
	executor store.Executor
	scanner  store.Scanner
}

// NewInbox validates the neutral seams and returns the inbox adapter.
func NewInbox(executor store.Executor, scanner store.Scanner) (*Inbox, error) {
	if executor == nil || scanner == nil {
		return nil, fault.New(fault.InternalFailure, "inbox store adapter requires an executor and a scanner")
	}
	return &Inbox{executor: executor, scanner: scanner}, nil
}

// Pipelines adapts pipeline management commits. newID allocates the identity of
// a new apply because ChangeUnit carries no pipeline identity for a first
// create; it defaults to secure randomness.
//
// pipeline.Reader is deliberately absent: the stored on.push trigger lives only
// inside the YAML bytes, the pipeline package exposes no push decoder and the
// adapter layer may not import go.yaml.in/yaml/v3, so a Definition with a
// non-nil Push cannot be reconstructed without duplicating decode policy.
type Pipelines struct {
	executor store.Executor
	scanner  store.Scanner
	newID    func() (pipeline.PipelineID, error)
}

// NewPipelines validates the neutral seam and returns the pipeline adapter.
// A nil newID uses crypto/rand.
func NewPipelines(executor store.Executor, scanner store.Scanner, newID func() (pipeline.PipelineID, error)) (*Pipelines, error) {
	if executor == nil || scanner == nil {
		return nil, fault.New(fault.InternalFailure, "pipeline store adapter requires an executor and a scanner")
	}
	if newID == nil {
		newID = randomPipelineID
	}
	return &Pipelines{executor: executor, scanner: scanner, newID: newID}, nil
}

// Triggers adapts the trigger event, selection and completion ports. The event
// reader runs under the controller's cross-scope read authority; every
// follow-up write commits under the scope carried by the event row.
type Triggers struct {
	executor   store.Executor
	scanner    store.Scanner
	controller store.Controller
	clock      func() time.Time
	newEventID func() (id.EventID, error)
}

// NewTriggers validates the neutral seams and returns the trigger adapter. A nil
// clock uses time.Now; a nil newEventID uses crypto/rand.
func NewTriggers(executor store.Executor, scanner store.Scanner, controller store.Controller, clock func() time.Time, newEventID func() (id.EventID, error)) (*Triggers, error) {
	if executor == nil || scanner == nil || controller == nil {
		return nil, fault.New(fault.InternalFailure, "trigger store adapter requires an executor, a scanner and a controller")
	}
	if clock == nil {
		clock = time.Now
	}
	if newEventID == nil {
		newEventID = randomEventID
	}
	return &Triggers{executor: executor, scanner: scanner, controller: controller, clock: clock, newEventID: newEventID}, nil
}

// randomEventID returns a secure random event identity.
func randomEventID() (id.EventID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fault.Wrap(fault.InternalFailure, "event identity allocation failed", err)
	}
	return id.EventID("event-" + hex.EncodeToString(raw[:])), nil
}

// randomPipelineID returns a secure random pipeline identity.
func randomPipelineID() (pipeline.PipelineID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fault.Wrap(fault.InternalFailure, "pipeline identity allocation failed", err)
	}
	return pipeline.PipelineID("pipeline-" + hex.EncodeToString(raw[:])), nil
}

// execute commits one complete unit.
func execute(ctx context.Context, executor store.Executor, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	return executor.Execute(ctx, scope, unit)
}

// withResult stores the durable domain result on the receipt so an equal retry
// can return the original result without re-applying the unit.
func withResult(record receipt.Record, result any) (receipt.Record, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return receipt.Record{}, fault.Wrap(fault.InternalFailure, "commit result cannot be encoded", err)
	}
	record.Result = encoded
	return record, nil
}

// replay decodes a replayed unit's original durable result. A replay without a
// usable stored result is an internal failure: an unreadable receipt is never
// reported as a fresh success.
func replay[T any](result store.UnitResult) (T, bool, error) {
	var zero T
	if !result.Replayed {
		return zero, false, nil
	}
	if len(result.Receipt.Result) == 0 {
		return zero, false, fault.New(fault.InternalFailure, "a replayed receipt has no durable result")
	}
	var decoded T
	if err := json.Unmarshal(result.Receipt.Result, &decoded); err != nil {
		return zero, false, fault.Wrap(fault.InternalFailure, "a replayed receipt result is not readable", err)
	}
	return decoded, true, nil
}

// jsonColumn encodes one JSON object/array column. A nil value stays SQL NULL.
func jsonColumn(value any) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "a JSON column value cannot be encoded", err)
	}
	return encoded, nil
}

// decodeJSON decodes one JSON column. A NULL column leaves the target zero.
func decodeJSON[T any](raw []byte) (T, error) {
	var value T
	if len(raw) == 0 {
		return value, nil
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return value, fault.Wrap(fault.InternalFailure, "a stored JSON column is not readable", err)
	}
	return value, nil
}

// nullableText renders an empty identity as SQL NULL, matching the nullable
// identity columns (pipeline_id, current_execution_id, question_id, ...).
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// textValue reads a text column, mapping NULL to the empty string.
func textValue(row store.Row, name string) string {
	value, _ := row.Values[name].(string)
	return value
}

// intValue reads an integer column, mapping NULL to zero.
func intValue(row store.Row, name string) int64 {
	switch typed := row.Values[name].(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		return 0
	}
}

// versionValue reads one optimistic version column.
func versionValue(row store.Row, name string) uint64 {
	value := intValue(row, name)
	if value < 0 {
		return 0
	}
	return uint64(value)
}

// bytesValue reads a bytea/jsonb column, preserving nil.
func bytesValue(row store.Row, name string) []byte {
	value, _ := row.Values[name].([]byte)
	return value
}

// timeValue reads a timestamptz column, mapping NULL to the zero time.
func timeValue(row store.Row, name string) time.Time {
	value, _ := row.Values[name].(time.Time)
	return value
}

// boolValue reads a boolean column, mapping NULL to false.
func boolValue(row store.Row, name string) bool {
	value, _ := row.Values[name].(bool)
	return value
}

// readRow reads one scoped row and refuses a missing row with not_found.
func readRow(ctx context.Context, executor store.Executor, scope tenancy.Scope, table, identity string) (store.Row, error) {
	return executor.Read(ctx, scope, table, identity)
}

// scanAll returns every row of one deterministic scan, following the keyset
// cursor. The caller supplies a stable order.
func scanAll(ctx context.Context, scanner store.Scanner, scope tenancy.Scope, query store.ScanQuery) ([]store.Row, error) {
	if query.Limit <= 0 || query.Limit > store.MaxQueryLimit {
		query.Limit = store.MaxQueryLimit
	}
	var rows []store.Row
	for {
		page, err := scanner.Query(ctx, scope, query)
		if err != nil {
			return nil, err
		}
		rows = append(rows, page.Rows...)
		if !page.HasMore || page.Next == "" {
			return rows, nil
		}
		query.After = page.Next
	}
}

// validProjectScope refuses a tenant-wide scope for a project-required adapter path.
func validProjectScope(scope tenancy.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !scope.ProjectScoped() {
		return fault.New(fault.InvalidInput, "this operation requires a project scope")
	}
	return nil
}

// validateReceipt refuses a unit without a receipt identity. A commit unit that
// silently commits without its idempotency record is never accepted.
func validateReceipt(record receipt.Record) error {
	if record.ID == "" {
		return fault.New(fault.InvalidInput, "commit unit receipt is missing")
	}
	if err := record.Key.Validate(); err != nil {
		return err
	}
	return nil
}

// requireNonEmpty refuses an empty string field a unit must carry.
func requireNonEmpty(kind, value string) error {
	if value == "" {
		return fault.New(fault.InvalidInput, kind+" is empty")
	}
	return nil
}
