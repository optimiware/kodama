package identity

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
)

// Compile-time pins: every resource binding port is implemented by one adapter.
var (
	_ resource.BindingPort       = (*Adapter)(nil)
	_ resource.BindingDeletePort = (*Adapter)(nil)
	_ resource.BindingReadPort   = (*Adapter)(nil)
	_ resource.BindingQueryPort  = (*Adapter)(nil)
)

// Apply registers a new binding or replaces one binding value under its
// expected version, committing its receipt, audit rows and publication intent
// with the binding row.
func (a *Adapter) Apply(ctx context.Context, unit resource.BindingUnit) (resource.BindingResult, error) {
	if err := unit.Owner.Validate(); err != nil {
		return resource.BindingResult{}, err
	}
	if err := unit.Binding.Validate(); err != nil {
		return resource.BindingResult{}, err
	}
	if unit.Binding.ID != unit.BindingID {
		return resource.BindingResult{}, fault.New(fault.InvalidInput, "binding unit identity does not match the binding value")
	}
	if unit.Binding.Scope != unit.Owner {
		return resource.BindingResult{}, fault.New(fault.InvalidInput, "binding unit scope does not match the binding value")
	}
	if unit.ExpectedVersion == 0 {
		if unit.Binding.Version != 1 {
			return resource.BindingResult{}, fault.New(fault.InvalidInput, "binding registration must start at version 1")
		}
	} else if err := requireUnitVersion(unit.ExpectedVersion, unit.Binding.Version); err != nil {
		return resource.BindingResult{}, err
	}
	values, err := bindingValues(unit.Binding)
	if err != nil {
		return resource.BindingResult{}, err
	}
	// Registration replaces the resource_ownership read with the
	// create-if-absent decision rule; the unit itself carries the conditional
	// resources-row insert. Update keeps the read (and the existing-row rule).
	register := unit.ExpectedVersion == 0
	authority := unit.Authority
	if register {
		authority, err = registerAuthorityReads(unit.Authority, unit.Binding.ResourceID)
		if err != nil {
			return resource.BindingResult{}, err
		}
	}
	locks, err := a.authorityLocks(ctx, authority)
	if err != nil {
		return resource.BindingResult{}, err
	}
	write := store.Write{
		Table:         bindingsTable,
		ID:            string(unit.BindingID),
		ExpectVersion: unit.ExpectedVersion,
		Values:        values,
	}
	if register {
		if err := requireCommitTime(unit.Receipt.At); err != nil {
			return resource.BindingResult{}, err
		}
		write.Insert = true
		write.ExpectVersion = 0
		write.Values["version"] = int64(unit.Binding.Version)
		write.Values["created_at"] = unit.Receipt.At
	} else {
		locks = append(locks, store.Lock{
			Table:         bindingsTable,
			IDs:           []string{string(unit.BindingID)},
			ExpectVersion: unit.ExpectedVersion,
		})
	}
	commit := store.Unit{
		Locks:   locks,
		Writes:  []store.Write{write},
		Receipt: &unit.Receipt,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	}
	if register {
		return a.applyRegister(ctx, unit, commit)
	}
	if err := a.requireExistingResourceRow(ctx, unit.Owner, unit.Binding.ResourceID); err != nil {
		return resource.BindingResult{}, err
	}
	return a.executeBindingCommit(ctx, unit, commit)
}

// DeleteBinding removes one binding under its expected version, committing the
// removal with its receipt, audit rows and publication intent.
func (a *Adapter) DeleteBinding(ctx context.Context, unit resource.BindingDeleteUnit) (resource.BindingDeleteResult, error) {
	if err := unit.Owner.Validate(); err != nil {
		return resource.BindingDeleteResult{}, err
	}
	if _, err := resource.ParseBindingID(string(unit.BindingID)); err != nil {
		return resource.BindingDeleteResult{}, err
	}
	if unit.ExpectedVersion == 0 {
		return resource.BindingDeleteResult{}, fault.New(fault.InvalidInput, "binding deletion expected version is zero")
	}
	if err := requireCommitTime(unit.At); err != nil {
		return resource.BindingDeleteResult{}, err
	}
	locks, err := a.authorityLocks(ctx, unit.Authority)
	if err != nil {
		return resource.BindingDeleteResult{}, err
	}
	locks = append(locks, store.Lock{
		Table:         bindingsTable,
		IDs:           []string{string(unit.BindingID)},
		ExpectVersion: unit.ExpectedVersion,
	})
	// Delete requires an existing resources row owned by exactly this scope.
	// The binding row names the resource identity. A missing binding keeps its
	// versioned-delete refusal: there is no resource identity to check, and the
	// unit's binding_version guard reports it.
	bindingRow, err := a.executor.Read(ctx, unit.Owner, bindingsTable, string(unit.BindingID))
	if err == nil {
		binding, decodeErr := decodeBinding(bindingRow)
		if decodeErr != nil {
			return resource.BindingDeleteResult{}, decodeErr
		}
		if err := a.requireExistingResourceRow(ctx, unit.Owner, binding.ResourceID); err != nil {
			return resource.BindingDeleteResult{}, err
		}
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		return resource.BindingDeleteResult{}, err
	}
	commit := store.Unit{
		Locks: locks,
		Writes: []store.Write{{
			Table:         bindingsTable,
			ID:            string(unit.BindingID),
			Delete:        true,
			ExpectVersion: unit.ExpectedVersion,
		}},
		Receipt: &unit.Receipt,
		Audit:   append([]event.Audit(nil), unit.Audit...),
		Events:  append([]event.Envelope(nil), unit.Events...),
	}
	result, err := a.executor.Execute(ctx, unit.Owner, commit)
	if err != nil {
		return resource.BindingDeleteResult{}, err
	}
	if replayed, ok, err := decodeReceiptResult[resource.BindingDeleteResult](result); ok {
		if err != nil {
			return resource.BindingDeleteResult{}, err
		}
		replayed.Replayed = true
		return replayed, nil
	}
	return resource.BindingDeleteResult{Version: unit.ExpectedVersion + 1}, nil
}

// ReadBinding returns one binding by its stable identity inside the caller's
// scope. The adapter applies the scope at read time; a cross-scope identity is
// not_found.
func (a *Adapter) ReadBinding(ctx context.Context, request resource.BindingReadRequest) (resource.Binding, error) {
	if err := request.Validate(); err != nil {
		return resource.Binding{}, err
	}
	row, err := a.executor.Read(ctx, request.Scope, bindingsTable, string(request.BindingID))
	if err != nil {
		return resource.Binding{}, err
	}
	binding, err := decodeBinding(row)
	if err != nil {
		return resource.Binding{}, err
	}
	// Read requires an existing resources row owned by exactly this scope;
	// absent and foreign are the same not_found class.
	if err := a.requireExistingResourceRow(ctx, request.Scope, binding.ResourceID); err != nil {
		return resource.Binding{}, err
	}
	return binding, nil
}

// QueryBindings returns one stably paginated binding page inside the caller's
// scope.
func (a *Adapter) QueryBindings(ctx context.Context, query resource.BindingQuery) (resource.BindingPage, error) {
	if err := query.Validate(); err != nil {
		return resource.BindingPage{}, err
	}
	equals := map[string]any{}
	if query.EnforcingService != "" {
		equals["service_id"] = string(query.EnforcingService)
	}
	if query.ResourceID != "" {
		equals["resource_id"] = string(query.ResourceID)
	}
	page, err := a.scanner.Query(ctx, query.Scope, store.ScanQuery{
		Table:   "resource_bindings",
		Equals:  equals,
		OrderBy: []store.Order{{Column: "id"}},
		After:   query.Cursor,
		Limit:   query.Limit,
	})
	if err != nil {
		return resource.BindingPage{}, err
	}
	bindings := make([]resource.Binding, 0, len(page.Rows))
	for _, row := range page.Rows {
		binding, err := decodeBinding(row)
		if err != nil {
			return resource.BindingPage{}, err
		}
		bindings = append(bindings, binding)
	}
	return resource.BindingPage{Bindings: bindings, NextCursor: page.Next}, nil
}

// bindingValues renders the writable binding columns.
func bindingValues(binding resource.Binding) (map[string]any, error) {
	origins, err := encodeJSON(binding.TrustedOrigins)
	if err != nil {
		return nil, err
	}
	operations := make([]string, 0, len(binding.Operations))
	for _, operation := range binding.Operations {
		operations = append(operations, string(operation))
	}
	encodedOperations, err := encodeJSON(operations)
	if err != nil {
		return nil, err
	}
	reportable, err := encodeJSON(binding.ReportableEvents)
	if err != nil {
		return nil, err
	}
	metadata, err := encodeJSON(binding.Metadata)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"resource_id":       string(binding.ResourceID),
		"service_id":        string(binding.EnforcingService),
		"origins":           origins,
		"operations":        encodedOperations,
		"reportable_events": reportable,
		"metadata":          metadata,
	}, nil
}

// decodeBinding rebuilds one binding value from its committed row.
func decodeBinding(row store.Row) (resource.Binding, error) {
	scope, err := decodeScope(row)
	if err != nil {
		return resource.Binding{}, err
	}
	identity, err := rowString(row, "id")
	if err != nil {
		return resource.Binding{}, err
	}
	resourceID, err := rowString(row, "resource_id")
	if err != nil {
		return resource.Binding{}, err
	}
	serviceID, err := rowString(row, "service_id")
	if err != nil {
		return resource.Binding{}, err
	}
	origins, err := decodeStringList(row, "origins")
	if err != nil {
		return resource.Binding{}, err
	}
	operations, err := decodeOperations(row)
	if err != nil {
		return resource.Binding{}, err
	}
	reportable, err := decodeStringList(row, "reportable_events")
	if err != nil {
		return resource.Binding{}, err
	}
	metadata, err := decodeMetadata(row)
	if err != nil {
		return resource.Binding{}, err
	}
	version, err := rowUint(row, "version")
	if err != nil {
		return resource.Binding{}, err
	}
	return resource.Binding{
		ID:               resource.BindingID(identity),
		ResourceID:       resource.ResourceID(resourceID),
		Scope:            scope,
		EnforcingService: service.ServiceID(serviceID),
		TrustedOrigins:   origins,
		Operations:       operations,
		ReportableEvents: reportable,
		Version:          version,
		Metadata:         metadata,
	}, nil
}

// decodeStringList decodes one JSON string-array column.
func decodeStringList(row store.Row, name string) ([]string, error) {
	data, err := rowBytes(row, name)
	if err != nil {
		return nil, err
	}
	var values []string
	if err := decodeJSON(data, &values); err != nil {
		return nil, err
	}
	return values, nil
}

// decodeOperations decodes the declared resource-operation column.
func decodeOperations(row store.Row) ([]resource.Operation, error) {
	names, err := decodeStringList(row, "operations")
	if err != nil {
		return nil, err
	}
	operations := make([]resource.Operation, 0, len(names))
	for _, name := range names {
		operations = append(operations, resource.Operation(name))
	}
	return operations, nil
}

// decodeMetadata decodes the bounded metadata object column.
func decodeMetadata(row store.Row) (map[string]string, error) {
	data, err := rowBytes(row, "metadata")
	if err != nil {
		return nil, err
	}
	var metadata map[string]string
	if err := decodeJSON(data, &metadata); err != nil {
		return nil, err
	}
	return metadata, nil
}
