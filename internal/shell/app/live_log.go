package app

// Live log publication while an action is still running.
//
// Retained collection alone is too late for a live subscription: the runtime's
// output is only made durable at the pre-stop checkpoint and at Remove, so
// a subscriber observes nothing while the workload is producing output. This
// file adds a bounded, best-effort poller that reads the same runtime log seam
// with its own continuation cursor and publishes each bounded piece through the
// composition's live publisher.
//
// The live pump is never the retained source:
//
//   - it never appends to the retained sink and never touches the durable
//     collection cursor;
//   - it reads at most logLivePagesPerTick bounded pages per poll and sleeps
//     between polls, so a long-running workload cannot turn into a busy loop;
//   - the live publisher is the bounded asynchronous adapter, so downstream
//     backpressure drops live progress instead of blocking observation;
//   - every published chunk carries the execution/action identity, stream,
//     monotonic offset and sequence it already had in the runtime batch, and
//     the scope comes only from the execution row, never from the chunk;
//   - a scope that cannot be resolved publishes nothing, and an invalid scope
//     is never widened or guessed;
//   - an empty read or a read failure backs the poll off instead of retrying at
//     the fixed cadence, so an idle workload or a persistently failing runtime
//     is not hammered;
//   - a read, split or publish failure is live loss only. Retained collection
//     at Stop/Remove continues to provide the complete read-back fallback.
//
// The pump is cancelled before the pre-stop retention checkpoint and before
// Remove, so the durable cursor and the live stream do not chase each other.
// A composition Close cancels every remaining pump.
//
// Live offsets are not strictly monotonic across a lifecycle boundary. The
// pre-stop checkpoint (Stop) and the final drain (Remove) read output the live
// pump may already have published, and they offer those pieces to the live
// publisher again. A consumer therefore sees the same (execution, action,
// stream, offset) piece more than when the runtime's cursor moved once: it must
// deduplicate by that chunk identity exactly as the retained sink does, and it
// must not treat a repeated or lower offset at a checkpoint as a new chunk.

import (
	"context"
	"time"

	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	// defaultLiveLogInterval is the live poll cadence when output is flowing.
	// It is deliberately much shorter than a typical action but never a timer
	// spin.
	defaultLiveLogInterval = 200 * time.Millisecond
	// logLivePagesPerTick bounds one poll so a large backlog is published over
	// several ticks instead of one unbounded burst.
	logLivePagesPerTick = 8
	// logLiveBackoffCeiling bounds the poll delay after empty reads or read
	// failures. The delay doubles from the configured cadence up to this bound
	// and resets as soon as a poll makes progress, so an idle workload or a
	// struggling runtime is never retried at the busy cadence indefinitely.
	logLiveBackoffCeiling = 2 * time.Second
	// logLiveStopWait bounds how long Stop/Remove/Close waits for a cancelled
	// pump. A runtime read that ignores cancellation cannot postpone the
	// destructive boundary beyond this bound. It also bounds the leak: if the
	// read never returns, the join gives up after this wait and that one
	// goroutine stays parked in the read until it returns. Its context is
	// already cancelled, so it publishes nothing afterwards and Close never
	// waits for it again; the goroutine is a bounded best-effort concession,
	// not an owned resource that can outlive the process.
	logLiveStopWait = 2 * time.Second
)

// liveLogPump is one cancelled-and-joined live reader.
type liveLogPump struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startLiveLogs opens one live pump for the started effect identity. A pump for
// the same identity is replaced, so a retried start never leaves two readers
// publishing the same stream.
func (runtime *logCollectingRuntime) startLiveLogs(identity kernelengine.EffectIdentity) {
	if runtime.live == nil {
		return
	}
	key := liveLogKey(identity)
	runtime.liveMu.Lock()
	if runtime.liveClosed {
		runtime.liveMu.Unlock()
		return
	}
	if previous := runtime.livePumps[key]; previous != nil {
		previous.cancel()
		delete(runtime.livePumps, key)
	}
	ctx, cancel := context.WithCancel(context.Background())
	pump := &liveLogPump{cancel: cancel, done: make(chan struct{})}
	runtime.livePumps[key] = pump
	runtime.liveMu.Unlock()
	go runtime.pumpLiveLogs(ctx, pump, identity)
}

// stopLiveLogs cancels and joins the live pump of one effect identity. It is
// best-effort and never converts a live-delivery concern into a lifecycle
// fault.
func (runtime *logCollectingRuntime) stopLiveLogs(ctx context.Context, identity kernelengine.EffectIdentity) {
	key := liveLogKey(identity)
	runtime.liveMu.Lock()
	pump := runtime.livePumps[key]
	delete(runtime.livePumps, key)
	runtime.liveMu.Unlock()
	joinLivePumps(ctx, []*liveLogPump{pump})
}

// Close cancels and joins every live pump. The composition registers it so no
// pump can outlive the runtime it reads. It is idempotent.
func (runtime *logCollectingRuntime) Close(ctx context.Context) error {
	runtime.liveMu.Lock()
	runtime.liveClosed = true
	pumps := make([]*liveLogPump, 0, len(runtime.livePumps))
	for _, pump := range runtime.livePumps {
		pumps = append(pumps, pump)
	}
	runtime.livePumps = map[string]*liveLogPump{}
	runtime.liveMu.Unlock()
	joinLivePumps(ctx, pumps)
	return nil
}

// joinLivePumps cancels and waits for the supplied pumps under the caller's
// context and the fixed stop bound. A pump that is already stopped is skipped.
//
// The join is bounded, not unconditional: a pump parked in a runtime read that
// ignores cancellation can outlive the bound here. The join then returns and
// the caller proceeds, but the pump's context is already cancelled, so it
// publishes nothing after cancellation. That one parked goroutine returns when
// its read does; it is never joined again. This is the accepted worst case for
// a runtime port that violates cancellation, and it cannot block Stop, Remove
// or Close beyond logLiveStopWait.
func joinLivePumps(ctx context.Context, pumps []*liveLogPump) {
	if ctx == nil {
		ctx = context.Background()
	}
	for _, pump := range pumps {
		if pump == nil {
			continue
		}
		pump.cancel()
	}
	deadline := time.Now().Add(logLiveStopWait)
	for _, pump := range pumps {
		if pump == nil {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-pump.done:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			return
		}
	}
}

// pumpLiveLogs resolves the execution scope, then polls the runtime's bounded
// log seam and publishes every new bounded piece until cancelled. An empty read
// or a read failure grows the poll delay; progress resets it to the configured
// cadence.
func (runtime *logCollectingRuntime) pumpLiveLogs(ctx context.Context, pump *liveLogPump, identity kernelengine.EffectIdentity) {
	defer close(pump.done)
	var (
		scope    tenancy.Scope
		resolved bool
		cursor   int64
	)
	delay := runtime.liveInterval
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if !resolved {
			candidate, err := runtime.scopes.ExecutionScope(ctx, identity.ExecutionID)
			if err == nil && shelllog.ValidateScope(candidate) == nil {
				scope = candidate
				resolved = true
			} else {
				if !runtime.sleepLive(ctx, delay) {
					return
				}
				delay = nextLiveBackoff(delay, runtime.liveInterval)
				continue
			}
		}
		progressed := false
		for range logLivePagesPerTick {
			if err := ctx.Err(); err != nil {
				return
			}
			batch, err := runtime.inner.Logs(ctx, kernelengine.LogsRequest{
				Identity:    identity,
				AfterOffset: cursor,
				Limit:       runtime.pageBytes,
			})
			if err != nil {
				// A live read fault is live loss only. The retained path retries
				// the same source at the checkpoint and at Remove.
				break
			}
			if len(batch.Chunks) == 0 {
				if batch.NextOffset > cursor {
					cursor = batch.NextOffset
				}
				break
			}
			if batch.NextOffset <= cursor {
				// A non-advancing page is ignored rather than re-published in a
				// tight loop; the retained path still reports it as a fault.
				break
			}
			for _, chunk := range batch.Chunks {
				pieces, err := splitLogChunk(identity, chunk, runtime.pageBytes)
				if err != nil {
					continue
				}
				for _, piece := range pieces {
					_ = runtime.live.PublishLive(ctx, scope, piece)
				}
			}
			cursor = batch.NextOffset
			progressed = true
		}
		if progressed {
			delay = runtime.liveInterval
		} else {
			delay = nextLiveBackoff(delay, runtime.liveInterval)
		}
		if !runtime.sleepLive(ctx, delay) {
			return
		}
	}
}

// nextLiveBackoff doubles one live poll delay up to the backoff ceiling and
// keeps it at least at the configured cadence.
func nextLiveBackoff(delay, floor time.Duration) time.Duration {
	if delay < floor {
		delay = floor
	}
	next := delay * 2
	if next < delay || next > logLiveBackoffCeiling {
		next = logLiveBackoffCeiling
	}
	if next < floor {
		next = floor
	}
	return next
}

// sleepLive waits one live poll delay and reports whether the pump should keep
// running.
func (runtime *logCollectingRuntime) sleepLive(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// liveLogKey scopes one pump to the full effect identity, including the
// workload, exactly like the durable collection cursor.
func liveLogKey(identity kernelengine.EffectIdentity) string {
	return string(identity.ExecutionID) + "\x00" + string(identity.ActionID) + "\x00" + string(identity.WorkloadID)
}
