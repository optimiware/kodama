package api

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// MCP JSON-RPC protocol constants.
const (
	// MCPProtocolVersion is the default MCP protocol revision string used as the
	// server's fallback during version negotiation.
	MCPProtocolVersion = "2025-06-18"
	// MCPJSONRPC is the only accepted JSON-RPC version.
	MCPJSONRPC = "2.0"
	// MCPMethodInitialize is the supported session handshake.
	MCPMethodInitialize = "initialize"
	// MCPMethodPing is the MCP liveness check.
	MCPMethodPing = "ping"
	// MCPMethodToolsList advertises the exposed operation tools.
	MCPMethodToolsList = "tools/list"
	// MCPMethodToolsCall is the supported operation method.
	MCPMethodToolsCall = "tools/call"
	// MCPNotificationInitialized is the client post-handshake notification.
	MCPNotificationInitialized = "notifications/initialized"
	// MCPNotificationCancelled is the client cancellation notification.
	MCPNotificationCancelled = "notifications/cancelled"
)

// supportedMCPProtocolVersions lists the MCP protocol revisions this server
// explicitly speaks, newest first.
var supportedMCPProtocolVersions = []string{
	"2025-11-25",
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
}

// SupportedMCPProtocolVersions returns the supported MCP protocol versions.
// The slice is a copy; callers cannot mutate the table.
func SupportedMCPProtocolVersions() []string {
	return append([]string(nil), supportedMCPProtocolVersions...)
}

// isSupportedMCPProtocolVersion reports whether version is one of the
// explicitly supported MCP protocol revisions.
func isSupportedMCPProtocolVersion(version string) bool {
	for _, candidate := range supportedMCPProtocolVersions {
		if candidate == version {
			return true
		}
	}
	return false
}

// validMCPDateVersion reports whether version is a well-formed 202x-MM-DD MCP
// date revision that can participate in MCP protocol version negotiation.
func validMCPDateVersion(version string) bool {
	if len(version) != 10 || !strings.HasPrefix(version, "202") {
		return false
	}
	if version[3] < '0' || version[3] > '9' || version[4] != '-' || version[7] != '-' {
		return false
	}
	month, err := strconv.Atoi(version[5:7])
	if err != nil || month < 1 || month > 12 {
		return false
	}
	day, err := strconv.Atoi(version[8:10])
	if err != nil || day < 1 || day > 31 {
		return false
	}
	return true
}

// NegotiateMCPProtocolVersion negotiates the MCP protocol version for a client
// initialize request: an explicitly supported revision is echoed verbatim, any
// other valid 202x-MM-DD MCP revision falls back to MCPProtocolVersion per MCP
// version negotiation rules, and empty or malformed strings are refused.
func NegotiateMCPProtocolVersion(requested string) (string, error) {
	if isSupportedMCPProtocolVersion(requested) {
		return requested, nil
	}
	if validMCPDateVersion(requested) {
		return MCPProtocolVersion, nil
	}
	return "", invalid("unsupported MCP protocol version %q", requested)
}

// maxMCPCallToolResultBytes bounds an encoded MCP CallToolResult envelope,
// which carries both the JSON text block and structuredContent for one
// MaxResponseBytes result.
const maxMCPCallToolResultBytes = 3 * MaxResponseBytes

// MCPRequest is one MCP JSON-RPC request or notification.
type MCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether request is a supported JSON-RPC notification
// (an id-free notifications/initialized or notifications/cancelled message).
func (request MCPRequest) IsNotification() bool {
	return len(request.ID) == 0 && isMCPNotificationMethod(request.Method)
}

func isMCPNotificationMethod(method string) bool {
	return method == MCPNotificationInitialized || method == MCPNotificationCancelled
}

// MCPImplementation is the client or server identity of the handshake.
type MCPImplementation struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	WebsiteURL  string          `json:"websiteUrl,omitempty"`
	Icons       json.RawMessage `json:"icons,omitempty"`
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
// version is negotiated via NegotiateMCPProtocolVersion; capabilities is an
// optional JSON object; clientInfo identifies the connecting client.
type MCPInitializeParams struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    json.RawMessage   `json:"capabilities,omitempty"`
	ClientInfo      MCPImplementation `json:"clientInfo"`
	Meta            json.RawMessage   `json:"_meta,omitempty"`
}

// Validate checks the negotiable protocol version, capabilities object and
// client identity.
func (params MCPInitializeParams) Validate() error {
	if _, err := NegotiateMCPProtocolVersion(params.ProtocolVersion); err != nil {
		return err
	}
	if !optionalJSONObject(params.Capabilities) {
		return invalid("initialize capabilities must be a JSON object")
	}
	if !optionalJSONObject(params.Meta) {
		return invalid("initialize _meta must be a JSON object")
	}
	return params.ClientInfo.Validate()
}

// MCPInitializeResult is the initialize response result. It echoes the
// negotiated supported protocol version and identifies the server.
type MCPInitializeResult struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    json.RawMessage   `json:"capabilities,omitempty"`
	ServerInfo      MCPImplementation `json:"serverInfo"`
}

// Validate checks the supported protocol version, capabilities object and
// server identity.
func (result MCPInitializeResult) Validate() error {
	if !isSupportedMCPProtocolVersion(result.ProtocolVersion) {
		return invalid("unsupported MCP protocol version %q", result.ProtocolVersion)
	}
	if !optionalJSONObject(result.Capabilities) {
		return invalid("initialize result capabilities must be a JSON object")
	}
	return result.ServerInfo.Validate()
}

// MCPPingParams is the ping parameter object.
type MCPPingParams struct {
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// MCPToolsCallParams is the tools/call parameter object. Name is either the
// underscore MCP tool name or the canonical dotted registry operation name, and
// Arguments is the operation request envelope, i.e. the JSON encoding of
// api.Request.
type MCPToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

// MCPTool is one advertised MCP tool. The catalog is the registry's
// MCP-exposed operations in registry order; InputSchema names the operation's
// input schema as a JSON Schema object.
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCPToolsListParams is the tools/list parameter object. Listing is
// scope-independent and never dispatches an operation.
type MCPToolsListParams struct {
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// MCPToolsListResult is the tools/list result object: the complete advertised
// catalog in registry declaration order.
type MCPToolsListResult struct {
	Tools []MCPTool `json:"tools"`
}

// MCPContentBlock is one content block inside an MCP CallToolResult.
type MCPContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// MCPCallToolResult is the standard MCP tools/call result object carrying both
// text content and structuredContent.
type MCPCallToolResult struct {
	Content           []MCPContentBlock `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError"`
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

// DecodeMCPRequest decodes one bounded MCP request or notification and
// validates its method and parameter subset.
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
// operations, the default 1 MiB otherwise). Both underscore MCP tool names and
// canonical dotted operation names are accepted and normalized to the canonical
// operation name.
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
	canonical, ok := MCPToolOperationName(params.Name)
	if !ok {
		return MCPToolsCallParams{}, invalid("tools/call names unknown operation %q", params.Name)
	}
	if bound := MaxEnvelopeBytes(canonical); len(data) > bound {
		return MCPToolsCallParams{}, invalid("MCP request is %d bytes; at most %d are allowed for %q", len(data), bound, canonical)
	}
	params.Name = canonical
	return params, nil
}

// Validate checks the JSON-RPC version, id, method and method-specific
// parameters. Unsupported methods, unknown operations and extra fields refuse.
func (request MCPRequest) Validate() error {
	if request.JSONRPC != MCPJSONRPC {
		return invalid("unsupported JSON-RPC version %q", request.JSONRPC)
	}
	if len(request.ID) == 0 {
		if isMCPNotificationMethod(request.Method) {
			if !optionalJSONObject(request.Params) {
				return invalid("notification params must be a JSON object")
			}
			return nil
		}
		return invalid("MCP request id is missing or not a JSON string or number")
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
	case MCPMethodPing:
		trimmed := strings.TrimSpace(string(request.Params))
		if trimmed == "" || trimmed == "null" {
			return nil
		}
		var params MCPPingParams
		if err := decodeStrict(request.Params, &params); err != nil {
			return invalid("ping params are invalid")
		}
		if !optionalJSONObject(params.Meta) {
			return invalid("ping _meta must be a JSON object")
		}
		return nil
	case MCPMethodToolsList:
		trimmed := strings.TrimSpace(string(request.Params))
		if trimmed == "" || trimmed == "null" {
			return nil
		}
		var params MCPToolsListParams
		if err := decodeStrict(request.Params, &params); err != nil {
			return invalid("tools/list params are invalid")
		}
		if !optionalJSONObject(params.Meta) {
			return invalid("tools/list _meta must be a JSON object")
		}
		return nil
	case MCPMethodToolsCall:
		var params MCPToolsCallParams
		if err := decodeStrict(request.Params, &params); err != nil {
			return invalid("tools/call params are invalid")
		}
		if !optionalJSONObject(params.Meta) {
			return invalid("tools/call _meta must be a JSON object")
		}
		if _, ok := MCPToolOperationName(params.Name); !ok {
			return invalid("tools/call names unknown operation %q", params.Name)
		}
		trimmedArgs := strings.TrimSpace(string(params.Arguments))
		if trimmedArgs == "" || trimmedArgs == "null" {
			return nil
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

// EncodeMCPResult encodes one successful tools/call result as a standard MCP
// CallToolResult carrying both text content and structuredContent.
func EncodeMCPResult(result Result) (json.RawMessage, error) {
	if err := result.Validate(); err != nil {
		return nil, err
	}
	rawResult, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(MCPCallToolResult{
		Content: []MCPContentBlock{{
			Type: "text",
			Text: string(rawResult),
		}},
		StructuredContent: rawResult,
		IsError:           false,
	})
}

// DecodeMCPResult decodes one bounded tools/call result from either the
// standard MCP CallToolResult shape (structuredContent or content[0].text, with
// isError checked) or a legacy direct api.Result JSON object.
func DecodeMCPResult(data []byte) (Result, error) {
	if len(data) > maxMCPCallToolResultBytes {
		return Result{}, invalid("MCP result is %d bytes; at most %d are allowed", len(data), maxMCPCallToolResultBytes)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return Result{}, invalid("MCP result is empty")
	}
	if trimmed[0] != '{' {
		return Result{}, invalid("MCP result must be a JSON object")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return Result{}, invalid("invalid MCP result JSON: %v", err)
	}
	if _, direct := probe["operation"]; direct {
		return decodeBoundedDirectResult(data)
	}

	var callResult MCPCallToolResult
	if err := decodeStrict(trimmed, &callResult); err != nil {
		return Result{}, err
	}
	if callResult.IsError {
		return Result{}, invalid("MCP tool call returned isError=true")
	}
	if len(callResult.Content) == 0 {
		return Result{}, invalid("MCP CallToolResult content is empty")
	}
	for _, block := range callResult.Content {
		if block.Type != "text" {
			return Result{}, invalid("unsupported MCP content block type %q", block.Type)
		}
	}
	structured := bytes.TrimSpace(callResult.StructuredContent)
	if len(structured) > 0 && !bytes.Equal(structured, []byte("null")) {
		return decodeBoundedDirectResult(callResult.StructuredContent)
	}
	return decodeBoundedDirectResult([]byte(callResult.Content[0].Text))
}

func decodeBoundedDirectResult(data []byte) (Result, error) {
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
