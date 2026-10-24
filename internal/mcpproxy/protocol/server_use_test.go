package protocol_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/protocol"
)

// sessionRuntime answers only what a request carrying an existing session id
// reaches before the SDK: authentication and the session check.
type sessionRuntime struct {
	protocol.Runtime
	session error
}

func (r sessionRuntime) Authenticate(context.Context, string, domain.AttemptID) (mcpproxy.Caller, error) {
	return mcpproxy.Caller{Scope: domain.Scope{TenantID: "t033_acme", ProjectID: "p033_inventory"}, RunID: "run_t033", AttemptID: "att_t033", Session: "rts_t033"}, nil
}

func (r sessionRuntime) Session(context.Context, mcpproxy.Caller, mcpproxy.RegistrationID, mcpproxy.SessionID) error {
	return r.session
}

func post(t *testing.T, rt protocol.Runtime) *http.Response {
	t.Helper()
	h, err := protocol.NewHandler(protocol.ServerConfig{Runtime: rt, IdleTimeout: time.Minute, MaxRequestBytes: 1 << 10})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/internal/mcp/v1/attempts/att_t033/servers/mcp_t033_a", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", "mcs_t033_0123456789abcdef")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// Test_session_check_outcomes: an ended session is the same not-found whatever
// ended it, so the response never reveals why; a dependency that gave no
// answer is a retryable unavailable, never a verdict about the session.
func Test_session_check_outcomes(t *testing.T) {
	var gone string
	for _, cause := range []error{mcpproxy.ErrSessionInvalid, mcpproxy.ErrNotFound, mcpproxy.ErrBindingInvalid, mcpproxy.ErrCredential, mcpproxy.ErrDenied} {
		res := post(t, sessionRuntime{session: cause})
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("%v: status = %d, want 404", cause, res.StatusCode)
		}
		if gone == "" {
			gone = string(body)
		} else if string(body) != gone {
			t.Fatalf("%v: body %q differs from %q: the response reveals the cause", cause, body, gone)
		}
	}
	for _, cause := range []error{mcpproxy.ErrUnavailable, mcpproxy.Unavailablef(errors.New("db down"), "the check failed")} {
		res := post(t, sessionRuntime{session: cause})
		if res.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("outage: status = %d, want 503", res.StatusCode)
		}
		if res.Header.Get("Retry-After") == "" {
			t.Fatal("an unavailable response must say it may be retried")
		}
	}
}
