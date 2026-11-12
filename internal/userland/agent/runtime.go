package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// reservedToolNames are the authority-bearing names the runtime never forwards.
// They cover every public operation that replies to a task, resolves a request,
// escalates a role or approves on a human's behalf, plus the former declaration
// names. A run may not allowlist them and a model may not request them.
var reservedToolNames = []string{
	"task.reply",
	"execution.request.resolve",
	"request.resolve",
	"role.escalate",
}

// Runtime is one bounded native-agent planning/tool/review loop. It starts no
// process and holds no authority of its own: every effect goes through a
// consumer-owned port and the current public operation envelope.
type Runtime struct {
	config RuntimeConfig
}

// NewRuntime binds the required model, tool, checkpoint, public and clock
// boundaries. Knowledge is optional until a run requests it.
func NewRuntime(config RuntimeConfig) (*Runtime, error) {
	boundaries := []struct {
		name    string
		missing bool
	}{
		{"model", config.Model == nil},
		{"tool", config.Tools == nil},
		{"checkpoint", config.Checkpoints == nil},
		{"public", config.Public == nil},
		{"clock", config.Clock == nil},
	}
	for _, boundary := range boundaries {
		if boundary.missing {
			return nil, fault.New(fault.InvalidInput, fmt.Sprintf("runtime has no %s port", boundary.name))
		}
	}
	return &Runtime{config: config}, nil
}

// runState is one run's private conversation and effect bookkeeping. It is
// never shared between runs.
type runState struct {
	messages      []Message
	completed     []string
	uncertain     []string
	usage         Usage
	modelCalls    int
	steps         int
	correlation   int
	checkpointRef string
}

// Run executes one bounded plan/tool/review loop.
func (runtime *Runtime) Run(ctx context.Context, input RunInput) (Outcome, error) {
	if err := input.Validate(); err != nil {
		return Outcome{Status: OutcomeFailed, Class: faultClassOf(err, fault.InvalidInput)}, err
	}
	limits := input.Limits.effective()
	state := &runState{}

	if input.RestoreRef != "" {
		restored, err := runtime.config.Checkpoints.Restore(ctx, input.RestoreRef)
		if err != nil {
			class := faultClassOf(err, fault.InternalFailure)
			return Outcome{Status: OutcomeFailed, Class: class, CheckpointRef: input.RestoreRef},
				fault.Wrap(class, "checkpoint restore failed", err)
		}
		if err := restored.Validate(); err != nil {
			return Outcome{Status: OutcomeFailed, Class: fault.InvalidInput, CheckpointRef: input.RestoreRef},
				fault.New(fault.InvalidInput, "restored checkpoint is invalid")
		}
		if restored.TaskID != input.TaskID {
			return Outcome{Status: OutcomeFailed, Class: fault.InvalidInput, CheckpointRef: input.RestoreRef},
				fault.New(fault.InvalidInput, "restored checkpoint belongs to another task")
		}
		if len(restored.InFlight) > 0 || len(restored.Uncertain) > 0 {
			uncertain := append([]string(nil), restored.InFlight...)
			uncertain = append(uncertain, restored.Uncertain...)
			return Outcome{
				Status:        OutcomeUncertain,
				Summary:       "restored context has an unreconciled effect",
				CheckpointRef: input.RestoreRef,
				Completed:     append([]string(nil), restored.Completed...),
				Uncertain:     uncertain,
				Usage:         restored.Usage,
				Class:         fault.UncertainExternalEffect,
			}, nil
		}
		// A restored conversation must be a closed, correlated prefix before
		// any model or effect call. Missing required context fails explicitly
		// rather than being replaced with manufactured history.
		if err := ValidateConversation(restored.Messages); err != nil {
			return Outcome{Status: OutcomeFailed, Class: fault.InvalidInput, CheckpointRef: input.RestoreRef},
				fault.Wrap(fault.InvalidInput, "restored checkpoint conversation is not reusable", err)
		}
		state.messages = copyMessages(restored.Messages)
		state.completed = append(state.completed, restored.Completed...)
		state.usage = restored.Usage
		state.checkpointRef = input.RestoreRef
	}

	if ctx.Err() != nil {
		return cancelledOutcome(state), nil
	}

	declarations, err := toolDeclarations(input.Tools)
	if err != nil {
		return Outcome{Status: OutcomeFailed, Class: fault.InvalidInput, CheckpointRef: state.checkpointRef},
			fault.Wrap(fault.InvalidInput, "run tools are invalid", err)
	}

	if len(input.Knowledge) > 0 {
		if runtime.config.Knowledge == nil {
			return Outcome{Status: OutcomeFailed, Class: fault.InvalidInput, CheckpointRef: state.checkpointRef},
				fault.New(fault.InvalidInput, "knowledge was requested without a knowledge port")
		}
		for _, path := range input.Knowledge {
			body, err := runtime.config.Knowledge.Load(ctx, path)
			if err != nil {
				if ctx.Err() != nil {
					return cancelledOutcome(state), nil
				}
				return Outcome{
					Status:        OutcomeFailed,
					Summary:       "knowledge could not be loaded",
					Class:         faultClassOf(err, fault.InternalFailure),
					CheckpointRef: state.checkpointRef,
					Completed:     copyStrings(state.completed),
					Usage:         state.usage,
				}, nil
			}
			if len(body) > MaxKnowledgeBytes {
				return Outcome{
					Status:        OutcomeFailed,
					Summary:       "knowledge exceeds the size bound",
					CheckpointRef: state.checkpointRef,
					Completed:     copyStrings(state.completed),
					Usage:         state.usage,
				}, nil
			}
			state.messages = append(state.messages, Message{Role: "system", Content: string(body)})
		}
	}
	state.messages = append(state.messages, copyMessages(input.Messages)...)

	for {
		if ctx.Err() != nil {
			return cancelledOutcome(state), nil
		}
		if state.modelCalls >= limits.ModelCalls {
			return Outcome{
				Status:        OutcomeFailed,
				Summary:       "model call limit reached",
				CheckpointRef: state.checkpointRef,
				Completed:     copyStrings(state.completed),
				Uncertain:     copyStrings(state.uncertain),
				Usage:         state.usage,
			}, nil
		}
		if len(state.messages) > limits.Messages {
			return Outcome{
				Status:        OutcomeFailed,
				Summary:       "message limit reached",
				CheckpointRef: state.checkpointRef,
				Completed:     copyStrings(state.completed),
				Uncertain:     copyStrings(state.uncertain),
				Usage:         state.usage,
			}, nil
		}

		state.modelCalls++
		request := ModelRequest{
			Model:    input.Model,
			Messages: copyMessages(state.messages),
			Tools:    copyToolDeclarations(declarations),
		}
		if err := request.Validate(); err != nil {
			return Outcome{
				Status:        OutcomeFailed,
				Summary:       "model request was malformed",
				Class:         fault.InvalidInput,
				CheckpointRef: state.checkpointRef,
				Completed:     copyStrings(state.completed),
				Uncertain:     copyStrings(state.uncertain),
				Usage:         state.usage,
			}, fault.Wrap(fault.InvalidInput, "model request was malformed", err)
		}
		response, err := runtime.config.Model.Invoke(ctx, request)
		if err != nil {
			if ctx.Err() != nil {
				return cancelledOutcome(state), nil
			}
			class := faultClassOf(err, fault.InternalFailure)
			return Outcome{
					Status:        OutcomeFailed,
					Summary:       "model invocation failed",
					Class:         class,
					CheckpointRef: state.checkpointRef,
					Completed:     copyStrings(state.completed),
					Uncertain:     copyStrings(state.uncertain),
					Usage:         state.usage,
				},
				fault.Wrap(class, "model invocation failed", err)
		}
		if err := response.Validate(); err != nil {
			return Outcome{
				Status:        OutcomeFailed,
				Summary:       "model response was malformed",
				CheckpointRef: state.checkpointRef,
				Completed:     copyStrings(state.completed),
				Uncertain:     copyStrings(state.uncertain),
				Usage:         state.usage,
			}, nil
		}
		state.usage.PromptTokens += response.Usage.PromptTokens
		state.usage.CompletionTokens += response.Usage.CompletionTokens
		state.usage.TotalTokens += response.Usage.TotalTokens
		if state.usage.TotalTokens > limits.Tokens {
			return Outcome{
				Status:        OutcomeFailed,
				Summary:       "token limit reached",
				CheckpointRef: state.checkpointRef,
				Completed:     copyStrings(state.completed),
				Uncertain:     copyStrings(state.uncertain),
				Usage:         state.usage,
			}, nil
		}

		switch response.FinishReason {
		case "length", "content_filter":
			return Outcome{
				Status:        OutcomeFailed,
				Summary:       "model stopped before completion",
				FinishReason:  response.FinishReason,
				CheckpointRef: state.checkpointRef,
				Completed:     copyStrings(state.completed),
				Uncertain:     copyStrings(state.uncertain),
				Usage:         state.usage,
			}, nil
		case "stop":
			if len(response.ToolCalls) > 0 {
				return Outcome{
					Status:        OutcomeFailed,
					Summary:       "model response was malformed",
					CheckpointRef: state.checkpointRef,
					Completed:     copyStrings(state.completed),
					Uncertain:     copyStrings(state.uncertain),
					Usage:         state.usage,
				}, nil
			}
			if strings.TrimSpace(response.Content) == "" {
				return Outcome{
					Status:        OutcomeFailed,
					Summary:       "model returned no final content",
					CheckpointRef: state.checkpointRef,
					Completed:     copyStrings(state.completed),
					Uncertain:     copyStrings(state.uncertain),
					Usage:         state.usage,
				}, nil
			}
			state.messages = append(state.messages, Message{Role: "assistant", Content: response.Content})
			ref, saveErr := runtime.saveCheckpoint(ctx, input, state, state.messages, state.completed, nil, state.uncertain)
			if saveErr != nil {
				class := faultClassOf(saveErr, fault.InternalFailure)
				return Outcome{
						Status:        OutcomeFailed,
						Summary:       "final checkpoint could not be saved",
						Class:         class,
						CheckpointRef: state.checkpointRef,
						Completed:     copyStrings(state.completed),
						Uncertain:     copyStrings(state.uncertain),
						Usage:         state.usage,
					},
					fault.Wrap(class, "final checkpoint could not be saved", saveErr)
			}
			state.checkpointRef = ref
			return Outcome{
				Status:        OutcomeSucceeded,
				Summary:       response.Content,
				Usage:         state.usage,
				CheckpointRef: ref,
				Completed:     copyStrings(state.completed),
			}, nil
		case "tool_calls":
			if len(response.ToolCalls) == 0 {
				return Outcome{
					Status:        OutcomeFailed,
					Summary:       "model response was malformed",
					CheckpointRef: state.checkpointRef,
					Completed:     copyStrings(state.completed),
					Uncertain:     copyStrings(state.uncertain),
					Usage:         state.usage,
				}, nil
			}
			recorded, err := state.assignCallIdentities(response.ToolCalls)
			if err != nil {
				return Outcome{
					Status:        OutcomeFailed,
					Summary:       "model response was malformed",
					CheckpointRef: state.checkpointRef,
					Completed:     copyStrings(state.completed),
					Uncertain:     copyStrings(state.uncertain),
					Usage:         state.usage,
				}, nil
			}
			state.messages = append(state.messages, Message{
				Role:      "assistant",
				Content:   response.Content,
				ToolCalls: recorded,
			})
			for index, call := range response.ToolCalls {
				if ctx.Err() != nil {
					return cancelledOutcome(state), nil
				}
				if state.steps >= limits.Steps {
					return Outcome{
						Status:        OutcomeFailed,
						Summary:       "step limit reached",
						CheckpointRef: state.checkpointRef,
						Completed:     copyStrings(state.completed),
						Uncertain:     copyStrings(state.uncertain),
						Usage:         state.usage,
					}, nil
				}
				state.steps++
				outcome, err, done := runtime.runToolCall(ctx, input, state, call, ToolCallID(recorded[index].ID))
				if done {
					return outcome, err
				}
			}
		}
	}
}

// runToolCall validates one model-requested tool call, persists intent before
// the effect and records a durable completion. Every tool message answers the
// assistant call's own correlation id. It returns done=true when the run must
// stop.
func (runtime *Runtime) runToolCall(ctx context.Context, input RunInput, state *runState, call ToolRequest, callID ToolCallID) (Outcome, error, bool) {
	if isReservedTool(call.Name) {
		summary := "reserved tool name is not available to the agent"
		return refusalOutcome(state, summary), fault.New(fault.Forbidden, summary), true
	}
	if !containsString(input.Tools, call.Name) {
		summary := "tool is not in the run allowlist"
		return refusalOutcome(state, summary), fault.New(fault.Forbidden, summary), true
	}
	if call.Name == "task.ask" {
		return runtime.handoff(ctx, input, state, call)
	}
	if len(call.Arguments) > MaxToolArgumentBytes {
		summary := "tool arguments exceed the bound"
		return invalidOutcome(state, summary), fault.New(fault.InvalidInput, summary), true
	}
	if err := call.Validate(); err != nil {
		summary := "tool request is malformed"
		return invalidOutcome(state, summary), fault.New(fault.InvalidInput, summary), true
	}

	effect := effectIdentity(call)
	if containsString(state.completed, effect) {
		state.messages = append(state.messages, Message{
			Role:       "tool",
			ToolCallID: callID,
			Content:    "effect " + effect + " was already completed",
		})
		return Outcome{}, nil, false
	}

	ref, err := runtime.saveCheckpoint(ctx, input, state, state.messages, state.completed, []string{effect}, state.uncertain)
	if err != nil {
		class := faultClassOf(err, fault.InternalFailure)
		return Outcome{
				Status:        OutcomeFailed,
				Summary:       "checkpoint intent could not be saved",
				Class:         class,
				CheckpointRef: state.checkpointRef,
				Completed:     copyStrings(state.completed),
				Uncertain:     copyStrings(state.uncertain),
				Usage:         state.usage,
			},
			fault.Wrap(class, "checkpoint intent could not be saved", err), true
	}
	state.checkpointRef = ref

	result, err := runtime.config.Tools.Invoke(ctx, call)
	if err != nil {
		class := faultClassOf(err, fault.UncertainExternalEffect)
		if ctx.Err() == nil && (class == fault.Forbidden || class == fault.Unauthenticated) {
			summary := "tool invocation was refused"
			return Outcome{
					Status:        OutcomeFailed,
					Summary:       summary,
					Class:         class,
					CheckpointRef: state.checkpointRef,
					Completed:     copyStrings(state.completed),
					Uncertain:     copyStrings(state.uncertain),
					Usage:         state.usage,
				},
				fault.Wrap(class, summary, err), true
		}
		return runtime.uncertainOutcome(ctx, input, state, effect), nil, true
	}

	content := result.Content
	if len(content) > MaxToolResultBytes {
		content = content[:MaxToolResultBytes]
	}
	state.messages = append(state.messages, Message{Role: "tool", ToolCallID: callID, Content: content})
	completed := append(append([]string(nil), state.completed...), effect)
	ref, err = runtime.saveCheckpoint(ctx, input, state, state.messages, completed, nil, state.uncertain)
	if err != nil {
		return Outcome{
			Status:        OutcomeUncertain,
			Summary:       "completion record could not be saved",
			CheckpointRef: state.checkpointRef,
			Completed:     copyStrings(state.completed),
			Uncertain:     []string{effect},
			Usage:         state.usage,
			Class:         fault.UncertainExternalEffect,
		}, nil, true
	}
	state.completed = completed
	state.checkpointRef = ref
	return Outcome{}, nil, false
}

// handoff persists durable context, publishes one versioned/keyed task.ask and
// exits. It never forwards task.ask to the tool port. The durable conversation
// is the closed prefix of the current turn: calls already answered in the same
// model turn keep their assistant call and tool messages, while task.ask and
// every later unrun call are dropped. The served task snapshot owns the
// outstanding question/reply, so restoration never replays task.ask or forwards
// an orphan assistant/tool turn.
func (runtime *Runtime) handoff(ctx context.Context, input RunInput, state *runState, call ToolRequest) (Outcome, error, bool) {
	var arguments struct {
		Question   string `json:"question"`
		Assignment string `json:"assignment"`
	}
	if len(call.Arguments) == 0 || len(call.Arguments) > MaxToolArgumentBytes {
		summary := "task.ask arguments are missing or oversized"
		return invalidOutcome(state, summary), fault.New(fault.InvalidInput, summary), true
	}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
		summary := "task.ask arguments are malformed"
		return invalidOutcome(state, summary), fault.New(fault.InvalidInput, summary), true
	}
	if strings.TrimSpace(arguments.Question) == "" || len(arguments.Question) > MaxQuestionBytes {
		summary := "task.ask question is missing or oversized"
		return invalidOutcome(state, summary), fault.New(fault.InvalidInput, summary), true
	}
	if strings.TrimSpace(arguments.Assignment) == "" || len(arguments.Assignment) > 256 {
		summary := "task.ask assignment is missing or oversized"
		return invalidOutcome(state, summary), fault.New(fault.InvalidInput, summary), true
	}
	request := api.TaskAskRequest{
		TaskID:     input.TaskID,
		Question:   arguments.Question,
		Assignment: arguments.Assignment,
	}
	if err := request.Validate(); err != nil {
		summary := "task.ask request is invalid"
		return invalidOutcome(state, summary), fault.New(fault.InvalidInput, summary), true
	}
	if input.TaskVersion == 0 {
		summary := "task.ask requires the current task version"
		return invalidOutcome(state, summary), fault.New(fault.InvalidInput, summary), true
	}

	ref, err := runtime.saveCheckpoint(ctx, input, state, state.messages, state.completed, nil, state.uncertain)
	if err != nil {
		class := faultClassOf(err, fault.InternalFailure)
		return Outcome{
				Status:        OutcomeFailed,
				Summary:       "handoff context could not be saved",
				Class:         class,
				CheckpointRef: state.checkpointRef,
				Completed:     copyStrings(state.completed),
				Uncertain:     copyStrings(state.uncertain),
				Usage:         state.usage,
			},
			fault.Wrap(class, "handoff context could not be saved", err), true
	}
	state.checkpointRef = ref

	request.References = []string{ref}
	payload, err := json.Marshal(request)
	if err != nil {
		summary := "task.ask input could not be encoded"
		return invalidOutcome(state, summary), fault.New(fault.InternalFailure, summary), true
	}
	version := input.TaskVersion
	invocation := api.Invocation{
		Operation: "task.ask",
		Request: api.Request{
			Input:           payload,
			ExpectedVersion: &version,
			IdempotencyKey:  askIdempotencyKey(input, effectIdentity(call)),
		},
	}
	if err := invocation.Validate(); err != nil {
		summary := "task.ask invocation is invalid"
		return invalidOutcome(state, summary), fault.New(fault.InvalidInput, summary), true
	}

	if _, err := runtime.config.Public.Invoke(ctx, invocation); err != nil {
		if ctx.Err() != nil {
			return cancelledOutcome(state), nil, true
		}
		class := faultClassOf(err, fault.InternalFailure)
		summary := "task.ask could not be published"
		return Outcome{
				Status:        OutcomeFailed,
				Summary:       summary,
				Class:         class,
				CheckpointRef: ref,
				Completed:     copyStrings(state.completed),
				Uncertain:     copyStrings(state.uncertain),
				Usage:         state.usage,
			},
			fault.Wrap(class, summary, err), true
	}
	return Outcome{
		Status:        OutcomeWaiting,
		Summary:       arguments.Question,
		CheckpointRef: ref,
		Completed:     copyStrings(state.completed),
		Usage:         state.usage,
	}, nil, true
}

// saveCheckpoint persists durable context. Cancellation never aborts the save:
// an effect that may have run must leave a record behind.
func (runtime *Runtime) saveCheckpoint(ctx context.Context, input RunInput, state *runState, messages []Message, completed, inFlight, uncertain []string) (string, error) {
	checkpoint := Checkpoint{
		Version:     CheckpointVersion,
		TaskID:      input.TaskID,
		ExecutionID: input.ExecutionID,
		Messages:    runtime.durableMessages(closedConversation(messages)),
		Completed:   append([]string(nil), completed...),
		InFlight:    append([]string(nil), inFlight...),
		Uncertain:   append([]string(nil), uncertain...),
		Usage:       state.usage,
		CreatedAt:   runtime.config.Clock.Now(),
	}
	if err := checkpoint.Validate(); err != nil {
		return "", fault.Wrap(fault.InternalFailure, "checkpoint is invalid", err)
	}
	ref, err := runtime.config.Checkpoints.Save(context.WithoutCancel(ctx), checkpoint)
	if err != nil {
		return "", err
	}
	if !ValidCheckpointRef(ref) {
		return "", fault.New(fault.InternalFailure, "checkpoint store returned an invalid reference")
	}
	return ref, nil
}

// uncertainOutcome records an effect whose outcome is unknown and stops.
func (runtime *Runtime) uncertainOutcome(ctx context.Context, input RunInput, state *runState, effect string) Outcome {
	uncertain := append(append([]string(nil), state.uncertain...), effect)
	ref, err := runtime.saveCheckpoint(ctx, input, state, state.messages, state.completed, nil, uncertain)
	if err == nil {
		state.uncertain = uncertain
		state.checkpointRef = ref
	}
	return Outcome{
		Status:        OutcomeUncertain,
		Summary:       "tool effect is uncertain",
		CheckpointRef: state.checkpointRef,
		Completed:     copyStrings(state.completed),
		Uncertain:     copyStrings(uncertain),
		Usage:         state.usage,
		Class:         fault.UncertainExternalEffect,
	}
}

// cancelledOutcome reports a cancelled or expired run.
func cancelledOutcome(state *runState) Outcome {
	return Outcome{
		Status:        OutcomeCancelled,
		Summary:       "run cancelled",
		CheckpointRef: state.checkpointRef,
		Completed:     copyStrings(state.completed),
		Uncertain:     copyStrings(state.uncertain),
		Usage:         state.usage,
	}
}

// refusalOutcome reports a refused tool request.
func refusalOutcome(state *runState, summary string) Outcome {
	return Outcome{
		Status:        OutcomeRefused,
		Summary:       summary,
		Class:         fault.Forbidden,
		CheckpointRef: state.checkpointRef,
		Completed:     copyStrings(state.completed),
		Uncertain:     copyStrings(state.uncertain),
		Usage:         state.usage,
	}
}

// invalidOutcome reports a bounded invalid request.
func invalidOutcome(state *runState, summary string) Outcome {
	return Outcome{
		Status:        OutcomeFailed,
		Summary:       summary,
		Class:         fault.InvalidInput,
		CheckpointRef: state.checkpointRef,
		Completed:     copyStrings(state.completed),
		Uncertain:     copyStrings(state.uncertain),
		Usage:         state.usage,
	}
}

// faultClassOf returns the fault class of err or the fallback class.
func faultClassOf(err error, fallback fault.Class) fault.Class {
	if class, ok := fault.ClassOf(err); ok {
		return class
	}
	return fallback
}

// copyStrings returns a defensive copy of values.
func copyStrings(values []string) []string {
	return append([]string(nil), values...)
}

// containsString reports whether values contains target.
func containsString(values []string, target string) bool {
	return slices.Contains(values, target)
}

// isReservedTool reports whether name carries authority the agent must not use.
// Registry operations and tool names are dotted lowercase, but the comparison
// folds case and the hyphen/underscore/slash slug separators so an aliased tool
// name cannot reach a reserved operation.
func isReservedTool(name string) bool {
	return containsString(reservedToolNames, normalizeToolName(name))
}

// normalizeToolName folds one tool name to its dotted lowercase form.
func normalizeToolName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var builder strings.Builder
	builder.Grow(len(name))
	for index := 0; index < len(name); index++ {
		switch name[index] {
		case '.', '-', '_', '/', ':', '\\':
			builder.WriteByte('.')
		default:
			builder.WriteByte(name[index])
		}
	}
	return builder.String()
}

// genericToolParameters is the object schema advertised for an ordinary
// native tool whose payload is validated by the runtime and its tool port.
const genericToolParameters = `{"type":"object","properties":{}}`

// askToolParameters makes the task.ask requirements explicit to the provider:
// one bounded question and one assignment.
const askToolParameters = `{"type":"object","properties":{"question":{"type":"string"},"assignment":{"type":"string"}},"required":["question","assignment"],"additionalProperties":false}`

// toolDeclarations translates the run allowlist into provider-facing tool
// declarations. Only allowlisted native tools are advertised, each name is
// translated before validation, the declaration count and encoded name bounds
// are enforced, and task.ask carries its explicit question/assignment schema.
func toolDeclarations(native []string) ([]ToolDeclaration, error) {
	if len(native) > MaxToolDeclarations {
		return nil, fmt.Errorf("run allowlists %d tools; at most %d are allowed", len(native), MaxToolDeclarations)
	}
	declarations := make([]ToolDeclaration, 0, len(native))
	seen := make(map[string]struct{}, len(native))
	for _, name := range native {
		encoded, err := ProviderToolName(name)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[encoded]; duplicate {
			return nil, fmt.Errorf("run allowlists tool %q more than once", name)
		}
		seen[encoded] = struct{}{}
		parameters := genericToolParameters
		if name == "task.ask" {
			parameters = askToolParameters
		}
		declarations = append(declarations, ToolDeclaration{
			Name:        encoded,
			Description: toolDescription(name),
			Parameters:  json.RawMessage(parameters),
		})
	}
	return declarations, nil
}

// toolDescription describes one advertised native tool.
func toolDescription(native string) string {
	if native == "task.ask" {
		return "Ask the task owner one bounded question and assign the reply."
	}
	return "Invoke the native Kodama tool " + native + "."
}

// assignCallIdentities records one assistant call turn. A provider call id is
// preserved; a legacy fixture that omits it receives a deterministic
// runtime-local correlation id that cannot collide with a supplied or restored
// id. The native name is translated for the provider transcript and the exact
// argument bytes are preserved.
func (state *runState) assignCallIdentities(calls []ToolRequest) ([]ToolCall, error) {
	used := make(map[string]struct{})
	for _, message := range state.messages {
		for _, call := range message.ToolCalls {
			if call.ID != "" {
				used[call.ID] = struct{}{}
			}
		}
	}
	for _, call := range calls {
		if call.ID != "" {
			used[call.ID] = struct{}{}
		}
	}
	recorded := make([]ToolCall, len(calls))
	for index, call := range calls {
		id := call.ID
		if id == "" {
			for {
				state.correlation++
				candidate := fmt.Sprintf("call-runtime-%d", state.correlation)
				if _, exists := used[candidate]; !exists {
					id = candidate
					break
				}
			}
			used[id] = struct{}{}
		}
		encoded, err := ProviderToolName(call.Name)
		if err != nil {
			return nil, err
		}
		recorded[index] = ToolCall{ID: id, Name: encoded, Arguments: callArguments(call.Arguments)}
	}
	return recorded, nil
}

// callArguments renders one request's arguments as a call-turn argument string.
// Absent or null arguments become the empty object; present bytes are preserved
// exactly.
func callArguments(arguments json.RawMessage) string {
	trimmed := strings.TrimSpace(string(arguments))
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	return string(arguments)
}

// askIdempotencyKey derives a stable bounded key for one task.ask handoff.
func askIdempotencyKey(input RunInput, effect string) string {
	sum := sha256.Sum256([]byte(input.TaskID + "\x00" + input.ExecutionID + "\x00" + effect))
	return "ask:" + hex.EncodeToString(sum[:20])
}
