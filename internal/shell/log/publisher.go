// Bounded asynchronous live log publisher.
//
// PublishLive only validates, redacts and enqueues one bounded job; it never
// waits for the downstream publisher. The queue is bounded, so downstream
// backpressure drops live progress instead of blocking workload observation.
// Close stops accepting, closes the queue, joins the worker and returns only
// after no further publication is in flight.
package log

import (
	"context"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// liveJob is one queued live publication. The chunk bytes are already redacted
// and copied.
type liveJob struct {
	ctx   context.Context
	scope tenancy.Scope
	chunk Chunk
}

// AsyncPublisher publishes live chunks through a bounded queue and one joined
// worker. It is safe for concurrent use.
type AsyncPublisher struct {
	downstream LivePublisher
	limits     Limits
	redactor   redactor

	mu      sync.Mutex
	closed  bool
	jobs    chan liveJob
	done    chan struct{}
	pending map[string][]byte
}

// NewAsyncPublisher validates the publisher configuration and starts its one
// worker.
func NewAsyncPublisher(config PublisherConfig) (*AsyncPublisher, error) {
	if config.Downstream == nil {
		return nil, fault.New(fault.InvalidInput, "live log publisher requires a downstream publisher")
	}
	limits, err := resolveLimits(config.Limits)
	if err != nil {
		return nil, err
	}
	redactor, err := newRedactor(config.Redact)
	if err != nil {
		return nil, err
	}
	publisher := &AsyncPublisher{
		downstream: config.Downstream,
		limits:     limits,
		redactor:   redactor,
		jobs:       make(chan liveJob, limits.MaxQueuedChunks),
		done:       make(chan struct{}),
		pending:    make(map[string][]byte),
	}
	go publisher.run()
	return publisher, nil
}

// PublishLive enqueues one live chunk without waiting for Downstream and returns
// nil even when Downstream is slow or fails. When the bounded queue is full the
// chunk is dropped rather than blocking the caller.
func (publisher *AsyncPublisher) PublishLive(ctx context.Context, scope tenancy.Scope, chunk Chunk) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateScope(scope); err != nil {
		return err
	}
	if err := validateChunk(chunk, publisher.limits.MaxChunkBytes); err != nil {
		return err
	}
	// Redaction and enqueue happen under one lock in caller order: a value split
	// across two live chunks is never published independently, because the
	// possible secret prefix is held back for the next chunk of the same stream.
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.closed {
		return fault.New(fault.InvalidInput, "live log publisher is closed")
	}
	data := chunk.Data
	if publisher.redactor.active() {
		key := streamKey(scope, chunk)
		stored, pending := publisher.redactor.stream(publisher.pending[key], chunk.Data)
		// Only a stream that currently holds a possible-secret suffix needs an
		// entry, so ordinary streams do not accumulate state for their whole
		// lifetime.
		if len(pending) == 0 {
			delete(publisher.pending, key)
		} else {
			publisher.pending[key] = pending
		}
		data = stored
	}
	chunk.Data = append([]byte(nil), data...)

	select {
	case publisher.jobs <- liveJob{ctx: ctx, scope: scope, chunk: chunk}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

// Close stops accepting, joins the worker and returns after no further
// publication is in flight. A repeated Close is a no-op that still waits for
// the joined worker.
func (publisher *AsyncPublisher) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	publisher.mu.Lock()
	if !publisher.closed {
		publisher.closed = true
		// A held suffix is only a possible secret prefix and is never published
		// on its own. Dropping it on close neither hangs the join nor leaks it.
		publisher.pending = nil
		close(publisher.jobs)
	}
	done := publisher.done
	publisher.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (publisher *AsyncPublisher) run() {
	defer close(publisher.done)
	for job := range publisher.jobs {
		// A downstream failure is live-delivery loss, not workload failure: the
		// worker continues with the next chunk. Retained logs remain the source.
		_ = publisher.downstream.PublishLive(job.ctx, job.scope, job.chunk)
	}
}
