package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/transport/mcp"
)

// publicHandler mounts the REST edge at the declared routes and the MCP
// JSON-RPC edge at the one composition path.
type publicHandler struct {
	rest    http.Handler
	mcp     *mcp.Edge
	mcpPath string
}

func newPublicHandler(rest http.Handler, edge *mcp.Edge, mcpPath string) http.Handler {
	return &publicHandler{rest: rest, mcp: edge, mcpPath: mcpPath}
}

func (handler *publicHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL != nil && request.URL.Path == handler.mcpPath {
		handler.serveMCP(writer, request)
		return
	}
	handler.rest.ServeHTTP(writer, request)
}

func (handler *publicHandler) serveMCP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeHTTPError(writer, fault.New(fault.NotFound, "no operation is bound to this method and path"))
		return
	}
	if request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Fragment != "" {
		writeHTTPError(writer, fault.New(fault.InvalidInput, "query strings and fragments are not part of the MCP path"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, api.MaxYAMLRequestBytes+1))
	if err != nil {
		writeHTTPError(writer, fault.New(fault.InternalFailure, "internal failure"))
		return
	}
	response := handler.mcp.Handle(request.Context(), bearerToken(request.Header.Get("Authorization")), body)
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(api.HTTPStatusSuccess)
	_, _ = writer.Write(response)
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
