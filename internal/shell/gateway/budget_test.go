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
	mu      sync.Mutex
	store   *fakeStore
	context map[id.ExecutionID]gateway.ExecutionBudgetContext
}

var _ gateway.BudgetReader = (*fakeBudgetReader)(nil)

func (r *fakeBudgetReader) ReadExecutionBudget(_ context.Context, _ tenancy.Scope, execID id.ExecutionID, _ id.ActionID) (gateway.ExecutionBudgetContext, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx, ok := r.context[execID]; ok {
		return ctx, nil
	}
	return gateway.ExecutionBudgetContext{
		ExecutionID:       execID,
		SiblingExecutions: []id.ExecutionID{execID},
	}, nil
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
			model, _ := write.Values["model"].(string)
			upstream, _ := write.Values["upstream_model"].(string)
			prompt, _ := write.Values["prompt_tokens"].(int64)
			completion, _ := write.Values["completion_tokens"].(int64)
			total, _ := write.Values["total_tokens"].(int64)
			rows = append(rows, gateway.UsageSummaryRow{
				ExecutionID:      eid,
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
