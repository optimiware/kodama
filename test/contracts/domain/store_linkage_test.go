package domaincontract

import (
	"testing"

	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// TestBindingResourceIDPersistsThroughSchema is the cross-contract consumer
// check for the domain/adapter boundary: the domain contract declares
// resource.Binding.ResourceID and verify.BindingQuery{ResourceID, ServiceID};
// the schema must declare the resource_bindings.resource_id column, its
// composite resource foreign key and the (tenant, project, resource, service)
// lookup identity so that binding is actually persistable and queryable. A
// schema that drops or renames the link would make the domain value
// unpersistable even though the declaration compiles.
func TestBindingResourceIDPersistsThroughSchema(t *testing.T) {
	schema := store.SchemaDesign()
	bindings := schemaTable(t, schema, "resource_bindings")

	resourceID := schemaColumn(t, bindings, "resource_id")
	if resourceID.Nullable {
		t.Fatal("resource_bindings.resource_id is nullable; a binding requires a resource link")
	}

	resourceFK := schemaForeignKey(t, bindings, "resource_bindings_resource")
	assertSchemaColumns(t, "resource_bindings_resource local columns", resourceFK.Columns, []string{"tenant_id", "project_id", "resource_id"})
	assertSchemaColumns(t, "resource_bindings_resource referenced columns", resourceFK.ReferencedColumns, []string{"tenant_id", "project_id", "id"})
	if resourceFK.References != "resources" {
		t.Errorf("resource_bindings_resource references %q; want resources", resourceFK.References)
	}

	lookup := schemaUnique(t, bindings, "resource_bindings_scope_resource_service")
	assertSchemaColumns(t, "resource_bindings_scope_resource_service", lookup.Columns, []string{"tenant_id", "project_id", "resource_id", "service_id"})

	// The domain values themselves must carry the same resource/service
	// identity the schema lookup uses, and refuse a missing resource link.
	binding := resource.Binding{
		ID:               resource.BindingID("binding-1"),
		ResourceID:       resource.ResourceID("resource-1"),
		Scope:            sampleScope(),
		EnforcingService: "service-1",
		Version:          1,
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("valid binding refused: %v", err)
	}
	query := verify.BindingQuery{Scope: sampleScope(), ResourceID: binding.ResourceID, ServiceID: binding.EnforcingService}
	if err := query.Validate(); err != nil {
		t.Fatalf("valid verification binding query refused: %v", err)
	}
	if query.ResourceID != binding.ResourceID || query.ServiceID != binding.EnforcingService {
		t.Fatalf("binding query lost the resource/service identity: %+v", query)
	}
}

func schemaTable(t *testing.T, schema store.Schema, name string) store.Table {
	t.Helper()
	for _, table := range schema.Tables {
		if table.Name == name {
			return table
		}
	}
	t.Fatalf("schema has no table %q", name)
	return store.Table{}
}

func schemaColumn(t *testing.T, table store.Table, name string) store.Column {
	t.Helper()
	for _, column := range table.Columns {
		if column.Name == name {
			return column
		}
	}
	t.Fatalf("table %q has no column %q", table.Name, name)
	return store.Column{}
}

func schemaForeignKey(t *testing.T, table store.Table, name string) store.ForeignKey {
	t.Helper()
	for _, foreignKey := range table.ForeignKeys {
		if foreignKey.Name == name {
			return foreignKey
		}
	}
	t.Fatalf("table %q has no foreign key %q", table.Name, name)
	return store.ForeignKey{}
}

func schemaUnique(t *testing.T, table store.Table, name string) store.UniqueConstraint {
	t.Helper()
	for _, unique := range table.Unique {
		if unique.Name == name {
			return unique
		}
	}
	t.Fatalf("table %q has no unique constraint %q", table.Name, name)
	return store.UniqueConstraint{}
}

func assertSchemaColumns(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v; want %v", name, got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("%s = %v; want %v", name, got, want)
		}
	}
}
