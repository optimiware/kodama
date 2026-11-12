//go:build integration

package services

// Native and external agent restoration across a conversation handoff on real
// Shell-launched executions.
//
// Each scenario runs the agent fixture image as an ordinary container action.
// The action reads its public credential from the mounted
// /kodama/secrets/KODAMA_TOKEN and its external checkpoint capability from the
// declared /kodama/secrets/KODAMA_CHECKPOINT_TOKEN secret, restores through the
// real checkpoint adapter, and reports its observation as the single JSON
// object on stdout.
//
// The first attempt writes a correlated tool call and result into the durable
// external store and then asks with its own mounted execution credential,
// referencing that checkpoint. The reviewer's reply carries the same reference
// into the fresh execution's admitted conversation. The fresh execution
// restores the transcript, receives the model's repeated effect request, and
// must skip it by its durable effect identity while running in a fresh
// workspace.
//
// The checkpoint service is a fixture and stores through the real
// app.CheckpointStorage adapter; the native workload restores through
// app.CheckpointPort and the session store. Missing, expired and corrupt
// checkpoints must fail the restoring action, not silently continue from an
// empty conversation.
//
// The native row exercises the execution-initiated ask path: the workload's
// admitted ExecOwnTaskAsk permission authorizes its own task.ask without a
// human/service grant. The external row and the restore-failure rows keep the
// operator-ask path as a separate positive and control case.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/userland/agent"
	agentapp "github.com/optimiware/kodama/internal/userland/agent/app"
	"github.com/optimiware/kodama/internal/userland/session"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	agentFixtureBinary     = "/usr/local/bin/kodama-agent-workload"
	agentFixtureMarker     = "/etc/kodama-agent-workload-marker"
	agentFixtureAction     = "agent"
	agentFixtureTimeout    = "10m"
	agentFixtureWorkspace  = "handoff-marker"
	agentFixtureInitial    = "perform the first step"
	agentFixtureQuestion   = "The first step is complete; continue from the checkpoint?"
	agentFixtureAssignment = "role/reviewer"
)

// agentPipelineDoc is the API-posted pipeline for one agent attempt.
type agentPipelineDoc struct {
	Actions []agentPipelineActionDoc `yaml:"actions"`
}

// agentPipelineActionDoc is one container action. The checkpoint capability is
// a declared secret, so it is mounted only in this execution's workspace.
type agentPipelineActionDoc struct {
	Name     string   `yaml:"name"`
	Image    string   `yaml:"image"`
	Commands []string `yaml:"commands,omitempty"`
	Secrets  []string `yaml:"secrets,omitempty"`
	Timeout  string   `yaml:"timeout,omitempty"`
}

// agentApply posts one agent pipeline through the public API.
func agentApply(t *testing.T, harness *factory.Harness, name string, document agentPipelineDoc) api.PipelineApplyResult {
	t.Helper()
	encoded, err := yaml.Marshal(document)
	if err != nil {
		t.Fatalf("marshal agent pipeline: %v", err)
	}
	for _, action := range document.Actions {
		t.Logf("agent pipeline %s action %s image %s", name, action.Name, action.Image)
	}
	result := harness.MustREST(harness.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      name,
		YAML:      encoded,
	}, nil, "apply-"+name)
	var applied api.PipelineApplyResult
	harness.Decode(result, &applied)
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("pipeline.apply returned %+v; want a committed pipeline", applied)
	}
	return applied
}

// agentReadTask reads one task through the served edge.
func agentReadTask(t *testing.T, harness *factory.Harness, token, taskID string) api.TaskReadResult {
	t.Helper()
	result := harness.MustREST(token, "task.read", api.TaskReadRequest{TaskID: taskID}, nil, "")
	var read api.TaskReadResult
	harness.Decode(result, &read)
	return read
}

// agentCreateTask creates one open task bound to the pipeline.
func agentCreateTask(t *testing.T, harness *factory.Harness, pipelineID, name string) api.TaskCreateResult {
	t.Helper()
	var created api.TaskCreateResult
	harness.Decode(harness.MustREST(harness.ProjectToken, "task.create", api.TaskCreateRequest{
		ProjectID:  factory.ProjectID,
		Title:      name,
		PipelineID: pipelineID,
	}, nil, "create-"+name), &created)
	if created.TaskID == "" || created.LifecycleVersion == 0 {
		t.Fatalf("task.create returned %+v; want a committed task", created)
	}
	return created
}

// agentStartTask starts one open task at its current lifecycle version.
func agentStartTask(t *testing.T, harness *factory.Harness, taskID string) string {
	t.Helper()
	read := agentReadTask(t, harness, harness.ProjectToken, taskID)
	var started api.TaskStartResult
	harness.Decode(harness.MustREST(harness.ProjectToken, "task.start", api.TaskStartRequest{
		TaskID: taskID,
	}, &read.LifecycleVersion, "start-"+taskID), &started)
	if started.ExecutionID == "" {
		t.Fatalf("task.start returned %+v; want a queued execution", started)
	}
	return started.ExecutionID
}

// agentCreateAndStart creates one open task bound to the pipeline and starts it.
func agentCreateAndStart(t *testing.T, harness *factory.Harness, pipelineID, name string) (string, string) {
	t.Helper()
	created := agentCreateTask(t, harness, pipelineID, name)
	return created.TaskID, agentStartTask(t, harness, created.TaskID)
}

// agentWaitTaskState waits for the served task state and fails early with the
// execution logs when the current execution settles while the expected state is
// still pending.
func agentWaitTaskState(t *testing.T, harness *factory.Harness, token, taskID, want string) api.TaskReadResult {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	var last api.TaskReadResult
	for time.Now().Before(deadline) {
		last = agentReadTask(t, harness, token, taskID)
		if last.State == want {
			return last
		}
		if last.CurrentExecution != "" {
			result, publicError := harness.REST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
				ExecutionID: last.CurrentExecution,
			}, nil, "")
			if publicError.Code == "" {
				var execution api.ExecutionInspectResult
				harness.Decode(result, &execution)
				switch execution.State {
				case "failed", "cancelled":
					logs := ""
					if action := actionByNameOrEmpty(execution, agentFixtureAction); action.ActionID != "" {
						logs = agentActionLogs(t, harness, execution.ExecutionID, action.ActionID)
					}
					t.Fatalf("task %s state %q while waiting for %q; execution %s state %q failure %+v logs=%s",
						taskID, last.State, want, execution.ExecutionID, execution.State, execution.Failure, logs)
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("task %s state %q did not reach %q within 2m (last %+v)", taskID, last.State, want, last)
	return last
}

// actionByNameOrEmpty returns the named action or a zero value.
func actionByNameOrEmpty(execution api.ExecutionInspectResult, name string) api.ExecutionAction {
	for _, action := range execution.Actions {
		if action.Name == name {
			return action
		}
	}
	return api.ExecutionAction{}
}

// agentAsk publishes one human task.ask that references the durable checkpoint.
func agentAsk(t *testing.T, harness *factory.Harness, taskID, reference string) api.TaskReadResult {
	t.Helper()
	read := agentReadTask(t, harness, harness.ProjectToken, taskID)
	harness.MustREST(harness.EngineerToken, "task.ask", api.TaskAskRequest{
		TaskID:     taskID,
		Question:   agentFixtureQuestion,
		Assignment: agentFixtureAssignment,
		References: []string{reference},
	}, &read.LifecycleVersion, "ask-"+taskID)
	return agentWaitTaskState(t, harness, harness.ProjectToken, taskID, "waiting")
}

// agentReply replies to the outstanding question as the assigned reviewer. The
// answer is the durable checkpoint reference, so the fresh execution's admitted
// conversation carries it into the restoring action.
func agentReply(t *testing.T, harness *factory.Harness, taskID, reference string) api.TaskReplyResult {
	t.Helper()
	read := agentWaitTaskState(t, harness, harness.ProjectToken, taskID, "waiting")
	if read.Question == nil {
		t.Fatalf("waiting task %s has no outstanding question: %+v", taskID, read)
	}
	var replied api.TaskReplyResult
	harness.Decode(harness.MustREST(harness.ReviewerToken, "task.reply", api.TaskReplyRequest{
		TaskID:                  taskID,
		QuestionID:              read.Question.QuestionID,
		ExpectedQuestionVersion: read.Question.Version,
		Answer:                  reference,
	}, &read.LifecycleVersion, "reply-"+taskID), &replied)
	if replied.ExecutionID == "" {
		t.Fatalf("task.reply returned %+v; want a fresh execution", replied)
	}
	return replied
}

// agentActionLogs reads the retained stdout and stderr of one action.
func agentActionLogs(t *testing.T, harness *factory.Harness, executionID, actionID string) string {
	t.Helper()
	var builder strings.Builder
	for _, stream := range []string{"stdout", "stderr"} {
		result, publicError := harness.REST(harness.ProjectToken, "execution.logs.read", api.ExecutionLogsReadRequest{
			ExecutionID: executionID,
			ActionID:    actionID,
			Stream:      stream,
		}, nil, "")
		if publicError.Code != "" {
			continue
		}
		var read api.ExecutionLogsReadResult
		harness.Decode(result, &read)
		for _, chunk := range read.Chunks {
			_, _ = builder.Write(chunk.Data)
		}
	}
	return builder.String()
}

// agentWorkloadOutput decodes the single workload JSON object from the action's
// retained logs.
func agentWorkloadOutput(t *testing.T, harness *factory.Harness, executionID, actionID string) factory.AgentWorkloadOutput {
	t.Helper()
	logs := agentActionLogs(t, harness, executionID, actionID)
	for line := range strings.SplitSeq(logs, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, `{"mode"`) {
			continue
		}
		var decoded factory.AgentWorkloadOutput
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			continue
		}
		return decoded
	}
	t.Fatalf("action %s of execution %s printed no workload JSON: %s", actionID, executionID, logs)
	return factory.AgentWorkloadOutput{}
}

// agentShellQuote renders one shell word.
func agentShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// agentCACommand materializes the private fixture CA inside the workspace.
func agentCACommand(harness *factory.Harness) string {
	material := string(harness.TLS.CAPEM)
	if !strings.HasSuffix(material, "\n") {
		material += "\n"
	}
	return "cat > /workspace/fixture-ca.pem <<'KODAMA_AGENT_CA'\n" + material + "KODAMA_AGENT_CA"
}

// agentWorkloadCommand renders the exact workload invocation. The credential and
// checkpoint capability are mounted file paths, never argv. allowedTools names
// the native tools the run may request; task.ask is added only for the
// execution-initiated handoff row.
func agentWorkloadCommand(checkpointURL, mode string, failAfterRun bool, allowedTools ...string) string {
	command := agentFixtureBinary +
		" -mode " + agentShellQuote(mode) +
		" -base-url \"$KODAMA_URL\"" +
		" -credential-file /kodama/secrets/KODAMA_TOKEN" +
		" -checkpoint-url " + agentShellQuote(checkpointURL) +
		" -checkpoint-credential-file /kodama/secrets/KODAMA_CHECKPOINT_TOKEN" +
		" -ca-file /workspace/fixture-ca.pem" +
		" -marker-file " + agentShellQuote(agentFixtureMarker) +
		" -project " + agentShellQuote(factory.ProjectID) +
		" -task \"$KODAMA_TASK\"" +
		" -execution \"$KODAMA_EXECUTION\"" +
		" -model " + agentShellQuote(factory.ModelAlias)
	if len(allowedTools) == 0 {
		allowedTools = []string{"workspace.write"}
	}
	for _, tool := range allowedTools {
		command += " -tool " + agentShellQuote(tool)
	}
	command += " -restore-from-conversation" +
		" -workspace-probe " + agentShellQuote(agentFixtureWorkspace) +
		" -workspace-marker " + agentShellQuote(agentFixtureWorkspace) +
		" -message " + agentShellQuote(agentFixtureInitial)
	if failAfterRun {
		command += " -fail-after-run"
	}
	return command
}

// agentPipeline builds the single-action agent pipeline.
func agentPipeline(harness *factory.Harness, image, checkpointURL, mode string, failAfterRun bool, allowedTools ...string) agentPipelineDoc {
	return agentPipelineDoc{Actions: []agentPipelineActionDoc{{
		Name:     agentFixtureAction,
		Image:    image,
		Timeout:  agentFixtureTimeout,
		Secrets:  []string{"KODAMA_CHECKPOINT_TOKEN"},
		Commands: []string{agentCACommand(harness), agentWorkloadCommand(checkpointURL, mode, failAfterRun, allowedTools...)},
	}}}
}

// agentStartFixture builds one shared real fixture: the bounded external
// checkpoint service, the TLS factory with the deterministic model gateway and
// the declared checkpoint capability, and the digest-pinned workload image.
func agentStartFixture(t *testing.T, fixture *factory.ModelFixture) (*factory.Harness, *factory.AgentCheckpointService, *factory.AgentWorkload) {
	t.Helper()
	service := factory.StartAgentCheckpointService(t, factory.AgentCheckpointConfig{
		Root:         filepath.Join(t.TempDir(), "agent-checkpoints"),
		WorkloadHost: factory.WorkloadHost(t),
	})
	harness := factory.Start(t,
		factory.WithTLS(),
		factory.WithModelFixture(fixture),
		factory.WithSecretRules(secret.NameRule{
			Scope: tenancy.Scope{
				TenantID:  id.TenantID(factory.TenantID),
				ProjectID: id.ProjectID(factory.ProjectID),
			},
			Names: []id.SecretName{"KODAMA_CHECKPOINT_TOKEN"},
		}),
		factory.WithSecrets(map[string]string{"KODAMA_CHECKPOINT_TOKEN": service.Token()}),
	)
	workload := factory.EnsureAgentWorkload(t, harness)
	return harness, service, workload
}

// agentCheckpointPort opens the real Userland checkpoint port over the durable
// service directory. It is the host-side observation of the same bytes the
// workload restored.
func agentCheckpointPort(t *testing.T, service *factory.AgentCheckpointService) *agentapp.CheckpointPort {
	t.Helper()
	storage, err := agentapp.NewCheckpointStorage(agentapp.StorageConfig{Root: service.Root()})
	if err != nil {
		t.Fatalf("NewCheckpointStorage: %v", err)
	}
	port, err := agentapp.NewCheckpointPort(storage, agentapp.SystemClock{}, session.RetentionPolicy{
		RawContent: true,
		Retention:  session.DefaultRawContentRetention,
	})
	if err != nil {
		t.Fatalf("NewCheckpointPort: %v", err)
	}
	return port
}

// agentSessionStore opens the real durable session store over the service
// directory. The external harness owns its transcript shape (no native effect
// bookkeeping), so its checkpoints are read through this boundary.
func agentSessionStore(t *testing.T, service *factory.AgentCheckpointService) session.Store {
	t.Helper()
	storage, err := agentapp.NewCheckpointStorage(agentapp.StorageConfig{Root: service.Root()})
	if err != nil {
		t.Fatalf("NewCheckpointStorage: %v", err)
	}
	store, err := session.NewStore(session.StoreConfig{Storage: storage, Clock: agentapp.SystemClock{}})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// agentRecordCheckpoint maps one restored session record onto the agent
// conversation shape used by the correlation assertion.
func agentRecordCheckpoint(record session.Record) agent.Checkpoint {
	messages := make([]agent.Message, len(record.Messages))
	for index, message := range record.Messages {
		messages[index] = agent.Message{
			Role:       message.Role,
			Content:    message.Content,
			ToolCallID: agent.ToolCallID(message.ToolCallID),
		}
		for _, call := range message.ToolCalls {
			messages[index].ToolCalls = append(messages[index].ToolCalls, agent.ToolCall{
				ID:        call.ID,
				Name:      call.Name,
				Arguments: call.Arguments,
			})
		}
	}
	return agent.Checkpoint{
		Version:     agent.CheckpointVersion,
		TaskID:      record.TaskID,
		ExecutionID: record.ExecutionID,
		Messages:    messages,
		CreatedAt:   record.CreatedAt,
	}
}

// agentRequireCorrelatedTool asserts the durable transcript carries one real
// tool call and the tool result that answers its exact call identity.
func agentRequireCorrelatedTool(t *testing.T, checkpoint agent.Checkpoint, callID, wantResult string) {
	t.Helper()
	provider, err := agent.ProviderToolName("workspace.write")
	if err != nil {
		t.Fatalf("ProviderToolName: %v", err)
	}
	if err := agent.ValidateConversation(checkpoint.Messages); err != nil {
		t.Fatalf("checkpoint conversation is not a closed correlated transcript: %v", err)
	}
	foundCall := false
	foundResult := false
	for _, message := range checkpoint.Messages {
		for _, call := range message.ToolCalls {
			if call.ID == callID {
				foundCall = true
				if call.Name != provider {
					t.Fatalf("tool call %q name = %q; want the provider name %q", callID, call.Name, provider)
				}
			}
		}
		if message.Role == "tool" && string(message.ToolCallID) == callID {
			foundResult = true
			if !strings.Contains(message.Content, wantResult) {
				t.Fatalf("tool result %q = %q; want it to contain %q", callID, message.Content, wantResult)
			}
		}
	}
	if !foundCall || !foundResult {
		t.Fatalf("checkpoint lacks call/result pair %q: call=%v result=%v messages=%+v",
			callID, foundCall, foundResult, checkpoint.Messages)
	}
}

// agentUsageByExecution counts the committed model usage rows per execution.
func agentUsageByExecution(t *testing.T, harness *factory.Harness) map[string]int {
	t.Helper()
	rows, err := harness.ModelUsageRows(context.Background())
	if err != nil {
		t.Fatalf("read model usage rows: %v", err)
	}
	counts := make(map[string]int)
	for _, row := range rows {
		execution, _ := row.Values["execution_id"].(string)
		if execution == "" {
			continue
		}
		counts[execution]++
	}
	return counts
}

// agentRequireCheckpointRefusesUnauthenticated proves the mounted capability
// token is required: the same read without it is refused.
func agentRequireCheckpointRefusesUnauthenticated(t *testing.T, service *factory.AgentCheckpointService) {
	t.Helper()
	body := `{"ref":"kodama-checkpoint/v1/` + strings.Repeat("a", 32) + `-1"}`
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		service.Endpoint()+"/v1/checkpoints/read", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build unauthenticated checkpoint request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("unauthenticated checkpoint request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated checkpoint read status = %d; want 401", response.StatusCode)
	}
}

// agentRequireNoCredentialInEvidence checks the known credentials never reach
// the workload's outputs or retained logs.
func agentRequireNoCredentialInEvidence(t *testing.T, harness *factory.Harness, service *factory.AgentCheckpointService, evidence ...string) {
	t.Helper()
	for _, value := range evidence {
		if strings.Contains(value, service.Token()) {
			t.Fatal("evidence carries the checkpoint capability token")
		}
		if strings.Contains(value, harness.ProjectToken) {
			t.Fatal("evidence carries the session token instead of the execution credential")
		}
	}
}

// agentModelExchange builds one deterministic provider exchange over the
// workspace.write declaration.
func agentModelExchange(name string, toolCallID string, reply factory.ModelReply) factory.ModelExchange {
	return agentModelExchangeWithTools(name, toolCallID, reply, "workspace.write")
}

// agentModelExchangeWithTools builds one exchange whose exact tool declaration
// list is the given native names in order.
func agentModelExchangeWithTools(name, toolCallID string, reply factory.ModelReply, tools ...string) factory.ModelExchange {
	providers := make([]string, len(tools))
	for index, native := range tools {
		provider, err := agent.ProviderToolName(native)
		if err != nil {
			panic(err)
		}
		providers[index] = provider
	}
	return factory.ModelExchange{
		Name:   name,
		Expect: factory.ModelExpectation{Tools: providers, ToolCallID: toolCallID},
		Reply:  reply,
	}
}

// agentAskToolPrompt renders the native task.ask handoff arguments.
func agentAskToolPrompt(question, assignment string) string {
	arguments, err := json.Marshal(map[string]string{
		"question":   question,
		"assignment": assignment,
	})
	if err != nil {
		panic(err)
	}
	return string(arguments)
}

// agentAskToolCallReply scripts one native task.ask handoff call.
func agentAskToolCallReply(callID, question, assignment string) factory.ModelReply {
	provider, err := agent.ProviderToolName("task.ask")
	if err != nil {
		panic(err)
	}
	return factory.ModelReply{
		ContentNull: true,
		ToolCalls: []api.ChatToolCall{{
			ID:   callID,
			Type: "function",
			Function: api.ChatToolCallFunction{
				Name:      provider,
				Arguments: agentAskToolPrompt(question, assignment),
			},
		}},
		FinishReason: "tool_calls",
		Usage:        api.ChatUsage{PromptTokens: 9, CompletionTokens: 4, TotalTokens: 13},
	}
}

// agentToolCallReply scripts one workspace.write call.
func agentToolCallReply(callID, arguments string) factory.ModelReply {
	provider, err := agent.ProviderToolName("workspace.write")
	if err != nil {
		panic(err)
	}
	return factory.ModelReply{
		ContentNull: true,
		ToolCalls: []api.ChatToolCall{{
			ID:   callID,
			Type: "function",
			Function: api.ChatToolCallFunction{
				Name:      provider,
				Arguments: arguments,
			},
		}},
		FinishReason: "tool_calls",
		Usage:        api.ChatUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
	}
}

// agentFinalReply scripts one final assistant answer.
func agentFinalReply(content string) factory.ModelReply {
	return factory.ModelReply{
		Content:      content,
		FinishReason: "stop",
		Usage:        api.ChatUsage{PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15},
	}
}

// agentRunHandoff drives the supported human handoff and returns the fresh
// execution. It asserts the question references the durable checkpoint.
func agentRunHandoff(t *testing.T, harness *factory.Harness, taskID, reference string) api.TaskReplyResult {
	t.Helper()
	task := agentAsk(t, harness, taskID, reference)
	if task.Question == nil || len(task.Question.References) != 1 || task.Question.References[0] != reference {
		t.Fatalf("question references = %+v; want the checkpoint %q", task.Question, reference)
	}
	return agentReply(t, harness, taskID, reference)
}

// agentWaitWorkloadAsk waits for an execution-initiated task.ask and returns the
// waiting task with its durable checkpoint reference. The operator publishes
// nothing: the workload's own mounted execution credential asks.
func agentWaitWorkloadAsk(t *testing.T, harness *factory.Harness, taskID string) api.TaskReadResult {
	t.Helper()
	task := agentWaitTaskState(t, harness, harness.ProjectToken, taskID, "waiting")
	if task.Question == nil {
		t.Fatalf("waiting task %s has no outstanding question: %+v", taskID, task)
	}
	if len(task.Question.References) != 1 {
		t.Fatalf("workload question references = %+v; want one durable checkpoint", task.Question.References)
	}
	return task
}

// TestFactoryNativeAgentHandoffRestoresCheckpoint proves the native app
// composition restores a correlated transcript across a real task handoff on
// Shell-launched executions: the first execution saves its checkpoint and asks
// the task question itself with the mounted execution credential; the reviewer's
// reply starts a fresh execution with a fresh workspace and a new mounted
// credential; the agent restores the transcript and does not replay the
// completed workspace effect.
func TestFactoryNativeAgentHandoffRestoresCheckpoint(t *testing.T) {
	const firstCall = "call-native-write-1"
	const replayCall = "call-native-write-2"
	const askCall = "call-native-ask-1"
	const firstArguments = `{"path":"agent-note","content":"attempt one"}`
	const restoredAnswer = "restored after handoff"
	nativeTools := []string{"workspace.write", "task.ask"}
	fixture := factory.NewModelFixture(t,
		agentModelExchangeWithTools("native-tool-call", "", agentToolCallReply(firstCall, firstArguments), nativeTools...),
		agentModelExchangeWithTools("native-workload-ask", firstCall, agentAskToolCallReply(askCall, agentFixtureQuestion, agentFixtureAssignment), nativeTools...),
		agentModelExchangeWithTools("native-replayed-effect", firstCall, agentToolCallReply(replayCall, firstArguments), nativeTools...),
		agentModelExchangeWithTools("native-restored-final", replayCall, agentFinalReply(restoredAnswer), nativeTools...),
	)
	harness, service, workload := agentStartFixture(t, fixture)
	applied := agentApply(t, harness, "agent-native-handoff", agentPipeline(harness, workload.Image().Reference, service.WorkloadURL(), "native", false, nativeTools...))
	taskID, firstExecutionID := agentCreateAndStart(t, harness, applied.PipelineID, "agent-native-handoff")

	// No operator publishes the ask: the workload's native task.ask handoff
	// does it with its own execution credential and the checkpoint reference.
	asked := agentWaitWorkloadAsk(t, harness, taskID)
	if asked.Question.Assignment != agentFixtureAssignment {
		t.Fatalf("workload question assignment = %q; want %q", asked.Question.Assignment, agentFixtureAssignment)
	}
	reference := asked.Question.References[0]
	if !agent.ValidCheckpointRef(reference) {
		t.Fatalf("agent checkpoint reference %q is not durable (question %+v)", reference, asked.Question)
	}
	if asked.CurrentExecution != firstExecutionID {
		t.Fatalf("waiting task current execution = %q; want the asking execution %q", asked.CurrentExecution, firstExecutionID)
	}
	first := waitExecutionSettled(t, harness, firstExecutionID)
	if first.State != "cancelled" {
		t.Fatalf("asking execution state = %q; want cancelled by its own task.ask (%+v)", first.State, first.Failure)
	}

	port := agentCheckpointPort(t, service)
	firstCheckpoint, err := port.Restore(context.Background(), reference)
	if err != nil {
		t.Fatalf("restore first checkpoint through the real port: %v", err)
	}
	if firstCheckpoint.TaskID != taskID || firstCheckpoint.ExecutionID != firstExecutionID {
		t.Fatalf("first checkpoint identity = task %q execution %q; want %q/%q",
			firstCheckpoint.TaskID, firstCheckpoint.ExecutionID, taskID, firstExecutionID)
	}
	if len(firstCheckpoint.Completed) != 1 {
		t.Fatalf("first checkpoint completed effects = %v; want one durable effect", firstCheckpoint.Completed)
	}
	agentRequireCorrelatedTool(t, firstCheckpoint, firstCall, "wrote")
	agentRequireNoCredentialInEvidence(t, harness, service, fmt.Sprintf("%+v", firstCheckpoint), fmt.Sprintf("%+v", asked))

	replied := agentReply(t, harness, taskID, reference)
	second := waitExecutionSettled(t, harness, replied.ExecutionID)
	if second.State != "succeeded" {
		t.Fatalf("restored execution state = %q; want succeeded (failure %+v, logs=%s)",
			second.State, second.Failure, agentActionLogs(t, harness, replied.ExecutionID, actionByName(t, second, agentFixtureAction).ActionID))
	}
	restoredAction := actionByName(t, second, agentFixtureAction)
	restoredOutput := agentWorkloadOutput(t, harness, replied.ExecutionID, restoredAction.ActionID)
	if !restoredOutput.Restored || restoredOutput.RestoredRef != reference {
		t.Fatalf("restored workload output = %+v; want the handoff checkpoint %q", restoredOutput, reference)
	}
	if restoredOutput.ToolCalls != 0 {
		t.Fatalf("restored workload executed %d tool call(s); want the completed effect skipped", restoredOutput.ToolCalls)
	}
	// The external row keeps the labelled fail-after-run boundary, so its
	// first workspace is marked before the handoff and its restored probe
	// proves freshness. The execution-initiated ask is stopped by its own
	// handoff, so this row does not claim that marker.
	if !restoredOutput.CredentialMounted || !restoredOutput.CheckpointCredentialMounted {
		t.Fatalf("restored workload did not mount both credentials: %+v", restoredOutput)
	}
	if restoredOutput.Content != restoredAnswer {
		t.Fatalf("restored agent content = %q; want %q", restoredOutput.Content, restoredAnswer)
	}
	if done := agentReadTask(t, harness, harness.ProjectToken, taskID); done.State != "done" || done.CurrentExecution != replied.ExecutionID {
		t.Fatalf("task after restore = state %q execution %q; want done/%q", done.State, done.CurrentExecution, replied.ExecutionID)
	}

	finalReference := restoredOutput.CheckpointRef
	finalCheckpoint, err := port.Restore(context.Background(), finalReference)
	if err != nil {
		t.Fatalf("restore final checkpoint through the real port: %v", err)
	}
	if finalCheckpoint.ExecutionID != replied.ExecutionID {
		t.Fatalf("final checkpoint execution = %q; want the fresh execution %q", finalCheckpoint.ExecutionID, replied.ExecutionID)
	}
	agentRequireCorrelatedTool(t, finalCheckpoint, firstCall, "wrote")
	agentRequireCorrelatedTool(t, finalCheckpoint, replayCall, "already completed")
	for _, message := range finalCheckpoint.Messages {
		if message.Role == "tool" && string(message.ToolCallID) == replayCall && strings.Contains(message.Content, "wrote ") {
			t.Fatalf("the repeated effect was executed again: %q", message.Content)
		}
	}
	if got := lastAssistantContent(finalCheckpoint.Messages); got != restoredAnswer {
		t.Fatalf("final checkpoint content = %q; want %q", got, restoredAnswer)
	}

	requests := fixture.Requests()
	if len(requests) != 4 {
		t.Fatalf("provider requests = %d; want two per execution", len(requests))
	}
	if !slices.Contains(requests[2].ToolCallIDs, firstCall) || !slices.Contains(requests[2].ToolResultIDs, firstCall) {
		t.Fatalf("the restored model request did not carry the correlated pair %q: %+v", firstCall, requests[2])
	}
	if !slices.Contains(requests[3].ToolCallIDs, replayCall) || !slices.Contains(requests[3].ToolResultIDs, replayCall) {
		t.Fatalf("the skipped effect was not correlated for the model: %+v", requests[3])
	}
	usage := agentUsageByExecution(t, harness)
	if usage[firstExecutionID] != 2 || usage[replied.ExecutionID] != 2 {
		t.Fatalf("usage attribution = %v; want two calls per execution", usage)
	}
	agentRequireCheckpointRefusesUnauthenticated(t, service)
}

// TestFactoryExternalAgentHandoffRestoresCheckpoint proves the external harness
// path keeps and restores its own correlated transcript across the same real
// handoff. The external driver owns tool execution, so its transcript is the
// effect ledger that refuses the replay.
func TestFactoryExternalAgentHandoffRestoresCheckpoint(t *testing.T) {
	const firstCall = "call-external-write-1"
	const replayCall = "call-external-write-2"
	const firstArguments = `{"path":"agent-note","content":"external attempt one"}`
	const firstAnswer = "external first attempt complete"
	const restoredAnswer = "external restored after handoff"
	fixture := factory.NewModelFixture(t,
		agentModelExchange("external-tool-call", "", agentToolCallReply(firstCall, firstArguments)),
		agentModelExchange("external-first-final", firstCall, agentFinalReply(firstAnswer)),
		agentModelExchange("external-replayed-effect", firstCall, agentToolCallReply(replayCall, firstArguments)),
		agentModelExchange("external-restored-final", replayCall, agentFinalReply(restoredAnswer)),
	)
	harness, service, workload := agentStartFixture(t, fixture)
	applied := agentApply(t, harness, "agent-external-handoff", agentPipeline(harness, workload.Image().Reference, service.WorkloadURL(), "external", true))
	taskID, firstExecutionID := agentCreateAndStart(t, harness, applied.PipelineID, "agent-external-handoff")
	first := waitExecutionSettled(t, harness, firstExecutionID)
	if first.State != "failed" {
		t.Fatalf("first execution state = %q; want the labelled handoff boundary failure (%+v)", first.State, first.Failure)
	}
	firstAction := actionByName(t, first, agentFixtureAction)
	firstOutput := agentWorkloadOutput(t, harness, firstExecutionID, firstAction.ActionID)
	reference := firstOutput.CheckpointRef
	if !agent.ValidCheckpointRef(reference) {
		t.Fatalf("external checkpoint reference %q is not durable (output %+v)", reference, firstOutput)
	}
	if firstOutput.Restored || firstOutput.ToolCalls != 1 || !firstOutput.WorkspaceClean {
		t.Fatalf("first external output = %+v; want an unrestored run with one real tool call in a clean workspace", firstOutput)
	}
	if !firstOutput.CredentialMounted || !firstOutput.CheckpointCredentialMounted {
		t.Fatalf("first external workload did not mount both credentials: %+v", firstOutput)
	}
	if firstOutput.ImageMarker != workload.Image().Marker {
		t.Fatalf("image marker = %q; want the fixture image marker %q", firstOutput.ImageMarker, workload.Image().Marker)
	}

	store := agentSessionStore(t, service)
	firstRecord, err := store.Restore(context.Background(), session.Ref(reference))
	if err != nil {
		t.Fatalf("restore external checkpoint through the real session store: %v", err)
	}
	firstCheckpoint := agentRecordCheckpoint(firstRecord)
	if firstCheckpoint.ExecutionID != firstExecutionID {
		t.Fatalf("external checkpoint execution = %q; want %q", firstCheckpoint.ExecutionID, firstExecutionID)
	}
	agentRequireCorrelatedTool(t, firstCheckpoint, firstCall, "wrote")

	replied := agentRunHandoff(t, harness, taskID, reference)
	second := waitExecutionSettled(t, harness, replied.ExecutionID)
	if second.State != "succeeded" {
		t.Fatalf("restored external execution state = %q; want succeeded (failure %+v, logs=%s)",
			second.State, second.Failure, agentActionLogs(t, harness, replied.ExecutionID, actionByName(t, second, agentFixtureAction).ActionID))
	}
	restoredAction := actionByName(t, second, agentFixtureAction)
	restoredOutput := agentWorkloadOutput(t, harness, replied.ExecutionID, restoredAction.ActionID)
	if !restoredOutput.Restored || restoredOutput.RestoredRef != reference {
		t.Fatalf("restored external output = %+v; want the handoff checkpoint %q", restoredOutput, reference)
	}
	if restoredOutput.ToolCalls != 0 || !restoredOutput.WorkspaceClean {
		t.Fatalf("restored external output = %+v; want a skipped replay in a fresh workspace", restoredOutput)
	}
	if restoredOutput.RestoredMessages == 0 {
		t.Fatalf("restored external message count = %d; want the durable transcript", restoredOutput.RestoredMessages)
	}
	if restoredOutput.Content != restoredAnswer {
		t.Fatalf("restored external content = %q; want %q", restoredOutput.Content, restoredAnswer)
	}

	finalReference := restoredOutput.CheckpointRef
	finalRecord, err := store.Restore(context.Background(), session.Ref(finalReference))
	if err != nil {
		t.Fatalf("restore final external checkpoint: %v", err)
	}
	finalCheckpoint := agentRecordCheckpoint(finalRecord)
	if finalCheckpoint.ExecutionID != replied.ExecutionID {
		t.Fatalf("final external checkpoint execution = %q; want %q", finalCheckpoint.ExecutionID, replied.ExecutionID)
	}
	agentRequireCorrelatedTool(t, finalCheckpoint, firstCall, "wrote")
	agentRequireCorrelatedTool(t, finalCheckpoint, replayCall, "already completed")
	if got := lastAssistantContent(finalCheckpoint.Messages); got != restoredAnswer {
		t.Fatalf("final external checkpoint content = %q; want %q", got, restoredAnswer)
	}
	requests := fixture.Requests()
	if len(requests) != 4 {
		t.Fatalf("provider requests = %d; want two per execution", len(requests))
	}
	if !slices.Contains(requests[2].ToolResultIDs, firstCall) {
		t.Fatalf("the external restored request lost the correlated pair %q: %+v", firstCall, requests[2])
	}
	usage := agentUsageByExecution(t, harness)
	if usage[firstExecutionID] != 2 || usage[replied.ExecutionID] != 2 {
		t.Fatalf("external usage attribution = %v; want two calls per execution", usage)
	}
}

// TestFactoryAgentCheckpointRestoreFailures proves missing, expired and corrupt
// checkpoints fail the restoring action and leave the task open instead of
// silently starting from incomplete context. The model's single control
// exchange is the positive control: a refused restore must not reach the model.
func TestFactoryAgentCheckpointRestoreFailures(t *testing.T) {
	fixture := factory.NewModelFixture(t,
		agentModelExchange("expired-control", "", agentFinalReply("a refused checkpoint was accepted")),
	)
	harness, service, workload := agentStartFixture(t, fixture)
	applied := agentApply(t, harness, "agent-checkpoint-failures",
		agentPipeline(harness, workload.Image().Reference, service.WorkloadURL(), "native", false))

	cases := []struct {
		name       string
		wantDetail string
	}{
		{name: "missing", wantDetail: "checkpoint was not found"},
		{name: "corrupt", wantDetail: "checkpoint is corrupt"},
		{name: "expired", wantDetail: "expired"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			created := agentCreateTask(t, harness, applied.PipelineID, "agent-failure-"+testCase.name)
			reference := ""
			switch testCase.name {
			case "missing":
				reference = "kodama-checkpoint/v1/" + strings.Repeat("b", 32) + "-1"
			case "corrupt":
				reference = agentSeedCorruptCheckpoint(t, service.Root())
			case "expired":
				reference = agentSeedExpiredCheckpoint(t, service.Root(), created.TaskID, "exec-expired-agent")
			}
			replied := agentRunHandoff(t, harness, created.TaskID, reference)
			execution := waitExecutionSettled(t, harness, replied.ExecutionID)
			if execution.State != "failed" {
				logs := agentActionLogs(t, harness, replied.ExecutionID, actionByName(t, execution, agentFixtureAction).ActionID)
				t.Fatalf("restoring execution state = %q; want failed (failure %+v, logs=%s)", execution.State, execution.Failure, logs)
			}
			action := actionByName(t, execution, agentFixtureAction)
			logs := agentActionLogs(t, harness, replied.ExecutionID, action.ActionID)
			if !strings.Contains(logs, testCase.wantDetail) {
				t.Fatalf("retained logs do not name %q: %s", testCase.wantDetail, logs)
			}
			if task := agentReadTask(t, harness, harness.ProjectToken, created.TaskID); task.State != "open" || task.CurrentExecution != replied.ExecutionID {
				t.Fatalf("failed restore task = state %q execution %q; want open/%q", task.State, task.CurrentExecution, replied.ExecutionID)
			}
		})
	}
	if requests := fixture.Requests(); len(requests) != 0 {
		t.Fatalf("provider requests = %d; a refused checkpoint must not reach the model", len(requests))
	}
}

// agentSeedExpiredCheckpoint writes one real session record whose opted-in raw
// transcript is already past its retention window.
func agentSeedExpiredCheckpoint(t *testing.T, root, taskID, executionID string) string {
	t.Helper()
	storage, err := agentapp.NewCheckpointStorage(agentapp.StorageConfig{Root: root})
	if err != nil {
		t.Fatalf("NewCheckpointStorage: %v", err)
	}
	store, err := session.NewStore(session.StoreConfig{Storage: storage, Clock: agentapp.SystemClock{}})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	record := session.Record{
		Version:     session.RefVersion,
		TaskID:      taskID,
		ExecutionID: executionID,
		SessionID:   session.SessionID("sess-expired-agent"),
		Messages:    []session.Message{{Role: "user", Content: "expired transcript"}},
		Retention:   session.RetentionPolicy{RawContent: true, Retention: time.Second},
		Outcome:     session.Outcome{Status: "checkpoint"},
		CreatedAt:   time.Now().UTC().Add(-time.Hour),
	}
	ref, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("seed expired checkpoint: %v", err)
	}
	return string(ref)
}

// agentSeedCorruptCheckpoint writes unreadable bytes at the exact record path a
// valid checkpoint reference names.
func agentSeedCorruptCheckpoint(t *testing.T, root string) string {
	t.Helper()
	key := strings.Repeat("c", 32)
	ref := "kodama-checkpoint/v1/" + key + "-1"
	path := filepath.Join(root, "record-"+key+"-1.json")
	if err := os.WriteFile(path, []byte("{not a checkpoint"), 0o600); err != nil {
		t.Fatalf("seed corrupt checkpoint: %v", err)
	}
	return ref
}

// lastAssistantContent returns the newest assistant content.
func lastAssistantContent(messages []agent.Message) string {
	content := ""
	for _, message := range messages {
		if message.Role == "assistant" && message.Content != "" {
			content = message.Content
		}
	}
	return content
}
