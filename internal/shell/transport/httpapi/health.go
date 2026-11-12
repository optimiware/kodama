package httpapi

// Operational health routes. They are a fixed, unauthenticated wrapper around
// the composed public handler:
//
//   - HealthPath is process liveness. It answers 200 whenever the process can
//     serve a request and calls no dependency, so a database, broker or KMS
//     outage never restarts a healthy process.
//   - ReadinessPath is dependency readiness. It answers 200 only when the
//     composition is started, not closing and every injected check succeeds,
//     otherwise 503. The two fixed bodies never carry an error text, a version
//     or secret material.
//   - MetricsPath is the operations metrics exposition. It answers 200 with the
//     source's bounded text or 503 with a fixed body; the source's error text is
//     never rendered.
//
// All routes accept exactly GET and HEAD; every other method gets 405 and
// every other path (including a query variant) is delegated to the composed
// handler, so public operations keep their authentication and routing. The
// wrapper refuses construction with an incomplete configuration or with a
// reserved path (the composed MCP route) that would collide with any route.

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/optimiware/kodama/internal/fault"
)

// Operational route paths.
const (
	HealthPath    = "/healthz"
	ReadinessPath = "/readyz"
	MetricsPath   = "/metrics"
)

// Fixed operational bodies. They are the entire response: no dependency error
// text, build version or secret can reach a probe client. The metrics body is
// supplied by the composition's bounded source.
const (
	HealthBody               = "ok\n"
	ReadinessBody            = "ready\n"
	ReadinessUnavailableBody = "not ready\n"
	MetricsUnavailableBody   = "metrics unavailable\n"
	methodNotAllowedBody     = "method not allowed\n"
)

// MetricsSource renders the bounded operations metrics exposition. It receives
// a context already bounded by the wrapper's timeout and must not retain it.
// The returned bytes are the complete response body.
type MetricsSource func(context.Context) ([]byte, error)

// ReadinessCheck is one dependency probe. A non-nil error makes the process
// not ready. The check receives a context already bounded by the readiness
// budget and the request context, and must not retain it.
type ReadinessCheck func(context.Context) error

// HealthConfig configures the operational wrapper. Inner, Started and Closing
// are required, and Timeout must be positive. Checks and Metrics are optional
// (a composition without declared dependencies is ready once started, and one
// without an operations source mounts no metrics route). Reserved paths are
// routes the composed handler must keep; a collision with any operational path
// is refused.
type HealthConfig struct {
	Inner         http.Handler
	Started       func() bool
	Closing       func() bool
	Checks        []ReadinessCheck
	Metrics       MetricsSource
	Timeout       time.Duration
	ReservedPaths []string
}

type healthHandler struct {
	inner   http.Handler
	started func() bool
	closing func() bool
	checks  []ReadinessCheck
	metrics MetricsSource
	timeout time.Duration
}

// NewHealthHandler builds the operational wrapper around the composed public
// handler.
func NewHealthHandler(config HealthConfig) (http.Handler, error) {
	if config.Inner == nil {
		return nil, fault.New(fault.InvalidInput, "operational handler requires a composed inner handler")
	}
	if config.Started == nil {
		return nil, fault.New(fault.InvalidInput, "operational handler requires a started checker")
	}
	if config.Closing == nil {
		return nil, fault.New(fault.InvalidInput, "operational handler requires a closing checker")
	}
	if config.Timeout <= 0 {
		return nil, fault.New(fault.InvalidInput, "operational handler requires a positive timeout")
	}
	checks := make([]ReadinessCheck, len(config.Checks))
	for index, check := range config.Checks {
		if check == nil {
			return nil, fault.New(fault.InvalidInput, "operational handler readiness check is nil")
		}
		checks[index] = check
	}
	for _, path := range config.ReservedPaths {
		if path == HealthPath || path == ReadinessPath || path == MetricsPath {
			return nil, fault.New(fault.InvalidInput,
				"operational handler reserved path collides with the operational routes")
		}
	}
	return &healthHandler{
		inner:   config.Inner,
		started: config.Started,
		closing: config.Closing,
		checks:  checks,
		metrics: config.Metrics,
		timeout: config.Timeout,
	}, nil
}

// ServeHTTP serves the two fixed routes and delegates every other request to
// the composed handler. A query or fragment is not part of either exact route,
// so it is delegated rather than silently accepted (the composed handler
// refuses it with the rest of the public request contract).
func (handler *healthHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request == nil || request.URL == nil {
		handler.inner.ServeHTTP(writer, request)
		return
	}
	if request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Fragment != "" {
		handler.inner.ServeHTTP(writer, request)
		return
	}
	switch request.URL.Path {
	case HealthPath:
		handler.serveLiveness(writer, request)
	case ReadinessPath:
		handler.serveReadiness(writer, request)
	case MetricsPath:
		if handler.metrics == nil {
			handler.inner.ServeHTTP(writer, request)
			return
		}
		handler.serveMetrics(writer, request)
	default:
		handler.inner.ServeHTTP(writer, request)
	}
}

// serveLiveness answers the process-liveness route. It deliberately consults
// no state and no dependency: the only question is whether this process can
// answer a request at all.
func (handler *healthHandler) serveLiveness(writer http.ResponseWriter, request *http.Request) {
	if !probeMethod(request.Method) {
		writeProbe(writer, request, http.StatusMethodNotAllowed, methodNotAllowedBody)
		return
	}
	writeProbe(writer, request, http.StatusOK, HealthBody)
}

// serveReadiness answers the dependency-readiness route. The state check runs
// before any dependency, so a closing process cannot be marked ready by a
// dependency that has not noticed the shutdown yet.
func (handler *healthHandler) serveReadiness(writer http.ResponseWriter, request *http.Request) {
	if !probeMethod(request.Method) {
		writeProbe(writer, request, http.StatusMethodNotAllowed, methodNotAllowedBody)
		return
	}
	if handler.closing() || !handler.started() {
		writeProbe(writer, request, http.StatusServiceUnavailable, ReadinessUnavailableBody)
		return
	}
	if err := handler.runChecks(request.Context()); err != nil {
		writeProbe(writer, request, http.StatusServiceUnavailable, ReadinessUnavailableBody)
		return
	}
	writeProbe(writer, request, http.StatusOK, ReadinessBody)
}

// serveMetrics answers the operations metrics route. The source is bounded by
// the same operational timeout as readiness; a failed or timed-out collection
// is the fixed 503 body, never adapter error text.
func (handler *healthHandler) serveMetrics(writer http.ResponseWriter, request *http.Request) {
	if !probeMethod(request.Method) {
		writeProbe(writer, request, http.StatusMethodNotAllowed, methodNotAllowedBody)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), handler.timeout)
	defer cancel()
	body, err := handler.metrics(ctx)
	if err != nil {
		writeProbe(writer, request, http.StatusServiceUnavailable, MetricsUnavailableBody)
		return
	}
	writeMetrics(writer, request, body)
}

// writeMetrics writes one metrics exposition. HEAD keeps the status, headers
// and Content-Length with no body, matching GET parity.
func writeMetrics(writer http.ResponseWriter, request *http.Request, body []byte) {
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(http.StatusOK)
	if request.Method == http.MethodHead {
		return
	}
	_, _ = writer.Write(body)
}

// runChecks runs every readiness check concurrently under one budget. It waits
// for all of them when they finish in time; a check that ignores its context
// cannot hold the probe past the budget (the broker dial is additionally
// bounded by its ConnectTimeout). The error text is never rendered.
func (handler *healthHandler) runChecks(parent context.Context) error {
	if len(handler.checks) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, handler.timeout)
	defer cancel()

	results := make(chan error, len(handler.checks))
	for _, check := range handler.checks {
		go func(check ReadinessCheck) {
			results <- check(ctx)
		}(check)
	}

	var first error
	for range handler.checks {
		select {
		case err := <-results:
			if err != nil && first == nil {
				first = err
			}
		case <-ctx.Done():
			if first == nil {
				first = ctx.Err()
			}
			return first
		}
	}
	return first
}

func probeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// writeProbe writes one fixed probe body. HEAD keeps the status and headers
// with no body, matching GET parity; every response is explicitly uncacheable.
func writeProbe(writer http.ResponseWriter, request *http.Request, status int, body string) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if status == http.StatusMethodNotAllowed {
		writer.Header().Set("Allow", "GET, HEAD")
	}
	writer.WriteHeader(status)
	if request.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(writer, body)
}
