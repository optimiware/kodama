package publiccontract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/agent"
	"github.com/optimiware/kodama/internal/userland/session"
	"github.com/optimiware/kodama/internal/userland/skill"
)

// TestCheckpointReferenceFormat pins the versioned durable checkpoint reference
// and the restoration error vocabulary. A checkpoint is an external reference,
// never a process-local session ID or a worktree path.
func TestCheckpointReferenceFormat(t *testing.T) {
	if session.RefPrefix != "kodama-checkpoint/" || session.RefVersion != "v1" {
		t.Fatalf("checkpoint prefix/version = %q/%q; want kodama-checkpoint//v1", session.RefPrefix, session.RefVersion)
	}
	if session.MaxRefBytes != 512 {
		t.Fatalf("MaxRefBytes = %d; want 512", session.MaxRefBytes)
	}
	ref, err := session.ParseRef("kodama-checkpoint/v1/ctx-0001")
	if err != nil {
		t.Fatalf("ParseRef refused a valid reference: %v", err)
	}
	if ref.Version() != session.RefVersion {
		t.Fatalf("Ref.Version() = %q; want %q", ref.Version(), session.RefVersion)
	}
	refusals := []string{
		"",
		"ctx-0001",
		"kodama-checkpoint/v2/ctx-0001",
		"kodama-checkpoint/v1/",
		"kodama-checkpoint/v1/ctx/0001",
		"kodama-checkpoint/v1/CTX-0001",
		"kodama-checkpoint/v1/..",
		"checkpoint/v1/ctx-0001",
		"kodama-checkpoint/v1/" + strings.Repeat("a", session.MaxRefBytes),
	}
	for _, value := range refusals {
		if parsed, err := session.ParseRef(value); err == nil {
			t.Errorf("ParseRef(%q) = %q; want refusal", value, parsed)
		}
	}

	for _, reason := range []session.Reason{
		session.ReasonMissing,
		session.ReasonVersionMismatch,
		session.ReasonCorrupt,
		session.ReasonContextMissing,
	} {
		if !reason.Valid() {
			t.Errorf("Reason(%q).Valid() = false", reason)
		}
		restoreError := session.RestoreError{Reason: reason, Ref: ref}
		if restoreError.Error() == "" {
			t.Errorf("RestoreError(%q).Error() is empty", reason)
		}
		var target session.RestoreError
		if !errors.As(error(restoreError), &target) || target.Reason != reason {
			t.Errorf("errors.As lost the restore reason %q", reason)
		}
	}
	if session.Reason("deleted").Valid() {
		t.Error("Reason(deleted).Valid() = true")
	}

	record := session.Record{
		Version:     session.RefVersion,
		TaskID:      "tsk-1",
		ExecutionID: "exec-1",
		Messages: []session.Message{
			{Role: "user", Content: "please continue"},
			{Role: "assistant", Content: "continuing"},
		},
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("valid session record refused: %v", err)
	}
	for _, invalid := range []session.Record{
		{Version: "v2", TaskID: "tsk-1", ExecutionID: "exec-1", Messages: record.Messages, CreatedAt: record.CreatedAt},
		{Version: session.RefVersion, TaskID: "", ExecutionID: "exec-1", Messages: record.Messages, CreatedAt: record.CreatedAt},
		{Version: session.RefVersion, TaskID: "tsk/1", ExecutionID: "exec-1", Messages: record.Messages, CreatedAt: record.CreatedAt},
		{Version: session.RefVersion, TaskID: "tsk-1", ExecutionID: "exec-1", CreatedAt: record.CreatedAt},
		{Version: session.RefVersion, TaskID: "tsk-1", ExecutionID: "exec-1", Messages: []session.Message{{Role: "wizard", Content: "x"}}, CreatedAt: record.CreatedAt},
		{Version: session.RefVersion, TaskID: "tsk-1", ExecutionID: "exec-1", Messages: record.Messages},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("Record.Validate accepted %+v", invalid)
		}
	}
	assertNoFieldMatching(t, reflect.TypeOf(session.Record{}), "credential", "token", "password", "secret")
}

// fakeSessionStore is a deterministic in-memory checkpoint store.
type fakeSessionStore struct {
	records map[session.Ref]session.Record
	next    int
}

func (store *fakeSessionStore) Save(_ context.Context, record session.Record) (session.Ref, error) {
	store.next++
	ref, err := session.ParseRef(fmt.Sprintf("kodama-checkpoint/v1/ctx-%d", store.next))
	if err != nil {
		return "", err
	}
	if store.records == nil {
		store.records = make(map[session.Ref]session.Record)
	}
	store.records[ref] = record
	return ref, nil
}

func (store *fakeSessionStore) Restore(_ context.Context, ref session.Ref) (session.Record, error) {
	record, ok := store.records[ref]
	if !ok {
		return session.Record{}, session.RestoreError{Reason: session.ReasonMissing, Ref: ref}
	}
	return record, nil
}

var _ session.Store = (*fakeSessionStore)(nil)

// TestSessionStoreRestoration proves the checkpoint port composes without a
// live agent process and that a missing context is a typed restoration error,
// not an empty success.
func TestSessionStoreRestoration(t *testing.T) {
	store := &fakeSessionStore{}
	record := session.Record{
		Version:     session.RefVersion,
		TaskID:      "tsk-1",
		ExecutionID: "exec-1",
		Messages:    []session.Message{{Role: "user", Content: "continue"}},
		CreatedAt:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	ref, err := store.Save(context.Background(), record)
	if err != nil {
		t.Fatalf("fake Save: %v", err)
	}
	restored, err := store.Restore(context.Background(), ref)
	if err != nil {
		t.Fatalf("fake Restore: %v", err)
	}
	if !reflect.DeepEqual(restored, record) {
		t.Fatalf("restored record = %+v; want %+v", restored, record)
	}
	missing, err := session.ParseRef("kodama-checkpoint/v1/ctx-missing")
	if err != nil {
		t.Fatalf("ParseRef(missing): %v", err)
	}
	if _, err := store.Restore(context.Background(), missing); err == nil {
		t.Fatal("Restore accepted a missing checkpoint")
	} else {
		var restoreError session.RestoreError
		if !errors.As(err, &restoreError) || restoreError.Reason != session.ReasonMissing {
			t.Fatalf("missing checkpoint error = %v; want ReasonMissing", err)
		}
	}
}

// TestSkillContract pins Markdown skill names, bodies and the loader port.
func TestSkillContract(t *testing.T) {
	name, err := skill.ParseName("code-review")
	if err != nil {
		t.Fatalf("ParseName refused a valid name: %v", err)
	}
	for _, invalid := range []string{"", "Code-Review", "-leading", "trailing-", "a--b", "a/b", "..", strings.Repeat("a", 65)} {
		if parsed, err := skill.ParseName(invalid); err == nil {
			t.Errorf("ParseName(%q) = %q; want refusal", invalid, parsed)
		}
	}
	valid := skill.Skill{Name: name, Path: "context/skills/code-review.md", Body: []byte("# Review\n")}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid skill refused: %v", err)
	}
	for _, invalid := range []skill.Skill{
		{Name: "", Path: "context/skills/x.md", Body: []byte("x")},
		{Name: name, Path: "/abs/x.md", Body: []byte("x")},
		{Name: name, Path: "../x.md", Body: []byte("x")},
		{Name: name, Path: "context/skills/x.txt", Body: []byte("x")},
		{Name: name, Path: "context/skills/x.md"},
		{Name: name, Path: "context/skills/x.md", Body: make([]byte, skill.MaxBodyBytes+1)},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("Skill.Validate accepted %+v", invalid)
		}
	}
	if skill.MaxBodyBytes != 1<<20 {
		t.Fatalf("MaxBodyBytes = %d; want 1 MiB", skill.MaxBodyBytes)
	}

	loader := &fakeSkillLoader{skills: map[skill.Name]skill.Skill{name: valid}}
	if loaded, err := loader.Load(context.Background(), name); err != nil || loaded.Name != name {
		t.Fatalf("fake Loader.Load = %+v,%v", loaded, err)
	}
	names, err := loader.List(context.Background())
	if err != nil || !reflect.DeepEqual(names, []skill.Name{name}) {
		t.Fatalf("fake Loader.List = %v,%v", names, err)
	}
}

type fakeSkillLoader struct {
	skills map[skill.Name]skill.Skill
}

func (loader *fakeSkillLoader) Load(_ context.Context, name skill.Name) (skill.Skill, error) {
	value, ok := loader.skills[name]
	if !ok {
		return skill.Skill{}, errors.New("skill not found")
	}
	return value, nil
}

func (loader *fakeSkillLoader) List(context.Context) ([]skill.Name, error) {
	names := make([]skill.Name, 0, len(loader.skills))
	for name := range loader.skills {
		names = append(names, name)
	}
	return names, nil
}

var _ skill.Loader = (*fakeSkillLoader)(nil)

// providerFixture is one deterministic model exchange.
type providerFixture struct {
	Provider  string `json:"provider"`
	Version   int    `json:"version"`
	Exchanges []struct {
		Request  agent.ModelRequest  `json:"request"`
		Response agent.ModelResponse `json:"response"`
	} `json:"exchanges"`
}

func loadProviderFixture(t *testing.T) providerFixture {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	data, err := os.ReadFile(filepath.Join(root, "test", "contracts", "public", "testdata", "agent", "provider.json"))
	if err != nil {
		t.Fatalf("read provider fixture: %v", err)
	}
	var fixture providerFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("unmarshal provider fixture: %v", err)
	}
	if fixture.Provider != "fixture" || fixture.Version != 1 || len(fixture.Exchanges) == 0 {
		t.Fatalf("provider fixture header = %q/%d/%d", fixture.Provider, fixture.Version, len(fixture.Exchanges))
	}
	return fixture
}

// scriptedModel replays deterministic provider fixtures; it never sleeps,
// randomizes or calls a network.
type scriptedModel struct {
	exchanges []providerFixtureExchange
	calls     int
}

type providerFixtureExchange struct {
	Request  agent.ModelRequest
	Response agent.ModelResponse
}

func (model *scriptedModel) Invoke(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	if model.calls >= len(model.exchanges) {
		return agent.ModelResponse{}, errors.New("fixture exhausted")
	}
	exchange := model.exchanges[model.calls]
	model.calls++
	if !reflect.DeepEqual(request, exchange.Request) {
		return agent.ModelResponse{}, errors.New("request does not match the deterministic fixture")
	}
	return exchange.Response, nil
}

var _ agent.ModelPort = (*scriptedModel)(nil)

// TestDeterministicProviderFixtures pins the native-agent model/tool contract
// and proves the provider fixtures replay deterministically with no clock,
// randomness or network.
func TestDeterministicProviderFixtures(t *testing.T) {
	fixture := loadProviderFixture(t)
	exchanges := make([]providerFixtureExchange, 0, len(fixture.Exchanges))
	for _, exchange := range fixture.Exchanges {
		if err := exchange.Request.Validate(); err != nil {
			t.Fatalf("fixture request refused: %v", err)
		}
		if err := exchange.Response.Validate(); err != nil {
			t.Fatalf("fixture response refused: %v", err)
		}
		exchanges = append(exchanges, providerFixtureExchange{Request: exchange.Request, Response: exchange.Response})
	}

	first := &scriptedModel{exchanges: exchanges}
	second := &scriptedModel{exchanges: exchanges}
	for index, exchange := range exchanges {
		gotFirst, err := first.Invoke(context.Background(), exchange.Request)
		if err != nil {
			t.Fatalf("run 1 exchange %d: %v", index, err)
		}
		gotSecond, err := second.Invoke(context.Background(), exchange.Request)
		if err != nil {
			t.Fatalf("run 2 exchange %d: %v", index, err)
		}
		if !reflect.DeepEqual(gotFirst, gotSecond) || !reflect.DeepEqual(gotFirst, exchange.Response) {
			t.Fatalf("exchange %d is not deterministic: %+v vs %+v", index, gotFirst, gotSecond)
		}
	}

	toolResult := agent.ToolResult{Name: "repo.read", Content: "README contents"}
	if toolResult.Name != "repo.read" || toolResult.IsError {
		t.Fatalf("tool result = %+v", toolResult)
	}
	if err := (agent.ToolRequest{Name: "repo.read", Arguments: json.RawMessage(`{"path":"README.md"}`)}).Validate(); err != nil {
		t.Fatalf("valid tool request refused: %v", err)
	}
	for _, invalid := range []agent.ToolRequest{
		{Name: ""},
		{Name: "repo/read"},
		{Name: "repo.read", Arguments: json.RawMessage(`[]`)},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("ToolRequest.Validate accepted %+v", invalid)
		}
	}

	for _, usage := range []agent.Usage{
		{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		{PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0},
	} {
		if err := usage.Validate(); err != nil {
			t.Errorf("Usage.Validate refused %+v: %v", usage, err)
		}
	}
	for _, usage := range []agent.Usage{
		{PromptTokens: -1, CompletionTokens: 2, TotalTokens: 1},
		{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 4},
	} {
		if err := usage.Validate(); err == nil {
			t.Errorf("Usage.Validate accepted %+v", usage)
		}
	}
	for _, invalid := range []agent.ModelRequest{
		{},
		{Model: "fixture-model"},
		{Model: "fixture-model", Messages: []agent.Message{{Role: "wizard", Content: "x"}}},
		{Model: "fixture-model", Messages: []agent.Message{{Role: "user", Content: "x"}}, MaxTokens: agent.MaxModelTokens + 1},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("ModelRequest.Validate accepted %+v", invalid)
		}
	}
	if agent.MaxModelTokens != 32768 || agent.MaxMessages != 128 {
		t.Fatalf("agent bounds = %d/%d; want 32768/128", agent.MaxModelTokens, agent.MaxMessages)
	}
}

// nativeAgentHarness is the compile-only composition of the native-agent ports:
// it uses only public/Userland contracts, never a Shell or Kernel package.
type nativeAgentHarness struct {
	Model    agent.ModelPort
	Tools    agent.ToolPort
	Sessions session.Store
	Skills   skill.Loader
	Clock    agent.Clock
}

type fakeToolPort struct{}

func (fakeToolPort) Invoke(_ context.Context, request agent.ToolRequest) (agent.ToolResult, error) {
	return agent.ToolResult{Name: request.Name, Content: "ok"}, nil
}

var _ agent.ToolPort = fakeToolPort{}

// TestNativeAgentPortsCompile proves the model/tool/session/skill ports compose
// with fakes and that the clock is an injected boundary.
func TestNativeAgentPortsCompile(t *testing.T) {
	harness := nativeAgentHarness{
		Model:    &scriptedModel{exchanges: []providerFixtureExchange{}},
		Tools:    fakeToolPort{},
		Sessions: &fakeSessionStore{},
		Skills:   &fakeSkillLoader{skills: map[skill.Name]skill.Skill{}},
		Clock:    fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
	}
	if harness.Clock.Now().IsZero() {
		t.Fatal("injected clock returned a zero time")
	}
	result, err := harness.Tools.Invoke(context.Background(), agent.ToolRequest{Name: "repo.read"})
	if err != nil || result.Name != "repo.read" {
		t.Fatalf("tool port = %+v,%v", result, err)
	}
	if _, err := harness.Model.Invoke(context.Background(), agent.ModelRequest{
		Model: "fixture-model", Messages: []agent.Message{{Role: "user", Content: "x"}},
	}); err == nil {
		t.Fatal("an exhausted deterministic fixture returned a response")
	}
}

// TestAgentToolDeclarationsMirror pins the provider-neutral mirrors of the
// public tool-calling vocabulary: declared tools, call identities, assistant
// calls and tool-message identities, with the same bounds as api. The
// injected-model fixtures keep their legacy shape without correlation.
func TestAgentToolDeclarationsMirror(t *testing.T) {
	if agent.MaxToolDeclarations != api.MaxToolDeclarations ||
		agent.MaxToolCalls != api.MaxToolCallsPerMessage ||
		agent.MaxToolSchemaBytes != api.MaxToolParametersBytes ||
		agent.MaxToolSchemasBytes != api.MaxToolSchemasBytes ||
		agent.MaxToolArgumentsBytes != api.MaxToolArgumentsBytes ||
		agent.MaxToolNameBytes != api.MaxToolNameBytes {
		t.Fatalf("agent tool bounds = %d/%d/%d/%d/%d/%d; want the api mirror %d/%d/%d/%d/%d/%d",
			agent.MaxToolDeclarations, agent.MaxToolCalls, agent.MaxToolSchemaBytes,
			agent.MaxToolSchemasBytes, agent.MaxToolArgumentsBytes, agent.MaxToolNameBytes,
			api.MaxToolDeclarations, api.MaxToolCallsPerMessage, api.MaxToolParametersBytes,
			api.MaxToolSchemasBytes, api.MaxToolArgumentsBytes, api.MaxToolNameBytes)
	}

	declaration := agent.ToolDeclaration{
		Name:        "repo_read",
		Description: "read one repository file",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}
	if err := declaration.Validate(); err != nil {
		t.Fatalf("valid tool declaration refused: %v", err)
	}
	for _, invalid := range []agent.ToolDeclaration{
		{Name: ""},
		{Name: "repo.read", Parameters: json.RawMessage(`{}`)},
		{Name: "repo_read"},
		{Name: "repo_read", Parameters: json.RawMessage(`[]`)},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("ToolDeclaration.Validate accepted %+v", invalid)
		}
	}

	call := agent.ToolCall{ID: "call-1", Name: "repo_read", Arguments: `{"path":"README.md"}`}
	if err := call.Validate(); err != nil {
		t.Fatalf("valid tool call refused: %v", err)
	}
	for _, invalid := range []agent.ToolCall{
		{ID: "", Name: "repo_read", Arguments: "{}"},
		{ID: "call/1", Name: "repo_read", Arguments: "{}"},
		{ID: "call-1", Name: "repo.read", Arguments: "{}"},
		{ID: "call-1", Name: "repo_read", Arguments: "[]"},
		{ID: "call-1", Name: "repo_read", Arguments: ""},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("ToolCall.Validate accepted %+v", invalid)
		}
	}

	// Assistant calls and tool-message identities are provider-neutral mirrors;
	// the legacy Message shape without correlation stays valid and comparable.
	callID, err := agent.ParseToolCallID("call-1")
	if err != nil || !callID.Valid() {
		t.Fatalf("ParseToolCallID(call-1) = %q, %v", callID, err)
	}
	for _, invalid := range []string{"", "call/1", strings.Repeat("a", 129)} {
		if parsed, err := agent.ParseToolCallID(invalid); err == nil {
			t.Errorf("ParseToolCallID(%q) = %q; want refusal", invalid, parsed)
		}
	}
	assistant := agent.AssistantMessage{ContentNull: true, Calls: []agent.ToolCall{call}}
	if err := assistant.Validate(); err != nil {
		t.Fatalf("valid assistant message refused: %v", err)
	}
	if err := (agent.AssistantMessage{Calls: make([]agent.ToolCall, agent.MaxToolCalls+1)}).Validate(); err == nil {
		t.Fatalf("%d assistant calls accepted; want refusal", agent.MaxToolCalls+1)
	}
	tool := agent.ToolMessage{CallID: callID, Content: "result"}
	if err := tool.Validate(); err != nil {
		t.Fatalf("valid tool message refused: %v", err)
	}
	if err := (agent.ToolMessage{Content: "result"}).Validate(); err == nil {
		t.Fatal("tool message without a call identity accepted")
	}
	if err := (agent.Message{Role: "user", Content: "x"}).Validate(); err != nil {
		t.Fatalf("legacy user message refused: %v", err)
	}
	if err := (agent.Message{Role: "wizard", Content: "x"}).Validate(); err == nil {
		t.Fatal("unknown role accepted")
	}

	// Declared tool and schema bounds mirror the API byte-based bounds.
	tools := make([]agent.ToolDeclaration, api.MaxToolDeclarations)
	for index := range tools {
		tools[index] = agent.ToolDeclaration{Name: "tool_" + fmt.Sprint(index), Parameters: json.RawMessage(`{}`)}
	}
	request := agent.ModelRequest{Model: "fixture-model", Messages: []agent.Message{{Role: "user", Content: "x"}}, Tools: tools}
	if err := request.Validate(); err != nil {
		t.Fatalf("%d tools refused: %v", api.MaxToolDeclarations, err)
	}
	request.Tools = append(request.Tools, agent.ToolDeclaration{Name: "extra", Parameters: json.RawMessage(`{}`)})
	if err := request.Validate(); err == nil {
		t.Fatalf("%d tools accepted; want refusal", api.MaxToolDeclarations+1)
	}

	padded := func(size int) json.RawMessage {
		return json.RawMessage(`{"p":"` + strings.Repeat("a", size-len(`{"p":""}`)) + `"}`)
	}
	atBound := agent.ModelRequest{
		Model:    "fixture-model",
		Messages: []agent.Message{{Role: "user", Content: "x"}},
		Tools:    []agent.ToolDeclaration{{Name: "repo_read", Parameters: padded(agent.MaxToolSchemaBytes)}},
	}
	if err := atBound.Validate(); err != nil {
		t.Fatalf("schema at the %d-byte bound refused: %v", agent.MaxToolSchemaBytes, err)
	}
	atBound.Tools[0].Parameters = padded(agent.MaxToolSchemaBytes + 1)
	if err := atBound.Validate(); err == nil {
		t.Fatalf("schema above %d bytes accepted", agent.MaxToolSchemaBytes)
	}
	aggregate := agent.ModelRequest{Model: "fixture-model", Messages: []agent.Message{{Role: "user", Content: "x"}}}
	for index := 0; index < 4; index++ {
		aggregate.Tools = append(aggregate.Tools, agent.ToolDeclaration{Name: "tool_" + fmt.Sprint(index), Parameters: padded(agent.MaxToolSchemaBytes)})
	}
	if err := aggregate.Validate(); err != nil {
		t.Fatalf("aggregate schema bytes at the %d-byte bound refused: %v", agent.MaxToolSchemasBytes, err)
	}
	aggregate.Tools = append(aggregate.Tools, agent.ToolDeclaration{Name: "extra", Parameters: json.RawMessage(`{}`)})
	if err := aggregate.Validate(); err == nil {
		t.Fatalf("aggregate schema bytes above %d accepted", agent.MaxToolSchemasBytes)
	}
}

// TestAgentMessageCorrelation pins the additive correlation members of the
// provider-neutral agent message: an assistant message carries its tool calls
// with their identities and a tool message answers one call by identity. An
// absent member stays valid so the legacy fixtures decode, while a
// present member obeys the api role rules, the 128-byte call-id grammar, the
// 32-call bound and the 64 KiB arguments bound.
func TestAgentMessageCorrelation(t *testing.T) {
	call := agent.ToolCall{ID: "call-1", Name: "repo_read", Arguments: `{"path":"README.md"}`}
	assistant := agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{call}}
	if err := assistant.Validate(); err != nil {
		t.Fatalf("assistant message with tool calls refused: %v", err)
	}
	tool := agent.Message{Role: "tool", Content: "README contents", ToolCallID: "call-1"}
	if err := tool.Validate(); err != nil {
		t.Fatalf("tool message with a call identity refused: %v", err)
	}

	// Legacy messages stay valid: absent correlation members are not a refusal.
	for _, legacy := range []agent.Message{
		{Role: "system", Content: "you are a test"},
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "plain"},
		{Role: "tool", Content: "legacy tool output"},
	} {
		if err := legacy.Validate(); err != nil {
			t.Errorf("legacy message %+v refused: %v", legacy, err)
		}
	}

	tooManyCalls := make([]agent.ToolCall, agent.MaxToolCalls+1)
	for index := range tooManyCalls {
		tooManyCalls[index] = agent.ToolCall{ID: fmt.Sprintf("call-%d", index), Name: "repo_read", Arguments: `{}`}
	}
	invalid := []agent.Message{
		{Role: "assistant", ToolCallID: "call-1"},
		{Role: "tool", ToolCalls: []agent.ToolCall{call}},
		{Role: "user", ToolCalls: []agent.ToolCall{call}},
		{Role: "system", ToolCallID: "call-1"},
		{Role: "tool", ToolCallID: "call/1"},
		{Role: "assistant", ToolCalls: []agent.ToolCall{call, call}},
		{Role: "assistant", ToolCalls: tooManyCalls},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "task.ask", Arguments: `{}`}}},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "repo_read", Arguments: `[]`}}},
		{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "call-1", Name: "repo_read", Arguments: emptyToolArguments(agent.MaxToolArgumentsBytes + 1)}}},
	}
	for _, message := range invalid {
		if err := message.Validate(); err == nil {
			t.Errorf("Message.Validate accepted %+v", message)
		}
	}

	request := agent.ModelRequest{
		Model: "fixture-model",
		Messages: []agent.Message{
			{Role: "user", Content: "go"},
			assistant,
			tool,
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("model request carrying correlation refused: %v", err)
	}
}

// emptyToolArguments returns a JSON object string of exactly size bytes.
func emptyToolArguments(size int) string {
	const prefix = `{"p":""`
	if size <= len(prefix) {
		return prefix
	}
	return prefix + strings.Repeat(" ", size-len(prefix)-1) + "}"
}

// TestAgentMessageStrictDecoding pins the compatibility rule: the four
// correlation members decode, an absent member decodes to its zero value, and
// an unknown member at any nesting level (or trailing data) is refused.
func TestAgentMessageStrictDecoding(t *testing.T) {
	var tool agent.Message
	if err := json.Unmarshal([]byte(`{"role":"tool","content":"done","tool_call_id":"call-1"}`), &tool); err != nil {
		t.Fatalf("tool message with a call identity refused: %v", err)
	}
	if tool.Role != "tool" || tool.Content != "done" || tool.ToolCallID != "call-1" || len(tool.ToolCalls) != 0 {
		t.Fatalf("decoded tool message = %+v", tool)
	}

	var legacy agent.Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hi"}`), &legacy); err != nil {
		t.Fatalf("legacy message refused: %v", err)
	}
	if legacy.Role != "user" || legacy.Content != "hi" || legacy.ToolCallID != "" || len(legacy.ToolCalls) != 0 {
		t.Fatalf("legacy message decoded as %+v", legacy)
	}

	var assistant agent.Message
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"","tool_calls":[{"id":"call-1","name":"repo_read","arguments":"{}"}]}`), &assistant); err != nil {
		t.Fatalf("assistant message with tool calls refused: %v", err)
	}
	if err := assistant.Validate(); err != nil {
		t.Fatalf("decoded assistant message is invalid: %v", err)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0] != (agent.ToolCall{ID: "call-1", Name: "repo_read", Arguments: "{}"}) {
		t.Fatalf("decoded assistant tool calls = %+v", assistant.ToolCalls)
	}

	for _, raw := range []string{
		`{"role":"user","content":"hi","refusal":null}`,
		`{"role":"assistant","content":"","tool_calls":[{"id":"call-1","name":"repo_read","arguments":"{}","type":"function"}]}`,
		`{"role":"user","content":"hi"} {"role":"user","content":"again"}`,
		`[]`,
	} {
		var message agent.Message
		if err := json.Unmarshal([]byte(raw), &message); err == nil {
			t.Errorf("strict decode accepted %s as %+v", raw, message)
		}
	}

	encoded, err := json.Marshal(assistant)
	if err != nil {
		t.Fatalf("marshal assistant message: %v", err)
	}
	var roundTripped agent.Message
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("round-trip decode: %v", err)
	}
	if !reflect.DeepEqual(roundTripped, assistant) {
		t.Fatalf("round trip = %+v; want %+v", roundTripped, assistant)
	}
}

// TestAgentResponseCallIdentities pins the provider call identity on a model
// response: it is optional for the legacy deterministic fixtures, but a
// present identity is a bounded ASCII call id and duplicates are refused, just
// like the api assistant-message rule.
func TestAgentResponseCallIdentities(t *testing.T) {
	valid := agent.ToolRequest{ID: "call-1", Name: "repo.read", Arguments: json.RawMessage(`{"path":"README.md"}`)}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid response call refused: %v", err)
	}
	if err := (agent.ToolRequest{ID: "call/1", Name: "repo.read", Arguments: json.RawMessage(`{}`)}).Validate(); err == nil {
		t.Fatal("malformed response call id accepted")
	}

	response := agent.ModelResponse{
		ToolCalls:    []agent.ToolRequest{valid, {ID: "call-2", Name: "repo.read", Arguments: json.RawMessage(`{}`)}},
		Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		FinishReason: "tool_calls",
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("response with distinct call ids refused: %v", err)
	}
	response.ToolCalls[1].ID = "call-1"
	if err := response.Validate(); err == nil {
		t.Fatal("duplicate response call ids accepted")
	}

	legacy := agent.ModelResponse{
		ToolCalls:    []agent.ToolRequest{{Name: "repo.read", Arguments: json.RawMessage(`{}`)}, {Name: "repo.write", Arguments: json.RawMessage(`{}`)}},
		Usage:        agent.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		FinishReason: "tool_calls",
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy response without call ids refused: %v", err)
	}

	tooMany := make([]agent.ToolRequest, agent.MaxToolCalls+1)
	for index := range tooMany {
		tooMany[index] = agent.ToolRequest{Name: "repo.read", Arguments: json.RawMessage(`{}`)}
	}
	if err := (agent.ModelResponse{ToolCalls: tooMany, FinishReason: "tool_calls"}).Validate(); err == nil {
		t.Fatalf("%d response tool calls accepted; want refusal", agent.MaxToolCalls+1)
	}
}

// TestProviderToolNameTranslation pins the collision-safe dotted-tool- name
// translation applied by the public model bridge. Native names are dotted;
// provider function names accept only [A-Za-z0-9_-]. The `kd_` prefix plus a
// prefix-free escape (`.` -> `_0`, `_` -> `_1`) is reversible for every valid
// native name and therefore collision-free, including `task.ask` versus
// `task_ask`, which a naive dot-to-underscore alias would merge.
func TestProviderToolNameTranslation(t *testing.T) {
	cases := map[string]string{
		"task.ask":       "kd_task_0ask",
		"task_ask":       "kd_task_1ask",
		"git.ref.update": "kd_git_0ref_0update",
		"a.b_c-d.9":      "kd_a_0b_1c-d_09",
		"task__dot__ask": "kd_task_1_1dot_1_1ask",
	}
	for native, want := range cases {
		encoded, err := agent.ProviderToolName(native)
		if err != nil {
			t.Fatalf("ProviderToolName(%q): %v", native, err)
		}
		if encoded != want {
			t.Errorf("ProviderToolName(%q) = %q; want %q", native, encoded, want)
		}
		decoded, err := agent.NativeToolName(encoded)
		if err != nil {
			t.Fatalf("NativeToolName(%q): %v", encoded, err)
		}
		if decoded != native {
			t.Errorf("NativeToolName(%q) = %q; want %q", encoded, decoded, native)
		}
	}

	// Every registry operation name must translate injectively and round-trip.
	seen := make(map[string]string)
	for _, route := range api.Routes() {
		encoded, err := agent.ProviderToolName(route.Operation)
		if err != nil {
			t.Fatalf("ProviderToolName(%q): %v", route.Operation, err)
		}
		if encoded == route.Operation || strings.Contains(encoded, ".") {
			t.Errorf("ProviderToolName(%q) = %q; want a dot-free provider name", route.Operation, encoded)
		}
		if len(encoded) > agent.MaxToolNameBytes {
			t.Errorf("ProviderToolName(%q) = %q is %d bytes; want at most %d", route.Operation, encoded, len(encoded), agent.MaxToolNameBytes)
		}
		declaration := agent.ToolDeclaration{Name: encoded, Parameters: json.RawMessage(`{}`)}
		if err := declaration.Validate(); err != nil {
			t.Errorf("ProviderToolName(%q) = %q is not a provider function name: %v", route.Operation, encoded, err)
		}
		if other, duplicate := seen[encoded]; duplicate {
			t.Errorf("ProviderToolName collision: %q and %q both encode to %q", other, route.Operation, encoded)
		}
		seen[encoded] = route.Operation
		decoded, err := agent.NativeToolName(encoded)
		if err != nil {
			t.Fatalf("NativeToolName(%q): %v", encoded, err)
		}
		if decoded != route.Operation {
			t.Errorf("NativeToolName(%q) = %q; want %q", encoded, decoded, route.Operation)
		}
	}
	if len(seen) != len(api.Routes()) {
		t.Fatalf("translated %d operations; want %d", len(seen), len(api.Routes()))
	}

	// The 64-byte provider bound decides how much native name fits.
	atBound := strings.Repeat("a", agent.MaxToolNameBytes-len(agent.ProviderToolNamePrefix))
	encodedBound, err := agent.ProviderToolName(atBound)
	if err != nil || len(encodedBound) != agent.MaxToolNameBytes {
		t.Fatalf("native name at the provider bound = %q,%v; want a %d-byte provider name", encodedBound, err, agent.MaxToolNameBytes)
	}
	if _, err := agent.ProviderToolName(atBound + "a"); err == nil {
		t.Fatal("native name whose provider form exceeds the bound accepted")
	}
	// NativeToolName enforces the same provider bound on its input: the
	// at-bound form decodes, one byte more is refused.
	providerAtBound := agent.ProviderToolNamePrefix + strings.Repeat("a", agent.MaxToolNameBytes-len(agent.ProviderToolNamePrefix))
	if decoded, err := agent.NativeToolName(providerAtBound); err != nil || decoded != atBound {
		t.Fatalf("NativeToolName(%q) = %q,%v; want %q", providerAtBound, decoded, err, atBound)
	}
	providerOverBound := providerAtBound + "a"
	if decoded, err := agent.NativeToolName(providerOverBound); err == nil {
		t.Fatalf("NativeToolName(%q) = %q; want refusal over the %d-byte provider bound", providerOverBound, decoded, agent.MaxToolNameBytes)
	}
	for _, native := range []string{"", "Task.ask", "task/ask", "task..ask", "task.ask.", "."} {
		if encoded, err := agent.ProviderToolName(native); err == nil {
			t.Errorf("ProviderToolName(%q) = %q; want refusal", native, encoded)
		}
	}
	for _, provider := range []string{"", "task_ask", "kd_", "kd_task_0", "kd_task_2", "kd_task.ask", "kd_Task", strings.Repeat("a", 65)} {
		if decoded, err := agent.NativeToolName(provider); err == nil {
			t.Errorf("NativeToolName(%q) = %q; want refusal", provider, decoded)
		}
	}
}

// TestAgentModelRequestDecodesToolCorrelation pins strict request decoding end
// to end: a correlated conversation decodes and validates, while an unknown
// member at any nesting level is refused.
func TestAgentModelRequestDecodesToolCorrelation(t *testing.T) {
	body := `{"model":"fixture-model","messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call-1","name":"task_ask","arguments":"{}"}]},` +
		`{"role":"tool","content":"ok","tool_call_id":"call-1"}]}`
	var request agent.ModelRequest
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("correlated model request refused: %v", err)
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("correlated model request is invalid: %v", err)
	}
	if len(request.Messages) != 3 || len(request.Messages[1].ToolCalls) != 1 || request.Messages[2].ToolCallID != "call-1" {
		t.Fatalf("decoded request messages = %+v", request.Messages)
	}
	if err := json.Unmarshal([]byte(`{"model":"fixture-model","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call-1","name":"task_ask","arguments":"{}","extra":true}]}]}`), &request); err == nil {
		t.Fatal("unknown nested tool-call member accepted")
	}
}
