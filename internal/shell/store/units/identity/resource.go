package identity

// Relational resource-row ownership for the binding adapter.
//
// `resource.binding.register` declares `membership` and `resource_ownership`.
// No other operation creates the `resources` row the ownership read needs, so
// the adapter replaces the register ownership read with the RegisterResourceRow
// decision rule: absence is satisfied by creating the row (identity =
// ResourceID, owner = the committing scope, version 1) in the same atomic unit
// as the binding, receipt, audit and event. The insert is the unit's
// conditional unique-id insert; when it loses a race the whole unit rolls back,
// the adapter re-reads the winner and re-applies ResolveRegisterConflict. A
// same-scope loser reuses the winner row, a foreign-scope loser gets the
// uniform not_found, and the resources-row conflict is never surfaced as
// conflict/version_conflict.
//
// update, delete, read and report keep requiring an existing row owned by
// exactly the committing scope through RequireOwnedResourceRow; absent and
// foreign are both not_found.

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// registerBindingOperation is the declared registry operation whose ownership
// read absence satisfies.
const registerBindingOperation authorize.OperationName = "resource.binding.register"

// Table and column names this adapter maps.
const (
	resourcesTable = "resources"
	bindingsTable  = "resource_bindings"
	// resourceRowKind is the stable kind of the ownership row registration
	// creates. The rule pins only the row identity, owner and version; the
	// schema's kind column is not part of the rule and carries this
	// neutral registered value.
	resourceRowKind = "resource"
)

// registerAuthorityReads removes the register operation's resource_ownership
// read from the authority lock list. The register decision rule replaces it
// (absence satisfies the read and the unit inserts the row), so a bare lock
// that requires the row to exist would wrongly refuse the first registration.
// A present read must name the binding's resource identity.
func registerAuthorityReads(reads []authorize.AuthorityRead, resourceID resource.ResourceID) ([]authorize.AuthorityRead, error) {
	remaining := make([]authorize.AuthorityRead, 0, len(reads))
	for _, read := range reads {
		if read.Check != authorize.CheckResourceOwnership {
			remaining = append(remaining, read)
			continue
		}
		if read.TargetID != "" && read.TargetID != string(resourceID) {
			return nil, fault.New(fault.InvalidInput, "resource ownership read does not name the binding resource")
		}
	}
	return remaining, nil
}

// readResourceRow reads the current resources row in the committing scope. An
// absent or out-of-scope row is (nil, nil): the adapter applies the register
// decision rule to it instead of treating absence as a read refusal.
func (a *Adapter) readResourceRow(ctx context.Context, scope tenancy.Scope, resourceID resource.ResourceID) (*resource.ResourceRow, error) {
	row, err := a.executor.Read(ctx, scope, resourcesTable, string(resourceID))
	if err != nil {
		if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
			return nil, nil
		}
		return nil, err
	}
	decoded, err := decodeResourceRow(row)
	if err != nil {
		return nil, err
	}
	return &decoded, nil
}

// decodeResourceRow rebuilds the ownership row from its committed columns.
func decodeResourceRow(row store.Row) (resource.ResourceRow, error) {
	scope, err := decodeScope(row)
	if err != nil {
		return resource.ResourceRow{}, err
	}
	identity, err := rowString(row, "id")
	if err != nil {
		return resource.ResourceRow{}, err
	}
	version, err := rowUint(row, "version")
	if err != nil {
		return resource.ResourceRow{}, err
	}
	return resource.ResourceRow{ID: resource.ResourceID(identity), Scope: scope, Version: version}, nil
}

// requireExistingResourceRow applies the existing-row rule update, delete and
// read keep using: only a row owned by exactly this scope is visible, and an
// absent or foreign row is the uniform not_found.
func (a *Adapter) requireExistingResourceRow(ctx context.Context, scope tenancy.Scope, resourceID resource.ResourceID) error {
	existing, err := a.readResourceRow(ctx, scope, resourceID)
	if err != nil {
		return err
	}
	if _, err := resource.RequireOwnedResourceRow(scope, resourceID, existing); err != nil {
		return err
	}
	return nil
}

// resourceRowWrite renders the conditional unique-id insert of the ownership
// row. The store's insert guard is the condition: an existing identity fails
// the whole unit, which the register path resolves by re-reading the winner.
func resourceRowWrite(row resource.ResourceRow, at time.Time) store.Write {
	return store.Write{
		Table:  resourcesTable,
		ID:     string(row.ID),
		Insert: true,
		Values: map[string]any{
			"kind":       resourceRowKind,
			"version":    int64(row.Version),
			"metadata":   []byte("{}"),
			"created_at": at,
		},
	}
}

// applyRegister commits one registration. It reads the current resources row,
// applies RegisterResourceRow, and adds the conditional insert to the binding
// unit when the rule reports a created row. The insert and the binding, receipt,
// audit and event commit or roll back together.
func (a *Adapter) applyRegister(ctx context.Context, unit resource.BindingUnit, commit store.Unit) (resource.BindingResult, error) {
	existing, err := a.readResourceRow(ctx, unit.Owner, unit.Binding.ResourceID)
	if err != nil {
		return resource.BindingResult{}, err
	}
	row, created, err := resource.RegisterResourceRow(unit.Owner, unit.Binding.ResourceID, existing)
	if err != nil {
		return resource.BindingResult{}, err
	}
	if !created {
		return a.executeBindingCommit(ctx, unit, commit)
	}
	withResource := commit
	withResource.Writes = append([]store.Write{resourceRowWrite(row, unit.Receipt.At)}, commit.Writes...)
	result, err := a.executor.Execute(ctx, unit.Owner, withResource)
	if err != nil {
		return a.resolveRegisterConflict(ctx, unit, commit, err)
	}
	return bindingCommitResult(unit, result)
}

// resolveRegisterConflict handles a registration unit that failed after
// attempting the conditional resources insert. A lost unique-id race rolled the
// whole unit back, so the adapter re-reads the winner row and re-applies the
// decision rule: a visible same-scope winner is reused, an absent re-read
// retries the conditional insert, and a row that stays invisible is the uniform
// foreign-scope not_found. The resources-row conflict is never surfaced.
func (a *Adapter) resolveRegisterConflict(ctx context.Context, unit resource.BindingUnit, commit store.Unit, cause error) (resource.BindingResult, error) {
	// Only a possible insert conflict is resolved here. A typed refusal such as
	// changed_idempotency_content or invalid_input is returned unchanged.
	if class, ok := fault.ClassOf(cause); ok && class != fault.VersionConflict {
		return resource.BindingResult{}, cause
	}
	existing, err := a.readResourceRow(ctx, unit.Owner, unit.Binding.ResourceID)
	if err != nil {
		return resource.BindingResult{}, err
	}
	if existing != nil {
		return a.reuseResourceRow(ctx, unit, commit, *existing)
	}

	// The resources row is invisible. Re-assert the unit's other guards before
	// treating the conflict as the resources insert: a guard that failed after
	// the caller read it (a revoked membership, a stale version) is the real
	// cause and must not be masked as a foreign resource.
	if err := a.reassertLocks(ctx, unit.Owner, commit.Locks); err != nil {
		return resource.BindingResult{}, err
	}

	// The re-read is absent: the winner either rolled back or its row is
	// outside this scope. Retry the conditional insert once instead of
	// inventing a row.
	row, created, err := resource.RegisterResourceRow(unit.Owner, unit.Binding.ResourceID, nil)
	if err != nil {
		return resource.BindingResult{}, err
	}
	if !created {
		return resource.BindingResult{}, fault.New(fault.InternalFailure, "the register decision rule refused an absent row")
	}
	retry := commit
	retry.Writes = append([]store.Write{resourceRowWrite(row, unit.Receipt.At)}, commit.Writes...)
	result, retryErr := a.executor.Execute(ctx, unit.Owner, retry)
	if retryErr == nil {
		return bindingCommitResult(unit, result)
	}
	if existing, err := a.readResourceRow(ctx, unit.Owner, unit.Binding.ResourceID); err != nil {
		return resource.BindingResult{}, err
	} else if existing != nil {
		return a.reuseResourceRow(ctx, unit, commit, *existing)
	}
	if err := a.reassertLocks(ctx, unit.Owner, commit.Locks); err != nil {
		return resource.BindingResult{}, err
	}
	if found, err := a.bindingExists(ctx, unit.Owner, unit.BindingID); err != nil {
		return resource.BindingResult{}, err
	} else if found {
		// The resources row stayed invisible, so the conflict is the binding
		// identity. Keep its version_conflict; it is not the resources row.
		return resource.BindingResult{}, cause
	}
	return resource.BindingResult{}, fault.New(fault.NotFound, "resource is not visible in this scope")
}

// reassertLocks re-validates one unit's read-only guards in a fresh transaction.
// The conflict path uses it to prove a failed registration was not caused by a
// guard that changed after the caller's read; a guard failure is returned
// unchanged instead of being reclassified as a foreign resources row.
func (a *Adapter) reassertLocks(ctx context.Context, scope tenancy.Scope, locks []store.Lock) error {
	if len(locks) == 0 {
		return nil
	}
	_, err := a.executor.Execute(ctx, scope, store.Unit{Locks: locks})
	return err
}

// reuseResourceRow re-applies ResolveRegisterConflict to a visible winner row
// and retries the binding unit without a second resources insert.
func (a *Adapter) reuseResourceRow(ctx context.Context, unit resource.BindingUnit, commit store.Unit, existing resource.ResourceRow) (resource.BindingResult, error) {
	if _, _, err := resource.ResolveRegisterConflict(unit.Owner, unit.Binding.ResourceID, existing); err != nil {
		return resource.BindingResult{}, err
	}
	return a.executeBindingCommit(ctx, unit, commit)
}

// bindingExists reports whether a binding row with this identity is visible in
// the scope. It distinguishes a duplicate binding insert from an invisible
// (foreign) resources row on the conflict path.
func (a *Adapter) bindingExists(ctx context.Context, scope tenancy.Scope, bindingID resource.BindingID) (bool, error) {
	_, err := a.executor.Read(ctx, scope, bindingsTable, string(bindingID))
	if err == nil {
		return true, nil
	}
	if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
		return false, nil
	}
	return false, err
}

// executeBindingCommit executes one already-built binding unit and decodes a
// replayed durable result.
func (a *Adapter) executeBindingCommit(ctx context.Context, unit resource.BindingUnit, commit store.Unit) (resource.BindingResult, error) {
	result, err := a.executor.Execute(ctx, unit.Owner, commit)
	if err != nil {
		return resource.BindingResult{}, err
	}
	return bindingCommitResult(unit, result)
}

// bindingCommitResult maps one committed or replayed unit result to the binding
// result the capability returns.
func bindingCommitResult(unit resource.BindingUnit, result store.UnitResult) (resource.BindingResult, error) {
	if replayed, ok, err := decodeReceiptResult[resource.BindingResult](result); ok {
		if err != nil {
			return resource.BindingResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return resource.BindingResult{Version: unit.Binding.Version}, nil
}
