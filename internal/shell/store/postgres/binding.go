package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// bindingColumns is the scoped binding lookup's SELECT list. The binding
// identity used by a verified report is (resource, enforcing service) inside one
// project, which the schema makes unique.
var bindingColumns = []string{
	"id", "tenant_id", "project_id", "resource_id", "service_id",
	"origins", "operations", "reportable_events", "metadata", "version",
}

// bindingLookupSQL reads the current binding for one calling service on one
// resource. Every identity and the ownership scope travel as arguments.
const bindingLookupSQL = `SELECT id, tenant_id, project_id, resource_id, service_id, origins, operations, reportable_events, metadata, version FROM resource_bindings WHERE tenant_id = $1 AND project_id = $2 AND resource_id = $3 AND service_id = $4`

var _ verify.BindingReader = (*Store)(nil)

// ReadBinding reads the current binding that authorizes one calling service on
// one registered resource. The read happens inside a scoped transaction, and the
// decoded row is checked against that scope again so a wrong predicate cannot
// return another project's binding. The returned value is current evidence for
// one verification, never durable permission.
func (s *Store) ReadBinding(ctx context.Context, query verify.BindingQuery) (resource.Binding, error) {
	if err := query.Validate(); err != nil {
		return resource.Binding{}, err
	}
	if query.Scope.TenantScoped() {
		return resource.Binding{}, faultInvalid("a binding read requires a project scope")
	}
	tx, err := s.Begin(ctx, query.Scope)
	if err != nil {
		return resource.Binding{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	rows, err := tx.Query(ctx, store.Query{
		SQL: bindingLookupSQL,
		Args: []any{
			string(query.Scope.TenantID), string(query.Scope.ProjectID),
			string(query.ResourceID), string(query.ServiceID),
		},
	})
	if err != nil {
		return resource.Binding{}, err
	}
	defer func() { _ = rows.Close() }()

	meta, ok := lookupTable("resource_bindings")
	if !ok {
		return resource.Binding{}, faultInternal("the frozen schema has no resource_bindings table")
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return resource.Binding{}, err
		}
		return resource.Binding{}, faultNotFound(fmt.Sprintf(
			"no binding for resource %q and service %q", query.ResourceID, query.ServiceID,
		))
	}
	values, err := scanColumns(meta, bindingColumns, rows)
	if err != nil {
		return resource.Binding{}, err
	}
	if err := rows.Err(); err != nil {
		return resource.Binding{}, err
	}
	if err := rows.Close(); err != nil {
		return resource.Binding{}, err
	}

	binding, err := bindingFromValues(query.Scope, values)
	if err != nil {
		return resource.Binding{}, err
	}
	if err := binding.Validate(); err != nil {
		return resource.Binding{}, fault.Wrap(fault.InternalFailure, "durable binding row is not usable evidence", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return resource.Binding{}, err
	}
	return binding, nil
}

// bindingFromValues decodes one binding row and rechecks its ownership scope.
func bindingFromValues(scope tenancy.Scope, values map[string]any) (resource.Binding, error) {
	tenant, _ := values["tenant_id"].(string)
	project, _ := values["project_id"].(string)
	if tenant != string(scope.TenantID) {
		return resource.Binding{}, faultForbidden("binding row belongs to another tenant")
	}
	if project != string(scope.ProjectID) {
		return resource.Binding{}, faultForbidden("binding row belongs to another project")
	}

	var origins []string
	if raw, ok := values["origins"].([]byte); ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &origins); err != nil {
			return resource.Binding{}, fault.Wrap(fault.InternalFailure, "decode binding origins", err)
		}
	}
	var declaredOperations []string
	if raw, ok := values["operations"].([]byte); ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &declaredOperations); err != nil {
			return resource.Binding{}, fault.Wrap(fault.InternalFailure, "decode binding operations", err)
		}
	}
	var reportable []string
	if raw, ok := values["reportable_events"].([]byte); ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &reportable); err != nil {
			return resource.Binding{}, fault.Wrap(fault.InternalFailure, "decode binding reportable events", err)
		}
	}
	var metadata map[string]string
	if raw, ok := values["metadata"].([]byte); ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &metadata); err != nil {
			return resource.Binding{}, fault.Wrap(fault.InternalFailure, "decode binding metadata", err)
		}
	}

	operations := make([]resource.Operation, len(declaredOperations))
	for position, name := range declaredOperations {
		operations[position] = resource.Operation(name)
	}

	version, ok := values["version"].(int64)
	if !ok || version < 0 {
		return resource.Binding{}, faultInternal("binding row has no usable version")
	}
	bindingID, _ := values["id"].(string)
	resourceID, _ := values["resource_id"].(string)
	serviceID, _ := values["service_id"].(string)
	return resource.Binding{
		ID:               resource.BindingID(bindingID),
		ResourceID:       resource.ResourceID(resourceID),
		Scope:            tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)},
		EnforcingService: service.ServiceID(serviceID),
		TrustedOrigins:   origins,
		Operations:       operations,
		ReportableEvents: reportable,
		Version:          uint64(version),
		Metadata:         metadata,
	}, nil
}
