// Command agentworkload is the executable half of the agent workload fixture.
//
// It runs inside the fixture image (built from the pinned workload base) and has
// two modes:
//
//   - native composes internal/userland/agent/app.Run with the default
//     PublicModel and the existing checkpoint port over a remote, file-backed
//     external storage adapter;
//   - external drives the public model.invoke contract directly and keeps its
//     own durable transcript through the same storage adapter.
//
// A restoring attempt resolves a durable checkpoint reference from the admitted
// /kodama/task.json conversation (the reply that started the fresh execution) or
// from the explicit -restore flag. A fixture-local workspace.write tool gives
// both modes a real correlated tool call and result in the durable transcript.
// The native runtime skips a completed effect by its durable identity; the
// external driver compares the call against the restored transcript. Neither
// replays the effect.
//
// -fail-after-run lets a first attempt stop at a labelled handoff boundary after
// its checkpoint is durable, so a Shell-launched execution can stay open for the
// operator's ask. It never turns a failed restore into a success.
//
// Neither mode fabricates a model success or a settlement: the public edge, the
// authorizer, the gateway and usage persistence are the served composition.
//
// The workload prints exactly one JSON object on stdout. Credentials are read
// from mounted owner-only files and never enter argv, environment or evidence.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/agent"
	agentapp "github.com/optimiware/kodama/internal/userland/agent/app"
	"github.com/optimiware/kodama/internal/userland/session"
)

const (
	defaultCredentialFile           = "/kodama/secrets/KODAMA_TOKEN"
	defaultCheckpointCredentialFile = "/kodama/secrets/KODAMA_CHECKPOINT_TOKEN"
	defaultMarkerFile               = "/etc/kodama-agent-workload-marker"
	defaultCAFile                   = "/kodama/fixture-ca.pem"
	defaultTaskFile                 = "/kodama/task.json"

	workspaceRoot = "/workspace"

	checkpointSavePath = "/v1/checkpoints/save"
	checkpointReadPath = "/v1/checkpoints/read"

	checkpointCodeMissing      = "missing"
	checkpointCodeConflict     = "version_conflict"
	checkpointCodeTooLarge     = "record_too_large"
	checkpointCodeStoreFull    = "store_full"
	checkpointCodeUnauthorized = "unauthenticated"
	checkpointCodeUnavailable  = "storage_closed"
	checkpointCodeInternal     = "internal_failure"

	checkpointRequestTimeout = 30 * time.Second
	externalModelSteps       = 8
)

// stringList is one repeatable flag.
type stringList []string

func (list *stringList) String() string { return strings.Join(*list, ",") }

func (list *stringList) Set(value string) error {
	*list = append(*list, value)
	return nil
}

// output is the single structured result the workload prints.
type output struct {
	Mode                        string `json:"mode"`
	ImageMarker                 string `json:"image_marker"`
	Status                      string `json:"status"`
	CheckpointRef               string `json:"checkpoint_ref,omitempty"`
	RestoredRef                 string `json:"restored_ref,omitempty"`
	Restored                    bool   `json:"restored"`
	RestoredMessages            int    `json:"restored_messages"`
	ToolCalls                   int    `json:"tool_calls"`
	WorkspaceClean              bool   `json:"workspace_clean"`
	CredentialMounted           bool   `json:"credential_mounted"`
	CheckpointCredentialMounted bool   `json:"checkpoint_credential_mounted"`
	Content                     string `json:"content,omitempty"`
	Error                       string `json:"error,omitempty"`
}

// config is the complete parsed run input. It carries no credential value.
type config struct {
	Mode                     string
	BaseURL                  string
	CredentialFile           string
	CheckpointURL            string
	CheckpointCredentialFile string
	MarkerFile               string
	CAFile                   string
	ProjectID                string
	TaskID                   string
	ExecutionID              string
	Model                    string
	Messages                 []string
	Tools                    []string
	RestoreRef               string
	RestoreFromConversation  bool
	FailAfterRun             bool
	WorkspaceProbe           string
	WorkspaceMarker          string
	TaskFile                 string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses the flags, executes one mode and prints the structured outcome.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agentworkload", flag.ContinueOnError)
	flags.SetOutput(stderr)
	mode := flags.String("mode", "", "native or external")
	baseURL := flags.String("base-url", "", "public edge base URL")
	credentialFile := flags.String("credential-file", defaultCredentialFile, "mounted public credential file")
	checkpointURL := flags.String("checkpoint-url", "", "external checkpoint service URL")
	checkpointCredentialFile := flags.String("checkpoint-credential-file", defaultCheckpointCredentialFile, "mounted checkpoint service token file")
	markerFile := flags.String("marker-file", defaultMarkerFile, "fixture marker baked into the image")
	caFile := flags.String("ca-file", defaultCAFile, "fixture CA file for the public HTTPS edge")
	project := flags.String("project", "", "project identity")
	task := flags.String("task", "", "agent task identity")
	execution := flags.String("execution", "", "agent execution identity")
	model := flags.String("model", "", "configured model alias")
	restore := flags.String("restore", "", "explicit durable checkpoint reference to restore")
	restoreFromConversation := flags.Bool("restore-from-conversation", false, "restore the checkpoint reference carried by the admitted reply")
	failAfterRun := flags.Bool("fail-after-run", false, "stop at the handoff boundary after a first (non-restoring) attempt saves its checkpoint")
	workspaceProbe := flags.String("workspace-probe", "", "workspace-relative file whose absence proves a fresh workspace")
	workspaceMarker := flags.String("workspace-marker", "", "workspace-relative marker written before the handoff boundary")
	taskFile := flags.String("task-file", defaultTaskFile, "materialized task conversation file")
	var messages stringList
	var tools stringList
	flags.Var(&messages, "message", "user message (repeatable)")
	flags.Var(&tools, "tool", "tool allowlist entry (repeatable)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "agentworkload: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	parsed := config{
		Mode:                     strings.TrimSpace(*mode),
		BaseURL:                  strings.TrimSpace(*baseURL),
		CredentialFile:           strings.TrimSpace(*credentialFile),
		CheckpointURL:            strings.TrimSpace(*checkpointURL),
		CheckpointCredentialFile: strings.TrimSpace(*checkpointCredentialFile),
		MarkerFile:               strings.TrimSpace(*markerFile),
		CAFile:                   strings.TrimSpace(*caFile),
		ProjectID:                strings.TrimSpace(*project),
		TaskID:                   strings.TrimSpace(*task),
		ExecutionID:              strings.TrimSpace(*execution),
		Model:                    strings.TrimSpace(*model),
		Messages:                 append([]string(nil), messages...),
		Tools:                    append([]string(nil), tools...),
		RestoreRef:               strings.TrimSpace(*restore),
		RestoreFromConversation:  *restoreFromConversation,
		FailAfterRun:             *failAfterRun,
		WorkspaceProbe:           strings.TrimSpace(*workspaceProbe),
		WorkspaceMarker:          strings.TrimSpace(*workspaceMarker),
		TaskFile:                 strings.TrimSpace(*taskFile),
	}
	if err := parsed.validate(); err != nil {
		fmt.Fprintf(stderr, "agentworkload: %v\n", err)
		return 2
	}

	result := output{Mode: parsed.Mode}
	marker, err := readMarker(parsed.MarkerFile)
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		emit(stdout, result)
		fmt.Fprintf(stderr, "agentworkload: %v\n", err)
		return 1
	}
	result.ImageMarker = marker

	ctx := context.Background()
	switch parsed.Mode {
	case "native":
		result = executeMode(ctx, parsed, marker, runNative)
	case "external":
		result = executeMode(ctx, parsed, marker, runExternal)
	}
	emit(stdout, result)
	if result.Status != "succeeded" {
		if result.Error == "" {
			result.Error = "agent workload failed"
		}
		fmt.Fprintf(stderr, "agentworkload: %s\n", result.Error)
		return 1
	}
	return 0
}

// validate checks the required input before any effect.
func (parsed config) validate() error {
	switch parsed.Mode {
	case "native", "external":
	default:
		return errors.New("-mode must be native or external")
	}
	for name, value := range map[string]string{
		"base url":              parsed.BaseURL,
		"credential file":       parsed.CredentialFile,
		"checkpoint url":        parsed.CheckpointURL,
		"checkpoint credential": parsed.CheckpointCredentialFile,
		"marker file":           parsed.MarkerFile,
		"project":               parsed.ProjectID,
		"task":                  parsed.TaskID,
		"execution":             parsed.ExecutionID,
		"model":                 parsed.Model,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if len(parsed.Messages) == 0 {
		return errors.New("at least one -message is required")
	}
	if parsed.WorkspaceProbe != "" && !safeWorkspaceName(parsed.WorkspaceProbe) {
		return errors.New("workspace probe is not a safe workspace file name")
	}
	if parsed.WorkspaceMarker != "" && !safeWorkspaceName(parsed.WorkspaceMarker) {
		return errors.New("workspace marker is not a safe workspace file name")
	}
	return nil
}

// executeMode runs one mode, enriches the result with the mounted-credential and
// workspace evidence, writes the handoff marker, and applies the labelled
// handoff boundary only to a first (non-restoring) attempt.
func executeMode(ctx context.Context, parsed config, marker string, run func(context.Context, config) (output, error)) output {
	result, runErr := run(ctx, parsed)
	result.Mode = parsed.Mode
	result.ImageMarker = marker
	result.CredentialMounted = mountedNonEmpty(parsed.CredentialFile)
	result.CheckpointCredentialMounted = mountedNonEmpty(parsed.CheckpointCredentialFile)
	result.WorkspaceClean = workspaceClean(parsed.WorkspaceProbe)
	if runErr != nil {
		result.Status = "failed"
		if result.Error == "" {
			result.Error = runErr.Error()
		}
		return result
	}
	if result.Status == "" {
		result.Status = "succeeded"
	}
	if result.Status == "succeeded" && !result.Restored && parsed.WorkspaceMarker != "" {
		if err := writeWorkspaceMarker(parsed.WorkspaceMarker); err != nil {
			result.Status = "failed"
			result.Error = err.Error()
			return result
		}
	}
	if result.Status == "succeeded" && parsed.FailAfterRun && !result.Restored {
		result.Status = "failed"
		result.Error = "handoff boundary: checkpoint saved for the next execution"
	}
	return result
}

// emit writes the single JSON result.
func emit(writer io.Writer, value output) {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

// readMarker reads the fixture-unique marker baked into the image.
func readMarker(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the fixture marker: %w", err)
	}
	marker := strings.TrimSpace(string(data))
	if marker == "" {
		return "", errors.New("fixture marker is empty")
	}
	return marker, nil
}

// mountedNonEmpty reports whether one mounted file exists with a non-empty
// value. It never returns or logs the value.
func mountedNonEmpty(path string) bool {
	data, err := os.ReadFile(path)
	return err == nil && len(bytes.TrimSpace(data)) > 0
}

// workspaceClean reports whether a workspace-relative file is absent. It is the
// fresh-workspace probe: a marker written by the previous execution must not
// appear in the fresh execution's workspace.
func workspaceClean(name string) bool {
	if name == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(workspaceRoot, name))
	return errors.Is(err, os.ErrNotExist)
}

// writeWorkspaceMarker writes one bounded marker inside the workspace.
func writeWorkspaceMarker(name string) error {
	if !safeWorkspaceName(name) {
		return errors.New("workspace marker is not a safe workspace file name")
	}
	return os.WriteFile(filepath.Join(workspaceRoot, name), []byte("handoff\n"), 0o644)
}

// safeWorkspaceName refuses a path outside the workspace root.
func safeWorkspaceName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\`) && !strings.ContainsRune(name, '\x00')
}

// newTransport builds the public HTTP transport over the mounted credential and
// the fixture CA. The public edge is HTTPS outside loopback; the workload never
// disables verification.
func newTransport(baseURL, credentialFile, caFile string) (client.Transport, error) {
	retry := client.RetryPolicy{MaxAttempts: 1, BaseDelay: 10 * time.Millisecond, MaxDelay: 100 * time.Millisecond}
	if err := retry.Validate(); err != nil {
		return nil, err
	}
	roundTripper, err := fixtureRoundTripper(caFile)
	if err != nil {
		return nil, err
	}
	return &client.HTTPTransport{
		BaseURL:      baseURL,
		RoundTripper: roundTripper,
		Source:       client.FileCredentialSource{Path: credentialFile},
		Retry:        retry,
	}, nil
}

// fixtureRoundTripper builds a CA-verifying transport. The fixture CA is the
// only added trust root; no process-wide trust change and no proxy.
func fixtureRoundTripper(caFile string) (http.RoundTripper, error) {
	if caFile == "" {
		return nil, errors.New("fixture CA file is required")
	}
	material, err := os.ReadFile(caFile)
	if err != nil {
		return nil, errors.New("fixture CA file is unreadable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(material) {
		return nil, errors.New("fixture CA file is not a certificate pool")
	}
	return &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		},
	}, nil
}

// resolveRestore resolves the effective restore reference. An explicit flag
// wins; otherwise the admitted reply's checkpoint reference is used.
func resolveRestore(parsed config) (string, error) {
	if parsed.RestoreRef != "" {
		if !agent.ValidCheckpointRef(parsed.RestoreRef) {
			return "", fmt.Errorf("explicit restore reference %q is invalid", parsed.RestoreRef)
		}
		return parsed.RestoreRef, nil
	}
	if !parsed.RestoreFromConversation {
		return "", nil
	}
	body, err := lastConversationMessage(parsed.TaskFile)
	if err != nil {
		return "", err
	}
	ref := strings.TrimSpace(body)
	if ref == "" {
		return "", nil
	}
	if !agent.ValidCheckpointRef(ref) {
		return "", fmt.Errorf("admitted reply %q is not a durable checkpoint reference", ref)
	}
	return ref, nil
}

// restoredInput returns the new user message for a restoring run. The updated
// task conversation is the authority: its newest message is the reply that
// started this fresh execution. A run without a restore uses the authored
// messages instead.
func restoredInput(parsed config, restoring bool) ([]string, error) {
	if !restoring {
		return append([]string(nil), parsed.Messages...), nil
	}
	body, err := lastConversationMessage(parsed.TaskFile)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(body) == "" {
		return append([]string(nil), parsed.Messages...), nil
	}
	return []string{body}, nil
}

// lastConversationMessage returns the newest body of the admitted task
// conversation. An absent task file is the direct fixture run (no Shell
// workspace) and yields no message.
func lastConversationMessage(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read the task conversation: %w", err)
	}
	var messages []struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(data, &messages); err != nil {
		return "", fmt.Errorf("task conversation is not a message array: %w", err)
	}
	if len(messages) == 0 {
		return "", nil
	}
	return messages[len(messages)-1].Body, nil
}

// nativeRun executes agentapp.Run with the default PublicModel and the existing
// checkpoint port over the external storage adapter.
func runNative(ctx context.Context, parsed config) (output, error) {
	result := output{Mode: "native"}
	transport, err := newTransport(parsed.BaseURL, parsed.CredentialFile, parsed.CAFile)
	if err != nil {
		return result, err
	}
	storage, err := newCheckpointStorage(parsed.CheckpointURL, parsed.CheckpointCredentialFile)
	if err != nil {
		return result, err
	}
	checkpoints, err := agentapp.NewCheckpointPort(storage, agentapp.SystemClock{}, session.RetentionPolicy{
		RawContent: true,
		Retention:  session.DefaultRawContentRetention,
	})
	if err != nil {
		return result, err
	}
	restoreRef, err := resolveRestore(parsed)
	if err != nil {
		return result, err
	}
	result.Restored = restoreRef != ""
	result.RestoredRef = restoreRef
	messages, err := restoredInput(parsed, result.Restored)
	if err != nil {
		return result, err
	}
	publicTools, err := agentapp.NewPublicTools(transport, parsed.ProjectID)
	if err != nil {
		return result, err
	}
	tools := &fixtureTools{fallback: publicTools}
	run := agent.RunInput{
		TaskID:      parsed.TaskID,
		ExecutionID: parsed.ExecutionID,
		Model:       parsed.Model,
		Tools:       append([]string(nil), parsed.Tools...),
		RestoreRef:  restoreRef,
	}
	for _, message := range messages {
		run.Messages = append(run.Messages, agent.Message{Role: "user", Content: message})
	}
	var encoded bytes.Buffer
	runErr := agentapp.Run(ctx, agentapp.Config{
		Run:         run,
		ProjectID:   parsed.ProjectID,
		Transport:   transport,
		Tools:       tools,
		Checkpoints: checkpoints,
		RetainRaw:   true,
		Out:         &encoded,
	})
	var outcome agent.Outcome
	if err := json.Unmarshal(encoded.Bytes(), &outcome); err == nil {
		result.Status = string(outcome.Status)
		result.CheckpointRef = outcome.CheckpointRef
		result.Content = outcome.Summary
	}
	result.ToolCalls = tools.invocations()
	if runErr != nil {
		result.Error = errorDetail(runErr)
		return result, runErr
	}
	if result.Status == "" {
		result.Status = "succeeded"
	}
	if result.Status != string(agent.OutcomeSucceeded) {
		return result, fmt.Errorf("agent run ended %s: %s", result.Status, outcome.Summary)
	}
	return result, nil
}

// fixtureTools is the workload's local tool port. workspace.write is the
// fixture-local effect that gives the transcript a correlated tool call and
// result; every other name falls through to the served public tool edge.
type fixtureTools struct {
	fallback agent.ToolPort
	count    int
}

// Invoke executes one tool call and counts only real local invocations.
func (tools *fixtureTools) Invoke(ctx context.Context, request agent.ToolRequest) (agent.ToolResult, error) {
	if request.Name != "workspace.write" {
		return tools.fallback.Invoke(ctx, request)
	}
	result, err := workspaceWrite(request)
	if err != nil {
		return agent.ToolResult{}, err
	}
	tools.count++
	return result, nil
}

// invocations returns the number of real local effects executed.
func (tools *fixtureTools) invocations() int { return tools.count }

// workspaceWrite writes one bounded file below /workspace.
func workspaceWrite(request agent.ToolRequest) (agent.ToolResult, error) {
	var arguments struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
		return agent.ToolResult{}, fmt.Errorf("workspace.write arguments are malformed: %w", err)
	}
	if !safeWorkspaceName(arguments.Path) {
		return agent.ToolResult{}, errors.New("workspace.write path is outside the workspace")
	}
	path := filepath.Join(workspaceRoot, arguments.Path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return agent.ToolResult{}, fmt.Errorf("workspace.write cannot create its directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(arguments.Content), 0o644); err != nil {
		return agent.ToolResult{}, fmt.Errorf("workspace.write failed: %w", err)
	}
	return agent.ToolResult{
		Name:    request.Name,
		Content: fmt.Sprintf("wrote %d bytes to %s", len(arguments.Content), path),
	}, nil
}

// runExternal drives the public model.invoke contract directly and keeps its own
// transcript through the external storage adapter.
func runExternal(ctx context.Context, parsed config) (output, error) {
	result := output{Mode: "external"}
	transport, err := newTransport(parsed.BaseURL, parsed.CredentialFile, parsed.CAFile)
	if err != nil {
		return result, err
	}
	adapter, err := newCheckpointStorage(parsed.CheckpointURL, parsed.CheckpointCredentialFile)
	if err != nil {
		return result, err
	}
	store, err := session.NewStore(session.StoreConfig{Storage: adapter, Clock: systemClock{}})
	if err != nil {
		return result, err
	}
	restoreRef, err := resolveRestore(parsed)
	if err != nil {
		return result, err
	}
	result.Restored = restoreRef != ""
	result.RestoredRef = restoreRef

	var restored []session.Message
	if restoreRef != "" {
		record, err := store.Restore(ctx, session.Ref(restoreRef))
		if err != nil {
			result.Error = errorDetail(err)
			return result, err
		}
		// The store withholds raw content past its retention window; an empty
		// transcript is not usable context, so the external driver refuses it
		// exactly like the product checkpoint port does.
		if record.Retention.RawContent && !record.Retention.RetainRaw(time.Now().UTC(), record.CreatedAt, false) {
			result.Error = "checkpoint transcript has expired"
			return result, errors.New(result.Error)
		}
		restored = record.Messages
	}
	result.RestoredMessages = len(restored)

	messages := make(api.Messages, 0, len(restored)+1)
	for _, message := range restored {
		mapped := api.ChatMessage{Role: message.Role, Content: message.Content, ToolCallID: string(message.ToolCallID)}
		for _, call := range message.ToolCalls {
			mapped.ToolCalls = append(mapped.ToolCalls, api.ChatToolCall{
				ID:   call.ID,
				Type: "function",
				Function: api.ChatToolCallFunction{
					Name:      call.Name,
					Arguments: call.Arguments,
				},
			})
		}
		messages = append(messages, mapped)
	}
	newMessages, err := restoredInput(parsed, result.Restored)
	if err != nil {
		return result, err
	}
	for _, message := range newMessages {
		messages = append(messages, api.ChatMessage{Role: "user", Content: message})
	}
	tools, err := externalTools(parsed.Tools)
	if err != nil {
		return result, err
	}
	usage := api.ChatUsage{}
	content := ""
	for range externalModelSteps {
		request := api.ModelInvokeRequest{
			ProjectID: parsed.ProjectID,
			Model:     parsed.Model,
			Messages:  messages,
			MaxTokens: 64,
			Tools:     tools,
		}
		var completion api.ChatCompletionResponse
		if err := invokeModel(ctx, transport, request, &completion); err != nil {
			return result, err
		}
		usage.PromptTokens += completion.Usage.PromptTokens
		usage.CompletionTokens += completion.Usage.CompletionTokens
		usage.TotalTokens += completion.Usage.TotalTokens
		if len(completion.Choices) != 1 {
			return result, errors.New("model completion has no single choice")
		}
		choice := completion.Choices[0]
		if len(choice.Message.ToolCalls) == 0 {
			if strings.TrimSpace(choice.Message.Content) == "" {
				return result, errors.New("model returned no final content")
			}
			messages = append(messages, api.ChatMessage{Role: "assistant", Content: choice.Message.Content})
			content = choice.Message.Content
			break
		}
		calls := append([]api.ChatToolCall(nil), choice.Message.ToolCalls...)
		messages = append(messages, api.ChatMessage{Role: "assistant", ContentNull: true, ToolCalls: calls})
		for _, call := range calls {
			native, err := agent.NativeToolName(call.Function.Name)
			if err != nil {
				return result, fmt.Errorf("model tool call name is invalid: %w", err)
			}
			if !containsString(parsed.Tools, native) {
				return result, fmt.Errorf("model requested a tool outside the allowlist: %q", native)
			}
			if completedInTranscript(messages, native, call.Function.Arguments) {
				messages = append(messages, api.ChatMessage{
					Role:       "tool",
					ToolCallID: call.ID,
					Content:    "effect " + externalEffectIdentity(native, call.Function.Arguments) + " was already completed",
				})
				continue
			}
			invoked, err := workspaceWrite(agent.ToolRequest{Name: native, Arguments: json.RawMessage(call.Function.Arguments)})
			if err != nil {
				return result, err
			}
			result.ToolCalls++
			messages = append(messages, api.ChatMessage{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    invoked.Content,
			})
		}
	}
	if content == "" {
		return result, errors.New("external agent did not reach a final answer")
	}
	result.Content = content

	recorded := make([]session.Message, 0, len(messages))
	for _, message := range messages {
		recorded = append(recorded, session.Message{
			Role:       message.Role,
			Content:    message.Content,
			ToolCallID: session.ToolCallID(message.ToolCallID),
		})
		for _, call := range message.ToolCalls {
			recorded[len(recorded)-1].ToolCalls = append(recorded[len(recorded)-1].ToolCalls, session.ToolCall{
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}
	}
	record := session.Record{
		Version:     session.RefVersion,
		TaskID:      parsed.TaskID,
		ExecutionID: parsed.ExecutionID,
		SessionID:   checkpointSessionID(parsed.TaskID, parsed.ExecutionID),
		Messages:    recorded,
		Retention:   session.RetentionPolicy{RawContent: true, Retention: session.DefaultRawContentRetention},
		Usage: []session.Usage{{
			PrincipalID: "external-agent",
			Model:       parsed.Model,
			Prompt:      usage.PromptTokens,
			Completion:  usage.CompletionTokens,
		}},
		Outcome:   session.Outcome{Status: "checkpoint"},
		CreatedAt: time.Now().UTC(),
	}
	ref, err := store.Save(ctx, record)
	if err != nil {
		return result, err
	}
	result.Status = "succeeded"
	result.CheckpointRef = string(ref)
	return result, nil
}

// externalTools renders the provider declarations for the allowlist.
func externalTools(names []string) ([]api.ChatTool, error) {
	tools := make([]api.ChatTool, 0, len(names))
	for _, name := range names {
		provider, err := agent.ProviderToolName(name)
		if err != nil {
			return nil, err
		}
		tools = append(tools, api.ChatTool{
			Type: "function",
			Function: api.ChatToolFunction{
				Name:        provider,
				Description: "fixture-local workspace write",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
			},
		})
	}
	return tools, nil
}

// externalEffectIdentity mirrors the native runtime identity so the already
// completed message names the same effect.
func externalEffectIdentity(name, arguments string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + arguments))
	return "effect-" + hex.EncodeToString(sum[:16])
}

// canonicalArguments normalizes one JSON-object argument string so an effect
// ledger compares the call's meaning, not a map's serialization order. The
// durable session store re-encodes retained arguments, so a byte comparison
// would replay an effect whose object is unchanged.
func canonicalArguments(value string) string {
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return value
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return value
	}
	return string(encoded)
}

// completedInTranscript reports whether the transcript already carries a tool
// result for an identical call. It is the external driver's durable effect
// ledger: the restored transcript is the only memory that crosses the handoff.
func completedInTranscript(messages api.Messages, name, arguments string) bool {
	for index, message := range messages {
		if message.Role != "assistant" {
			continue
		}
		for _, call := range message.ToolCalls {
			native, err := agent.NativeToolName(call.Function.Name)
			if err != nil || native != name || canonicalArguments(call.Function.Arguments) != canonicalArguments(arguments) {
				continue
			}
			for later := index + 1; later < len(messages); later++ {
				if messages[later].Role == "tool" && messages[later].ToolCallID == call.ID {
					return true
				}
			}
		}
	}
	return false
}

// invokeModel performs one public model invocation and decodes its completion.
func invokeModel(ctx context.Context, transport client.Transport, request api.ModelInvokeRequest, target *api.ChatCompletionResponse) error {
	input, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode model request: %w", err)
	}
	result, err := transport.Invoke(ctx, api.Invocation{
		Operation: "model.invoke",
		Request:   api.Request{Input: input},
	})
	if err != nil {
		return err
	}
	if result.Operation != "model.invoke" {
		return fmt.Errorf("public result names operation %q, not model.invoke", result.Operation)
	}
	if err := json.Unmarshal(result.Body, target); err != nil {
		return fmt.Errorf("decode model result: %w", err)
	}
	return target.Validate()
}

// errorDetail renders the safe message chain of one failure. It collects only
// public fault messages and typed session reasons, never a raw cause, so a
// diagnostic cannot leak credential or content bytes.
func errorDetail(err error) string {
	var parts []string
	for current := err; current != nil; current = errors.Unwrap(current) {
		switch value := current.(type) {
		case *fault.Error:
			message := value.Message()
			if message != "" && !containsString(parts, message) {
				parts = append(parts, message)
			}
		case session.RestoreError:
			part := "reason=" + string(value.Reason)
			if !containsString(parts, part) {
				parts = append(parts, part)
			}
		}
	}
	if len(parts) == 0 {
		return err.Error()
	}
	return strings.Join(parts, ": ")
}

// containsString reports whether the list contains the value.
func containsString(list []string, value string) bool {
	return slices.Contains(list, value)
}

// checkpointSessionID derives the stable per-attempt session identity.
func checkpointSessionID(taskID, executionID string) session.SessionID {
	sum := sha256.Sum256([]byte(taskID + "\x00" + executionID))
	return session.SessionID("sess-" + hex.EncodeToString(sum[:]))
}

// systemClock is the real clock boundary.
type systemClock struct{}

// Now returns the current time.
func (systemClock) Now() time.Time { return time.Now() }

// checkpointStorage implements session.Storage over the external HTTP service.
type checkpointStorage struct {
	baseURL string
	token   string
	client  *http.Client
}

var _ session.Storage = (*checkpointStorage)(nil)

// newCheckpointStorage reads the mounted service token and binds the endpoint.
func newCheckpointStorage(baseURL, tokenFile string) (*checkpointStorage, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if endpoint == "" {
		return nil, errors.New("checkpoint service URL is empty")
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, errors.New("checkpoint service token is unreadable")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, errors.New("checkpoint service token is empty")
	}
	return &checkpointStorage{
		baseURL: endpoint,
		token:   token,
		client:  &http.Client{Timeout: checkpointRequestTimeout},
	}, nil
}

// saveRequest is the save envelope.
type saveRequest struct {
	ExpectedRevision uint64         `json:"expected_revision"`
	Record           session.Record `json:"record"`
}

// saveResponse is one committed reference.
type saveResponse struct {
	Ref string `json:"ref"`
}

// readRequest is the read envelope.
type readRequest struct {
	Ref string `json:"ref"`
}

// readResponse is one immutable record.
type readResponse struct {
	Record session.Record `json:"record"`
}

// errorResponse is one typed refusal.
type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// CompareAndSwap commits one record through the external service.
func (storage *checkpointStorage) CompareAndSwap(ctx context.Context, record session.Record, expectedRevision uint64) (session.Ref, error) {
	encoded, err := json.Marshal(saveRequest{ExpectedRevision: expectedRevision, Record: record})
	if err != nil {
		return "", fmt.Errorf("encode checkpoint save: %w", err)
	}
	var response saveResponse
	if err := storage.post(ctx, checkpointSavePath, encoded, &response); err != nil {
		return "", err
	}
	ref, err := session.ParseRef(response.Ref)
	if err != nil {
		return "", fmt.Errorf("external checkpoint service returned an invalid reference: %w", err)
	}
	return ref, nil
}

// Read returns one immutable record through the external service. An absent
// reference wraps session.ErrCheckpointMissing so the store reports the typed
// missing reason instead of a dependency failure.
func (storage *checkpointStorage) Read(ctx context.Context, ref session.Ref) (session.Record, error) {
	encoded, err := json.Marshal(readRequest{Ref: string(ref)})
	if err != nil {
		return session.Record{}, fmt.Errorf("encode checkpoint read: %w", err)
	}
	var response readResponse
	if err := storage.post(ctx, checkpointReadPath, encoded, &response); err != nil {
		if errors.Is(err, errExternalCheckpointMissing) {
			return session.Record{}, fmt.Errorf("checkpoint %q: %w", ref, session.ErrCheckpointMissing)
		}
		return session.Record{}, err
	}
	return response.Record, nil
}

// errExternalCheckpointMissing marks a typed absent reference.
var errExternalCheckpointMissing = errors.New("external checkpoint missing")

// post performs one authenticated service request.
func (storage *checkpointStorage) post(ctx context.Context, path string, body []byte, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, storage.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build checkpoint request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+storage.token)
	response, err := storage.client.Do(request)
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "external checkpoint service is unreachable", err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "external checkpoint response is unreadable", err)
	}
	if response.StatusCode != http.StatusOK {
		var refusal errorResponse
		_ = json.Unmarshal(data, &refusal)
		switch refusal.Code {
		case checkpointCodeMissing:
			return fmt.Errorf("%w: %s", errExternalCheckpointMissing, refusal.Message)
		case checkpointCodeConflict:
			return fault.New(fault.VersionConflict, "external checkpoint revision is stale")
		case checkpointCodeTooLarge, checkpointCodeStoreFull:
			return fault.New(fault.InvalidInput, "external checkpoint service refused the record: "+refusal.Code)
		case checkpointCodeUnauthorized:
			return fault.New(fault.Unauthenticated, "external checkpoint service refused the token")
		case checkpointCodeUnavailable:
			return fault.New(fault.DependencyUnavailable, "external checkpoint storage is closed")
		case checkpointCodeInternal:
			return fault.New(fault.InternalFailure, "external checkpoint service failed: "+refusal.Message)
		default:
			return fault.New(fault.DependencyUnavailable, "external checkpoint service failed")
		}
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fault.Wrap(fault.InternalFailure, "external checkpoint response is malformed", err)
	}
	return nil
}
