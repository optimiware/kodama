// The resource operation vocabulary: the repository-level `git.read` operation
// follows `git.ref.update` and stays outside the public operation registry.

package domaincontract

import (
	"fmt"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestResourceVocabularyOrder pins the resource operations and their
// declaration order: git.ref.update first, then git.read.
func TestResourceVocabularyOrder(t *testing.T) {
	want := []resource.Operation{resource.OperationGitRefUpdate, resource.OperationGitRead}
	operations := resource.Operations()
	if len(operations) != len(want) {
		t.Fatalf("resource.Operations() = %v; want %v", operations, want)
	}
	for index := range want {
		if operations[index] != want[index] {
			t.Fatalf("resource operation %d = %q; want %q", index, operations[index], want[index])
		}
	}
	if resource.OperationGitRead != "git.read" {
		t.Fatalf("OperationGitRead = %q; want git.read", resource.OperationGitRead)
	}

	// The returned slice stays a copy.
	operations[0] = resource.Operation("git.read")
	if got := resource.Operations(); len(got) != 2 || got[0] != resource.OperationGitRefUpdate || got[1] != resource.OperationGitRead {
		t.Fatalf("resource.Operations() exposed mutable state: %v", got)
	}

	for _, operation := range want {
		if !operation.Valid() {
			t.Errorf("resource operation %q reported invalid", operation)
		}
		parsed, err := resource.ParseOperation(string(operation))
		if err != nil || parsed != operation {
			t.Errorf("resource.ParseOperation(%q) = %q, %v", operation, parsed, err)
		}
	}
	for _, name := range []string{"", "git.ref.read", "git.ref.delete", "resource.report", "task.start"} {
		if resource.Operation(name).Valid() {
			t.Errorf("resource.Operation(%q) reported valid", name)
		}
		if _, err := resource.ParseOperation(name); err == nil {
			t.Errorf("resource.ParseOperation(%q) accepted an invalid name", name)
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Errorf("resource.ParseOperation(%q) class = %v; want invalid_input", name, class)
		}
	}
}

// TestGitReadIsNotAPublicOperationAndBindingsDeclareIt pins the disjoint
// vocabularies: the public registry has exactly the declared operations and
// never accepts git.read, while a repository binding may declare it next to
// git.ref.update.
func TestGitReadIsNotAPublicOperationAndBindingsDeclareIt(t *testing.T) {
	if len(authorize.Operations()) != 58 {
		t.Fatalf("public registry has %d operations; want 58", len(authorize.Operations()))
	}
	for _, operation := range authorize.Operations() {
		if operation.Name == "git.read" || operation.Name == "git.ref.update" {
			t.Fatalf("resource operation leaked into the public registry: %q", operation.Name)
		}
	}
	for _, name := range []string{"git.read", "git.ref.update", "git.ref.read"} {
		if authorize.OperationName(name).Valid() {
			t.Errorf("public registry accepted resource operation %q", name)
		}
	}

	binding := resource.Binding{
		ID:               resource.BindingID("binding-1"),
		ResourceID:       resource.ResourceID("resource-1"),
		Scope:            tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("project-1")},
		EnforcingService: service.ServiceID("service-1"),
		Operations:       []resource.Operation{resource.OperationGitRefUpdate, resource.OperationGitRead},
		ReportableEvents: []string{"git.ref.updated"},
		Version:          3,
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("binding declaring git.ref.update and git.read refused: %v", err)
	}

	readOnly := binding
	readOnly.Operations = []resource.Operation{resource.OperationGitRead}
	if err := readOnly.Validate(); err != nil {
		t.Fatalf("binding declaring only git.read refused: %v", err)
	}

	for _, name := range []string{"git.ref.read", "git.read.all", "git.tail"} {
		invalid := binding
		invalid.Operations = []resource.Operation{resource.OperationGitRead, resource.Operation(name)}
		if err := invalid.Validate(); err == nil {
			t.Errorf("binding accepted invalid resource operation %q", name)
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Errorf("binding invalid operation %q class = %v; want invalid_input", name, class)
		}
	}
}

// TestResourceOperationMirrorsStayInSync pins that the domain and public
// vocabularies have the same members in the same order.
func TestResourceOperationMirrorsStayInSync(t *testing.T) {
	domain := resource.Operations()
	if len(domain) != 2 {
		t.Fatalf("resource.Operations() = %v; want two operations", domain)
	}
	// The public mirror is checked in the public contract tests; this message
	// keeps the domain failure self-explanatory.
	if fmt.Sprint(domain[0]) != "git.ref.update" || fmt.Sprint(domain[1]) != "git.read" {
		t.Fatalf("resource vocabulary order = %v; want [git.ref.update git.read]", domain)
	}
}
