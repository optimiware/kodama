package gateway_test

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/gateway"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

type fakeBudgetReader struct {
	mu         sync.Mutex
	store      *fakeStore
	context    map[id.ExecutionID]gateway.ExecutionBudgetContext
	actionCtx  map[string]gateway.ExecutionBudgetContext
	taskExecs  map[id.TaskID][]id.ExecutionID
	onReadTask func(taskID id.TaskID)
}

var _ gateway.BudgetReader = (*fakeBudgetReader)(nil)

func (r *fakeBudgetReader) ReadExecutionBudget(_ context.Context, _ tenancy.Scope, execID id.ExecutionID, actionID id.ActionID) (gateway.ExecutionBudgetContext, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if actionID != "" && r.actionCtx != nil {
		if ctx, ok := r.actionCtx[string(execID)+"/"+string(actionID)]; ok {
			return ctx, nil
		}
	}
	if ctx, ok := r.context[execID]; ok {
		return ctx, nil
	}
	return gateway.ExecutionBudgetContext{
		ExecutionID:       execID,
		SiblingExecutions: []id.ExecutionID{execID},
	}, nil
}

func (r *fakeBudgetReader) ReadTaskExecutions(_ context.Context, _ tenancy.Scope, taskID id.TaskID) ([]id.ExecutionID, error) {
	r.mu.Lock()
	hook := r.onReadTask
	siblings := append([]id.ExecutionID(nil), r.taskExecs[taskID]...)
	r.mu.Unlock()
	if hook != nil {
		hook(taskID)
	}
	return siblings, nil
}

func (r *fakeBudgetReader) ReadUsageTotals(_ context.Context, scope tenancy.Scope, execIDs []id.ExecutionID) ([]gateway.UsageSummaryRow, error) {
	wanted := make(map[id.ExecutionID]bool, len(execIDs))
	for _, eid := range execIDs {
		wanted[eid] = true
	}
	var rows []gateway.UsageSummaryRow
	for _, commit := range r.store.callList() {
		if commit.scope != scope {
			continue
		}
		for _, write := range commit.unit.Writes {
			if write.Table != gateway.UsageTable {
				continue
			}
			rawExec, _ := write.Values["execution_id"].(string)
			eid := id.ExecutionID(rawExec)
			if !wanted[eid] {
				continue
			}
			rawAct, _ := write.Values["action_id"].(string)
			model, _ := write.Values["model"].(string)
			upstream, _ := write.Values["upstream_model"].(string)
			prompt, _ := write.Values["prompt_tokens"].(int64)
			completion, _ := write.Values["completion_tokens"].(int64)
			total, _ := write.Values["total_tokens"].(int64)
			rows = append(rows, gateway.UsageSummaryRow{
				ExecutionID:      eid,
				ActionID:         id.ActionID(rawAct),
				Model:            model,
				UpstreamModel:    upstream,
				PromptTokens:     prompt,
				CompletionTokens: completion,
				TotalTokens:      total,
			})
		}
	}
	return rows, nil
}

type fakeBudgetEvents struct {
	mu     sync.Mutex
	events []gateway.BudgetExceededEvent
}

var _ gateway.BudgetEventRecorder = (*fakeBudgetEvents)(nil)

func (e *fakeBudgetEvents) RecordBudgetExceeded(_ context.Context, ev gateway.BudgetExceededEvent) error {
	e.mu.Lock()
	e.events = append(e.events, ev)
	e.mu.Unlock()
	return nil
}

func (e *fakeBudgetEvents) list() []gateway.BudgetExceededEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]gateway.BudgetExceededEvent(nil), e.events...)
}

func completionWithUsage(prompt, completion int) string {
	return fmt.Sprintf(
		`{"id":"cmpl-1","model":"upstream-small","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		prompt, completion, prompt+completion,
	)
}

func TestGatewayBudget_ExecutionTokensAndCostUSD(t *testing.T) {
	t.Run("enforces execution max_tokens before calling upstream", func(t *testing.T) {
		fake := newFakes()
		fake.transport.handler = func(*http.Request) (*http.Response, error) {
			return fake.transport.reply(http.StatusOK, "application/json", []byte(completionWithUsage(400, 200))), nil
		}
		budgetReader := &fakeBudgetReader{store: fake.store}
		budgetEvents := &fakeBudgetEvents{}

		cfg := validConfig()
		cfg.Budgets = gateway.BudgetConfig{
			DefaultExecution: gateway.BudgetLimits{MaxTokens: 1000},
		}
		gw, err := gateway.New(
			cfg,
			fake.dependencies(),
			gateway.WithExecutionReader(fake.executions),
			gateway.WithBudgetReader(budgetReader),
			gateway.WithBudgetEventRecorder(budgetEvents),
		)
		if err != nil {
			t.Fatalf("gateway.New: %v", err)
		}

		inv := gateway.Invocation{Credential: testCredential, ProjectID: testProject, Request: validRequest()}
		// Call 1: 0 -> 600 tokens (allowed)
		if _, err := gw.Invoke(context.Background(), inv); err != nil {
			t.Fatalf("call 1: %v", err)
		}
		// Call 2: 600 -> 1200 tokens (allowed, reaches/exceeds 1000)
		if _, err := gw.Invoke(context.Background(), inv); err != nil {
			t.Fatalf("call 2: %v", err)
		}
		if got := fake.transport.count(); got != 2 {
			t.Fatalf("upstream calls after call 2 = %d; want 2", got)
		}

		// Call 3: MUST be rejected BEFORE calling upstream or resolving secret!
		secretsBefore := fake.secrets.count()
		_, err = gw.Invoke(context.Background(), inv)
		if err == nil {
			t.Fatal("call 3 succeeded; want budget_exceeded")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.BudgetExceeded {
			t.Fatalf("call 3 fault class = %q (%v); want budget_exceeded (err=%v)", class, ok, err)
		}
		if code, ok := fault.CodeOf(err); !ok || code != fault.CodeBudgetExceeded {
			t.Fatalf("call 3 fault code = %q (%v); want budget_exceeded", code, ok)
		}
		if got := fake.transport.count(); got != 2 {
			t.Fatalf("upstream calls after budget refusal = %d; want 2 (zero calls once budget hit)", got)
		}
		if fake.secrets.count() != secretsBefore {
			t.Fatalf("secret resolver called after budget refusal: %d -> %d", secretsBefore, fake.secrets.count())
		}
		evs := budgetEvents.list()
		if len(evs) != 1 || evs[0].ScopeKind != "execution" || evs[0].LimitKind != "tokens" || evs[0].UsedTokens != 1200 || evs[0].MaxTokens != 1000 {
			t.Fatalf("budget events = %+v; want 1 execution/tokens event", evs)
		}
	})

	t.Run("enforces execution max_cost_usd using model pricing rates", func(t *testing.T) {
		fake := newFakes()
		// 10,000 prompt tokens @ $3/1M = $0.03; 2,000 completion tokens @ $15/1M = $0.03 => $0.06/call
		fake.transport.handler = func(*http.Request) (*http.Response, error) {
			return fake.transport.reply(http.StatusOK, "application/json", []byte(completionWithUsage(10000, 2000))), nil
		}
		budgetReader := &fakeBudgetReader{store: fake.store}
		budgetEvents := &fakeBudgetEvents{}

		cfg := validConfig()
		cfg.Models[0].InputUSDPerMillionTokens = 3.0
		cfg.Models[0].OutputUSDPerMillionTokens = 15.0
		cfg.Budgets = gateway.BudgetConfig{
			DefaultExecution: gateway.BudgetLimits{MaxCostUSD: 0.05},
		}
		gw, err := gateway.New(
			cfg,
			fake.dependencies(),
			gateway.WithExecutionReader(fake.executions),
			gateway.WithBudgetReader(budgetReader),
			gateway.WithBudgetEventRecorder(budgetEvents),
		)
		if err != nil {
			t.Fatalf("gateway.New: %v", err)
		}

		inv := gateway.Invocation{Credential: testCredential, ProjectID: testProject, Request: validRequest()}
		if _, err := gw.Invoke(context.Background(), inv); err != nil {
			t.Fatalf("call 1: %v", err)
		}
		_, err = gw.Invoke(context.Background(), inv)
		if class, ok := fault.ClassOf(err); !ok || class != fault.BudgetExceeded {
			t.Fatalf("call 2 fault class = %q (%v); want budget_exceeded (err=%v)", class, ok, err)
		}
		if got := fake.transport.count(); got != 1 {
			t.Fatalf("upstream calls = %d; want 1", got)
		}
		evs := budgetEvents.list()
		if len(evs) != 1 || evs[0].LimitKind != "cost_usd" || evs[0].UsedCostUSD < 0.059 {
			t.Fatalf("budget events = %+v; want cost_usd event with ~$0.06 spend", evs)
		}
	})
}

func TestGatewayBudget_TaskScopeAndConcurrency(t *testing.T) {
	t.Run("enforces cumulative task budget across sibling executions", func(t *testing.T) {
		fake := newFakes()
		exec2ID := id.ExecutionID("exec-2")
		cred2 := "kodex.exec-2.credential-material"
		principal2 := identity.Principal{
			ID:          id.PrincipalID("principal-exec-2"),
			Kind:        identity.KindExecution,
			Scope:       testScope(),
			Enabled:     true,
			Version:     1,
			ExecutionID: exec2ID,
		}
		fake.principals.set(cred2, principal2, nil)
		fake.executions.set(authorize.AdmittedExecution{
			Principal:   principal2,
			ExecutionID: exec2ID,
			ActionID:    id.ActionID("action-2"),
			Permission:  authorize.ExecConfiguredModels,
			Deadline:    fixedNow.Add(time.Hour),
		})
		fake.transport.handler = func(*http.Request) (*http.Response, error) {
			return fake.transport.reply(http.StatusOK, "application/json", []byte(completionWithUsage(300, 200))), nil
		}

		siblings := []id.ExecutionID{testExecution, exec2ID}
		budgetReader := &fakeBudgetReader{
			store: fake.store,
			context: map[id.ExecutionID]gateway.ExecutionBudgetContext{
				testExecution: {ExecutionID: testExecution, TaskID: "task-1", SiblingExecutions: siblings},
				exec2ID:       {ExecutionID: exec2ID, TaskID: "task-1", SiblingExecutions: siblings},
			},
		}
		budgetEvents := &fakeBudgetEvents{}

		cfg := validConfig()
		cfg.Budgets = gateway.BudgetConfig{
			DefaultTask: gateway.BudgetLimits{MaxTokens: 500},
		}
		gw, err := gateway.New(
			cfg,
			fake.dependencies(),
			gateway.WithExecutionReader(fake.executions),
			gateway.WithBudgetReader(budgetReader),
			gateway.WithBudgetEventRecorder(budgetEvents),
		)
		if err != nil {
			t.Fatalf("gateway.New: %v", err)
		}

		// Execution 1 consumes 500 tokens (reaching task budget).
		if _, err := gw.Invoke(context.Background(), gateway.Invocation{
			Credential: testCredential, ProjectID: testProject, Request: validRequest(),
		}); err != nil {
			t.Fatalf("exec-1 call: %v", err)
		}
		// Execution 2 in the SAME task is immediately rejected before dialing upstream!
		_, err = gw.Invoke(context.Background(), gateway.Invocation{
			Credential: cred2, ProjectID: testProject, Request: validRequest(),
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.BudgetExceeded {
			t.Fatalf("exec-2 fault class = %q (%v); want budget_exceeded (err=%v)", class, ok, err)
		}
		if got := fake.transport.count(); got != 1 {
			t.Fatalf("upstream calls = %d; want 1", got)
		}
		evs := budgetEvents.list()
		if len(evs) != 1 || evs[0].ScopeKind != "task" || evs[0].TaskID != "task-1" {
			t.Fatalf("budget events = %+v; want task/task-1 event", evs)
		}
	})

	t.Run("concurrent requests for same execution cannot blow past budget", func(t *testing.T) {
		fake := newFakes()
		fake.transport.handler = func(*http.Request) (*http.Response, error) {
			time.Sleep(15 * time.Millisecond)
			return fake.transport.reply(http.StatusOK, "application/json", []byte(completionWithUsage(80, 40))), nil
		}
		budgetReader := &fakeBudgetReader{store: fake.store}

		cfg := validConfig()
		cfg.Budgets = gateway.BudgetConfig{
			DefaultExecution: gateway.BudgetLimits{MaxTokens: 100},
		}
		gw, err := gateway.New(
			cfg,
			fake.dependencies(),
			gateway.WithExecutionReader(fake.executions),
			gateway.WithBudgetReader(budgetReader),
		)
		if err != nil {
			t.Fatalf("gateway.New: %v", err)
		}

		const workers = 6
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			okCount  int
			refused  int
			otherErr []error
		)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, callErr := gw.Invoke(context.Background(), gateway.Invocation{
					Credential: testCredential, ProjectID: testProject, Request: validRequest(),
				})
				mu.Lock()
				defer mu.Unlock()
				if callErr == nil {
					okCount++
				} else if class, ok := fault.ClassOf(callErr); ok && class == fault.BudgetExceeded {
					refused++
				} else {
					otherErr = append(otherErr, callErr)
				}
			}()
		}
		wg.Wait()

		if len(otherErr) > 0 {
			t.Fatalf("unexpected errors: %v", otherErr)
		}
		if okCount != 1 || refused != workers-1 {
			t.Fatalf("ok=%d refused=%d; want ok=1 refused=%d", okCount, refused, workers-1)
		}
		if got := fake.transport.count(); got != 1 {
			t.Fatalf("upstream calls = %d; want exactly 1", got)
		}
	})

	t.Run("rejects invalid budget config", func(t *testing.T) {
		badConfigs := []gateway.BudgetConfig{
			{DefaultExecution: gateway.BudgetLimits{MaxTokens: -1}},
			{DefaultTask: gateway.BudgetLimits{MaxCostUSD: -0.5}},
			{DefaultExecution: gateway.BudgetLimits{MaxCostUSD: math.NaN()}},
			{Pricing: []gateway.ModelPricing{{Model: "", InputUSDPerMillionTokens: 1}}},
			{Pricing: []gateway.ModelPricing{{Model: "m1", InputUSDPerMillionTokens: -1}}},
			{Pricing: []gateway.ModelPricing{{Model: "m1", InputUSDPerMillionTokens: 1}, {Model: "m1", InputUSDPerMillionTokens: 2}}},
		}
		for i, bad := range badConfigs {
			if err := bad.Validate(); err == nil {
				t.Errorf("case %d: Validate(%+v) succeeded; want invalid_input", i, bad)
			}
		}
	})
}

func validRequest() api.ChatCompletionRequest {
	return api.ChatCompletionRequest{
		Model: testModel,
		Messages: []api.ChatMessage{
			{Role: "user", Content: "hi"},
		},
	}
}

func TestGatewayBudget_ActionScopeIsolatedAndPipelineCumulative(t *testing.T) {
	t.Run("action 1 consuming 6000 tokens does not block action 2 with 5000 action budget", func(t *testing.T) {
		fake := newFakes()
		action1 := id.ActionID("action-1")
		action2 := id.ActionID("action-2")
		fake.transport.handler = func(*http.Request) (*http.Response, error) {
			return fake.transport.reply(http.StatusOK, "application/json", []byte(completionWithUsage(2000, 1000))), nil
		}
		budgetReader := &fakeBudgetReader{
			store: fake.store,
			actionCtx: map[string]gateway.ExecutionBudgetContext{
				string(testExecution) + "/" + string(action1): {
					ExecutionID: testExecution,
					Action:      gateway.BudgetLimits{MaxTokens: 10000},
				},
				string(testExecution) + "/" + string(action2): {
					ExecutionID: testExecution,
					Action:      gateway.BudgetLimits{MaxTokens: 5000},
				},
			},
		}
		budgetEvents := &fakeBudgetEvents{}

		gw, err := gateway.New(
			validConfig(),
			fake.dependencies(),
			gateway.WithExecutionReader(fake.executions),
			gateway.WithBudgetReader(budgetReader),
			gateway.WithBudgetEventRecorder(budgetEvents),
		)
		if err != nil {
			t.Fatalf("gateway.New: %v", err)
		}

		// Action 1 makes 2 calls = 6,000 tokens.
		fake.executions.set(authorize.AdmittedExecution{
			Principal:   executionPrincipal(),
			ExecutionID: testExecution,
			ActionID:    action1,
			Permission:  authorize.ExecConfiguredModels,
			Deadline:    fixedNow.Add(time.Hour),
		})
		inv := gateway.Invocation{Credential: testCredential, ProjectID: testProject, Request: validRequest()}
		for i := range 2 {
			if _, err := gw.Invoke(context.Background(), inv); err != nil {
				t.Fatalf("action 1 call %d: %v", i+1, err)
			}
		}

		// Switch to Action 2 (budget: 5,000 tokens). Action 1's 6,000 tokens MUST NOT block Action 2!
		fake.executions.set(authorize.AdmittedExecution{
			Principal:   executionPrincipal(),
			ExecutionID: testExecution,
			ActionID:    action2,
			Permission:  authorize.ExecConfiguredModels,
			Deadline:    fixedNow.Add(time.Hour),
		})
		// Action 2 call 1: 0 -> 3,000 tokens (allowed)
		if _, err := gw.Invoke(context.Background(), inv); err != nil {
			t.Fatalf("action 2 call 1 blocked by action 1 usage: %v", err)
		}
		// Action 2 call 2: 3,000 -> 6,000 tokens (allowed, crosses 5,000)
		if _, err := gw.Invoke(context.Background(), inv); err != nil {
			t.Fatalf("action 2 call 2: %v", err)
		}
		// Action 2 call 3: 6,000 >= 5,000 -> rejected with ScopeKind="action"!
		_, err = gw.Invoke(context.Background(), inv)
		if class, ok := fault.ClassOf(err); !ok || class != fault.BudgetExceeded {
			t.Fatalf("action 2 call 3 err = %v; want budget_exceeded", err)
		}
		evs := budgetEvents.list()
		if len(evs) != 1 || evs[0].ScopeKind != "action" || evs[0].ActionID != action2 || evs[0].UsedTokens != 6000 || evs[0].MaxTokens != 5000 {
			t.Fatalf("budget events = %+v; want 1 action-scoped event for action-2 (6000/5000)", evs)
		}
	})

	t.Run("pipeline budget applies cumulatively across action 1 and action 2", func(t *testing.T) {
		fake := newFakes()
		action1 := id.ActionID("action-1")
		action2 := id.ActionID("action-2")
		fake.transport.handler = func(*http.Request) (*http.Response, error) {
			return fake.transport.reply(http.StatusOK, "application/json", []byte(completionWithUsage(2000, 1000))), nil
		}
		budgetReader := &fakeBudgetReader{
			store: fake.store,
			actionCtx: map[string]gateway.ExecutionBudgetContext{
				string(testExecution) + "/" + string(action1): {
					ExecutionID: testExecution,
					Pipeline:    gateway.BudgetLimits{MaxTokens: 8000},
					Action:      gateway.BudgetLimits{MaxTokens: 10000},
				},
				string(testExecution) + "/" + string(action2): {
					ExecutionID: testExecution,
					Pipeline:    gateway.BudgetLimits{MaxTokens: 8000},
					Action:      gateway.BudgetLimits{MaxTokens: 5000},
				},
			},
		}
		budgetEvents := &fakeBudgetEvents{}

		gw, err := gateway.New(
			validConfig(),
			fake.dependencies(),
			gateway.WithExecutionReader(fake.executions),
			gateway.WithBudgetReader(budgetReader),
			gateway.WithBudgetEventRecorder(budgetEvents),
		)
		if err != nil {
			t.Fatalf("gateway.New: %v", err)
		}

		// Action 1 consumes 6,000 tokens (2 calls).
		fake.executions.set(authorize.AdmittedExecution{
			Principal:   executionPrincipal(),
			ExecutionID: testExecution,
			ActionID:    action1,
			Permission:  authorize.ExecConfiguredModels,
			Deadline:    fixedNow.Add(time.Hour),
		})
		inv := gateway.Invocation{Credential: testCredential, ProjectID: testProject, Request: validRequest()}
		for i := range 2 {
			if _, err := gw.Invoke(context.Background(), inv); err != nil {
				t.Fatalf("action 1 call %d: %v", i+1, err)
			}
		}

		// Action 2 call 1 consumes 3,000 tokens (cumulative execution total = 9,000 >= 8,000).
		fake.executions.set(authorize.AdmittedExecution{
			Principal:   executionPrincipal(),
			ExecutionID: testExecution,
			ActionID:    action2,
			Permission:  authorize.ExecConfiguredModels,
			Deadline:    fixedNow.Add(time.Hour),
		})
		if _, err := gw.Invoke(context.Background(), inv); err != nil {
			t.Fatalf("action 2 call 1: %v", err)
		}

		// Action 2 call 2: Action 2 only used 3,000 < 5,000, but execution total is 9,000 >= 8,000!
		_, err = gw.Invoke(context.Background(), inv)
		if class, ok := fault.ClassOf(err); !ok || class != fault.BudgetExceeded {
			t.Fatalf("action 2 call 2 err = %v; want budget_exceeded", err)
		}
		evs := budgetEvents.list()
		if len(evs) != 1 || evs[0].ScopeKind != "execution" || evs[0].UsedTokens != 9000 || evs[0].MaxTokens != 8000 {
			t.Fatalf("budget events = %+v; want 1 execution-scoped event (9000/8000)", evs)
		}
	})
}

func TestGatewayBudget_TaskSiblingExecutionsRefreshedInsideGate(t *testing.T) {
	fake := newFakes()
	exec2ID := id.ExecutionID("exec-2")
	cred2 := "kodex.exec-2.credential-material"
	principal2 := identity.Principal{
		ID:          id.PrincipalID("principal-exec-2"),
		Kind:        identity.KindExecution,
		Scope:       testScope(),
		Enabled:     true,
		Version:     1,
		ExecutionID: exec2ID,
	}
	fake.principals.set(cred2, principal2, nil)
	fake.executions.set(authorize.AdmittedExecution{
		Principal:   principal2,
		ExecutionID: exec2ID,
		ActionID:    id.ActionID("action-2"),
		Permission:  authorize.ExecConfiguredModels,
		Deadline:    fixedNow.Add(time.Hour),
	})

	exec1InFlight := make(chan struct{})
	releaseExec1 := make(chan struct{})
	fake.transport.handler = func(*http.Request) (*http.Response, error) {
		close(exec1InFlight)
		<-releaseExec1
		return fake.transport.reply(http.StatusOK, "application/json", []byte(completionWithUsage(400, 200))), nil
	}

	// Initially, ReadExecutionBudget only knows about each execution's own ID (stale SiblingExecutions).
	// While exec-1 holds the task gate, exec-2 is registered in taskExecs and races on the same task!
	budgetReader := &fakeBudgetReader{
		store: fake.store,
		context: map[id.ExecutionID]gateway.ExecutionBudgetContext{
			testExecution: {ExecutionID: testExecution, TaskID: "task-race", SiblingExecutions: []id.ExecutionID{testExecution}},
			exec2ID:       {ExecutionID: exec2ID, TaskID: "task-race", SiblingExecutions: []id.ExecutionID{exec2ID}},
		},
		taskExecs: map[id.TaskID][]id.ExecutionID{
			"task-race": {testExecution},
		},
	}

	cfg := validConfig()
	cfg.Budgets = gateway.BudgetConfig{
		DefaultTask: gateway.BudgetLimits{MaxTokens: 500},
	}
	gw, err := gateway.New(
		cfg,
		fake.dependencies(),
		gateway.WithExecutionReader(fake.executions),
		gateway.WithBudgetReader(budgetReader),
	)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}

	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err1 = gw.Invoke(context.Background(), gateway.Invocation{
			Credential: testCredential, ProjectID: testProject, Request: validRequest(),
		})
	}()

	// Wait until exec-1 holds the task gate and is inside its upstream call.
	<-exec1InFlight

	// Now exec-2 joins the task dynamically while exec-1 is in flight, and launches a concurrent request.
	budgetReader.mu.Lock()
	budgetReader.taskExecs["task-race"] = []id.ExecutionID{testExecution, exec2ID}
	budgetReader.mu.Unlock()

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err2 = gw.Invoke(context.Background(), gateway.Invocation{
			Credential: cred2, ProjectID: testProject, Request: validRequest(),
		})
	}()

	// Give exec-2 time to block on the task gate, then let exec-1 complete and persist its 600 tokens.
	time.Sleep(25 * time.Millisecond)
	close(releaseExec1)
	wg.Wait()

	if err1 != nil {
		t.Fatalf("exec-1 failed: %v", err1)
	}
	if class, ok := fault.ClassOf(err2); !ok || class != fault.BudgetExceeded {
		t.Fatalf("exec-2 err = %v; want budget_exceeded (refreshed siblings inside gate)", err2)
	}
	if got := fake.transport.count(); got != 1 {
		t.Fatalf("upstream calls = %d; want exactly 1", got)
	}
}

func TestGatewayBudget_CrossTenantPricingIsolation(t *testing.T) {
	scopeA := tenancy.Scope{TenantID: "tenant-a", ProjectID: "proj-a"}
	scopeB := tenancy.Scope{TenantID: "tenant-b", ProjectID: "proj-b"}
	execA := id.ExecutionID("exec-a")
	execB := id.ExecutionID("exec-b")

	usageByScope := map[tenancy.Scope][]gateway.UsageSummaryRow{
		scopeA: {{ExecutionID: execA, Model: "gpt-4o", UpstreamModel: "gpt-4o", PromptTokens: 10000, CompletionTokens: 0, TotalTokens: 10000}},
		scopeB: {{ExecutionID: execB, Model: "gpt-4o", UpstreamModel: "gpt-4o", PromptTokens: 10000, CompletionTokens: 0, TotalTokens: 10000}},
	}
	reader := &staticBudgetReader{usageByScope: usageByScope}

	// Tenant A prices gpt-4o at $10/1M (10k tokens = $0.10 >= $0.05 limit).
	// Tenant B prices the SAME model name "gpt-4o" at $0.001/1M (10k tokens = $0.00001 < $0.05 limit).
	models := []gateway.ModelConfig{
		{
			Scope:                     scopeA,
			Model:                     "gpt-4o",
			UpstreamModel:             "gpt-4o",
			InputUSDPerMillionTokens:  10.0,
			OutputUSDPerMillionTokens: 30.0,
		},
		{
			Scope:                     scopeB,
			Model:                     "gpt-4o",
			UpstreamModel:             "gpt-4o",
			InputUSDPerMillionTokens:  0.001,
			OutputUSDPerMillionTokens: 0.001,
		},
	}
	tracker, err := gateway.NewBudgetTracker(gateway.BudgetConfig{
		DefaultExecution: gateway.BudgetLimits{MaxCostUSD: 0.05},
	}, models, reader, nil)
	if err != nil {
		t.Fatalf("NewBudgetTracker: %v", err)
	}

	// Tenant A MUST still see $10/1M -> $0.10 >= $0.05 -> rejected!
	_, errA := tracker.CheckAndAcquire(context.Background(), gateway.BudgetCheckRequest{
		Scope:       scopeA,
		PrincipalID: "p-a",
		ExecutionID: execA,
		Model:       "gpt-4o",
	})
	if class, ok := fault.ClassOf(errA); !ok || class != fault.BudgetExceeded {
		t.Fatalf("tenant A err = %v; want budget_exceeded (Tenant B must not overwrite Tenant A pricing)", errA)
	}

	// Tenant B MUST see $0.001/1M -> $0.00001 < $0.05 -> allowed!
	releaseB, errB := tracker.CheckAndAcquire(context.Background(), gateway.BudgetCheckRequest{
		Scope:       scopeB,
		PrincipalID: "p-b",
		ExecutionID: execB,
		Model:       "gpt-4o",
	})
	if errB != nil {
		t.Fatalf("tenant B err = %v; want allowed under Tenant B's own pricing", errB)
	}
	releaseB()
}

func TestGatewayBudget_MissingPricingFailsClosedOnActiveCostBudget(t *testing.T) {
	scope := testScope()
	reader := &staticBudgetReader{}

	t.Run("rejects unpriced model when MaxCostUSD is active", func(t *testing.T) {
		tracker, err := gateway.NewBudgetTracker(gateway.BudgetConfig{
			DefaultExecution: gateway.BudgetLimits{MaxCostUSD: 1.0},
		}, nil, reader, nil)
		if err != nil {
			t.Fatalf("NewBudgetTracker: %v", err)
		}
		_, err = tracker.CheckAndAcquire(context.Background(), gateway.BudgetCheckRequest{
			Scope:       scope,
			PrincipalID: testPrincipal,
			ExecutionID: testExecution,
			Model:       "openai/unpriced-model",
		})
		if err == nil {
			t.Fatal("CheckAndAcquire succeeded for unpriced model under active MaxCostUSD; want fail-closed error")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.UnsupportedCapability {
			t.Fatalf("err class = %q (%v); want unsupported_capability (err=%v)", class, ok, err)
		}
	})

	t.Run("allows model covered by wildcard pricing", func(t *testing.T) {
		tracker, err := gateway.NewBudgetTracker(gateway.BudgetConfig{
			DefaultExecution: gateway.BudgetLimits{MaxCostUSD: 1.0},
			Pricing: []gateway.ModelPricing{
				{Model: "openai/*", InputUSDPerMillionTokens: 2.0, OutputUSDPerMillionTokens: 8.0},
			},
		}, nil, reader, nil)
		if err != nil {
			t.Fatalf("NewBudgetTracker: %v", err)
		}
		release, err := tracker.CheckAndAcquire(context.Background(), gateway.BudgetCheckRequest{
			Scope:       scope,
			PrincipalID: testPrincipal,
			ExecutionID: testExecution,
			Model:       "openai/gpt-4o",
		})
		if err != nil {
			t.Fatalf("CheckAndAcquire with wildcard pricing failed: %v", err)
		}
		release()
	})

	t.Run("rejects unpriced historical usage row when MaxCostUSD is active", func(t *testing.T) {
		histReader := &staticBudgetReader{
			usageByScope: map[tenancy.Scope][]gateway.UsageSummaryRow{
				scope: {{
					ExecutionID:      testExecution,
					Model:            "anthropic/unpriced-old",
					PromptTokens:     5000,
					CompletionTokens: 1000,
					TotalTokens:      6000,
				}},
			},
		}
		tracker, err := gateway.NewBudgetTracker(gateway.BudgetConfig{
			DefaultExecution: gateway.BudgetLimits{MaxCostUSD: 1.0},
			Pricing: []gateway.ModelPricing{
				{Model: "openai/gpt-4o", InputUSDPerMillionTokens: 2.0, OutputUSDPerMillionTokens: 8.0},
			},
		}, nil, histReader, nil)
		if err != nil {
			t.Fatalf("NewBudgetTracker: %v", err)
		}
		_, err = tracker.CheckAndAcquire(context.Background(), gateway.BudgetCheckRequest{
			Scope:       scope,
			PrincipalID: testPrincipal,
			ExecutionID: testExecution,
			Model:       "openai/gpt-4o",
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.UnsupportedCapability {
			t.Fatalf("err = %v; want unsupported_capability for unpriced historical usage", err)
		}
	})
}

func TestGatewayBudget_NegativeOverflowAndNonFiniteSafety(t *testing.T) {
	scope := testScope()

	t.Run("negative token counts are clamped to 0 and cannot evade budget", func(t *testing.T) {
		reader := &staticBudgetReader{
			usageByScope: map[tenancy.Scope][]gateway.UsageSummaryRow{
				scope: {
					{ExecutionID: testExecution, Model: "m", TotalTokens: 1000},
					{ExecutionID: testExecution, Model: "m", PromptTokens: -900, CompletionTokens: -900, TotalTokens: -900},
				},
			},
		}
		tracker, err := gateway.NewBudgetTracker(gateway.BudgetConfig{
			DefaultExecution: gateway.BudgetLimits{MaxTokens: 1000},
		}, nil, reader, nil)
		if err != nil {
			t.Fatalf("NewBudgetTracker: %v", err)
		}
		_, err = tracker.CheckAndAcquire(context.Background(), gateway.BudgetCheckRequest{
			Scope:       scope,
			PrincipalID: testPrincipal,
			ExecutionID: testExecution,
			Model:       "m",
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.BudgetExceeded {
			t.Fatalf("err = %v; want budget_exceeded (negative tokens must not subtract)", err)
		}
	})

	t.Run("int64 overflow saturates instead of wrapping negative", func(t *testing.T) {
		reader := &staticBudgetReader{
			usageByScope: map[tenancy.Scope][]gateway.UsageSummaryRow{
				scope: {
					{ExecutionID: testExecution, Model: "m", TotalTokens: math.MaxInt64 - 10},
					{ExecutionID: testExecution, Model: "m", TotalTokens: 1000},
				},
			},
		}
		events := &fakeBudgetEvents{}
		tracker, err := gateway.NewBudgetTracker(gateway.BudgetConfig{
			DefaultExecution: gateway.BudgetLimits{MaxTokens: 1000},
		}, nil, reader, events)
		if err != nil {
			t.Fatalf("NewBudgetTracker: %v", err)
		}
		_, err = tracker.CheckAndAcquire(context.Background(), gateway.BudgetCheckRequest{
			Scope:       scope,
			PrincipalID: testPrincipal,
			ExecutionID: testExecution,
			Model:       "m",
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.BudgetExceeded {
			t.Fatalf("err = %v; want budget_exceeded (overflow must saturate)", err)
		}
		evs := events.list()
		if len(evs) != 1 || evs[0].UsedTokens != math.MaxInt64 {
			t.Fatalf("events = %+v; want UsedTokens == MaxInt64", evs)
		}
	})

	t.Run("RecordBudgetExceeded sanitizes NaN and Inf costs before JSON marshaling", func(t *testing.T) {
		fake := newFakes()
		recorder := &gateway.StoreBudgetEventRecorder{
			Executor: fake.store,
			Clock:    fake.clock,
			NewID:    func() (id.EventID, error) { return id.EventID("ev-nan-inf"), nil },
		}
		for _, cost := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -5.0} {
			err := recorder.RecordBudgetExceeded(context.Background(), gateway.BudgetExceededEvent{
				Scope:       scope,
				PrincipalID: testPrincipal,
				ExecutionID: testExecution,
				Model:       "m",
				ScopeKind:   "execution",
				LimitKind:   "cost_usd",
				UsedTokens:  -10,
				MaxTokens:   -5,
				UsedCostUSD: cost,
				MaxCostUSD:  math.Inf(1),
			})
			if err != nil {
				t.Fatalf("RecordBudgetExceeded(cost=%v) failed: %v", cost, err)
			}
		}
	})
}

type staticBudgetReader struct {
	usageByScope map[tenancy.Scope][]gateway.UsageSummaryRow
}

func (r *staticBudgetReader) ReadExecutionBudget(_ context.Context, _ tenancy.Scope, execID id.ExecutionID, _ id.ActionID) (gateway.ExecutionBudgetContext, error) {
	return gateway.ExecutionBudgetContext{ExecutionID: execID}, nil
}

func (r *staticBudgetReader) ReadTaskExecutions(_ context.Context, _ tenancy.Scope, _ id.TaskID) ([]id.ExecutionID, error) {
	return nil, nil
}

func (r *staticBudgetReader) ReadUsageTotals(_ context.Context, scope tenancy.Scope, _ []id.ExecutionID) ([]gateway.UsageSummaryRow, error) {
	return r.usageByScope[scope], nil
}
