package opsapp

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// TestInspectActionMapsRequestSubjectAndAssignment pins the action view: a
// request action exposes the immutable typed subject (deterministically ordered
// by field name) and the exact assignment, so a responder can decide without a
// second read. A container action exposes neither.
func TestInspectActionMapsRequestSubjectAndAssignment(t *testing.T) {
	activated := time.Unix(1_700_000_000, 0).UTC()
	deadline := activated.Add(time.Hour)
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"seed":     {Kind: definition.SubjectLiteral, Literal: "seed-1"},
		"artifact": {Kind: definition.SubjectLiteral, Literal: "built:seed-1"},
	})
	action := execution.ActionView{
		ID:          id.ActionID("act-1"),
		Index:       1,
		Name:        id.ActionName("approve"),
		Kind:        definition.ActionKindRequest,
		State:       kernel.ActionRunning,
		Version:     2,
		Timeout:     time.Hour,
		ActivatedAt: activated,
		Deadline:    deadline,
		Subject:     &subject,
		Assignment:  request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
	}

	mapped, err := inspectAction(action)
	if err != nil {
		t.Fatalf("inspectAction: %v", err)
	}
	if mapped.Subject == nil {
		t.Fatalf("mapped request action carries no subject: %+v", mapped)
	}
	wantFields := []api.SubjectField{
		{Name: "artifact", Kind: "string", Value: "built:seed-1"},
		{Name: "seed", Kind: "string", Value: "seed-1"},
	}
	if !slices.Equal(mapped.Subject.Fields, wantFields) {
		t.Errorf("mapped subject fields = %+v; want %+v", mapped.Subject.Fields, wantFields)
	}
	if mapped.Assignment != "role/reviewer" {
		t.Errorf("mapped assignment = %q; want role/reviewer", mapped.Assignment)
	}
	if mapped.Deadline == nil || !mapped.Deadline.Equal(deadline) {
		t.Errorf("mapped deadline = %v; want %s", mapped.Deadline, deadline)
	}
	if err := mapped.Validate(); err != nil {
		t.Fatalf("mapped request action refused: %v", err)
	}

	container, err := inspectAction(execution.ActionView{
		ID:      id.ActionID("act-0"),
		Index:   0,
		Name:    id.ActionName("build"),
		Kind:    definition.ActionKindContainer,
		State:   kernel.ActionSucceeded,
		Version: 2,
	})
	if err != nil {
		t.Fatalf("inspectAction container: %v", err)
	}
	if container.Subject != nil || container.Assignment != "" {
		t.Errorf("container action exposed request facts: subject=%+v assignment=%q", container.Subject, container.Assignment)
	}
}

// inspectFake answers exactly one execution.inspect read with a prepared view.
type inspectFake struct {
	view execution.View
	err  error
}

func (fake inspectFake) Start(context.Context, execution.StartCommand) (execution.StartResult, error) {
	return execution.StartResult{}, fault.New(fault.InternalFailure, "unexpected start")
}

func (fake inspectFake) Inspect(context.Context, execution.InspectCommand) (execution.View, error) {
	return fake.view, fake.err
}

func (fake inspectFake) List(context.Context, execution.ListCommand) (execution.Page, error) {
	return execution.Page{}, fault.New(fault.InternalFailure, "unexpected list")
}

func (fake inspectFake) Cancel(context.Context, execution.CancelCommand) (execution.TransitionResult, error) {
	return execution.TransitionResult{}, fault.New(fault.InternalFailure, "unexpected cancel")
}

func (fake inspectFake) StageOutputs(context.Context, execution.OutputsCommand) (execution.TransitionResult, error) {
	return execution.TransitionResult{}, fault.New(fault.InternalFailure, "unexpected stage")
}

// TestExecutionInspectHandlerFailsClosedThroughMapper pins the handler-level
// error mapping: when the action projection refuses a corrupt running request
// subject, execution.inspect must return the mapper's internal_failure class,
// never swallow the error or reclassify it as invalid_input.
func TestExecutionInspectHandlerFailsClosedThroughMapper(t *testing.T) {
	lookup := definition.NewSubject(map[string]definition.SubjectValue{
		"artifact": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "prepare", Key: "artifact"}},
	})
	empty := definition.NewSubject(nil)
	base := execution.ActionView{
		ID:         id.ActionID("act-1"),
		Index:      0,
		Name:       id.ActionName("approve"),
		Kind:       definition.ActionKindRequest,
		State:      kernel.ActionRunning,
		Version:    1,
		Assignment: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
	}
	cases := []struct {
		name    string
		subject *definition.Subject
	}{
		{"unresolved lookup", &lookup},
		{"empty subject", &empty},
		{"running request without a subject", nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			action := base
			action.Subject = testCase.subject
			deps := Dependencies{Executions: inspectFake{view: execution.View{
				ExecutionID:   id.ExecutionID("exe-1"),
				State:         kernel.ExecutionRunning,
				Version:       1,
				CurrentAction: 0,
				Actions:       []execution.ActionView{action},
				Cleanup:       kernel.Cleanup{State: kernel.CleanupNone},
			}}}
			handler := executionHandlers(deps)["execution.inspect"]
			if handler == nil {
				t.Fatal("execution.inspect has no handler")
			}
			_, err := handler.Handle(context.Background(), ops.AuthorizedCall{
				Operation: authorize.Operation{Name: "execution.inspect"},
				Targets:   map[string]string{"execution": "exe-1"},
				Request:   api.Request{Input: json.RawMessage(`{}`)},
			})
			if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
				t.Fatalf("handler fault class = %v (%v); want internal_failure", class, err)
			}
		})
	}
}

// TestInspectResultOmitsEmptyPipelineIdentity pins the task-free inline
// projection: an execution with no pipeline identity validates and encodes
// without a pipeline_id member rather than fabricating one. A stored-pipeline
// execution keeps the present member.
func TestInspectResultOmitsEmptyPipelineIdentity(t *testing.T) {
	inline := execution.View{
		ExecutionID:   id.ExecutionID("exe-inline"),
		State:         kernel.ExecutionRunning,
		Version:       1,
		CurrentAction: -1,
		Cleanup:       kernel.Cleanup{State: kernel.CleanupNone},
	}
	mapped, err := inspectResult(inline)
	if err != nil {
		t.Fatalf("inspectResult: %v", err)
	}
	if mapped.PipelineID != "" {
		t.Fatalf("inline inspect pipeline_id = %q; want the empty optional identity", mapped.PipelineID)
	}
	if err := mapped.Validate(); err != nil {
		t.Fatalf("inline inspect result refused: %v", err)
	}
	encoded, err := json.Marshal(mapped)
	if err != nil {
		t.Fatalf("marshal inline inspect result: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("unmarshal inline inspect result: %v", err)
	}
	if _, present := object["pipeline_id"]; present {
		t.Fatalf("inline inspect result carries a fabricated pipeline_id: %s", encoded)
	}

	stored := inline
	stored.PipelineID = "pipeline-1"
	storedResult, err := inspectResult(stored)
	if err != nil {
		t.Fatalf("inspectResult stored: %v", err)
	}
	if got := storedResult.PipelineID; got != "pipeline-1" {
		t.Fatalf("stored inspect pipeline_id = %q; want pipeline-1", got)
	}
}

// TestInspectActionFailsClosedOnCorruptSubject proves execution.inspect never
// exposes a partial or fabricated subject. A running request whose stored
// subject still carries a runtime lookup, whose subject is present but empty,
// or which has no stored subject at all is corruption and returns
// internal_failure. A pending request without a subject stays a legitimate,
// empty projection.
func TestInspectActionFailsClosedOnCorruptSubject(t *testing.T) {
	base := execution.ActionView{
		ID:         id.ActionID("act-1"),
		Index:      1,
		Name:       id.ActionName("approve"),
		Kind:       definition.ActionKindRequest,
		State:      kernel.ActionRunning,
		Version:    2,
		Deadline:   time.Unix(1_700_000_000, 0).UTC(),
		Assignment: request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer},
	}
	literal := definition.NewSubject(map[string]definition.SubjectValue{
		"seed": {Kind: definition.SubjectLiteral, Literal: "seed-1"},
	})
	mixed := definition.NewSubject(map[string]definition.SubjectValue{
		"seed":     {Kind: definition.SubjectLiteral, Literal: "seed-1"},
		"artifact": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "prepare", Key: "artifact"}},
	})
	lookup := definition.NewSubject(map[string]definition.SubjectValue{
		"artifact": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "prepare", Key: "artifact"}},
	})
	empty := definition.NewSubject(nil)

	// A complete literal subject is accepted.
	complete := base
	complete.Subject = &literal
	if _, err := inspectAction(complete); err != nil {
		t.Fatalf("a running request with a literal subject was refused: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*execution.ActionView)
	}{
		{"mixed literal and lookup", func(view *execution.ActionView) { view.Subject = &mixed }},
		{"lookup only", func(view *execution.ActionView) { view.Subject = &lookup }},
		{"non-nil empty subject", func(view *execution.ActionView) { view.Subject = &empty }},
		{"running request without a subject", func(view *execution.ActionView) { view.Subject = nil }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			view := base
			testCase.mutate(&view)
			_, err := inspectAction(view)
			if err == nil {
				t.Fatal("inspectAction accepted a corrupt running request subject")
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
				t.Fatalf("fault class = %v (%v); want internal_failure", class, err)
			}
		})
	}

	// A request that has not activated yet legitimately carries no subject.
	pending := base
	pending.State = kernel.ActionPending
	pending.Subject = nil
	mapped, err := inspectAction(pending)
	if err != nil {
		t.Fatalf("pending request without a subject was refused: %v", err)
	}
	if mapped.Subject != nil || mapped.Assignment != "role/reviewer" {
		t.Fatalf("pending request projection = %+v; want assignment and no subject", mapped)
	}
}
