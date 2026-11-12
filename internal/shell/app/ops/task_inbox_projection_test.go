package opsapp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// testAssignment parses one assignment fixture.
func testAssignment(t *testing.T, value string) request.Assignment {
	t.Helper()
	assignment, err := request.ParseAssignment(value)
	if err != nil {
		t.Fatalf("parse assignment %q: %v", value, err)
	}
	return assignment
}

// inboxFake answers exactly one task.inbox read with a prepared page.
type inboxFake struct {
	page inbox.Page
	err  error
}

func (fake inboxFake) Query(context.Context, inbox.QueryCommand) (inbox.Page, error) {
	return fake.page, fake.err
}

// TestTaskInboxHandlerFailsClosedThroughMapper pins the handler-level error
// mapping: when the item projection refuses a corrupt request subject,
// task.inbox must return the mapper's internal_failure class, never swallow the
// error or reclassify it as invalid_input.
func TestTaskInboxHandlerFailsClosedThroughMapper(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	lookup := definition.NewSubject(map[string]definition.SubjectValue{
		"artifact": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "prepare", Key: "artifact"}},
	})
	deps := Dependencies{Inbox: inboxFake{page: inbox.Page{Items: []inbox.Item{{
		Kind:        inbox.KindRequest,
		ExecutionID: id.ExecutionID("exe-1"),
		ActionID:    id.ActionID("act-1"),
		Assignment:  testAssignment(t, "role/reviewer"),
		Subject:     &lookup,
		Outputs:     []id.OutputKey{"approved"},
		Version:     2,
		At:          at,
	}}}}}
	handler := taskHandlers(deps)["task.inbox"]
	if handler == nil {
		t.Fatal("task.inbox has no handler")
	}
	_, err := handler.Handle(context.Background(), ops.AuthorizedCall{
		Operation: authorize.Operation{Name: "task.inbox"},
		Targets:   map[string]string{"project": "project-a"},
		Request:   api.Request{Input: json.RawMessage(`{}`)},
	})
	if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
		t.Fatalf("handler fault class = %v (%v); want internal_failure", class, err)
	}
}

// TestInboxItemMapsWaitingQuestionProjection pins the wire mapping: a
// waiting task item copies the question identity, body, durable references and
// task lifecycle version, keeps Version as the question version, and the mapped
// item passes public validation.
func TestInboxItemMapsWaitingQuestionProjection(t *testing.T) {
	const reference = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	at := time.Unix(1_700_000_000, 0).UTC()
	source := inbox.Item{
		Kind:             inbox.KindWaitingTask,
		TaskID:           id.TaskID("tsk-1"),
		Assignment:       testAssignment(t, "role/reviewer"),
		QuestionID:       task.QuestionID("qst-1"),
		Question:         "Which base should the merge use?",
		References:       []string{reference},
		Version:          3,
		LifecycleVersion: 9,
		At:               at,
	}

	mapped, err := inboxItem(source)
	if err != nil {
		t.Fatalf("inboxItem: %v", err)
	}
	if mapped.Kind != "waiting_task" || mapped.TaskID != "tsk-1" {
		t.Fatalf("mapped item = %+v; want the waiting task tsk-1", mapped)
	}
	if mapped.QuestionID != "qst-1" {
		t.Errorf("question_id = %q; want qst-1", mapped.QuestionID)
	}
	if mapped.Question != source.Question {
		t.Errorf("question = %q; want the source body", mapped.Question)
	}
	if len(mapped.References) != 1 || mapped.References[0] != reference {
		t.Errorf("references = %v; want [%s]", mapped.References, reference)
	}
	if mapped.Version != 3 {
		t.Errorf("version = %d; want the question version 3", mapped.Version)
	}
	if mapped.LifecycleVersion != 9 {
		t.Errorf("lifecycle_version = %d; want the task lifecycle version 9", mapped.LifecycleVersion)
	}
	if err := mapped.Validate(); err != nil {
		t.Fatalf("mapped waiting item refused: %v", err)
	}
	if err := (api.TaskInboxResult{Items: []api.InboxItem{mapped}}).Validate(); err != nil {
		t.Fatalf("mapped task.inbox result refused: %v", err)
	}
}

// TestInboxItemDropsQuestionProjectionForOtherKinds pins the "waiting_task
// only" half: a request or failed_task item never exposes the question
// projection even if the inbox observation carried it, and the mapped item
// still passes public validation.
func TestInboxItemDropsQuestionProjectionForOtherKinds(t *testing.T) {
	const reference = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	at := time.Unix(1_700_000_000, 0).UTC()
	projection := func(item *inbox.Item) {
		item.QuestionID = task.QuestionID("qst-1")
		item.Question = "why?"
		item.References = []string{reference}
		item.LifecycleVersion = 9
	}

	requestItem := inbox.Item{
		Kind:        inbox.KindRequest,
		ExecutionID: id.ExecutionID("exe-1"),
		ActionID:    id.ActionID("act-1"),
		Assignment:  testAssignment(t, "role/reviewer"),
		Subject:     literalSubject("seed", "seed-1"),
		Version:     2,
		At:          at,
	}
	projection(&requestItem)
	failedItem := inbox.Item{
		Kind:    inbox.KindFailedTask,
		TaskID:  id.TaskID("tsk-1"),
		Version: 4,
		At:      at,
	}
	projection(&failedItem)

	for name, source := range map[string]inbox.Item{"request": requestItem, "failed_task": failedItem} {
		mapped, err := inboxItem(source)
		if err != nil {
			t.Fatalf("%s inboxItem: %v", name, err)
		}
		if mapped.QuestionID != "" || mapped.Question != "" || len(mapped.References) != 0 || mapped.LifecycleVersion != 0 {
			t.Errorf("%s item exposed the waiting-only projection: %+v", name, mapped)
		}
		if mapped.Version != source.Version {
			t.Errorf("%s version = %d; want %d", name, mapped.Version, source.Version)
		}
		if err := mapped.Validate(); err != nil {
			t.Errorf("mapped %s item refused: %v", name, err)
		}
	}
}

// TestInboxItemMapsRequestProjection pins the wire mapping: a request item
// copies the immutable typed subject (declaration order is the sorted field
// name, because the stored subject is a map), the declared response output
// names in declaration order and the action deadline, and the mapped item
// passes public validation.
func TestInboxItemMapsRequestProjection(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	source := inbox.Item{
		Kind:        inbox.KindRequest,
		ExecutionID: id.ExecutionID("exe-1"),
		ActionID:    id.ActionID("act-1"),
		Assignment:  testAssignment(t, "role/reviewer"),
		Subject:     literalSubject("seed", "seed-1", "artifact", "built:seed-1"),
		Outputs:     []id.OutputKey{"approved", "decision"},
		Version:     2,
		At:          at,
	}

	mapped, err := inboxItem(source)
	if err != nil {
		t.Fatalf("inboxItem: %v", err)
	}
	if mapped.Subject == nil {
		t.Fatalf("mapped request item carries no subject: %+v", mapped)
	}
	wantFields := []api.SubjectField{
		{Name: "artifact", Kind: "string", Value: "built:seed-1"},
		{Name: "seed", Kind: "string", Value: "seed-1"},
	}
	if len(mapped.Subject.Fields) != len(wantFields) {
		t.Fatalf("mapped subject fields = %+v; want %+v", mapped.Subject.Fields, wantFields)
	}
	for index := range wantFields {
		if mapped.Subject.Fields[index] != wantFields[index] {
			t.Errorf("mapped subject field %d = %+v; want %+v", index, mapped.Subject.Fields[index], wantFields[index])
		}
	}
	if len(mapped.Outputs) != 2 || mapped.Outputs[0] != "approved" || mapped.Outputs[1] != "decision" {
		t.Errorf("mapped outputs = %v; want the declared names [approved decision]", mapped.Outputs)
	}
	if !mapped.At.Equal(at) {
		t.Errorf("mapped at = %s; want the action deadline %s", mapped.At, at)
	}
	if err := mapped.Validate(); err != nil {
		t.Fatalf("mapped request item refused: %v", err)
	}
	if err := (api.TaskInboxResult{Items: []api.InboxItem{mapped}}).Validate(); err != nil {
		t.Fatalf("mapped task.inbox result refused: %v", err)
	}
}

// TestInboxItemDropsRequestProjectionForOtherKinds proves the request
// projection is request-only: a waiting_task or failed_task observation never
// exposes the request subject or its declared outputs.
func TestInboxItemDropsRequestProjectionForOtherKinds(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	requestOnly := func(item *inbox.Item) {
		item.Subject = literalSubject("seed", "seed-1")
		item.Outputs = []id.OutputKey{"approved"}
	}
	waiting := inbox.Item{
		Kind:             inbox.KindWaitingTask,
		TaskID:           id.TaskID("tsk-1"),
		Assignment:       testAssignment(t, "role/reviewer"),
		QuestionID:       task.QuestionID("qst-1"),
		Question:         "which base?",
		Version:          1,
		LifecycleVersion: 2,
		At:               at,
	}
	requestOnly(&waiting)
	failed := inbox.Item{Kind: inbox.KindFailedTask, TaskID: id.TaskID("tsk-2"), Version: 4, At: at}
	requestOnly(&failed)

	for name, source := range map[string]inbox.Item{"waiting_task": waiting, "failed_task": failed} {
		mapped, err := inboxItem(source)
		if err != nil {
			t.Fatalf("%s inboxItem: %v", name, err)
		}
		if mapped.Subject != nil || len(mapped.Outputs) != 0 {
			t.Errorf("%s item exposed the request-only projection: %+v", name, mapped)
		}
		if err := mapped.Validate(); err != nil {
			t.Errorf("mapped %s item refused: %v", name, err)
		}
	}
}

// literalSubject builds one immutable subject of literal fields from
// name,value pairs.
func literalSubject(pairs ...string) *definition.Subject {
	fields := make(map[string]definition.SubjectValue, len(pairs)/2)
	for index := 0; index+1 < len(pairs); index += 2 {
		fields[pairs[index]] = definition.SubjectValue{Kind: definition.SubjectLiteral, Literal: pairs[index+1]}
	}
	subject := definition.NewSubject(fields)
	return &subject
}

// TestInboxItemFailsClosedOnUnresolvedSubject proves the public inbox mapping
// never drops or fabricates a request subject: an observation whose stored
// subject still carries an unresolved runtime lookup fails closed with
// internal_failure instead of exposing a partial literal set or an empty value.
func TestInboxItemFailsClosedOnUnresolvedSubject(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	base := inbox.Item{
		Kind:        inbox.KindRequest,
		ExecutionID: id.ExecutionID("exe-1"),
		ActionID:    id.ActionID("act-1"),
		Assignment:  testAssignment(t, "role/reviewer"),
		Outputs:     []id.OutputKey{"approved"},
		Version:     2,
		At:          at,
	}
	fields := map[string]definition.SubjectValue{
		"seed":     {Kind: definition.SubjectLiteral, Literal: "seed-1"},
		"artifact": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "prepare", Key: "artifact"}},
	}
	mixed := definition.NewSubject(fields)
	empty := definition.NewSubject(nil)
	for name, subject := range map[string]*definition.Subject{
		"mixed literal and lookup": &mixed,
		"non-nil empty subject":    &empty,
	} {
		t.Run(name, func(t *testing.T) {
			item := base
			item.Subject = subject
			if _, err := inboxItem(item); err == nil {
				t.Fatal("inboxItem accepted a corrupt request subject")
			} else if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
				t.Fatalf("fault class = %v (%v); want internal_failure", class, err)
			}
		})
	}
}
