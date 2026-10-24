package chatops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// WorkerConfig configures a Worker. Every value is explicit constructor
// configuration (no new environment variable, no default): retry timing, the
// maximum attempts, the claim lease and the per-attempt deadline are operational
// values the installation chooses (T-075).
type WorkerConfig struct {
	Work    WorkStore
	Tenants TenantSource
	Sender  Sender
	Retry   RetryPolicy
	// Interval is the period of the polling pass; Wake only shortens the wait.
	Interval time.Duration
	// Batch bounds the claims taken per tenant per pass.
	Batch int
	// Lease is how long a claim stays exclusive. AttemptTimeout, the deadline of
	// one send, must be shorter so a send ends before another worker may
	// reclaim the delivery.
	Lease          time.Duration
	AttemptTimeout time.Duration
	// Concurrency bounds simultaneous sends within a pass.
	Concurrency int
	Clock       func() time.Time
	Logger      *slog.Logger
}

// WorkerResult counts one pass.
type WorkerResult struct {
	Claimed, Delivered, Retried, Failed, Refused, Exhausted, Stale int
	// Unrecorded counts attempts whose outcome could not be stored; their
	// claims expire and are reclaimed, the receiver's idempotency key keeps the
	// resend safe.
	Unrecorded int
}

// Worker polls tenants for due deliveries, sends them and records each outcome.
// Delivery effects happen outside any SQL transaction: a claim commits (with its
// attempt record) before the receiver is called, and the outcome is written
// under the claim's current fencing token afterwards.
type Worker struct {
	cfg    WorkerConfig
	now    func() time.Time
	log    *slog.Logger
	wake   chan struct{}
	paused atomic.Bool
}

// NewWorker validates the configuration.
func NewWorker(cfg WorkerConfig) (*Worker, error) {
	switch {
	case cfg.Work == nil || cfg.Tenants == nil || cfg.Sender == nil:
		return nil, errors.New("chatops: a worker needs a work store, a tenant source and a sender")
	case cfg.Interval <= 0 || cfg.Batch < 1 || cfg.Concurrency < 1:
		return nil, errors.New("chatops: a worker needs a positive interval, batch and concurrency")
	case cfg.Lease <= 0 || cfg.AttemptTimeout <= 0 || cfg.AttemptTimeout >= cfg.Lease:
		return nil, errors.New("chatops: the attempt timeout must be positive and shorter than the claim lease")
	}
	if err := cfg.Retry.Validate(); err != nil {
		return nil, err
	}
	w := &Worker{cfg: cfg, now: cfg.Clock, log: cfg.Logger, wake: make(chan struct{}, 1)}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	if w.log == nil {
		w.log = slog.New(slog.DiscardHandler)
	}
	return w, nil
}

// Wake asks the worker to run a pass soon; a missed wakeup is repaired by the
// periodic pass.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Pause stops this process from claiming new work (in-flight sends finish).
// It is the process-local half of the restore fence; FenceDeliveries is the
// durable per-tenant half that also stops other processes.
func (w *Worker) Pause() { w.paused.Store(true) }

// Resume lifts Pause.
func (w *Worker) Resume() { w.paused.Store(false) }

// Paused reports whether Pause is in effect.
func (w *Worker) Paused() bool { return w.paused.Load() }

// Run polls until ctx ends.
func (w *Worker) Run(ctx context.Context) error {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		if _, err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			w.log.Warn("chatops worker pass", "error_type", fmt.Sprintf("%T", err))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case <-w.wake:
		}
	}
}

// RunOnce makes one pass over every tenant. A failing tenant does not stop the
// others; the first error is returned after the pass.
func (w *Worker) RunOnce(ctx context.Context) (WorkerResult, error) {
	var total WorkerResult
	if w.Paused() {
		return total, nil
	}
	tenants, err := w.cfg.Tenants.TenantIDs(ctx)
	if err != nil {
		return total, fmt.Errorf("chatops: list tenants: %w", err)
	}
	var first error
	for _, tenant := range tenants {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		res, err := w.runTenant(ctx, tenant)
		total.add(res)
		if err != nil && first == nil {
			first = err
		}
	}
	return total, first
}

func (r *WorkerResult) add(o WorkerResult) {
	r.Claimed += o.Claimed
	r.Delivered += o.Delivered
	r.Retried += o.Retried
	r.Failed += o.Failed
	r.Refused += o.Refused
	r.Exhausted += o.Exhausted
	r.Stale += o.Stale
	r.Unrecorded += o.Unrecorded
}

func (w *Worker) runTenant(ctx context.Context, tenant domain.TenantID) (WorkerResult, error) {
	var res WorkerResult
	batch, err := w.cfg.Work.ClaimDue(ctx, tenant, w.now(), w.cfg.Lease, w.cfg.Batch, w.cfg.Retry)
	if err != nil {
		return res, err
	}
	res.Refused, res.Exhausted = batch.Refused, batch.Exhausted
	res.Claimed = len(batch.Claims)
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, w.cfg.Concurrency)
	)
	for _, c := range batch.Claims {
		sem <- struct{}{}
		wg.Add(1)
		go func(c Claim) {
			defer func() { <-sem; wg.Done() }()
			one := w.attempt(ctx, c)
			mu.Lock()
			res.add(one)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return res, nil
}

// attempt sends one claimed delivery and records the outcome.
func (w *Worker) attempt(ctx context.Context, c Claim) (res WorkerResult) {
	remaining := c.LeaseExpiresAt.Sub(w.now())
	timeout := min(w.cfg.AttemptTimeout, remaining)
	var out Outcome
	if timeout <= 0 {
		// The lease ended before the send could start (a stalled worker): do not
		// send; the next claim takes over. Nothing was sent, so the outcome is
		// not recorded as an attempt result either.
		res.Unrecorded++
		return res
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	out = w.send(sctx, c)
	cancel()

	// Record the outcome even if the worker is shutting down: the attempt may
	// have reached the receiver, and the claim is the durable evidence.
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer fcancel()
	var final Delivery
	var err error
	for try := 0; try < 3; try++ {
		final, err = w.cfg.Work.Finish(fctx, c, out, w.now(), w.cfg.Retry)
		if err == nil || errors.Is(err, ErrStaleClaim) || fctx.Err() != nil {
			break
		}
		time.Sleep(time.Duration(try+1) * 50 * time.Millisecond)
	}
	switch {
	case errors.Is(err, ErrStaleClaim):
		res.Stale++
	case err != nil:
		res.Unrecorded++
	case final.State == StateDelivered:
		res.Delivered++
	case final.State == StatePending:
		res.Retried++
	default:
		res.Failed++
	}
	return res
}

// send calls the sender and turns a panic or an unclassified result into a
// transient internal failure that can never read as delivery.
func (w *Worker) send(ctx context.Context, c Claim) (out Outcome) {
	defer func() {
		if r := recover(); r != nil {
			// The sender may already have reached the receiver before it
			// panicked, so the outcome is unknown. The panic value is never kept.
			out = Outcome{Class: ClassInternal, Uncertain: true}
		}
	}()
	out = w.cfg.Sender.Send(ctx, SendRequest{
		Scope: c.Delivery.Scope, Destination: c.Destination, DeliveryID: c.Delivery.ID, IdempotencyKey: string(c.Delivery.ID),
		EventID: c.Delivery.EventID, EventType: c.Delivery.EventType, Attempt: c.Attempt, Payload: c.Delivery.Payload,
	})
	if !out.Delivered && !out.Class.Known() {
		out = Outcome{Class: ClassInternal, Uncertain: true}
	}
	return out
}
