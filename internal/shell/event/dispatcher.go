package event

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
)

// Sequence is the durable ordering cursor of the event table.
type Sequence uint64

// PendingEvent is one committed, not-yet-published event row.
type PendingEvent struct {
	Sequence Sequence `json:"sequence"`
	Event    Envelope `json:"event"`
}

// Outbox reads already-committed, not-yet-published event rows and
// acknowledges them. Pending returns at most limit committed unacknowledged
// rows in durable sequence order. MarkPublished records that one row was
// delivered by the broker; it is called only after Publish succeeds.
//
// A row's durable sequence is assigned when the event row is inserted, while the
// row becomes readable only when its transaction commits, so commit-visibility
// order can differ from sequence order. Pending must therefore return every
// committed unacknowledged row regardless of how far publication has progressed:
// a row that commits late with a lower sequence than an already-acknowledged row
// is still pending and must never be skipped. The after argument is an exclusive
// lower-bound cursor for bounded pagination only; it must never be carried across
// passes as a publication high-water mark. Durable sequences are positive, so the
// zero cursor means "from the beginning".
//
// The event table is truth: an envelope that was never committed can never be
// returned here, and an unacknowledged row is never skipped.
type Outbox interface {
	Pending(context.Context, Sequence, int) ([]PendingEvent, error)
	MarkPublished(context.Context, id.EventID, Sequence) error
}

// DispatcherConfig is the complete publication-worker composition: the durable
// event-table read/acknowledgement seam and the broker publisher. It has no
// domain store, so a broker failure can never roll back domain progress.
type DispatcherConfig struct {
	Outbox    Outbox
	Publisher Publisher
	BatchSize int
	Idle      func(context.Context) error
}

// DispatchResult reports one bounded publication pass. Published lists only
// fully settled rows in sequence order. Failed names the row that stopped the
// pass, if any; it is retained and never skipped. More reports that another
// pass may have work (a full batch or a failed pass).
type DispatchResult struct {
	Published []id.EventID
	Failed    *id.EventID
	More      bool
}

// Dispatcher drives the durable outbox into the broker. It holds only the
// read/acknowledgement seam and the publisher; it can never create publication
// intent or touch domain state. It keeps no publication high-water mark:
// acknowledgement, not an in-memory cursor, is what removes a settled row.
type Dispatcher struct {
	outbox    Outbox
	publisher Publisher
	batchSize int
	idle      func(context.Context) error

	// mu serializes passes so two concurrent Dispatch calls can never publish
	// and acknowledge the same rows twice.
	mu sync.Mutex
}

// NewDispatcher validates the composition and returns a dispatcher.
func NewDispatcher(config DispatcherConfig) (*Dispatcher, error) {
	switch {
	case config.Outbox == nil:
		return nil, fault.New(fault.InvalidInput, "dispatcher requires an outbox")
	case config.Publisher == nil:
		return nil, fault.New(fault.InvalidInput, "dispatcher requires a publisher")
	case config.BatchSize <= 0:
		return nil, fault.New(fault.InvalidInput, fmt.Sprintf("dispatcher batch size %d is not positive", config.BatchSize))
	}
	return &Dispatcher{
		outbox:    config.Outbox,
		publisher: config.Publisher,
		batchSize: config.BatchSize,
		idle:      config.Idle,
	}, nil
}

// Dispatch performs one bounded pass.
//
// It reads at most BatchSize committed unacknowledged rows from the start of the
// outbox, publishes them in sequence order and acknowledges each row with its own
// identity and sequence only after Publish succeeds. It keeps no cursor between
// passes: the durable sequence is assigned at insert, so a transaction that
// commits late can expose a lower sequence after a higher one was already
// acknowledged. Querying from the start every pass, and letting MarkPublished
// remove settled rows, means such a row is still published instead of being
// skipped forever. A publish or acknowledgement failure stops the pass at that
// row, reports it in Failed and leaves More true; the row is not acknowledged or
// skipped and the next pass re-publishes the same event ID (at-least-once). A
// durable-read failure is returned as an error instead of being treated as an
// empty backlog.
func (dispatcher *Dispatcher) Dispatch(ctx context.Context) (DispatchResult, error) {
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return DispatchResult{}, err
	}

	// The zero cursor means "from the beginning". A sequence-based high-water
	// mark would skip a row whose transaction commits after a higher sequence
	// was acknowledged; MarkPublished already excludes acknowledged rows.
	rows, err := dispatcher.outbox.Pending(ctx, 0, dispatcher.batchSize)
	if err != nil {
		return DispatchResult{}, err
	}
	sort.SliceStable(rows, func(left, right int) bool {
		return rows[left].Sequence < rows[right].Sequence
	})
	// The seam promises at most BatchSize rows; bound the pass even if an
	// adapter returns a larger backlog, so one pass can never run unbounded.
	if len(rows) > dispatcher.batchSize {
		rows = rows[:dispatcher.batchSize]
	}

	result := DispatchResult{Published: make([]id.EventID, 0, len(rows))}
	for _, row := range rows {
		if err := dispatcher.publisher.Publish(ctx, row.Event); err != nil {
			failed := row.Event.ID
			result.Failed = &failed
			result.More = true
			return result, nil
		}
		if err := dispatcher.outbox.MarkPublished(ctx, row.Event.ID, row.Sequence); err != nil {
			failed := row.Event.ID
			result.Failed = &failed
			result.More = true
			return result, nil
		}
		result.Published = append(result.Published, row.Event.ID)
	}
	result.More = len(rows) >= dispatcher.batchSize
	return result, nil
}

// Run repeats Dispatch until ctx is cancelled and then returns ctx.Err().
//
// Broker and durable-read failures are tolerated: a failed row pauses one pass
// and is retried with the same identity on the next one. Idle is called between
// passes when supplied, so an outage or an idle outbox does not spin. Run spawns
// no goroutine, so it joins on cancellation without leaking one.
func (dispatcher *Dispatcher) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		result, dispatchErr := dispatcher.Dispatch(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}

		if dispatcher.idle != nil {
			_ = dispatcher.idle(ctx)
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}

		// A failed read or a retained row is retried immediately; a composed
		// worker supplies Idle to pace the retry. Without an injected poll
		// cadence there is no interval to wait on once the outbox is drained,
		// so block until cancellation instead of spinning.
		if dispatchErr != nil || result.More || result.Failed != nil {
			continue
		}
		<-ctx.Done()
		return ctx.Err()
	}
}
