package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent"
	"github.com/optimiware/kodama/internal/userland/session"
)

// Config is the complete `kodama agent` composition input.
type Config struct {
	// Run is the one bounded run request.
	Run agent.RunInput
	// ProjectID is the public route project identity.
	ProjectID string
	// KnowledgeRoot is the rooted knowledge directory.
	KnowledgeRoot string
	// CheckpointRoot is the owner-only durable checkpoint directory.
	CheckpointRoot string
	// RetainRaw opts into raw model/tool content retention.
	RetainRaw bool
	// Out receives the outcome JSON. A nil writer discards it.
	Out io.Writer

	// Transport is the public client boundary. It is required.
	Transport client.Transport

	// Injected boundaries. A nil value selects the documented public or
	// filesystem default where one exists.
	Model       agent.ModelPort
	Tools       agent.ToolPort
	Knowledge   agent.KnowledgePort
	Checkpoints agent.CheckpointPort
	Clock       agent.Clock
}

// Run executes one bounded native-agent run and writes its outcome. It starts
// no process and uses only public client contracts.
func Run(ctx context.Context, config Config) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := config.Run.Validate(); err != nil {
		return err
	}
	if config.Transport == nil {
		return fault.New(fault.InvalidInput, "agent run requires the public client transport")
	}

	run := config.Run
	// A native handoff needs the task's current lifecycle version as the
	// task.ask expected version. The caller may not know it, so read the task
	// once when the run may ask. The version guard stays in the runtime: an
	// unreadable task never produces a versionless ask.
	if run.TaskVersion == 0 && allowsTool(run.Tools, "task.ask") {
		version, err := readTaskLifecycleVersion(ctx, config.Transport, run.TaskID)
		if err != nil {
			return err
		}
		run.TaskVersion = version
	}

	clock := config.Clock
	if clock == nil {
		clock = SystemClock{}
	}

	model := config.Model
	if model == nil {
		built, err := NewPublicModel(config.Transport, config.ProjectID)
		if err != nil {
			return err
		}
		model = built
	}
	tools := config.Tools
	if tools == nil {
		built, err := NewPublicTools(config.Transport, config.ProjectID)
		if err != nil {
			return err
		}
		tools = built
	}
	checkpoints := config.Checkpoints
	if checkpoints == nil {
		if strings.TrimSpace(config.CheckpointRoot) == "" {
			return fault.New(fault.InvalidInput, "agent run requires a checkpoint directory")
		}
		storage, err := NewCheckpointStorage(StorageConfig{Root: config.CheckpointRoot})
		if err != nil {
			return err
		}
		built, err := NewCheckpointPort(storage, sessionClock{clock: clock}, checkpointRetention(config.RetainRaw))
		if err != nil {
			return err
		}
		checkpoints = built
	}
	knowledge := config.Knowledge
	var closeKnowledge func() error
	if knowledge == nil && len(config.Run.Knowledge) > 0 && strings.TrimSpace(config.KnowledgeRoot) != "" {
		built, err := NewFileKnowledge(config.KnowledgeRoot)
		if err != nil {
			return err
		}
		knowledge = built
		closeKnowledge = built.Close
	}
	if closeKnowledge != nil {
		defer func() { _ = closeKnowledge() }()
	}

	runtime, err := agent.NewRuntime(agent.RuntimeConfig{
		Model:       model,
		Tools:       tools,
		Checkpoints: checkpoints,
		Knowledge:   knowledge,
		Public:      config.Transport,
		Clock:       clock,
		RetainRaw:   config.RetainRaw,
	})
	if err != nil {
		return err
	}
	outcome, err := runtime.Run(ctx, run)
	if config.Out != nil {
		encoder := json.NewEncoder(config.Out)
		encoder.SetEscapeHTML(false)
		if encodeErr := encoder.Encode(outcome); encodeErr != nil {
			return fmt.Errorf("write agent outcome: %w", encodeErr)
		}
	}
	return err
}

// sessionClock adapts the agent clock to the session store clock boundary.
type sessionClock struct {
	clock agent.Clock
}

// Now returns the current time.
func (adapter sessionClock) Now() time.Time { return adapter.clock.Now() }

// checkpointRetention maps the explicit trusted raw-content opt-in to the
// session store's retention policy. Raw content is off by default; opting in
// selects the raw-content window, whose zero duration is the
// seven-day default applied by the session store together with structural
// argument redaction and expiry.
func checkpointRetention(retainRaw bool) session.RetentionPolicy {
	if !retainRaw {
		return session.RetentionPolicy{PlaceholderTranscript: true}
	}
	return session.RetentionPolicy{RawContent: true, Retention: session.DefaultRawContentRetention}
}

// allowsTool reports whether the run allowlist admits one tool.
func allowsTool(tools []string, name string) bool {
	return slices.Contains(tools, name)
}

// readTaskLifecycleVersion reads the served task and returns its current
// lifecycle version. It is the version a task.ask must carry as
// expected_version, so an unreadable or mismatched task fails the run instead
// of publishing a versionless ask.
func readTaskLifecycleVersion(ctx context.Context, transport client.Transport, taskID string) (uint64, error) {
	input, err := json.Marshal(api.TaskReadRequest{TaskID: taskID})
	if err != nil {
		return 0, fault.Wrap(fault.InternalFailure, "task.read input could not be encoded", err)
	}
	result, err := transport.Invoke(ctx, api.Invocation{
		Operation: "task.read",
		Request:   api.Request{Input: input},
	})
	if err != nil {
		if ctx.Err() != nil {
			return 0, fault.Wrap(fault.DeadlineExpired, "task read was canceled", ctx.Err())
		}
		if _, classified := fault.ClassOf(err); classified {
			return 0, err
		}
		return 0, fault.Wrap(fault.DependencyUnavailable, "task read failed", err)
	}
	var task api.TaskReadResult
	if err := decodePublicResult(result, "task.read", &task); err != nil {
		return 0, err
	}
	if err := task.Validate(); err != nil {
		return 0, fault.Wrap(fault.InternalFailure, "task.read result is invalid", err)
	}
	if task.TaskID != taskID {
		return 0, fault.New(fault.InternalFailure, "task.read returned another task")
	}
	return task.LifecycleVersion, nil
}
