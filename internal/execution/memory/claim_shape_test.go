package memory

import (
	"errors"
	"fmt"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/memdb"
)

// TestExecutionClaimShapeMemory: the memory store refuses an attempt whose
// workspace binding does not match its access, exactly as the PostgreSQL
// constraint step_attempts_workspace_shape does.
func TestExecutionClaimShapeMemory(t *testing.T) {
	cases := []struct {
		name                       string
		access                     execution.Access
		workspace, input, expected string
		ok                         bool
	}{
		{"none without a workspace", execution.AccessNone, "", "", "", true},
		{"none with a workspace", execution.AccessNone, "ws", "", "", false},
		{"none with an input commit", execution.AccessNone, "", "c1", "", false},
		{"none with an expected tip", execution.AccessNone, "", "", "c1", false},
		{"read with workspace and input", execution.AccessRead, "ws", "c1", "", true},
		{"read without a workspace", execution.AccessRead, "", "c1", "", false},
		{"read without an input commit", execution.AccessRead, "ws", "", "", false},
		{"read with an expected tip", execution.AccessRead, "ws", "c1", "c1", false},
		{"write with workspace, input and tip", execution.AccessWrite, "ws", "c1", "c1", true},
		{"write without a workspace", execution.AccessWrite, "", "c1", "c1", false},
		{"write without an input commit", execution.AccessWrite, "ws", "", "c1", false},
		{"write without an expected tip", execution.AccessWrite, "ws", "c1", "", false},
		{"an unknown access", execution.Access("other"), "", "", "", false},
		{"no access", "", "", "", "", false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := guardStore(t)
			a := &execution.Attempt{
				Scope: gScope, ID: domain.AttemptID(fmt.Sprintf("att_shape_%d", i)), RunID: "run_g", StepID: "s", Ordinal: 1,
				Access: tc.access, WorkspaceID: tc.workspace, InputCommit: tc.input, ExpectedTip: tc.expected,
				Status: execution.AttemptRunning, Version: 1, CreatedAt: gNow, UpdatedAt: gNow,
			}
			err := in(t, s, func(tx memdb.Tx) error { return s.insertAttempt(tx, a) })
			switch {
			case tc.ok && err != nil:
				t.Fatalf("a well-shaped attempt was refused: %v", err)
			case !tc.ok && !errors.Is(err, execution.ErrInvalid):
				t.Fatalf("a mis-shaped attempt: %v", err)
			}
			if !tc.ok {
				var stored int
				if err := in(t, s, func(tx memdb.Tx) error {
					stored = len(collect(tx, attemptsT, func(execution.Attempt) bool { return true }))
					return nil
				}); err != nil || stored != 0 {
					t.Fatalf("a refused attempt was stored (%d, %v)", stored, err)
				}
			}
		})
	}
}
