package execution

import (
	"context"
	"testing"
	"time"
)

// signalRuntime reports a Stop once it has happened, so a test can wait on it without sharing memory.
type signalRuntime struct {
	*stubRuntime
	stopped chan struct{}
}

func (r *signalRuntime) Stop(ctx context.Context, w Workload) (StopEvidence, error) {
	ev, err := r.stubRuntime.Stop(ctx, w)
	close(r.stopped)
	return ev, err
}

// An accepted result is acknowledged first and its workload stopped afterwards, without the reconciler: a run
// that the result itself made terminal is not scanned again, so the stop must come from this call.
func TestAcceptedResultStopsItsWorkloadOnlyAfterTheAcknowledgmentWindow(t *testing.T) {
	store, at := running(t, &RuntimeBinding{Backend: "kubernetes"})
	rt := &signalRuntime{stubRuntime: fullRuntime("kubernetes"), stopped: make(chan struct{})}
	s := runtimeService(t, store, rt, nil)
	s.o.TerminationDelay = 150 * time.Millisecond
	store.agg.Revoked = []*Attempt{at}

	start := time.Now()
	s.terminateAfterAcknowledgment(context.Background(), store.agg)
	select {
	case <-rt.stopped:
		t.Fatalf("the workload was stopped %s after the result, inside the acknowledgment window", time.Since(start))
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-rt.stopped:
		if time.Since(start) < 150*time.Millisecond {
			t.Fatalf("stopped after %s, before the delay", time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the workload of an accepted result was never stopped")
	}
}

func TestANegativeDelayStopsAtOnce(t *testing.T) {
	store, at := running(t, &RuntimeBinding{Backend: "kubernetes"})
	rt := fullRuntime("kubernetes")
	s := runtimeService(t, store, rt, nil)
	s.o.TerminationDelay = -1
	store.agg.Revoked = []*Attempt{at}
	s.terminateAfterAcknowledgment(context.Background(), store.agg)
	if len(rt.calls) != 1 || rt.calls[0] != "stop" {
		t.Fatalf("calls %v, want one immediate stop", rt.calls)
	}
}
