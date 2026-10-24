package agent

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/optimiware/kodama/internal/cli/client"
)

// profilesPath is the only collection this package addresses.
var profilesPath = []string{"agent-profiles"}

func listRequest(scope client.Scope, q url.Values) client.Request {
	return client.Request{Method: http.MethodGet, Scope: scope, Path: profilesPath, Query: q}
}

// registerRequest sends the submitted profile object unchanged. Registration
// is content-addressed, so it carries neither If-Match nor Idempotency-Key.
func registerRequest(scope client.Scope, profile json.RawMessage) client.Request {
	return client.Request{Method: http.MethodPost, Scope: scope, Path: profilesPath, Body: profile}
}
