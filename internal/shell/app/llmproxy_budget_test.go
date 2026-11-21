package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/gateway"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/pipeline"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/store/memory"
	"github.com/optimiware/kodema/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

func seedBudgetExecution(
	t *testing.T,
	mem *memory.Store,
	scope tenancy.Scope,
	execID id.ExecutionID,
	actionID id.ActionID,
	principalID id.PrincipalID,
	taskID id.TaskID,
	pipelineBudget *definition.Budget,
	actionBudget *definition.Budget,
) {
	t.Helper()
	ctx := context.Background()

	// Seed tenant, project, and task (if present) idempotently.
	_, _ = mem.Execute(ctx, tenancy.Scope{TenantID: scope.TenantID}, store.Unit{
		Writes: []store.Write{{
			Table:  "tenants",
			ID:     string(scope.TenantID),
			Insert: true,
			Values: map[string]any{"name": string(scope.TenantID), "version": int64(1), "created_at": time.Now().UTC()},
		}},
	})
	_, _ = mem.Execute(ctx, scope, store.Unit{
		Writes: []store.Write{{
			Table:  "projects",
			ID:     string(scope.ProjectID),
			Insert: true,
			Values: map[string]any{"name": string(scope.ProjectID), "version": int64(1), "created_at": time.Now().UTC()},
		}},
	})
	if taskID != "" {
		_, _ = mem.Execute(ctx, scope, store.Unit{
			Writes: []store.Write{{
				Table:  "tasks",
				ID:     string(taskID),
				Insert: true,
				Values: map[string]any{
					"title":             "budget task",
					"description":       "test",
					"state":             "open",
					"lifecycle_version": int64(1),
					"content_version":   int64(1),
					"created_at":        time.Now().UTC(),
					"updated_at":        time.Now().UTC(),
				},
			}},
		})
	}

	snap, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:   []byte("actions:\n  - name: work\n    image: reg/img@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n    commands: [\"true\"]\n"),
		Budget: pipelineBudget,
		TaskID: taskID,
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	admitted := snapshotdoc.Admitted{
		Snapshot: snap,
		Actions: []pipeline.AdmittedAction{{
			Index:   0,
			Name:    "work",
			Kind:    definition.ActionKindContainer,
			Timeout: 5 * time.Minute,
			Container: &definition.Container{
				Image:    "reg/img@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Commands: []string{"true"},
				Budget:   actionBudget,
			},
		}},
	}
	snapBytes, err := snapshotdoc.Encode(admitted)
	if err != nil {
		t.Fatalf("snapshotdoc.Encode: %v", err)
	}
	var nullableTask any
	if taskID != "" {
		nullableTask = string(taskID)
	}
	if _, err := mem.Execute(ctx, scope, store.Unit{
		Writes: []store.Write{
			{
				Table:  "executions",
				ID:     string(execID),
				Insert: true,
				Values: map[string]any{
					"task_id":      nullableTask,
					"principal_id": string(principalID),
					"state":        string(kernel.ExecutionRunning),
					"version":      int64(1),
					"yaml_sha256":  snap.YAMLSHA256[:],
					"snapshot":     snapBytes,
					"workspace_id": "ws-" + string(execID),
					"created_at":   time.Now().UTC(),
				},
			},
			{
				Table:  "actions",
				ID:     string(actionID),
				Insert: true,
				Values: map[string]any{
					"execution_id":       string(execID),
					"index":              int64(0),
					"name":               "work",
					"kind":               string(definition.ActionKindContainer),
					"state":              string(kernel.ActionRunning),
					"version":            int64(1),
					"timeout_ms":         int64(300000),
					"staged_outputs_set": false,
					"launch_state":       string(kernel.LaunchIssued),
				},
			},
		},
	}); err != nil {
		t.Fatalf("seed execution %s: %v", execID, err)
	}
}

func TestLLMProxy_ActiveBudgetsExecutionActionTaskAndConcurrency(t *testing.T) {
	scope := tenancy.Scope{TenantID: id.TenantID("t1"), ProjectID: id.ProjectID("p1")}
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clk := budgetTestClock{now: now}

	t.Run("pipeline YAML action max_tokens & max_cost_usd reject streaming calls before dialing upstream", func(t *testing.T) {
		var upstreamCalls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4\",\"usage\":{\"input_tokens\":10000,\"output_tokens\":0}}}\n\n")
			_, _ = fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2000}}\n\n")
		}))
		defer upstream.Close()

		mem := memory.NewWithClock(clk.Now)
		execID := id.ExecutionID("exec-yaml-budget-1")
		actionID := id.ActionID("act-yaml-budget-1")
		principalID := id.PrincipalID("principal-exec-1")
		tokenStr := "kodex.exec-yaml-budget-1.mac"

		// Action budget: $0.05 USD (pricing below makes 10k in + 2k out = $0.06 USD).
		seedBudgetExecution(t, mem, scope, execID, actionID, principalID, "",
			&definition.Budget{MaxTokens: 100000},
			&definition.Budget{MaxCostUSD: 0.05},
		)

		principal := identity.Principal{
			ID:          principalID,
			Kind:        identity.KindExecution,
			Scope:       scope,
			Enabled:     true,
			Version:     1,
			ExecutionID: execID,
		}
		admitted := authorize.AdmittedExecution{
			Principal:   principal,
			ExecutionID: execID,
			ActionID:    actionID,
			Permission:  authorize.ExecConfiguredModels,
			Deadline:    now.Add(10 * time.Minute),
		}

		var usageSeq, eventSeq atomic.Int64
		cipher := newTestCipher(t)
		cfg := &ModelsConfig{
			Providers: []ModelProviderConfig{{
				Scope:      scope,
				Name:       "anthropic",
				BaseURL:    upstream.URL,
				SecretName: llmProxyTestAnthropicSecretName,
			}},
			Budgets: gateway.BudgetConfig{
				Pricing: []gateway.ModelPricing{{
					Model:                     "anthropic/claude-sonnet-4",
					InputUSDPerMillionTokens:  3.0,
					OutputUSDPerMillionTokens: 15.0,
				}},
			},
		}
		handler, stop, err := newLLMProxyHandler(llmProxyDependencies{
			Config:         cfg,
			Profile:        ProfileDevelopment,
			InstallationID: "installation-1",
			Executions:     fixedExecutionReader{admitted: admitted},
			Principals:     staticPrincipalResolver{byToken: map[string]identity.Principal{tokenStr: principal}},
			Authority:      budgetTestAllowAuthority{},
			Lookup: mapSecretLookup{
				cipher: cipher,
				values: map[id.SecretName]string{llmProxyTestAnthropicSecretName: llmProxyTestAnthropicSecret},
			},
			Cipher:     cipher,
			Store:      mem,
			Scanner:    mem,
			NewUsageID: func() (gateway.UsageID, error) { return gateway.UsageID(fmt.Sprintf("u-%d", usageSeq.Add(1))), nil },
			NewEventID: func() (id.EventID, error) { return id.EventID(fmt.Sprintf("ev-%d", eventSeq.Add(1))), nil },
			Clock:      clk,
		})
		if err != nil {
			t.Fatalf("newLLMProxyHandler: %v", err)
		}
		if stop != nil {
			defer func() { _ = stop(context.Background()) }()
		}

		// Request 1 succeeds and consumes ~$0.06 USD.
		rec1 := httptest.NewRecorder()
		req1 := httptest.NewRequest(http.MethodPost, "/llm/anthropic/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","stream":true}`))
		req1.Header.Set("x-api-key", tokenStr)
		handler.ServeHTTP(rec1, req1)
		if rec1.Code != http.StatusOK {
			t.Fatalf("call 1 status = %d body=%s; want 200", rec1.Code, rec1.Body.String())
		}
		if upstreamCalls.Load() != 1 {
			t.Fatalf("upstream calls after call 1 = %d; want 1", upstreamCalls.Load())
		}

		// Request 2 MUST be rejected BEFORE dialing upstream with 429 budget_exceeded!
		rec2 := httptest.NewRecorder()
		req2 := httptest.NewRequest(http.MethodPost, "/llm/anthropic/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","stream":true}`))
		req2.Header.Set("x-api-key", tokenStr)
		handler.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusTooManyRequests {
			t.Fatalf("call 2 status = %d body=%s; want 429", rec2.Code, rec2.Body.String())
		}
		var apiErr api.Error
		if err := json.Unmarshal(rec2.Body.Bytes(), &apiErr); err != nil {
			t.Fatalf("unmarshal error body: %v (%s)", err, rec2.Body.String())
		}
		if apiErr.Code != api.CodeBudgetExceeded {
			t.Fatalf("call 2 error code = %q; want %q", apiErr.Code, api.CodeBudgetExceeded)
		}
		if upstreamCalls.Load() != 1 {
			t.Fatalf("upstream calls after budget exceeded = %d; want 1 (zero upstream calls once budget hit)", upstreamCalls.Load())
		}

		// Verify attributable model.budget_exceeded event was persisted.
		page, err := mem.Query(context.Background(), scope, store.ScanQuery{
			Table:   "events",
			Equals:  map[string]any{"type": string(event.TypeModelBudgetExceeded)},
			OrderBy: []store.Order{{Column: "at"}, {Column: "id"}},
			Limit:   10,
		})
		if err != nil {
			t.Fatalf("query budget events: %v", err)
		}
		if len(page.Rows) != 1 {
			t.Fatalf("persisted model.budget_exceeded events = %d; want 1", len(page.Rows))
		}
	})

	t.Run("task budget enforced across sibling executions and concurrent requests", func(t *testing.T) {
		var upstreamCalls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamCalls.Add(1)
			time.Sleep(15 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"c1","model":"gpt-4o","usage":{"prompt_tokens":80,"completion_tokens":40,"total_tokens":120}}`))
		}))
		defer upstream.Close()

		mem := memory.NewWithClock(clk.Now)
		taskID := id.TaskID("task-budget-1")
		exec1 := id.ExecutionID("exec-task-1")
		act1 := id.ActionID("act-task-1")
		p1 := id.PrincipalID("principal-task-1")
		tok1 := "kodex.exec-task-1.mac"

		exec2 := id.ExecutionID("exec-task-2")
		act2 := id.ActionID("act-task-2")
		p2 := id.PrincipalID("principal-task-2")
		tok2 := "kodex.exec-task-2.mac"

		seedBudgetExecution(t, mem, scope, exec1, act1, p1, taskID, nil, nil)
		seedBudgetExecution(t, mem, scope, exec2, act2, p2, taskID, nil, nil)

		pr1 := identity.Principal{ID: p1, Kind: identity.KindExecution, Scope: scope, Enabled: true, Version: 1, ExecutionID: exec1}
		pr2 := identity.Principal{ID: p2, Kind: identity.KindExecution, Scope: scope, Enabled: true, Version: 1, ExecutionID: exec2}
		execReader := mapExecutionReader{
			byID: map[id.ExecutionID]authorize.AdmittedExecution{
				exec1: {Principal: pr1, ExecutionID: exec1, ActionID: act1, Permission: authorize.ExecConfiguredModels, Deadline: now.Add(10 * time.Minute)},
				exec2: {Principal: pr2, ExecutionID: exec2, ActionID: act2, Permission: authorize.ExecConfiguredModels, Deadline: now.Add(10 * time.Minute)},
			},
		}

		var usageSeq, eventSeq atomic.Int64
		cipher := newTestCipher(t)
		cfg := &ModelsConfig{
			Providers: []ModelProviderConfig{{
				Scope:      scope,
				Name:       "openai",
				BaseURL:    upstream.URL,
				SecretName: llmProxyTestOpenAISecretName,
			}},
			Budgets: gateway.BudgetConfig{
				DefaultTask: gateway.BudgetLimits{MaxTokens: 100},
			},
		}
		handler, stop, err := newLLMProxyHandler(llmProxyDependencies{
			Config:         cfg,
			Profile:        ProfileDevelopment,
			InstallationID: "installation-1",
			Executions:     execReader,
			Principals:     staticPrincipalResolver{byToken: map[string]identity.Principal{tok1: pr1, tok2: pr2}},
			Authority:      budgetTestAllowAuthority{},
			Lookup: mapSecretLookup{
				cipher: cipher,
				values: map[id.SecretName]string{llmProxyTestOpenAISecretName: llmProxyTestOpenAISecret},
			},
			Cipher:     cipher,
			Store:      mem,
			Scanner:    mem,
			NewUsageID: func() (gateway.UsageID, error) { return gateway.UsageID(fmt.Sprintf("u-%d", usageSeq.Add(1))), nil },
			NewEventID: func() (id.EventID, error) { return id.EventID(fmt.Sprintf("ev-%d", eventSeq.Add(1))), nil },
			Clock:      clk,
		})
		if err != nil {
			t.Fatalf("newLLMProxyHandler: %v", err)
		}
		if stop != nil {
			defer func() { _ = stop(context.Background()) }()
		}

		// Fire 5 concurrent requests across exec-1 and exec-2 in the same task.
		// Exactly ONE request may dial upstream (consuming 120 >= 100 tokens);
		// all remaining 4 must receive HTTP 429 budget_exceeded without dialing upstream!
		tokens := []string{tok1, tok2, tok1, tok2, tok1}
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			okCount int
			bCount  int
		)
		for _, tok := range tokens {
			wg.Add(1)
			go func(credential string) {
				defer wg.Done()
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/llm/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
				req.Header.Set("Authorization", "Bearer "+credential)
				handler.ServeHTTP(rec, req)
				mu.Lock()
				defer mu.Unlock()
				switch rec.Code {
				case http.StatusOK:
					okCount++
				case http.StatusTooManyRequests:
					var apiErr api.Error
					if err := json.Unmarshal(rec.Body.Bytes(), &apiErr); err == nil && apiErr.Code == api.CodeBudgetExceeded {
						bCount++
					}
				}
			}(tok)
		}
		wg.Wait()

		if okCount != 1 || bCount != len(tokens)-1 {
			t.Fatalf("concurrent task budget results: ok=%d budget_exceeded=%d; want ok=1 budget_exceeded=%d", okCount, bCount, len(tokens)-1)
		}
		if got := upstreamCalls.Load(); got != 1 {
			t.Fatalf("upstream calls = %d; want exactly 1", got)
		}
	})

	t.Run("LoadModelsConfigFile parses budgets and pricing and rejects invalid entries", func(t *testing.T) {
		validJSON := `{
			"providers": [{
				"tenant_id": "t1",
				"project_id": "p1",
				"name": "openai",
				"base_url": "https://api.openai.com",
				"secret_name": "OPENAI_API_KEY"
			}],
			"budgets": {
				"default_execution": {"max_tokens": 50000, "max_cost_usd": 2.5},
				"default_task": {"max_tokens": 200000, "max_cost_usd": 10.0},
				"pricing": [{
					"model": "openai/gpt-4o",
					"input_usd_per_million_tokens": 2.5,
					"output_usd_per_million_tokens": 10.0
				}]
			}
		}`
		path := filepath.Join(t.TempDir(), "models.json")
		if err := os.WriteFile(path, []byte(validJSON), 0o600); err != nil {
			t.Fatalf("write models.json: %v", err)
		}
		loaded, err := LoadModelsConfigFile(path, ProfileProduction)
		if err != nil {
			t.Fatalf("LoadModelsConfigFile valid budgets: %v", err)
		}
		if loaded.Budgets.DefaultExecution.MaxTokens != 50000 || loaded.Budgets.DefaultTask.MaxCostUSD != 10.0 || len(loaded.Budgets.Pricing) != 1 {
			t.Fatalf("loaded budgets = %+v", loaded.Budgets)
		}

		badPath := filepath.Join(t.TempDir(), "bad-models.json")
		badJSON := `{
			"providers": [{
				"tenant_id": "t1",
				"project_id": "p1",
				"name": "openai",
				"base_url": "https://api.openai.com",
				"secret_name": "OPENAI_API_KEY"
			}],
			"budgets": {
				"default_execution": {"max_tokens": -5}
			}
		}`
		if err := os.WriteFile(badPath, []byte(badJSON), 0o600); err != nil {
			t.Fatalf("write bad models.json: %v", err)
		}
		if _, err := LoadModelsConfigFile(badPath, ProfileDevelopment); err == nil {
			t.Fatal("LoadModelsConfigFile accepted negative max_tokens budget")
		}
	})
}

type budgetTestClock struct{ now time.Time }

func (c budgetTestClock) Now() time.Time { return c.now }

func (budgetTestClock) Sleep(context.Context, time.Duration) error { return nil }

type budgetTestAllowAuthority struct{}

func (budgetTestAllowAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	facts := make([]authorize.AuthorityFact, len(reads))
	for i, r := range reads {
		facts[i] = authorize.AuthorityFact{
			Check:       r.Check,
			Scope:       r.Scope,
			PrincipalID: r.PrincipalID,
			TargetID:    r.TargetID,
			Allowed:     true,
		}
	}
	return facts, nil
}

type mapExecutionReader struct {
	byID map[id.ExecutionID]authorize.AdmittedExecution
}

func (r mapExecutionReader) ReadExecution(_ context.Context, eid id.ExecutionID) (authorize.AdmittedExecution, error) {
	admitted, ok := r.byID[eid]
	if !ok {
		return authorize.AdmittedExecution{}, os.ErrNotExist
	}
	return admitted, nil
}
