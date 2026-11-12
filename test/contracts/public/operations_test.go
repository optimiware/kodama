// The wire vocabulary: per-operation request/result DTOs and the
// registry-keyed codec table.
//
// These tests pin a field-level request and result schema for the operations
// listed in operationCases, including distinct capability-shaped DTOs for the
// event operations and an optional question_id on task.ask's result. They are
// compile-time consumers of the declaration surface below; the golden vectors
// in testdata/wire/operations.json are the authoritative field sets.
//
// Pinned declaration surface:
//
//	api.OperationCodec struct {
//	    Operation              string
//	    InputSchema            string  // "<operation>.input.v1"
//	    ResultSchema           string  // "<operation>.result.v1"
//	    RequiresIdempotencyKey bool    // registry receipt policy, not method
//	    DecodeRequest  func(json.RawMessage) (any, error)
//	    EncodeRequest  func(any) (json.RawMessage, error)
//	    DecodeResult   func(json.RawMessage) (any, error)
//	    EncodeResult   func(any) (json.RawMessage, error)
//	}
//	api.OperationCodecs() []api.OperationCodec
//	        exactly the 51 registry operations in registry declaration order.
//	api.OperationInputSchema(operation string) (json.RawMessage, error)
//	        the MCP tools/list inputSchema: a field-level JSON Schema object for
//	        the shared request envelope whose `input` member types the
//	        operation's request DTO and keeps the title "<operation>.input.v1".
//
// One `<Operation>Request`/`<Operation>Result` struct per typed
// operation, named by CamelCase operation segments (task.create_and_start ->
// TaskCreateAndStartRequest/TaskCreateAndStartResult), with stable snake_case
// JSON names, an exported `Validate() error`, and strict decode through the
// table. Nested helper values are named MembershipView, TaskHistoryEntry,
// InboxItem, ExecutionAction, ExecutionSummary and LogChunk; only their JSON
// shape is fixed by the vectors.
//
// event.publish, event.read and event.subscribe have distinct request/result
// DTOs that match the real capability services, so those three are covered by
// the vectors below. The remaining operations (actor.verify, resource.report,
// model.invoke) have no vector here: the table must still expose one codec
// entry each, but these tests do not pin their payload shapes.
package publiccontract

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/authorize"
)

// enumProbe is one request field value that must be refused.
type enumProbe struct {
	field string
	value string // raw JSON
}

// operationCase is one typed operation: the concrete DTO pair, the request
// fields carrying a bounded identity, and the request fields with a fixed
// value set that the golden vector must exercise with a refusal.
type operationCase struct {
	operation   string
	request     any
	result      any
	identifiers []string
	enums       []enumProbe
}

// operationCases is the set of typed operations, in the registry declaration
// order used by api.OperationCodecs().
var operationCases = []operationCase{
	// Tenant, project and membership administration.
	{operation: "tenant.create", request: api.TenantCreateRequest{}, result: api.TenantCreateResult{}, identifiers: []string{"tenant_id"}},
	{operation: "tenant.rotate_key", request: api.TenantRotateKeyRequest{}, result: api.TenantRotateKeyResult{}},
	{operation: "project.create", request: api.ProjectCreateRequest{}, result: api.ProjectCreateResult{}, identifiers: []string{"project_id"}},
	{operation: "project.update", request: api.ProjectUpdateRequest{}, result: api.ProjectUpdateResult{}},
	{operation: "membership.grant", request: api.MembershipGrantRequest{}, result: api.MembershipGrantResult{}, enums: []enumProbe{{"roles", `["owner"]`}}},
	{operation: "membership.revoke", request: api.MembershipRevokeRequest{}, result: api.MembershipRevokeResult{}},
	{operation: "membership.list", request: api.MembershipListRequest{}, result: api.MembershipListResult{}},

	// Resource bindings.
	{operation: "resource.binding.register", request: api.ResourceBindingRegisterRequest{}, result: api.ResourceBindingRegisterResult{}, identifiers: []string{"binding_id", "resource_id", "enforcing_service"}, enums: []enumProbe{{"operations", `["git.ref.read"]`}, {"reportable_events", `["task.created"]`}}},
	{operation: "resource.binding.update", request: api.ResourceBindingUpdateRequest{}, result: api.ResourceBindingUpdateResult{}, identifiers: []string{"resource_id", "enforcing_service"}, enums: []enumProbe{{"operations", `["git.ref.read"]`}}},
	{operation: "resource.binding.delete", request: api.ResourceBindingDeleteRequest{}, result: api.ResourceBindingDeleteResult{}},
	{operation: "resource.binding.read", request: api.ResourceBindingReadRequest{}, result: api.ResourceBindingReadResult{}},

	// Pipelines and immutable execution start.
	{operation: "pipeline.apply", request: api.PipelineApplyRequest{}, result: api.PipelineApplyResult{}},
	{operation: "pipeline.read", request: api.PipelineReadRequest{}, result: api.PipelineReadResult{}},
	{operation: "pipeline.update", request: api.PipelineUpdateRequest{}, result: api.PipelineUpdateResult{}},
	{operation: "pipeline.delete", request: api.PipelineDeleteRequest{}, result: api.PipelineDeleteResult{}},
	{operation: "pipeline.start", request: api.PipelineStartRequest{}, result: api.PipelineStartResult{}},

	// Secret metadata and writes.
	{operation: "secret.metadata.read", request: api.SecretMetadataReadRequest{}, result: api.SecretMetadataReadResult{}},
	{operation: "secret.write", request: api.SecretWriteRequest{}, result: api.SecretWriteResult{}},
	{operation: "secret.delete", request: api.SecretDeleteRequest{}, result: api.SecretDeleteResult{}},

	// Service accounts and credential lifecycle.
	{operation: "service.create", request: api.ServiceCreateRequest{}, result: api.ServiceCreateResult{}, identifiers: []string{"service_id"}, enums: []enumProbe{{"roles", `["admin"]`}}},
	{operation: "service.update", request: api.ServiceUpdateRequest{}, result: api.ServiceUpdateResult{}, enums: []enumProbe{{"roles", `["admin"]`}}},
	{operation: "service.enable", request: api.ServiceEnableRequest{}, result: api.ServiceEnableResult{}},
	{operation: "service.disable", request: api.ServiceDisableRequest{}, result: api.ServiceDisableResult{}},
	{operation: "service.credential.issue", request: api.ServiceCredentialIssueRequest{}, result: api.ServiceCredentialIssueResult{}},
	{operation: "service.credential.rotate", request: api.ServiceCredentialRotateRequest{}, result: api.ServiceCredentialRotateResult{}},
	{operation: "service.credential.revoke", request: api.ServiceCredentialRevokeRequest{}, result: api.ServiceCredentialRevokeResult{}},

	// Tasks and conversation handoffs.
	{operation: "task.create", request: api.TaskCreateRequest{}, result: api.TaskCreateResult{}, identifiers: []string{"pipeline_id"}},
	{operation: "task.read", request: api.TaskReadRequest{}, result: api.TaskReadResult{}},
	{operation: "task.edit", request: api.TaskEditRequest{}, result: api.TaskEditResult{}},
	{operation: "task.comment", request: api.TaskCommentRequest{}, result: api.TaskCommentResult{}},
	{operation: "task.select_pipeline", request: api.TaskSelectPipelineRequest{}, result: api.TaskSelectPipelineResult{}, identifiers: []string{"pipeline_id"}},
	{operation: "task.start", request: api.TaskStartRequest{}, result: api.TaskStartResult{}},
	{operation: "task.create_and_start", request: api.TaskCreateAndStartRequest{}, result: api.TaskCreateAndStartResult{}, identifiers: []string{"pipeline_id"}},
	{operation: "task.ask", request: api.TaskAskRequest{}, result: api.TaskAskResult{}, enums: []enumProbe{{"assignment", `"role/owner"`}}},
	{operation: "task.reply", request: api.TaskReplyRequest{}, result: api.TaskReplyResult{}, identifiers: []string{"question_id"}},
	{operation: "task.cancel", request: api.TaskCancelRequest{}, result: api.TaskCancelResult{}},
	{operation: "task.history", request: api.TaskHistoryRequest{}, result: api.TaskHistoryResult{}},
	{operation: "task.inbox", request: api.TaskInboxRequest{}, result: api.TaskInboxResult{}, enums: []enumProbe{{"kinds", `["everything"]`}}},

	// Executions, outputs, request resolution and retained logs.
	{operation: "execution.inspect", request: api.ExecutionInspectRequest{}, result: api.ExecutionInspectResult{}},
	{operation: "execution.list", request: api.ExecutionListRequest{}, result: api.ExecutionListResult{}, identifiers: []string{"task_id", "pipeline_id"}, enums: []enumProbe{{"state", `"sleeping"`}}},
	{operation: "execution.cancel", request: api.ExecutionCancelRequest{}, result: api.ExecutionCancelResult{}},
	{operation: "execution.action.outputs", request: api.ExecutionActionOutputsRequest{}, result: api.ExecutionActionOutputsResult{}},
	{operation: "execution.request.resolve", request: api.ExecutionRequestResolveRequest{}, result: api.ExecutionRequestResolveResult{}, enums: []enumProbe{{"decision", `"maybe"`}}},
	{operation: "execution.logs.read", request: api.ExecutionLogsReadRequest{}, result: api.ExecutionLogsReadResult{}, identifiers: []string{"action_id"}, enums: []enumProbe{{"stream", `"stderr2"`}}},

	// Custom observations, authorized reads and subscription setup. Each has
	// its own DTO pair: a single api.Event for all three could not represent
	// the capability results.
	{operation: "event.publish", request: api.EventPublishRequest{}, result: api.EventPublishResult{}, identifiers: []string{"project_id"}, enums: []enumProbe{{"type", `"task.created"`}}},
	{operation: "event.read", request: api.EventReadRequest{}, result: api.EventReadResult{}, identifiers: []string{"project_id"}, enums: []enumProbe{{"types", `["task.teleported"]`}}},
	{operation: "event.subscribe", request: api.EventSubscribeRequest{}, result: api.EventSubscribeResult{}, identifiers: []string{"project_id"}, enums: []enumProbe{{"types", `["task.teleported"]`}}},

	// Optional tool surface.
	{operation: "tool.invoke", request: api.ToolInvokeRequest{}, result: api.ToolInvokeResult{}},
}

// zeroValidRequestFields are required request fields whose zero value is a
// legitimate value. There are none: every required request member is
// semantically non-zero.
var zeroValidRequestFields = map[string]bool{}

// zeroValidResultFields are required result fields whose zero value is a
// legitimate observation (an index, an offset, an empty list, a false flag, an
// empty chunk body, a count). They must still be present in the JSON object but
// may not be refused for being 0, empty or nil.
var zeroValidResultFields = map[string]bool{
	"index":               true,
	"current_action":      true,
	"offset":              true,
	"sequence":            true,
	"revoked_credentials": true,
	"is_error":            true,
	"truncated":           true,
	"data":                true,
	"content":             true,
	"memberships":         true,
	"entries":             true,
	"items":               true,
	"actions":             true,
	"executions":          true,
	"chunks":              true,
	"events":              true,
	"has_more":            true,
	"replayed":            true,
}

// replayFlagOperations are the operations whose result may legitimately carry
// the replay fact. event.publish reports whether an equal retry replayed the
// committed observation; every other result is free of a replay flag.
var replayFlagOperations = map[string]bool{
	"event.publish": true,
}

// credentialTokenOperations are the only two operations whose result may carry
// a fresh credential token.
var credentialTokenOperations = map[string]bool{
	"service.credential.issue":  true,
	"service.credential.rotate": true,
}

// operationVector is one frozen request/result vector.
type operationVector struct {
	Operation string          `json:"operation"`
	Request   json.RawMessage `json:"request"`
	Result    json.RawMessage `json:"result"`
}

type operationVectorFile struct {
	Operations []operationVector `json:"operations"`
}

// loadOperationVectors reads the frozen per-operation vectors and refuses a
// malformed, duplicated or missing entry.
func loadOperationVectors(t *testing.T) map[string]operationVector {
	t.Helper()
	path := filepath.Join("testdata", "wire", "operations.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var file operationVectorFile
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	byOperation := make(map[string]operationVector, len(file.Operations))
	for _, vector := range file.Operations {
		if vector.Operation == "" {
			t.Fatalf("%s has a vector with no operation", path)
		}
		if _, duplicate := byOperation[vector.Operation]; duplicate {
			t.Fatalf("%s repeats operation %q", path, vector.Operation)
		}
		if len(vector.Request) == 0 || len(vector.Result) == 0 {
			t.Fatalf("%s vector %q needs a request and a result", path, vector.Operation)
		}
		byOperation[vector.Operation] = vector
	}
	if len(byOperation) != len(operationCases) {
		t.Fatalf("%s has %d vectors; want exactly %d typed operations", path, len(byOperation), len(operationCases))
	}
	for _, testCase := range operationCases {
		if _, ok := byOperation[testCase.operation]; !ok {
			t.Errorf("%s is missing the vector for %q", path, testCase.operation)
		}
	}
	return byOperation
}

// codecFor returns the registry-keyed codec for one operation.
func codecFor(t *testing.T, operation string) api.OperationCodec {
	t.Helper()
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == operation {
			return codec
		}
	}
	t.Fatalf("api.OperationCodecs() has no codec for %q", operation)
	return api.OperationCodec{}
}

// indirectType strips pointer indirection so a value or pointer DTO compares
// equal.
func indirectType(typ reflect.Type) reflect.Type {
	for typ != nil && typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return typ
}

// jsonFieldName returns the wire name of a struct field and whether it is
// optional (omitempty) or excluded ("-").
func jsonFieldName(field reflect.StructField) (name string, omitempty bool, ok bool) {
	tag, present := field.Tag.Lookup("json")
	if !present {
		return field.Name, false, true
	}
	parts := strings.Split(tag, ",")
	if parts[0] == "-" {
		return "", false, false
	}
	name = parts[0]
	if name == "" {
		name = field.Name
	}
	for _, option := range parts[1:] {
		if option == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty, true
}

// structFields returns the wire-visible fields of one DTO struct type.
func structFields(typ reflect.Type) []reflect.StructField {
	typ = indirectType(typ)
	if typ == nil || typ.Kind() != reflect.Struct {
		return nil
	}
	fields := make([]reflect.StructField, 0, typ.NumField())
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		if !field.IsExported() {
			continue
		}
		if _, _, ok := jsonFieldName(field); !ok {
			continue
		}
		fields = append(fields, field)
	}
	return fields
}

// wireFieldNames returns every declared wire name of one DTO struct.
func wireFieldNames(typ reflect.Type) []string {
	fields := structFields(typ)
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		name, _, _ := jsonFieldName(field)
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// requiredFieldNames returns the non-omitempty wire names of one DTO struct.
func requiredFieldNames(typ reflect.Type) []string {
	fields := structFields(typ)
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		name, omitempty, _ := jsonFieldName(field)
		if !omitempty {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// fieldByName returns the struct field carrying the wire name.
func fieldByName(typ reflect.Type, name string) (reflect.StructField, bool) {
	for _, field := range structFields(typ) {
		candidate, _, _ := jsonFieldName(field)
		if candidate == name {
			return field, true
		}
	}
	return reflect.StructField{}, false
}

// wrongJSONFor returns a JSON value of the wrong kind for the field type.
func wrongJSONFor(typ reflect.Type) json.RawMessage {
	typ = indirectType(typ)
	switch typ.Kind() {
	case reflect.String:
		return json.RawMessage(`42`)
	case reflect.Bool:
		return json.RawMessage(`"true"`)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return json.RawMessage(`"not-a-number"`)
	case reflect.Slice:
		return json.RawMessage(`"not-an-array"`)
	case reflect.Map:
		return json.RawMessage(`[]`)
	case reflect.Struct:
		return json.RawMessage(`"not-an-object"`)
	default:
		return json.RawMessage(`[]`)
	}
}

// decodeObject parses one JSON object into raw members.
func decodeObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decode JSON object %s: %v", raw, err)
	}
	return object
}

// encodeObject re-encodes one JSON object.
func encodeObject(t *testing.T, object map[string]json.RawMessage) json.RawMessage {
	t.Helper()
	encoded := marshalObject(object)
	if encoded == nil {
		t.Fatalf("encode JSON object: %v", object)
	}
	return encoded
}

// marshalObject re-encodes one raw JSON object without a test handle.
func marshalObject(object map[string]json.RawMessage) json.RawMessage {
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil
	}
	return encoded
}

// cloneObject copies one raw JSON object.
func cloneObject(object map[string]json.RawMessage) map[string]json.RawMessage {
	cloned := make(map[string]json.RawMessage, len(object))
	for key, value := range object {
		cloned[key] = value
	}
	return cloned
}

// childUnknownMutants returns the raw values of one field with an unknown
// member injected into every nested declared struct object.
func childUnknownMutants(raw json.RawMessage, typ reflect.Type) []json.RawMessage {
	typ = indirectType(typ)
	if typ == nil {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		return unknownFieldMutants(raw, typ)
	case reflect.Slice:
		element := indirectType(typ.Elem())
		if element == nil || element.Kind() != reflect.Struct {
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil
		}
		var mutants []json.RawMessage
		for index, item := range items {
			for _, mutant := range unknownFieldMutants(item, element) {
				cloned := append([]json.RawMessage(nil), items...)
				cloned[index] = mutant
				encoded, err := json.Marshal(cloned)
				if err != nil {
					continue
				}
				mutants = append(mutants, encoded)
			}
		}
		return mutants
	}
	return nil
}

// unknownFieldMutants injects an unknown member into the value at every
// declared struct level. Opaque members (maps, raw JSON, scalars) are not
// descended into: their contents belong to the operation, not the envelope.
func unknownFieldMutants(raw json.RawMessage, typ reflect.Type) []json.RawMessage {
	typ = indirectType(typ)
	if typ == nil || typ.Kind() != reflect.Struct {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil
	}
	var mutants []json.RawMessage
	withUnknown := cloneObject(object)
	withUnknown["bogus_field"] = json.RawMessage(`true`)
	mutants = append(mutants, marshalObject(withUnknown))
	for key, value := range object {
		field, ok := fieldByName(typ, key)
		if !ok {
			continue
		}
		for _, mutant := range childUnknownMutants(value, field.Type) {
			cloned := cloneObject(object)
			cloned[key] = mutant
			mutants = append(mutants, marshalObject(cloned))
		}
	}
	return mutants
}

// missingFieldMutants deletes each required member at every declared struct
// level. A required field is one without omitempty, except a zero-valid
// observation.
func missingFieldMutants(raw json.RawMessage, typ reflect.Type, zeroValid map[string]bool) []json.RawMessage {
	typ = indirectType(typ)
	if typ == nil || typ.Kind() != reflect.Struct {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil
	}
	var mutants []json.RawMessage
	for key := range object {
		field, ok := fieldByName(typ, key)
		if !ok {
			continue
		}
		name, omitempty, _ := jsonFieldName(field)
		if !omitempty && !zeroValid[name] {
			cloned := cloneObject(object)
			delete(cloned, key)
			mutants = append(mutants, marshalObject(cloned))
		}
		childZeroValid := zeroValid
		if zeroValid == nil {
			childZeroValid = map[string]bool{}
		}
		for _, mutant := range missingChildMutants(object[key], field.Type, childZeroValid) {
			cloned := cloneObject(object)
			cloned[key] = mutant
			mutants = append(mutants, marshalObject(cloned))
		}
	}
	return mutants
}

// missingChildMutants deletes required members from nested struct values.
func missingChildMutants(raw json.RawMessage, typ reflect.Type, zeroValid map[string]bool) []json.RawMessage {
	typ = indirectType(typ)
	if typ == nil {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		return missingFieldMutants(raw, typ, zeroValid)
	case reflect.Slice:
		element := indirectType(typ.Elem())
		if element == nil || element.Kind() != reflect.Struct {
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil
		}
		var mutants []json.RawMessage
		for index, item := range items {
			for _, mutant := range missingFieldMutants(item, element, zeroValid) {
				cloned := append([]json.RawMessage(nil), items...)
				cloned[index] = mutant
				encoded, err := json.Marshal(cloned)
				if err != nil {
					continue
				}
				mutants = append(mutants, encoded)
			}
		}
		return mutants
	}
	return nil
}

// decodeRequestPayload decodes one request payload through the table and then
// validates the typed value.
func decodeRequestPayload(codec api.OperationCodec, raw json.RawMessage) (any, error) {
	value, err := codec.DecodeRequest(raw)
	if err != nil {
		return nil, err
	}
	if validator, ok := value.(interface{ Validate() error }); ok {
		if err := validator.Validate(); err != nil {
			return nil, err
		}
	}
	return value, nil
}

// decodeResultPayload decodes one result payload through the table and then
// validates the typed value.
func decodeResultPayload(codec api.OperationCodec, raw json.RawMessage) (any, error) {
	value, err := codec.DecodeResult(raw)
	if err != nil {
		return nil, err
	}
	if validator, ok := value.(interface{ Validate() error }); ok {
		if err := validator.Validate(); err != nil {
			return nil, err
		}
	}
	return value, nil
}

// requireRequestRefusal asserts that a request payload is refused with the
// stable invalid_input code.
func requireRequestRefusal(t *testing.T, codec api.OperationCodec, raw json.RawMessage, what string) {
	t.Helper()
	if _, err := decodeRequestPayload(codec, raw); err == nil {
		t.Errorf("%s: request %s was accepted; want refusal", codec.Operation, what)
	} else {
		requirePublicError(t, err, api.CodeInvalidInput)
	}
}

// requireResultRefusal asserts that a result payload is refused.
func requireResultRefusal(t *testing.T, codec api.OperationCodec, raw json.RawMessage, what string) {
	t.Helper()
	if _, err := decodeResultPayload(codec, raw); err == nil {
		t.Errorf("%s: result %s was accepted; want refusal", codec.Operation, what)
	}
}

// TestOperationCodecsCoverTheFrozenRegistry pins that the table is exactly the
// 51 registry operations, in declaration order, with the registry schema names
// and one route binding each, and that every entry exposes all four codec
// functions.
func TestOperationCodecsCoverTheFrozenRegistry(t *testing.T) {
	codecs := api.OperationCodecs()
	names := authorize.OperationNames()
	if len(codecs) != len(names) {
		t.Fatalf("OperationCodecs() has %d entries; want %d registry operations", len(codecs), len(names))
	}
	seen := make(map[string]bool, len(codecs))
	for index, operation := range names {
		name := string(operation)
		codec := codecs[index]
		if codec.Operation != name {
			t.Fatalf("OperationCodecs()[%d] = %q; want %q in registry order", index, codec.Operation, name)
		}
		if seen[name] {
			t.Errorf("OperationCodecs() repeats operation %q", name)
		}
		seen[name] = true
		if codec.InputSchema != name+".input.v1" {
			t.Errorf("%s input schema = %q; want %q", name, codec.InputSchema, name+".input.v1")
		}
		if codec.ResultSchema != name+".result.v1" {
			t.Errorf("%s result schema = %q; want %q", name, codec.ResultSchema, name+".result.v1")
		}
		route, ok := api.LookupRoute(name)
		if !ok {
			t.Fatalf("LookupRoute(%q) refused a registry operation", name)
		}
		if codec.InputSchema != route.InputSchema || codec.ResultSchema != route.ResultSchema {
			t.Errorf("%s codec schemas %q/%q drift from the route %q/%q", name, codec.InputSchema, codec.ResultSchema, route.InputSchema, route.ResultSchema)
		}
		if codec.DecodeRequest == nil || codec.EncodeRequest == nil || codec.DecodeResult == nil || codec.EncodeResult == nil {
			t.Errorf("%s codec has a nil request/result function", name)
		}
	}
	if len(seen) != len(names) {
		t.Errorf("OperationCodecs() covers %d operations; want %d", len(seen), len(names))
	}
}

// TestOperationCodecTypesMatchFrozenDTOs pins the concrete DTO each typed
// operation decodes into, and that every decoded value carries Validate().
func TestOperationCodecTypesMatchFrozenDTOs(t *testing.T) {
	vectors := loadOperationVectors(t)
	for _, testCase := range operationCases {
		t.Run(testCase.operation, func(t *testing.T) {
			codec := codecFor(t, testCase.operation)
			vector := vectors[testCase.operation]

			request, err := decodeRequestPayload(codec, vector.Request)
			if err != nil {
				t.Fatalf("DecodeRequest(%s): %v", vector.Request, err)
			}
			if _, ok := request.(interface{ Validate() error }); !ok {
				t.Errorf("%s decoded request %T has no Validate()", testCase.operation, request)
			}
			if got, want := indirectType(reflect.TypeOf(request)), indirectType(reflect.TypeOf(testCase.request)); got != want {
				t.Errorf("%s decodes its request into %v; want %v", testCase.operation, got, want)
			}

			result, err := decodeResultPayload(codec, vector.Result)
			if err != nil {
				t.Fatalf("DecodeResult(%s): %v", vector.Result, err)
			}
			if _, ok := result.(interface{ Validate() error }); !ok {
				t.Errorf("%s decoded result %T has no Validate()", testCase.operation, result)
			}
			if got, want := indirectType(reflect.TypeOf(result)), indirectType(reflect.TypeOf(testCase.result)); got != want {
				t.Errorf("%s decodes its result into %v; want %v", testCase.operation, got, want)
			}
		})
	}
}

// TestOperationVectorsRoundTrip pins the golden request and result of every
// typed operation: strict decode, semantic equality with the vector and a
// byte-stable encode/decode fixed point.
func TestOperationVectorsRoundTrip(t *testing.T) {
	vectors := loadOperationVectors(t)
	for _, testCase := range operationCases {
		t.Run(testCase.operation, func(t *testing.T) {
			codec := codecFor(t, testCase.operation)
			vector := vectors[testCase.operation]

			request, err := decodeRequestPayload(codec, vector.Request)
			if err != nil {
				t.Fatalf("DecodeRequest(%s): %v", vector.Request, err)
			}
			encodedRequest, err := codec.EncodeRequest(request)
			if err != nil {
				t.Fatalf("EncodeRequest(%T): %v", request, err)
			}
			requireJSONObject(t, encodedRequest, "encoded request")
			requireJSONEquivalent(t, encodedRequest, vector.Request, "encoded request")
			reRequest, err := decodeRequestPayload(codec, encodedRequest)
			if err != nil {
				t.Fatalf("re-decode request %s: %v", encodedRequest, err)
			}
			stableRequest, err := codec.EncodeRequest(reRequest)
			if err != nil {
				t.Fatalf("re-encode request: %v", err)
			}
			if !bytes.Equal(encodedRequest, stableRequest) {
				t.Errorf("request encoding is not byte-stable: %s then %s", encodedRequest, stableRequest)
			}

			result, err := decodeResultPayload(codec, vector.Result)
			if err != nil {
				t.Fatalf("DecodeResult(%s): %v", vector.Result, err)
			}
			encodedResult, err := codec.EncodeResult(result)
			if err != nil {
				t.Fatalf("EncodeResult(%T): %v", result, err)
			}
			requireJSONObject(t, encodedResult, "encoded result")
			requireJSONEquivalent(t, encodedResult, vector.Result, "encoded result")
			reResult, err := decodeResultPayload(codec, encodedResult)
			if err != nil {
				t.Fatalf("re-decode result %s: %v", encodedResult, err)
			}
			stableResult, err := codec.EncodeResult(reResult)
			if err != nil {
				t.Fatalf("re-encode result: %v", err)
			}
			if !bytes.Equal(encodedResult, stableResult) {
				t.Errorf("result encoding is not byte-stable: %s then %s", encodedResult, stableResult)
			}
		})
	}
}

// TestOperationRequestRefusals pins the per-operation request refusals: a
// missing required member, a wrong type, an empty or oversized identity, an
// unknown member at every declared nesting level, and every frozen enum.
func TestOperationRequestRefusals(t *testing.T) {
	vectors := loadOperationVectors(t)
	for _, testCase := range operationCases {
		t.Run(testCase.operation, func(t *testing.T) {
			codec := codecFor(t, testCase.operation)
			vector := vectors[testCase.operation]
			requestType := reflect.TypeOf(testCase.request)
			object := decodeObject(t, vector.Request)

			required := requiredFieldNames(requestType)
			for _, mutant := range missingFieldMutants(vector.Request, requestType, zeroValidRequestFields) {
				requireRequestRefusal(t, codec, mutant, "with a missing required member")
			}
			if len(required) > 0 {
				requireRequestRefusal(t, codec, json.RawMessage(`{}`), "empty object")
			}

			for _, name := range required {
				field, ok := fieldByName(requestType, name)
				if !ok {
					continue
				}
				cloned := cloneObject(object)
				cloned[name] = wrongJSONFor(field.Type)
				requireRequestRefusal(t, codec, encodeObject(t, cloned), "with a wrong type for "+name)
			}

			for _, name := range testCase.identifiers {
				field, ok := fieldByName(requestType, name)
				if !ok {
					t.Errorf("%s declares identifier %q that the request DTO does not have", testCase.operation, name)
					continue
				}
				_, omitempty, _ := jsonFieldName(field)
				if !omitempty {
					cloned := cloneObject(object)
					cloned[name] = json.RawMessage(`""`)
					requireRequestRefusal(t, codec, encodeObject(t, cloned), "with an empty "+name)
				}
				cloned := cloneObject(object)
				cloned[name] = json.RawMessage(`"` + strings.Repeat("a", 128) + `"`)
				if _, err := decodeRequestPayload(codec, encodeObject(t, cloned)); err != nil {
					t.Errorf("%s refused a 128-byte %s: %v", testCase.operation, name, err)
				}
				cloned = cloneObject(object)
				cloned[name] = json.RawMessage(`"` + strings.Repeat("a", 129) + `"`)
				requireRequestRefusal(t, codec, encodeObject(t, cloned), "with an oversized "+name)
				cloned = cloneObject(object)
				cloned[name] = json.RawMessage(`"bad/id"`)
				requireRequestRefusal(t, codec, encodeObject(t, cloned), "with a non-identity "+name)
			}

			for _, probe := range testCase.enums {
				if _, ok := fieldByName(requestType, probe.field); !ok {
					t.Errorf("%s declares enum field %q that the request DTO does not have", testCase.operation, probe.field)
					continue
				}
				cloned := cloneObject(object)
				cloned[probe.field] = json.RawMessage(probe.value)
				requireRequestRefusal(t, codec, encodeObject(t, cloned), "with an invalid "+probe.field)
			}

			for _, mutant := range unknownFieldMutants(vector.Request, requestType) {
				requireRequestRefusal(t, codec, mutant, "with an unknown member")
			}
		})
	}
}

// TestOperationResultRefusals pins the per-operation result refusals: a missing
// required observation, an unknown member at every declared nesting level, and
// a zero version where a version is required.
func TestOperationResultRefusals(t *testing.T) {
	vectors := loadOperationVectors(t)
	for _, testCase := range operationCases {
		t.Run(testCase.operation, func(t *testing.T) {
			codec := codecFor(t, testCase.operation)
			vector := vectors[testCase.operation]
			resultType := reflect.TypeOf(testCase.result)
			object := decodeObject(t, vector.Result)

			for _, mutant := range missingFieldMutants(vector.Result, resultType, zeroValidResultFields) {
				requireResultRefusal(t, codec, mutant, "with a missing required member")
			}
			for _, mutant := range unknownFieldMutants(vector.Result, resultType) {
				requireResultRefusal(t, codec, mutant, "with an unknown member")
			}

			for name := range object {
				field, ok := fieldByName(resultType, name)
				if !ok {
					continue
				}
				if name != "version" && !strings.HasSuffix(name, "_version") {
					continue
				}
				if indirectType(field.Type).Kind() != reflect.Uint64 {
					continue
				}
				cloned := cloneObject(object)
				cloned[name] = json.RawMessage(`0`)
				requireResultRefusal(t, codec, encodeObject(t, cloned), "with a zero "+name)
			}
		})
	}
}

// TestOperationSecretFreedom pins that no result leaks a secret value, actor
// authority or transaction internals, and that only the two credential
// lifecycle results may carry a fresh one-time token.
func TestOperationSecretFreedom(t *testing.T) {
	vectors := loadOperationVectors(t)
	forbidden := []string{
		"secret_value",
		"plaintext",
		"actor_credential",
		"nonce",
		"wrapped_key",
		"key_ref",
		"private_key",
		"password",
		"authority",
		"replayed",
	}
	for _, testCase := range operationCases {
		t.Run(testCase.operation, func(t *testing.T) {
			codec := codecFor(t, testCase.operation)
			vector := vectors[testCase.operation]
			object := decodeObject(t, vector.Result)

			var decoded any
			if err := json.Unmarshal(vector.Result, &decoded); err != nil {
				t.Fatalf("parse result: %v", err)
			}
			for key := range collectJSONKeys(decoded) {
				for _, banned := range forbidden {
					if key == banned {
						if banned == "replayed" && replayFlagOperations[testCase.operation] {
							continue
						}
						t.Errorf("%s result carries the forbidden member %q", testCase.operation, key)
					}
				}
				if key == "token" && !credentialTokenOperations[testCase.operation] {
					t.Errorf("%s result carries a token member", testCase.operation)
				}
			}

			for _, banned := range forbidden {
				if banned == "replayed" && replayFlagOperations[testCase.operation] {
					continue
				}
				cloned := cloneObject(object)
				cloned[banned] = json.RawMessage(`"leak"`)
				requireResultRefusal(t, codec, encodeObject(t, cloned), "with an injected "+banned)
			}
			if credentialTokenOperations[testCase.operation] {
				if token, ok := object["token"]; !ok || string(token) == `""` {
					t.Fatalf("%s golden result must carry the fresh one-time token", testCase.operation)
				}
			} else {
				cloned := cloneObject(object)
				cloned["token"] = json.RawMessage(`"tok-leak"`)
				requireResultRefusal(t, codec, encodeObject(t, cloned), "with an injected token")
			}
		})
	}

	for _, operation := range []string{"service.credential.issue", "service.credential.rotate"} {
		t.Run(operation+"/token bounds", func(t *testing.T) {
			codec := codecFor(t, operation)
			base := decodeObject(t, loadOperationVectors(t)[operation].Result)

			withoutToken := cloneObject(base)
			delete(withoutToken, "token")
			if _, err := decodeResultPayload(codec, encodeObject(t, withoutToken)); err != nil {
				t.Errorf("%s refused a replayed result without a token: %v", operation, err)
			}

			emptyToken := cloneObject(base)
			emptyToken["token"] = json.RawMessage(`""`)
			requireResultRefusal(t, codec, encodeObject(t, emptyToken), "with an empty token")

			atBound := cloneObject(base)
			atBound["token"] = json.RawMessage(`"` + strings.Repeat("t", api.MaxCredentialBytes) + `"`)
			if _, err := decodeResultPayload(codec, encodeObject(t, atBound)); err != nil {
				t.Errorf("%s refused a %d-byte token: %v", operation, api.MaxCredentialBytes, err)
			}
			oversized := cloneObject(base)
			oversized["token"] = json.RawMessage(`"` + strings.Repeat("t", api.MaxCredentialBytes+1) + `"`)
			requireResultRefusal(t, codec, encodeObject(t, oversized), "with an oversized token")
		})
	}
}

// TestOperationIdempotencyPolicy pins that every receipt-bearing registry
// operation requires a caller idempotency key while every read does not.
func TestOperationIdempotencyPolicy(t *testing.T) {
	for _, operation := range authorize.Operations() {
		name := string(operation.Name)
		codec := codecFor(t, name)
		want := operation.Receipt != authorize.ReceiptNone
		if codec.RequiresIdempotencyKey != want {
			t.Errorf("%s RequiresIdempotencyKey = %v; want %v from receipt policy %q",
				name, codec.RequiresIdempotencyKey, want, operation.Receipt)
		}
		route, ok := api.LookupRoute(name)
		if !ok {
			t.Fatalf("LookupRoute(%q) refused a registry operation", name)
		}
		if route.Method == api.MethodGet && codec.RequiresIdempotencyKey {
			t.Errorf("%s is a GET read but requires an idempotency key", name)
		}
	}
}

// TestOperationBodyBounds pins the bounded request bodies: the pipeline YAML
// document and the stored secret value accept exactly their frozen bound and
// refuse one byte more.
func TestOperationBodyBounds(t *testing.T) {
	apply := codecFor(t, "pipeline.apply")
	atBound := api.PipelineApplyRequest{Name: "build.v2", YAML: make([]byte, 1<<20)}
	encoded, err := apply.EncodeRequest(atBound)
	if err != nil {
		t.Fatalf("pipeline.apply refused a 1 MiB YAML document: %v", err)
	}
	if _, err := decodeRequestPayload(apply, encoded); err != nil {
		t.Fatalf("pipeline.apply refused a 1 MiB YAML document on decode: %v", err)
	}
	if _, err := apply.EncodeRequest(api.PipelineApplyRequest{Name: "build.v2", YAML: make([]byte, 1<<20+1)}); err == nil {
		t.Error("pipeline.apply accepted a YAML document one byte over the bound")
	} else {
		requirePublicError(t, err, api.CodeInvalidInput)
	}
	if _, err := apply.EncodeRequest(api.PipelineApplyRequest{}); err == nil {
		t.Error("pipeline.apply accepted an empty YAML document")
	}

	update := codecFor(t, "pipeline.update")
	if _, err := update.EncodeRequest(api.PipelineUpdateRequest{Name: "build", YAML: make([]byte, 1<<20+1)}); err == nil {
		t.Error("pipeline.update accepted a YAML document one byte over the bound")
	}

	write := codecFor(t, "secret.write")
	atSecretBound := api.SecretWriteRequest{Value: strings.Repeat("v", 64<<10)}
	encoded, err = write.EncodeRequest(atSecretBound)
	if err != nil {
		t.Fatalf("secret.write refused a 64 KiB value: %v", err)
	}
	if _, err := decodeRequestPayload(write, encoded); err != nil {
		t.Fatalf("secret.write refused a 64 KiB value on decode: %v", err)
	}
	if _, err := write.EncodeRequest(api.SecretWriteRequest{Value: strings.Repeat("v", 64<<10+1)}); err == nil {
		t.Error("secret.write accepted a value one byte over the bound")
	} else {
		requirePublicError(t, err, api.CodeInvalidInput)
	}
	if _, err := write.EncodeRequest(api.SecretWriteRequest{}); err == nil {
		t.Error("secret.write accepted an empty value")
	}
}

// TestOperationToolArguments pins that tool.invoke carries an opaque JSON
// object and refuses a non-object argument payload.
func TestOperationToolArguments(t *testing.T) {
	codec := codecFor(t, "tool.invoke")
	for _, raw := range []string{`{"arguments":[]}`, `{"arguments":"text"}`, `{"arguments":null}`, `{"arguments":42}`, `{}`} {
		requireRequestRefusal(t, codec, json.RawMessage(raw), raw)
	}
	encoded, err := codec.EncodeRequest(api.ToolInvokeRequest{Arguments: json.RawMessage(`{"path":"README.md","nested":{"free":true}}`)})
	if err != nil {
		t.Fatalf("tool.invoke refused object arguments: %v", err)
	}
	if _, err := decodeRequestPayload(codec, encoded); err != nil {
		t.Fatalf("tool.invoke refused object arguments on decode: %v", err)
	}
}

// requireJSONObject asserts that value is exactly one JSON object.
func requireJSONObject(t *testing.T, raw json.RawMessage, what string) {
	t.Helper()
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") || !json.Valid([]byte(trimmed)) {
		t.Fatalf("%s is not a JSON object: %q", what, trimmed)
	}
}

// requireJSONEquivalent asserts that two JSON values are semantically equal.
func requireJSONEquivalent(t *testing.T, got, want json.RawMessage, what string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("%s is not valid JSON: %v", what, err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("vector is not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("%s = %s; want the vector %s", what, got, want)
	}
}

// collectJSONKeys returns every object member name in a decoded JSON value.
func collectJSONKeys(value any) map[string]bool {
	keys := make(map[string]bool)
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			keys[key] = true
			for nested := range collectJSONKeys(child) {
				keys[nested] = true
			}
		}
	case []any:
		for _, child := range typed {
			for nested := range collectJSONKeys(child) {
				keys[nested] = true
			}
		}
	}
	return keys
}

// TestTaskCreateMetadataAndOptionalFields pins that task.create accepts
// bounded metadata and an omitted description/pipeline, while
// task.create_and_start still requires a pipeline.
func TestTaskCreateMetadataAndOptionalFields(t *testing.T) {
	create := codecFor(t, "task.create")
	for _, request := range []api.TaskCreateRequest{
		{Title: "ship it"},
		{Title: "ship it", Metadata: map[string]string{"stage": "build"}},
		{Title: "ship it", Description: "optional", PipelineID: "pipe-1", Inputs: map[string]string{"branch": "main"}},
	} {
		encoded, err := create.EncodeRequest(request)
		if err != nil {
			t.Fatalf("task.create refused %+v: %v", request, err)
		}
		decoded, err := create.DecodeRequest(encoded)
		if err != nil {
			t.Fatalf("task.create re-decode %s: %v", encoded, err)
		}
		if got := decoded.(api.TaskCreateRequest); !reflect.DeepEqual(got, request) {
			t.Errorf("task.create round trip = %+v; want %+v", got, request)
		}
	}
	if _, err := create.EncodeRequest(api.TaskCreateRequest{Title: "x", Metadata: map[string]string{"big": strings.Repeat("v", api.MaxTaskMetadataBytes)}}); err == nil {
		t.Error("task.create accepted oversized metadata")
	} else {
		requirePublicError(t, err, api.CodeInvalidInput)
	}

	createAndStart := codecFor(t, "task.create_and_start")
	if _, err := createAndStart.EncodeRequest(api.TaskCreateAndStartRequest{Title: "x", PipelineID: "pipe-1", Metadata: map[string]string{"stage": "build"}}); err != nil {
		t.Errorf("task.create_and_start refused optional description with metadata: %v", err)
	}
	if _, err := createAndStart.EncodeRequest(api.TaskCreateAndStartRequest{Title: "x"}); err == nil {
		t.Error("task.create_and_start accepted a missing pipeline")
	}

	edit := codecFor(t, "task.edit")
	if _, err := edit.EncodeRequest(api.TaskEditRequest{TaskID: "tsk-1", Title: "x", Metadata: map[string]string{"stage": "review"}}); err != nil {
		t.Errorf("task.edit refused optional description with metadata: %v", err)
	}
}

// TestTaskReadCarriesReplyInputs pins that task.read carries the complete task
// content and the outstanding question, and task.reply's question_id and
// expected_question_version derive from that read.
func TestTaskReadCarriesReplyInputs(t *testing.T) {
	readCodec := codecFor(t, "task.read")
	value, err := readCodec.DecodeResult(loadOperationVectors(t)["task.read"].Result)
	if err != nil {
		t.Fatalf("decode task.read golden: %v", err)
	}
	read := value.(api.TaskReadResult)
	if read.Initiator == "" || read.CurrentExecution == "" || len(read.Messages) == 0 || len(read.ExecutionHistory) == 0 {
		t.Fatalf("task.read does not carry the complete task content: %+v", read)
	}
	if read.Metadata["stage"] != "build" || read.Inputs["branch"] != "main" {
		t.Fatalf("task.read metadata/inputs = %+v/%+v", read.Metadata, read.Inputs)
	}
	if read.Question == nil {
		t.Fatal("task.read does not carry the outstanding question")
	}
	if read.Question.QuestionID != "qst-1" || read.Question.Version != 1 || read.Question.Assignment != "role/reviewer" {
		t.Fatalf("task.read question = %+v", read.Question)
	}

	replyCodec := codecFor(t, "task.reply")
	reply := api.TaskReplyRequest{
		QuestionID:              read.Question.QuestionID,
		ExpectedQuestionVersion: read.Question.Version,
		Answer:                  "main",
	}
	encoded, err := replyCodec.EncodeRequest(reply)
	if err != nil {
		t.Fatalf("task.reply refused a reply derived from task.read: %v", err)
	}
	if _, err := replyCodec.DecodeRequest(encoded); err != nil {
		t.Fatalf("task.reply re-decode: %v", err)
	}
}

// TestTaskHistoryKindDiscrimination pins that execution entries require their
// identity/state, while message/question entries carry their payload instead.
func TestTaskHistoryKindDiscrimination(t *testing.T) {
	message := api.TaskMessage{Author: "prn-1", At: time.Unix(1, 0).UTC(), Body: "hello"}
	question := api.TaskQuestion{QuestionID: "qst-1", Body: "which?", Assignment: "role/reviewer", Version: 1}
	valid := []api.TaskHistoryEntry{
		{Kind: "execution", ExecutionID: "exe-1", State: "succeeded", At: time.Unix(1, 0).UTC()},
		{Kind: "message", Message: &message, At: time.Unix(1, 0).UTC()},
		{Kind: "question", Question: &question, At: time.Unix(1, 0).UTC()},
	}
	for _, entry := range valid {
		if err := entry.Validate(); err != nil {
			t.Errorf("valid %s history entry refused: %v", entry.Kind, err)
		}
	}
	invalid := []api.TaskHistoryEntry{
		{Kind: "execution", State: "succeeded", At: time.Unix(1, 0).UTC()},
		{Kind: "execution", ExecutionID: "exe-1", At: time.Unix(1, 0).UTC()},
		{Kind: "message", At: time.Unix(1, 0).UTC()},
		{Kind: "message", Message: &message, ExecutionID: "exe-1", At: time.Unix(1, 0).UTC()},
		{Kind: "question", At: time.Unix(1, 0).UTC()},
		{Kind: "question", Question: &question, State: "failed", At: time.Unix(1, 0).UTC()},
	}
	for _, entry := range invalid {
		if err := entry.Validate(); err == nil {
			t.Errorf("invalid %s history entry was accepted: %+v", entry.Kind, entry)
		}
	}
	codec := codecFor(t, "task.history")
	value, err := codec.DecodeResult(loadOperationVectors(t)["task.history"].Result)
	if err != nil {
		t.Fatalf("decode task.history golden: %v", err)
	}
	if entries := value.(api.TaskHistoryResult).Entries; len(entries) != 3 {
		t.Fatalf("task.history golden has %d entries; want execution/message/question", len(entries))
	}
}

// TestTaskAskResultCarriesQuestionID pins that task.ask may return the
// question identity task.reply needs, and that the member is optional: the
// capability's AskResult has no question id, so a handler that cannot read it
// omits the member and clients learn the outstanding question from task.read.
func TestTaskAskResultCarriesQuestionID(t *testing.T) {
	withID := api.TaskAskResult{QuestionID: "qst-1", LifecycleVersion: 4, QuestionVersion: 1}
	if err := withID.Validate(); err != nil {
		t.Fatalf("task.ask result with question_id refused: %v", err)
	}
	withoutID := api.TaskAskResult{LifecycleVersion: 4, QuestionVersion: 1}
	if err := withoutID.Validate(); err != nil {
		t.Fatalf("task.ask result without question_id refused: %v", err)
	}
	if err := (api.TaskAskResult{QuestionID: "bad/id", LifecycleVersion: 4, QuestionVersion: 1}).Validate(); err == nil {
		t.Error("task.ask result with a malformed question_id was accepted")
	}
	if err := (api.TaskAskResult{QuestionVersion: 1}).Validate(); err == nil {
		t.Error("task.ask result without a lifecycle version was accepted")
	}

	codec := codecFor(t, "task.ask")
	value, err := codec.DecodeResult(loadOperationVectors(t)["task.ask"].Result)
	if err != nil {
		t.Fatalf("decode task.ask golden: %v", err)
	}
	if got := value.(api.TaskAskResult).QuestionID; got != "qst-1" {
		t.Errorf("task.ask golden question_id = %q", got)
	}

	encoded, err := codec.EncodeResult(withoutID)
	if err != nil {
		t.Fatalf("encode task.ask result without question_id: %v", err)
	}
	if strings.Contains(string(encoded), "question_id") {
		t.Errorf("task.ask encoded an omitted question_id: %s", encoded)
	}
	decoded, err := codec.DecodeResult(encoded)
	if err != nil {
		t.Fatalf("decode task.ask result without question_id: %v", err)
	}
	if got := decoded.(api.TaskAskResult); got != withoutID {
		t.Errorf("task.ask result without question_id round trip = %+v; want %+v", got, withoutID)
	}
	reEncoded, err := codec.EncodeResult(withID)
	if err != nil {
		t.Fatalf("encode task.ask result with question_id: %v", err)
	}
	reDecoded, err := codec.DecodeResult(reEncoded)
	if err != nil {
		t.Fatalf("decode task.ask result with question_id: %v", err)
	}
	if got := reDecoded.(api.TaskAskResult); got != withID {
		t.Errorf("task.ask result with question_id round trip = %+v; want %+v", got, withID)
	}
}

// TestEventOperationDTOs pins the event DTOs against the capability commands:
// publish carries only client-chosen facts and returns identities plus the
// replay fact; read carries the opaque cursor, bounded limit and lifecycle
// filter and returns a page; subscribe carries optional types and a resume
// cursor and returns the scope-qualified subjects, cursor and expiry.
func TestEventOperationDTOs(t *testing.T) {
	t.Run("publish", func(t *testing.T) {
		codec := codecFor(t, "event.publish")
		base := decodeObject(t, loadOperationVectors(t)["event.publish"].Request)
		if _, err := decodeRequestPayload(codec, encodeObject(t, base)); err != nil {
			t.Fatalf("event.publish golden request refused: %v", err)
		}

		for _, value := range []string{`""`, `"task.created"`, `"resource.verified"`, `"request.activated"`} {
			mutated := cloneObject(base)
			mutated["type"] = json.RawMessage(value)
			requireRequestRefusal(t, codec, encodeObject(t, mutated), "with type "+value)
		}
		withReport := cloneObject(base)
		withReport["report_type"] = json.RawMessage(`"git.ref.updated"`)
		requireRequestRefusal(t, codec, encodeObject(t, withReport), "with a custom observation report_type")

		for _, member := range []string{"id", "at", "source", "scope", "replayed"} {
			unknown := cloneObject(base)
			unknown[member] = json.RawMessage(`"server-derived"`)
			requireRequestRefusal(t, codec, encodeObject(t, unknown), "with the server-derived member "+member)
		}

		emptySubject := cloneObject(base)
		emptySubject["subject_id"] = json.RawMessage(`""`)
		requireRequestRefusal(t, codec, encodeObject(t, emptySubject), "with an empty subject_id")
		oversizedSubject := cloneObject(base)
		oversizedSubject["subject_id"] = json.RawMessage(`"` + strings.Repeat("s", api.MaxSubjectValueBytes+1) + `"`)
		requireRequestRefusal(t, codec, encodeObject(t, oversizedSubject), "with an oversized subject_id")

		atBound := cloneObject(base)
		atBound["data"] = json.RawMessage(`"` + base64.StdEncoding.EncodeToString(make([]byte, api.MaxEventDataBytes)) + `"`)
		if _, err := decodeRequestPayload(codec, encodeObject(t, atBound)); err != nil {
			t.Errorf("event.publish refused data at the %d-byte bound: %v", api.MaxEventDataBytes, err)
		}
		overBound := cloneObject(base)
		overBound["data"] = json.RawMessage(`"` + base64.StdEncoding.EncodeToString(make([]byte, api.MaxEventDataBytes+1)) + `"`)
		requireRequestRefusal(t, codec, encodeObject(t, overBound), "with oversized data")

		for _, replayed := range []bool{false, true} {
			result := api.EventPublishResult{EventID: "evt-0001", ReceiptID: "rcp-0001", Replayed: replayed}
			encoded, err := codec.EncodeResult(result)
			if err != nil {
				t.Fatalf("encode event.publish result (replayed=%v): %v", replayed, err)
			}
			decoded, err := codec.DecodeResult(encoded)
			if err != nil {
				t.Fatalf("decode event.publish result (replayed=%v): %v", replayed, err)
			}
			if got := decoded.(api.EventPublishResult); got != result {
				t.Errorf("event.publish result round trip = %+v; want %+v", got, result)
			}
		}
		if err := (api.EventPublishResult{ReceiptID: "rcp-0001"}).Validate(); err == nil {
			t.Error("event.publish result without an event id was accepted")
		}
		if err := (api.EventPublishResult{EventID: "evt-0001"}).Validate(); err == nil {
			t.Error("event.publish result without a receipt id was accepted")
		}
	})

	t.Run("read", func(t *testing.T) {
		codec := codecFor(t, "event.read")
		cursor, err := api.EncodeCursor("2")
		if err != nil {
			t.Fatalf("EncodeCursor(2): %v", err)
		}
		request := api.EventReadRequest{ProjectID: "prj-1", Page: &api.Page{Cursor: cursor, Limit: 2}, Types: []string{"task.created"}}
		encoded, err := codec.EncodeRequest(request)
		if err != nil {
			t.Fatalf("encode event.read request: %v", err)
		}
		decoded, err := codec.DecodeRequest(encoded)
		if err != nil {
			t.Fatalf("decode event.read request: %v", err)
		}
		if got := decoded.(api.EventReadRequest); got.Page == nil || got.Page.Cursor != cursor || got.Page.Limit != 2 || len(got.Types) != 1 {
			t.Errorf("event.read request round trip = %+v; want %+v", got, request)
		}
		for _, raw := range []string{
			`{"page":{"cursor":"2"}}`,
			`{"page":{"cursor":"not base64!!"}}`,
			`{"page":{"cursor":"` + strings.Repeat("A", api.MaxCursorBytes+1) + `"}}`,
		} {
			requireRequestRefusal(t, codec, json.RawMessage(raw), "with cursor "+raw)
		}
		nonDecimal, err := api.EncodeCursor("not-a-number")
		if err != nil {
			t.Fatalf("EncodeCursor(non-decimal): %v", err)
		}
		requireRequestRefusal(t, codec, json.RawMessage(`{"page":{"cursor":"`+nonDecimal+`"}}`), "with a non-decimal cursor")
		nonCanonical, err := api.EncodeCursor("01")
		if err != nil {
			t.Fatalf("EncodeCursor(01): %v", err)
		}
		requireRequestRefusal(t, codec, json.RawMessage(`{"page":{"cursor":"`+nonCanonical+`"}}`), "with a non-canonical decimal cursor")
		for _, limit := range []string{`-1`, `201`} {
			requireRequestRefusal(t, codec, json.RawMessage(`{"page":{"limit":`+limit+`}}`), "with limit "+limit)
		}
		for _, raw := range []string{`{"page":{"limit":0}}`, `{"page":{"limit":200}}`, `{"types":[]}`, `{"types":["task.created","execution.failed"]}`} {
			if _, err := decodeRequestPayload(codec, json.RawMessage(raw)); err != nil {
				t.Errorf("event.read refused %s: %v", raw, err)
			}
		}
		requireRequestRefusal(t, codec, json.RawMessage(`{"types":["task.teleported"]}`), "with an unknown type filter")
		requireRequestRefusal(t, codec, json.RawMessage(`{"types":["request.activated"]}`), "with the internal lifecycle alias")

		next, err := api.EncodeCursor("3")
		if err != nil {
			t.Fatalf("EncodeCursor(3): %v", err)
		}
		envelope := api.Event{
			ID:        "evt-0001",
			Type:      "observation.custom",
			Source:    api.EventSource{Kind: "shell", PrincipalID: "prn-1"},
			At:        time.Unix(1700000000, 0).UTC(),
			Scope:     api.EventScope{TenantID: "ten-1", ProjectID: "prj-1"},
			SubjectID: "tsk-1",
			Data:      []byte(`{"ok":true}`),
		}
		result := api.EventReadResult{Events: []api.Event{envelope}, Next: next, HasMore: true}
		encodedResult, err := codec.EncodeResult(result)
		if err != nil {
			t.Fatalf("encode event.read result: %v", err)
		}
		decodedResult, err := codec.DecodeResult(encodedResult)
		if err != nil {
			t.Fatalf("decode event.read result: %v", err)
		}
		if got := decodedResult.(api.EventReadResult); !reflect.DeepEqual(got, result) {
			t.Errorf("event.read result round trip = %+v; want %+v", got, result)
		}
		empty := api.EventReadResult{Events: []api.Event{}, Next: cursor, HasMore: false}
		if err := empty.Validate(); err != nil {
			t.Errorf("event.read empty page refused: %v", err)
		}
		for _, raw := range []string{
			`{"events":[],"has_more":false}`,
			`{"events":[],"next":"","has_more":false}`,
			`{"events":[],"next":"3","has_more":false}`,
			`{"events":[],"next":"not base64!!","has_more":false}`,
		} {
			requireResultRefusal(t, codec, json.RawMessage(raw), "with next "+raw)
		}
		badEvent := result
		badEvent.Events = []api.Event{envelope}
		badEvent.Events[0].Type = "task.teleported"
		encodedBad, err := json.Marshal(badEvent)
		if err != nil {
			t.Fatalf("marshal invalid event page: %v", err)
		}
		requireResultRefusal(t, codec, encodedBad, "with an invalid nested event")
	})

	t.Run("subscribe", func(t *testing.T) {
		codec := codecFor(t, "event.subscribe")
		cursor, err := api.EncodeCursor("0")
		if err != nil {
			t.Fatalf("EncodeCursor(0): %v", err)
		}
		request := api.EventSubscribeRequest{ProjectID: "prj-1", Types: []string{"task.created"}, Cursor: cursor}
		encoded, err := codec.EncodeRequest(request)
		if err != nil {
			t.Fatalf("encode event.subscribe request: %v", err)
		}
		decoded, err := codec.DecodeRequest(encoded)
		if err != nil {
			t.Fatalf("decode event.subscribe request: %v", err)
		}
		if got := decoded.(api.EventSubscribeRequest); got.Cursor != cursor || len(got.Types) != 1 {
			t.Errorf("event.subscribe request round trip = %+v; want %+v", got, request)
		}
		requireRequestRefusal(t, codec, json.RawMessage(`{"cursor":"0"}`), "with a raw decimal cursor")
		requireRequestRefusal(t, codec, json.RawMessage(`{"types":["task.teleported"]}`), "with an unknown type filter")

		at := time.Unix(1700003600, 0).UTC()
		result := api.EventSubscribeResult{
			Subjects:  []string{"kodama.v1.ten-1.prj-1.events"},
			Cursor:    cursor,
			ExpiresAt: at,
		}
		encodedResult, err := codec.EncodeResult(result)
		if err != nil {
			t.Fatalf("encode event.subscribe result: %v", err)
		}
		decodedResult, err := codec.DecodeResult(encodedResult)
		if err != nil {
			t.Fatalf("decode event.subscribe result: %v", err)
		}
		if got := decodedResult.(api.EventSubscribeResult); !reflect.DeepEqual(got, result) {
			t.Errorf("event.subscribe result round trip = %+v; want %+v", got, result)
		}
		tenantWide := result
		tenantWide.Subjects = []string{"kodama.v1.ten-1._.events"}
		if err := tenantWide.Validate(); err != nil {
			t.Errorf("event.subscribe refused the tenant-wide subject: %v", err)
		}

		tooMany := result
		tooMany.Subjects = make([]string, api.MaxSubscribeSubjects+1)
		for index := range tooMany.Subjects {
			tooMany.Subjects[index] = "kodama.v1.ten-1.prj-1.events"
		}
		if err := tooMany.Validate(); err == nil {
			t.Error("event.subscribe accepted more than MaxSubscribeSubjects subjects")
		}
		for _, subjects := range [][]string{
			{},
			{"kodama.v1.ten-1.prj-1.events", "kodama.v1.ten-2.prj-1.events"},
			{"kodama.v1.ten-1.*.events"},
			{"kodama.v1.ten-1.prj-1"},
			{"kodama.v1.ten-1.prj-1.events.extra"},
			{"kodama.v2.ten-1.prj-1.events"},
		} {
			mutated := result
			mutated.Subjects = subjects
			if err := mutated.Validate(); err == nil {
				t.Errorf("event.subscribe accepted subjects %v", subjects)
			}
		}
		zeroExpiry := result
		zeroExpiry.ExpiresAt = time.Time{}
		if err := zeroExpiry.Validate(); err == nil {
			t.Error("event.subscribe accepted a zero expiry")
		}
		rawCursor := result
		rawCursor.Cursor = "0"
		if err := rawCursor.Validate(); err == nil {
			t.Error("event.subscribe accepted a raw decimal cursor")
		}
	})
}

// TestInboxPerKindIdentities pins that identities are optional per kind, every
// item carries its assignment grammar where applicable, and an empty kinds
// list asks for every kind.
func TestInboxPerKindIdentities(t *testing.T) {
	at := time.Unix(1, 0).UTC()
	// The waiting projection is complete so every negative waiting vector
	// below is refused for exactly the rule it isolates, never for a missing
	// question identity, body, references or lifecycle version.
	withProjection := func(item api.InboxItem) api.InboxItem {
		item.QuestionID = "qst-1"
		item.Question = "which base?"
		item.References = []string{"sha256:1111111111111111111111111111111111111111111111111111111111111111"}
		item.LifecycleVersion = 2
		return item
	}
	withSubject := func(item api.InboxItem) api.InboxItem {
		item.Subject = &api.Subject{Fields: []api.SubjectField{{Name: "seed", Kind: "string", Value: "seed-1"}}}
		return item
	}
	valid := []api.InboxItem{
		withSubject(api.InboxItem{Kind: "request", TaskID: "tsk-1", ExecutionID: "exe-1", ActionID: "act-1", Assignment: "role/reviewer", Version: 1, At: at}),
		withSubject(api.InboxItem{Kind: "request", ExecutionID: "exe-1", ActionID: "act-1", Assignment: "service/svc-1", Version: 1, At: at}),
		withProjection(api.InboxItem{Kind: "waiting_task", TaskID: "tsk-1", Assignment: "role/reviewer", Version: 1, At: at}),
		{Kind: "failed_task", TaskID: "tsk-1", Version: 1, At: at},
	}
	for _, item := range valid {
		if err := item.Validate(); err != nil {
			t.Errorf("valid %s inbox item refused: %v", item.Kind, err)
		}
	}
	invalid := []struct {
		name string
		item api.InboxItem
	}{
		{"request without execution_id", api.InboxItem{Kind: "request", ActionID: "act-1", Assignment: "role/reviewer", Version: 1, At: at}},
		{"request without action_id", api.InboxItem{Kind: "request", ExecutionID: "exe-1", Assignment: "role/reviewer", Version: 1, At: at}},
		{"request without assignment", api.InboxItem{Kind: "request", ExecutionID: "exe-1", ActionID: "act-1", Version: 1, At: at}},
		{"request with a malformed task_id", api.InboxItem{Kind: "request", TaskID: "bad/id", ExecutionID: "exe-1", ActionID: "act-1", Assignment: "role/reviewer", Version: 1, At: at}},
		{"waiting_task without task_id", withProjection(api.InboxItem{Kind: "waiting_task", Assignment: "role/reviewer", Version: 1, At: at})},
		{"waiting_task with an execution_id", withProjection(api.InboxItem{Kind: "waiting_task", TaskID: "tsk-1", ExecutionID: "exe-1", Assignment: "role/reviewer", Version: 1, At: at})},
		{"waiting_task with an action_id", withProjection(api.InboxItem{Kind: "waiting_task", TaskID: "tsk-1", ActionID: "act-1", Assignment: "role/reviewer", Version: 1, At: at})},
		{"waiting_task with a malformed assignment", withProjection(api.InboxItem{Kind: "waiting_task", TaskID: "tsk-1", Assignment: "reviewer", Version: 1, At: at})},
		{"waiting_task with a zero version", withProjection(api.InboxItem{Kind: "waiting_task", TaskID: "tsk-1", Assignment: "role/reviewer", Version: 0, At: at})},
		{"waiting_task with a zero at", withProjection(api.InboxItem{Kind: "waiting_task", TaskID: "tsk-1", Assignment: "role/reviewer", Version: 1})},
		{"unknown kind", api.InboxItem{Kind: "everything", Version: 1, At: at}},
		{"failed_task without task_id", api.InboxItem{Kind: "failed_task", Version: 1, At: at}},
		{"failed_task with an execution_id", api.InboxItem{Kind: "failed_task", TaskID: "tsk-1", ExecutionID: "exe-1", Version: 1, At: at}},
		{"failed_task with an action_id", api.InboxItem{Kind: "failed_task", TaskID: "tsk-1", ActionID: "act-1", Version: 1, At: at}},
		{"failed_task with a malformed assignment", api.InboxItem{Kind: "failed_task", TaskID: "tsk-1", Assignment: "reviewer", Version: 1, At: at}},
	}
	for _, testCase := range invalid {
		if err := testCase.item.Validate(); err == nil {
			t.Errorf("%s was accepted: %+v", testCase.name, testCase.item)
		}
	}
	if err := (api.TaskInboxRequest{}).Validate(); err != nil {
		t.Errorf("empty kinds request refused: %v", err)
	}
	if err := (api.TaskInboxRequest{Kinds: []string{}}).Validate(); err != nil {
		t.Errorf("explicit empty kinds request refused: %v", err)
	}
	if err := (api.TaskInboxRequest{Kinds: []string{"everything"}}).Validate(); err == nil {
		t.Error("unknown inbox kind was accepted")
	}
	if err := (api.TaskInboxRequest{ProjectID: "bad/id"}).Validate(); err == nil {
		t.Error("task.inbox accepted a malformed project_id")
	}
	if err := (api.TaskInboxRequest{Page: &api.Page{Limit: -1}}).Validate(); err == nil {
		t.Error("task.inbox accepted a negative page limit")
	}
	if err := (api.TaskInboxRequest{Page: &api.Page{Limit: api.MaxPageLimit}}).Validate(); err != nil {
		t.Errorf("task.inbox refused a page limit at the bound: %v", err)
	}
	if err := (api.TaskInboxResult{NextCursor: strings.Repeat("A", api.MaxCursorBytes)}).Validate(); err != nil {
		t.Errorf("task.inbox refused a next_cursor at the bound: %v", err)
	}
	if err := (api.TaskInboxResult{NextCursor: strings.Repeat("A", api.MaxCursorBytes+1)}).Validate(); err == nil {
		t.Error("task.inbox accepted a next_cursor above the bound")
	}
	if err := (api.TaskInboxResult{Items: []api.InboxItem{{Kind: "everything", Version: 1, At: at}}}).Validate(); err == nil {
		t.Error("task.inbox result accepted an invalid item")
	}
	codec := codecFor(t, "task.inbox")
	value, err := codec.DecodeResult(loadOperationVectors(t)["task.inbox"].Result)
	if err != nil {
		t.Fatalf("decode task.inbox golden: %v", err)
	}
	if items := value.(api.TaskInboxResult).Items; len(items) != 3 {
		t.Fatalf("task.inbox golden has %d items; want request/waiting_task/failed_task", len(items))
	}
}

// TestExecutionResolveAndInspect pins the action-only resolve: the request
// names only the expected action version (the shared envelope keeps the
// expected execution version), and inspection exposes recorded outputs,
// decision facts, deadline/stop evidence, failure and pending cleanup.
func TestExecutionResolveAndInspect(t *testing.T) {
	resolve := codecFor(t, "execution.request.resolve")
	value, err := resolve.DecodeRequest(loadOperationVectors(t)["execution.request.resolve"].Request)
	if err != nil {
		t.Fatalf("decode resolve golden: %v", err)
	}
	request := value.(api.ExecutionRequestResolveRequest)
	if request.ExpectedActionVersion != 2 {
		t.Fatalf("resolve expected_action_version = %d; want 2", request.ExpectedActionVersion)
	}
	actionOnly := api.ExecutionRequestResolveRequest{ActionID: "act-1", ExpectedActionVersion: 2, Decision: "pass"}
	if err := actionOnly.Validate(); err != nil {
		t.Errorf("resolve without an execution-version body member was refused: %v", err)
	}
	if err := (api.ExecutionRequestResolveRequest{ActionID: "act-1", Decision: "pass"}).Validate(); err == nil {
		t.Error("resolve without an expected action version was accepted")
	}
	if _, err := resolve.DecodeRequest([]byte(`{"action_id":"act-1","expected_execution_version":3,"expected_action_version":2,"decision":"pass"}`)); err == nil {
		t.Error("resolve accepted the removed expected_execution_version body member")
	}

	inspect := codecFor(t, "execution.inspect")
	value, err = inspect.DecodeResult(loadOperationVectors(t)["execution.inspect"].Result)
	if err != nil {
		t.Fatalf("decode inspect golden: %v", err)
	}
	result := value.(api.ExecutionInspectResult)
	if result.Failure == nil || result.Failure.Kind != "runtime" {
		t.Fatalf("inspect failure = %+v", result.Failure)
	}
	if result.Cleanup.State != "pending" || result.Cleanup.LastError == "" {
		t.Fatalf("inspect cleanup = %+v", result.Cleanup)
	}
	if len(result.Actions) != 2 {
		t.Fatalf("inspect actions = %d; want 2", len(result.Actions))
	}
	build, review := result.Actions[0], result.Actions[1]
	if build.Outputs["digest"] != "sha256:abc" || build.Stop == nil || build.TimeoutSeconds != 600 || build.Deadline == nil {
		t.Fatalf("container action inspection = %+v", build)
	}
	if review.Decision != "fail" || review.Actor != "prn-1" || review.AnsweredAt == nil || review.Stop == nil {
		t.Fatalf("request action inspection = %+v", review)
	}

	taskFree := api.ExecutionInspectResult{
		ExecutionID: "exe-2", PipelineID: "pipe-1", State: "queued", Version: 1,
		CurrentAction: -1, Cleanup: api.ExecutionCleanup{State: "none"},
	}
	if err := taskFree.Validate(); err != nil {
		t.Errorf("task-free queued inspection refused: %v", err)
	}
	bad := taskFree
	bad.Cleanup.State = "forgotten"
	if err := bad.Validate(); err == nil {
		t.Error("inspection accepted an unknown cleanup state")
	}
	bad = taskFree
	bad.Failure = &api.ExecutionFailure{Kind: "unknown"}
	if err := bad.Validate(); err == nil {
		t.Error("inspection accepted an unknown failure kind")
	}
}

// TestResourceBindingMetadataAndEmptyLists pins that metadata is bounded and
// empty origin/operation/report lists are accepted.
func TestResourceBindingMetadataAndEmptyLists(t *testing.T) {
	register := codecFor(t, "resource.binding.register")
	empty := api.ResourceBindingRegisterRequest{
		BindingID: "bnd-1", ResourceID: "res-1", EnforcingService: "svc-1",
		TrustedOrigins: []string{}, Operations: []string{}, ReportableEvents: []string{},
		Metadata: map[string]string{"owner": "platform"},
	}
	encoded, err := register.EncodeRequest(empty)
	if err != nil {
		t.Fatalf("register with empty lists refused: %v", err)
	}
	if _, err := register.DecodeRequest(encoded); err != nil {
		t.Fatalf("register with empty lists re-decode: %v", err)
	}
	missing := empty
	missing.TrustedOrigins = nil
	if err := missing.Validate(); err == nil {
		t.Error("register accepted a missing trusted_origins list")
	}

	update := codecFor(t, "resource.binding.update")
	updateRequest := api.ResourceBindingUpdateRequest{
		BindingID: "bnd-1", ResourceID: "res-1", EnforcingService: "svc-1",
		TrustedOrigins: []string{}, Operations: []string{}, ReportableEvents: []string{},
		Metadata: map[string]string{"owner": "platform-team"},
	}
	if _, err := update.EncodeRequest(updateRequest); err != nil {
		t.Errorf("update with empty lists refused: %v", err)
	}

	read := codecFor(t, "resource.binding.read")
	value, err := read.DecodeResult(loadOperationVectors(t)["resource.binding.read"].Result)
	if err != nil {
		t.Fatalf("decode resource.binding.read golden: %v", err)
	}
	if got := value.(api.ResourceBindingReadResult).Metadata["owner"]; got != "platform-team" {
		t.Errorf("read metadata owner = %q", got)
	}
	readResult := api.ResourceBindingReadResult{
		BindingID: "bnd-1", ResourceID: "res-1", EnforcingService: "svc-1",
		TrustedOrigins: []string{}, Operations: []string{}, ReportableEvents: []string{}, Version: 1,
	}
	if err := readResult.Validate(); err != nil {
		t.Errorf("read with empty lists refused: %v", err)
	}

	many := make(map[string]string, api.MaxMetadataEntries+1)
	for index := 0; index <= api.MaxMetadataEntries; index++ {
		many["k"+strings.Repeat("a", index+1)] = "v"
	}
	tooMany := empty
	tooMany.Metadata = many
	if err := tooMany.Validate(); err == nil {
		t.Error("register accepted more than MaxMetadataEntries")
	}
	longKey := empty
	longKey.Metadata = map[string]string{strings.Repeat("k", api.MaxMetadataKeyBytes+1): "v"}
	if err := longKey.Validate(); err == nil {
		t.Error("register accepted an oversized metadata key")
	}
	longValue := empty
	longValue.Metadata = map[string]string{"k": strings.Repeat("v", api.MaxMetadataValueBytes+1)}
	if err := longValue.Validate(); err == nil {
		t.Error("register accepted an oversized metadata value")
	}
	atBound := empty
	atBound.Metadata = map[string]string{
		strings.Repeat("k", api.MaxMetadataKeyBytes): strings.Repeat("v", api.MaxMetadataValueBytes),
	}
	if err := atBound.Validate(); err != nil {
		t.Errorf("register refused metadata at the bound: %v", err)
	}
}

// routeParamExtraVectors supplies valid request bodies for the operations that
// have no golden in operations.json. The event operations are in the golden
// file; model.invoke is not.
var routeParamExtraVectors = map[string]json.RawMessage{
	"model.invoke": json.RawMessage(`{"project_id":"prj-1","model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`),
}

// TestRouteParamsAreRequestFields pins that every route-template path
// parameter is an explicit request field, an MCP caller must supply it, a REST
// caller may let the route fill it, a conflicting body value refuses, and the
// advertised inputSchema lists it.
func TestRouteParamsAreRequestFields(t *testing.T) {
	vectors := loadOperationVectors(t)
	for _, codec := range api.OperationCodecs() {
		t.Run(codec.Operation, func(t *testing.T) {
			route, ok := api.LookupRoute(codec.Operation)
			if !ok {
				t.Fatalf("LookupRoute(%q) refused a registry operation", codec.Operation)
			}
			names := pathParams(t, route.Path)
			byName := make(map[string]api.RouteParam, len(codec.RouteParams))
			for _, param := range codec.RouteParams {
				byName[param.Name] = param
			}
			for _, name := range names {
				param, ok := byName[name]
				if !ok {
					t.Fatalf("route parameter {%s} is not in the codec RouteParams table", name)
				}
				if !structHasJSONPath(codec.RequestType, param.Field) {
					t.Errorf("route parameter {%s} field %q is not declared by %v", name, param.Field, codec.RequestType)
				}
			}
			if len(names) == 0 {
				if len(codec.RouteParams) != 0 {
					t.Errorf("operation has no route parameters but declares %+v", codec.RouteParams)
				}
				return
			}

			base, ok := routeParamBase(vectors, codec.Operation)
			if !ok {
				t.Fatalf("no valid request body for %s", codec.Operation)
			}
			if _, err := codec.DecodeMCPRequest(base); err != nil {
				t.Fatalf("MCP decode of the complete input refused: %v", err)
			}
			inputProperties := inputSchemaProperties(t, codec.Operation)
			for _, param := range codec.RouteParams {
				if !schemaHasJSONPath(inputProperties, param.Field) {
					t.Errorf("inputSchema does not list route field %q", param.Field)
				}
			}

			routeValues := make(map[string]string, len(codec.RouteParams))
			for _, param := range codec.RouteParams {
				value, ok := jsonPathString(base, param.Field)
				if !ok || value == "" {
					t.Fatalf("golden request has no route field %q", param.Field)
				}
				routeValues[param.Name] = value
			}
			for _, param := range codec.RouteParams {
				stripped := removeJSONPath(t, base, param.Field)
				if _, err := codec.DecodeMCPRequest(stripped); err == nil {
					t.Errorf("MCP decode accepted an input without route field %q", param.Field)
				}
				bound, err := api.DecodeRESTRequest(codec.Operation, stripped, routeValues)
				if err != nil {
					t.Fatalf("REST decode without route field %q refused: %v", param.Field, err)
				}
				emptyRoute := make(map[string]string, len(routeValues))
				for name, value := range routeValues {
					emptyRoute[name] = value
				}
				emptyRoute[param.Name] = ""
				if _, err := api.DecodeRESTRequest(codec.Operation, stripped, emptyRoute); err == nil {
					t.Errorf("REST decode accepted an empty route value for %q", param.Name)
				}
				encoded, err := codec.EncodeRequest(bound)
				if err != nil {
					t.Fatalf("re-encode bound REST request: %v", err)
				}
				if got, _ := jsonPathString(encoded, param.Field); got != routeValues[param.Name] {
					t.Errorf("REST decode filled %q with %q; want %q", param.Field, got, routeValues[param.Name])
				}
				conflicting := setJSONPath(t, base, param.Field, "conflict-1")
				if _, err := api.DecodeRESTRequest(codec.Operation, conflicting, routeValues); err == nil {
					t.Errorf("REST decode accepted a body value conflicting with route parameter {%s}", param.Name)
				}
			}
		})
	}
}

// routeParamBase returns the valid request body of one operation.
func routeParamBase(vectors map[string]operationVector, operation string) (json.RawMessage, bool) {
	if vector, ok := vectors[operation]; ok {
		return vector.Request, true
	}
	raw, ok := routeParamExtraVectors[operation]
	return raw, ok
}

// inputSchemaProperties returns the typed input properties of one operation.
func inputSchemaProperties(t *testing.T, operation string) map[string]any {
	t.Helper()
	_, properties := inputSchemaFor(t, operation)
	return properties
}

// structHasJSONPath reports whether a struct type declares a dotted JSON field
// path.
func structHasJSONPath(typ reflect.Type, path string) bool {
	current := indirectType(typ)
	for _, segment := range strings.Split(path, ".") {
		if current == nil || current.Kind() != reflect.Struct {
			return false
		}
		field, ok := fieldByName(current, segment)
		if !ok {
			return false
		}
		current = indirectType(field.Type)
	}
	return true
}

// schemaHasJSONPath reports whether a schema property map lists a dotted field
// path.
func schemaHasJSONPath(properties map[string]any, path string) bool {
	current := properties
	for index, segment := range strings.Split(path, ".") {
		property, ok := current[segment].(map[string]any)
		if !ok {
			return false
		}
		if index == len(strings.Split(path, "."))-1 {
			return true
		}
		next, ok := property["properties"].(map[string]any)
		if !ok {
			return false
		}
		current = next
	}
	return false
}

// jsonPathString returns the string value at a dotted JSON path.
func jsonPathString(raw json.RawMessage, path string) (string, bool) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	current := value
	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = object[segment]
		if !ok {
			return "", false
		}
	}
	text, ok := current.(string)
	return text, ok
}

// removeJSONPath returns a copy of one JSON object with a dotted path removed.
func removeJSONPath(t *testing.T, raw json.RawMessage, path string) json.RawMessage {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	segments := strings.Split(path, ".")
	parent := value
	for _, segment := range segments[:len(segments)-1] {
		object, ok := parent.(map[string]any)
		if !ok {
			t.Fatalf("path %q is not an object", path)
		}
		parent, ok = object[segment]
		if !ok {
			t.Fatalf("path %q is missing", path)
		}
	}
	object, ok := parent.(map[string]any)
	if !ok {
		t.Fatalf("path %q parent is not an object", path)
	}
	delete(object, segments[len(segments)-1])
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode mutated request: %v", err)
	}
	return encoded
}

// setJSONPath returns a copy of one JSON object with a dotted path replaced.
func setJSONPath(t *testing.T, raw json.RawMessage, path, value string) json.RawMessage {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	segments := strings.Split(path, ".")
	parent := decoded
	for _, segment := range segments[:len(segments)-1] {
		object, ok := parent.(map[string]any)
		if !ok {
			t.Fatalf("path %q is not an object", path)
		}
		parent, ok = object[segment]
		if !ok {
			t.Fatalf("path %q is missing", path)
		}
	}
	object, ok := parent.(map[string]any)
	if !ok {
		t.Fatalf("path %q parent is not an object", path)
	}
	object[segments[len(segments)-1]] = value
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("encode mutated request: %v", err)
	}
	return encoded
}

// TestTaskReferencesAreOpaque pins that references are durable opaque
// pointers, so commits, digests and restoration URLs round trip through
// comment, ask and read without the identity grammar.
func TestTaskReferencesAreOpaque(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	restore := "https://svc.example/restore/ctx-1"
	refs := []string{digest, restore}

	comment := codecFor(t, "task.comment")
	encoded, err := comment.EncodeRequest(api.TaskCommentRequest{Body: "context", References: refs})
	if err != nil {
		t.Fatalf("task.comment refused opaque references: %v", err)
	}
	decodedComment, err := comment.DecodeRequest(encoded)
	if err != nil {
		t.Fatalf("task.comment re-decode: %v", err)
	}
	if got := decodedComment.(api.TaskCommentRequest).References; !reflect.DeepEqual(got, refs) {
		t.Errorf("comment references = %v; want %v", got, refs)
	}

	ask := codecFor(t, "task.ask")
	encoded, err = ask.EncodeRequest(api.TaskAskRequest{Question: "restore?", Assignment: "role/reviewer", References: refs})
	if err != nil {
		t.Fatalf("task.ask refused opaque references: %v", err)
	}
	if _, err := ask.DecodeRequest(encoded); err != nil {
		t.Fatalf("task.ask re-decode: %v", err)
	}

	read := codecFor(t, "task.read")
	value, err := read.DecodeResult(loadOperationVectors(t)["task.read"].Result)
	if err != nil {
		t.Fatalf("task.read refused opaque references: %v", err)
	}
	result := value.(api.TaskReadResult)
	if len(result.Messages) == 0 || !reflect.DeepEqual(result.Messages[0].References, refs) {
		t.Errorf("task.read message references = %+v; want %v", result.Messages, refs)
	}
	if result.Question == nil || !reflect.DeepEqual(result.Question.References, refs) {
		t.Errorf("task.read question references = %+v; want %v", result.Question, refs)
	}

	for _, bad := range [][]string{{""}, {strings.Repeat("x", api.MaxReferenceBytes+1)}} {
		if err := (api.TaskCommentRequest{Body: "x", References: bad}).Validate(); err == nil {
			t.Errorf("task.comment accepted reference %q", bad)
		}
	}
	tooMany := make([]string, api.MaxReferences+1)
	for index := range tooMany {
		tooMany[index] = "ctx-" + strings.Repeat("a", index+1)
	}
	if err := (api.TaskAskRequest{Question: "x", Assignment: "role/reviewer", References: tooMany}).Validate(); err == nil {
		t.Errorf("task.ask accepted %d references; bound is %d", len(tooMany), api.MaxReferences)
	}
}

// TestPipelineApplyUsesName pins that pipeline.apply addresses a pipeline by
// its domain name, while the other pipeline operations keep the id grammar.
func TestPipelineApplyUsesName(t *testing.T) {
	if _, ok := reflect.TypeOf(api.PipelineApplyRequest{}).FieldByName("PipelineID"); ok {
		t.Error("PipelineApplyRequest still carries pipeline_id; apply addresses the pipeline name")
	}
	apply := codecFor(t, "pipeline.apply")
	valid := api.PipelineApplyRequest{Name: "build.v2", YAML: []byte("pipeline: build")}
	encoded, err := apply.EncodeRequest(valid)
	if err != nil {
		t.Fatalf("pipeline.apply refused name build.v2: %v", err)
	}
	if _, err := apply.DecodeRequest(encoded); err != nil {
		t.Fatalf("pipeline.apply re-decode: %v", err)
	}
	for _, name := range []string{"", ".", "..", "bad/name", strings.Repeat("a", 129)} {
		if err := (api.PipelineApplyRequest{Name: name, YAML: []byte("x")}).Validate(); err == nil {
			t.Errorf("pipeline.apply accepted name %q", name)
		}
	}
	if err := (api.PipelineApplyRequest{Name: strings.Repeat("a", 128), YAML: []byte("x")}).Validate(); err != nil {
		t.Errorf("pipeline.apply refused a 128-byte name: %v", err)
	}

	routeParams := map[string]string{"project": "prj-1", "pipeline": "build.v2"}
	body := json.RawMessage(`{"yaml":"cGlwZWxpbmU6IGJ1aWxk"}`)
	value, err := api.DecodeRESTRequest("pipeline.apply", body, routeParams)
	if err != nil {
		t.Fatalf("REST apply without a body name refused: %v", err)
	}
	if got := value.(api.PipelineApplyRequest).Name; got != "build.v2" {
		t.Errorf("REST apply filled name = %q; want build.v2", got)
	}
	if _, err := apply.DecodeMCPRequest(body); err == nil {
		t.Error("MCP apply without a name was accepted")
	}
	conflict := json.RawMessage(`{"name":"other","yaml":"cGlwZWxpbmU6IGJ1aWxk"}`)
	if _, err := api.DecodeRESTRequest("pipeline.apply", conflict, routeParams); err == nil {
		t.Error("REST apply accepted a body name conflicting with the route")
	}

	for _, id := range []string{"pipe-1", "build_v2"} {
		if err := (api.PipelineReadRequest{PipelineID: id}).Validate(); err != nil {
			t.Errorf("pipeline.read refused id %q: %v", id, err)
		}
	}
	if err := (api.PipelineReadRequest{PipelineID: "build.v2"}).Validate(); err == nil {
		t.Error("pipeline.read accepted a dotted name as a pipeline id")
	}
	if err := (api.PipelineUpdateRequest{PipelineID: "build.v2", Name: "build.v2", YAML: []byte("x")}).Validate(); err == nil {
		t.Error("pipeline.update accepted a dotted name as a pipeline id")
	}
}

// TestOpaqueEventSubjectAndToolName pins that event subject ids and configured
// tool names are opaque bounded strings, not Kodama ids.
func TestOpaqueEventSubjectAndToolName(t *testing.T) {
	for _, subject := range []string{"sha256:" + strings.Repeat("a", 64), "https://svc.example/restore/ctx-1", "repo@refs/heads/main"} {
		event := api.Event{
			ID: "evt-1", Type: "observation.custom",
			Source:    api.EventSource{Kind: "shell", PrincipalID: "prn-1"},
			At:        time.Unix(1, 0).UTC(),
			Scope:     api.EventScope{TenantID: "ten-1", ProjectID: "prj-1"},
			SubjectID: subject,
		}
		if err := event.Validate(); err != nil {
			t.Errorf("event subject %q refused: %v", subject, err)
		}
	}
	if err := (api.Event{ID: "evt-1", Type: "observation.custom", Source: api.EventSource{Kind: "shell", PrincipalID: "prn-1"}, At: time.Unix(1, 0).UTC(), Scope: api.EventScope{TenantID: "ten-1"}, SubjectID: strings.Repeat("s", api.MaxSubjectValueBytes+1)}).Validate(); err == nil {
		t.Error("event accepted an oversized subject id")
	}
	for _, tool := range []string{"git.tool", "repo@refs/heads/main", "tool:v1"} {
		if err := (api.ToolInvokeRequest{Tool: tool, Arguments: json.RawMessage(`{}`)}).Validate(); err != nil {
			t.Errorf("tool.invoke refused opaque tool name %q: %v", tool, err)
		}
	}
	if err := (api.ToolInvokeRequest{Tool: strings.Repeat("t", api.MaxSubjectValueBytes+1), Arguments: json.RawMessage(`{}`)}).Validate(); err == nil {
		t.Error("tool.invoke accepted an oversized tool name")
	}
	if err := (api.Subject{Fields: []api.SubjectField{{Name: strings.Repeat("n", 200), Kind: "string", Value: "x"}}}).Validate(); err != nil {
		t.Errorf("subject field name longer than an id was refused: %v", err)
	}
}

// TestDomainAcceptedEmptyRoleSetsAndLargeInputs pins that the domain accepts a
// present-but-empty membership/service role set and string input values larger
// than a subject value, so the public DTOs must not reject them.
func TestDomainAcceptedEmptyRoleSetsAndLargeInputs(t *testing.T) {
	if err := (api.MembershipGrantRequest{Roles: []string{}}).Validate(); err != nil {
		t.Errorf("membership.grant refused an empty role set: %v", err)
	}
	if err := (api.MembershipGrantRequest{}).Validate(); err == nil {
		t.Error("membership.grant accepted a missing role set")
	}
	if err := (api.ServiceCreateRequest{ServiceID: "svc-1", Roles: []string{}}).Validate(); err != nil {
		t.Errorf("service.create refused an empty role set: %v", err)
	}
	if err := (api.ServiceUpdateRequest{Roles: []string{}}).Validate(); err != nil {
		t.Errorf("service.update refused an empty role set: %v", err)
	}
	if err := (api.ServiceUpdateRequest{}).Validate(); err == nil {
		t.Error("service.update accepted a missing role set")
	}
	if err := (api.MembershipView{PrincipalID: "prn-1", Roles: []string{}, Version: 1}).Validate(); err != nil {
		t.Errorf("membership view refused an empty role set: %v", err)
	}
	large := strings.Repeat("x", api.MaxSubjectValueBytes+1)
	if err := (api.PipelineStartRequest{PipelineID: "pipe-1", Inputs: map[string]string{"note": large}}).Validate(); err != nil {
		t.Errorf("pipeline.start refused a large string input: %v", err)
	}
	if err := (api.TaskCreateRequest{Title: "x", Inputs: map[string]string{"note": large}}).Validate(); err != nil {
		t.Errorf("task.create refused a large string input: %v", err)
	}
	if err := (api.ExecutionActionOutputsRequest{Outputs: map[string]string{}}).Validate(); err != nil {
		t.Errorf("execution.action.outputs refused a present empty output object: %v", err)
	}
	if err := (api.ExecutionActionOutputsRequest{}).Validate(); err == nil {
		t.Error("execution.action.outputs accepted a missing output object")
	}
}
