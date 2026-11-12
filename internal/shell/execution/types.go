// Package execution declares the Shell execution transition commit unit.
//
// Execution and action states are Kernel vocabulary; the Shell never invents a
// second state machine. A transition unit carries the expected execution/action
// versions, the pure Kernel commit, optional credential clearing, receipt,
// audit, event publication intent, current-authority reads and post-commit
// effects, so the adapter can apply one atomic settlement.
package execution

import (
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// TransitionUnit is the complete atomic execution-transition commit unit.
type TransitionUnit struct {
	Owner                    tenancy.Scope             `json:"owner"`
	ExecutionID              id.ExecutionID            `json:"execution_id"`
	ExpectedExecutionVersion uint64                    `json:"expected_execution_version"`
	ExpectedActionVersion    uint64                    `json:"expected_action_version"`
	Commit                   flow.Commit               `json:"commit"`
	CredentialUpdate         *token.Credential         `json:"credential_update,omitempty"`
	Receipt                  receipt.Record            `json:"receipt"`
	Audit                    []event.Audit             `json:"audit"`
	Events                   []event.Envelope          `json:"events"`
	Authority                []authorize.AuthorityRead `json:"authority"`
	PostCommit               []event.PostCommitEffect  `json:"post_commit"`
}

// TransitionResult is the committed transition result.
type TransitionResult struct {
	ExecutionVersion uint64                `json:"execution_version"`
	ActionVersion    uint64                `json:"action_version"`
	NextExecution    kernel.ExecutionState `json:"next_execution_state"`
	NextAction       kernel.ActionState    `json:"next_action_state"`
	Replayed         bool                  `json:"replayed"`
}
