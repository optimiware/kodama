package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

// BudgetLimits bounds total tokens and/or estimated USD spend for one scope
// (execution, action, or task). Zero values mean no limit on that dimension.
type BudgetLimits struct {
	MaxTokens  int64   `json:"max_tokens,omitempty"`
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
}

// Active reports whether at least one positive token or USD limit is set.
func (limits BudgetLimits) Active() bool {
	return limits.MaxTokens > 0 || limits.MaxCostUSD > 0
}

// Validate checks that limits are non-negative, finite, and within sane bounds.
// All-zero limits are valid and represent "unlimited".
func (limits BudgetLimits) Validate() error {
	if limits.MaxTokens < 0 {
		return fault.New(fault.InvalidInput, "budget max_tokens must not be negative")
	}
	if limits.MaxTokens > definition.MaxBudgetTokens {
		return fault.New(fault.InvalidInput, fmt.Sprintf("budget max_tokens %d exceeds maximum %d", limits.MaxTokens, definition.MaxBudgetTokens))
	}
	if math.IsNaN(limits.MaxCostUSD) || math.IsInf(limits.MaxCostUSD, 0) {
		return fault.New(fault.InvalidInput, "budget max_cost_usd must be a finite number")
	}
	if limits.MaxCostUSD < 0 {
		return fault.New(fault.InvalidInput, "budget max_cost_usd must not be negative")
	}
	if limits.MaxCostUSD > definition.MaxBudgetCostUSD {
		return fault.New(fault.InvalidInput, fmt.Sprintf("budget max_cost_usd %.2f exceeds maximum %.2f", limits.MaxCostUSD, definition.MaxBudgetCostUSD))
	}
	return nil
}

// Stricter combines two optional limits, taking the smaller positive limit on
// each dimension.
func (limits BudgetLimits) Stricter(other BudgetLimits) BudgetLimits {
	out := limits
	if other.MaxTokens > 0 && (out.MaxTokens == 0 || other.MaxTokens < out.MaxTokens) {
		out.MaxTokens = other.MaxTokens
	}
	if other.MaxCostUSD > 0 && (out.MaxCostUSD == 0 || other.MaxCostUSD < out.MaxCostUSD) {
		out.MaxCostUSD = other.MaxCostUSD
	}
	return out
}

// ModelPricing specifies USD rates per 1,000,000 input/output tokens for one
// model label (for example "gpt-4o", "anthropic/claude-sonnet-4",
// "anthropic/*", or "*" as a fallback rate).
type ModelPricing struct {
	Model                     string  `json:"model"`
	InputUSDPerMillionTokens  float64 `json:"input_usd_per_million_tokens"`
	OutputUSDPerMillionTokens float64 `json:"output_usd_per_million_tokens"`
}

// Validate checks that the pricing entry names a model and finite non-negative rates.
func (pricing ModelPricing) Validate() error {
	if strings.TrimSpace(pricing.Model) == "" {
		return fault.New(fault.InvalidInput, "model pricing entry has an empty model")
	}
	if math.IsNaN(pricing.InputUSDPerMillionTokens) || math.IsInf(pricing.InputUSDPerMillionTokens, 0) || pricing.InputUSDPerMillionTokens < 0 {
		return fault.New(fault.InvalidInput, fmt.Sprintf("model pricing %q input_usd_per_million_tokens must be finite and non-negative", pricing.Model))
	}
	if math.IsNaN(pricing.OutputUSDPerMillionTokens) || math.IsInf(pricing.OutputUSDPerMillionTokens, 0) || pricing.OutputUSDPerMillionTokens < 0 {
		return fault.New(fault.InvalidInput, fmt.Sprintf("model pricing %q output_usd_per_million_tokens must be finite and non-negative", pricing.Model))
	}
	return nil
}

// BudgetConfig configures default per-execution and per-task LLM limits plus
// optional per-model USD pricing rates used to compute cumulative spend from
// recorded model_usage rows.
type BudgetConfig struct {
	DefaultExecution BudgetLimits   `json:"default_execution,omitempty"`
	DefaultTask      BudgetLimits   `json:"default_task,omitempty"`
	Pricing          []ModelPricing `json:"pricing,omitempty"`
}

// Validate checks default limits and model pricing declarations.
func (config BudgetConfig) Validate() error {
	if err := config.DefaultExecution.Validate(); err != nil {
		return err
	}
	if err := config.DefaultTask.Validate(); err != nil {
		return err
	}
	seen := make(map[string]bool, len(config.Pricing))
	for _, entry := range config.Pricing {
		if err := entry.Validate(); err != nil {
			return err
		}
		key := strings.TrimSpace(entry.Model)
		if seen[key] {
			return fault.New(fault.InvalidInput, fmt.Sprintf("model pricing %q is declared twice", key))
		}
		seen[key] = true
	}
	return nil
}

// ExecutionBudgetContext carries the task association, sibling executions, and
// admitted pipeline/action budgets of one running execution.
type ExecutionBudgetContext struct {
	ExecutionID       id.ExecutionID
	TaskID            id.TaskID
	Pipeline          BudgetLimits
	Action            BudgetLimits
	SiblingExecutions []id.ExecutionID
}

// UsageSummaryRow is one persisted model_usage projection used for active
// budget aggregation.
type UsageSummaryRow struct {
	ExecutionID      id.ExecutionID
	ActionID         id.ActionID
	Model            string
	UpstreamModel    string
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// BudgetReader reads admitted execution/action/task budget context, task
// sibling execution IDs, and already-recorded model_usage rows within a
// verified project scope.
type BudgetReader interface {
	ReadExecutionBudget(ctx context.Context, scope tenancy.Scope, executionID id.ExecutionID, actionID id.ActionID) (ExecutionBudgetContext, error)
	ReadTaskExecutions(ctx context.Context, scope tenancy.Scope, taskID id.TaskID) ([]id.ExecutionID, error)
	ReadUsageTotals(ctx context.Context, scope tenancy.Scope, executionIDs []id.ExecutionID) ([]UsageSummaryRow, error)
}

// BudgetExceededEvent is the attributable structured event emitted when an LLM
// call is rejected by active budget enforcement before contacting an upstream
// provider.
type BudgetExceededEvent struct {
	Scope       tenancy.Scope
	PrincipalID id.PrincipalID
	ExecutionID id.ExecutionID
	ActionID    id.ActionID
	TaskID      id.TaskID
	Model       string
	ScopeKind   string // "action", "execution", or "task"
	LimitKind   string // "tokens" or "cost_usd"
	UsedTokens  int64
	MaxTokens   int64
	UsedCostUSD float64
	MaxCostUSD  float64
}

// BudgetEventRecorder persists an attributable budget_exceeded event.
type BudgetEventRecorder interface {
	RecordBudgetExceeded(ctx context.Context, ev BudgetExceededEvent) error
}

// BudgetCheckRequest identifies one inbound execution LLM call to check before
// upstream dispatch.
type BudgetCheckRequest struct {
	Scope         tenancy.Scope
	PrincipalID   id.PrincipalID
	ExecutionID   id.ExecutionID
	ActionID      id.ActionID
	Model         string
	UpstreamModel string
}

type scopedPricingKey struct {
	scope tenancy.Scope
	model string
}

type budgetGate struct {
	sem  chan struct{}
	refs int
}

// BudgetTracker enforces active LLM token and USD spend budgets across both
// the non-streaming model gateway and the streaming /llm/{provider}/* proxy.
// Concurrent calls within the same budgeted execution or task scope are
// serialized through a context-aware per-scope gate until usage is persisted,
// preventing concurrent requests from racing past the budget.
type BudgetTracker struct {
	config        BudgetConfig
	scopedPricing map[scopedPricingKey]ModelPricing
	globalPricing map[string]ModelPricing
	reader        BudgetReader
	events        BudgetEventRecorder

	mu    sync.Mutex
	gates map[string]*budgetGate
}

// NewBudgetTracker constructs one validated BudgetTracker. ModelConfig-derived
// pricing is isolated by tenancy.Scope so one tenant/project cannot overwrite
// another's rates, falling back to global config.Pricing exact/wildcard rates.
func NewBudgetTracker(config BudgetConfig, models []ModelConfig, reader BudgetReader, events BudgetEventRecorder) (*BudgetTracker, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	scopedPricing := make(map[scopedPricingKey]ModelPricing, len(models)*2)
	for _, model := range models {
		if model.InputUSDPerMillionTokens > 0 || model.OutputUSDPerMillionTokens > 0 {
			entry := ModelPricing{
				Model:                     model.Model,
				InputUSDPerMillionTokens:  model.InputUSDPerMillionTokens,
				OutputUSDPerMillionTokens: model.OutputUSDPerMillionTokens,
			}
			scopedPricing[scopedPricingKey{scope: model.Scope, model: model.Model}] = entry
			if model.UpstreamModel != "" {
				upKey := scopedPricingKey{scope: model.Scope, model: model.UpstreamModel}
				if _, exists := scopedPricing[upKey]; !exists {
					scopedPricing[upKey] = ModelPricing{
						Model:                     model.UpstreamModel,
						InputUSDPerMillionTokens:  model.InputUSDPerMillionTokens,
						OutputUSDPerMillionTokens: model.OutputUSDPerMillionTokens,
					}
				}
			}
		}
	}
	globalPricing := make(map[string]ModelPricing, len(config.Pricing))
	for _, entry := range config.Pricing {
		globalPricing[strings.TrimSpace(entry.Model)] = entry
	}
	return &BudgetTracker{
		config:        config,
		scopedPricing: scopedPricing,
		globalPricing: globalPricing,
		reader:        reader,
		events:        events,
		gates:         make(map[string]*budgetGate),
	}, nil
}

// CheckAndAcquire evaluates active action, execution, and task LLM budgets
// before dialing an upstream provider. When a budget applies, it holds a
// per-scope concurrency gate until the returned release function is invoked
// (after model_usage is persisted). Task sibling execution IDs are refreshed
// after acquiring the gate so concurrent sibling executions cannot race past
// a shared task budget. When a limit is reached or exceeded, it records an
// attributable budget_exceeded event, releases the gate immediately, and
// returns a typed fault.BudgetExceeded error.
func (tracker *BudgetTracker) CheckAndAcquire(ctx context.Context, req BudgetCheckRequest) (func(), error) {
	noop := func() {}
	if tracker == nil || req.ExecutionID == "" || tracker.reader == nil {
		return noop, nil
	}
	if err := ctx.Err(); err != nil {
		return noop, deadlineFault()
	}

	execCtx, err := tracker.reader.ReadExecutionBudget(ctx, req.Scope, req.ExecutionID, req.ActionID)
	if err != nil {
		if ctx.Err() != nil {
			return noop, deadlineFault()
		}
		if _, ok := fault.ClassOf(err); ok {
			return noop, err
		}
		return noop, fault.Wrap(fault.DependencyUnavailable, "execution budget metadata is unavailable", err)
	}

	actionLimit := BudgetLimits{}
	if req.ActionID != "" {
		actionLimit = execCtx.Action
	}
	execLimit := tracker.config.DefaultExecution.Stricter(execCtx.Pipeline)
	taskLimit := BudgetLimits{}
	if execCtx.TaskID != "" {
		taskLimit = tracker.config.DefaultTask
	}
	if !actionLimit.Active() && !execLimit.Active() && !taskLimit.Active() {
		return noop, nil
	}

	gateKey := fmt.Sprintf("exec:%s/%s/%s", req.Scope.TenantID, req.Scope.ProjectID, req.ExecutionID)
	if execCtx.TaskID != "" && taskLimit.Active() {
		gateKey = fmt.Sprintf("task:%s/%s/%s", req.Scope.TenantID, req.Scope.ProjectID, execCtx.TaskID)
	}
	release, err := tracker.acquireGate(ctx, gateKey)
	if err != nil {
		return noop, err
	}

	actionCostActive := req.ActionID != "" && actionLimit.MaxCostUSD > 0
	execCostActive := execLimit.MaxCostUSD > 0
	taskCostActive := execCtx.TaskID != "" && taskLimit.MaxCostUSD > 0
	anyCostActive := actionCostActive || execCostActive || taskCostActive
	if anyCostActive {
		if _, ok := tracker.lookupPricing(req.Scope, req.Model, req.UpstreamModel); !ok {
			release()
			return noop, fault.New(fault.UnsupportedCapability,
				fmt.Sprintf("cost budget enforcement cannot price model %q: no matching pricing rate is configured", req.Model))
		}
	}

	execIDs := []id.ExecutionID{req.ExecutionID}
	if execCtx.TaskID != "" && taskLimit.Active() {
		siblings, err := tracker.reader.ReadTaskExecutions(ctx, req.Scope, execCtx.TaskID)
		if err != nil {
			release()
			if ctx.Err() != nil {
				return noop, deadlineFault()
			}
			if _, ok := fault.ClassOf(err); ok {
				return noop, err
			}
			return noop, fault.Wrap(fault.DependencyUnavailable, "task sibling executions could not be read for budget enforcement", err)
		}
		if len(siblings) == 0 {
			siblings = execCtx.SiblingExecutions
		}
		execIDs = ensureExecutionIncluded(siblings, req.ExecutionID)
	}
	rows, err := tracker.reader.ReadUsageTotals(ctx, req.Scope, execIDs)
	if err != nil {
		release()
		if ctx.Err() != nil {
			return noop, deadlineFault()
		}
		if _, ok := fault.ClassOf(err); ok {
			return noop, err
		}
		return noop, fault.Wrap(fault.DependencyUnavailable, "model usage totals could not be read for budget enforcement", err)
	}

	var (
		actionTokens  int64
		actionCostUSD float64
		execTokens    int64
		execCostUSD   float64
		taskTokens    int64
		taskCostUSD   float64
	)
	for _, row := range rows {
		prompt := clampNonNegative(row.PromptTokens)
		completion := clampNonNegative(row.CompletionTokens)
		tokens := clampNonNegative(row.TotalTokens)
		if tokens == 0 && (prompt > 0 || completion > 0) {
			tokens = saturatingAddInt64(prompt, completion)
		}
		inExec := row.ExecutionID == req.ExecutionID
		inAction := inExec && req.ActionID != "" && row.ActionID == req.ActionID
		rowNeedsPrice := (inAction && actionCostActive) || (inExec && execCostActive) || taskCostActive

		cost, priced := tracker.costUSD(req.Scope, row.Model, row.UpstreamModel, prompt, completion)
		if !priced && rowNeedsPrice && (prompt > 0 || completion > 0) {
			release()
			return noop, fault.New(fault.UnsupportedCapability,
				fmt.Sprintf("cost budget enforcement cannot price recorded usage for model %q: no matching pricing rate is configured", row.Model))
		}

		taskTokens = saturatingAddInt64(taskTokens, tokens)
		taskCostUSD = safeAddCostUSD(taskCostUSD, cost)
		if inExec {
			execTokens = saturatingAddInt64(execTokens, tokens)
			execCostUSD = safeAddCostUSD(execCostUSD, cost)
		}
		if inAction {
			actionTokens = saturatingAddInt64(actionTokens, tokens)
			actionCostUSD = safeAddCostUSD(actionCostUSD, cost)
		}
	}

	if req.ActionID != "" && actionLimit.MaxTokens > 0 && actionTokens >= actionLimit.MaxTokens {
		ev := BudgetExceededEvent{
			Scope:       req.Scope,
			PrincipalID: req.PrincipalID,
			ExecutionID: req.ExecutionID,
			ActionID:    req.ActionID,
			TaskID:      execCtx.TaskID,
			Model:       req.Model,
			ScopeKind:   "action",
			LimitKind:   "tokens",
			UsedTokens:  actionTokens,
			MaxTokens:   actionLimit.MaxTokens,
			UsedCostUSD: actionCostUSD,
			MaxCostUSD:  actionLimit.MaxCostUSD,
		}
		tracker.emitExceeded(ctx, ev)
		release()
		return noop, fault.New(fault.BudgetExceeded,
			fmt.Sprintf("action LLM token budget exceeded (%d/%d tokens used)", actionTokens, actionLimit.MaxTokens))
	}
	if actionCostActive && costExceeded(actionCostUSD, actionLimit.MaxCostUSD) {
		ev := BudgetExceededEvent{
			Scope:       req.Scope,
			PrincipalID: req.PrincipalID,
			ExecutionID: req.ExecutionID,
			ActionID:    req.ActionID,
			TaskID:      execCtx.TaskID,
			Model:       req.Model,
			ScopeKind:   "action",
			LimitKind:   "cost_usd",
			UsedTokens:  actionTokens,
			MaxTokens:   actionLimit.MaxTokens,
			UsedCostUSD: actionCostUSD,
			MaxCostUSD:  actionLimit.MaxCostUSD,
		}
		tracker.emitExceeded(ctx, ev)
		release()
		return noop, fault.New(fault.BudgetExceeded,
			fmt.Sprintf("action LLM cost budget exceeded ($%.4f/$%.4f USD used)", finiteNonNegativeFloat(actionCostUSD), actionLimit.MaxCostUSD))
	}

	if execLimit.MaxTokens > 0 && execTokens >= execLimit.MaxTokens {
		ev := BudgetExceededEvent{
			Scope:       req.Scope,
			PrincipalID: req.PrincipalID,
			ExecutionID: req.ExecutionID,
			ActionID:    req.ActionID,
			TaskID:      execCtx.TaskID,
			Model:       req.Model,
			ScopeKind:   "execution",
			LimitKind:   "tokens",
			UsedTokens:  execTokens,
			MaxTokens:   execLimit.MaxTokens,
			UsedCostUSD: execCostUSD,
			MaxCostUSD:  execLimit.MaxCostUSD,
		}
		tracker.emitExceeded(ctx, ev)
		release()
		return noop, fault.New(fault.BudgetExceeded,
			fmt.Sprintf("execution LLM token budget exceeded (%d/%d tokens used)", execTokens, execLimit.MaxTokens))
	}
	if execCostActive && costExceeded(execCostUSD, execLimit.MaxCostUSD) {
		ev := BudgetExceededEvent{
			Scope:       req.Scope,
			PrincipalID: req.PrincipalID,
			ExecutionID: req.ExecutionID,
			ActionID:    req.ActionID,
			TaskID:      execCtx.TaskID,
			Model:       req.Model,
			ScopeKind:   "execution",
			LimitKind:   "cost_usd",
			UsedTokens:  execTokens,
			MaxTokens:   execLimit.MaxTokens,
			UsedCostUSD: execCostUSD,
			MaxCostUSD:  execLimit.MaxCostUSD,
		}
		tracker.emitExceeded(ctx, ev)
		release()
		return noop, fault.New(fault.BudgetExceeded,
			fmt.Sprintf("execution LLM cost budget exceeded ($%.4f/$%.4f USD used)", finiteNonNegativeFloat(execCostUSD), execLimit.MaxCostUSD))
	}

	if execCtx.TaskID != "" && taskLimit.MaxTokens > 0 && taskTokens >= taskLimit.MaxTokens {
		ev := BudgetExceededEvent{
			Scope:       req.Scope,
			PrincipalID: req.PrincipalID,
			ExecutionID: req.ExecutionID,
			ActionID:    req.ActionID,
			TaskID:      execCtx.TaskID,
			Model:       req.Model,
			ScopeKind:   "task",
			LimitKind:   "tokens",
			UsedTokens:  taskTokens,
			MaxTokens:   taskLimit.MaxTokens,
			UsedCostUSD: taskCostUSD,
			MaxCostUSD:  taskLimit.MaxCostUSD,
		}
		tracker.emitExceeded(ctx, ev)
		release()
		return noop, fault.New(fault.BudgetExceeded,
			fmt.Sprintf("task LLM token budget exceeded (%d/%d tokens used)", taskTokens, taskLimit.MaxTokens))
	}
	if taskCostActive && costExceeded(taskCostUSD, taskLimit.MaxCostUSD) {
		ev := BudgetExceededEvent{
			Scope:       req.Scope,
			PrincipalID: req.PrincipalID,
			ExecutionID: req.ExecutionID,
			ActionID:    req.ActionID,
			TaskID:      execCtx.TaskID,
			Model:       req.Model,
			ScopeKind:   "task",
			LimitKind:   "cost_usd",
			UsedTokens:  taskTokens,
			MaxTokens:   taskLimit.MaxTokens,
			UsedCostUSD: taskCostUSD,
			MaxCostUSD:  taskLimit.MaxCostUSD,
		}
		tracker.emitExceeded(ctx, ev)
		release()
		return noop, fault.New(fault.BudgetExceeded,
			fmt.Sprintf("task LLM cost budget exceeded ($%.4f/$%.4f USD used)", finiteNonNegativeFloat(taskCostUSD), taskLimit.MaxCostUSD))
	}

	return release, nil
}

func (tracker *BudgetTracker) acquireGate(ctx context.Context, key string) (func(), error) {
	tracker.mu.Lock()
	gate := tracker.gates[key]
	if gate == nil {
		gate = &budgetGate{sem: make(chan struct{}, 1)}
		tracker.gates[key] = gate
	}
	gate.refs++
	tracker.mu.Unlock()

	select {
	case gate.sem <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-gate.sem
				tracker.mu.Lock()
				gate.refs--
				if gate.refs == 0 {
					delete(tracker.gates, key)
				}
				tracker.mu.Unlock()
			})
		}, nil
	case <-ctx.Done():
		tracker.mu.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(tracker.gates, key)
		}
		tracker.mu.Unlock()
		return nil, deadlineFault()
	}
}

func (tracker *BudgetTracker) costUSD(scope tenancy.Scope, model, upstreamModel string, promptTokens, completionTokens int64) (float64, bool) {
	rate, ok := tracker.lookupPricing(scope, model, upstreamModel)
	if !ok {
		return 0, false
	}
	prompt := clampNonNegative(promptTokens)
	completion := clampNonNegative(completionTokens)
	cost := (float64(prompt)*rate.InputUSDPerMillionTokens + float64(completion)*rate.OutputUSDPerMillionTokens) / 1_000_000.0
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return math.MaxFloat64, true
	}
	if cost < 0 {
		return 0, true
	}
	return cost, true
}

func (tracker *BudgetTracker) lookupPricing(scope tenancy.Scope, model, upstreamModel string) (ModelPricing, bool) {
	if len(tracker.scopedPricing) > 0 {
		if model != "" {
			if rate, ok := tracker.scopedPricing[scopedPricingKey{scope: scope, model: model}]; ok {
				return rate, true
			}
		}
		if upstreamModel != "" {
			if rate, ok := tracker.scopedPricing[scopedPricingKey{scope: scope, model: upstreamModel}]; ok {
				return rate, true
			}
		}
		if scope.ProjectID != "" {
			tenantScope := tenancy.Scope{TenantID: scope.TenantID}
			if model != "" {
				if rate, ok := tracker.scopedPricing[scopedPricingKey{scope: tenantScope, model: model}]; ok {
					return rate, true
				}
			}
			if upstreamModel != "" {
				if rate, ok := tracker.scopedPricing[scopedPricingKey{scope: tenantScope, model: upstreamModel}]; ok {
					return rate, true
				}
			}
		}
	}
	if len(tracker.globalPricing) == 0 {
		return ModelPricing{}, false
	}
	if model != "" {
		if rate, ok := tracker.globalPricing[model]; ok {
			return rate, true
		}
	}
	if upstreamModel != "" {
		if rate, ok := tracker.globalPricing[upstreamModel]; ok {
			return rate, true
		}
	}
	if provider, _, hasSlash := strings.Cut(model, "/"); hasSlash && provider != "" {
		if rate, ok := tracker.globalPricing[provider+"/*"]; ok {
			return rate, true
		}
	}
	if rate, ok := tracker.globalPricing["*"]; ok {
		return rate, true
	}
	return ModelPricing{}, false
}

func clampNonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

func saturatingAddInt64(a, b int64) int64 {
	a = clampNonNegative(a)
	b = clampNonNegative(b)
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func safeAddCostUSD(a, b float64) float64 {
	if math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
		return math.MaxFloat64
	}
	if a < 0 {
		a = 0
	}
	if b < 0 {
		b = 0
	}
	sum := a + b
	if math.IsNaN(sum) || math.IsInf(sum, 0) {
		return math.MaxFloat64
	}
	return sum
}

func costExceeded(used, maxCost float64) bool {
	if maxCost <= 0 {
		return false
	}
	if math.IsNaN(used) || math.IsInf(used, 0) {
		return true
	}
	return used >= maxCost
}

func finiteNonNegativeFloat(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return math.MaxFloat64
	}
	if v < 0 {
		return 0
	}
	return v
}

func (tracker *BudgetTracker) emitExceeded(ctx context.Context, ev BudgetExceededEvent) {
	if tracker.events == nil {
		return
	}
	_ = tracker.events.RecordBudgetExceeded(context.WithoutCancel(ctx), ev)
}

func ensureExecutionIncluded(siblings []id.ExecutionID, self id.ExecutionID) []id.ExecutionID {
	seen := make(map[id.ExecutionID]bool, len(siblings)+1)
	out := make([]id.ExecutionID, 0, len(siblings)+1)
	for _, candidate := range siblings {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	if !seen[self] {
		out = append(out, self)
	}
	return out
}

// StoreBudgetReader is a minimal BudgetReader backed directly by a store.Scanner
// over model_usage. It enforces configured default execution budgets even when
// no full execution-snapshot reader is wired.
type StoreBudgetReader struct {
	Scanner store.Scanner
}

var _ BudgetReader = (*StoreBudgetReader)(nil)

func (reader *StoreBudgetReader) ReadExecutionBudget(_ context.Context, _ tenancy.Scope, executionID id.ExecutionID, _ id.ActionID) (ExecutionBudgetContext, error) {
	return ExecutionBudgetContext{
		ExecutionID:       executionID,
		SiblingExecutions: []id.ExecutionID{executionID},
	}, nil
}

func (reader *StoreBudgetReader) ReadTaskExecutions(ctx context.Context, scope tenancy.Scope, taskID id.TaskID) ([]id.ExecutionID, error) {
	if reader == nil || reader.Scanner == nil || !scope.ProjectScoped() || taskID == "" {
		return nil, nil
	}
	return QueryTaskExecutionIDs(ctx, reader.Scanner, scope, taskID)
}

func (reader *StoreBudgetReader) ReadUsageTotals(ctx context.Context, scope tenancy.Scope, executionIDs []id.ExecutionID) ([]UsageSummaryRow, error) {
	if reader == nil || reader.Scanner == nil {
		return nil, nil
	}
	return QueryModelUsageRows(ctx, reader.Scanner, scope, executionIDs)
}

// QueryTaskExecutionIDs reads all execution IDs belonging to taskID within the
// verified project scope.
func QueryTaskExecutionIDs(ctx context.Context, scanner store.Scanner, scope tenancy.Scope, taskID id.TaskID) ([]id.ExecutionID, error) {
	if scanner == nil || !scope.ProjectScoped() || taskID == "" {
		return nil, nil
	}
	var (
		after string
		out   []id.ExecutionID
	)
	for {
		page, err := scanner.Query(ctx, scope, store.ScanQuery{
			Table:   "executions",
			Equals:  map[string]any{"task_id": string(taskID)},
			OrderBy: []store.Order{{Column: "created_at"}, {Column: "id"}},
			After:   after,
			Limit:   store.MaxQueryLimit,
		})
		if err != nil {
			return nil, err
		}
		for _, row := range page.Rows {
			if rawID, ok := row.Values["id"].(string); ok && rawID != "" {
				out = append(out, id.ExecutionID(rawID))
			}
		}
		if !page.HasMore {
			break
		}
		after = page.Next
	}
	return out, nil
}

// QueryModelUsageRows reads attributed model_usage rows for the given execution
// IDs inside a verified project scope.
func QueryModelUsageRows(ctx context.Context, scanner store.Scanner, scope tenancy.Scope, executionIDs []id.ExecutionID) ([]UsageSummaryRow, error) {
	if scanner == nil || !scope.ProjectScoped() || len(executionIDs) == 0 {
		return nil, nil
	}
	var out []UsageSummaryRow
	for _, executionID := range executionIDs {
		if executionID == "" {
			continue
		}
		var after string
		for {
			page, err := scanner.Query(ctx, scope, store.ScanQuery{
				Table:   UsageTable,
				Equals:  map[string]any{"execution_id": string(executionID)},
				OrderBy: []store.Order{{Column: "recorded_at"}, {Column: "id"}},
				After:   after,
				Limit:   store.MaxQueryLimit,
			})
			if err != nil {
				return nil, err
			}
			for _, row := range page.Rows {
				actionID, _ := row.Values["action_id"].(string)
				model, _ := row.Values["model"].(string)
				upstream, _ := row.Values["upstream_model"].(string)
				prompt := rowInt64(row.Values["prompt_tokens"])
				completion := rowInt64(row.Values["completion_tokens"])
				total := rowInt64(row.Values["total_tokens"])
				out = append(out, UsageSummaryRow{
					ExecutionID:      executionID,
					ActionID:         id.ActionID(actionID),
					Model:            model,
					UpstreamModel:    upstream,
					PromptTokens:     prompt,
					CompletionTokens: completion,
					TotalTokens:      total,
				})
			}
			if !page.HasMore {
				break
			}
			after = page.Next
		}
	}
	return out, nil
}

func rowInt64(value any) int64 {
	switch v := value.(type) {
	case int64:
		return clampNonNegative(v)
	case int:
		return clampNonNegative(int64(v))
	case int32:
		return clampNonNegative(int64(v))
	case uint64:
		if v > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(v)
	case uint:
		if uint64(v) > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(v)
	default:
		return 0
	}
}

// StoreBudgetEventRecorder persists an attributable event.TypeModelBudgetExceeded
// envelope in the scoped store and optionally notifies a post-commit hook.
type StoreBudgetEventRecorder struct {
	Executor store.Executor
	Clock    Clock
	NewID    func() (id.EventID, error)
	OnEvent  func(tenancy.Scope)
}

var _ BudgetEventRecorder = (*StoreBudgetEventRecorder)(nil)

func (recorder *StoreBudgetEventRecorder) RecordBudgetExceeded(ctx context.Context, ev BudgetExceededEvent) error {
	if recorder == nil || recorder.Executor == nil || recorder.Clock == nil || recorder.NewID == nil {
		return nil
	}
	eventID, err := recorder.NewID()
	if err != nil || eventID == "" {
		return err
	}
	now := recorder.Clock.Now()
	if now.IsZero() {
		return nil
	}
	usedCost := finiteNonNegativeFloat(ev.UsedCostUSD)
	maxCost := finiteNonNegativeFloat(ev.MaxCostUSD)
	payload, err := json.Marshal(struct {
		ExecutionID string  `json:"execution_id"`
		ActionID    string  `json:"action_id,omitempty"`
		TaskID      string  `json:"task_id,omitempty"`
		Model       string  `json:"model,omitempty"`
		ScopeKind   string  `json:"scope_kind"`
		LimitKind   string  `json:"limit_kind"`
		UsedTokens  int64   `json:"used_tokens"`
		MaxTokens   int64   `json:"max_tokens,omitempty"`
		UsedCostUSD float64 `json:"used_cost_usd,omitempty"`
		MaxCostUSD  float64 `json:"max_cost_usd,omitempty"`
	}{
		ExecutionID: string(ev.ExecutionID),
		ActionID:    string(ev.ActionID),
		TaskID:      string(ev.TaskID),
		Model:       ev.Model,
		ScopeKind:   ev.ScopeKind,
		LimitKind:   ev.LimitKind,
		UsedTokens:  clampNonNegative(ev.UsedTokens),
		MaxTokens:   clampNonNegative(ev.MaxTokens),
		UsedCostUSD: usedCost,
		MaxCostUSD:  maxCost,
	})
	if err != nil {
		return err
	}
	subjectID := string(ev.ExecutionID)
	if ev.ScopeKind == "task" && ev.TaskID != "" {
		subjectID = string(ev.TaskID)
	}
	envelope := event.Envelope{
		ID:        eventID,
		Type:      event.TypeModelBudgetExceeded,
		Source:    event.Source{Kind: event.SourceShell, PrincipalID: ev.PrincipalID},
		At:        now,
		Scope:     ev.Scope,
		SubjectID: subjectID,
		Data:      payload,
	}
	if _, err := recorder.Executor.Execute(ctx, ev.Scope, store.Unit{
		Events: []event.Envelope{envelope},
	}); err != nil {
		return err
	}
	if recorder.OnEvent != nil {
		recorder.OnEvent(ev.Scope)
	}
	return nil
}
