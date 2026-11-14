// Package httpapi is the thin REST edge over the single operation router.
//
// It is a transport only: it matches the declared method/path table, extracts
// the presented bearer credential, decodes exactly one bounded api.Request
// envelope for every method (GET and DELETE included), and hands one
// ops.Invocation to the router. It never authenticates, authorizes, looks up a
// resource or performs an effect itself, so REST and MCP cannot invent a grant
// or a hidden operation.
//
// A successful call is HTTP 200 with an api.Result body. A refusal is the
// stable api.Error body with the declared api.HTTPStatus mapping; a fault's
// public code/message survive while a private cause is never rendered.
//
// The one exception is the fixed unauthenticated discovery route
// api.DiscoveryPath, which is outside the 58-operation registry: it is served
// from an optional consumer-owned DiscoveryProvider before operation matching,
// validates the provider's document on every response and never dispatches an
// operation or reads a credential.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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

// DiscoveryProvider is the consumer-owned port for the one unauthenticated
// discovery document. The edge never builds, caches or trusts a configured
// document of its own: the composition supplies the current document and the
// edge validates it before every response. The port carries no credential and
// is independent of the operation router.
type DiscoveryProvider interface {
	Discovery(context.Context) (api.DiscoveryDocument, error)
}

// Config is the REST edge construction input. Router is required. Discovery is
// optional: a nil provider makes the fixed discovery route a clean 404 instead
// of a 500.
type Config struct {
	Router    Router
	Discovery DiscoveryProvider
}

// Edge is the REST transport. It holds no per-request state and is safe for
// concurrent use.
type Edge struct {
	router    Router
	discovery DiscoveryProvider
}

// NewEdge builds the REST edge over the shared router without a discovery
// provider. A nil router is refused.
func NewEdge(router Router) (*Edge, error) {
	return NewEdgeWithConfig(Config{Router: router})
}

// NewEdgeWithConfig builds the REST edge from its optional composition inputs.
// A nil router is refused; a nil discovery provider disables the discovery route
// as a clean 404.
func NewEdgeWithConfig(config Config) (*Edge, error) {
	if config.Router == nil {
		return nil, fault.New(fault.InvalidInput, "REST edge router is nil")
	}
	return &Edge{router: config.Router, discovery: config.Discovery}, nil
}

// ServeHTTP routes one in-process REST request through the shared router.
func (edge *Edge) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request == nil || request.URL == nil {
		writeError(writer, fault.New(fault.InvalidInput, "request is missing"))
		return
	}

	// No declared route carries a query string or fragment; a query is a
	// request shape refusal, not an unknown path.
	if request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Fragment != "" {
		writeError(writer, fault.New(fault.InvalidInput, "query strings and fragments are not part of any public route"))
		return
	}

	// The discovery document is the one fixed unauthenticated root route. It is
	// served before operation matching and never reaches the router, so it
	// cannot be shadowed by or shadow a registry route and any presented
	// Authorization header is ignored rather than validated.
	if request.URL.Path == api.DiscoveryPath {
		edge.serveDiscovery(writer, request)
		return
	}

	route, params, ok := api.Match(api.Method(request.Method), request.URL.Path)
	if !ok {
		writeError(writer, fault.New(fault.NotFound, "no operation is bound to this method and path"))
		return
	}

	body, err := readRequest(request, api.MaxEnvelopeBytes(route.Operation))
	if err != nil {
		writeError(writer, err)
		return
	}
	if route.Method == api.MethodGet && len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	decoded, err := api.DecodeOperationRequest(route.Operation, body)
	if err != nil {
		writeError(writer, err)
		return
	}

	result, err := edge.router.Invoke(request.Context(), ops.Invocation{
		Operation:  authorize.OperationName(route.Operation),
		Credential: bearerCredential(request.Header.Get("Authorization")),
		Request:    decoded,
		Params:     params,
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	if result.Operation != route.Operation {
		writeError(writer, fault.New(fault.InternalFailure, "internal failure"))
		return
	}
	if err := result.Validate(); err != nil {
		slog.Error("operation result failed validation", "operation", route.Operation, "causes", causeChain(err))
		writeError(writer, fault.New(fault.InternalFailure, "internal failure"))
		return
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		writeError(writer, fault.New(fault.InternalFailure, "internal failure"))
		return
	}
	if len(encoded) > api.MaxResponseBytes {
		writeError(writer, fault.New(fault.InternalFailure, "internal failure"))
		return
	}

	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(api.HTTPStatusSuccess)
	_, _ = writer.Write(encoded)
}

// discoveryUnavailableMessage is the fixed, secret-free public message for any
// provider or document failure at the discovery route.
const discoveryUnavailableMessage = "discovery document is unavailable"

// serveDiscovery serves api.DiscoveryPath with the exact declared shape: GET
// and HEAD only, no request body, no query string (refused by ServeHTTP before
// this point), a provider-owned document validated on every response, strict
// non-HTML-escaped JSON and a bounded body. The document is public, so it is
// cacheable and no credential is read or logged.
func (edge *Edge) serveDiscovery(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		writeFixedError(writer, http.StatusMethodNotAllowed, api.Error{
			Code:    api.CodeInvalidInput,
			Message: "the discovery route accepts only GET and HEAD",
		})
		return
	}

	if hasRequestBody(request) {
		writeError(writer, fault.New(fault.InvalidInput,
			"the discovery route does not accept a request body"))
		return
	}

	if edge.discovery == nil {
		writeError(writer, fault.New(fault.NotFound,
			"no discovery document is configured"))
		return
	}

	document, err := edge.discovery.Discovery(request.Context())
	if err != nil {
		// The provider's error text may carry private material, so it is never
		// rendered.
		writeFixedError(writer, http.StatusServiceUnavailable, api.Error{
			Code:    api.CodeDependencyUnavailable,
			Message: discoveryUnavailableMessage,
		})
		return
	}
	if err := document.Validate(); err != nil {
		// A provider returning an invalid document is a server-side fault: the
		// route refuses rather than serve a partial or invalid document.
		writeFixedError(writer, http.StatusServiceUnavailable, api.Error{
			Code:    api.CodeInternalFailure,
			Message: discoveryUnavailableMessage,
		})
		return
	}

	encoded, err := encodeDiscoveryDocument(document)
	if err != nil || len(encoded) > api.MaxDiscoveryBytes {
		writeFixedError(writer, http.StatusServiceUnavailable, api.Error{
			Code:    api.CodeInternalFailure,
			Message: discoveryUnavailableMessage,
		})
		return
	}

	writer.Header().Set("Content-Type", api.MediaType)
	writer.Header().Set("Cache-Control", "public, max-age=300")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
	writer.WriteHeader(http.StatusOK)
	if request.Method == http.MethodHead {
		return
	}
	_, _ = writer.Write(encoded)
}

// encodeDiscoveryDocument serializes the validated struct deterministically.
// The struct order fixes the member order and HTML escaping is disabled so a
// legitimate URL character is not rendered as a JSON escape.
func encodeDiscoveryDocument(document api.DiscoveryDocument) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// hasRequestBody reports whether one discovery request carries any body. An
// unknown length is probed with a single byte; the route refuses it either way.
func hasRequestBody(request *http.Request) bool {
	if request.Body == nil || request.Body == http.NoBody {
		return false
	}
	if request.ContentLength > 0 {
		return true
	}
	if request.ContentLength == 0 {
		return false
	}
	var probe [1]byte
	read, _ := request.Body.Read(probe[:])
	return read > 0
}

// writeFixedError renders one caller-fixed public error body with an explicit
// status. It is only used where the declared per-operation status mapping does
// not apply, and it never renders a private cause.
func writeFixedError(writer http.ResponseWriter, status int, publicError api.Error) {
	encoded, err := json.Marshal(publicError)
	if err != nil {
		encoded = []byte(`{"code":"internal_failure","message":"internal failure"}`)
		status = http.StatusInternalServerError
	}
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}

// readRequest reads exactly one request body bounded by the matched operation's
// api.MaxEnvelopeBytes bound (2 MiB for the three YAML-carrying operations, the
// declared 1 MiB otherwise). A body over the bound is an invalid_input refusal
// before decoding.
func readRequest(request *http.Request, bound int) ([]byte, error) {
	if request.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, int64(bound)+1))
	if err != nil {
		return nil, fault.New(fault.InternalFailure, "internal failure")
	}
	if len(body) > bound {
		return nil, fault.New(fault.InvalidInput,
			"request body exceeds the maximum allowed size")
	}
	return body, nil
}

// bearerCredential extracts the raw bearer material. An absent or malformed
// header yields the empty credential, which the router's authenticator refuses.
func bearerCredential(header string) string {
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return ""
	}
	return fields[1]
}

// writeError renders one public refusal with the declared status mapping. A
// private cause, a credential and an unexpected error text are never rendered.
func writeError(writer http.ResponseWriter, err error) {
	publicError := publicErrorOf(err)
	if publicError.Code == api.CodeInternalFailure {
		// The client sees only "internal failure"; the operator needs the cause.
		// The chain is the private causes' own messages: database and driver
		// diagnostics, never a credential (connection strings are not rendered).
		slog.Error("internal failure", "causes", causeChain(err))
	}
	status, ok := api.HTTPStatus(publicError.Code)
	if !ok {
		publicError = api.Error{Code: api.CodeInternalFailure, Message: "internal failure"}
		status = http.StatusInternalServerError
	}
	encoded, marshalErr := json.Marshal(publicError)
	if marshalErr != nil {
		encoded = []byte(`{"code":"internal_failure","message":"internal failure"}`)
		status = http.StatusInternalServerError
	}
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}

// causeChain renders an error and its wrapped causes for the server log. The
// rendered text is redacted: the client still sees only the public fault, while
// the operator sees the diagnostic without a DSN password, Authorization value
// or private key that an adapter may have put into its error text.
func causeChain(err error) []string {
	var chain []string
	for ; err != nil && len(chain) < 8; err = errors.Unwrap(err) {
		chain = append(chain, ops.RedactForLog(err.Error()))
	}
	return chain
}

// publicErrorOf maps a router or decode error to the stable public body. A
// typed fault keeps its class/code/message; anything else is a generic internal
// failure.
func publicErrorOf(err error) api.Error {
	if err == nil {
		return api.Error{Code: api.CodeInternalFailure, Message: "internal failure"}
	}
	var publicError *api.Error
	if errors.As(err, &publicError) && publicError != nil &&
		api.IsErrorCode(publicError.Code) && publicError.Message != "" {
		return api.Error{Code: publicError.Code, Message: publicError.Message}
	}
	if class, ok := fault.ClassOf(err); ok {
		if code, ok := fault.CodeFor(class); ok {
			message, _ := fault.MessageOf(err)
			if message == "" {
				message = string(code)
			}
			return api.Error{Code: string(code), Message: message}
		}
	}
	return api.Error{Code: api.CodeInternalFailure, Message: "internal failure"}
}
