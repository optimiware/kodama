package http

import (
	"fmt"
	"testing"

	"github.com/optimiware/kodama/internal/workspace"
)

// The synchronization refusals of internally hosted workspaces are 409s with a
// safe code, never a 500.
func TestUnitMapErrorInternalHostingRefusals(t *testing.T) {
	for name, err := range map[string]error{
		"checkpoint required": fmt.Errorf("refusing admission: %w", workspace.ErrCheckpointRequired),
		"push pending":        fmt.Errorf("x: %w", workspace.ErrPushPending),
		"writer blocked":      fmt.Errorf("%w: %w", workspace.ErrWriterBlocked, workspace.ErrPushPending),
	} {
		got := mapError(err, nil)
		if got == nil || got.Status != 409 || got.Code != CodeStateConflict {
			t.Fatalf("%s: %+v", name, got)
		}
		if name == "checkpoint required" && got.Details["lastError"] != workspace.ReasonUnacceptedPush {
			t.Fatalf("%s: %+v", name, got.Details)
		}
	}
}

// A failure of the push records is retryable (503), not a state conflict.
func TestUnitMapErrorPushRecordOutageIsRetryable(t *testing.T) {
	got := mapError(fmt.Errorf("push records: %w", workspace.ErrUnavailable), nil)
	if got == nil || got.Status != 503 {
		t.Fatalf("%+v", got)
	}
	if got := mapError(workspace.ErrNotConfigured, nil); got == nil || got.Status != 503 {
		t.Fatalf("%+v", got)
	}
}
