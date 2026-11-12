package app

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// shutdownTimeout bounds one graceful shutdown.
const shutdownTimeout = 10 * time.Second

// App is one constructed Shell composition. New performs startup preflight and
// seeds the first administrator; Run and Serve start the owned background
// loops.
type App struct {
	cfg           Config
	logger        *slog.Logger
	handler       http.Handler
	engine        *kernelengine.Engine
	events        *event.Dispatcher
	trigger       *trigger.Processor
	settlement    *taskSettlement
	runtime       *trackedRuntime
	renewal       *renewalLoop
	retention     *retentionLoop
	clock         Clock
	gate          *dispatchGate
	metrics       *operationsMetrics
	metricsSource *metricsSource

	stops []namedStop

	mu       sync.Mutex
	started  bool
	closed   bool
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closeErr error
	done     chan struct{}
}

// New builds the composition, runs startup preflight, seeds the first
// administrator and returns an App that owns no running goroutine. Any later
// construction failure stops every already-started dependency in reverse order.
func New(ctx context.Context, config Config) (*App, error) {
	config = config.resolved()
	if err := config.validate(); err != nil {
		return nil, err
	}
	builder := &builder{cfg: config, logger: config.logger()}
	environment, err := buildEnvironment(ctx, builder)
	if err != nil {
		builder.cleanup(context.WithoutCancel(ctx))
		return nil, err
	}
	app, err := wireApp(ctx, builder, environment)
	if err != nil {
		builder.cleanup(context.WithoutCancel(ctx))
		return nil, err
	}
	app.stops = append([]namedStop(nil), builder.stops...)
	handler, err := newHealthHandler(app, app.handler, config, environment.readiness, app.metricsSource)
	if err != nil {
		builder.cleanup(context.WithoutCancel(ctx))
		return nil, err
	}
	app.handler = handler
	return app, nil
}

// Handler returns the public HTTP handler: the REST edge at the declared routes
// plus the MCP JSON-RPC edge at the configured path.
func (app *App) Handler() http.Handler {
	return app.handler
}

// Run starts the background loops and blocks until ctx is canceled or Close is
// called.
func (app *App) Run(ctx context.Context) error {
	if err := app.start(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return app.Close(context.Background())
	case <-app.done:
		return app.Wait()
	}
}

// Serve starts the background loops and serves the public handler on listener
// until ctx is canceled, the server fails or Close is called.
func (app *App) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return fault.New(fault.InvalidInput, "serve requires a listener")
	}
	if err := app.start(ctx); err != nil {
		return err
	}
	server := &http.Server{Handler: app.handler, ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() {
		served <- server.Serve(listener)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		<-served
		return app.Close(shutdownCtx)
	case <-app.done:
		// Close was called from outside Serve. Stop accepting and drain
		// in-flight requests so the server never keeps serving against the
		// dependencies Close already stopped.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		<-served
		return app.Wait()
	case err := <-served:
		_ = app.Close(context.Background())
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// Wait waits for Close to finish and returns its result.
func (app *App) Wait() error {
	<-app.done
	return app.closeErr
}

// Close stops the background loops, closes each started dependency in reverse
// order and joins every owned goroutine. It stops live workloads only for the
// process runtime, whose host processes cannot outlive the controller process.
// A durable Podman or Kubernetes workload is deliberately left running: its
// stable identity is observed and adopted by the next controller, and only
// persisted cancellation, deadline or terminal reconciliation stops it. Close
// is idempotent and safe for concurrent callers, and it synchronizes with start
// so a later Run or Serve cannot launch a loop against closed dependencies.
func (app *App) Close(ctx context.Context) error {
	app.mu.Lock()
	if app.closed {
		app.mu.Unlock()
		<-app.done
		return app.closeErr
	}
	app.closed = true
	cancel := app.cancel
	app.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	app.wg.Wait()
	cleanup := ctx
	if cleanup == nil {
		cleanup = context.Background()
	}
	if cleanup.Err() != nil {
		// Shutdown is not abandoned just because the caller's request
		// context ended; the caller's deadline already bounded the call.
		cleanup = context.WithoutCancel(cleanup)
	}
	if app.runtime != nil {
		if err := app.runtime.stopAll(cleanup); err != nil && app.closeErr == nil {
			app.closeErr = err
		}
	}
	for _, step := range slices.Backward(app.stops) {

		if err := step.stop(cleanup); err != nil && app.closeErr == nil {
			app.closeErr = err
		}
	}
	close(app.done)
	return app.closeErr
}

// start launches the owned loops exactly once: engine, dispatch, trigger and
// settlement, plus the workload credential renewal loop when the composition
// wired one. It records the cancel function and the WaitGroup count under the
// same lock Close uses, and it refuses to launch anything after Close has
// begun.
func (app *App) start(ctx context.Context) error {
	app.mu.Lock()
	if app.closed {
		app.mu.Unlock()
		return fault.New(fault.InternalFailure, "app is already closed")
	}
	if app.started {
		app.mu.Unlock()
		return nil
	}
	app.started = true
	lifecycle, cancel := context.WithCancel(ctx)
	app.cancel = cancel
	loops := 4
	if app.renewal != nil {
		loops++
	}
	if app.retention != nil {
		loops++
	}
	app.wg.Add(loops)
	app.mu.Unlock()

	go func() {
		defer app.wg.Done()
		app.superviseLoop(lifecycle, "engine", app.engineLoop)
	}()
	go func() {
		defer app.wg.Done()
		app.superviseLoop(lifecycle, "dispatch", app.dispatchLoop)
	}()
	go func() {
		defer app.wg.Done()
		app.superviseLoop(lifecycle, "trigger", app.triggerLoop)
	}()
	go func() {
		defer app.wg.Done()
		app.superviseLoop(lifecycle, "settlement", app.settlementLoop)
	}()
	if app.renewal != nil {
		go func() {
			defer app.wg.Done()
			app.superviseLoop(lifecycle, "renewal", app.renewal.run)
		}()
	}
	if app.retention != nil {
		go func() {
			defer app.wg.Done()
			app.superviseLoop(lifecycle, "retention", app.retention.run)
		}()
	}
	return nil
}

// superviseLoop runs one owned background loop and restarts it after any
// non-cancellation exit. The failure is logged with a stable, secret-free
// detail and retried after the configured pause, so a loop can never die
// silently or spin; cancellation ends the loop.
func (app *App) superviseLoop(ctx context.Context, name string, loop func(context.Context) error) {
	for {
		if ctx.Err() != nil {
			return
		}
		err := loop(ctx)
		if ctx.Err() != nil {
			return
		}
		if isDependencyError(err) {
			app.metrics.countDependencyError()
		}
		app.logger.Warn("background loop stopped unexpectedly; retrying",
			"loop", name, "detail", loopFailureDetail(err))
		if app.clock.Sleep(ctx, app.cfg.LoopInterval) != nil {
			return
		}
	}
}

// loopFailureDetail renders a stable, secret-free reason for a supervised loop
// exit. Adapter error text is never logged, so credential or payload material
// cannot leak through a loop warning.
func loopFailureDetail(err error) string {
	if err == nil {
		return "returned without cancellation"
	}
	if class, ok := fault.ClassOf(err); ok {
		return string(class)
	}
	return "unexpected error"
}

// engineLoop reconciles due and unfinished executions on the configured
// cadence. It is independent of the broker: a publication outage never stops
// execution progression.
func (app *App) engineLoop(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if app.gate.Enabled() {
			if err := app.engine.Scan(ctx); err != nil && ctx.Err() == nil {
				if isDependencyError(err) {
					app.metrics.countDependencyError()
				}
				// The class is stable and secret-free; the adapter's error
				// text may quote a DSN, credential or payload.
				app.logger.Warn("kernel reconcile scan failed", "class", loopFailureDetail(err))
			}
		}
		if err := app.clock.Sleep(ctx, app.cfg.LoopInterval); err != nil {
			return err
		}
	}
}

// dispatchLoop drains the durable outbox into the broker. A broker failure
// leaves the publication intent in the outbox and retries the same identity.
func (app *App) dispatchLoop(ctx context.Context) error {
	return app.events.Run(ctx)
}

// triggerLoop scans committed accepted events for push pipelines.
func (app *App) triggerLoop(ctx context.Context) error {
	if !app.gate.Enabled() {
		// A disabled dispatch gate still lets the trigger processor observe
		// cancellation; a restore enables the same gate before retries.
		for {
			if err := app.clock.Sleep(ctx, app.cfg.LoopInterval); err != nil {
				return err
			}
			if app.gate.Enabled() {
				break
			}
		}
	}
	return app.trigger.Run(ctx)
}

// settlementLoop reports a current execution success to the task capability.
// It rescans durable open tasks (and the succeeded-execution scopes they can
// appear in) on every pass, so an observation lost to a crash is recovered on
// the next start instead of relying on an in-memory hook. A pass that cannot
// report an outcome ends here so the supervisor logs one fault class and paces
// the retry; a permanently failing observation never becomes a busy spin.
func (app *App) settlementLoop(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := app.settlement.settle(ctx); err != nil {
			return err
		}
		if err := app.clock.Sleep(ctx, app.cfg.LoopInterval); err != nil {
			return err
		}
	}
}
