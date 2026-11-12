package engine

import (
	"context"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// Clock supplies authoritative transaction time. It is injected so Kernel tests
// use deterministic time and restarts keep the persisted deadline.
type Clock interface {
	Now() time.Time
}

// State is the consumer-owned durable state port. It loads one execution,
// applies exactly one complete compare-and-commit transition payload, and scans
// due or unfinished work for recovery.
//
// There is deliberately no split Save/Update/SetState/Persist: a transition is
// one atomic commit unit with expected versions and every intent.
type State interface {
	Load(ctx context.Context, executionID id.ExecutionID) (kernel.Execution, error)
	Commit(ctx context.Context, commit flow.Commit) (kernel.Execution, error)
	Scan(ctx context.Context, request ScanRequest) ([]id.ExecutionID, error)
}

// Runtime is the consumer-owned workload port implemented by Shell adapters.
// Every blocking call takes a context first and returns structured evidence
// instead of collapsing uncertainty into a boolean.
type Runtime interface {
	Capabilities(ctx context.Context) (Capabilities, error)
	Prepare(ctx context.Context, request PrepareRequest) (Preparation, error)
	Start(ctx context.Context, request StartRequest) (StartResult, error)
	Observe(ctx context.Context, request ObserveRequest) (kernel.Observation, error)
	Logs(ctx context.Context, request LogsRequest) (LogBatch, error)
	Stop(ctx context.Context, request StopRequest) (kernel.StopEvidence, error)
	Remove(ctx context.Context, request RemoveRequest) (Removal, error)
	RefreshCredential(ctx context.Context, request CredentialRequest) (CredentialRefresh, error)
}

// Workspace is the consumer-owned retained-workspace port. It is separate from
// Runtime because workspace ownership differs from workload ownership.
type Workspace interface {
	Status(ctx context.Context, workspaceID id.WorkspaceID) (WorkspaceObservation, error)
	Retain(ctx context.Context, workspaceID id.WorkspaceID) error
	Release(ctx context.Context, workspaceID id.WorkspaceID) error
}
