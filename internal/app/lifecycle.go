package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// workerGroup starts and stops the background workers of the assembly
// deliberately. Every worker runs under one cancellable context; stopping waits
// for all of them (bounded by the caller's context), so a shutdown never leaves
// a goroutine writing after the pools are closed. A worker that returns while
// its context is live is a failure: it is recorded and readiness reports it, so
// the process is restarted by its supervisor instead of silently losing a loop.
type workerGroup struct {
	log    *slog.Logger
	cancel context.CancelFunc
	ctx    context.Context
	wg     sync.WaitGroup

	mu      sync.Mutex
	started map[string]bool
	failed  map[string]error
}

func newWorkerGroup(parent context.Context, log *slog.Logger) *workerGroup {
	ctx, cancel := context.WithCancel(parent)
	return &workerGroup{log: log, ctx: ctx, cancel: cancel, started: map[string]bool{}, failed: map[string]error{}}
}

// Go starts one named worker. Names are unique.
func (g *workerGroup) Go(name string, run func(ctx context.Context) error) {
	g.mu.Lock()
	if g.started[name] {
		g.mu.Unlock()
		panic("app: duplicate worker " + name) // a wiring error caught by every boot test
	}
	g.started[name] = true
	g.mu.Unlock()
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				g.fail(name, fmt.Errorf("panic: %v", p))
			}
		}()
		err := run(g.ctx)
		if g.ctx.Err() != nil && (err == nil || errors.Is(err, context.Canceled)) {
			return
		}
		if err == nil {
			err = errors.New("the worker returned before shutdown")
		}
		g.fail(name, err)
	}()
}

func (g *workerGroup) fail(name string, err error) {
	g.log.Error("worker stopped", "worker", name, "error", err)
	g.mu.Lock()
	g.failed[name] = err
	g.mu.Unlock()
}

// Healthy reports the first failed worker.
func (g *workerGroup) Healthy() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	names := make([]string, 0, len(g.failed))
	for n := range g.failed {
		names = append(names, n)
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return fmt.Errorf("worker %s stopped: %w", names[0], g.failed[names[0]])
}

// Names lists the started workers.
func (g *workerGroup) Names() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.started))
	for n := range g.started {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Stop cancels every worker and waits for them to return or ctx to end.
func (g *workerGroup) Stop(ctx context.Context) error {
	g.cancel()
	done := make(chan struct{})
	go func() { g.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("workers did not stop: %w", ctx.Err())
	}
}

// probe is one dependency check of readiness.
type probe struct {
	name string
	run  func(ctx context.Context) error
}

// readiness answers /readyz from the configured dependencies: every probe must
// pass within the timeout, no worker may have failed, and the process must not
// be draining. A result is reused for ttl so probe traffic cannot amplify into
// dependency load; a failing result is reused as briefly as a passing one.
type readiness struct {
	log      *slog.Logger
	probes   []probe
	workers  *workerGroup
	timeout  time.Duration
	ttl      time.Duration
	now      func() time.Time
	draining atomic.Bool

	mu       sync.Mutex
	checked  time.Time
	failures map[string]string
	lastKey  string
}

func (r *readiness) Drain() { r.draining.Store(true) }

// Check runs (or reuses) the dependency probes and returns the failed ones by
// name; the map is empty when the process is ready.
func (r *readiness) Check(ctx context.Context) map[string]string {
	if r.draining.Load() {
		return map[string]string{"process": "draining"}
	}
	r.mu.Lock()
	if r.now().Sub(r.checked) < r.ttl && !r.checked.IsZero() {
		out := copyFailures(r.failures)
		r.mu.Unlock()
		return out
	}
	r.mu.Unlock()

	out := map[string]string{}
	if err := r.workers.Healthy(); err != nil {
		out["workers"] = err.Error()
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range r.probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			if err := p.run(pctx); err != nil {
				mu.Lock()
				out[p.name] = err.Error()
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	r.mu.Lock()
	r.failures, r.checked = out, r.now()
	key := failureKey(out)
	changed := key != r.lastKey
	r.lastKey = key
	r.mu.Unlock()
	if changed { // log transitions, not every probe
		if len(out) == 0 {
			r.log.Info("readiness restored")
		} else {
			r.log.Warn("not ready", "failed", key)
		}
	}
	return copyFailures(out)
}

func copyFailures(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func failureKey(m map[string]string) string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	s := ""
	for _, n := range names {
		s += n + ";"
	}
	return s
}
