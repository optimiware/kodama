// Package mcp is the thin MCP JSON-RPC 2.0 edge over the single operation
// router.
//
// It supports the initialize handshake, the tools/list catalog and tools/call.
// Every other method refuses with the stable invalid_input mapping. initialize
// and tools/list never dispatch an operation; tools/call names one registry
// operation and passes the strict api.Request envelope to the router with the
// presented credential. The edge never authenticates, authorizes, looks up a
// resource or performs an effect itself.
//
// A success result is the JSON encoding of api.Result. An error carries the
// declared api.MCPErrorCode mapping plus the api.Error body in data. A private
// fault cause and the presented credential are never rendered.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// Router is the single operation boundary both public edges call.
type Router interface {
	Invoke(context.Context, ops.Invocation) (api.Result, error)
}

// Config is the MCP edge construction input. Both the router and the server
// identity are required.
type Config struct {
	Router     Router
	ServerInfo api.MCPImplementation
}

// Edge is the MCP transport. It holds no per-request state and is safe for
// concurrent use.
type Edge struct {
	router     Router
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
	return &Edge{router: config.Router, serverInfo: config.ServerInfo}, nil
}

// Handle decodes one bounded MCP message and returns exactly one encoded
// JSON-RPC response. A tools/call message uses the YAML-aware decoder: a hard
// 2 MiB cap before decoding and then the named operation's
// api.MaxEnvelopeBytes bound. Every other method keeps the declared 1 MiB
// bound.
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

	switch request.Method {
	case api.MCPMethodInitialize:
		return edge.handleInitialize(request)
	case api.MCPMethodToolsList:
		return edge.handleToolsList(request)
	default:
		return encodeError(request.ID, api.CodeInvalidInput,
			fmt.Sprintf("unsupported MCP method %q", request.Method))
	}
}

// handleInitialize echoes the pinned protocol version and the configured server
// identity. It never dispatches an operation.
func (edge *Edge) handleInitialize(request api.MCPRequest) []byte {
	result := api.MCPInitializeResult{
		ProtocolVersion: api.MCPProtocolVersion,
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
// operations in registry order with their field-level input schemas. Listing
// needs no credential and dispatches nothing; an unusable catalog refuses
// rather than advertising an incomplete inventory.
func (edge *Edge) handleToolsList(request api.MCPRequest) []byte {
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
// presented credential and the strict request envelope as arguments. The id is
// the already-validated JSON-RPC id; the arguments envelope is decoded with the
// named operation's own api.MaxEnvelopeBytes bound.
func (edge *Edge) handleToolsCall(ctx context.Context, credential string, id json.RawMessage, params api.MCPToolsCallParams) []byte {
	var decoded api.Request
	switch strings.TrimSpace(string(params.Arguments)) {
	case "", "null":
		decoded = api.Request{}
	default:
		arguments, err := api.DecodeOperationRequest(params.Name, params.Arguments)
		if err != nil {
			return encodeError(id, api.CodeInvalidInput, messageOf(err))
		}
		decoded = arguments
	}

	result, err := edge.router.Invoke(ctx, ops.Invocation{
		Operation:  authorize.OperationName(params.Name),
		Credential: credential,
		Request:    decoded,
	})
	if err != nil {
		return encodeFault(id, err)
	}
	if result.Operation != params.Name {
		return encodeError(id, api.CodeInternalFailure, "internal failure")
	}
	encoded, err := api.EncodeMCPResult(result)
	if err != nil {
		return encodeError(id, api.CodeInternalFailure, "internal failure")
	}
	if len(encoded) > api.MaxResponseBytes {
		return encodeError(id, api.CodeInternalFailure,
			"MCP result exceeds the maximum allowed size")
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
