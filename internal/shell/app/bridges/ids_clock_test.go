// Behavior tests for the production identity and clock bridges.
package bridges_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
)

func TestIDsAllocateDistinctTypedIdentities(t *testing.T) {
	ids, err := bridges.NewIDs(strings.NewReader(strings.Repeat("0123456789abcdef", 64)))
	if err != nil {
		t.Fatalf("NewIDs: %v", err)
	}
	event, err := ids.NewEventID()
	if err != nil {
		t.Fatalf("NewEventID: %v", err)
	}
	receipt, err := ids.NewReceiptID()
	if err != nil {
		t.Fatalf("NewReceiptID: %v", err)
	}
	execution, err := ids.NewExecutionID()
	if err != nil {
		t.Fatalf("NewExecutionID: %v", err)
	}
	principal, err := ids.NewPrincipalID()
	if err != nil {
		t.Fatalf("NewPrincipalID: %v", err)
	}
	task, err := ids.NewTaskID()
	if err != nil {
		t.Fatalf("NewTaskID: %v", err)
	}
	question, err := ids.NewQuestionID()
	if err != nil {
		t.Fatalf("NewQuestionID: %v", err)
	}
	secretID, err := ids.NewSecretID()
	if err != nil {
		t.Fatalf("NewSecretID: %v", err)
	}
	row, err := ids.NewRowID()
	if err != nil {
		t.Fatalf("NewRowID: %v", err)
	}
	usage, err := ids.NewUsageID()
	if err != nil {
		t.Fatalf("NewUsageID: %v", err)
	}
	all := []string{string(event), string(receipt), string(execution), string(principal), string(task), string(question), string(secretID), row, string(usage)}
	seen := make(map[string]bool, len(all))
	for _, value := range all {
		if value == "" {
			t.Fatal("allocator returned an empty identity")
		}
		if seen[value] {
			t.Fatalf("allocator repeated identity %q", value)
		}
		seen[value] = true
	}
	prefixes := map[string]string{
		string(event): "event-", string(receipt): "receipt-", string(execution): "execution-",
		string(principal): "principal-", string(task): "task-", string(question): "question-",
		string(secretID): "secret-", row: "row-", string(usage): "usage-",
	}
	for value, prefix := range prefixes {
		if !strings.HasPrefix(value, prefix) {
			t.Fatalf("identity %q does not carry prefix %q", value, prefix)
		}
	}
}

func TestIDsRefuseNilSourceAndPropagateRandomFailure(t *testing.T) {
	if _, err := bridges.NewIDs(nil); err == nil {
		t.Fatal("NewIDs accepted a nil randomness source")
	}
	sentinel := errors.New("entropy unavailable")
	ids, err := bridges.NewIDs(failingReader{err: sentinel})
	if err != nil {
		t.Fatalf("NewIDs: %v", err)
	}
	_, err = ids.NewEventID()
	requireFaultClass(t, err, fault.InternalFailure)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error does not wrap the reader failure: %v", err)
	}
}

func TestIDsConcurrentAllocationIsDistinct(t *testing.T) {
	ids := bridges.NewSecureIDs()
	const workers = 32
	results := make(chan string, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Go(func() {
			value, err := ids.NewEventID()
			if err != nil {
				results <- "error:" + err.Error()
				return
			}
			results <- string(value)
		})
	}
	wait.Wait()
	close(results)
	seen := make(map[string]bool, workers)
	for value := range results {
		if strings.HasPrefix(value, "error:") {
			t.Fatalf("concurrent allocation failed: %s", value)
		}
		if seen[value] {
			t.Fatalf("concurrent allocation repeated %q", value)
		}
		seen[value] = true
	}
	if len(seen) != workers {
		t.Fatalf("unique identities = %d; want %d", len(seen), workers)
	}
}

func TestSecureIDsAllocateWithoutInjectedSource(t *testing.T) {
	ids := bridges.NewSecureIDs()
	first, err := ids.NewEventID()
	if err != nil {
		t.Fatalf("NewEventID: %v", err)
	}
	second, err := ids.NewEventID()
	if err != nil {
		t.Fatalf("NewEventID: %v", err)
	}
	if first == second {
		t.Fatalf("secure allocator repeated %q", first)
	}
}

func TestSystemClockUsesMonotonicTimeAndPropagatesCancellation(t *testing.T) {
	clock := bridges.NewSystemClock()
	first := clock.Now()
	time.Sleep(time.Millisecond)
	second := clock.Now()
	if second.Before(first) {
		t.Fatalf("clock moved backwards: %v then %v", first, second)
	}
	select {
	case <-clock.After(time.Millisecond):
	case <-time.After(time.Second):
		t.Fatal("After did not fire")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := clock.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep error = %v; want context.Canceled", err)
	}
	if err := clock.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
}

type failingReader struct {
	err error
}

func (reader failingReader) Read([]byte) (int, error) {
	return 0, reader.err
}

var _ io.Reader = failingReader{}
