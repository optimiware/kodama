// Request-projection vectors.
//
// A `request` inbox item is self-sufficient: `inbox.Item` carries the
// immutable resolved `Subject` and the declared response `Outputs` names, and
// `At` is the action deadline. `execution.ActionView` exposes the
// immutable `Subject` and `Assignment` so a responder can decide without a
// second read. A task-free execution may report an empty `PipelineID`.
package domaincontract

import (
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/request"
)

// TestInboxItemDeclaresRequestProjection pins the exact declaration names, JSON
// tags and types of the request projection. A request item's fields are
// additive: every existing constructor keeps compiling.
func TestInboxItemDeclaresRequestProjection(t *testing.T) {
	typ := reflect.TypeOf(inbox.Item{})
	subject, ok := typ.FieldByName("Subject")
	if !ok {
		t.Fatal("inbox.Item does not declare Subject")
	}
	if subject.Tag.Get("json") != "subject,omitempty" {
		t.Errorf("inbox.Item.Subject json tag = %q; want subject,omitempty", subject.Tag.Get("json"))
	}
	if subject.Type != reflect.TypeOf((*definition.Subject)(nil)) {
		t.Errorf("inbox.Item.Subject type = %s; want *definition.Subject", subject.Type)
	}
	outputs, ok := typ.FieldByName("Outputs")
	if !ok {
		t.Fatal("inbox.Item does not declare Outputs")
	}
	if outputs.Tag.Get("json") != "outputs,omitempty" {
		t.Errorf("inbox.Item.Outputs json tag = %q; want outputs,omitempty", outputs.Tag.Get("json"))
	}
	if outputs.Type.Kind() != reflect.Slice || outputs.Type.Elem().Name() != "OutputKey" {
		t.Errorf("inbox.Item.Outputs type = %s; want []id.OutputKey", outputs.Type)
	}
	// At is the action deadline.
	at, ok := typ.FieldByName("At")
	if !ok || at.Tag.Get("json") != "at" {
		t.Errorf("inbox.Item.At = %v; want the existing deadline member at", at.Tag.Get("json"))
	}
}

// TestExecutionActionViewDeclaresSubjectAndAssignment pins the inspection half:
// ActionView exposes the immutable typed subject and the exact assignment, both
// additive to the existing view.
func TestExecutionActionViewDeclaresSubjectAndAssignment(t *testing.T) {
	typ := reflect.TypeOf(execution.ActionView{})
	subject, ok := typ.FieldByName("Subject")
	if !ok {
		t.Fatal("execution.ActionView does not declare Subject")
	}
	if subject.Type != reflect.TypeOf((*definition.Subject)(nil)) {
		t.Errorf("ActionView.Subject type = %s; want *definition.Subject", subject.Type)
	}
	assignment, ok := typ.FieldByName("Assignment")
	if !ok {
		t.Fatal("execution.ActionView does not declare Assignment")
	}
	if assignment.Type != reflect.TypeOf(request.Assignment{}) {
		t.Errorf("ActionView.Assignment type = %s; want request.Assignment", assignment.Type)
	}

	// The fields are additive: a zero view still compiles and carries no
	// fabricated facts.
	view := execution.ActionView{}
	if view.Subject != nil || view.Assignment != (request.Assignment{}) {
		t.Fatalf("zero ActionView carries request facts: %+v", view)
	}
}
