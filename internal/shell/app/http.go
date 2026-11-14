package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
	"github.com/optimiware/kodama/internal/shell/transport/mcp"
)

// publicHandler mounts the REST edge at the declared routes, the MCP
// Streamable HTTP JSON-RPC edge at the composition path, and the RFC 9728 OAuth
// Protected Resource Metadata endpoint when login discovery is configured.
type publicHandler struct {
	rest      http.Handler
	mcp       *mcp.Edge
	mcpPath   string
	discovery httpapi.DiscoveryProvider
	llmProxy  http.Handler
}

func newPublicHandler(rest http.Handler, edge *mcp.Edge, mcpPath string, discovery httpapi.DiscoveryProvider, llmProxy http.Handler) http.Handler {
	return &publicHandler{
		rest:      rest,
		mcp:       edge,
		mcpPath:   mcpPath,
		discovery: discovery,
		llmProxy:  llmProxy,
	}
}

func (handler *publicHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request != nil && request.URL != nil {
		if request.URL.Path == handler.mcpPath {
			handler.serveMCP(writer, request)
			return
		}
		if request.URL.Path == api.OAuthProtectedResourcePath {
			handler.serveOAuthProtectedResource(writer, request)
			return
		}
		if handler.llmProxy != nil && strings.HasPrefix(request.URL.Path, "/llm/") {
			handler.llmProxy.ServeHTTP(writer, request)
			return
		}
	}
	handler.rest.ServeHTTP(writer, request)
}

func (handler *publicHandler) serveMCP(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Origin") != "" && !handler.mcpOwnOrigin(request) {
		writeHTTPError(writer, fault.New(fault.Forbidden, "the MCP endpoint refuses requests from another origin"))
		return
	}
	if request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Fragment != "" {
		writeHTTPError(writer, fault.New(fault.InvalidInput, "query strings and fragments are not part of the MCP path"))
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeHTTPFixedError(writer, http.StatusMethodNotAllowed, api.Error{
			Code:    api.CodeInvalidInput,
			Message: "the MCP endpoint accepts only POST",
		})
		return
	}
	if request.Body == nil {
		request.Body = http.NoBody
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, api.MaxYAMLRequestBytes+1))
	if err != nil {
		writeHTTPError(writer, fault.New(fault.InternalFailure, "internal failure"))
		return
	}
	response := handler.mcp.Handle(request.Context(), bearerToken(request.Header.Get("Authorization")), body)
	if len(response) == 0 {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	if isUnauthenticatedMCPResponse(response) {
		writer.Header().Set("WWW-Authenticate", handler.mcpAuthenticateHeader(request.Context()))
		writer.Header().Set("Content-Type", api.MediaType)
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write(response)
		return
	}
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(api.HTTPStatusSuccess)
	_, _ = writer.Write(response)
}

// mcpOwnOrigin reports whether the browser Origin of an MCP request is this
// server's own: the configured public API origin or, when none is configured,
// the request host. Only the configured origin also stops a DNS-rebinding page,
// whose Host header names the attacker.
func (handler *publicHandler) mcpOwnOrigin(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if handler.discovery != nil {
		document, err := handler.discovery.Discovery(request.Context())
		if err != nil {
			return false
		}
		public, err := url.Parse(document.APIURL)
		return err == nil && strings.EqualFold(origin, public.Scheme+"://"+public.Host)
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, request.Host)
}

func (handler *publicHandler) serveOAuthProtectedResource(writer http.ResponseWriter, request *http.Request) {
	if request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Fragment != "" {
		writeHTTPError(writer, fault.New(fault.InvalidInput, "query strings and fragments are not part of any public route"))
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		writeHTTPFixedError(writer, http.StatusMethodNotAllowed, api.Error{
			Code:    api.CodeInvalidInput,
			Message: "the protected resource metadata route accepts only GET and HEAD",
		})
		return
	}
	if hasHTTPRequestBody(request) {
		writeHTTPError(writer, fault.New(fault.InvalidInput, "the protected resource metadata route does not accept a request body"))
		return
	}
	if handler.discovery == nil {
		writeHTTPError(writer, fault.New(fault.NotFound, "no discovery document is configured"))
		return
	}
	document, err := handler.discovery.Discovery(request.Context())
	if err != nil {
		writeHTTPFixedError(writer, http.StatusServiceUnavailable, api.Error{
			Code:    api.CodeDependencyUnavailable,
			Message: "discovery document is unavailable",
		})
		return
	}
	if err := document.Validate(); err != nil {
		writeHTTPFixedError(writer, http.StatusServiceUnavailable, api.Error{
			Code:    api.CodeInternalFailure,
			Message: "discovery document is unavailable",
		})
		return
	}
	metadata := api.OAuthProtectedResourceMetadata{
		Resource:               document.APIURL,
		AuthorizationServers:   []string{document.Issuer},
		BearerMethodsSupported: []string{"header"},
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(metadata); err != nil {
		writeHTTPFixedError(writer, http.StatusServiceUnavailable, api.Error{
			Code:    api.CodeInternalFailure,
			Message: "discovery document is unavailable",
		})
		return
	}
	encoded := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
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

func (handler *publicHandler) mcpAuthenticateHeader(ctx context.Context) string {
	if handler.discovery != nil {
		document, err := handler.discovery.Discovery(ctx)
		if err == nil && document.Validate() == nil && document.APIURL != "" {
			resourceMetadata := strings.TrimRight(document.APIURL, "/") + api.OAuthProtectedResourcePath
			return fmt.Sprintf("Bearer resource_metadata=%q", resourceMetadata)
		}
	}
	return "Bearer"
}

func isUnauthenticatedMCPResponse(raw []byte) bool {
	var response api.MCPResponse
	if err := json.Unmarshal(raw, &response); err != nil || response.Error == nil {
		return false
	}
	unauthCode, ok := api.MCPErrorCode(api.CodeUnauthenticated)
	if ok && response.Error.Code == unauthCode {
		return true
	}
	if len(response.Error.Data) > 0 {
		if publicError, err := api.DecodeMCPErrorData(response.Error.Data); err == nil {
			return publicError.Code == api.CodeUnauthenticated
		}
	}
	return false
}

func hasHTTPRequestBody(request *http.Request) bool {
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

// writeHTTPFixedError writes an explicit HTTP status with a stable api.Error
// body.
func writeHTTPFixedError(writer http.ResponseWriter, status int, publicError api.Error) {
	body, err := json.Marshal(publicError)
	if err != nil {
		body = []byte(`{"code":"internal_failure","message":"internal failure"}`)
		status = http.StatusInternalServerError
	}
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

// writeHTTPError renders one stable api.Error body with its status.
func writeHTTPError(writer http.ResponseWriter, err error) {
	publicError := api.Error{Code: api.CodeInternalFailure, Message: "internal failure"}
	if class, ok := fault.ClassOf(err); ok {
		if code, ok := fault.CodeFor(class); ok {
			message, _ := fault.MessageOf(err)
			if message == "" {
				message = string(code)
			}
			publicError = api.Error{Code: string(code), Message: message}
		}
	}
	status, ok := api.HTTPStatus(publicError.Code)
	if !ok {
		publicError = api.Error{Code: api.CodeInternalFailure, Message: "internal failure"}
		status = http.StatusInternalServerError
	}
	body, marshalErr := json.Marshal(publicError)
	if marshalErr != nil {
		body = []byte(`{"code":"internal_failure","message":"internal failure"}`)
		status = http.StatusInternalServerError
	}
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}
