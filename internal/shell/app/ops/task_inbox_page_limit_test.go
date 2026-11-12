package opsapp_test

import (
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// TestTaskInboxServesThePublicPageLimit proves the served task.inbox handler
// accepts every page limit the public contract accepts
// (api.MaxPageLimit = 200), keeps the default for an omitted or zero limit,
// and refuses 201 with the exact contract error. A narrower inbox-service
// bound would refuse 101..200 with invalid_input even though api.Page accepts
// them.
func TestTaskInboxServesThePublicPageLimit(t *testing.T) {
	h := newHarness(t)

	for _, limit := range []int{100, 101, api.MaxPageLimit} {
		_, err := h.invoke(t, testToken, "task.inbox", jsonBody(t, map[string]any{
			"page": map[string]any{"limit": limit},
		}), map[string]string{"project": testProject})
		if err != nil {
			t.Fatalf("task.inbox page.limit %d: %v", limit, err)
		}
	}

	// The default page size must not move: an omitted page and an explicit
	// zero are the same accepted default request.
	for _, input := range []map[string]any{{}, {"page": map[string]any{"limit": 0}}} {
		_, err := h.invoke(t, testToken, "task.inbox", jsonBody(t, input), map[string]string{"project": testProject})
		if err != nil {
			t.Fatalf("task.inbox default page %+v: %v", input, err)
		}
	}

	// 201 is outside the contract bound and must be refused with the exact
	// page-limit error, not an inbox-service bound.
	_, err := h.invoke(t, testToken, "task.inbox", jsonBody(t, map[string]any{
		"page": map[string]any{"limit": api.MaxPageLimit + 1},
	}), map[string]string{"project": testProject})
	if err == nil {
		t.Fatalf("task.inbox page.limit %d was accepted; want invalid_input", api.MaxPageLimit+1)
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("task.inbox page.limit %d fault class = %v (%v); want invalid_input", api.MaxPageLimit+1, class, err)
	}
	message, ok := fault.MessageOf(err)
	if !ok {
		t.Fatalf("task.inbox page.limit %d error %v has no public message", api.MaxPageLimit+1, err)
	}
	want := "page limit 201 is outside 0..200"
	if message != want {
		t.Fatalf("task.inbox page.limit %d message = %q; want %q", api.MaxPageLimit+1, message, want)
	}
}
