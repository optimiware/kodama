// Package log declares the retained and live log delivery seams.
//
// Retained logs and live progress are separate contracts. A retained sink
// appends bounded chunks and serves bounded scoped reads; a live publisher
// carries progress to subscribers without ever becoming the retained source.
// Broker failure therefore cannot block workload observation or progression,
// and a retained read never depends on a live subscription.
//
// Every append, read, publication and subscription carries the ownership scope
// explicitly. A chunk alone cannot widen the scope: the adapter derives the
// scoped subject or row key from the supplied tenancy.Scope, never from the
// execution/action identity embedded in the chunk.
package log

import (
	"context"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// Retention is the declared retained-log policy: bounded chunks kept for 30
// days. Ordinary evidence has a different retention period; this value only
// pins the log stream.
type Retention struct {
	MaxChunkBytes int
	RetainFor     time.Duration
}

// DefaultRetention returns the declared retained-log policy. The chunk bound is
// the same bound every producer and retained sink uses.
func DefaultRetention() Retention {
	return Retention{
		MaxChunkBytes: DefaultBounds.MaxChunkBytes,
		RetainFor:     30 * 24 * time.Hour,
	}
}

// AppendResult is the evidence of one retained append. Offset and Sequence
// identify the stored chunk so a retry can be deduplicated.
type AppendResult struct {
	Offset   uint64
	Sequence uint64
}

// ValidateScope checks the ownership scope supplied to one log operation. An
// empty or malformed scope is refused; there is no scope-less log path.
func ValidateScope(scope tenancy.Scope) error {
	return scope.Validate()
}

// ReadRequest is one bounded scoped retained read. Scope names the tenant and
// optional project whose retained chunks may be read. AfterOffset continues a
// previous read; Limit bounds the number of chunks returned.
type ReadRequest struct {
	Scope       tenancy.Scope
	ExecutionID id.ExecutionID
	ActionID    id.ActionID
	Stream      Stream
	AfterOffset uint64
	Limit       int
}

// Validate checks the scope, execution/action identity, stream and bounded
// limit of one retained read. A scope-less or malformed request is refused
// before the adapter touches storage.
func (request ReadRequest) Validate() error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if _, err := id.ParseExecutionID(string(request.ExecutionID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "retained read execution id is invalid", err)
	}
	if _, err := id.ParseActionID(string(request.ActionID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "retained read action id is invalid", err)
	}
	if !request.Stream.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("retained read stream %q is unknown", request.Stream))
	}
	if request.Limit <= 0 {
		return fault.New(fault.InvalidInput, "retained read limit is not positive")
	}
	return nil
}

// ReadResult is one bounded retained read. NextOffset continues the read;
// Truncated reports that the requested range exceeded the retained policy.
type ReadResult struct {
	Chunks     []Chunk
	NextOffset uint64
	Truncated  bool
}

// RetainedSink appends and reads bounded retained log chunks in one explicit
// ownership scope. It deliberately has no live-publication method: live
// delivery is a separate seam.
type RetainedSink interface {
	Append(context.Context, tenancy.Scope, Chunk) (AppendResult, error)
	Read(context.Context, ReadRequest) (ReadResult, error)
}

// LivePublisher publishes one live chunk in one explicit ownership scope. It
// has no retained append method and is never the retained source. The adapter
// derives the scoped live subject from the supplied scope only.
type LivePublisher interface {
	PublishLive(context.Context, tenancy.Scope, Chunk) error
}

// LiveSubscription is one open live log subscription. Next blocks for the next
// chunk; Close releases it.
type LiveSubscription interface {
	Next(context.Context) (Chunk, error)
	Close(context.Context) error
}

// LiveSubscriber opens a live subscription for one execution inside one
// explicit ownership scope. A subscription for one scope never receives
// another scope's chunks; the adapter derives the scoped subject from the
// supplied scope only.
type LiveSubscriber interface {
	SubscribeLive(context.Context, tenancy.Scope, id.ExecutionID) (LiveSubscription, error)
}
