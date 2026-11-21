package executionstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/shell/workspace"
)

// Table names come only from the declared schema design.
const (
	tableExecutions        = "executions"
	tableActions           = "actions"
	tableCleanup           = "cleanup"
	tablePipelines         = "pipelines"
	tableTasks             = "tasks"
	tableTaskMessages      = "task_messages"
	tableServices          = "services"
	tableBindings          = "resource_bindings"
	tableMemberships       = "memberships"
	tableResources         = "resources"
	tableApprovalDecisions = store.ApprovalDecisionsTable
)

// answerMetaMagic distinguishes a structured answer metadata envelope (carrying
// a decision note and optional raw provenance bytes) from legacy raw provenance
// bytes stored directly in actions.answer_provenance.
var answerMetaMagic = []byte("kodema:answer-meta:v1\x00")

type answerMetaEnvelope struct {
	Note       string `json:"note,omitempty"`
	Provenance []byte `json:"provenance,omitempty"`
}

// encodeAnswerProvenance renders one answer's optional decision note and
// provenance bytes into actions.answer_provenance. When note is empty and
// provenance does not collide with the magic prefix, the raw provenance bytes
// are stored verbatim so existing tests and rows stay byte-identical.
func encodeAnswerProvenance(note string, provenance []byte) ([]byte, error) {
	if note == "" && !bytes.HasPrefix(provenance, answerMetaMagic) {
		if len(provenance) == 0 {
			return nil, nil
		}
		return append([]byte(nil), provenance...), nil
	}
	encoded, err := json.Marshal(answerMetaEnvelope{
		Note:       note,
		Provenance: provenance,
	})
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "answer metadata could not be encoded", err)
	}
	return slices.Concat(answerMetaMagic, encoded), nil
}

// decodeAnswerProvenance rebuilds an answer's optional decision note and raw
// provenance bytes from actions.answer_provenance.
func decodeAnswerProvenance(raw []byte) (string, []byte, error) {
	if len(raw) == 0 {
		return "", nil, nil
	}
	if !bytes.HasPrefix(raw, answerMetaMagic) {
		return "", append([]byte(nil), raw...), nil
	}
	var envelope answerMetaEnvelope
	if err := json.Unmarshal(raw[len(answerMetaMagic):], &envelope); err != nil {
		return "", nil, fault.Wrap(fault.InternalFailure, "answer metadata is malformed", err)
	}
	return envelope.Note, envelope.Provenance, nil
}

// decodeSnapshot rebuilds the immutable kernel snapshot from the shared
// snapshot document. A malformed document is an internal_failure, never an
// empty snapshot: admitted bytes are never silently dropped.
func decodeSnapshot(data []byte) (*definition.Snapshot, error) {
	admitted, err := snapshotdoc.Decode(data)
	if err != nil {
		return nil, err
	}
	return definitionSnapshot(admitted)
}

// definitionSnapshot maps the shared admitted snapshot into the Kernel's
// immutable definition snapshot. The admitted action definitions come from the
// document because the schema has no action-definition columns.
func definitionSnapshot(admitted snapshotdoc.Admitted) (*definition.Snapshot, error) {
	snapshot := admitted.Snapshot
	inputs := make(map[string]string, len(snapshot.Inputs))
	keys := make([]id.InputKey, 0, len(snapshot.Inputs))
	for key, value := range snapshot.Inputs {
		keys = append(keys, key)
		inputs[string(key)] = value
	}
	slices.Sort(keys)
	actions := make([]definition.Action, len(admitted.Actions))
	for index, action := range admitted.Actions {
		actions[index] = definition.Action{
			Name:      action.Name,
			Timeout:   action.Timeout,
			Container: action.Container,
			Request:   action.Request,
		}
	}
	var taskFiles map[string][]byte
	if snapshot.TaskID != "" && snapshot.HasConversation() {
		var err error
		taskFiles, err = workspace.BuildTaskFilesWithParent(snapshot.TaskID, snapshot.ParentID, snapshot.Title, snapshot.Description, snapshot.Metadata)
		if err != nil {
			return nil, err
		}
	}
	return definition.NewSnapshot(definition.SnapshotInput{
		Pipeline: definition.Pipeline{
			Inputs:  keys,
			Env:     append([]definition.EnvVar(nil), snapshot.Env...),
			Budget:  snapshot.Budget,
			Actions: actions,
		},
		YAML:       snapshot.YAMLBytes(),
		YAMLDigest: snapshot.YAMLSHA256,
		Inputs:     inputs,
		Runtime:    definition.RuntimeRequirements{Capabilities: append([]string(nil), snapshot.Requirements...)},
		Task:       snapshot.ConversationBytes(),
		TaskID:     snapshot.TaskID,
		TaskFiles:  taskFiles,
	}), nil
}

// cleanupID derives the one durable cleanup row identity of an execution.
func cleanupID(executionID id.ExecutionID) string {
	sum := sha256.Sum256([]byte(string(executionID) + "\x00cleanup"))
	return "cln-" + hex.EncodeToString(sum[:16])
}

// encodeStop renders the kernel stop barrier of one action. The zero value is
// stored as NULL.
func encodeStop(stop kernel.StopEvidence) ([]byte, error) {
	if stop.State == "" && stop.Detail == "" {
		return nil, nil
	}
	encoded, err := json.Marshal(stop)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "action stop evidence could not be encoded", err)
	}
	return encoded, nil
}

// decodeStop rebuilds the kernel stop barrier from the stored column.
func decodeStop(value any) (kernel.StopEvidence, error) {
	data, ok := value.([]byte)
	if !ok || len(data) == 0 {
		return kernel.StopEvidence{}, nil
	}
	var stop kernel.StopEvidence
	if err := json.Unmarshal(data, &stop); err != nil {
		return kernel.StopEvidence{}, fault.Wrap(fault.InternalFailure, "action stop evidence is malformed", err)
	}
	return stop, nil
}

// encodeJSON renders one nullable jsonb column value.
func encodeJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "jsonb value could not be encoded", err)
	}
	return encoded, nil
}

// decodeJSON decodes one nullable jsonb column into target.
func decodeJSON(value any, target any) error {
	data, ok := value.([]byte)
	if !ok || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fault.Wrap(fault.InternalFailure, "jsonb column is malformed", err)
	}
	return nil
}

// encodeObservation renders one kernel observation. The Kernel value owns its
// exact JSON shape and refuses an inconsistent value.
func encodeObservation(observation kernel.Observation) ([]byte, error) {
	encoded, err := json.Marshal(observation)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "action observation could not be encoded", err)
	}
	return encoded, nil
}

// decodeObservation rebuilds one kernel observation from its stored column.
func decodeObservation(value any) (kernel.Observation, bool, error) {
	data, ok := value.([]byte)
	if !ok || len(data) == 0 {
		return kernel.Observation{}, false, nil
	}
	var observation kernel.Observation
	if err := json.Unmarshal(data, &observation); err != nil {
		return kernel.Observation{}, false, fault.Wrap(fault.InternalFailure, "action observation is malformed", err)
	}
	return observation, true, nil
}

// encodeFailure renders one typed terminal failure.
func encodeFailure(failure *kernel.Failure) ([]byte, error) {
	if failure == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(failure)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "execution failure could not be encoded", err)
	}
	return encoded, nil
}

// decodeFailure rebuilds the typed terminal failure from its stored column.
func decodeFailure(value any) (*kernel.Failure, error) {
	data, ok := value.([]byte)
	if !ok || len(data) == 0 {
		return nil, nil
	}
	var failure kernel.Failure
	if err := json.Unmarshal(data, &failure); err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "execution failure is malformed", err)
	}
	return &failure, nil
}

// ---------------------------------------------------------------------------
// Row value helpers. store.Row promises text as string, integers as int64,
// bytea/jsonb as []byte, booleans as bool, timestamptz as time.Time and NULL
// as nil.
// ---------------------------------------------------------------------------

func rowString(values map[string]any, name string) (string, error) {
	value, present := values[name]
	if !present || value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", fault.New(fault.InternalFailure, fmt.Sprintf("column %q is not text", name))
	}
	return text, nil
}

func rowUint64(values map[string]any, name string) (uint64, error) {
	value, present := values[name]
	if !present || value == nil {
		return 0, nil
	}
	number, ok := value.(int64)
	if !ok || number < 0 {
		return 0, fault.New(fault.InternalFailure, fmt.Sprintf("column %q is not an unsigned integer", name))
	}
	return uint64(number), nil
}

func rowInt(values map[string]any, name string) (int, error) {
	value, present := values[name]
	if !present || value == nil {
		return 0, nil
	}
	number, ok := value.(int64)
	if !ok {
		return 0, fault.New(fault.InternalFailure, fmt.Sprintf("column %q is not an integer", name))
	}
	return int(number), nil
}

func rowBool(values map[string]any, name string) (bool, error) {
	value, present := values[name]
	if !present || value == nil {
		return false, nil
	}
	flag, ok := value.(bool)
	if !ok {
		return false, fault.New(fault.InternalFailure, fmt.Sprintf("column %q is not a boolean", name))
	}
	return flag, nil
}

func rowTime(values map[string]any, name string) (time.Time, error) {
	value, present := values[name]
	if !present || value == nil {
		return time.Time{}, nil
	}
	moment, ok := value.(time.Time)
	if !ok {
		return time.Time{}, fault.New(fault.InternalFailure, fmt.Sprintf("column %q is not a timestamp", name))
	}
	return moment, nil
}

func rowBytes(values map[string]any, name string) ([]byte, error) {
	value, present := values[name]
	if !present || value == nil {
		return nil, nil
	}
	data, ok := value.([]byte)
	if !ok {
		return nil, fault.New(fault.InternalFailure, fmt.Sprintf("column %q is not bytes", name))
	}
	return data, nil
}

// nullableText returns a NULL when the text is empty, matching the optional
// scope columns of the declared schema.
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// scopeFromOwner validates and returns the unit owner scope.
func scopeFromOwner(owner tenancy.Scope) (tenancy.Scope, error) {
	if err := owner.Validate(); err != nil {
		return tenancy.Scope{}, err
	}
	return owner, nil
}

// executionVersionLock returns the declared-order lock for one execution row.
func executionVersionLock(executionID id.ExecutionID, version uint64) store.Lock {
	return store.Lock{Table: tableExecutions, IDs: []string{string(executionID)}, ExpectVersion: version}
}
