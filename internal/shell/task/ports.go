package task

import (
	"context"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// StartPort applies one complete start/admission commit unit.
type StartPort interface {
	Start(context.Context, StartUnit) (StartResult, error)
}

// AskPort applies one complete conversation-handoff commit unit.
type AskPort interface {
	Ask(context.Context, AskUnit) (AskResult, error)
}

// ReplyPort applies one complete reply commit unit.
type ReplyPort interface {
	Reply(context.Context, ReplyUnit) (ReplyResult, error)
}

// CancelPort applies one complete task-cancellation commit unit.
type CancelPort interface {
	Cancel(context.Context, CancelUnit) (CancelResult, error)
}

// ExecutionAggregateReader loads the current execution aggregate a task
// operation must reduce before it writes. It is the consumer-owned read seam
// the task service uses to build the Kernel cancel fact (`flow.CancelFact`)
// and to populate `AskUnit.Commit`/`CancelUnit.Commit`; the store adapter owns
// the implementation.
//
// The read is scoped: the scope is the owning task scope, and an execution
// outside it, an unknown execution and a missing execution are all the
// uniform `not_found` class, never `forbidden`, so the read exposes no
// existence oracle. The returned aggregate is the whole execution (state,
// version, current action and every action row), not a lossy view: the
// cancellation reduction needs the version-guarded pre-image.
type ExecutionAggregateReader interface {
	Load(ctx context.Context, scope tenancy.Scope, id id.ExecutionID) (kernel.Execution, error)
}
