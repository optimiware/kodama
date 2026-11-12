package readers

import (
	"context"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// BindingReader is the store-neutral verify.BindingReader.
//
// The PostgreSQL adapter keeps its own binding reader; this adapter answers the
// same query over store.Scanner so the memory store serves it too. It reads the
// current binding of one calling service on one resource inside the caller's
// project scope. The binding is current evidence for one verification, never
// durable permission.
type BindingReader struct {
	scanner store.Scanner
}

// NewBindingReader builds the binding reader.
func NewBindingReader(scanner store.Scanner) (*BindingReader, error) {
	if scanner == nil {
		return nil, fault.New(fault.InvalidInput, "binding reader requires a scanner")
	}
	return &BindingReader{scanner: scanner}, nil
}

var _ verify.BindingReader = (*BindingReader)(nil)

// ReadBinding returns the current binding that authorizes one calling service
// on one resource.
func (reader *BindingReader) ReadBinding(ctx context.Context, query verify.BindingQuery) (resource.Binding, error) {
	if err := ctx.Err(); err != nil {
		return resource.Binding{}, err
	}
	if err := query.Validate(); err != nil {
		return resource.Binding{}, err
	}
	if query.Scope.TenantScoped() {
		return resource.Binding{}, fault.New(fault.InvalidInput, "a binding read requires a project scope")
	}
	page, err := reader.scanner.Query(ctx, query.Scope, store.ScanQuery{
		Table: "resource_bindings",
		Equals: map[string]any{
			"resource_id": string(query.ResourceID),
			"service_id":  string(query.ServiceID),
		},
		Limit: 1,
	})
	if err != nil {
		return resource.Binding{}, err
	}
	if len(page.Rows) == 0 {
		return resource.Binding{}, fault.New(fault.NotFound, "no binding for the requested resource and service")
	}
	binding, err := bindingFromRow(page.Rows[0])
	if err != nil {
		return resource.Binding{}, err
	}
	if binding.Scope != query.Scope || binding.ResourceID != query.ResourceID || binding.EnforcingService != query.ServiceID {
		return resource.Binding{}, fault.New(fault.InternalFailure, "stored binding does not match its lookup identity")
	}
	return binding, nil
}

// bindingFromRow rebuilds one binding value from its committed row.
func bindingFromRow(row store.Row) (resource.Binding, error) {
	identity, err := rowString(row, "id")
	if err != nil {
		return resource.Binding{}, err
	}
	tenant, err := rowString(row, "tenant_id")
	if err != nil {
		return resource.Binding{}, err
	}
	project, err := rowString(row, "project_id")
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
	originsData, err := rowBytes(row, "origins")
	if err != nil {
		return resource.Binding{}, err
	}
	origins, err := decodeStringList(originsData)
	if err != nil {
		return resource.Binding{}, err
	}
	operationsData, err := rowBytes(row, "operations")
	if err != nil {
		return resource.Binding{}, err
	}
	operationNames, err := decodeStringList(operationsData)
	if err != nil {
		return resource.Binding{}, err
	}
	operations := make([]resource.Operation, 0, len(operationNames))
	for _, name := range operationNames {
		operations = append(operations, resource.Operation(name))
	}
	reportableData, err := rowBytes(row, "reportable_events")
	if err != nil {
		return resource.Binding{}, err
	}
	reportable, err := decodeStringList(reportableData)
	if err != nil {
		return resource.Binding{}, err
	}
	metadataData, err := rowBytes(row, "metadata")
	if err != nil {
		return resource.Binding{}, err
	}
	var metadata map[string]string
	if err := decodeJSON(metadataData, &metadata); err != nil {
		return resource.Binding{}, err
	}
	version, err := rowUint(row, "version")
	if err != nil {
		return resource.Binding{}, err
	}
	return resource.Binding{
		ID:               resource.BindingID(identity),
		ResourceID:       resource.ResourceID(resourceID),
		Scope:            tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)},
		EnforcingService: service.ServiceID(serviceID),
		TrustedOrigins:   origins,
		Operations:       operations,
		ReportableEvents: reportable,
		Version:          version,
		Metadata:         metadata,
	}, nil
}
