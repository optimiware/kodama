package opsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// authorizeOperation is the registry operation-name type.
type authorizeOperation = authorize.OperationName

// handlerFunc adapts one function to the router's Handler interface.
type handlerFunc func(context.Context, ops.AuthorizedCall) (api.Result, error)

// Handle runs the bound operation.
func (handle handlerFunc) Handle(ctx context.Context, call ops.AuthorizedCall) (api.Result, error) {
	return handle(ctx, call)
}

// codecs is the registry-keyed wire vocabulary. It is derived once from
// api.OperationCodecs so the bindings cannot drift from the public contract.
var codecs = func() map[authorize.OperationName]api.OperationCodec {
	table := make(map[authorize.OperationName]api.OperationCodec, len(api.OperationCodecs()))
	for _, codec := range api.OperationCodecs() {
		table[authorize.OperationName(codec.Operation)] = codec
	}
	return table
}()

// wireCodec adapts one public operation codec to the router's Codec interface.
// The input side decodes the strict request DTO; the result side decodes the
// strict result DTO. Both only refuse values and perform no effect.
type wireCodec struct {
	validate func(json.RawMessage) error
}

// Validate runs the wrapped public codec.
func (codec wireCodec) Validate(raw json.RawMessage) error {
	return codec.validate(raw)
}

// inputCodec returns the router input codec of one operation.
//
// The router validates the raw body before the handler and does not pass the
// REST route parameters to the codec. A REST caller may omit every route-derived
// field, so the codec fills each one from the body when present and with a valid
// placeholder otherwise; the handler then applies the real route values and
// refuses a missing or conflicting one. Every non-route field is still validated
// by the DTO exactly once.
//
// pipeline.start is the one operation with two registered REST bindings and an
// exclusive stored/inline selection: an inline document selects the task-free
// alternate binding, whose route derives only the project. Synthesizing a
// canonical pipeline id would turn one inline selection into two. Every other
// call keeps the canonical binding and behavior unchanged.
func inputCodec(codec api.OperationCodec) ops.Codec {
	return wireCodec{validate: func(raw json.RawMessage) error {
		body := bodyOf(raw)
		route, ok := preliminaryRoute(codec, body)
		if !ok {
			return fault.New(fault.InternalFailure, "operation has no registered route binding")
		}
		params := make(map[string]string, 2)
		for _, name := range routeParamNamesOf(route.Path) {
			value, ok := jsonField(body, routeFieldOf(codec, name))
			if !ok || value == "" {
				value = "placeholder"
			}
			params[name] = value
		}
		_, err := api.DecodeRouteRequest(route, body, params)
		return err
	}}
}

// preliminaryRoute returns the binding the router must use to validate a raw
// body it cannot bind to concrete path parameters. An inline pipeline.start
// document is validated against the alternate task-free binding so no stored
// pipeline id is synthesized; every other body uses the canonical binding.
func preliminaryRoute(codec api.OperationCodec, body json.RawMessage) (api.Route, bool) {
	canonical, ok := api.LookupRoute(codec.Operation)
	if !ok {
		return api.Route{}, false
	}
	if codec.Operation != "pipeline.start" {
		return canonical, true
	}
	if value, present := jsonField(body, "yaml"); !present || value == "" {
		return canonical, true
	}
	for _, alternate := range api.AlternateRoutes() {
		if alternate.Operation == codec.Operation {
			return alternate, true
		}
	}
	return api.Route{}, false
}

// routeFieldOf returns the canonical request field of one route parameter.
func routeFieldOf(codec api.OperationCodec, name string) string {
	for _, param := range codec.RouteParams {
		if param.Name == name {
			return param.Field
		}
	}
	return ""
}

// routeParamNamesOf extracts the `{name}` parameters of one route template in
// order.
func routeParamNamesOf(path string) []string {
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

// routeForTargets returns the registered canonical or alternate binding whose
// route parameters are exactly the resolved REST targets. A REST call always
// carries its concrete path parameters, so the route-aware decoder can
// fill exactly that binding's fields instead of the canonical table's.
func routeForTargets(operation string, targets map[string]string) (api.Route, bool) {
	candidates := make([]api.Route, 0, 2)
	if route, ok := api.LookupRoute(operation); ok {
		candidates = append(candidates, route)
	}
	for _, route := range api.AlternateRoutes() {
		if route.Operation == operation {
			candidates = append(candidates, route)
		}
	}
	var matched []api.Route
	for _, candidate := range candidates {
		if sameRouteTargets(routeParamNamesOf(candidate.Path), targets) {
			matched = append(matched, candidate)
		}
	}
	if len(matched) != 1 {
		return api.Route{}, false
	}
	return matched[0], true
}

// sameRouteTargets reports whether one binding's route parameter names are
// exactly the resolved target keys.
func sameRouteTargets(names []string, targets map[string]string) bool {
	if len(names) != len(targets) {
		return false
	}
	for _, name := range names {
		if _, ok := targets[name]; !ok {
			return false
		}
	}
	return true
}

// resultCodec returns the router result codec of one operation.
func resultCodec(codec api.OperationCodec) ops.Codec {
	return wireCodec{validate: func(raw json.RawMessage) error {
		_, err := codec.DecodeResult(bodyOf(raw))
		return err
	}}
}

// bodyOf substitutes the empty JSON object for an empty body. The router
// accepts an omitted input for an operation whose DTO has no required member,
// while the strict decoder requires an object.
func bodyOf(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

// decodeRequest decodes one typed request DTO from the authorized call.
//
// A REST call carries the route parameters the router already resolved as
// targets; the route-aware REST decoder fills exactly that registered binding's
// fields (canonical or alternate) and refuses a body value that disagrees. An
// MCP call carries the fields in the input, so the MCP decoder requires them.
// The router validated the same raw input with the same public codec before the
// handler ran, so a decode failure here is a defensive internal failure rather
// than a new refusal path.
func decodeRequest[T any](call ops.AuthorizedCall) (T, error) {
	var zero T
	codec, ok := codecs[call.Operation.Name]
	if !ok {
		return zero, fault.New(fault.InternalFailure, "operation has no public codec")
	}
	raw := bodyOf(call.Request.Input)
	var (
		value any
		err   error
	)
	if len(call.Targets) > 0 {
		route, ok := routeForTargets(codec.Operation, call.Targets)
		if !ok {
			return zero, fault.New(fault.InternalFailure, "operation has no registered route binding for the supplied targets")
		}
		value, err = api.DecodeRouteRequest(route, raw, call.Targets)
	} else {
		value, err = api.DecodeMCPOperationRequest(codec.Operation, raw)
	}
	if err != nil {
		return zero, wireRefusal(err)
	}
	typed, ok := value.(T)
	if !ok {
		return zero, fault.New(fault.InternalFailure, "operation request decoded into an unexpected type")
	}
	return typed, nil
}

// wireRefusal keeps the public code vocabulary as a typed fault. The router
// sanitizes a non-fault handler error into internal_failure, so a decode
// refusal must be mapped here to its fault class.
func wireRefusal(err error) error {
	var publicError *api.Error
	if errors.As(err, &publicError) && publicError != nil {
		if class, ok := fault.ClassFor(fault.Code(publicError.Code)); ok {
			message := publicError.Message
			if message == "" {
				message = string(class)
			}
			return fault.New(class, message)
		}
	}
	return err
}

// respond encodes one typed result DTO as the transport result.
func respond[T any](call ops.AuthorizedCall, value T) (api.Result, error) {
	codec, ok := codecs[call.Operation.Name]
	if !ok {
		return api.Result{}, fault.New(fault.InternalFailure, "operation has no public codec")
	}
	body, err := codec.EncodeResult(value)
	if err != nil {
		return api.Result{}, err
	}
	return api.Result{Operation: string(call.Operation.Name), Body: body}, nil
}

// receiptContent builds the immutable idempotency content of one request from
// the decoded canonical request and the expected version. The canonical request
// is the typed DTO after the REST route fill, so equal logical requests hash
// equal across the REST and MCP edges, while a different route target or body
// value hashes differently. Fingerprinting only the raw input would let one key
// replay the first result for a different target.
func receiptContent[T any](call ops.AuthorizedCall, request T) receipt.Content {
	payload := struct {
		Request         T       `json:"request"`
		ExpectedVersion *uint64 `json:"expected_version,omitempty"`
	}{Request: request, ExpectedVersion: call.Request.ExpectedVersion}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// Every receipt-bearing request DTO is composed of JSON-serializable
		// fields, so this is unreachable; a defensive marker keeps the
		// fingerprint well-formed and distinct from a canonical request.
		encoded = []byte(`{"request":null}`)
	}
	return receipt.Content{MediaType: api.MediaType, Bytes: encoded}
}

// expectedVersion returns the envelope's expected version, or zero when the
// caller supplied none. Capabilities treat zero as "no prior version" where the
// operation is an insert.
func expectedVersion(call ops.AuthorizedCall) uint64 {
	if call.Request.ExpectedVersion == nil {
		return 0
	}
	return *call.Request.ExpectedVersion
}

// requiredVersion returns the envelope's expected version and refuses a
// missing one for a versioned mutation.
func requiredVersion(call ops.AuthorizedCall) (uint64, error) {
	version := expectedVersion(call)
	if version == 0 {
		return 0, fault.New(fault.InvalidInput, "operation requires expected_version")
	}
	return version, nil
}

// inputsOf converts the public string-keyed inputs into the domain key type.
func inputsOf(inputs map[string]string) map[id.InputKey]string {
	if inputs == nil {
		return nil
	}
	converted := make(map[id.InputKey]string, len(inputs))
	for key, value := range inputs {
		converted[id.InputKey(key)] = value
	}
	return converted
}

// outputsOf converts the public string-keyed outputs into the Kernel key type.
func outputsOf(outputs map[string]string) map[id.OutputKey]string {
	if outputs == nil {
		return nil
	}
	converted := make(map[id.OutputKey]string, len(outputs))
	for key, value := range outputs {
		converted[id.OutputKey(key)] = value
	}
	return converted
}

// stringsOf converts the public string-keyed outputs back to the wire type.
func stringsOf(outputs map[id.OutputKey]string) map[string]string {
	if outputs == nil {
		return nil
	}
	converted := make(map[string]string, len(outputs))
	for key, value := range outputs {
		converted[string(key)] = value
	}
	return converted
}

// metadataBytes encodes bounded task metadata for the task service, which
// accepts the raw JSON object. An omitted metadata map stays nil.
func metadataBytes(metadata map[string]string) ([]byte, error) {
	if metadata == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, fault.New(fault.InvalidInput, "metadata is not encodable")
	}
	return encoded, nil
}
