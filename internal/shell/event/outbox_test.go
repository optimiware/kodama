package event_test

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
)

// TestDispatcherPublishesCommittedRowsInSequenceOrder asserts one bounded pass
// publishes every committed row in durable sequence order and acknowledges each
// row with its own identity.
func TestDispatcherPublishesCommittedRowsInSequenceOrder(t *testing.T) {
	outbox := newFakeOutbox("event-1", "event-2", "event-3")
	publisher := newFakePublisher()
	dispatcher := newDispatcher(t, outbox, publisher, 10, nil)

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	wantIDs := []id.EventID{"event-1", "event-2", "event-3"}
	if got := publisher.deliveredIDs(); !reflect.DeepEqual(got, wantIDs) {
		t.Errorf("published = %v, want %v in sequence order", got, wantIDs)
	}
	if got := outbox.markedSequences(); !reflect.DeepEqual(got, []event.Sequence{1, 2, 3}) {
		t.Errorf("marked sequences = %v, want [1 2 3]", got)
	}
	if !reflect.DeepEqual(result.Published, wantIDs) {
		t.Errorf("result published = %v, want %v", result.Published, wantIDs)
	}
	if result.Failed != nil {
		t.Errorf("failed = %q, want none", *result.Failed)
	}
	if result.More {
		t.Error("More = true after draining a partial batch")
	}
	if got := outbox.queriedAfter(); !reflect.DeepEqual(got, []event.Sequence{0}) {
		t.Errorf("pending queries = %v, want one query after the zero cursor", got)
	}
	if got := outbox.lastLimit(); got != 10 {
		t.Errorf("pending limit = %d, want the configured batch 10", got)
	}

	result, err = dispatcher.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}
	if len(result.Published) != 0 || result.More {
		t.Errorf("second pass published %v (More=%v), want nothing", result.Published, result.More)
	}
	if got := len(publisher.delivered()); got != 3 {
		t.Errorf("deliveries = %d, want 3: a drained outbox must not republish", got)
	}
}

// TestDispatcherBoundsBatchAndDrainsRemainder asserts the batch bound leaves the
// remainder for later passes instead of reading an unbounded backlog.
func TestDispatcherBoundsBatchAndDrainsRemainder(t *testing.T) {
	outbox := newFakeOutbox("event-1", "event-2", "event-3", "event-4", "event-5")
	publisher := newFakePublisher()
	dispatcher := newDispatcher(t, outbox, publisher, 2, nil)

	passes := 0
	var more []bool
	for {
		result, err := dispatcher.Dispatch(context.Background())
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		passes++
		more = append(more, result.More)
		if !result.More {
			break
		}
		if passes > 10 {
			t.Fatal("dispatcher never drained the bounded backlog")
		}
	}
	if passes != 3 {
		t.Errorf("passes = %d, want 3 bounded passes", passes)
	}
	if !reflect.DeepEqual(more, []bool{true, true, false}) {
		t.Errorf("More per pass = %v, want [true true false]", more)
	}
	wantIDs := []id.EventID{"event-1", "event-2", "event-3", "event-4", "event-5"}
	if got := publisher.deliveredIDs(); !reflect.DeepEqual(got, wantIDs) {
		t.Errorf("published = %v, want %v", got, wantIDs)
	}
}

// TestDispatcherPublishesLateVisibleLowerSequenceRow asserts the dispatcher
// keeps no in-memory high-water mark. A durable sequence is assigned at insert,
// not at commit, so a transaction can reserve sequence 5, lose the race to
// sequence 6 and only become committed afterwards. Row 5 must still be returned
// and published on a later pass instead of being skipped forever.
func TestDispatcherPublishesLateVisibleLowerSequenceRow(t *testing.T) {
	outbox := newFakeOutbox()
	outbox.addRowAt(5, "event-5", false) // sequence reserved, transaction uncommitted
	outbox.addRowAt(6, "event-6", true)  // committed first
	publisher := newFakePublisher()
	dispatcher := newDispatcher(t, outbox, publisher, 10, nil)

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !reflect.DeepEqual(result.Published, []id.EventID{"event-6"}) {
		t.Errorf("first pass published = %v, want only the committed event-6", result.Published)
	}

	// The transaction that reserved sequence 5 commits after row 6 was published
	// and acknowledged.
	outbox.reveal(5)

	result, err = dispatcher.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}
	if !reflect.DeepEqual(result.Published, []id.EventID{"event-5"}) {
		t.Errorf("second pass published = %v, want the late-visible event-5", result.Published)
	}
	if got := publisher.deliveredIDs(); !reflect.DeepEqual(got, []id.EventID{"event-6", "event-5"}) {
		t.Errorf("delivered = %v, want event-6 then the late event-5 exactly once", got)
	}
	if got := outbox.markedSequences(); !reflect.DeepEqual(got, []event.Sequence{6, 5}) {
		t.Errorf("marked sequences = %v, want [6 5]: acknowledgement is what removes a row", got)
	}
	if got := outbox.queriedAfter(); !reflect.DeepEqual(got, []event.Sequence{0, 0}) {
		t.Errorf("pending queries = %v, want the zero cursor on every pass", got)
	}

	// The acknowledged late row must not be republished.
	result, err = dispatcher.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("third Dispatch: %v", err)
	}
	if len(result.Published) != 0 {
		t.Errorf("third pass published = %v, want nothing", result.Published)
	}
	if got := len(publisher.delivered()); got != 2 {
		t.Errorf("deliveries = %d, want 2: the late row is published once per acknowledged attempt", got)
	}
}

// TestDispatcherRetainsFailedRowAndDoesNotSkip asserts a broker failure stops
// the pass at the failed row, never acknowledges or skips it, and the retry
// republishes the same event identity.
func TestDispatcherRetainsFailedRowAndDoesNotSkip(t *testing.T) {
	outbox := newFakeOutbox("event-1", "event-2", "event-3")
	publisher := newFakePublisher()
	publisher.failOnce("event-2")
	dispatcher := newDispatcher(t, outbox, publisher, 10, nil)

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !reflect.DeepEqual(result.Published, []id.EventID{"event-1"}) {
		t.Errorf("published = %v, want only the settled event-1", result.Published)
	}
	if result.Failed == nil || *result.Failed != id.EventID("event-2") {
		t.Errorf("failed = %v, want event-2", result.Failed)
	}
	if !result.More {
		t.Error("More = false after a failed pass")
	}
	if got := outbox.markedSequences(); !reflect.DeepEqual(got, []event.Sequence{1}) {
		t.Errorf("marked sequences = %v, want only [1]: a failed row must not be acknowledged or skipped", got)
	}
	if got := publisher.deliveredIDs(); !reflect.DeepEqual(got, []id.EventID{"event-1"}) {
		t.Errorf("delivered = %v, want event-3 withheld behind the failed row", got)
	}

	result, err = dispatcher.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("retry Dispatch: %v", err)
	}
	if !reflect.DeepEqual(result.Published, []id.EventID{"event-2", "event-3"}) {
		t.Errorf("retry published = %v, want event-2 then event-3", result.Published)
	}
	if result.More || result.Failed != nil {
		t.Errorf("retry result = %#v, want a complete pass", result)
	}
	attempts := publisher.attempted()
	if len(attempts) != 4 {
		t.Fatalf("publish attempts = %d, want 4", len(attempts))
	}
	if attempts[1].ID != id.EventID("event-2") || attempts[2].ID != id.EventID("event-2") {
		t.Errorf("retried ids = %q/%q, want the same event-2", attempts[1].ID, attempts[2].ID)
	}
	if !reflect.DeepEqual(attempts[1], attempts[2]) {
		t.Errorf("retry changed the envelope:\n failed %#v\n retry  %#v", attempts[1], attempts[2])
	}
	if got := outbox.markedSequences(); !reflect.DeepEqual(got, []event.Sequence{1, 2, 3}) {
		t.Errorf("marked sequences = %v, want [1 2 3]", got)
	}
}

// TestDispatcherRepublishesSameIDAfterLostAcknowledgement asserts an
// acknowledgement failure after a successful publish re-publishes the identical
// event on the next pass (at-least-once delivery).
func TestDispatcherRepublishesSameIDAfterLostAcknowledgement(t *testing.T) {
	outbox := newFakeOutbox("event-1")
	outbox.markFailures[1] = 1
	publisher := newFakePublisher()
	dispatcher := newDispatcher(t, outbox, publisher, 10, nil)

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(result.Published) != 0 {
		t.Errorf("published = %v, want no fully settled row after an acknowledgement failure", result.Published)
	}
	if result.Failed == nil || *result.Failed != id.EventID("event-1") {
		t.Errorf("failed = %v, want event-1", result.Failed)
	}
	if !result.More {
		t.Error("More = false after an acknowledgement failure")
	}
	if got := outbox.markedSequences(); len(got) != 0 {
		t.Errorf("marked sequences = %v, want none", got)
	}

	result, err = dispatcher.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("retry Dispatch: %v", err)
	}
	if !reflect.DeepEqual(result.Published, []id.EventID{"event-1"}) {
		t.Errorf("retry published = %v, want event-1", result.Published)
	}
	deliveries := publisher.delivered()
	if len(deliveries) != 2 {
		t.Fatalf("deliveries = %d, want 2: at-least-once duplicates are tolerated", len(deliveries))
	}
	if !reflect.DeepEqual(deliveries[0], deliveries[1]) {
		t.Errorf("duplicate delivery changed the envelope:\n first  %#v\n second %#v", deliveries[0], deliveries[1])
	}
	if got := outbox.markedSequences(); !reflect.DeepEqual(got, []event.Sequence{1}) {
		t.Errorf("marked sequences = %v, want [1]", got)
	}
}

// TestDispatcherNeverPublishesWithoutAPendingRow asserts the event table is the
// only source of publication: an empty outbox publishes nothing.
func TestDispatcherNeverPublishesWithoutAPendingRow(t *testing.T) {
	outbox := newFakeOutbox()
	publisher := newFakePublisher()
	dispatcher := newDispatcher(t, outbox, publisher, 10, nil)

	result, err := dispatcher.Dispatch(context.Background())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(result.Published) != 0 || result.Failed != nil || result.More {
		t.Errorf("empty outbox result = %#v, want an empty complete pass", result)
	}
	if got := len(publisher.attempted()); got != 0 {
		t.Errorf("publish attempts = %d, want 0 without a committed row", got)
	}
	if got := outbox.markedSequences(); len(got) != 0 {
		t.Errorf("marked sequences = %v, want none", got)
	}
}

// TestDispatcherPublishesOnlyRowsReturnedByTheOutbox asserts the dispatcher can
// only ever publish rows the durable outbox returned, so publication before
// commit is impossible.
func TestDispatcherPublishesOnlyRowsReturnedByTheOutbox(t *testing.T) {
	outbox := newFakeOutbox("event-1", "event-2")
	publisher := newFakePublisher()
	dispatcher := newDispatcher(t, outbox, publisher, 10, nil)

	if _, err := dispatcher.Dispatch(context.Background()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	committed := map[id.EventID]bool{}
	for _, eventID := range outbox.committedIDs() {
		committed[eventID] = true
	}
	for _, eventID := range publisher.deliveredIDs() {
		if !committed[eventID] {
			t.Errorf("published %q, which the committed outbox never returned", eventID)
		}
	}
}

// TestDispatcherReturnsOutboxReadError asserts a durable-read failure is
// reported instead of being treated as an empty backlog.
func TestDispatcherReturnsOutboxReadError(t *testing.T) {
	outbox := newFakeOutbox("event-1")
	outbox.pendingErr = fault.New(fault.DependencyUnavailable, "event table unavailable")
	outbox.pendingErrCalls = 1
	publisher := newFakePublisher()
	dispatcher := newDispatcher(t, outbox, publisher, 10, nil)

	_, err := dispatcher.Dispatch(context.Background())
	assertFaultClass(t, err, fault.DependencyUnavailable)
	if got := len(publisher.attempted()); got != 0 {
		t.Errorf("publish attempts = %d, want 0 when the event table cannot be read", got)
	}
}

// TestDispatcherRunJoinsOnCancellation asserts the publication loop returns
// after cancellation and leaves no goroutine behind.
func TestDispatcherRunJoinsOnCancellation(t *testing.T) {
	outbox := newFakeOutbox()
	publisher := newFakePublisher()
	reached := make(chan struct{}, 1)
	idle := func(ctx context.Context) error {
		select {
		case reached <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	dispatcher := newDispatcher(t, outbox, publisher, 10, idle)

	ctx, cancel := context.WithCancel(context.Background())
	baseline := runtime.NumGoroutine()
	runErr := make(chan error, 1)
	go func() {
		runErr <- dispatcher.Run(ctx)
	}()

	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("dispatcher never reached its idle wait")
	}
	cancel()

	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled after cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+1 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d, want at most %d: the publication loop leaked", runtime.NumGoroutine(), baseline+1)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDispatcherRunRetriesBrokerFailure asserts a broker failure pauses one row
// but the loop retries it with the same identity and continues.
func TestDispatcherRunRetriesBrokerFailure(t *testing.T) {
	outbox := newFakeOutbox("event-1", "event-2")
	publisher := newFakePublisher()
	publisher.failOnce("event-2")

	retried := make(chan struct{})
	idleCalls := 0
	idle := func(ctx context.Context) error {
		if idleCalls == 0 {
			idleCalls++
			return nil
		}
		close(retried)
		<-ctx.Done()
		return ctx.Err()
	}
	dispatcher := newDispatcher(t, outbox, publisher, 10, idle)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		runErr <- dispatcher.Run(ctx)
	}()

	select {
	case <-retried:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("dispatcher never completed the retry pass")
	}
	wantIDs := []id.EventID{"event-1", "event-2"}
	if got := publisher.deliveredIDs(); !reflect.DeepEqual(got, wantIDs) {
		t.Errorf("delivered = %v, want %v after the retry", got, wantIDs)
	}
	cancel()

	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

// TestNewDispatcherRefusesInvalidConfig asserts the worker cannot be composed
// without its durable read seam, its publisher or a positive batch bound.
func TestNewDispatcherRefusesInvalidConfig(t *testing.T) {
	cases := []struct {
		name   string
		config event.DispatcherConfig
	}{
		{
			name: "missing outbox",
			config: event.DispatcherConfig{
				Publisher: newFakePublisher(),
				BatchSize: 10,
			},
		},
		{
			name: "missing publisher",
			config: event.DispatcherConfig{
				Outbox:    newFakeOutbox(),
				BatchSize: 10,
			},
		},
		{
			name: "zero batch",
			config: event.DispatcherConfig{
				Outbox:    newFakeOutbox(),
				Publisher: newFakePublisher(),
			},
		},
		{
			name: "negative batch",
			config: event.DispatcherConfig{
				Outbox:    newFakeOutbox(),
				Publisher: newFakePublisher(),
				BatchSize: -1,
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := event.NewDispatcher(testCase.config); err == nil {
				t.Fatal("NewDispatcher accepted an invalid config")
			} else {
				assertFaultClass(t, err, fault.InvalidInput)
			}
		})
	}
}
