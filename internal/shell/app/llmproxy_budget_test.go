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
	"github.com/optimiware/kodema/internal/fault"
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

func TestScopedBudgetReader_FailsClosedOnCorruptSnapshotAndActionStoreErrors(t *testing.T) {
	scope := tenancy.Scope{TenantID: id.TenantID("t1"), ProjectID: id.ProjectID("p1")}
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	t.Run("corrupt snapshot bytes fail closed instead of ignoring decode error", func(t *testing.T) {
		mem := memory.NewWithClock(func() time.Time { return now })
		execID := id.ExecutionID("exec-corrupt-snap")
		actID := id.ActionID("act-corrupt-snap")
		seedBudgetExecution(t, mem, scope, execID, actID, "principal-1", "", nil, &definition.Budget{MaxTokens: 100})

		// Replace snapshot with corrupt JSON bytes.
		_, err := mem.Execute(context.Background(), scope, store.Unit{
			Writes: []store.Write{{
				Table:         "executions",
				ID:            string(execID),
				ExpectVersion: 1,
				Values:        map[string]any{"snapshot": []byte(`{not-valid-snapshot`)},
			}},
		})
		if err != nil {
			t.Fatalf("corrupt snapshot write: %v", err)
		}

		reader := newScopedBudgetReader(mem, mem)
		_, err = reader.ReadExecutionBudget(context.Background(), scope, execID, actID)
		if err == nil {
			t.Fatal("ReadExecutionBudget succeeded on corrupt snapshot; want fail-closed error")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
			t.Fatalf("corrupt snapshot fault class = %q (%v); want internal_failure (err=%v)", class, ok, err)
		}
	})

	t.Run("non-NotFound store error reading action row fails closed", func(t *testing.T) {
		mem := memory.NewWithClock(func() time.Time { return now })
		execID := id.ExecutionID("exec-act-err")
		actID := id.ActionID("act-err-1")
		seedBudgetExecution(t, mem, scope, execID, actID, "principal-1", "", nil, &definition.Budget{MaxTokens: 100})

		failingExec := &actionReadFailingExecutor{
			inner: mem,
			err:   fault.New(fault.DependencyUnavailable, "injected actions read failure"),
		}
		reader := newScopedBudgetReader(failingExec, mem)
		_, err := reader.ReadExecutionBudget(context.Background(), scope, execID, actID)
		if err == nil {
			t.Fatal("ReadExecutionBudget succeeded when actions read failed; want fail-closed error")
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("actions read error fault class = %q (%v); want dependency_unavailable (err=%v)", class, ok, err)
		}
	})

	t.Run("missing action row (NotFound) still succeeds cleanly", func(t *testing.T) {
		mem := memory.NewWithClock(func() time.Time { return now })
		execID := id.ExecutionID("exec-act-missing")
		actID := id.ActionID("act-existing")
		seedBudgetExecution(t, mem, scope, execID, actID, "principal-1", "", &definition.Budget{MaxTokens: 500}, nil)

		reader := newScopedBudgetReader(mem, mem)
		got, err := reader.ReadExecutionBudget(context.Background(), scope, execID, id.ActionID("act-nonexistent"))
		if err != nil {
			t.Fatalf("ReadExecutionBudget with NotFound action row failed: %v", err)
		}
		if got.Pipeline.MaxTokens != 500 {
			t.Fatalf("pipeline budget = %+v; want 500", got.Pipeline)
		}
	})
}

type actionReadFailingExecutor struct {
	inner store.Executor
	err   error
}

func (e *actionReadFailingExecutor) Read(ctx context.Context, scope tenancy.Scope, table, rowID string) (store.Row, error) {
	if table == "actions" {
		return store.Row{}, e.err
	}
	return e.inner.Read(ctx, scope, table, rowID)
}

func (e *actionReadFailingExecutor) Execute(ctx context.Context, scope tenancy.Scope, unit store.Unit) (store.UnitResult, error) {
	return e.inner.Execute(ctx, scope, unit)
}

func TestLLMProxy_OpenAIStreamingInjectsIncludeUsageAndEnforcesBudget(t *testing.T) {
	scope := tenancy.Scope{TenantID: id.TenantID("t1"), ProjectID: id.ProjectID("p1")}
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clk := budgetTestClock{now: now}

	var (
		upstreamCalls atomic.Int32
		sawInclude    atomic.Bool
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		var body struct {
			Stream        bool `json:"stream"`
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.Stream && body.StreamOptions != nil && body.StreamOptions.IncludeUsage {
			sawInclude.Store(true)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"gpt-4o\",\"choices\":[],\"usage\":{\"prompt_tokens\":600,\"completion_tokens\":400,\"total_tokens\":1000}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	mem := memory.NewWithClock(clk.Now)
	execID := id.ExecutionID("exec-openai-sse")
	actionID := id.ActionID("act-openai-sse")
	principalID := id.PrincipalID("principal-openai-sse")
	tok := "kodex.exec-openai-sse.mac"

	seedBudgetExecution(t, mem, scope, execID, actionID, principalID, "", nil, &definition.Budget{MaxTokens: 1000})

	principal := identity.Principal{ID: principalID, Kind: identity.KindExecution, Scope: scope, Enabled: true, Version: 1, ExecutionID: execID}
	admitted := authorize.AdmittedExecution{Principal: principal, ExecutionID: execID, ActionID: actionID, Permission: authorize.ExecConfiguredModels, Deadline: now.Add(10 * time.Minute)}
	cipher := newTestCipher(t)
	var usageSeq, eventSeq atomic.Int64

	handler, stop, err := newLLMProxyHandler(llmProxyDependencies{
		Config: &ModelsConfig{
			Providers: []ModelProviderConfig{{
				Scope:      scope,
				Name:       "openai",
				BaseURL:    upstream.URL,
				SecretName: llmProxyTestOpenAISecretName,
			}},
		},
		Profile:        ProfileDevelopment,
		InstallationID: "installation-1",
		Executions:     fixedExecutionReader{admitted: admitted},
		Principals:     staticPrincipalResolver{byToken: map[string]identity.Principal{tok: principal}},
		Authority:      budgetTestAllowAuthority{},
		Lookup: mapSecretLookup{
			cipher: cipher,
			values: map[id.SecretName]string{llmProxyTestOpenAISecretName: llmProxyTestOpenAISecret},
		},
		Cipher:     cipher,
		Store:      mem,
		Scanner:    mem,
		NewUsageID: func() (gateway.UsageID, error) { return gateway.UsageID(fmt.Sprintf("u-sse-%d", usageSeq.Add(1))), nil },
		NewEventID: func() (id.EventID, error) { return id.EventID(fmt.Sprintf("ev-sse-%d", eventSeq.Add(1))), nil },
		Clock:      clk,
	})
	if err != nil {
		t.Fatalf("newLLMProxyHandler: %v", err)
	}
	if stop != nil {
		defer func() { _ = stop(context.Background()) }()
	}

	// Call 1: streaming OpenAI request WITHOUT stream_options.
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/llm/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req1.Header.Set("Authorization", "Bearer "+tok)
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("call 1 status = %d body=%s; want 200", rec1.Code, rec1.Body.String())
	}
	if !sawInclude.Load() {
		t.Fatal("upstream did not receive injected stream_options.include_usage=true on streaming /chat/completions")
	}

	// Verify model_usage row recorded action_id and 1000 tokens.
	rows, err := gateway.QueryModelUsageRows(context.Background(), mem, scope, []id.ExecutionID{execID})
	if err != nil {
		t.Fatalf("QueryModelUsageRows: %v", err)
	}
	if len(rows) != 1 || rows[0].ActionID != actionID || rows[0].TotalTokens != 1000 {
		t.Fatalf("recorded usage rows = %+v; want 1 row with action_id=%q total=1000", rows, actionID)
	}

	// Call 2 MUST be rejected with 429 budget_exceeded without dialing upstream!
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/llm/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"again"}]}`))
	req2.Header.Set("Authorization", "Bearer "+tok)
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("call 2 status = %d body=%s; want 429 budget_exceeded", rec2.Code, rec2.Body.String())
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d; want 1", upstreamCalls.Load())
	}

	// Unit checks on ensureOpenAIStreamIncludeUsage edge cases.
	if got := string(ensureOpenAIStreamIncludeUsage("/v1/chat/completions", []byte(`{"stream":false}`))); got != `{"stream":false}` {
		t.Fatalf("non-streaming modified: %s", got)
	}
	if got := string(ensureOpenAIStreamIncludeUsage("/v1/messages", []byte(`{"stream":true}`))); got != `{"stream":true}` {
		t.Fatalf("non-chat-completions modified: %s", got)
	}
	if got := string(ensureOpenAIStreamIncludeUsage("/v1/chat/completions", []byte(`not-json`))); got != `not-json` {
		t.Fatalf("invalid json modified: %s", got)
	}
	preserved := ensureOpenAIStreamIncludeUsage("/v1/chat/completions", []byte(`{"stream":true,"stream_options":{"foo":"bar"}}`))
	if !strings.Contains(string(preserved), `"foo":"bar"`) || !strings.Contains(string(preserved), `"include_usage":true`) {
		t.Fatalf("existing stream_options keys not preserved: %s", string(preserved))
	}
}

func TestBudgetTracker_PersistsAcrossControlPlaneRestart(t *testing.T) {
	scope := tenancy.Scope{TenantID: id.TenantID("t1"), ProjectID: id.ProjectID("p1")}
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clk := budgetTestClock{now: now}
	mem := memory.NewWithClock(clk.Now)

	execID := id.ExecutionID("exec-restart-1")
	actID := id.ActionID("act-restart-1")
	principalID := id.PrincipalID("principal-restart-1")
	seedBudgetExecution(t, mem, scope, execID, actID, principalID, "", &definition.Budget{MaxTokens: 1000}, nil)

	// Instance 1 (before control-plane restart) admits one call and persists 1000 tokens to the store.
	tracker1, err := gateway.NewBudgetTracker(gateway.BudgetConfig{}, nil, newScopedBudgetReader(mem, mem), nil)
	if err != nil {
		t.Fatalf("NewBudgetTracker 1: %v", err)
	}
	release1, err := tracker1.CheckAndAcquire(context.Background(), gateway.BudgetCheckRequest{
		Scope:       scope,
		PrincipalID: principalID,
		ExecutionID: execID,
		ActionID:    actID,
		Model:       "openai/gpt-4o",
	})
	if err != nil {
		t.Fatalf("tracker1 CheckAndAcquire: %v", err)
	}
	unit, err := (gateway.UsageRecord{
		ID:            "usage-before-restart",
		Scope:         scope,
		PrincipalID:   principalID,
		ExecutionID:   execID,
		ActionID:      actID,
		Model:         "openai/gpt-4o",
		UpstreamModel: "gpt-4o",
		Outcome:       gateway.UsageSucceeded,
		UsageKnown:    true,
		Usage:         api.ChatUsage{PromptTokens: 600, CompletionTokens: 400, TotalTokens: 1000},
		At:            now,
	}).Unit()
	if err != nil {
		t.Fatalf("UsageRecord.Unit: %v", err)
	}
	if _, err := mem.Execute(context.Background(), scope, unit); err != nil {
		t.Fatalf("persist usage: %v", err)
	}
	release1()

	// Simulate control-plane restart: brand-new BudgetTracker over the same store with zero in-memory state.
	tracker2, err := gateway.NewBudgetTracker(gateway.BudgetConfig{}, nil, newScopedBudgetReader(mem, mem), nil)
	if err != nil {
		t.Fatalf("NewBudgetTracker 2: %v", err)
	}
	_, err = tracker2.CheckAndAcquire(context.Background(), gateway.BudgetCheckRequest{
		Scope:       scope,
		PrincipalID: principalID,
		ExecutionID: execID,
		ActionID:    actID,
		Model:       "openai/gpt-4o",
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.BudgetExceeded {
		t.Fatalf("restarted tracker CheckAndAcquire err = %v; want budget_exceeded from persisted store rows", err)
	}
}

func TestLLMProxy_MultiActionAndPipelineBudgetIsolation(t *testing.T) {
	scope := tenancy.Scope{TenantID: id.TenantID("t1"), ProjectID: id.ProjectID("p1")}
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	clk := budgetTestClock{now: now}
	mem := memory.NewWithClock(clk.Now)

	ctx := context.Background()
	_, _ = mem.Execute(ctx, tenancy.Scope{TenantID: scope.TenantID}, store.Unit{
		Writes: []store.Write{{
			Table:  "tenants",
			ID:     string(scope.TenantID),
			Insert: true,
			Values: map[string]any{"name": string(scope.TenantID), "version": int64(1), "created_at": now},
		}},
	})
	_, _ = mem.Execute(ctx, scope, store.Unit{
		Writes: []store.Write{{
			Table:  "projects",
			ID:     string(scope.ProjectID),
			Insert: true,
			Values: map[string]any{"name": string(scope.ProjectID), "version": int64(1), "created_at": now},
		}},
	})

	execID := id.ExecutionID("exec-multi-act-1")
	act1 := id.ActionID("act-multi-1")
	act2 := id.ActionID("act-multi-2")
	principalID := id.PrincipalID("principal-multi-1")
	tok := "kodex.exec-multi-act-1.mac"

	snap, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:   []byte("budget:\n  max_tokens: 8000\nactions:\n  - name: step-1\n    image: reg/img@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n    commands: [\"true\"]\n  - name: step-2\n    image: reg/img@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n    commands: [\"true\"]\n"),
		Budget: &definition.Budget{MaxTokens: 8000},
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	admittedDoc := snapshotdoc.Admitted{
		Snapshot: snap,
		Actions: []pipeline.AdmittedAction{
			{
				Index:   0,
				Name:    "step-1",
				Kind:    definition.ActionKindContainer,
				Timeout: 5 * time.Minute,
				Container: &definition.Container{
					Image:    "reg/img@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					Commands: []string{"true"},
					Budget:   &definition.Budget{MaxTokens: 10000},
				},
			},
			{
				Index:   1,
				Name:    "step-2",
				Kind:    definition.ActionKindContainer,
				Timeout: 5 * time.Minute,
				Container: &definition.Container{
					Image:    "reg/img@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					Commands: []string{"true"},
					Budget:   &definition.Budget{MaxTokens: 5000},
				},
			},
		},
	}
	snapBytes, err := snapshotdoc.Encode(admittedDoc)
	if err != nil {
		t.Fatalf("snapshotdoc.Encode: %v", err)
	}
	if _, err := mem.Execute(ctx, scope, store.Unit{
		Writes: []store.Write{
			{
				Table:  "executions",
				ID:     string(execID),
				Insert: true,
				Values: map[string]any{
					"task_id":      nil,
					"principal_id": string(principalID),
					"state":        string(kernel.ExecutionRunning),
					"version":      int64(1),
					"yaml_sha256":  snap.YAMLSHA256[:],
					"snapshot":     snapBytes,
					"workspace_id": "ws-multi-1",
					"created_at":   now,
				},
			},
			{
				Table:  "actions",
				ID:     string(act1),
				Insert: true,
				Values: map[string]any{
					"execution_id":       string(execID),
					"index":              int64(0),
					"name":               "step-1",
					"kind":               string(definition.ActionKindContainer),
					"state":              string(kernel.ActionSucceeded),
					"version":            int64(1),
					"timeout_ms":         int64(300000),
					"staged_outputs_set": false,
					"launch_state":       string(kernel.LaunchIssued),
				},
			},
			{
				Table:  "actions",
				ID:     string(act2),
				Insert: true,
				Values: map[string]any{
					"execution_id":       string(execID),
					"index":              int64(1),
					"name":               "step-2",
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
		t.Fatalf("seed multi-action execution: %v", err)
	}

	// Seed Action 1 usage = 6,000 tokens.
	u1, err := (gateway.UsageRecord{
		ID:            "usage-act1",
		Scope:         scope,
		PrincipalID:   principalID,
		ExecutionID:   execID,
		ActionID:      act1,
		Model:         "openai/gpt-4o",
		UpstreamModel: "gpt-4o",
		Outcome:       gateway.UsageSucceeded,
		UsageKnown:    true,
		Usage:         api.ChatUsage{PromptTokens: 4000, CompletionTokens: 2000, TotalTokens: 6000},
		At:            now,
	}).Unit()
	if err != nil {
		t.Fatalf("UsageRecord.Unit: %v", err)
	}
	if _, err := mem.Execute(ctx, scope, u1); err != nil {
		t.Fatalf("seed act1 usage: %v", err)
	}

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","model":"gpt-4o","usage":{"prompt_tokens":2000,"completion_tokens":500,"total_tokens":2500}}`))
	}))
	defer upstream.Close()

	principal := identity.Principal{ID: principalID, Kind: identity.KindExecution, Scope: scope, Enabled: true, Version: 1, ExecutionID: execID}
	admitted := authorize.AdmittedExecution{Principal: principal, ExecutionID: execID, ActionID: act2, Permission: authorize.ExecConfiguredModels, Deadline: now.Add(10 * time.Minute)}
	cipher := newTestCipher(t)
	var usageSeq, eventSeq atomic.Int64

	handler, stop, err := newLLMProxyHandler(llmProxyDependencies{
		Config: &ModelsConfig{
			Providers: []ModelProviderConfig{{
				Scope:      scope,
				Name:       "openai",
				BaseURL:    upstream.URL,
				SecretName: llmProxyTestOpenAISecretName,
			}},
		},
		Profile:        ProfileDevelopment,
		InstallationID: "installation-1",
		Executions:     fixedExecutionReader{admitted: admitted},
		Principals:     staticPrincipalResolver{byToken: map[string]identity.Principal{tok: principal}},
		Authority:      budgetTestAllowAuthority{},
		Lookup: mapSecretLookup{
			cipher: cipher,
			values: map[id.SecretName]string{llmProxyTestOpenAISecretName: llmProxyTestOpenAISecret},
		},
		Cipher:     cipher,
		Store:      mem,
		Scanner:    mem,
		NewUsageID: func() (gateway.UsageID, error) { return gateway.UsageID(fmt.Sprintf("u-ma-%d", usageSeq.Add(1))), nil },
		NewEventID: func() (id.EventID, error) { return id.EventID(fmt.Sprintf("ev-ma-%d", eventSeq.Add(1))), nil },
		Clock:      clk,
	})
	if err != nil {
		t.Fatalf("newLLMProxyHandler: %v", err)
	}
	if stop != nil {
		defer func() { _ = stop(context.Background()) }()
	}

	// Action 2 call 1: Action 1 used 6,000, Action 2 budget is 5,000, pipeline budget is 8,000.
	// Action 2 has 0/5000 used and execution has 6000/8000 used -> ALLOWED!
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/llm/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req1.Header.Set("Authorization", "Bearer "+tok)
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("action 2 call 1 status = %d body=%s; want 200", rec1.Code, rec1.Body.String())
	}

	// Action 2 call 2: Action 2 has 2500/5000, but execution cumulative is 8500 >= 8000 -> BLOCKED by pipeline/execution budget!
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/llm/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req2.Header.Set("Authorization", "Bearer "+tok)
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("action 2 call 2 status = %d body=%s; want 429 budget_exceeded", rec2.Code, rec2.Body.String())
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d; want 1", upstreamCalls.Load())
	}
	page, err := mem.Query(ctx, scope, store.ScanQuery{
		Table:   "events",
		Equals:  map[string]any{"type": string(event.TypeModelBudgetExceeded)},
		OrderBy: []store.Order{{Column: "at"}, {Column: "id"}},
		Limit:   10,
	})
	if err != nil || len(page.Rows) != 1 {
		t.Fatalf("budget exceeded events = %d (%v); want 1", len(page.Rows), err)
	}
	var evData struct {
		ScopeKind  string `json:"scope_kind"`
		UsedTokens int64  `json:"used_tokens"`
		MaxTokens  int64  `json:"max_tokens"`
	}
	if err := json.Unmarshal(page.Rows[0].Values["data"].([]byte), &evData); err != nil {
		t.Fatalf("unmarshal event payload: %v", err)
	}
	if evData.ScopeKind != "execution" || evData.UsedTokens != 8500 || evData.MaxTokens != 8000 {
		t.Fatalf("event payload = %+v; want execution 8500/8000", evData)
	}
}
