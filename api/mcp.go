package api

import (
	"encoding/json"
	"strings"
)

// MCP JSON-RPC subset constants.
//
// The subset contains the initialize handshake, tools/list and tools/call.
// notifications/initialized and every other method are outside the subset and
// refuse as unsupported methods.
const (
	// MCPProtocolVersion is the pinned MCP protocol revision string.
	MCPProtocolVersion = "2025-06-18"
	// MCPJSONRPC is the only accepted JSON-RPC version.
	MCPJSONRPC = "2.0"
	// MCPMethodInitialize is the supported session handshake.
	MCPMethodInitialize = "initialize"
	// MCPMethodToolsList advertises the exposed operation tools.
	MCPMethodToolsList = "tools/list"
	// MCPMethodToolsCall is the supported operation method.
	MCPMethodToolsCall = "tools/call"
)

// MCPRequest is one MCP JSON-RPC request.
type MCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// MCPImplementation is the client or server identity of the handshake.
type MCPImplementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Validate checks the bounded non-empty implementation name and version.
func (implementation MCPImplementation) Validate() error {
	if implementation.Name == "" {
		return invalid("MCP implementation name is empty")
	}
	if len(implementation.Name) > maxIDLength {
		return invalid("MCP implementation name %q is too long", implementation.Name)
	}
	if implementation.Version == "" {
		return invalid("MCP implementation version is empty")
	}
	if len(implementation.Version) > maxIDLength {
		return invalid("MCP implementation version %q is too long", implementation.Version)
	}
	return nil
}

// MCPInitializeParams is the initialize request parameter object. The protocol
// version must equal MCPProtocolVersion; capabilities is an optional JSON
// object; clientInfo identifies the connecting client.
type MCPInitializeParams struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    json.RawMessage   `json:"capabilities,omitempty"`
	ClientInfo      MCPImplementation `json:"clientInfo"`
}

// Validate checks the supported protocol version, capabilities object and
// client identity.
func (params MCPInitializeParams) Validate() error {
	if params.ProtocolVersion != MCPProtocolVersion {
		return invalid("unsupported MCP protocol version %q; want %q", params.ProtocolVersion, MCPProtocolVersion)
	}
	if !optionalJSONObject(params.Capabilities) {
		return invalid("initialize capabilities must be a JSON object")
	}
	return params.ClientInfo.Validate()
}

// MCPInitializeResult is the initialize response result. It echoes the pinned
// protocol version and identifies the server.
type MCPInitializeResult struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    json.RawMessage   `json:"capabilities,omitempty"`
	ServerInfo      MCPImplementation `json:"serverInfo"`
}

// Validate checks the pinned protocol version, capabilities object and server
// identity.
func (result MCPInitializeResult) Validate() error {
	if result.ProtocolVersion != MCPProtocolVersion {
		return invalid("unsupported MCP protocol version %q; want %q", result.ProtocolVersion, MCPProtocolVersion)
	}
	if !optionalJSONObject(result.Capabilities) {
		return invalid("initialize result capabilities must be a JSON object")
	}
	return result.ServerInfo.Validate()
}

// MCPToolsCallParams is the tools/call parameter object. Name is the registry
// operation name and Arguments is the operation request envelope, i.e. the JSON
// encoding of api.Request.
type MCPToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// MCPTool is one advertised MCP tool. The catalog is exactly the registry's
// MCP-exposed operations in registry order; InputSchema names the operation's
// input schema as a JSON Schema object.
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCPToolsListParams is the tools/list parameter object. It is empty: listing is
// scope-independent and never dispatches an operation.
type MCPToolsListParams struct{}

// MCPToolsListResult is the tools/list result object: the complete advertised
// catalog in registry declaration order.
type MCPToolsListResult struct {
	Tools []MCPTool `json:"tools"`
}

// MCPError is one MCP JSON-RPC error object.
type MCPError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// MCPResponse is one MCP JSON-RPC response.
type MCPResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *MCPError       `json:"error,omitempty"`
}

// DecodeMCPRequest decodes one bounded MCP request and validates its method and
// parameter subset.
func DecodeMCPRequest(data []byte) (MCPRequest, error) {
	if len(data) > MaxRequestBytes {
		return MCPRequest{}, invalid("MCP request is %d bytes; at most %d are allowed", len(data), MaxRequestBytes)
	}
	var request MCPRequest
	if err := decodeStrict(data, &request); err != nil {
		return MCPRequest{}, err
	}
	if err := request.Validate(); err != nil {
		return MCPRequest{}, err
	}
	return request, nil
}

// DecodeMCPToolsCall decodes one MCP tools/call body under the YAML-aware
// envelope rule: a hard 2 MiB cap applies before decoding, then the named
// operation's MaxEnvelopeBytes bound decides (2 MiB for the three YAML-carrying
// operations, the default 1 MiB otherwise). DecodeMCPRequest keeps the 1 MiB
// default for every other MCP method.
func DecodeMCPToolsCall(data []byte) (MCPToolsCallParams, error) {
	if len(data) > MaxYAMLRequestBytes {
		return MCPToolsCallParams{}, invalid("MCP request is %d bytes; at most %d are allowed", len(data), MaxYAMLRequestBytes)
	}
	var request MCPRequest
	if err := decodeStrict(data, &request); err != nil {
		return MCPToolsCallParams{}, err
	}
	if err := request.Validate(); err != nil {
		return MCPToolsCallParams{}, err
	}
	if request.Method != MCPMethodToolsCall {
		return MCPToolsCallParams{}, invalid("MCP method %q is not %q", request.Method, MCPMethodToolsCall)
	}
	var params MCPToolsCallParams
	if err := decodeStrict(request.Params, &params); err != nil {
		return MCPToolsCallParams{}, invalid("tools/call params are invalid")
	}
	if bound := MaxEnvelopeBytes(params.Name); len(data) > bound {
		return MCPToolsCallParams{}, invalid("MCP request is %d bytes; at most %d are allowed for %q", len(data), bound, params.Name)
	}
	return params, nil
}

// Validate checks the JSON-RPC version, id, method and method-specific
// parameters. Unsupported methods, unknown operations and extra fields refuse.
func (request MCPRequest) Validate() error {
	if request.JSONRPC != MCPJSONRPC {
		return invalid("unsupported JSON-RPC version %q", request.JSONRPC)
	}
	if !validJSONRPCID(request.ID) {
		return invalid("MCP request id is missing or not a JSON string or number")
	}
	switch request.Method {
	case MCPMethodInitialize:
		var params MCPInitializeParams
		if err := decodeStrict(request.Params, &params); err != nil {
			return invalid("initialize params are invalid")
		}
		return params.Validate()
	case MCPMethodToolsList:
		trimmed := strings.TrimSpace(string(request.Params))
		if trimmed == "" || trimmed == "null" {
			return nil
		}
		var params MCPToolsListParams
		if err := decodeStrict(request.Params, &params); err != nil {
			return invalid("tools/list params are invalid")
		}
		return nil
	case MCPMethodToolsCall:
		var params MCPToolsCallParams
		if err := decodeStrict(request.Params, &params); err != nil {
			return invalid("tools/call params are invalid")
		}
		if _, ok := LookupRoute(params.Name); !ok {
			return invalid("tools/call names unknown operation %q", params.Name)
		}
		var arguments Request
		if err := decodeStrict(params.Arguments, &arguments); err != nil {
			return invalid("tools/call arguments are invalid")
		}
		return arguments.Validate()
	default:
		return invalid("unsupported MCP method %q", request.Method)
	}
}

// Validate checks the JSON-RPC version, id and exactly one result or error.
func (response MCPResponse) Validate() error {
	if response.JSONRPC != MCPJSONRPC {
		return invalid("unsupported JSON-RPC version %q", response.JSONRPC)
	}
	if !validJSONRPCID(response.ID) {
		return invalid("MCP response id is missing or not a JSON string or number")
	}
	hasResult := len(response.Result) > 0
	hasError := response.Error != nil
	if hasResult == hasError {
		return invalid("MCP response must carry exactly one result or error")
	}
	if hasError {
		if response.Error.Code >= 0 {
			return invalid("MCP error code %d must be negative", response.Error.Code)
		}
		if response.Error.Message == "" {
			return invalid("MCP error message is empty")
		}
	}
	return nil
}

// EncodeMCPResult encodes one successful tools/call result. The MCP response
// result is exactly the JSON encoding of api.Result.
func EncodeMCPResult(result Result) (json.RawMessage, error) {
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

// DecodeMCPResult decodes one bounded tools/call result encoded as api.Result.
func DecodeMCPResult(data []byte) (Result, error) {
	if len(data) > MaxResponseBytes {
		return Result{}, invalid("MCP result is %d bytes; at most %d are allowed", len(data), MaxResponseBytes)
	}
	var result Result
	if err := decodeStrict(data, &result); err != nil {
		return Result{}, err
	}
	if err := result.Validate(); err != nil {
		return Result{}, err
	}
	return result, nil
}

// EncodeMCPErrorData encodes the public error carried in an MCP error's data
// field. The data is exactly the JSON encoding of api.Error, so the stable
// public code is always available to the caller.
func EncodeMCPErrorData(publicError Error) (json.RawMessage, error) {
	if err := publicError.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(publicError)
}

// DecodeMCPErrorData decodes the public error carried in an MCP error's data
// field.
func DecodeMCPErrorData(data []byte) (Error, error) {
	if len(data) > MaxResponseBytes {
		return Error{}, invalid("MCP error data is %d bytes; at most %d are allowed", len(data), MaxResponseBytes)
	}
	var publicError Error
	if err := decodeStrict(data, &publicError); err != nil {
		return Error{}, err
	}
	if err := publicError.Validate(); err != nil {
		return Error{}, err
	}
	return publicError, nil
}

// validJSONRPCID reports whether id is a JSON string or number.
func validJSONRPCID(id json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(id))
	if trimmed == "" || trimmed == "null" {
		return false
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return false
	}
	switch value.(type) {
	case string, float64, json.Number:
		return true
	default:
		return false
	}
}

// mcpErrorCodes is the deterministic, distinct MCP mapping of the public codes.
var mcpErrorCodes = []struct {
	code   string
	number int
}{
	{CodeInvalidInput, -32602},
	{CodeUnauthenticated, -32001},
	{CodeForbidden, -32003},
	{CodeNotFound, -32004},
	{CodeVersionConflict, -32009},
	{CodeChangedIdempotencyContent, -32010},
	{CodeDeadlineExpired, -32008},
	{CodeUnsupportedCapability, -32012},
	{CodeDependencyUnavailable, -32013},
	{CodeUncertainExternalEffect, -32014},
	{CodeInternalFailure, -32603},
}

// MCPErrorCode returns the MCP JSON-RPC error number for a public code.
func MCPErrorCode(code string) (int, bool) {
	for _, entry := range mcpErrorCodes {
		if entry.code == code {
			return entry.number, true
		}
	}
	return 0, false
}
