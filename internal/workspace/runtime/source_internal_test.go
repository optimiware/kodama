package runtime

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/workspace"
)

// Every error the adapter can produce is a code the frozen proposeSource
// operation lists, with the status the contract assigns to that code.
func TestEveryMappedErrorIsDeclaredByTheContract(t *testing.T) {
	var op runtimev1.Operation
	for _, o := range runtimev1.Operations {
		if o.ID == "proposeSource" {
			op = o
		}
	}
	if op.ID == "" {
		t.Fatal("contract has no proposeSource operation")
	}
	for name, err := range map[string]error{
		"conflict":          &workspace.ConflictError{ObservedTip: workspace.Commit("a"), Reason: "tip_moved"},
		"proposal changed":  workspace.ErrProposalChanged,
		"admission closed":  workspace.ErrAdmissionClosed,
		"revoked":           workspace.ErrAttemptRevoked,
		"not the writer":    workspace.ErrNotWriter,
		"not found":         workspace.ErrNotFound,
		"bundle incomplete": workspace.ErrBundleIncomplete,
		"bundle invalid":    &workspace.BundleError{Reason: workspace.ReasonDigestMismatch},
		"secret":            workspace.ErrSecretInSource,
		"limit":             workspace.ErrLimitExceeded,
		"invalid":           workspace.ErrInvalidValue,
		"unavailable":       fmt.Errorf("%w: x", workspace.ErrUnavailable),
		"fenced":            workspace.ErrFenced,
		"unknown":           errors.New("anything else"),
	} {
		e := mapError(err)
		if !slices.Contains(op.Errors, e.Code) {
			t.Errorf("%s: code %q is not declared for proposeSource (%v)", name, e.Code, op.Errors)
		}
		if want, _ := runtimev1.Status(e.Code); e.Status != want {
			t.Errorf("%s: status %d, contract says %d", name, e.Status, want)
		}
		if body := e.Body("req_01"); body.Validate() != nil {
			t.Errorf("%s: %v", name, body.Validate())
		}
	}
}
