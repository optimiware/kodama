package execution

import "context"

// TransitionPort applies one complete execution-transition commit unit.
//
// The adapter compares the expected execution/action versions and re-reads the
// supplied current authority inside the same transaction as the state write,
// credential change, receipt, audit and event publication intent. It is not a
// second state machine: the next states come from the pure Kernel commit.
type TransitionPort interface {
	Transition(context.Context, TransitionUnit) (TransitionResult, error)
}
