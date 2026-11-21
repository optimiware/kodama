package app

import (
	"context"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/shell/gateway"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

// scopedBudgetReader resolves admitted pipeline/action LLM budgets, task
// sibling executions, and persisted model_usage rows strictly inside the
// execution's verified project scope.
type scopedBudgetReader struct {
	executor store.Executor
	scanner  store.Scanner
}

var _ gateway.BudgetReader = (*scopedBudgetReader)(nil)

func newScopedBudgetReader(executor store.Executor, scanner store.Scanner) *scopedBudgetReader {
	if scanner == nil && executor != nil {
		if candidate, ok := executor.(store.Scanner); ok {
			scanner = candidate
		}
	}
	return &scopedBudgetReader{
		executor: executor,
		scanner:  scanner,
	}
}

func (reader *scopedBudgetReader) ReadExecutionBudget(
	ctx context.Context,
	scope tenancy.Scope,
	executionID id.ExecutionID,
	actionID id.ActionID,
) (gateway.ExecutionBudgetContext, error) {
	result := gateway.ExecutionBudgetContext{
		ExecutionID:       executionID,
		SiblingExecutions: []id.ExecutionID{executionID},
	}
	if reader == nil || reader.executor == nil || !scope.ProjectScoped() || executionID == "" {
		return result, nil
	}
	row, err := reader.executor.Read(ctx, scope, "executions", string(executionID))
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
			// Unit tests may use a synthetic ExecutionReader without an
			// executions row; default gateway/proxy budgets still enforce.
			return result, nil
		}
		return gateway.ExecutionBudgetContext{}, err
	}

	if rawTaskID, ok := row.Values["task_id"].(string); ok && rawTaskID != "" {
		result.TaskID = id.TaskID(rawTaskID)
	}

	if snapBytes, ok := row.Values["snapshot"].([]byte); ok && len(snapBytes) > 0 {
		admitted, decErr := snapshotdoc.Decode(snapBytes)
		if decErr == nil {
			if result.TaskID == "" && admitted.Snapshot.TaskID != "" {
				result.TaskID = admitted.Snapshot.TaskID
			}
			if admitted.Snapshot.Budget != nil {
				result.Pipeline = budgetLimitsFromDefinition(admitted.Snapshot.Budget)
			}
			if actionID != "" {
				actionIdx, hasIdx := reader.actionIndex(ctx, scope, actionID)
				if hasIdx && actionIdx >= 0 && actionIdx < len(admitted.Actions) {
					admittedAction := admitted.Actions[actionIdx]
					if admittedAction.Container != nil && admittedAction.Container.Budget != nil {
						result.Action = budgetLimitsFromDefinition(admittedAction.Container.Budget)
					}
				}
			}
		}
	}

	if result.TaskID != "" && reader.scanner != nil {
		siblings, err := reader.taskExecutionIDs(ctx, scope, result.TaskID)
		if err != nil {
			return gateway.ExecutionBudgetContext{}, err
		}
		if len(siblings) > 0 {
			result.SiblingExecutions = siblings
		}
	}
	return result, nil
}

func (reader *scopedBudgetReader) actionIndex(ctx context.Context, scope tenancy.Scope, actionID id.ActionID) (int, bool) {
	row, err := reader.executor.Read(ctx, scope, "actions", string(actionID))
	if err != nil {
		return 0, false
	}
	switch v := row.Values["index"].(type) {
	case int64:
		return int(v), true
	case int:
		return v, true
	default:
		return 0, false
	}
}

func (reader *scopedBudgetReader) taskExecutionIDs(ctx context.Context, scope tenancy.Scope, taskID id.TaskID) ([]id.ExecutionID, error) {
	var (
		after string
		out   []id.ExecutionID
	)
	for {
		page, err := reader.scanner.Query(ctx, scope, store.ScanQuery{
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

func (reader *scopedBudgetReader) ReadUsageTotals(
	ctx context.Context,
	scope tenancy.Scope,
	executionIDs []id.ExecutionID,
) ([]gateway.UsageSummaryRow, error) {
	if reader == nil || reader.scanner == nil {
		return nil, nil
	}
	return gateway.QueryModelUsageRows(ctx, reader.scanner, scope, executionIDs)
}

func budgetLimitsFromDefinition(b *definition.Budget) gateway.BudgetLimits {
	if b == nil {
		return gateway.BudgetLimits{}
	}
	return gateway.BudgetLimits{
		MaxTokens:  b.MaxTokens,
		MaxCostUSD: b.MaxCostUSD,
	}
}

func newSharedBudgetTracker(
	config *ModelsConfig,
	executor store.Executor,
	scanner store.Scanner,
	clock Clock,
	newEventID func() (id.EventID, error),
	onEvent func(tenancy.Scope),
) (*gateway.BudgetTracker, error) {
	if config == nil {
		return nil, nil
	}
	reader := newScopedBudgetReader(executor, scanner)
	var recorder gateway.BudgetEventRecorder
	if executor != nil && clock != nil && newEventID != nil {
		recorder = &gateway.StoreBudgetEventRecorder{
			Executor: executor,
			Clock:    clock,
			NewID:    newEventID,
			OnEvent:  onEvent,
		}
	}
	return gateway.NewBudgetTracker(config.Budgets, config.Models, reader, recorder)
}
