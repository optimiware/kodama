// The waiting-task inbox item and the pipeline.start inputs.
//
//  1. A waiting_task inbox item exposes the outstanding question so the exact
//     assignee can answer without a second read: question_id, the question
//     body, durable references and the task lifecycle_version. Version stays
//     the QUESTION version. Request and failed_task items must not carry any
//     of the question projection.
//  2. pipeline.start accepts an exclusive stored-pipeline selection or inline
//     YAML on the task-free route POST /api/v1/projects/{project}/executions
//     (same MCP tool, oneOf input), with exact-byte storage, a 1 MiB decoded
//     YAML bound, a 2 MiB YAML-operation request envelope, and an execution
//     identity/version result.
//
// These tests are compile-time consumers and deterministic table checks. They
// never start a process, container, database, broker, Git server, agent,
// session, service or cloud call, and they never assemble a real Shell
// capability.

package publiccontract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// inboxNow is one fixed observation time so the vectors stay deterministic.
var inboxNow = time.Unix(1_700_000_000, 0).UTC()

// inboxReference is one opaque durable reference accepted by the item.
const inboxReference = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// requireInvalidInput asserts one refusal is the stable public invalid_input.
func requireInvalidInput(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was accepted; want invalid_input", what)
	}
	var publicError *api.Error
	if !errors.As(err, &publicError) {
		t.Fatalf("%s refusal is %T; want *api.Error", what, err)
	}
	if publicError.Code != api.CodeInvalidInput {
		t.Fatalf("%s refusal code = %q; want %q", what, publicError.Code, api.CodeInvalidInput)
	}
	if publicError.Message == "" {
		t.Fatalf("%s refusal has an empty message", what)
	}
}

// waitingItem is one complete waiting_task item. The question version and
// the task lifecycle version are deliberately different values.
func waitingItem() api.InboxItem {
	return api.InboxItem{
		Kind:             "waiting_task",
		TaskID:           "tsk-1",
		Assignment:       "role/reviewer",
		QuestionID:       "qst-1",
		Question:         "Which base should the merge use?",
		References:       []string{inboxReference},
		Version:          3,
		LifecycleVersion: 9,
		At:               inboxNow,
	}
}

// TestWaitingInboxItemCarriesQuestionProjection pins the waiting item: the
// question identity/body/references and the task lifecycle version are
// present, Version is the question version, and the JSON names are fixed.
func TestWaitingInboxItemCarriesQuestionProjection(t *testing.T) {
	item := waitingItem()
	if item.Version == item.LifecycleVersion {
		t.Fatalf("question version and task lifecycle version collapsed to %d; they are independent", item.Version)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("complete waiting item refused: %v", err)
	}

	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal waiting item: %v", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("unmarshal waiting item: %v", err)
	}
	for _, name := range []string{"question_id", "question", "references", "lifecycle_version"} {
		if _, ok := object[name]; !ok {
			t.Errorf("waiting item JSON has no %q member: %s", name, encoded)
		}
	}
	if got := string(object["question_id"]); got != `"qst-1"` {
		t.Errorf("question_id = %s; want \"qst-1\"", got)
	}
	if got := string(object["lifecycle_version"]); got != "9" {
		t.Errorf("lifecycle_version = %s; want 9", got)
	}

	// The task.inbox result codec must round-trip the complete item and keep
	// both independent versions.
	codec := codecFor(t, "task.inbox")
	body, err := json.Marshal(api.TaskInboxResult{Items: []api.InboxItem{item}})
	if err != nil {
		t.Fatalf("marshal task.inbox result: %v", err)
	}
	decoded, err := codec.DecodeResult(body)
	if err != nil {
		t.Fatalf("decode complete task.inbox result: %v", err)
	}
	items := decoded.(api.TaskInboxResult).Items
	if len(items) != 1 {
		t.Fatalf("decoded %d items; want 1", len(items))
	}
	if items[0].QuestionID != "qst-1" || items[0].Question == "" ||
		len(items[0].References) != 1 || items[0].LifecycleVersion != 9 {
		t.Errorf("decoded waiting item lost the question projection: %+v", items[0])
	}
	if items[0].Version != 3 {
		t.Errorf("decoded waiting item version = %d; want the question version 3", items[0].Version)
	}
}

// TestWaitingInboxItemRequiresQuestionProjection pins that a waiting task item
// without the question identity, question body or lifecycle version is refused:
// a reviewer could not answer from it.
func TestWaitingInboxItemRequiresQuestionProjection(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*api.InboxItem)
	}{
		{"no question id", func(item *api.InboxItem) { item.QuestionID = "" }},
		{"malformed question id", func(item *api.InboxItem) { item.QuestionID = "bad/question" }},
		{"no question body", func(item *api.InboxItem) { item.Question = "" }},
		{"no lifecycle version", func(item *api.InboxItem) { item.LifecycleVersion = 0 }},
		{"no question version", func(item *api.InboxItem) { item.Version = 0 }},
		{"no assignment", func(item *api.InboxItem) { item.Assignment = "" }},
		{"empty reference", func(item *api.InboxItem) { item.References = []string{""} }},
		{"no task id", func(item *api.InboxItem) { item.TaskID = "" }},
		{"malformed task id", func(item *api.InboxItem) { item.TaskID = "bad/task" }},
		{"execution identity", func(item *api.InboxItem) { item.ExecutionID = "exe-1" }},
		{"action identity", func(item *api.InboxItem) { item.ActionID = "act-1" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			item := waitingItem()
			testCase.mutate(&item)
			requireInvalidInput(t, item.Validate(), "waiting item with "+testCase.name)
		})
	}
}

// TestNonWaitingInboxItemsRefuseQuestionProjection pins the "waiting_task only"
// rule: a request or failed_task item that carries any question field or the
// task lifecycle version is refused, and a failed_task item carries no
// execution/action identity.
func TestNonWaitingInboxItemsRefuseQuestionProjection(t *testing.T) {
	requestItem := api.InboxItem{
		Kind:        "request",
		ExecutionID: "exe-1",
		ActionID:    "act-1",
		Assignment:  "role/reviewer",
		// The request item is self-sufficient: a valid request item carries
		// the immutable typed subject (and optional declared outputs). It
		// still refuses the question projection.
		Subject: &api.Subject{Fields: []api.SubjectField{{Name: "seed", Kind: "string", Value: "seed-1"}}},
		Version: 1,
		At:      inboxNow,
	}
	if err := requestItem.Validate(); err != nil {
		t.Fatalf("valid request item refused: %v", err)
	}
	failedItem := api.InboxItem{
		Kind:    "failed_task",
		TaskID:  "tsk-1",
		Version: 4,
		At:      inboxNow,
	}
	if err := failedItem.Validate(); err != nil {
		t.Fatalf("valid failed_task item refused: %v", err)
	}

	projections := []struct {
		name  string
		apply func(*api.InboxItem)
	}{
		{"question_id", func(item *api.InboxItem) { item.QuestionID = "qst-1" }},
		{"question", func(item *api.InboxItem) { item.Question = "why?" }},
		{"references", func(item *api.InboxItem) { item.References = []string{inboxReference} }},
		{"lifecycle_version", func(item *api.InboxItem) { item.LifecycleVersion = 2 }},
	}
	for _, kind := range []struct {
		name string
		base api.InboxItem
	}{
		{"request", requestItem},
		{"failed_task", failedItem},
	} {
		for _, projection := range projections {
			t.Run(kind.name+"/"+projection.name, func(t *testing.T) {
				item := kind.base
				projection.apply(&item)
				requireInvalidInput(t, item.Validate(), kind.name+" item carrying "+projection.name)
			})
		}
	}

	// A failed_task item must carry no execution or action identity.
	for _, name := range []string{"execution", "action"} {
		item := failedItem
		if name == "execution" {
			item.ExecutionID = "exe-1"
		} else {
			item.ActionID = "act-1"
		}
		requireInvalidInput(t, item.Validate(), "failed_task item carrying an "+name+" identity")
	}

	// Omitting the projection is not an error: only waiting_task items carry it.
	for _, item := range []api.InboxItem{requestItem, failedItem} {
		encoded, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal %s item: %v", item.Kind, err)
		}
		for _, name := range []string{"question_id", "question", "references", "lifecycle_version"} {
			if strings.Contains(string(encoded), `"`+name+`"`) {
				t.Errorf("%s item encoded the question projection member %q: %s", item.Kind, name, encoded)
			}
		}
	}
}

// TestInboxItemQuestionProjectionBounds pins the durable-reference bounds and
// the question identity grammar on a waiting item.
func TestInboxItemQuestionProjectionBounds(t *testing.T) {
	item := waitingItem()
	item.References = make([]string, api.MaxReferences)
	for index := range item.References {
		item.References[index] = inboxReference
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("waiting item with %d references refused: %v", api.MaxReferences, err)
	}
	item.References = append(item.References, inboxReference)
	requireInvalidInput(t, item.Validate(), "waiting item above the reference count bound")

	item = waitingItem()
	item.References = []string{strings.Repeat("r", api.MaxReferenceBytes+1)}
	requireInvalidInput(t, item.Validate(), "waiting item above the reference byte bound")

	item = waitingItem()
	item.QuestionID = strings.Repeat("q", 129)
	requireInvalidInput(t, item.Validate(), "waiting item with an oversized question id")
}

// TestWaitingInboxItemSuppliesReplyVersions pins the consumer rule: a
// role-limited reviewer takes the reply's question identity and expected
// question version from the waiting item's QuestionID and Version, and the
// task lifecycle version from the shared envelope's expected_version.
func TestWaitingInboxItemSuppliesReplyVersions(t *testing.T) {
	item := waitingItem()
	reply := api.TaskReplyRequest{
		TaskID:                  item.TaskID,
		QuestionID:              item.QuestionID,
		ExpectedQuestionVersion: item.Version,
		Answer:                  "merge into main",
	}
	if err := reply.Validate(); err != nil {
		t.Fatalf("reply built from the waiting item refused: %v", err)
	}
	if reply.QuestionID != "qst-1" || reply.ExpectedQuestionVersion != 3 {
		t.Errorf("reply question fields = %q/%d; want the item's question id/version",
			reply.QuestionID, reply.ExpectedQuestionVersion)
	}
	envelopeInput, err := json.Marshal(reply)
	if err != nil {
		t.Fatalf("marshal reply input: %v", err)
	}
	envelope := api.Request{
		Input:           envelopeInput,
		ExpectedVersion: &item.LifecycleVersion,
		IdempotencyKey:  "reply-1",
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("reply envelope with the item lifecycle version refused: %v", err)
	}
	if *envelope.ExpectedVersion != 9 {
		t.Errorf("reply envelope expected version = %d; want the item lifecycle version 9", *envelope.ExpectedVersion)
	}
	if item.Version == item.LifecycleVersion {
		t.Fatal("the item's question version and lifecycle version must stay independent")
	}
}

// TestInboxGoldenVectorCarriesQuestionProjection pins the golden task.inbox
// vector: the waiting item in the authoritative vector file carries the
// question fields, and the request and failed_task items do not.
func TestInboxGoldenVectorCarriesQuestionProjection(t *testing.T) {
	path := filepath.Join("testdata", "wire", "operations.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var file struct {
		Operations []struct {
			Operation string          `json:"operation"`
			Result    json.RawMessage `json:"result"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var result json.RawMessage
	for _, vector := range file.Operations {
		if vector.Operation == "task.inbox" {
			result = vector.Result
			break
		}
	}
	if len(result) == 0 {
		t.Fatalf("%s has no task.inbox vector", path)
	}
	codec := codecFor(t, "task.inbox")
	decoded, err := codec.DecodeResult(result)
	if err != nil {
		t.Fatalf("decode the golden task.inbox vector: %v", err)
	}
	items := decoded.(api.TaskInboxResult).Items
	if len(items) != 3 {
		t.Fatalf("task.inbox vector has %d items; want request/waiting_task/failed_task", len(items))
	}
	var waiting, request, failed *api.InboxItem
	for index := range items {
		switch items[index].Kind {
		case "waiting_task":
			waiting = &items[index]
		case "request":
			request = &items[index]
		case "failed_task":
			failed = &items[index]
		}
	}
	if waiting == nil || request == nil || failed == nil {
		t.Fatalf("task.inbox vector is missing a kind: %+v", items)
	}
	if waiting.QuestionID == "" || waiting.Question == "" || waiting.LifecycleVersion == 0 {
		t.Errorf("golden waiting item lost the question projection: %+v", *waiting)
	}
	if waiting.Version == waiting.LifecycleVersion {
		t.Errorf("golden waiting item question and lifecycle versions collapsed to %d", waiting.Version)
	}
	for name, item := range map[string]api.InboxItem{"request": *request, "failed_task": *failed} {
		if item.QuestionID != "" || item.Question != "" || len(item.References) != 0 || item.LifecycleVersion != 0 {
			t.Errorf("golden %s item carries the waiting-only projection: %+v", name, item)
		}
	}
}
