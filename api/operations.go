package api

import (
	"encoding/json"
	"reflect"
)

// RouteParam is one route-derived request field. Name is the route-template
// parameter (`project`, `task`, ...); Field is the canonical request DTO field
// (a dotted JSON path when the identity already lives in a nested object).
type RouteParam struct {
	Name  string `json:"name"`
	Field string `json:"field"`
}

// OperationCodec is the strict wire codec of one registered operation. The
// table is keyed by the registry operation name and carries the schema
// names, the receipt-derived idempotency policy, the request DTO type, the
// route-derived input fields and the request/result encode/decode functions.
// RouteParams is the canonical REST binding's field table; MCPRouteParams is the
// set an MCP caller must supply (pipeline.start accepts inline YAML with only
// the project, so its stored pipeline id is not a required MCP field).
type OperationCodec struct {
	Operation              string
	InputSchema            string
	ResultSchema           string
	RequiresIdempotencyKey bool
	RequestType            reflect.Type
	RouteParams            []RouteParam
	MCPRouteParams         []RouteParam
	DecodeRequest          func(json.RawMessage) (any, error)
	DecodeMCPRequest       func(json.RawMessage) (any, error)
	EncodeRequest          func(any) (json.RawMessage, error)
	DecodeResult           func(json.RawMessage) (any, error)
	EncodeResult           func(any) (json.RawMessage, error)
}

// operationCodecSpec is the internal table row: the codec functions plus the
// request type the MCP input schema is projected from.
type operationCodecSpec struct {
	requiresIdempotencyKey bool
	inputType              reflect.Type
	decodeRequest          func(json.RawMessage) (any, error)
	decodeRequestStrict    func(json.RawMessage) (any, error)
	encodeRequest          func(any) (json.RawMessage, error)
	decodeResult           func(json.RawMessage) (any, error)
	encodeResult           func(any) (json.RawMessage, error)
}

// newOperationSpec builds one table row for a request/result DTO pair.
func newOperationSpec[TRequest interface{ Validate() error }, TResult interface{ Validate() error }](requiresIdempotencyKey bool) operationCodecSpec {
	return operationCodecSpec{
		requiresIdempotencyKey: requiresIdempotencyKey,
		inputType:              reflect.TypeOf((*TRequest)(nil)).Elem(),
		decodeRequest:          decodeOperationRequest[TRequest],
		decodeRequestStrict:    decodeOperationRequestStrict[TRequest],
		encodeRequest:          encodeOperationValue[TRequest],
		decodeResult:           decodeOperationResult[TResult],
		encodeResult:           encodeOperationValue[TResult],
	}
}

// operationCodecSpecs is the registry-keyed vocabulary table. It repeats the
// Shell operation names and receipt policy so the public schema package
// stays independent of Shell internals; the route/operation parity tests keep
// the repetition honest.
var operationCodecSpecs = map[string]operationCodecSpec{
	// Tenant, project and membership administration.
	"tenant.create":     newOperationSpec[TenantCreateRequest, TenantCreateResult](true),
	"tenant.rotate_key": newOperationSpec[TenantRotateKeyRequest, TenantRotateKeyResult](true),
	"project.create":    newOperationSpec[ProjectCreateRequest, ProjectCreateResult](true),
	"project.update":    newOperationSpec[ProjectUpdateRequest, ProjectUpdateResult](true),
	"membership.grant":  newOperationSpec[MembershipGrantRequest, MembershipGrantResult](true),
	"membership.revoke": newOperationSpec[MembershipRevokeRequest, MembershipRevokeResult](true),
	"membership.list":   newOperationSpec[MembershipListRequest, MembershipListResult](false),

	// Resource bindings.
	"resource.binding.register": newOperationSpec[ResourceBindingRegisterRequest, ResourceBindingRegisterResult](true),
	"resource.binding.update":   newOperationSpec[ResourceBindingUpdateRequest, ResourceBindingUpdateResult](true),
	"resource.binding.delete":   newOperationSpec[ResourceBindingDeleteRequest, ResourceBindingDeleteResult](true),
	"resource.binding.read":     newOperationSpec[ResourceBindingReadRequest, ResourceBindingReadResult](false),

	// Pipelines and immutable execution start.
	"pipeline.apply":  newOperationSpec[PipelineApplyRequest, PipelineApplyResult](true),
	"pipeline.read":   newOperationSpec[PipelineReadRequest, PipelineReadResult](false),
	"pipeline.update": newOperationSpec[PipelineUpdateRequest, PipelineUpdateResult](true),
	"pipeline.delete": newOperationSpec[PipelineDeleteRequest, PipelineDeleteResult](true),
	"pipeline.start":  newOperationSpec[PipelineStartRequest, PipelineStartResult](true),

	// Secret metadata and writes.
	"secret.metadata.read": newOperationSpec[SecretMetadataReadRequest, SecretMetadataReadResult](false),
	"secret.write":         newOperationSpec[SecretWriteRequest, SecretWriteResult](true),
	"secret.delete":        newOperationSpec[SecretDeleteRequest, SecretDeleteResult](true),

	// Service accounts and credential lifecycle.
	"service.create":            newOperationSpec[ServiceCreateRequest, ServiceCreateResult](true),
	"service.update":            newOperationSpec[ServiceUpdateRequest, ServiceUpdateResult](true),
	"service.enable":            newOperationSpec[ServiceEnableRequest, ServiceEnableResult](true),
	"service.disable":           newOperationSpec[ServiceDisableRequest, ServiceDisableResult](true),
	"service.credential.issue":  newOperationSpec[ServiceCredentialIssueRequest, ServiceCredentialIssueResult](true),
	"service.credential.rotate": newOperationSpec[ServiceCredentialRotateRequest, ServiceCredentialRotateResult](true),
	"service.credential.revoke": newOperationSpec[ServiceCredentialRevokeRequest, ServiceCredentialRevokeResult](true),

	// Tasks and conversation handoffs.
	"task.create":           newOperationSpec[TaskCreateRequest, TaskCreateResult](true),
	"task.read":             newOperationSpec[TaskReadRequest, TaskReadResult](false),
	"task.edit":             newOperationSpec[TaskEditRequest, TaskEditResult](true),
	"task.comment":          newOperationSpec[TaskCommentRequest, TaskCommentResult](true),
	"task.select_pipeline":  newOperationSpec[TaskSelectPipelineRequest, TaskSelectPipelineResult](true),
	"task.start":            newOperationSpec[TaskStartRequest, TaskStartResult](true),
	"task.create_and_start": newOperationSpec[TaskCreateAndStartRequest, TaskCreateAndStartResult](true),
	"task.ask":              newOperationSpec[TaskAskRequest, TaskAskResult](true),
	"task.reply":            newOperationSpec[TaskReplyRequest, TaskReplyResult](true),
	"task.cancel":           newOperationSpec[TaskCancelRequest, TaskCancelResult](true),
	"task.history":          newOperationSpec[TaskHistoryRequest, TaskHistoryResult](false),
	"task.inbox":            newOperationSpec[TaskInboxRequest, TaskInboxResult](false),

	// Executions, outputs, request resolution and retained logs.
	"execution.inspect":         newOperationSpec[ExecutionInspectRequest, ExecutionInspectResult](false),
	"execution.list":            newOperationSpec[ExecutionListRequest, ExecutionListResult](false),
	"execution.cancel":          newOperationSpec[ExecutionCancelRequest, ExecutionCancelResult](true),
	"execution.action.outputs":  newOperationSpec[ExecutionActionOutputsRequest, ExecutionActionOutputsResult](true),
	"execution.request.resolve": newOperationSpec[ExecutionRequestResolveRequest, ExecutionRequestResolveResult](true),
	"execution.logs.read":       newOperationSpec[ExecutionLogsReadRequest, ExecutionLogsReadResult](false),

	// Verification and verified resource reports. The table exposes one codec
	// entry each.
	"actor.verify":    newOperationSpec[VerifyRequest, VerifyResponse](false),
	"resource.report": newOperationSpec[ReportRequest, ReportResponse](true),

	// Custom events and authorized reads/subscriptions. Each operation has
	// its capability-shaped request/result DTO.
	"event.publish":   newOperationSpec[EventPublishRequest, EventPublishResult](true),
	"event.read":      newOperationSpec[EventReadRequest, EventReadResult](false),
	"event.subscribe": newOperationSpec[EventSubscribeRequest, EventSubscribeResult](false),

	// Optional model/tool surfaces.
	"model.invoke": newOperationSpec[ModelInvokeRequest, ChatCompletionResponse](false),
	"tool.invoke":  newOperationSpec[ToolInvokeRequest, ToolInvokeResult](false),
}

// OperationCodecs returns every registered operation codec in the registry
// declaration order. The slice is a copy; callers cannot mutate the table.
func OperationCodecs() []OperationCodec {
	codecs := make([]OperationCodec, 0, len(routes))
	for _, route := range routes {
		spec, ok := operationCodecSpecs[route.Operation]
		if !ok {
			continue
		}
		codecs = append(codecs, newOperationCodec(route, spec))
	}
	return codecs
}

// newOperationCodec builds one public codec row, including the route-derived
// field table and the MCP-mode decoder that requires those fields.
func newOperationCodec(route Route, spec operationCodecSpec) OperationCodec {
	routeParams := routeParamsFor(route.Operation)
	mcpRouteParams := mcpRouteParamsFor(route.Operation, routeParams)
	return OperationCodec{
		Operation:              route.Operation,
		InputSchema:            route.InputSchema,
		ResultSchema:           route.ResultSchema,
		RequiresIdempotencyKey: spec.requiresIdempotencyKey,
		RequestType:            spec.inputType,
		RouteParams:            routeParams,
		MCPRouteParams:         mcpRouteParams,
		DecodeRequest:          spec.decodeRequest,
		DecodeMCPRequest: func(raw json.RawMessage) (any, error) {
			value, err := spec.decodeRequestStrict(raw)
			if err != nil {
				return nil, err
			}
			if err := requireRouteParams(value, mcpRouteParams); err != nil {
				return nil, err
			}
			if validator, ok := value.(interface{ Validate() error }); ok {
				if err := validator.Validate(); err != nil {
					return nil, err
				}
			}
			return value, nil
		},
		EncodeRequest: spec.encodeRequest,
		DecodeResult:  spec.decodeResult,
		EncodeResult:  spec.encodeResult,
	}
}

// mcpRouteParamOverrides restricts the MCP-required route fields of an
// operation whose input can select a task-free alternate binding. A stored
// pipeline selection still needs pipeline_id, but that is enforced by the DTO's
// exclusive-selection validation rather than by the MCP route requirement.
var mcpRouteParamOverrides = map[string][]string{
	"pipeline.start": {"project"},
}

// mcpRouteParamsFor returns the route fields an MCP caller must supply for one
// operation.
func mcpRouteParamsFor(operation string, params []RouteParam) []RouteParam {
	allowed, ok := mcpRouteParamOverrides[operation]
	if !ok {
		return params
	}
	out := make([]RouteParam, 0, len(allowed))
	for _, param := range params {
		for _, name := range allowed {
			if param.Name == name {
				out = append(out, param)
				break
			}
		}
	}
	return out
}

// operationCodecFor returns the registry-keyed codec for one operation.
func operationCodecFor(operation string) (OperationCodec, bool) {
	route, ok := LookupRoute(operation)
	if !ok {
		return OperationCodec{}, false
	}
	spec, ok := operationCodecSpecs[operation]
	if !ok {
		return OperationCodec{}, false
	}
	return newOperationCodec(route, spec), true
}

// DecodeRESTRequest decodes one operation input for a REST call whose route
// supplied params. The body may omit a route-derived field; the route value is
// filled in. A body value that conflicts with the route refuses.
func DecodeRESTRequest(operation string, raw json.RawMessage, params map[string]string) (any, error) {
	codec, ok := operationCodecFor(operation)
	if !ok {
		return nil, invalid("unknown operation %q", operation)
	}
	spec, ok := operationCodecSpecs[operation]
	if !ok {
		return nil, invalid("unknown operation %q", operation)
	}
	value, err := spec.decodeRequestStrict(raw)
	if err != nil {
		return nil, err
	}
	bound, err := bindRouteParams(value, codec.RouteParams, params)
	if err != nil {
		return nil, err
	}
	if validator, ok := bound.(interface{ Validate() error }); ok {
		if err := validator.Validate(); err != nil {
			return nil, err
		}
	}
	return bound, nil
}

// DecodeRouteRequest decodes one operation input for a concrete REST binding
// whose route supplied params. The route must be a registered canonical or
// alternate binding of the operation, and the operation's own route params are
// derived from that binding's path, not from the canonical table: the alternate
// pipeline.start route supplies only the project. A body value that conflicts
// with the route refuses.
func DecodeRouteRequest(route Route, raw json.RawMessage, params map[string]string) (any, error) {
	spec, ok := operationCodecSpecs[route.Operation]
	if !ok {
		return nil, invalid("unknown operation %q", route.Operation)
	}
	if !registeredBinding(route) {
		return nil, invalid("route %s %s is not a registered binding of %q", route.Method, route.Path, route.Operation)
	}
	routeParams, err := routeParamsForPath(route.Operation, route.Path)
	if err != nil {
		return nil, err
	}
	value, err := spec.decodeRequestStrict(raw)
	if err != nil {
		return nil, err
	}
	bound, err := bindRouteParams(value, routeParams, params)
	if err != nil {
		return nil, err
	}
	if validator, ok := bound.(interface{ Validate() error }); ok {
		if err := validator.Validate(); err != nil {
			return nil, err
		}
	}
	return bound, nil
}

// registeredBinding reports whether route is one of the operation's declared
// bindings. It compares the operation, method and path so a caller cannot decode
// an invented route or path parameter.
func registeredBinding(route Route) bool {
	for _, candidate := range allRoutes {
		if candidate.Operation == route.Operation &&
			candidate.Method == route.Method &&
			candidate.Path == route.Path {
			return true
		}
	}
	return false
}

// routeParamsForPath returns the route-derived input fields of one registered
// binding path. A parameter without a canonical request field refuses.
func routeParamsForPath(operation, path string) ([]RouteParam, error) {
	names := routeParamNames(path)
	params := make([]RouteParam, 0, len(names))
	for _, name := range names {
		field := canonicalRouteField(operation, name)
		if field == "" {
			return nil, invalid("route parameter %q has no request field", name)
		}
		params = append(params, RouteParam{Name: name, Field: field})
	}
	return params, nil
}

// DecodeMCPOperationRequest decodes one operation input for an MCP caller. Every
// route-derived field must be supplied in the input; an omitted or empty field
// refuses.
func DecodeMCPOperationRequest(operation string, raw json.RawMessage) (any, error) {
	codec, ok := operationCodecFor(operation)
	if !ok {
		return nil, invalid("unknown operation %q", operation)
	}
	return codec.DecodeMCPRequest(raw)
}

// yamlOperations is the set of operations that may carry inline YAML and
// therefore accept the larger request envelope.
var yamlOperations = []string{"pipeline.apply", "pipeline.update", "pipeline.start"}

// YAMLOperations returns the operations that may carry inline YAML. The slice is
// a copy; callers cannot mutate the table.
func YAMLOperations() []string {
	return append([]string(nil), yamlOperations...)
}

// MaxEnvelopeBytes returns the request envelope bound of one operation. The
// three YAML-carrying operations accept MaxYAMLRequestBytes because a 1 MiB
// document expands to about 1.4 MiB of base64; every other operation keeps the
// default MaxRequestBytes. An unknown operation gets the default bound.
func MaxEnvelopeBytes(operation string) int {
	for _, candidate := range yamlOperations {
		if candidate == operation {
			return MaxYAMLRequestBytes
		}
	}
	return MaxRequestBytes
}

// DecodeOperationRequest decodes one bounded request envelope for a named
// operation using that operation's envelope bound. It validates only the shared
// envelope; the operation's typed input is validated by the operation codec or
// the MCP decoder so a REST edge can still apply route-derived fields.
func DecodeOperationRequest(operation string, data []byte) (Request, error) {
	if _, ok := LookupRoute(operation); !ok {
		return Request{}, invalid("unknown operation %q", operation)
	}
	bound := MaxEnvelopeBytes(operation)
	if len(data) > bound {
		return Request{}, invalid("request body is %d bytes; at most %d are allowed", len(data), bound)
	}
	var request Request
	if err := decodeStrict(data, &request); err != nil {
		return Request{}, err
	}
	if err := request.Validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

// routeParamsFor returns the route-derived input fields of one operation in
// route-template order.
func routeParamsFor(operation string) []RouteParam {
	route, ok := LookupRoute(operation)
	if !ok {
		return nil
	}
	names := routeParamNames(route.Path)
	params := make([]RouteParam, 0, len(names))
	for _, name := range names {
		field := canonicalRouteField(operation, name)
		if field == "" {
			continue
		}
		params = append(params, RouteParam{Name: name, Field: field})
	}
	return params
}

// canonicalRouteField returns the canonical request field for one route
// parameter. A body that already carries the identity under another name keeps
// that one canonical name.
func canonicalRouteField(operation, name string) string {
	if fields, ok := routeParamFieldOverrides[operation]; ok {
		if field, ok := fields[name]; ok {
			return field
		}
	}
	return routeParamFields[name]
}

// routeParamFields is the default canonical field for each declared route
// parameter.
var routeParamFields = map[string]string{
	"tenant":    "tenant_id",
	"project":   "project_id",
	"principal": "principal_id",
	"binding":   "binding_id",
	"pipeline":  "pipeline_id",
	"secret":    "secret_id",
	"service":   "service_id",
	"task":      "task_id",
	"execution": "execution_id",
	"action":    "action_id",
	"model":     "model",
	"tool":      "tool",
}

// routeParamFieldOverrides records operations whose body already carries a
// route identity in a nested canonical field.
var routeParamFieldOverrides = map[string]map[string]string{
	// pipeline.apply addresses a pipeline by name, while every other pipeline
	// operation addresses the stored pipeline id.
	"pipeline.apply": {"pipeline": "name"},
}

// routeParamNames extracts the `{name}` parameters of one route template in
// order.
func routeParamNames(path string) []string {
	var names []string
	for index := 0; index < len(path); index++ {
		if path[index] != '{' {
			continue
		}
		end := index + 1
		for end < len(path) && path[end] != '}' {
			end++
		}
		if end >= len(path) {
			break
		}
		names = append(names, path[index+1:end])
		index = end
	}
	return names
}

// requireRouteParams refuses an MCP input that omits or empties a route-derived
// field.
func requireRouteParams(value any, params []RouteParam) error {
	for _, param := range params {
		field, err := fieldValueByJSONPath(reflect.ValueOf(value), param.Field)
		if err != nil || field.Kind() != reflect.String || field.String() == "" {
			return invalid("operation requires route parameter %q as input field %q", param.Name, param.Field)
		}
	}
	return nil
}

// bindRouteParams fills omitted route-derived fields from the REST route and
// refuses a body value that conflicts with the route.
func bindRouteParams(value any, params []RouteParam, routeValues map[string]string) (any, error) {
	if len(params) == 0 {
		return value, nil
	}
	pointer := reflect.New(reflect.TypeOf(value))
	pointer.Elem().Set(reflect.ValueOf(value))
	for _, param := range params {
		routeValue, ok := routeValues[param.Name]
		if !ok || routeValue == "" {
			return nil, invalid("route parameter %q is missing", param.Name)
		}
		field, err := fieldValueByJSONPath(pointer.Elem(), param.Field)
		if err != nil {
			return nil, err
		}
		if field.Kind() != reflect.String {
			return nil, invalid("route field %q is not a string", param.Field)
		}
		if current := field.String(); current != "" && current != routeValue {
			return nil, invalid("input field %q conflicts with route parameter %q", param.Field, param.Name)
		}
		field.SetString(routeValue)
	}
	return pointer.Elem().Interface(), nil
}

// fieldValueByJSONPath resolves a dotted JSON field path inside one struct
// value. It reports an error when a segment is not a declared exported field.
func fieldValueByJSONPath(root reflect.Value, path string) (reflect.Value, error) {
	current := root
	for _, segment := range splitJSONPath(path) {
		for current.Kind() == reflect.Pointer {
			if current.IsNil() {
				return reflect.Value{}, invalid("route field %q is missing", path)
			}
			current = current.Elem()
		}
		if current.Kind() != reflect.Struct {
			return reflect.Value{}, invalid("route field %q is not a struct", path)
		}
		field, ok := structFieldByJSONName(current, segment)
		if !ok {
			return reflect.Value{}, invalid("route field %q is not declared", path)
		}
		current = field
	}
	return current, nil
}

// structFieldByJSONName returns one exported struct field by its JSON name.
func structFieldByJSONName(value reflect.Value, name string) (reflect.Value, bool) {
	typ := value.Type()
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		if !field.IsExported() {
			continue
		}
		wireName, _, ok := schemaJSONName(field)
		if !ok || wireName != name {
			continue
		}
		return value.Field(index), true
	}
	return reflect.Value{}, false
}

// splitJSONPath splits a dotted JSON field path.
func splitJSONPath(path string) []string {
	var segments []string
	start := 0
	for index := 0; index <= len(path); index++ {
		if index == len(path) || path[index] == '.' {
			if index > start {
				segments = append(segments, path[start:index])
			}
			start = index + 1
		}
	}
	return segments
}

// decodeOperationRequest strictly decodes and validates one request DTO.
func decodeOperationRequest[T interface{ Validate() error }](raw json.RawMessage) (any, error) {
	value, err := decodeOperationRequestStrict[T](raw)
	if err != nil {
		return nil, err
	}
	if err := value.(T).Validate(); err != nil {
		return nil, err
	}
	return value, nil
}

// decodeOperationRequestStrict strictly decodes one request DTO without
// validating it, so the REST edge can fill route-derived fields before the
// typed validation runs.
func decodeOperationRequestStrict[T interface{ Validate() error }](raw json.RawMessage) (any, error) {
	var value T
	if err := decodeStrict(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

// decodeOperationResult strictly decodes and validates one result DTO.
func decodeOperationResult[T interface{ Validate() error }](raw json.RawMessage) (any, error) {
	var value T
	if err := decodeStrict(raw, &value); err != nil {
		return nil, err
	}
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return value, nil
}

// encodeOperationValue validates and encodes one request or result DTO.
func encodeOperationValue[T interface{ Validate() error }](input any) (json.RawMessage, error) {
	value, ok := input.(T)
	if !ok {
		pointer, pointerOK := input.(*T)
		if !pointerOK || pointer == nil {
			return nil, invalid("operation value has an unexpected type")
		}
		value = *pointer
	}
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
