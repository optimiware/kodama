package adapters

import (
	"context"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Entry is the durable decision recorded for one idempotency key: exactly one
// of RunID (a run was admitted) and Refusal (admission was refused, with the
// first recorded reason) is set. Decisions are final.
type Entry struct {
	RunID   domain.RunID
	Refusal string
}

// Admission is the result of Ledger.Admit.
type Admission struct {
	RunID domain.RunID
	// Created is true when this call created the run, false when the key
	// already named one.
	Created bool
}

// Ledger is the one admission store behind gitwebhook.Runs ("StartRun, FindRun
// and DecideRefusal over one admission store"): run creation and the refusal
// ledger share one atomic unit per idempotency key, so a key is decided exactly
// once and never as both. Every method is scoped: a key of another scope is a
// different key, and an adapter never reveals another tenant's decision.
//
// The PostgreSQL implementation is internal/integrations/gitwebhook/postgres.
// ledgertest holds the memory implementation and the contract both run.
type Ledger interface {
	// Lookup returns the decision recorded under key, found=false when none.
	Lookup(ctx context.Context, scope domain.Scope, key string) (entry Entry, found bool, err error)
	// Admit creates the run of plan under key in the same unit that records the
	// key, unless the key was already decided: an existing run is returned with
	// Created=false and nothing is created; a recorded refusal returns
	// gitwebhook.ErrAdmissionClosed and creates nothing, even for a caller that
	// planned before the refusal and resumes after it. Errors of the run
	// admission itself (execution's classified errors) roll the unit back and
	// are returned unchanged, leaving the key undecided.
	Admit(ctx context.Context, scope domain.Scope, key string, plan *execution.AdmissionPlan) (Admission, error)
	// Decide records the refusal reason for key unless the key is already
	// decided: an existing run or an earlier refusal wins and is returned.
	Decide(ctx context.Context, scope domain.Scope, key, reason string) (Entry, error)
}
