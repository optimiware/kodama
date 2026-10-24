package protocol

import (
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/optimiware/kodama/internal/mcpproxy"
)

// Test_protocol_error_classes: what a workload learns from a failed tools
// method. Ended sessions and credentials are the same unknown tool or session;
// authority loss is not authorized; an outage is an internal error, which the
// client may retry, not an assertion about the session.
func Test_protocol_error_classes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int64
		msg  string
	}{
		{mcpproxy.ErrCredential, jsonrpc.CodeInvalidParams, "unknown tool or session"},
		{mcpproxy.ErrBindingInvalid, jsonrpc.CodeInvalidParams, "unknown tool or session"},
		{mcpproxy.ErrSessionInvalid, jsonrpc.CodeInvalidParams, "unknown tool or session"},
		{mcpproxy.ErrToolHidden, jsonrpc.CodeInvalidParams, "unknown tool or session"},
		{mcpproxy.ErrDenied, jsonrpc.CodeInvalidRequest, "not authorized"},
		{mcpproxy.ErrUnavailable, jsonrpc.CodeInternalError, "the proxy could not complete the request"},
		{errors.New("sql: secret detail"), jsonrpc.CodeInternalError, "the proxy could not complete the request"},
	} {
		var je *jsonrpc.Error
		if !errors.As(protoError(tc.err), &je) {
			t.Fatalf("%v: not a JSON-RPC error", tc.err)
		}
		if je.Code != tc.code || je.Message != tc.msg {
			t.Fatalf("%v: got %d %q, want %d %q", tc.err, je.Code, je.Message, tc.code, tc.msg)
		}
	}
}
