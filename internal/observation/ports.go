package observation

import (
	"context"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
)

// The ports below are small and owned by this module. Adapters implement them;
// the composition root wires them. Nothing has an allow-all default.

// Actor is the verified caller of a stream. Observation never authenticates: the
// transport supplies the identity its injected authenticator verified.
type Actor struct {
	ID string
}

// Watermark is a project scope's committed high-water sequence and retention
// floor. A position below Floor has expired; a position above HighWater was
// never issued for this stream.
type Watermark struct {
	HighWater int64
	Floor     int64
}

// Record is one retained observation with its scope sequence, exactly as the
// event store committed it.
type Record struct {
	Sequence int64
	Envelope eventsv1.Envelope
	// Size is the stored encoded size, used to bound buffered bytes.
	Size int
}

// Log is the durable read port. PostgreSQL observations, never broker delivery
// or broker sequence, are the authority of public continuation.
type Log interface {
	// Watermark reads the project scope's current watermark.
	Watermark(ctx context.Context, scope Scope) (Watermark, error)
	// Read returns up to limit records with sequence > after, in order, with the
	// watermark observed by the read. A position below the retention floor yields
	// ErrCursorExpired, including when retention advances during the read.
	Read(ctx context.Context, scope Scope, after int64, limit int) ([]Record, Watermark, error)
}

// Resource is the narrow, current-state projection of one observed resource.
// It repeats the event catalog's field spellings (resource type, id, version)
// and adds only the state a client needs to align with the retained sequence;
// the owning module's own routes remain the source of full resources.
type Resource struct {
	Type    Family
	ID      string
	Version int64
	// State is the resource's current lifecycle state.
	State string
	// Kind is the work-item kind; empty for other resources.
	Kind string
	// RunID and StepID identify the owning run and step of a step attempt.
	RunID  string
	StepID string
}

// Snapshot is the complete current state of a stream's projection together with
// the retained position that state is consistent with: every committed event at
// or below Position is reflected in Resources and every later event is not.
type Snapshot struct {
	Position  int64
	Resources []Resource
}

// State is the port to the resource owners' current state.
type State interface {
	// Verify reports ErrNotFound when the project, or the run of a run stream,
	// does not exist inside the scope's tenant and project. It never reveals
	// resources of other scopes.
	Verify(ctx context.Context, scope Scope) error
	// Snapshot reads the projection and the watermark in one atomic read, after
	// verifying the project or run. It returns at most maxResources resources
	// and fails with ErrSnapshotTooLarge rather than truncating. An unavailable
	// resource binding is ErrUnavailable, never an empty success.
	Snapshot(ctx context.Context, scope Scope, maxResources int) (Snapshot, error)
}

// Access answers the current-access question for a family. Implementations
// read current state on every call; no positive answer is cached.
//
// nil allows. ErrHidden means the caller must not learn the target exists.
// ErrDenied means the caller is inside the tenant but not permitted. Any other
// error is an outage and never access.
type Access interface {
	Authorize(ctx context.Context, actor Actor, scope Scope, family Family) error
}

// Delivery is one forwarded observation. Position is the continuation position
// after it: resuming from it continues with the next observation.
type Delivery struct {
	Position int64
	Family   Family
	Envelope eventsv1.Envelope
}

// Sink is the transport side of a stream. Implementations apply their own write
// bounds and report a client that does not read as ErrSlowClient. Calls are made
// from one goroutine, in order.
type Sink interface {
	// Snapshot writes the initial snapshot of a stream that has no resume position.
	Snapshot(ctx context.Context, s Snapshot) error
	// Event writes one observation.
	Event(ctx context.Context, d Delivery) error
	// Checkpoint writes a continuation position that carries no event: everything
	// up to position was decided on and none of it needs to be seen again.
	Checkpoint(ctx context.Context, position int64) error
	// Heartbeat proves liveness while the stream is idle.
	Heartbeat(ctx context.Context) error
	// Flush forces buffered frames to the client.
	Flush(ctx context.Context) error
	// Resync tells the client, best effort and last, that it must resynchronize
	// from a snapshot because cause ended the stream.
	Resync(ctx context.Context, cause error) error
}
