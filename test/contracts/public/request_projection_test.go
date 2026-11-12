// Request-projection vectors.
//
// A `request` inbox item is self-sufficient: it carries the immutable typed
// `subject`, the declared response `outputs` names and the action deadline
// (`at`). `execution.inspect` exposes the action view's `subject` and
// `assignment`, and a task-free inline execution carries an empty optional
// `pipeline_id` (omitted from the wire) rather than a fabricated one.
package publiccontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// frozenCodec returns the strict codec of one registered operation.
func frozenCodec(t *testing.T, operation string) api.OperationCodec {
	t.Helper()
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == operation {
			return codec
		}
	}
	t.Fatalf("operation %q has no frozen codec", operation)
	return api.OperationCodec{}
}

// requestItemSubject is one complete typed subject with two fields.
func requestItemSubject() api.Subject {
	return api.Subject{Fields: []api.SubjectField{
		{Name: "artifact", Kind: "string", Value: "built:seed-1"},
		{Name: "seed", Kind: "string", Value: "seed-1"},
	}}
}

// requestInboxVector returns one complete request item result body.
func requestInboxVector() []byte {
	return []byte(`{
		"items": [{
			"kind": "request",
			"execution_id": "exe-1",
			"action_id": "act-1",
			"assignment": "role/reviewer",
			"subject": {"fields": [
				{"name": "artifact", "kind": "string", "value": "built:seed-1"},
				{"name": "seed", "kind": "string", "value": "seed-1"}
			]},
			"outputs": ["approved", "decision"],
			"version": 2,
			"at": "2026-01-02T03:04:05Z"
		}]
	}`)
}

// TestRequestInboxItemWireCarriesSubjectAndOutputs pins the exact wire names
// and shape of the self-sufficient request item and the strict codec round
// trip. Unknown members are refused at every level.
func TestRequestInboxItemWireCarriesSubjectAndOutputs(t *testing.T) {
	codec := frozenCodec(t, "task.inbox")
	value, err := codec.DecodeResult(requestInboxVector())
	if err != nil {
		t.Fatalf("decode request inbox result: %v", err)
	}
	result := value.(api.TaskInboxResult)
	if len(result.Items) != 1 {
		t.Fatalf("decoded %d items; want 1", len(result.Items))
	}
	item := result.Items[0]
	if item.Subject == nil {
		t.Fatalf("request item carries no subject: %+v", item)
	}
	if want := requestItemSubject(); !reflect.DeepEqual(*item.Subject, want) {
		t.Fatalf("request item subject = %+v; want %+v", *item.Subject, want)
	}
	if !reflect.DeepEqual(item.Outputs, []string{"approved", "decision"}) {
		t.Fatalf("request item outputs = %v; want the declared names [approved decision]", item.Outputs)
	}

	// The strict result codec round-trips the exact members.
	encoded, err := codec.EncodeResult(result)
	if err != nil {
		t.Fatalf("encode request inbox result: %v", err)
	}
	var decoded, reencoded any
	if err := json.Unmarshal(requestInboxVector(), &decoded); err != nil {
		t.Fatalf("unmarshal the source vector: %v", err)
	}
	if err := json.Unmarshal(encoded, &reencoded); err != nil {
		t.Fatalf("unmarshal the re-encoded result: %v", err)
	}
	if !reflect.DeepEqual(decoded, reencoded) {
		t.Fatalf("task.inbox codec round trip changed the request item:\nwant=%s\ngot =%s", requestInboxVector(), encoded)
	}

	// An unknown item member and an unknown nested subject member are refused.
	withExtraMember := []byte(`{"items": [{"kind":"request","execution_id":"exe-1","action_id":"act-1","assignment":"role/reviewer","subject":{"fields":[{"name":"seed","kind":"string","value":"seed-1"}]},"outputs":["approved"],"version":2,"at":"2026-01-02T03:04:05Z","note":"x"}]}`)
	if _, err := codec.DecodeResult(withExtraMember); err == nil {
		t.Fatal("task.inbox accepted an unknown request item member")
	}
	withExtraSubjectMember := []byte(`{"items": [{"kind":"request","execution_id":"exe-1","action_id":"act-1","assignment":"role/reviewer","subject":{"fields":[{"name":"seed","kind":"string","value":"seed-1","extra":true}]},"outputs":["approved"],"version":2,"at":"2026-01-02T03:04:05Z"}]}`)
	if _, err := codec.DecodeResult(withExtraSubjectMember); err == nil {
		t.Fatal("task.inbox accepted an unknown nested subject member")
	}
}

// TestRequestInboxItemValidationRequiresImmutableSubject pins that a request
// item is self-sufficient: the subject is present and typed, the declared
// outputs are bounded names, and the request-only projection is refused on the
// other kinds.
func TestRequestInboxItemValidationRequiresImmutableSubject(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	subject := requestItemSubject()
	valid := api.InboxItem{
		Kind:        "request",
		ExecutionID: "exe-1",
		ActionID:    "act-1",
		Assignment:  "role/reviewer",
		Subject:     &subject,
		Outputs:     []string{"approved", "decision"},
		Version:     2,
		At:          at,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("complete request item refused: %v", err)
	}

	empty := api.Subject{}
	duplicate := api.Subject{Fields: []api.SubjectField{
		{Name: "seed", Kind: "string", Value: "a"},
		{Name: "seed", Kind: "string", Value: "b"},
	}}
	unknownKind := api.Subject{Fields: []api.SubjectField{{Name: "seed", Kind: "token", Value: "a"}}}
	cases := []struct {
		name   string
		mutate func(*api.InboxItem)
	}{
		{"request without a subject", func(item *api.InboxItem) { item.Subject = nil }},
		{"request with an empty subject", func(item *api.InboxItem) { item.Subject = &empty }},
		{"request with a duplicate subject field", func(item *api.InboxItem) { item.Subject = &duplicate }},
		{"request with an unknown subject kind", func(item *api.InboxItem) { item.Subject = &unknownKind }},
		{"request with an empty output name", func(item *api.InboxItem) { item.Outputs = []string{""} }},
		{"request with a malformed assignment", func(item *api.InboxItem) { item.Assignment = "reviewer" }},
		{"request with an empty assignment", func(item *api.InboxItem) { item.Assignment = "" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			item := valid
			testCase.mutate(&item)
			requireInvalid(t, item.Validate(), testCase.name)
		})
	}

	// The request projection is kind-owned: a waiting_task or failed_task item
	// never carries the request subject or its declared outputs.
	waiting := api.InboxItem{
		Kind:             "waiting_task",
		TaskID:           "tsk-1",
		Assignment:       "role/reviewer",
		QuestionID:       "qst-1",
		Question:         "which base?",
		Version:          1,
		LifecycleVersion: 2,
		At:               at,
	}
	failed := api.InboxItem{Kind: "failed_task", TaskID: "tsk-2", Version: 4, At: at}
	for name, base := range map[string]api.InboxItem{"waiting_task": waiting, "failed_task": failed} {
		withSubject := base
		subjectCopy := requestItemSubject()
		withSubject.Subject = &subjectCopy
		requireInvalid(t, withSubject.Validate(), name+" item carrying a request subject")
		withOutputs := base
		withOutputs.Outputs = []string{"approved"}
		requireInvalid(t, withOutputs.Validate(), name+" item carrying request outputs")
	}
}

// TestExecutionProjectionOptionalPipelineID pins the optional pipeline_id: a
// task-free inline execution omits `pipeline_id` instead of fabricating one, a
// stored-pipeline execution still carries it, and the strict decoder refuses an
// explicit empty string rather than treating it as absent.
func TestExecutionProjectionOptionalPipelineID(t *testing.T) {
	inspect := frozenCodec(t, "execution.inspect")
	inline := []byte(`{"execution_id":"exe-1","state":"running","version":1,"current_action":-1,"actions":[],"cleanup":{"state":"none"}}`)
	value, err := inspect.DecodeResult(inline)
	if err != nil {
		t.Fatalf("decode inline execution.inspect result: %v", err)
	}
	if got := value.(api.ExecutionInspectResult).PipelineID; got != "" {
		t.Fatalf("inline inspect pipeline_id = %q; want the omitted optional identity", got)
	}

	encoded, err := inspect.EncodeResult(value)
	if err != nil {
		t.Fatalf("encode inline execution.inspect result: %v", err)
	}
	if bytes.Contains(encoded, []byte(`"pipeline_id"`)) {
		t.Fatalf("inline inspect result carries a fabricated pipeline_id: %s", encoded)
	}

	stored := []byte(`{"execution_id":"exe-1","pipeline_id":"pipe-1","state":"running","version":1,"current_action":-1,"actions":[],"cleanup":{"state":"none"}}`)
	value, err = inspect.DecodeResult(stored)
	if err != nil {
		t.Fatalf("decode stored execution.inspect result: %v", err)
	}
	if got := value.(api.ExecutionInspectResult).PipelineID; got != "pipe-1" {
		t.Fatalf("stored inspect pipeline_id = %q; want pipe-1", got)
	}

	empty := []byte(`{"execution_id":"exe-1","pipeline_id":"","state":"running","version":1,"current_action":-1,"actions":[],"cleanup":{"state":"none"}}`)
	if _, err := inspect.DecodeResult(empty); err == nil {
		t.Fatal("execution.inspect accepted an explicit empty pipeline_id")
	}

	list := frozenCodec(t, "execution.list")
	inlineList := []byte(`{"executions":[{"execution_id":"exe-1","state":"running","version":1}]}`)
	value, err = list.DecodeResult(inlineList)
	if err != nil {
		t.Fatalf("decode inline execution.list result: %v", err)
	}
	summaries := value.(api.ExecutionListResult).Executions
	if len(summaries) != 1 || summaries[0].PipelineID != "" {
		t.Fatalf("inline execution.list summaries = %+v; want an empty optional pipeline_id", summaries)
	}
	storedList := []byte(`{"executions":[{"execution_id":"exe-1","pipeline_id":"pipe-1","state":"running","version":1}]}`)
	if _, err := list.DecodeResult(storedList); err != nil {
		t.Fatalf("stored execution.list result refused: %v", err)
	}
	emptyList := []byte(`{"executions":[{"execution_id":"exe-1","pipeline_id":"","state":"running","version":1}]}`)
	if _, err := list.DecodeResult(emptyList); err == nil {
		t.Fatal("execution.list accepted an explicit empty pipeline_id")
	}
}

// TestExecutionProjectionNullPipelineIDDecodesAsOmitted pins that a JSON
// `null` for `pipeline_id` decodes as the omitted optional identity on both
// execution.inspect and execution.list. The decoded value equals the omitted
// form and re-encoding omission drops the member instead of fabricating or
// preserving a null.
func TestExecutionProjectionNullPipelineIDDecodesAsOmitted(t *testing.T) {
	inspect := frozenCodec(t, "execution.inspect")
	omittedInspect := []byte(`{"execution_id":"exe-1","state":"running","version":1,"current_action":-1,"actions":[],"cleanup":{"state":"none"}}`)
	nullInspect := []byte(`{"execution_id":"exe-1","pipeline_id":null,"state":"running","version":1,"current_action":-1,"actions":[],"cleanup":{"state":"none"}}`)
	omittedValue, err := inspect.DecodeResult(omittedInspect)
	if err != nil {
		t.Fatalf("decode omitted execution.inspect result: %v", err)
	}
	nullValue, err := inspect.DecodeResult(nullInspect)
	if err != nil {
		t.Fatalf("decode null pipeline_id execution.inspect result: %v", err)
	}
	if !reflect.DeepEqual(omittedValue, nullValue) {
		t.Fatalf("null pipeline_id inspect decoded as %+v; want the omitted form %+v", nullValue, omittedValue)
	}
	if got := nullValue.(api.ExecutionInspectResult).PipelineID; got != "" {
		t.Fatalf("null pipeline_id inspect = %q; want the omitted optional identity", got)
	}
	encoded, err := inspect.EncodeResult(nullValue)
	if err != nil {
		t.Fatalf("encode null pipeline_id execution.inspect result: %v", err)
	}
	if bytes.Contains(encoded, []byte(`"pipeline_id"`)) {
		t.Fatalf("re-encoded null pipeline_id inspect carried the member: %s", encoded)
	}

	list := frozenCodec(t, "execution.list")
	omittedList := []byte(`{"executions":[{"execution_id":"exe-1","state":"running","version":1}]}`)
	nullList := []byte(`{"executions":[{"execution_id":"exe-1","pipeline_id":null,"state":"running","version":1}]}`)
	omittedSummaries, err := list.DecodeResult(omittedList)
	if err != nil {
		t.Fatalf("decode omitted execution.list result: %v", err)
	}
	nullSummaries, err := list.DecodeResult(nullList)
	if err != nil {
		t.Fatalf("decode null pipeline_id execution.list result: %v", err)
	}
	if !reflect.DeepEqual(omittedSummaries, nullSummaries) {
		t.Fatalf("null pipeline_id list decoded as %+v; want the omitted form %+v", nullSummaries, omittedSummaries)
	}
	if got := nullSummaries.(api.ExecutionListResult).Executions[0].PipelineID; got != "" {
		t.Fatalf("null pipeline_id summary = %q; want the omitted optional identity", got)
	}
	encoded, err = list.EncodeResult(nullSummaries)
	if err != nil {
		t.Fatalf("encode null pipeline_id execution.list result: %v", err)
	}
	if bytes.Contains(encoded, []byte(`"pipeline_id"`)) {
		t.Fatalf("re-encoded null pipeline_id list carried the member: %s", encoded)
	}
}

// TestInboxItemNullSubjectOnNonRequestKindsDecodesAsOmitted pins that a JSON
// `null` for `subject` decodes as the omitted optional subject for the
// non-request kinds: a waiting_task and failed_task item omit the member on
// decode and on re-encode, so null is never a fabricated subject.
func TestInboxItemNullSubjectOnNonRequestKindsDecodesAsOmitted(t *testing.T) {
	codec := frozenCodec(t, "task.inbox")
	vectors := map[string][]byte{
		"waiting_task": []byte(`{"items":[{"kind":"waiting_task","task_id":"tsk-1","assignment":"role/reviewer","question_id":"qst-1","question":"which base?","version":2,"lifecycle_version":3,"at":"2026-01-02T03:04:05Z","subject":null}]}`),
		"failed_task":  []byte(`{"items":[{"kind":"failed_task","task_id":"tsk-2","version":4,"at":"2026-01-02T03:04:05Z","subject":null}]}`),
	}
	for name, body := range vectors {
		t.Run(name, func(t *testing.T) {
			value, err := codec.DecodeResult(body)
			if err != nil {
				t.Fatalf("decode %s result with a null subject: %v", name, err)
			}
			result := value.(api.TaskInboxResult)
			if len(result.Items) != 1 {
				t.Fatalf("decoded %d items; want 1", len(result.Items))
			}
			if result.Items[0].Subject != nil {
				t.Fatalf("null subject decoded as %+v; want the omitted projection", result.Items[0].Subject)
			}
			encoded, err := codec.EncodeResult(result)
			if err != nil {
				t.Fatalf("encode %s result: %v", name, err)
			}
			if bytes.Contains(encoded, []byte(`"subject"`)) {
				t.Fatalf("%s re-encode carried a subject member: %s", name, encoded)
			}
		})
	}
}

// TestExecutionActionWireCarriesSubjectAndAssignment pins the action view half
// of the projection: a request action exposes the typed subject and the exact
// assignment beside the deadline, while a container action carries neither
// member.
func TestExecutionActionWireCarriesSubjectAndAssignment(t *testing.T) {
	codec := frozenCodec(t, "execution.inspect")
	body := []byte(`{
		"execution_id": "exe-1",
		"pipeline_id": "pipe-1",
		"state": "running",
		"version": 3,
		"current_action": 1,
		"actions": [
			{"action_id":"act-0","index":0,"name":"build","kind":"container","state":"succeeded","version":2},
			{
				"action_id": "act-1",
				"index": 1,
				"name": "approve",
				"kind": "request",
				"state": "running",
				"version": 2,
				"timeout_seconds": 3600,
				"deadline": "2026-01-02T04:04:05Z",
				"subject": {"fields": [{"name": "seed", "kind": "string", "value": "seed-1"}]},
				"assignment": "role/reviewer"
			}
		],
		"cleanup": {"state": "none"}
	}`)
	value, err := codec.DecodeResult(body)
	if err != nil {
		t.Fatalf("decode execution.inspect result: %v", err)
	}
	result := value.(api.ExecutionInspectResult)
	if len(result.Actions) != 2 {
		t.Fatalf("decoded %d actions; want 2", len(result.Actions))
	}
	requestAction := result.Actions[1]
	if requestAction.Subject == nil || len(requestAction.Subject.Fields) != 1 ||
		requestAction.Subject.Fields[0] != (api.SubjectField{Name: "seed", Kind: "string", Value: "seed-1"}) {
		t.Fatalf("request action subject = %+v; want the typed seed field", requestAction.Subject)
	}
	if requestAction.Assignment != "role/reviewer" {
		t.Fatalf("request action assignment = %q; want role/reviewer", requestAction.Assignment)
	}
	if container := result.Actions[0]; container.Subject != nil || container.Assignment != "" {
		t.Fatalf("container action carries request facts: subject=%+v assignment=%q", container.Subject, container.Assignment)
	}

	encoded, err := codec.EncodeResult(result)
	if err != nil {
		t.Fatalf("encode execution.inspect result: %v", err)
	}
	var decoded, reencoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal the source body: %v", err)
	}
	if err := json.Unmarshal(encoded, &reencoded); err != nil {
		t.Fatalf("unmarshal the re-encoded body: %v", err)
	}
	if !reflect.DeepEqual(decoded, reencoded) {
		t.Fatalf("execution.inspect codec round trip changed the action view:\nwant=%s\ngot =%s", body, encoded)
	}
}

// TestRequestInboxItemPinsOutputBoundAndGrammar pins the bounded declared
// response names: the count is bounded (the bound is accepted, bound+1 is
// refused) and each name is the exact lowercase declared-key grammar.
func TestRequestInboxItemPinsOutputBoundAndGrammar(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	subject := requestItemSubject()
	base := api.InboxItem{
		Kind:        "request",
		ExecutionID: "exe-1",
		ActionID:    "act-1",
		Assignment:  "role/reviewer",
		Subject:     &subject,
		Version:     2,
		At:          at,
	}

	// The count bound is accepted exactly and bound+1 is refused.
	atBound := make([]string, api.MaxSubjectFields)
	for index := range atBound {
		atBound[index] = fmt.Sprintf("out%d", index)
	}
	item := base
	item.Outputs = atBound
	if err := item.Validate(); err != nil {
		t.Fatalf("request item with %d declared outputs refused: %v", len(atBound), err)
	}
	overBound := append(append([]string(nil), atBound...), "extra")
	item.Outputs = overBound
	requireInvalid(t, item.Validate(), "request item with bound+1 declared outputs")

	// Exactly the lowercase declared-key grammar is accepted.
	for _, valid := range []string{"a", "approved2", "a_b", "a-b", "a1-b_2"} {
		item := base
		item.Outputs = []string{valid}
		if err := item.Validate(); err != nil {
			t.Errorf("request item with valid output name %q refused: %v", valid, err)
		}
	}

	// Uppercase, empty, over-long and otherwise malformed names are refused.
	invalidNames := []string{
		"Approved",
		"APPROVED",
		"",
		strings.Repeat("a", api.MaxSubjectFields+65),
		"bad name",
		"bad/name",
		"1bad",
		"_bad",
		"-bad",
		"bad.",
		"bad:name",
	}
	for _, invalidName := range invalidNames {
		item := base
		item.Outputs = []string{invalidName}
		requireInvalid(t, item.Validate(), fmt.Sprintf("request item with invalid output name %q", invalidName))
	}
}

// TestExecutionPipelineIDWhenPresentIsValidated pins that a present optional
// pipeline_id is a bounded identity on both execution.inspect and
// execution.list: an invalid present value is refused, a valid one is accepted,
// and the strict result codec applies the same rule.
func TestExecutionPipelineIDWhenPresentIsValidated(t *testing.T) {
	invalidIDs := []string{
		strings.Repeat("a", 129),
		"bad/id",
		"has space",
		".",
		"..",
		"bad:name",
		"bad.name",
	}
	for _, invalidID := range invalidIDs {
		inspect := api.ExecutionInspectResult{
			ExecutionID:   "exe-1",
			PipelineID:    invalidID,
			State:         "running",
			Version:       1,
			CurrentAction: -1,
			Cleanup:       api.ExecutionCleanup{State: "none"},
		}
		requireInvalid(t, inspect.Validate(), fmt.Sprintf("inspect pipeline_id %q", invalidID))
		summary := api.ExecutionSummary{
			ExecutionID: "exe-1",
			PipelineID:  invalidID,
			State:       "running",
			Version:     1,
		}
		requireInvalid(t, summary.Validate(), fmt.Sprintf("summary pipeline_id %q", invalidID))
	}

	for _, validID := range []string{"pipe-1", "PIPE_1", "aBc123", "pipeline_1"} {
		inspect := api.ExecutionInspectResult{
			ExecutionID:   "exe-1",
			PipelineID:    validID,
			State:         "running",
			Version:       1,
			CurrentAction: -1,
			Cleanup:       api.ExecutionCleanup{State: "none"},
		}
		if err := inspect.Validate(); err != nil {
			t.Errorf("inspect with valid pipeline_id %q refused: %v", validID, err)
		}
		summary := api.ExecutionSummary{
			ExecutionID: "exe-1",
			PipelineID:  validID,
			State:       "running",
			Version:     1,
		}
		if err := summary.Validate(); err != nil {
			t.Errorf("summary with valid pipeline_id %q refused: %v", validID, err)
		}
	}

	// The strict decoder validates the same optional identity on the wire.
	inspectCodec := frozenCodec(t, "execution.inspect")
	listCodec := frozenCodec(t, "execution.list")
	for _, invalidID := range invalidIDs {
		inspectBody := fmt.Sprintf(`{"execution_id":"exe-1","pipeline_id":%q,"state":"running","version":1,"current_action":-1,"actions":[],"cleanup":{"state":"none"}}`, invalidID)
		if _, err := inspectCodec.DecodeResult([]byte(inspectBody)); err == nil {
			t.Errorf("execution.inspect accepted invalid pipeline_id %q", invalidID)
		}
		listBody := fmt.Sprintf(`{"executions":[{"execution_id":"exe-1","pipeline_id":%q,"state":"running","version":1}]}`, invalidID)
		if _, err := listCodec.DecodeResult([]byte(listBody)); err == nil {
			t.Errorf("execution.list accepted invalid pipeline_id %q", invalidID)
		}
	}
	validBody := `{"execution_id":"exe-1","pipeline_id":"pipe-1","state":"running","version":1,"current_action":-1,"actions":[],"cleanup":{"state":"none"}}`
	if _, err := inspectCodec.DecodeResult([]byte(validBody)); err != nil {
		t.Errorf("execution.inspect refused a valid pipeline_id: %v", err)
	}
}

// inspectActionBody builds one execution.inspect result body with a single
// request action whose subject is supplied verbatim.
func inspectActionBody(subject string) []byte {
	return []byte(`{"execution_id":"exe-1","state":"running","version":1,"current_action":0,"actions":[{"action_id":"act-1","index":0,"name":"approve","kind":"request","state":"running","version":1,` + subject + `"assignment":"role/reviewer"}],"cleanup":{"state":"none"}}`)
}

// TestExecutionActionSubjectDecodeIsPresenceAware pins that the inspection
// subject decodes member-presence exactly like the inbox item: a field that
// omits its name, kind or value is refused instead of defaulting the missing
// value to an empty string. A null subject is the omitted optional projection.
func TestExecutionActionSubjectDecodeIsPresenceAware(t *testing.T) {
	codec := frozenCodec(t, "execution.inspect")
	for _, subject := range []string{
		`"subject":{"fields":[{"name":"seed","kind":"string"}]},`,
		`"subject":{"fields":[{"name":"seed","value":"seed-1"}]},`,
		`"subject":{"fields":[{"kind":"string","value":"seed-1"}]},`,
	} {
		if _, err := codec.DecodeResult(inspectActionBody(subject)); err == nil {
			t.Errorf("execution.inspect accepted a subject field missing a required member: %s", subject)
		}
	}

	value, err := codec.DecodeResult(inspectActionBody(`"subject":null,`))
	if err != nil {
		t.Fatalf("execution.inspect refused a null subject: %v", err)
	}
	action := value.(api.ExecutionInspectResult).Actions[0]
	if action.Subject != nil {
		t.Fatalf("null subject decoded as %+v; want the omitted projection", action.Subject)
	}

	// A subject present but empty is not an omitted subject on a request
	// action: it is refused rather than fabricated.
	if _, err := codec.DecodeResult(inspectActionBody(`"subject":{"fields":[]},`)); err == nil {
		t.Fatal("execution.inspect accepted a non-nil empty request subject")
	}
	emptySubject := api.Subject{}
	action = api.ExecutionAction{
		ActionID:   "act-1",
		Index:      0,
		Name:       "approve",
		Kind:       "request",
		State:      "running",
		Version:    1,
		Subject:    &emptySubject,
		Assignment: "role/reviewer",
	}
	requireInvalid(t, action.Validate(), "request action with a non-nil empty subject")
}

// TestExecutionActionSubjectStrictness pins the inspection subject to the same
// strict decoder and validator as the inbox item: an unknown nested member, a
// duplicate field name, an unknown value kind and an empty value kind are each
// refused with the public invalid_input class, while a valid action subject
// decodes. The action must not fall back to a lenient nested decode.
func TestExecutionActionSubjectStrictness(t *testing.T) {
	codec := frozenCodec(t, "execution.inspect")
	for name, subject := range map[string]string{
		"unknown nested member": `"subject":{"fields":[{"name":"seed","kind":"string","value":"seed-1","extra":true}]},`,
		"duplicate field name":  `"subject":{"fields":[{"name":"seed","kind":"string","value":"a"},{"name":"seed","kind":"string","value":"b"}]},`,
		"unknown value kind":    `"subject":{"fields":[{"name":"seed","kind":"token","value":"a"}]},`,
		"empty value kind":      `"subject":{"fields":[{"name":"seed","kind":"","value":"a"}]},`,
		"empty field name":      `"subject":{"fields":[{"name":"","kind":"string","value":"a"}]},`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := codec.DecodeResult(inspectActionBody(subject))
			requireInvalid(t, err, "execution.inspect subject with "+name)
		})
	}

	// Positive control: the strict path still accepts and preserves the valid
	// typed action subject.
	value, err := codec.DecodeResult(inspectActionBody(`"subject":{"fields":[{"name":"seed","kind":"string","value":"seed-1"}]},`))
	if err != nil {
		t.Fatalf("execution.inspect refused a valid action subject: %v", err)
	}
	action := value.(api.ExecutionInspectResult).Actions[0]
	if action.Subject == nil || len(action.Subject.Fields) != 1 ||
		action.Subject.Fields[0] != (api.SubjectField{Name: "seed", Kind: "string", Value: "seed-1"}) {
		t.Fatalf("decoded action subject = %+v; want the typed seed field", action.Subject)
	}
}

// TestExecutionActionRefusesRequestFactsOnNonRequestKinds pins that only a
// request action may carry the request-only subject and assignment, and that a
// present assignment must satisfy the role/<role> or service/<id>
// grammar.
func TestExecutionActionRefusesRequestFactsOnNonRequestKinds(t *testing.T) {
	subject := requestItemSubject()
	container := api.ExecutionAction{
		ActionID: "act-0",
		Index:    0,
		Name:     "build",
		Kind:     "container",
		State:    "succeeded",
		Version:  1,
	}
	withSubject := container
	withSubject.Subject = &subject
	requireInvalid(t, withSubject.Validate(), "container action carrying a subject")
	withAssignment := container
	withAssignment.Assignment = "role/reviewer"
	requireInvalid(t, withAssignment.Validate(), "container action carrying an assignment")

	requestAction := api.ExecutionAction{
		ActionID:   "act-1",
		Index:      0,
		Name:       "approve",
		Kind:       "request",
		State:      "running",
		Version:    1,
		Subject:    &subject,
		Assignment: "role/reviewer",
	}
	if err := requestAction.Validate(); err != nil {
		t.Fatalf("complete request action refused: %v", err)
	}
	for _, invalid := range []string{"reviewer", "role/", "/reviewer", "role/reviewer/extra", "team/reviewer", "role/Reviewer", "service/bad/id"} {
		action := requestAction
		action.Assignment = invalid
		requireInvalid(t, action.Validate(), fmt.Sprintf("request assignment %q", invalid))
	}
	for _, valid := range []string{"role/reviewer", "role/admin", "service/svc-1", "service/aBc_1"} {
		action := requestAction
		action.Assignment = valid
		if err := action.Validate(); err != nil {
			t.Errorf("valid request assignment %q refused: %v", valid, err)
		}
	}
}
