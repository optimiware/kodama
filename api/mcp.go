package api

import (
	"bytes"
	"encoding/json"
	"reflect"
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
	// MCPMethodResourcesList advertises the embedded documentation resources.
	MCPMethodResourcesList = "resources/list"
	// MCPMethodResourcesTemplatesList advertises parameterized resource templates.
	MCPMethodResourcesTemplatesList = "resources/templates/list"
	// MCPMethodResourcesRead reads one embedded documentation resource by URI.
	MCPMethodResourcesRead = "resources/read"
	// MCPMethodPromptsList advertises the embedded documentation prompts.
	MCPMethodPromptsList = "prompts/list"
	// MCPMethodPromptsGet renders one embedded documentation prompt.
	MCPMethodPromptsGet = "prompts/get"
	// MCPNotificationInitialized is the client post-handshake notification.
	MCPNotificationInitialized = "notifications/initialized"
	// MCPNotificationCancelled is the client cancellation notification.
	MCPNotificationCancelled = "notifications/cancelled"
)

// MCPCodeResourceNotFound is the MCP-specified JSON-RPC error code for an
// unknown resource URI returned by resources/read.
const MCPCodeResourceNotFound = -32002

const maxMCPResourceURILength = 512

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
	if version[4] != '-' || version[7] != '-' {
		return false
	}
	for _, index := range []int{3, 5, 6, 8, 9} {
		if version[index] < '0' || version[index] > '9' {
			return false
		}
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
// negotiated supported protocol version, identifies the server and may include
// concise server instructions for connecting harnesses.
type MCPInitializeResult struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    json.RawMessage   `json:"capabilities,omitempty"`
	ServerInfo      MCPImplementation `json:"serverInfo"`
	Instructions    string            `json:"instructions,omitempty"`
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

// MCPListParams is the shared parameter object for paginated MCP list methods
// (resources/list, resources/templates/list, and prompts/list).
type MCPListParams struct {
	Cursor string          `json:"cursor,omitempty"`
	Meta   json.RawMessage `json:"_meta,omitempty"`
}

// MCPResource is one advertised MCP resource (embedded Kodema documentation).
type MCPResource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

// MCPResourcesListResult is the resources/list result object.
type MCPResourcesListResult struct {
	Resources []MCPResource `json:"resources"`
}

// MCPResourceTemplate is one parameterized MCP resource template.
type MCPResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

// MCPResourcesTemplatesListResult is the resources/templates/list result object.
type MCPResourcesTemplatesListResult struct {
	ResourceTemplates []MCPResourceTemplate `json:"resourceTemplates"`
}

// MCPResourcesReadParams is the resources/read parameter object.
type MCPResourcesReadParams struct {
	URI  string          `json:"uri"`
	Meta json.RawMessage `json:"_meta,omitempty"`
}

// MCPResourceContents is one text content entry returned by resources/read.
type MCPResourceContents struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text"`
}

// MCPResourcesReadResult is the resources/read result object.
type MCPResourcesReadResult struct {
	Contents []MCPResourceContents `json:"contents"`
}

// MCPPromptArgument describes one named prompt argument.
type MCPPromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// MCPPrompt is one advertised MCP prompt.
type MCPPrompt struct {
	Name        string              `json:"name"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Arguments   []MCPPromptArgument `json:"arguments,omitempty"`
}

// MCPPromptsListResult is the prompts/list result object.
type MCPPromptsListResult struct {
	Prompts []MCPPrompt `json:"prompts"`
}

// MCPPromptsGetParams is the prompts/get parameter object.
type MCPPromptsGetParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments,omitempty"`
	Meta      json.RawMessage   `json:"_meta,omitempty"`
}

// MCPPromptMessage is one role/content message returned by prompts/get.
type MCPPromptMessage struct {
	Role    string          `json:"role"`
	Content MCPContentBlock `json:"content"`
}

// MCPPromptsGetResult is the prompts/get result object.
type MCPPromptsGetResult struct {
	Description string             `json:"description,omitempty"`
	Messages    []MCPPromptMessage `json:"messages"`
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
		canonical, ok := MCPToolOperationName(params.Name)
		if !ok {
			return invalid("tools/call names unknown operation %q", params.Name)
		}
		if _, err := DecodeMCPOperationArguments(canonical, params.Arguments); err != nil {
			return err
		}
		return nil
	case MCPMethodResourcesList, MCPMethodResourcesTemplatesList, MCPMethodPromptsList:
		trimmed := strings.TrimSpace(string(request.Params))
		if trimmed == "" || trimmed == "null" {
			return nil
		}
		var params MCPListParams
		if err := decodeStrict(request.Params, &params); err != nil {
			return invalid("%s params are invalid", request.Method)
		}
		if !optionalJSONObject(params.Meta) {
			return invalid("%s _meta must be a JSON object", request.Method)
		}
		return nil
	case MCPMethodResourcesRead:
		var params MCPResourcesReadParams
		if err := decodeStrict(request.Params, &params); err != nil {
			return invalid("resources/read params are invalid")
		}
		if !optionalJSONObject(params.Meta) {
			return invalid("resources/read _meta must be a JSON object")
		}
		if strings.TrimSpace(params.URI) == "" {
			return invalid("resources/read uri is empty")
		}
		if len(params.URI) > maxMCPResourceURILength {
			return invalid("resources/read uri is too long")
		}
		return nil
	case MCPMethodPromptsGet:
		var params MCPPromptsGetParams
		if err := decodeStrict(request.Params, &params); err != nil {
			return invalid("prompts/get params are invalid")
		}
		if !optionalJSONObject(params.Meta) {
			return invalid("prompts/get _meta must be a JSON object")
		}
		if strings.TrimSpace(params.Name) == "" {
			return invalid("prompts/get name is empty")
		}
		if len(params.Name) > maxIDLength {
			return invalid("prompts/get name %q is too long", params.Name)
		}
		return nil
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

// DecodeMCPOperationArguments decodes one bounded MCP tools/call arguments
// object for a named operation. It accepts both the canonical api.Request
// envelope ({"input": {...}, "expected_version": ..., "idempotency_key": ...})
// and flat top-level operation fields (optionally accompanied by
// expected_version and idempotency_key). Unknown fields and mixed wrapped+flat
// objects are refused with invalid_input.
func DecodeMCPOperationArguments(operation string, data []byte) (Request, error) {
	if canonical, ok := MCPToolOperationName(operation); ok {
		operation = canonical
	}
	if _, ok := LookupRoute(operation); !ok {
		return Request{}, invalid("unknown operation %q", operation)
	}
	bound := MaxEnvelopeBytes(operation)
	if len(data) > bound {
		return Request{}, invalid("request body is %d bytes; at most %d are allowed", len(data), bound)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return Request{}, nil
	}
	if trimmed[0] != '{' {
		return Request{}, invalid("tools/call arguments must be a JSON object")
	}
	var fields map[string]json.RawMessage
	if err := decodeStrict(trimmed, &fields); err != nil {
		return Request{}, err
	}
	if _, wrapped := fields["input"]; wrapped {
		return DecodeOperationRequest(operation, trimmed)
	}

	var expectedVersion *uint64
	if rawVersion, ok := fields["expected_version"]; ok {
		if err := json.Unmarshal(rawVersion, &expectedVersion); err != nil {
			return Request{}, invalid("expected_version is invalid")
		}
		delete(fields, "expected_version")
	}
	var idempotencyKey string
	if rawKey, ok := fields["idempotency_key"]; ok {
		if err := json.Unmarshal(rawKey, &idempotencyKey); err != nil {
			return Request{}, invalid("idempotency_key is invalid")
		}
		delete(fields, "idempotency_key")
	}

	var rawInput json.RawMessage
	if len(fields) > 0 {
		allowed := operationInputFieldNames(operation)
		for name := range fields {
			if !allowed[name] {
				return Request{}, invalid("tools/call arguments contain unknown field %q", name)
			}
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			return Request{}, invalid("tools/call arguments are invalid")
		}
		rawInput = encoded
	}
	request := Request{
		Input:           rawInput,
		ExpectedVersion: expectedVersion,
		IdempotencyKey:  idempotencyKey,
	}
	if err := request.Validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

// operationInputFieldNames returns the declared top-level JSON wire field names
// of one registered operation's request DTO.
func operationInputFieldNames(operation string) map[string]bool {
	spec, ok := operationCodecSpecs[operation]
	if !ok {
		return nil
	}
	typ := indirectSchemaType(spec.inputType)
	if typ == nil || typ.Kind() != reflect.Struct {
		return nil
	}
	names := make(map[string]bool, typ.NumField())
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		if !field.IsExported() {
			continue
		}
		if name, _, ok := schemaJSONName(field); ok {
			names[name] = true
		}
	}
	return names
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

// EncodeMCPToolError encodes one public domain or authorization refusal from
// tools/call as a standard MCP CallToolResult with isError=true, carrying the
// stable api.Error JSON in both text content and structuredContent.
func EncodeMCPToolError(publicError Error) (json.RawMessage, error) {
	if err := publicError.Validate(); err != nil {
		return nil, err
	}
	rawError, err := json.Marshal(publicError)
	if err != nil {
		return nil, err
	}
	return json.Marshal(MCPCallToolResult{
		Content: []MCPContentBlock{{
			Type: "text",
			Text: string(rawError),
		}},
		StructuredContent: rawError,
		IsError:           true,
	})
}

// DecodeMCPToolError decodes one MCP CallToolResult carrying isError=true into
// its stable api.Error. When data is a non-error CallToolResult or a direct
// api.Result object, it returns (Error{}, false, nil).
func DecodeMCPToolError(data []byte) (Error, bool, error) {
	if len(data) > maxMCPCallToolResultBytes {
		return Error{}, false, invalid("MCP result is %d bytes; at most %d are allowed", len(data), maxMCPCallToolResultBytes)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Error{}, false, invalid("MCP result must be a JSON object")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return Error{}, false, invalid("invalid MCP result JSON: %v", err)
	}
	if _, direct := probe["operation"]; direct {
		return Error{}, false, nil
	}
	var callResult MCPCallToolResult
	if err := decodeStrict(trimmed, &callResult); err != nil {
		return Error{}, false, err
	}
	if !callResult.IsError {
		return Error{}, false, nil
	}
	if len(callResult.Content) == 0 {
		return Error{}, true, invalid("MCP CallToolResult content is empty")
	}
	for _, block := range callResult.Content {
		if block.Type != "text" {
			return Error{}, true, invalid("unsupported MCP content block type %q", block.Type)
		}
	}
	structured := bytes.TrimSpace(callResult.StructuredContent)
	if len(structured) > 0 && !bytes.Equal(structured, []byte("null")) {
		publicError, err := DecodeMCPErrorData(callResult.StructuredContent)
		if err != nil {
			return Error{}, true, err
		}
		return publicError, true, nil
	}
	publicError, err := DecodeMCPErrorData([]byte(callResult.Content[0].Text))
	if err != nil {
		return Error{}, true, err
	}
	return publicError, true, nil
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

// IsMCPToolErrorCode reports whether code represents a domain, authorization,
// or operational refusal from tools/call that should be returned as an MCP
// CallToolResult with isError=true rather than a JSON-RPC protocol error.
// Unauthenticated (HTTP 401 / OAuth transport challenge) and internal_failure
// (server/protocol error) remain JSON-RPC errors.
func IsMCPToolErrorCode(code string) bool {
	switch code {
	case CodeInvalidInput,
		CodeForbidden,
		CodeNotFound,
		CodeVersionConflict,
		CodeChangedIdempotencyContent,
		CodeDeadlineExpired,
		CodeUnsupportedCapability,
		CodeDependencyUnavailable,
		CodeUncertainExternalEffect:
		return true
	default:
		return false
	}
}
