package executionstore

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
)

// Activate applies one complete request-activation commit unit: the running
// request action with its immutable subject, assignment and persisted deadline,
// the cleared execution credential, the confirmed stop evidence, the receipt,
// audit rows and event publication intent commit together.
func (s *Store) Activate(ctx context.Context, unit request.ActivationUnit) (request.ActivationResult, error) {
	if err := ctx.Err(); err != nil {
		return request.ActivationResult{}, err
	}
	scope, err := scopeFromOwner(unit.Owner)
	if err != nil {
		return request.ActivationResult{}, err
	}
	if _, err := id.ParseExecutionID(string(unit.ExecutionID)); err != nil {
		return request.ActivationResult{}, fault.Wrap(fault.InvalidInput, "activation execution id is invalid", err)
	}
	if _, err := id.ParseActionID(string(unit.ActionID)); err != nil {
		return request.ActivationResult{}, fault.Wrap(fault.InvalidInput, "activation action id is invalid", err)
	}
	if !unit.Assignment.Valid() {
		return request.ActivationResult{}, fault.New(fault.InvalidInput, "activation assignment is invalid")
	}
	result := request.ActivationResult{
		ExecutionVersion: unit.ExpectedExecutionVersion + 1,
		ActionVersion:    unit.ExpectedActionVersion + 1,
		Deadline:         unit.Deadline,
	}
	record, err := resultReceipt(unit.Receipt, result)
	if err != nil {
		return request.ActivationResult{}, err
	}

	executionValues := map[string]any{"state": "running"}
	if unit.CredentialUpdate == nil || !unit.CredentialUpdate.Present() {
		executionValues["token_nonce"] = nil
		executionValues["token_expires_at"] = nil
	} else {
		executionValues["token_nonce"] = append([]byte(nil), unit.CredentialUpdate.Nonce...)
		executionValues["token_expires_at"] = unit.CredentialUpdate.Expiry
	}

	subject, err := encodeJSON(unit.Subject)
	if err != nil {
		return request.ActivationResult{}, err
	}
	assignment, err := snapshotdoc.EncodeAssignment(unit.Assignment)
	if err != nil {
		return request.ActivationResult{}, err
	}
	stop, err := encodeStop(unit.StopEvidence)
	if err != nil {
		return request.ActivationResult{}, err
	}
	actionValues := map[string]any{
		"state":               "running",
		"deadline":            unit.Deadline,
		"timeout_ms":          int64(unit.Timeout.Milliseconds()),
		"request_subject":     subject,
		"request_assignment":  assignment,
		"request_decision":    nil,
		"request_actor":       nil,
		"request_resolved_at": nil,
	}
	if stop != nil {
		actionValues["stop"] = stop
	}

	locks := []store.Lock{
		executionVersionLock(unit.ExecutionID, unit.ExpectedExecutionVersion),
		{Table: tableActions, IDs: []string{string(unit.ActionID)}, ExpectVersion: unit.ExpectedActionVersion},
	}
	authority, err := s.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return request.ActivationResult{}, err
	}
	locks = append(locks, authority...)

	committed, err := s.executor.Execute(ctx, scope, store.Unit{
		Locks: locks,
		Writes: []store.Write{
			{
				Table:         tableExecutions,
				ID:            string(unit.ExecutionID),
				ExpectVersion: unit.ExpectedExecutionVersion,
				Values:        executionValues,
			},
			{
				Table:         tableActions,
				ID:            string(unit.ActionID),
				ExpectVersion: unit.ExpectedActionVersion,
				Values:        actionValues,
			},
		},
		Receipt: record,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	})
	if err != nil {
		return request.ActivationResult{}, err
	}
	if committed.Replayed {
		replayed, err := decodeResult[request.ActivationResult](committed.Receipt, result)
		if err != nil {
			return request.ActivationResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return result, nil
}

// Resolve applies one complete request-resolution commit unit. It is
// receipt-first: an equal-content retry of a settled answer returns the original
// result and applies nothing, a changed content is refused, and only a miss
// applies the Kernel reduction the unit carries. First settlement additionally
// refuses a late answer through the transaction-clock deadline guard.
func (s *Store) Resolve(ctx context.Context, unit request.ResolveUnit) (request.ResolveResult, error) {
	if err := ctx.Err(); err != nil {
		return request.ResolveResult{}, err
	}
	scope, err := scopeFromOwner(unit.Owner)
	if err != nil {
		return request.ResolveResult{}, err
	}
	if _, err := id.ParseExecutionID(string(unit.ExecutionID)); err != nil {
		return request.ResolveResult{}, fault.Wrap(fault.InvalidInput, "resolve execution id is invalid", err)
	}
	if _, err := id.ParseActionID(string(unit.ActionID)); err != nil {
		return request.ResolveResult{}, fault.Wrap(fault.InvalidInput, "resolve action id is invalid", err)
	}
	if err := unit.ValidateReplay(); err != nil {
		return request.ResolveResult{}, err
	}
	result := request.ResolveResult{
		ExecutionVersion: unit.ExpectedExecutionVersion + 1,
		ActionVersion:    unit.ExpectedActionVersion + 1,
		Decision:         unit.Decision,
	}
	record, err := resultReceipt(unit.Receipt, result)
	if err != nil {
		return request.ResolveResult{}, err
	}

	// A replay unit carries no reduction: it may only return an already settled
	// answer. Completion mode is a compare-and-set, so a miss is version_conflict
	// and a changed content is refused without inserting anything.
	if unit.Commit.ExecutionID == "" {
		if record == nil {
			return request.ResolveResult{}, fault.New(fault.InvalidInput, "resolve replay unit carries no receipt")
		}
		authority, err := s.authorityLocks(ctx, unit.Authority)
		if err != nil {
			return request.ResolveResult{}, err
		}
		committed, err := s.executor.Execute(ctx, scope, store.Unit{
			Locks:         authority,
			Receipt:       record,
			ReceiptMode:   store.ReceiptComplete,
			ReceiptExpect: &store.ReceiptExpect{ContentHash: unit.Receipt.ContentHash},
		})
		if err != nil {
			return request.ResolveResult{}, err
		}
		if !committed.Replayed {
			return request.ResolveResult{}, fault.New(fault.InternalFailure, "resolve replay did not find the settled receipt")
		}
		replayed, err := decodeResult[request.ResolveResult](committed.Receipt, result)
		if err != nil {
			return request.ResolveResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}

	plan, err := planCommit(ctx, scopedSource{executor: s.executor, scanner: s.scanner, scope: scope}, unit.Commit, nil, &store.DeadlineGuard{Column: "deadline"}, false)
	if err != nil {
		return request.ResolveResult{}, err
	}
	authority, err := s.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return request.ResolveResult{}, err
	}
	plan.locks = append(plan.locks, authority...)

	committed, err := s.executor.Execute(ctx, scope, store.Unit{
		Locks:   plan.locks,
		Writes:  plan.writes,
		Receipt: record,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	})
	if err != nil {
		return request.ResolveResult{}, err
	}
	if committed.Replayed {
		replayed, err := decodeResult[request.ResolveResult](committed.Receipt, result)
		if err != nil {
			return request.ResolveResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return result, nil
}
