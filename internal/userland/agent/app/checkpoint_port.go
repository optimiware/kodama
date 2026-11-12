package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent"
	"github.com/optimiware/kodama/internal/userland/session"
)

// checkpointStatePrefix marks the encoded effect bookkeeping. The session
// record has no dedicated effect field, and raw outcome content is withheld by
// retention, so the bookkeeping is stored in a reserved durable usage entry.
// This mapping is wire-compatible with the Shell checkpoint bridge.
const checkpointStatePrefix = "checkpoint:"

// checkpointUsagePrincipal is the reserved principal marker.
const checkpointUsagePrincipal = "kodama-checkpoint"

// checkpointUsageModel is the reserved model marker of the real usage entry.
const checkpointUsageModel = "agent"

// checkpointState is the durable effect bookkeeping of one agent checkpoint.
type checkpointState struct {
	Completed []string `json:"completed,omitempty"`
	InFlight  []string `json:"in_flight,omitempty"`
	Uncertain []string `json:"uncertain,omitempty"`
}

// CheckpointPort implements agent.CheckpointPort over one session.Store.
type CheckpointPort struct {
	store     session.Store
	clock     session.Clock
	retention session.RetentionPolicy

	mu        sync.Mutex
	revisions map[session.SessionID]uint64
}

var _ agent.CheckpointPort = (*CheckpointPort)(nil)

// NewCheckpointPort builds the port over one durable session store, clock and
// explicit trusted raw-content retention selection. The zero selection keeps raw
// conversation content off; the composition opts in through the seven-day
// raw-content window, which the session store applies with structural argument
// redaction and expiry.
func NewCheckpointPort(storage session.Storage, clock session.Clock, retention session.RetentionPolicy) (*CheckpointPort, error) {
	if storage == nil {
		return nil, fault.New(fault.InvalidInput, "checkpoint port requires storage")
	}
	if clock == nil {
		return nil, fault.New(fault.InvalidInput, "checkpoint port requires a clock")
	}
	if err := retention.Validate(); err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "checkpoint retention is invalid", err)
	}
	store, err := session.NewStore(session.StoreConfig{Storage: storage, Clock: clock})
	if err != nil {
		return nil, err
	}
	return &CheckpointPort{
		store:     store,
		clock:     clock,
		retention: retention,
		revisions: make(map[session.SessionID]uint64),
	}, nil
}

// Save persists one agent checkpoint and returns its durable reference.
func (port *CheckpointPort) Save(ctx context.Context, checkpoint agent.Checkpoint) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fault.Wrap(fault.DeadlineExpired, "checkpoint save was canceled", err)
	}
	if port == nil || port.store == nil {
		return "", fault.New(fault.InternalFailure, "checkpoint port is not configured")
	}
	if err := checkpoint.Validate(); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "checkpoint is invalid", err)
	}
	state, err := encodeCheckpointState(checkpoint)
	if err != nil {
		return "", err
	}
	sessionID := checkpointSessionID(checkpoint.TaskID, checkpoint.ExecutionID)
	revision := port.revision(sessionID)
	record := session.Record{
		Version:     session.RefVersion,
		TaskID:      checkpoint.TaskID,
		ExecutionID: checkpoint.ExecutionID,
		SessionID:   sessionID,
		Revision:    revision,
		Messages:    sessionMessages(checkpoint.Messages),
		Retention:   port.retention,
		Usage:       sessionUsage(checkpoint.Usage, state),
		Outcome:     session.Outcome{Status: "checkpoint"},
		CreatedAt:   checkpoint.CreatedAt,
	}
	ref, err := port.store.Save(ctx, record)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fault.Wrap(fault.DeadlineExpired, "checkpoint save was canceled", ctxErr)
		}
		if _, classified := fault.ClassOf(err); classified {
			return "", err
		}
		return "", fault.Wrap(fault.DependencyUnavailable, "checkpoint save failed", err)
	}
	if _, err := session.ParseRef(string(ref)); err != nil {
		return "", fault.Wrap(fault.InternalFailure, "checkpoint store returned an invalid reference", err)
	}
	port.advance(sessionID, revision+1)
	return string(ref), nil
}

// Restore loads one durable checkpoint reference.
func (port *CheckpointPort) Restore(ctx context.Context, ref string) (agent.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return agent.Checkpoint{}, fault.Wrap(fault.DeadlineExpired, "checkpoint restore was canceled", err)
	}
	if port == nil || port.store == nil {
		return agent.Checkpoint{}, fault.New(fault.InternalFailure, "checkpoint port is not configured")
	}
	if !agent.ValidCheckpointRef(ref) {
		return agent.Checkpoint{}, fault.New(fault.InvalidInput, "checkpoint reference is invalid")
	}
	record, err := port.store.Restore(ctx, session.Ref(ref))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return agent.Checkpoint{}, fault.Wrap(fault.DeadlineExpired, "checkpoint restore was canceled", ctxErr)
		}
		return agent.Checkpoint{}, restoreFault(err)
	}
	// The store withholds raw content that has outlived the retention window.
	// Restoring that record would silently continue from an incomplete
	// conversation, so an expired transcript fails the restoring action even
	// though the effect bookkeeping is still readable (S06 §4). A raw-off
	// checkpoint is not expiry: its content was withheld at write time by
	// explicit policy.
	if record.Retention.RawContent && !record.Retention.RetainRaw(port.clock.Now(), record.CreatedAt, false) {
		return agent.Checkpoint{}, fault.New(fault.NotFound, "checkpoint transcript has expired")
	}
	if err := record.Validate(); err != nil {
		return agent.Checkpoint{}, fault.Wrap(fault.InternalFailure, "restored checkpoint record is invalid", err)
	}
	state, err := decodeCheckpointState(record.Usage)
	if err != nil {
		return agent.Checkpoint{}, err
	}
	port.advance(record.SessionID, record.Revision)
	checkpoint := agent.Checkpoint{
		Version:     agent.CheckpointVersion,
		TaskID:      record.TaskID,
		ExecutionID: record.ExecutionID,
		Messages:    agentMessages(record.Messages),
		Completed:   state.Completed,
		InFlight:    state.InFlight,
		Uncertain:   state.Uncertain,
		Usage:       agentUsage(record.Usage),
		CreatedAt:   record.CreatedAt,
	}
	if err := checkpoint.Validate(); err != nil {
		return agent.Checkpoint{}, fault.Wrap(fault.InternalFailure, "restored checkpoint is invalid", err)
	}
	return checkpoint, nil
}

// checkpointSessionID derives one stable, bounded session identity.
func checkpointSessionID(taskID, executionID string) session.SessionID {
	sum := sha256.Sum256([]byte(taskID + "\x00" + executionID))
	return session.SessionID("sess-" + hex.EncodeToString(sum[:]))
}

func (port *CheckpointPort) revision(id session.SessionID) uint64 {
	port.mu.Lock()
	defer port.mu.Unlock()
	return port.revisions[id]
}

func (port *CheckpointPort) advance(id session.SessionID, revision uint64) {
	port.mu.Lock()
	defer port.mu.Unlock()
	if port.revisions[id] < revision {
		port.revisions[id] = revision
	}
}

func encodeCheckpointState(checkpoint agent.Checkpoint) (string, error) {
	encoded, err := json.Marshal(checkpointState{
		Completed: checkpoint.Completed,
		InFlight:  checkpoint.InFlight,
		Uncertain: checkpoint.Uncertain,
	})
	if err != nil {
		return "", fault.Wrap(fault.InternalFailure, "checkpoint effect state cannot be encoded", err)
	}
	return checkpointStatePrefix + base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeCheckpointState(usage []session.Usage) (checkpointState, error) {
	for _, entry := range usage {
		if len(entry.Model) < len(checkpointStatePrefix) || entry.Model[:len(checkpointStatePrefix)] != checkpointStatePrefix {
			continue
		}
		decoded, err := base64.RawURLEncoding.DecodeString(entry.Model[len(checkpointStatePrefix):])
		if err != nil {
			return checkpointState{}, fault.Wrap(fault.InternalFailure, "restored checkpoint effect state is corrupt", err)
		}
		var state checkpointState
		if err := json.Unmarshal(decoded, &state); err != nil {
			return checkpointState{}, fault.Wrap(fault.InternalFailure, "restored checkpoint effect state is corrupt", err)
		}
		return state, nil
	}
	return checkpointState{}, fault.New(fault.InternalFailure, "restored checkpoint has no effect state")
}

func sessionMessages(messages []agent.Message) []session.Message {
	converted := make([]session.Message, len(messages))
	for index, message := range messages {
		converted[index] = session.Message{
			Role:       message.Role,
			Content:    message.Content,
			ToolCalls:  sessionToolCalls(message.ToolCalls),
			ToolCallID: session.ToolCallID(message.ToolCallID),
		}
	}
	return converted
}

func agentMessages(messages []session.Message) []agent.Message {
	converted := make([]agent.Message, len(messages))
	for index, message := range messages {
		converted[index] = agent.Message{
			Role:       message.Role,
			Content:    message.Content,
			ToolCalls:  agentToolCalls(message.ToolCalls),
			ToolCallID: agent.ToolCallID(message.ToolCallID),
		}
	}
	return converted
}

// sessionToolCalls copies an assistant call list into the recorded shape. The
// slice is always fresh so a checkpoint never aliases the caller's memory.
func sessionToolCalls(calls []agent.ToolCall) []session.ToolCall {
	if calls == nil {
		return nil
	}
	converted := make([]session.ToolCall, len(calls))
	for index, call := range calls {
		converted[index] = session.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments}
	}
	return converted
}

// agentToolCalls copies a recorded call list back into the agent shape. The
// slice is always fresh so a restored checkpoint never aliases the store.
func agentToolCalls(calls []session.ToolCall) []agent.ToolCall {
	if calls == nil {
		return nil
	}
	converted := make([]agent.ToolCall, len(calls))
	for index, call := range calls {
		converted[index] = agent.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments}
	}
	return converted
}

func sessionUsage(usage agent.Usage, state string) []session.Usage {
	entries := make([]session.Usage, 0, 2)
	if usage != (agent.Usage{}) {
		entries = append(entries, session.Usage{
			PrincipalID: checkpointUsagePrincipal,
			Model:       checkpointUsageModel,
			Prompt:      usage.PromptTokens,
			Completion:  usage.CompletionTokens,
		})
	}
	entries = append(entries, session.Usage{
		PrincipalID: checkpointUsagePrincipal,
		Model:       state,
	})
	return entries
}

func agentUsage(usage []session.Usage) agent.Usage {
	var prompt, completion int
	for _, entry := range usage {
		if len(entry.Model) >= len(checkpointStatePrefix) && entry.Model[:len(checkpointStatePrefix)] == checkpointStatePrefix {
			continue
		}
		prompt += entry.Prompt
		completion += entry.Completion
	}
	return agent.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}
}

func restoreFault(err error) error {
	var restoreErr session.RestoreError
	if ok := asRestoreError(err, &restoreErr); ok {
		switch restoreErr.Reason {
		case session.ReasonMissing, session.ReasonContextMissing:
			return fault.Wrap(fault.NotFound, "checkpoint was not found", err)
		default:
			return fault.Wrap(fault.InvalidInput, "checkpoint cannot be restored", err)
		}
	}
	if _, classified := fault.ClassOf(err); classified {
		return err
	}
	return fault.Wrap(fault.DependencyUnavailable, "checkpoint restore failed", err)
}

func asRestoreError(err error, target *session.RestoreError) bool {
	for err != nil {
		if value, ok := err.(session.RestoreError); ok {
			*target = value
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
