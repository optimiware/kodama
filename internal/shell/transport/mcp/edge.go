// Package mcp is the thin MCP JSON-RPC 2.0 edge over the single operation
// router.
//
// It supports the initialize handshake, ping, the tools/list catalog and
// tools/call. Every other method refuses with the stable invalid_input mapping.
// When Principals is configured, initialize and tools/list authenticate the
// presented credential before returning server metadata or tool schemas, while
// never dispatching an operation; tools/call names one registry operation and
// passes either the canonical api.Request envelope or flat top-level arguments
// to the router with the presented credential.
//
// A success result is an MCP CallToolResult carrying the JSON api.Result.
// Domain, authorization and operational refusals from tools/call return an MCP
// CallToolResult with isError=true carrying the stable api.Error in text
// content and structuredContent. Protocol errors, unauthenticated refusals and
// internal failures return JSON-RPC errors with the declared api.MCPErrorCode
// mapping plus the api.Error body in data. A private fault cause and the
// presented credential are never rendered.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/shell/authorize"
	"github.com/optimiware/kodema/internal/shell/transport/ops"
)

// Router is the single operation boundary both public edges call.
type Router interface {
	Invoke(context.Context, ops.Invocation) (api.Result, error)
}

// Config is the MCP edge construction input. Both the router and the server
// identity are required. Principals is optional for isolated unit tests and
// wired by the server composition so initialize and tools/list require an
// authenticated credential.
type Config struct {
	Router     Router
	Principals authorize.CurrentPrincipal
	ServerInfo api.MCPImplementation
}

// Edge is the MCP transport. It holds no per-request state and is safe for
// concurrent use.
type Edge struct {
	router     Router
	principals authorize.CurrentPrincipal
	serverInfo api.MCPImplementation
}

// NewEdge builds the MCP edge over the shared router. A nil router, an invalid
// server identity and an unusable tool catalog are refused.
func NewEdge(config Config) (*Edge, error) {
	if config.Router == nil {
		return nil, fault.New(fault.InvalidInput, "MCP edge router is nil")
	}
	if err := config.ServerInfo.Validate(); err != nil {
		return nil, err
	}
	if toolCatalogErr != nil {
		return nil, toolCatalogErr
	}
	return &Edge{
		router:     config.Router,
		principals: config.Principals,
		serverInfo: config.ServerInfo,
	}, nil
}

// Handle decodes one bounded MCP message and returns the encoded JSON-RPC
// response, or nil for a valid JSON-RPC notification. A tools/call message uses
// the YAML-aware decoder: a hard 2 MiB cap before decoding and then the named
// operation's api.MaxEnvelopeBytes bound. Every other method keeps the declared
// 1 MiB bound.
func (edge *Edge) Handle(ctx context.Context, credential string, body []byte) []byte {
	if len(body) > api.MaxYAMLRequestBytes {
		return encodeError(nil, api.CodeInvalidInput, "MCP request exceeds the maximum allowed size")
	}

	// Peek only the method: tools/call has its own operation-aware bound, while
	// every other method is decoded by the declared 1 MiB decoder. A malformed
	// body falls through and keeps the existing stable refusal.
	var envelope struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &envelope)
	if envelope.Method == api.MCPMethodToolsCall {
		params, err := api.DecodeMCPToolsCall(body)
		if err != nil {
			return encodeError(requestID(body), api.CodeInvalidInput, messageOf(err))
		}
		return edge.handleToolsCall(ctx, credential, requestID(body), params)
	}

	request, err := api.DecodeMCPRequest(body)
	if err != nil {
		return encodeError(requestID(body), api.CodeInvalidInput, messageOf(err))
	}
	if request.IsNotification() {
		return nil
	}

	switch request.Method {
	case api.MCPMethodInitialize:
		return edge.handleInitialize(ctx, credential, request)
	case api.MCPMethodPing:
		return encodeResult(request.ID, json.RawMessage(`{}`))
	case api.MCPMethodToolsList:
		return edge.handleToolsList(ctx, credential, request)
	default:
		return encodeError(request.ID, api.CodeInvalidInput,
			fmt.Sprintf("unsupported MCP method %q", request.Method))
	}
}

// authenticate verifies the presented credential against the optional
// principal resolver, returning a sanitized JSON-RPC refusal when
// authentication fails and nil when the caller is authenticated (or no
// resolver is configured). Unassigned principals are permitted so any
// authenticated human or service can complete MCP discovery.
func (edge *Edge) authenticate(ctx context.Context, credential string, id json.RawMessage) []byte {
	if edge.principals == nil {
		return nil
	}
	principal, err := edge.principals.Resolve(authorize.WithUnassignedPrincipal(ctx), credential)
	if err != nil {
		return encodeFault(id, err)
	}
	if err := principal.Validate(); err != nil {
		return encodeFault(id, err)
	}
	return nil
}

// handleInitialize echoes the negotiated protocol version and the configured
// server identity after authenticating the caller when Principals is wired. It
// never dispatches an operation.
func (edge *Edge) handleInitialize(ctx context.Context, credential string, request api.MCPRequest) []byte {
	if refusal := edge.authenticate(ctx, credential, request.ID); refusal != nil {
		return refusal
	}
	var params api.MCPInitializeParams
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return encodeError(request.ID, api.CodeInvalidInput, "initialize params are invalid")
	}
	version, err := api.NegotiateMCPProtocolVersion(params.ProtocolVersion)
	if err != nil {
		return encodeError(request.ID, api.CodeInvalidInput, messageOf(err))
	}
	result := api.MCPInitializeResult{
		ProtocolVersion: version,
		Capabilities:    json.RawMessage(`{"tools":{}}`),
		ServerInfo:      edge.serverInfo,
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return encodeError(request.ID, api.CodeInternalFailure, "internal failure")
	}
	return encodeResult(request.ID, encoded)
}

// handleToolsList returns exactly ToolCatalog(): the registry's MCP-exposed
// operations in registry order with their field-level input schemas. When
// Principals is wired, listing authenticates the presented credential before
// advertising schemas while never dispatching an operation; an unusable catalog
// refuses rather than advertising an incomplete inventory.
func (edge *Edge) handleToolsList(ctx context.Context, credential string, request api.MCPRequest) []byte {
	if refusal := edge.authenticate(ctx, credential, request.ID); refusal != nil {
		return refusal
	}
	if toolCatalogErr != nil {
		return encodeError(request.ID, api.CodeInternalFailure, "internal failure")
	}
	encoded, err := json.Marshal(api.MCPToolsListResult{Tools: ToolCatalog()})
	if err != nil {
		return encodeError(request.ID, api.CodeInternalFailure, "internal failure")
	}
	return encodeResult(request.ID, encoded)
}

// handleToolsCall routes one named operation through the shared router with the
// presented credential and either the canonical api.Request envelope or flat
// top-level operation fields as arguments. The id is the already-validated
// JSON-RPC id; the arguments object is decoded with the named operation's own
// api.MaxEnvelopeBytes bound.
func (edge *Edge) handleToolsCall(ctx context.Context, credential string, id json.RawMessage, params api.MCPToolsCallParams) []byte {
	operation, ok := api.MCPToolOperationName(params.Name)
	if !ok {
		return encodeError(id, api.CodeInvalidInput, fmt.Sprintf("tools/call names unknown operation %q", params.Name))
	}
	var decoded api.Request
	switch strings.TrimSpace(string(params.Arguments)) {
	case "", "null":
		decoded = api.Request{}
	default:
		arguments, err := api.DecodeMCPOperationArguments(operation, params.Arguments)
		if err != nil {
			return encodeError(id, api.CodeInvalidInput, messageOf(err))
		}
		decoded = arguments
	}

	result, err := edge.router.Invoke(ctx, ops.Invocation{
		Operation:  authorize.OperationName(operation),
		Credential: credential,
		Request:    decoded,
	})
	if err != nil {
		return encodeToolFault(id, err)
	}
	if result.Operation != operation {
		return encodeError(id, api.CodeInternalFailure, "internal failure")
	}
	rawResult, err := json.Marshal(result)
	if err != nil {
		return encodeError(id, api.CodeInternalFailure, "internal failure")
	}
	if len(rawResult) > api.MaxResponseBytes {
		return encodeError(id, api.CodeInternalFailure,
			"MCP result exceeds the maximum allowed size")
	}
	encoded, err := api.EncodeMCPResult(result)
	if err != nil {
		return encodeError(id, api.CodeInternalFailure, "internal failure")
	}
	return encodeResult(id, encoded)
}

// encodeResult renders one successful JSON-RPC response.
func encodeResult(id json.RawMessage, result json.RawMessage) []byte {
	response := api.MCPResponse{JSONRPC: api.MCPJSONRPC, ID: id, Result: result}
	encoded, err := json.Marshal(response)
	if err != nil {
		return encodeError(id, api.CodeInternalFailure, "internal failure")
	}
	return encoded
}

// encodeError renders one public refusal. The MCP code is the declared mapping
// of the public code and the data body carries the stable api.Error.
func encodeError(id json.RawMessage, code string, message string) []byte {
	if message == "" {
		message = code
	}
	number, ok := api.MCPErrorCode(code)
	if !ok {
		code = api.CodeInternalFailure
		message = "internal failure"
		number, _ = api.MCPErrorCode(code)
	}
	data, err := api.EncodeMCPErrorData(api.Error{Code: code, Message: message})
	if err != nil {
		code = api.CodeInternalFailure
		message = "internal failure"
		data, _ = api.EncodeMCPErrorData(api.Error{Code: code, Message: message})
		number, _ = api.MCPErrorCode(code)
	}
	response := api.MCPResponse{
		JSONRPC: api.MCPJSONRPC,
		ID:      id,
		Error:   &api.MCPError{Code: number, Message: message, Data: data},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return []byte(`{"jsonrpc":"2.0","error":{"code":-32603,"message":"internal failure"}}`)
	}
	return encoded
}

// encodeFault maps a typed router fault to its public code and message. An
// unexpected error becomes a generic internal failure that never renders its
// cause.
func encodeFault(id json.RawMessage, err error) []byte {
	if class, ok := fault.ClassOf(err); ok {
		if code, ok := fault.CodeFor(class); ok {
			message, _ := fault.MessageOf(err)
			return encodeError(id, string(code), message)
		}
	}
	return encodeError(id, api.CodeInternalFailure, "internal failure")
}

// encodeToolFault maps a typed router refusal from tools/call: domain,
// authorization and operational refusals render as an MCP CallToolResult with
// isError=true carrying the sanitized public api.Error, while unauthenticated
// and internal/unexpected failures remain JSON-RPC errors.
func encodeToolFault(id json.RawMessage, err error) []byte {
	if class, ok := fault.ClassOf(err); ok {
		if code, ok := fault.CodeFor(class); ok && api.IsMCPToolErrorCode(string(code)) {
			message, _ := fault.MessageOf(err)
			if message == "" {
				message = string(code)
			}
			encoded, encodeErr := api.EncodeMCPToolError(api.Error{
				Code:    string(code),
				Message: message,
			})
			if encodeErr != nil {
				return encodeError(id, api.CodeInternalFailure, "internal failure")
			}
			return encodeResult(id, encoded)
		}
	}
	return encodeFault(id, err)
}

// requestID recovers the request id for a response to a message that failed
// strict validation. An unparseable message has no recoverable id and gets none;
// a parseable message with a missing or invalid id gets a safe numeric id so the
// response stays a valid JSON-RPC response.
func requestID(body []byte) json.RawMessage {
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	trimmed := strings.TrimSpace(string(envelope.ID))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage("0")
	}
	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return json.RawMessage("0")
	}
	switch value.(type) {
	case string, float64:
		return json.RawMessage(trimmed)
	default:
		return json.RawMessage("0")
	}
}

// messageOf returns the public message of a decode refusal without rendering any
// private cause or echoing the request body.
func messageOf(err error) string {
	if err == nil {
		return ""
	}
	var publicError *api.Error
	if errors.As(err, &publicError) && publicError != nil && publicError.Message != "" {
		return publicError.Message
	}
	if message, ok := fault.MessageOf(err); ok && message != "" {
		return message
	}
	return "invalid MCP request"
}
