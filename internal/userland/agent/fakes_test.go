package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/agent"
)

// eventLog records cross-port ordering without assuming goroutine timing.
type eventLog struct {
	mu      sync.Mutex
	entries []string
}

func (log *eventLog) add(entry string) {
	log.mu.Lock()
	log.entries = append(log.entries, entry)
	log.mu.Unlock()
}

func (log *eventLog) snapshot() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.entries...)
}

func (log *eventLog) index(entry string) int {
	for index, candidate := range log.snapshot() {
		if candidate == entry {
			return index
		}
	}
	return -1
}

type fakeClock struct {
	at time.Time
}

func (clock fakeClock) Now() time.Time { return clock.at }

type fakeModel struct {
	mu        sync.Mutex
	responses []agent.ModelResponse
	errAt     map[int]error
	respond   func(agent.ModelRequest) (agent.ModelResponse, error)
	calls     int
	requests  []agent.ModelRequest
}

func (model *fakeModel) Invoke(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
	model.mu.Lock()
	index := model.calls
	model.calls++
	model.requests = append(model.requests, request)
	respond := model.respond
	var response agent.ModelResponse
	if index < len(model.responses) {
		response = model.responses[index]
	}
	err := model.errAt[index]
	model.mu.Unlock()

	if err != nil {
		return agent.ModelResponse{}, err
	}
	if respond != nil {
		return respond(request)
	}
	if index >= len(model.responses) {
		return agent.ModelResponse{}, fmt.Errorf("fake model script exhausted at call %d", index)
	}
	return response, nil
}

func (model *fakeModel) callCount() int {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.calls
}

func (model *fakeModel) requestAt(index int) agent.ModelRequest {
	model.mu.Lock()
	defer model.mu.Unlock()
	if index < 0 || index >= len(model.requests) {
		return agent.ModelRequest{}
	}
	return model.requests[index]
}

type fakeTool struct {
	mu      sync.Mutex
	results map[string]agent.ToolResult
	errAt   map[int]error
	after   func(index int, request agent.ToolRequest)
	log     *eventLog
	calls   []agent.ToolRequest
}

func (tool *fakeTool) Invoke(_ context.Context, request agent.ToolRequest) (agent.ToolResult, error) {
	tool.mu.Lock()
	index := len(tool.calls)
	tool.calls = append(tool.calls, request)
	err := tool.errAt[index]
	result := tool.results[request.Name]
	after := tool.after
	tool.mu.Unlock()

	if tool.log != nil {
		tool.log.add("tool:" + request.Name)
	}
	if after != nil {
		after(index, request)
	}
	if err != nil {
		return agent.ToolResult{}, err
	}
	if result.Name == "" {
		result.Name = request.Name
	}
	return result, nil
}

func (tool *fakeTool) callCount() int {
	tool.mu.Lock()
	defer tool.mu.Unlock()
	return len(tool.calls)
}

func (tool *fakeTool) requests() []agent.ToolRequest {
	tool.mu.Lock()
	defer tool.mu.Unlock()
	return append([]agent.ToolRequest(nil), tool.calls...)
}

type fakeCheckpoints struct {
	mu           sync.Mutex
	store        map[string]agent.Checkpoint
	refs         []string
	saves        []agent.Checkpoint
	failAt       map[int]error
	restoreErr   error
	restoreCalls []string
	log          *eventLog
}

func newFakeCheckpoints() *fakeCheckpoints {
	return &fakeCheckpoints{store: make(map[string]agent.Checkpoint)}
}

func (store *fakeCheckpoints) Save(_ context.Context, checkpoint agent.Checkpoint) (string, error) {
	store.mu.Lock()
	index := len(store.saves)
	if err := store.failAt[index]; err != nil {
		store.mu.Unlock()
		return "", err
	}
	ref := fmt.Sprintf("kodama-checkpoint/v1/ctx-%d", index+1)
	store.saves = append(store.saves, copyCheckpoint(checkpoint))
	store.refs = append(store.refs, ref)
	store.store[ref] = copyCheckpoint(checkpoint)
	store.mu.Unlock()
	if store.log != nil {
		store.log.add("checkpoint.save")
	}
	return ref, nil
}

func (store *fakeCheckpoints) Restore(_ context.Context, ref string) (agent.Checkpoint, error) {
	store.mu.Lock()
	store.restoreCalls = append(store.restoreCalls, ref)
	err := store.restoreErr
	checkpoint, ok := store.store[ref]
	store.mu.Unlock()
	if err != nil {
		return agent.Checkpoint{}, err
	}
	if !ok {
		return agent.Checkpoint{}, fmt.Errorf("checkpoint %q is missing", ref)
	}
	return copyCheckpoint(checkpoint), nil
}

func (store *fakeCheckpoints) saveCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.saves)
}

func (store *fakeCheckpoints) refAt(index int) string {
	store.mu.Lock()
	defer store.mu.Unlock()
	if index < 0 || index >= len(store.refs) {
		return ""
	}
	return store.refs[index]
}

func (store *fakeCheckpoints) checkpointAt(index int) agent.Checkpoint {
	store.mu.Lock()
	defer store.mu.Unlock()
	if index < 0 || index >= len(store.saves) {
		return agent.Checkpoint{}
	}
	return copyCheckpoint(store.saves[index])
}

func (store *fakeCheckpoints) restoreCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.restoreCalls)
}

func copyCheckpoint(checkpoint agent.Checkpoint) agent.Checkpoint {
	copied := checkpoint
	copied.Messages = append([]agent.Message(nil), checkpoint.Messages...)
	copied.Completed = append([]string(nil), checkpoint.Completed...)
	copied.InFlight = append([]string(nil), checkpoint.InFlight...)
	copied.Uncertain = append([]string(nil), checkpoint.Uncertain...)
	return copied
}

type fakeKnowledge struct {
	bodies map[string][]byte
	errs   map[string]error
	calls  []string
	mu     sync.Mutex
}

func (knowledge *fakeKnowledge) Load(_ context.Context, path string) ([]byte, error) {
	knowledge.mu.Lock()
	knowledge.calls = append(knowledge.calls, path)
	err := knowledge.errs[path]
	body := knowledge.bodies[path]
	knowledge.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), body...), nil
}

type fakePublic struct {
	mu     sync.Mutex
	result api.Result
	err    error
	calls  []api.Invocation
	log    *eventLog
}

func (public *fakePublic) Invoke(_ context.Context, invocation api.Invocation) (api.Result, error) {
	public.mu.Lock()
	public.calls = append(public.calls, invocation)
	err := public.err
	result := public.result
	public.mu.Unlock()
	if public.log != nil {
		public.log.add("public:" + invocation.Operation)
	}
	if err != nil {
		return api.Result{}, err
	}
	if result.Operation == "" {
		result.Operation = invocation.Operation
	}
	return result, nil
}

func (public *fakePublic) callCount() int {
	public.mu.Lock()
	defer public.mu.Unlock()
	return len(public.calls)
}

func (public *fakePublic) invocationAt(index int) api.Invocation {
	public.mu.Lock()
	defer public.mu.Unlock()
	if index < 0 || index >= len(public.calls) {
		return api.Invocation{}
	}
	return public.calls[index]
}

func newTestRuntime(t *testing.T, config agent.RuntimeConfig) *agent.Runtime {
	t.Helper()
	runtime, err := agent.NewRuntime(config)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return runtime
}

func checkpointJSON(t *testing.T, checkpoint agent.Checkpoint) string {
	t.Helper()
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatalf("marshal checkpoint: %v", err)
	}
	return string(payload)
}

func requireNoSecret(t *testing.T, label, text, canary string) {
	t.Helper()
	if strings.Contains(text, canary) {
		t.Fatalf("%s leaked the secret canary: %s", label, text)
	}
}
