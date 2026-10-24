package executionhttp

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/httpapi"
)

// Safe error mapping: every execution failure class has one status and code and
// a constant message that never echoes the wrapped detail.
func Test_runs_error_mapping(t *testing.T) {
	const leak = "SECRET-DETAIL-postgres://user:pw@host/db"
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{execution.ErrNotFound, 404, "not_found"}, {execution.ErrHidden, 404, "not_found"}, {execution.ErrDenied, 403, "forbidden"},
		{execution.ErrPurposeDenied, 403, "forbidden"}, {execution.ErrInvalid, 400, "invalid_request"},
		{execution.ErrAdmissionRefused, 422, "admission_refused"}, {execution.ErrWorkCancelled, 409, "work_item_not_runnable"},
		{execution.ErrNotActionable, 409, "work_item_not_runnable"}, {execution.ErrConflict, 409, "run_conflict"},
		{execution.ErrInvalidTransition, 409, "run_conflict"}, {execution.ErrStale, 409, "run_conflict"},
		{execution.ErrUnavailable, 503, "unavailable"}, {execution.ErrNotConfigured, 503, "unavailable"}, {execution.ErrBusy, 503, "unavailable"},
		{execution.ErrQuotaExceeded, 503, "unavailable"},
	}
	for _, c := range cases {
		mapped := mapError(fmt.Errorf("%w: %s", c.err, leak))
		var api *httpapi.APIError
		if !errors.As(mapped, &api) {
			t.Errorf("%v: not an API error: %v", c.err, mapped)
			continue
		}
		if api.Status != c.status || string(api.Code) != c.code {
			t.Errorf("%v: %d %s, want %d %s", c.err, api.Status, api.Code, c.status, c.code)
		}
		if strings.Contains(api.Message, "SECRET") || strings.Contains(api.Message, "postgres") {
			t.Errorf("%v: the message echoes its cause: %q", c.err, api.Message)
		}
		if c.status == http.StatusServiceUnavailable && api.Header.Get("Retry-After") == "" {
			t.Errorf("%v: no Retry-After", c.err)
		}
	}
	// An unclassified failure stays unclassified for the router's fixed 500.
	if got := mapError(errors.New(leak)); got == nil || strings.Contains(got.Error(), "classified") {
		t.Errorf("unclassified: %v", got)
	}
	if _, ok := mapError(errors.New(leak)).(*httpapi.APIError); ok {
		t.Error("an unclassified error became an API error with its detail")
	}
}

func Test_runs_duplicate_keys(t *testing.T) {
	for body, bad := range map[string]bool{
		`{"a":1,"b":{"c":2}}`:                   false,
		`{"a":1,"a":2}`:                         true,
		`{"a":{"b":1,"b":2}}`:                   true,
		`{"a":[{"x":1},{"x":2}]}`:               false,
		`{"a":[{"x":1,"x":2}]}`:                 true,
		`{"inputs":{"region":"a"},"inputs":{}}`: true,
	} {
		if err := rejectDuplicateKeys([]byte(body)); (err != nil) != bad {
			t.Errorf("%s: %v", body, err)
		}
	}
}
