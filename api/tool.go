package api

import "encoding/json"

// Optional tool-invocation wire vocabulary.
//
// Tool arguments are an opaque JSON object owned by the configured tool; the
// public envelope only requires that the member is a JSON object and never
// descends into it.

// ToolInvokeRequest is the tool.invoke input. The project is carried explicitly
// for MCP callers and is optional for REST callers whose route supplies it. The
// tool route segment is a configured tool name, so it is an opaque bounded name
// rather than a Kodama identity.
type ToolInvokeRequest struct {
	ProjectID string          `json:"project_id,omitempty" schema:"id"`
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments" schema:"arguments"`
}

// Validate checks the optional project identity, the bounded opaque tool name
// and that the arguments member is a JSON object.
func (request ToolInvokeRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id is invalid")
	}
	if request.Tool != "" && len(request.Tool) > MaxSubjectValueBytes {
		return invalid("tool name is %d bytes; at most %d are allowed", len(request.Tool), MaxSubjectValueBytes)
	}
	if !jsonObject(request.Arguments) {
		return invalid("tool arguments must be a JSON object")
	}
	return nil
}

// ToolInvokeResult is the tool.invoke result.
type ToolInvokeResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

// Validate accepts any bounded content and error flag. The empty content and a
// false flag are legitimate observations.
func (result ToolInvokeResult) Validate() error {
	if len(result.Content) > MaxResponseBytes {
		return invalid("tool content is %d bytes; at most %d are allowed", len(result.Content), MaxResponseBytes)
	}
	return nil
}
