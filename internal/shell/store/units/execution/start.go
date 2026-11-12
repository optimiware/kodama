package executionstore

import (
	"context"
	"encoding/json"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
)

// Start applies one complete task-free execution-start commit unit: the queued
// execution row, its admitted actions, the immutable snapshot document, the
// receipt, audit rows and event publication intent commit together.
func (s *Store) Start(ctx context.Context, unit execution.StartUnit) (execution.StartResult, error) {
	if err := ctx.Err(); err != nil {
		return execution.StartResult{}, err
	}
	scope, err := scopeFromOwner(unit.Owner)
	if err != nil {
		return execution.StartResult{}, err
	}
	if _, err := id.ParseExecutionID(string(unit.NewExecutionID)); err != nil {
		return execution.StartResult{}, fault.Wrap(fault.InvalidInput, "start execution id is invalid", err)
	}
	actions, err := snapshotdoc.ActionRows(unit.NewExecutionID, unit.Actions)
	if err != nil {
		return execution.StartResult{}, err
	}
	snapshot, err := snapshotdoc.Encode(snapshotdoc.Admitted{
		PipelineID:      unit.PipelineID,
		PipelineVersion: unit.ExpectedPipelineVersion,
		Snapshot:        unit.Snapshot,
		Actions:         unit.Actions,
	})
	if err != nil {
		return execution.StartResult{}, err
	}

	result := execution.StartResult{
		ExecutionID:      unit.NewExecutionID,
		ExecutionVersion: 1,
		PipelineVersion:  unit.ExpectedPipelineVersion,
	}
	record, err := resultReceipt(unit.Receipt, result)
	if err != nil {
		return execution.StartResult{}, err
	}

	digest := unit.Snapshot.YAMLSHA256
	executionValues := map[string]any{
		"pipeline_id":  nullableText(string(unit.PipelineID)),
		"task_id":      nullableText(string(unit.Snapshot.TaskID)),
		"principal_id": string(unit.ExecutionPrincipal),
		"state":        "queued",
		"version":      int64(1),
		"yaml_sha256":  append([]byte(nil), digest[:]...),
		"snapshot":     snapshot,
		"workspace_id": string(snapshotdoc.DeriveWorkspaceID(unit.NewExecutionID)),
		"created_at":   unit.At,
	}
	writes := []store.Write{{
		Table:  tableExecutions,
		ID:     string(unit.NewExecutionID),
		Insert: true,
		Values: executionValues,
	}}
	for _, action := range actions {
		writes = append(writes, store.Write{
			Table:  tableActions,
			ID:     action.ID,
			Insert: true,
			Values: action.Values,
		})
	}

	locks, err := s.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return execution.StartResult{}, err
	}
	if unit.PipelineID != "" {
		locks = append(locks, store.Lock{
			Table:         tablePipelines,
			IDs:           []string{string(unit.PipelineID)},
			ExpectVersion: unit.ExpectedPipelineVersion,
		})
	}

	committed, err := s.executor.Execute(ctx, scope, store.Unit{
		Locks:   locks,
		Writes:  writes,
		Receipt: record,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	})
	if err != nil {
		return execution.StartResult{}, err
	}
	if committed.Replayed {
		replayed, err := decodeResult[execution.StartResult](committed.Receipt, result)
		if err != nil {
			return execution.StartResult{}, err
		}
		// A receipt written before StartResult carried ExecutionVersion has no
		// such member. Every committed execution starts at version 1, so a
		// missing (zero) member decodes to the insertion invariant.
		if replayed.ExecutionVersion == 0 {
			replayed.ExecutionVersion = 1
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return result, nil
}

// resultReceipt attaches the committed domain result to the receipt so an equal
// replay returns the original result without re-applying anything.
func resultReceipt(record receipt.Record, result any) (*receipt.Record, error) {
	if record.ID == "" {
		return nil, nil
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "commit result could not be encoded", err)
	}
	record.Result = encoded
	return &record, nil
}

// decodeResult returns the original committed result from a replayed receipt.
func decodeResult[T any](record receipt.Record, fallback T) (T, error) {
	var result T
	if len(record.Result) == 0 {
		return fallback, nil
	}
	if err := json.Unmarshal(record.Result, &result); err != nil {
		return result, fault.Wrap(fault.InternalFailure, "replayed receipt result is malformed", err)
	}
	return result, nil
}
