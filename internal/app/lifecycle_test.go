package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestWorkersStopAndAreAwaited(t *testing.T) {
	g := newWorkerGroup(context.Background(), quiet)
	var stopped atomic.Int32
	for _, name := range []string{"a", "b", "c"} {
		g.Go(name, func(ctx context.Context) error {
			<-ctx.Done()
			time.Sleep(20 * time.Millisecond) // finishing takes time: Stop must wait for it
			stopped.Add(1)
			return ctx.Err()
		})
	}
	if err := g.Healthy(); err != nil {
		t.Fatal(err)
	}
	if got := g.Names(); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("names %v", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := g.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if stopped.Load() != 3 {
		t.Fatalf("Stop returned with %d of 3 workers finished", stopped.Load())
	}
	if err := g.Healthy(); err != nil {
		t.Fatalf("an orderly stop is not a failure: %v", err)
	}
}

func TestWorkerFailureIsReportedNotSwallowed(t *testing.T) {
	g := newWorkerGroup(context.Background(), quiet)
	g.Go("returns-early", func(context.Context) error { return nil })
	g.Go("errors", func(context.Context) error { return errors.New("broken") })
	g.Go("panics", func(context.Context) error { panic("boom") })
	deadline := time.Now().Add(2 * time.Second)
	for {
		g.mu.Lock()
		n := len(g.failed)
		g.mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 3 failures recorded", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if g.Healthy() == nil {
		t.Fatal("failed workers must make the group unhealthy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = g.Stop(ctx)
}

func TestStopGivesUpWhenAWorkerIgnoresCancellation(t *testing.T) {
	g := newWorkerGroup(context.Background(), quiet)
	release := make(chan struct{})
	g.Go("stuck", func(context.Context) error { <-release; return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := g.Stop(ctx); err == nil {
		t.Fatal("a worker that does not stop must be reported")
	}
	close(release)
}

func TestDuplicateWorkerNamesAreAWiringError(t *testing.T) {
	g := newWorkerGroup(context.Background(), quiet)
	g.Go("x", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	defer func() {
		if recover() == nil {
			t.Fatal("a duplicate worker name must panic at wiring time")
		}
		_ = g.Stop(context.Background())
	}()
	g.Go("x", func(context.Context) error { return nil })
}

func TestReadiness(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := newWorkerGroup(context.Background(), quiet)
	defer func() { _ = g.Stop(context.Background()) }()
	var calls atomic.Int32
	var failing atomic.Bool
	slow := make(chan struct{})
	r := &readiness{
		log: quiet, workers: g, timeout: 50 * time.Millisecond, ttl: time.Second, now: func() time.Time { return now },
		probes: []probe{
			{"db", func(context.Context) error {
				calls.Add(1)
				if failing.Load() {
					return errors.New("down")
				}
				return nil
			}},
			{"hang", func(ctx context.Context) error {
				select {
				case <-slow:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}},
		},
	}
	close(slow)
	if f := r.Check(context.Background()); len(f) != 0 {
		t.Fatalf("ready expected: %v", f)
	}
	failing.Store(true)
	if f := r.Check(context.Background()); len(f) != 0 || calls.Load() != 1 {
		t.Fatalf("a result is reused for the ttl (calls %d, failures %v)", calls.Load(), f)
	}
	now = now.Add(2 * time.Second)
	if f := r.Check(context.Background()); f["db"] != "down" || len(f) != 1 {
		t.Fatalf("an outage must be reported by name: %v", f)
	}
	slow = make(chan struct{})
	r.probes[1].run = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	failing.Store(false)
	now = now.Add(2 * time.Second)
	if f := r.Check(context.Background()); f["hang"] == "" {
		t.Fatalf("a probe past the timeout is a failure: %v", f)
	}
	r.probes = r.probes[:1]
	now = now.Add(2 * time.Second)
	g.Go("dead", func(context.Context) error { return errors.New("gone") })
	deadline := time.Now().Add(time.Second)
	for g.Healthy() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f := r.Check(context.Background()); f["workers"] == "" {
		t.Fatalf("a dead worker must fail readiness: %v", f)
	}
	r.Drain()
	if f := r.Check(context.Background()); f["process"] != "draining" {
		t.Fatalf("draining: %v", f)
	}
}
