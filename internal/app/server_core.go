package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is the running core assembly: the HTTP listener plus the started
// workers. It is created by Start and stopped, in the reverse order, by
// Shutdown.
type Server struct {
	core       *Core
	srv        *http.Server
	ln         net.Listener
	log        *slog.Logger
	cancelBase context.CancelFunc
	done       chan error
	stopOnce   sync.Once
	stopErr    error
}

// Start builds the core assembly, listens on cfg.Process.HTTPAddr, starts the
// event, repair and reconciliation workers and begins serving. The listener
// opens last, so a request is never accepted by a half-built process, and a
// construction failure returns before any worker started.
func Start(ctx context.Context, cfg CoreConfig, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	core, err := NewCore(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", cfg.Process.HTTPAddr)
	if err != nil {
		core.closeConnections()
		return nil, fmt.Errorf("listen: %w", err)
	}
	return core.serve(ln, log), nil
}

// StartOn is Start over a caller-provided listener.
func StartOn(ctx context.Context, ln net.Listener, cfg CoreConfig, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	core, err := NewCore(ctx, cfg, log)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	return core.serve(ln, log), nil
}

func (c *Core) serve(ln net.Listener, log *slog.Logger) *Server {
	c.startWorkers(c.workers)
	baseCtx, cancelBase := context.WithCancel(context.Background())
	s := &Server{
		core: c, ln: ln, log: log, cancelBase: cancelBase, done: make(chan error, 1),
		srv: &http.Server{
			Handler: c.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: c.cfg.Tuning.HTTPWriteTimeout,
			IdleTimeout: time.Minute, MaxHeaderBytes: 1 << 20, BaseContext: func(net.Listener) context.Context { return baseCtx },
		},
	}
	go func() {
		err := s.srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.done <- err
	}()
	return s
}

// Addr is the listener address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Core exposes the constructed assembly (evidence and later assembly tasks).
func (s *Server) Core() *Core { return s.core }

// Done receives the serve result when the listener stops.
func (s *Server) Done() <-chan error { return s.done }

// Shutdown drains the process in a fixed order: readiness turns false first,
// in-flight requests finish (open event streams are given half the budget and
// then closed, since a stream never ends by itself; workloads that call the
// runtime API meanwhile retry against the next process), the workers, dispatch
// among them, stop and are awaited so nothing starts a workload or writes after
// the runtime and the pools go away, and only then are the runtime's resources,
// the database and the broker connections closed. It is idempotent. A workload
// already started keeps running: its attempt and workload identity are durable,
// and the next process adopts or reconciles it.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.core.ready.Drain()
		deadline, ok := ctx.Deadline()
		grace := 5 * time.Second
		if ok {
			grace = time.Until(deadline) / 2
		}
		first, cancel := context.WithTimeout(ctx, grace)
		err := s.srv.Shutdown(first)
		cancel()
		if err != nil {
			s.cancelBase() // end the streams that outlived the grace period
			err = s.srv.Shutdown(ctx)
			if err != nil {
				_ = s.srv.Close()
			}
		}
		s.cancelBase()
		werr := s.core.workers.Stop(ctx)
		s.core.closeConnections()
		s.stopErr = errors.Join(err, werr)
	})
	return s.stopErr
}

// Handler is the process handler: the unauthenticated probes and the API
// router. Everything under /api/v1 passes the router's authenticate → throttle
// → authorize chain; a path no module registered is a 404 envelope.
func (c *Core) Handler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body + "\n"))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { write(w, http.StatusOK, `{"status":"ok"}`) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if failed := c.ready.Check(r.Context()); len(failed) > 0 {
			write(w, http.StatusServiceUnavailable, `{"status":"not_ready"}`)
			return
		}
		write(w, http.StatusOK, `{"status":"ready"}`)
	})
	// The runtime API of the workloads (/runtime/v1/attempts/{id}/...). It has
	// its own authentication (the attempt's session token, never a user
	// credential) and is not covered by the public router's chain.
	mux.Handle(runtimePrefix+"/", c.runtime)
	// The workload routes of the model gateway and the MCP proxy (T-039), when the
	// installation selected them: the attempt's runtime token is their only
	// credential. Unselected, the router below answers its deny-by-default 404.
	if c.ModelTools != nil {
		mux.Handle(GatewayWorkloadPrefix, c.ModelTools.GatewayHandler)
		mux.Handle(MCPWorkloadPrefix, c.ModelTools.MCPHandler)
	}
	// The native Git receive-hook endpoint: authenticated by its HMAC signature
	// alone, behind the edge throttle, never by the public router's bearer chain.
	if c.Triggers != nil {
		mux.Handle(c.Triggers.Pattern, c.Triggers.Handler)
	}
	mux.Handle("/", releaseCreated(c.router))
	return mux
}

// releaseCreated sends the run-creation response as soon as it is written. The
// run handler commits the admission, writes its answer and then confirms the
// retained source reference against Git (which can take as long as the Git
// deadline); the answer is already truthful, so the client must not wait for the
// confirmation. Everything else is untouched.
func releaseCreated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/runs") && strings.HasPrefix(r.URL.Path, APIPrefix+"/") {
			w = newFlushingWriter(w)
		}
		next.ServeHTTP(w, r)
	})
}

// flushingWriter delays the status line until the first write, gives that write
// an exact Content-Length (the handlers write each answer in one Write), closes
// the connection after the handler and flushes, so the client sees a complete
// response while the handler goes on and cannot queue another request behind it.
type flushingWriter struct {
	http.ResponseWriter
	rc    *http.ResponseController
	state *flushState
}

type flushState struct {
	status  int
	written bool
}

func newFlushingWriter(w http.ResponseWriter) flushingWriter {
	return flushingWriter{ResponseWriter: w, rc: http.NewResponseController(w), state: &flushState{}}
}

func (f flushingWriter) WriteHeader(code int) {
	if f.state.written {
		return
	}
	f.state.status = code
}

func (f flushingWriter) Write(b []byte) (int, error) {
	if !f.state.written {
		f.state.written = true
		h := f.ResponseWriter.Header()
		if h.Get("Content-Length") == "" {
			h.Set("Content-Length", strconv.Itoa(len(b)))
		}
		// The handler keeps working on this connection after the answer is
		// complete; a client that reused it would queue behind that work.
		h.Set("Connection", "close")
		if f.state.status != 0 {
			f.ResponseWriter.WriteHeader(f.state.status)
		}
		n, err := f.ResponseWriter.Write(b)
		if err == nil {
			_ = f.rc.Flush()
		}
		return n, err
	}
	return f.ResponseWriter.Write(b)
}

func (f flushingWriter) Unwrap() http.ResponseWriter { return f.ResponseWriter }

// NotReady returns the failing readiness probes by name (empty when ready).
func (c *Core) NotReady(ctx context.Context) map[string]string { return c.ready.Check(ctx) }

// Workers lists the started worker names.
func (c *Core) Workers() []string { return c.workers.Names() }
