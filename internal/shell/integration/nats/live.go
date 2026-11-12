package nats

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// liveLogBuffer bounds the chunks buffered for one live subscription. A slow
// consumer never grows memory without bound: the handler drops the overflow
// and Next reports ErrLiveOverflow instead of silently losing the gap.
const liveLogBuffer = 64

// ErrLiveOverflow reports that one or more live chunks were dropped because
// the bounded subscription buffer was full. The consumer must fall back to the
// retained log read for the missing range; the loss is never silent.
var ErrLiveOverflow = errors.New("live log subscription overflowed; live chunks were dropped")

// SubscribeLive opens one Core NATS live log subscription for one execution
// inside one explicit scope. The subject derives only from the supplied scope
// and execution, so a subscription never receives another scope's chunks.
//
// The declared log.LiveSubscriber port carries no credential, so opening the
// stream is authorized by the caller before this call. The adapter still fails
// closed on the live-authorization boundary it can observe: it watches the
// scope-qualified invalidation subject and closes the stream as soon as any
// invalidation is observed for that scope. A subscription that observed an
// invalidation never delivers another chunk.
func (broker *Broker) SubscribeLive(ctx context.Context, scope tenancy.Scope, executionID id.ExecutionID) (log.LiveSubscription, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if _, err := id.ParseExecutionID(string(executionID)); err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "live log subscription execution id is invalid", err)
	}
	logSubject, err := LogSubject(scope, executionID)
	if err != nil {
		return nil, err
	}
	invalidationSubject, err := InvalidationSubject(scope)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fault.Wrap(fault.DeadlineExpired, "live log subscription was canceled", err)
	}
	client, err := broker.ensureClient(ctx)
	if err != nil {
		return nil, dependencyError("live log subscription failed", err)
	}

	live := &liveSubscription{
		broker:              broker,
		scope:               scope,
		executionID:         executionID,
		logSubject:          logSubject,
		invalidationSubject: invalidationSubject,
		chunks:              make(chan log.Chunk, liveLogBuffer),
		notify:              make(chan struct{}, 1),
		done:                make(chan struct{}),
	}
	live.idle = sync.NewCond(&live.mu)

	logClient, err := client.Subscribe(ctx, logSubject, live.handleChunk)
	if err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "live log subscription failed", err)
	}
	invalidationClient, err := client.Subscribe(ctx, invalidationSubject, live.handleInvalidation)
	if err != nil {
		_ = logClient.Close(context.Background())
		return nil, fault.Wrap(fault.DependencyUnavailable, "live log subscription failed", err)
	}

	broker.mu.Lock()
	if broker.closed {
		broker.mu.Unlock()
		_ = logClient.Close(context.Background())
		_ = invalidationClient.Close(context.Background())
		return nil, fault.New(fault.DependencyUnavailable, "broker adapter is closed")
	}
	live.logClient = logClient
	live.invalidationClient = invalidationClient
	broker.liveSubscriptions[live] = struct{}{}
	broker.mu.Unlock()
	return live, nil
}

// liveSubscription is one open Core NATS live log subscription. Its bounded
// chunk buffer is filled by the client callback and drained by Next.
type liveSubscription struct {
	broker              *Broker
	scope               tenancy.Scope
	executionID         id.ExecutionID
	logSubject          Subject
	invalidationSubject Subject

	mu                 sync.Mutex
	idle               *sync.Cond
	closed             bool
	overflow           bool
	inFlight           int
	logClient          Subscription
	invalidationClient Subscription
	chunks             chan log.Chunk
	notify             chan struct{}
	done               chan struct{}
}

// Next blocks for the next decoded chunk, an explicit overflow signal, closure
// or context cancellation.
func (live *liveSubscription) Next(ctx context.Context) (log.Chunk, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		if err := ctx.Err(); err != nil {
			return log.Chunk{}, err
		}
		live.mu.Lock()
		if live.closed {
			live.mu.Unlock()
			return log.Chunk{}, liveClosedError()
		}
		if live.overflow {
			live.overflow = false
			live.mu.Unlock()
			return log.Chunk{}, ErrLiveOverflow
		}
		live.mu.Unlock()
		select {
		case chunk := <-live.chunks:
			// Closure can race with a ready chunk: never hand a chunk to the
			// consumer after an observed invalidation closed the stream.
			live.mu.Lock()
			closed := live.closed
			live.mu.Unlock()
			if closed {
				return log.Chunk{}, liveClosedError()
			}
			return chunk, nil
		case <-live.notify:
		case <-ctx.Done():
			return log.Chunk{}, ctx.Err()
		case <-live.done:
			return log.Chunk{}, liveClosedError()
		}
	}
}

// Close releases the subscription, closes both client subscriptions and joins
// any in-flight delivery. It is idempotent.
func (live *liveSubscription) Close(context.Context) error {
	live.shutdown()
	live.wait()
	return nil
}

// handleChunk validates one live payload against the subscription's execution
// and enqueues it in the bounded buffer. It never blocks the client callback.
func (live *liveSubscription) handleChunk(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	live.mu.Lock()
	if live.closed {
		live.mu.Unlock()
		return liveClosedError()
	}
	live.inFlight++
	live.mu.Unlock()
	defer live.finishDelivery()

	var chunk log.Chunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return fault.Wrap(fault.InvalidInput, "live log payload is not JSON", err)
	}
	if err := validateLiveChunk(chunk, live.executionID); err != nil {
		return err
	}

	live.mu.Lock()
	defer live.mu.Unlock()
	if live.closed {
		return liveClosedError()
	}
	select {
	case live.chunks <- chunk:
		return nil
	default:
		// The consumer is not keeping up. Keep the bound, signal the gap and
		// let the consumer fall back to retained logs.
		live.overflow = true
		live.signal()
		return nil
	}
}

// handleInvalidation fails the live stream closed when any invalidation is
// observed for its scope. The port carries no credential to match precisely,
// so the adapter refuses to keep streaming rather than risk delivering after a
// revocation.
func (live *liveSubscription) handleInvalidation(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var invalidation token.Invalidation
	if err := json.Unmarshal(payload, &invalidation); err != nil {
		return fault.Wrap(fault.InvalidInput, "invalidation payload is not JSON", err)
	}
	if err := invalidation.Validate(); err != nil {
		return err
	}
	if invalidation.Scope != live.scope {
		return fault.New(fault.InvalidInput, "invalidation names another scope")
	}
	live.shutdown()
	return nil
}

// reestablish moves one still-open live subscription to a fresh client with the
// same scope-qualified subjects.
func (live *liveSubscription) reestablish(ctx context.Context, client Client) {
	live.mu.Lock()
	closed := live.closed
	live.mu.Unlock()
	if closed {
		return
	}
	logClient, err := client.Subscribe(ctx, live.logSubject, live.handleChunk)
	if err != nil {
		live.shutdown()
		return
	}
	invalidationClient, err := client.Subscribe(ctx, live.invalidationSubject, live.handleInvalidation)
	if err != nil {
		_ = logClient.Close(context.Background())
		live.shutdown()
		return
	}
	live.mu.Lock()
	if live.closed {
		live.mu.Unlock()
		_ = logClient.Close(context.Background())
		_ = invalidationClient.Close(context.Background())
		return
	}
	previousLog := live.logClient
	previousInvalidation := live.invalidationClient
	live.logClient = logClient
	live.invalidationClient = invalidationClient
	live.mu.Unlock()
	if previousLog != nil {
		_ = previousLog.Close(context.Background())
	}
	if previousInvalidation != nil {
		_ = previousInvalidation.Close(context.Background())
	}
}

// shutdown closes both client subscriptions and marks the subscription closed.
// It is idempotent and never waits.
func (live *liveSubscription) shutdown() {
	live.mu.Lock()
	if live.closed {
		live.mu.Unlock()
		return
	}
	live.closed = true
	logClient := live.logClient
	invalidationClient := live.invalidationClient
	live.logClient = nil
	live.invalidationClient = nil
	close(live.done)
	live.mu.Unlock()

	live.broker.unregisterLive(live)
	if logClient != nil {
		_ = logClient.Close(context.Background())
	}
	if invalidationClient != nil {
		_ = invalidationClient.Close(context.Background())
	}
}

// wait joins any in-flight delivery of the subscription.
func (live *liveSubscription) wait() {
	<-live.done
	live.mu.Lock()
	for live.inFlight > 0 {
		live.idle.Wait()
	}
	live.mu.Unlock()
}

// finishDelivery records that one callback returned.
func (live *liveSubscription) finishDelivery() {
	live.mu.Lock()
	live.inFlight--
	if live.inFlight == 0 {
		live.idle.Broadcast()
	}
	live.mu.Unlock()
}

// signal wakes one blocked Next without blocking the caller.
func (live *liveSubscription) signal() {
	select {
	case live.notify <- struct{}{}:
	default:
	}
}

// validateLiveChunk refuses a malformed or foreign-execution live chunk.
func validateLiveChunk(chunk log.Chunk, executionID id.ExecutionID) error {
	if chunk.ExecutionID != executionID {
		return fault.New(fault.InvalidInput, "live log chunk names another execution")
	}
	if _, err := id.ParseActionID(string(chunk.ActionID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "live log chunk action id is invalid", err)
	}
	if !chunk.Stream.Valid() {
		return fault.New(fault.InvalidInput, "live log chunk stream is unknown")
	}
	if err := log.DefaultBounds.CheckChunk(len(chunk.Data)); err != nil {
		return err
	}
	return nil
}

// liveClosedError reports that the live subscription is closed.
func liveClosedError() error {
	return fault.New(fault.DependencyUnavailable, "live log subscription is closed")
}
