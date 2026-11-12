package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// Native-agent runtime bounds. They bound one planning loop, one model call
// budget, one tool argument/result and one knowledge body.
const (
	// CheckpointVersion is the durable checkpoint record version.
	CheckpointVersion = 1
	// CheckpointRefPrefix is the fixed checkpoint reference prefix.
	CheckpointRefPrefix = "kodama-checkpoint/"
	// CheckpointRefVersion is the supported checkpoint reference version.
	CheckpointRefVersion = "v1"
	// MaxCheckpointRefBytes bounds one checkpoint reference.
	MaxCheckpointRefBytes = 512
	// MaxToolArgumentBytes bounds one tool argument object.
	MaxToolArgumentBytes = 64 << 10
	// MaxToolResultBytes bounds one tool result entering the conversation.
	MaxToolResultBytes = 64 << 10
	// MaxKnowledgeBytes bounds one loaded knowledge body.
	MaxKnowledgeBytes = 1 << 20
	// MaxQuestionBytes bounds one task.ask question.
	MaxQuestionBytes = 4096
	// MaxModelCalls bounds one run's model calls.
	MaxModelCalls = 32
	// MaxSteps bounds one run's tool effects.
	MaxSteps = 64
	// DefaultModelCalls is the model-call budget used when none is supplied.
	DefaultModelCalls = 16
	// DefaultSteps is the step budget used when none is supplied.
	DefaultSteps = 32
)

// Limits bounds one run. A zero field means the default bound.
type Limits struct {
	ModelCalls int `json:"model_calls,omitempty"`
	Messages   int `json:"messages,omitempty"`
	Tokens     int `json:"tokens,omitempty"`
	Steps      int `json:"steps,omitempty"`
}

// DefaultLimits returns the default run bounds.
func DefaultLimits() Limits {
	return Limits{
		ModelCalls: DefaultModelCalls,
		Messages:   MaxMessages,
		Tokens:     MaxModelTokens,
		Steps:      DefaultSteps,
	}
}

// effective replaces zero fields with their defaults.
func (limits Limits) effective() Limits {
	if limits.ModelCalls == 0 {
		limits.ModelCalls = DefaultModelCalls
	}
	if limits.Messages == 0 {
		limits.Messages = MaxMessages
	}
	if limits.Tokens == 0 {
		limits.Tokens = MaxModelTokens
	}
	if limits.Steps == 0 {
		limits.Steps = DefaultSteps
	}
	return limits
}

// Validate checks each bound is inside its declared range. Zero means default.
func (limits Limits) Validate() error {
	for _, bound := range []struct {
		name  string
		value int
		max   int
	}{
		{"model calls", limits.ModelCalls, MaxModelCalls},
		{"messages", limits.Messages, MaxMessages},
		{"tokens", limits.Tokens, MaxModelTokens},
		{"steps", limits.Steps, MaxSteps},
	} {
		if bound.value < 0 {
			return fault.New(fault.InvalidInput, fmt.Sprintf("%s bound %d must not be negative", bound.name, bound.value))
		}
		if bound.value > bound.max {
			return fault.New(fault.InvalidInput, fmt.Sprintf("%s bound %d exceeds %d", bound.name, bound.value, bound.max))
		}
	}
	return nil
}

// OutcomeStatus is the bounded result vocabulary of one run.
type OutcomeStatus string

const (
	// OutcomeSucceeded reports a final, validated answer.
	OutcomeSucceeded OutcomeStatus = "succeeded"
	// OutcomeWaiting reports a durable task.ask handoff.
	OutcomeWaiting OutcomeStatus = "waiting"
	// OutcomeFailed reports a bounded, non-retryable failure.
	OutcomeFailed OutcomeStatus = "failed"
	// OutcomeCancelled reports a cancelled or expired run.
	OutcomeCancelled OutcomeStatus = "cancelled"
	// OutcomeUncertain reports an effect whose outcome is unknown.
	OutcomeUncertain OutcomeStatus = "uncertain"
	// OutcomeRefused reports a refused tool or authority request.
	OutcomeRefused OutcomeStatus = "refused"
)

// outcomeStatuses is the declaration order.
var outcomeStatuses = []OutcomeStatus{
	OutcomeSucceeded,
	OutcomeWaiting,
	OutcomeFailed,
	OutcomeCancelled,
	OutcomeUncertain,
	OutcomeRefused,
}

// Valid reports whether status is one of the declared statuses.
func (status OutcomeStatus) Valid() bool {
	return slices.Contains(outcomeStatuses, status)
}

// RunInput is one native-agent run request.
type RunInput struct {
	TaskID      string    `json:"task_id"`
	ExecutionID string    `json:"execution_id"`
	TaskVersion uint64    `json:"task_version,omitempty"`
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Knowledge   []string  `json:"knowledge,omitempty"`
	Tools       []string  `json:"tools,omitempty"`
	Limits      Limits    `json:"limits,omitempty"`
	RestoreRef  string    `json:"restore_ref,omitempty"`
}

// Validate checks identity, model, conversation, allowlist (which never admits
// a reserved authority operation), knowledge paths, the restore reference and
// the run bounds.
func (input RunInput) Validate() error {
	if !validAgentID(input.TaskID) {
		return fault.New(fault.InvalidInput, "run input has no valid task id")
	}
	if !validAgentID(input.ExecutionID) {
		return fault.New(fault.InvalidInput, "run input has no valid execution id")
	}
	if strings.TrimSpace(input.Model) == "" {
		return fault.New(fault.InvalidInput, "run input has no model")
	}
	if err := input.Limits.Validate(); err != nil {
		return err
	}
	limits := input.Limits.effective()
	if len(input.Messages) == 0 {
		return fault.New(fault.InvalidInput, "run input has no messages")
	}
	if len(input.Messages) > limits.Messages {
		return fault.New(fault.InvalidInput, fmt.Sprintf("run input has %d messages; at most %d are allowed", len(input.Messages), limits.Messages))
	}
	for _, message := range input.Messages {
		if !validMessageRole(message.Role) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("run input has unknown message role %q", message.Role))
		}
	}
	if len(input.Tools) > MaxToolDeclarations {
		return fault.New(fault.InvalidInput, fmt.Sprintf("run input allowlists %d tools; at most %d are allowed", len(input.Tools), MaxToolDeclarations))
	}
	allowed := make(map[string]struct{}, len(input.Tools))
	for _, tool := range input.Tools {
		if isReservedTool(tool) {
			return fault.New(fault.Forbidden, fmt.Sprintf("run input allowlists reserved tool %q", tool))
		}
		if !validToolName(tool) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("run input has invalid tool name %q", tool))
		}
		if _, duplicate := allowed[tool]; duplicate {
			return fault.New(fault.InvalidInput, fmt.Sprintf("run input allowlists tool %q more than once", tool))
		}
		allowed[tool] = struct{}{}
	}
	for _, path := range input.Knowledge {
		if !validKnowledgePath(path) {
			return fault.New(fault.InvalidInput, fmt.Sprintf("run input has invalid knowledge path %q", path))
		}
	}
	if input.RestoreRef != "" && !ValidCheckpointRef(input.RestoreRef) {
		return fault.New(fault.InvalidInput, fmt.Sprintf("run input has invalid checkpoint reference %q", input.RestoreRef))
	}
	return nil
}

// Outcome is the bounded result of one run.
type Outcome struct {
	Status        OutcomeStatus `json:"status"`
	Summary       string        `json:"summary,omitempty"`
	Usage         Usage         `json:"usage"`
	CheckpointRef string        `json:"checkpoint_ref,omitempty"`
	Completed     []string      `json:"completed,omitempty"`
	Uncertain     []string      `json:"uncertain,omitempty"`
	FinishReason  string        `json:"finish_reason,omitempty"`
	Class         fault.Class   `json:"class,omitempty"`
}

// RuntimeConfig binds the consumer-owned runtime boundaries.
type RuntimeConfig struct {
	Model       ModelPort
	Tools       ToolPort
	Checkpoints CheckpointPort
	Knowledge   KnowledgePort
	Public      PublicClient
	Clock       Clock
	RetainRaw   bool
}

// CheckpointPort persists and restores durable conversation context.
type CheckpointPort interface {
	Save(ctx context.Context, checkpoint Checkpoint) (string, error)
	Restore(ctx context.Context, ref string) (Checkpoint, error)
}

// KnowledgePort loads one bounded knowledge body.
type KnowledgePort interface {
	Load(ctx context.Context, path string) ([]byte, error)
}

// PublicClient invokes one authorized public operation.
type PublicClient interface {
	Invoke(ctx context.Context, invocation api.Invocation) (api.Result, error)
}

// validAgentID reports whether value is a bounded ASCII identity.
func validAgentID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	if value == "." || value == ".." {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// validKnowledgePath reports whether path is a bounded relative knowledge path.
func validKnowledgePath(path string) bool {
	if path == "" || len(path) > 1024 {
		return false
	}
	if strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return false
	}
	if strings.HasSuffix(path, "/") {
		return false
	}
	for segment := range strings.SplitSeq(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
