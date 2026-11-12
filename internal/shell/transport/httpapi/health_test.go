package httpapi_test

// The operational handler wrapper serves exactly two unauthenticated fixed
// routes around the composed public handler.
//
//   - /healthz is process liveness only: it consults no started/closing state
//     and calls no dependency check. A failing database, broker or KMS must
//     never restart a healthy process.
//   - /readyz is dependency readiness: 200 only when the app is started, not
//     closing and every check succeeds; otherwise 503 with a fixed body. Error
//     text, versions and secret material are never rendered.
//
// Both routes accept exactly GET and HEAD, delegate every other path (so the
// API keeps its authentication), bound the check budget, honour request
// cancellation, and refuse construction without a complete configuration or
// with a path that collides with the composed MCP route.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
)

// healthDelegate is the composed handler stand-in. It counts delegations and
// answers with one distinctive status/body so a wrapper route that accidentally
// falls through is observable.
type healthDelegate struct {
	calls  atomic.Int64
	status int
	body   string
}

func (delegate *healthDelegate) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	delegate.calls.Add(1)
	if delegate.status != 0 {
		writer.WriteHeader(delegate.status)
	}
	if delegate.body != "" {
		_, _ = writer.Write([]byte(delegate.body))
	}
}

func newHealthHandler(t *testing.T, config httpapi.HealthConfig) http.Handler {
	t.Helper()
	handler, err := httpapi.NewHealthHandler(config)
	if err != nil {
		t.Fatalf("NewHealthHandler: %v", err)
	}
	return handler
}

func healthCall(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func alwaysStarted() bool { return true }
func neverClosing() bool  { return false }

func TestHealthRoutesServeFixedUnauthenticatedBodies(t *testing.T) {
	delegate := &healthDelegate{status: http.StatusTeapot}
	handler := newHealthHandler(t, httpapi.HealthConfig{
		Inner:   delegate,
		Started: alwaysStarted,
		Closing: neverClosing,
		Timeout: time.Second,
	})

	cases := []struct {
		path string
		body string
	}{
		{httpapi.HealthPath, httpapi.HealthBody},
		{httpapi.ReadinessPath, httpapi.ReadinessBody},
	}
	for _, testCase := range cases {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			request := httptest.NewRequest(method, testCase.path, nil)
			// The routes are unauthenticated: a presented credential is
			// ignored rather than validated or logged.
			request.Header.Set("Authorization", "Bearer an-unrelated-credential")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("%s %s status = %d; want 200", method, testCase.path, recorder.Code)
			}
			if got := recorder.Body.String(); method == http.MethodHead {
				if got != "" {
					t.Fatalf("HEAD %s wrote body %q; want no body", testCase.path, got)
				}
			} else if got != testCase.body {
				t.Fatalf("%s %s body = %q; want the fixed %q", method, testCase.path, got, testCase.body)
			}
			if got, want := recorder.Header().Get("Content-Length"), strconv.Itoa(len(testCase.body)); got != want {
				t.Fatalf("%s %s Content-Length = %q; want %q", method, testCase.path, got, want)
			}
			if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
				t.Fatalf("%s %s Content-Type = %q; want text/plain", method, testCase.path, contentType)
			}
		}
	}
	if calls := delegate.calls.Load(); calls != 0 {
		t.Fatalf("the operational routes delegated %d requests to the composed handler; want 0", calls)
	}
}

func TestLivenessNeverConsultsStateOrDependencies(t *testing.T) {
	var checkCalls atomic.Int64
	handler := newHealthHandler(t, httpapi.HealthConfig{
		Inner: &healthDelegate{status: http.StatusTeapot},
		// A not-yet-started, closing process is deliberately hostile state:
		// liveness must still answer 200 because the process itself runs.
		Started: func() bool { return false },
		Closing: func() bool { return true },
		Checks: []httpapi.ReadinessCheck{
			func(context.Context) error {
				checkCalls.Add(1)
				return errors.New("LIVENESS-MUST-NOT-CALL-THIS")
			},
		},
		Timeout: time.Second,
	})

	recorder := healthCall(t, handler, http.MethodGet, httpapi.HealthPath)
	if recorder.Code != http.StatusOK || recorder.Body.String() != httpapi.HealthBody {
		t.Fatalf("healthz = %d %q; want fixed 200 %q", recorder.Code, recorder.Body.String(), httpapi.HealthBody)
	}
	if calls := checkCalls.Load(); calls != 0 {
		t.Fatalf("healthz ran %d dependency checks; liveness must not depend on anything", calls)
	}
}

func TestReadinessRequiresStartedAndNotClosing(t *testing.T) {
	var started, closing atomic.Bool
	handler := newHealthHandler(t, httpapi.HealthConfig{
		Inner:   &healthDelegate{status: http.StatusTeapot},
		Started: started.Load,
		Closing: closing.Load,
		Timeout: time.Second,
	})

	cases := []struct {
		name     string
		started  bool
		closing  bool
		wantCode int
		wantBody string
	}{
		{name: "not started", started: false, closing: false, wantCode: http.StatusServiceUnavailable, wantBody: httpapi.ReadinessUnavailableBody},
		{name: "not started and closing", started: false, closing: true, wantCode: http.StatusServiceUnavailable, wantBody: httpapi.ReadinessUnavailableBody},
		{name: "closing", started: true, closing: true, wantCode: http.StatusServiceUnavailable, wantBody: httpapi.ReadinessUnavailableBody},
		{name: "started", started: true, closing: false, wantCode: http.StatusOK, wantBody: httpapi.ReadinessBody},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			started.Store(testCase.started)
			closing.Store(testCase.closing)
			recorder := healthCall(t, handler, http.MethodGet, httpapi.ReadinessPath)
			if recorder.Code != testCase.wantCode || recorder.Body.String() != testCase.wantBody {
				t.Fatalf("readyz = %d %q; want %d %q", recorder.Code, recorder.Body.String(), testCase.wantCode, testCase.wantBody)
			}
		})
	}
}

func TestReadinessFailsWhenAnyCheckFails(t *testing.T) {
	const checks = 3
	for failing := range checks {
		t.Run(fmt.Sprintf("check-%d", failing), func(t *testing.T) {
			var calls [checks]atomic.Int64
			readiness := make([]httpapi.ReadinessCheck, checks)
			for index := range readiness {
				position := index
				readiness[index] = func(context.Context) error {
					calls[position].Add(1)
					if position == failing {
						return errors.New("DEPENDENCY-FAILURE-SENTINEL")
					}
					return nil
				}
			}
			handler := newHealthHandler(t, httpapi.HealthConfig{
				Inner:   &healthDelegate{status: http.StatusTeapot},
				Started: alwaysStarted,
				Closing: neverClosing,
				Checks:  readiness,
				Timeout: time.Second,
			})

			recorder := healthCall(t, handler, http.MethodGet, httpapi.ReadinessPath)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("readyz with check %d failing = %d; want 503", failing, recorder.Code)
			}
			if body := recorder.Body.String(); body != httpapi.ReadinessUnavailableBody {
				t.Fatalf("readyz failure body = %q; want the fixed %q", body, httpapi.ReadinessUnavailableBody)
			}
			if body := recorder.Body.String(); strings.Contains(body, "SENTINEL") {
				t.Fatalf("readyz rendered the dependency error text: %q", body)
			}
			// Every check is invoked: one failing dependency must not mask
			// another, and the wrapper must not skip work based on order.
			for index := range calls {
				if got := calls[index].Load(); got != 1 {
					t.Fatalf("check %d ran %d times; want exactly once", index, got)
				}
			}
		})
	}
}

func TestReadinessRecoversAfterTransientFailure(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	handler := newHealthHandler(t, httpapi.HealthConfig{
		Inner:   &healthDelegate{status: http.StatusTeapot},
		Started: alwaysStarted,
		Closing: neverClosing,
		Checks: []httpapi.ReadinessCheck{
			func(context.Context) error {
				if failing.Load() {
					return errors.New("transient dependency failure")
				}
				return nil
			},
		},
		Timeout: time.Second,
	})

	if recorder := healthCall(t, handler, http.MethodGet, httpapi.ReadinessPath); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz while failing = %d; want 503", recorder.Code)
	}
	failing.Store(false)
	recorder := healthCall(t, handler, http.MethodGet, httpapi.ReadinessPath)
	if recorder.Code != http.StatusOK || recorder.Body.String() != httpapi.ReadinessBody {
		t.Fatalf("readyz after recovery = %d %q; want 200 %q", recorder.Code, recorder.Body.String(), httpapi.ReadinessBody)
	}
}

func TestReadinessDeadlineBoundsChecksThatIgnoreCancellation(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	handler := newHealthHandler(t, httpapi.HealthConfig{
		Inner:   &healthDelegate{status: http.StatusTeapot},
		Started: alwaysStarted,
		Closing: neverClosing,
		Checks: []httpapi.ReadinessCheck{
			func(context.Context) error {
				// A driver that ignores its context must not hold the probe
				// open past the readiness budget (natsgo.Connect is bounded by
				// ConnectTimeout for the same reason).
				<-release
				return nil
			},
		},
		Timeout: 50 * time.Millisecond,
	})

	startedAt := time.Now()
	recorder := healthCall(t, handler, http.MethodGet, httpapi.ReadinessPath)
	elapsed := time.Since(startedAt)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with a blocked check = %d; want 503", recorder.Code)
	}
	if elapsed > time.Second {
		t.Fatalf("readyz returned after %s; the readiness budget did not bound the check", elapsed)
	}
}

func TestReadinessPropagatesRequestCancellation(t *testing.T) {
	observed := make(chan struct{})
	handler := newHealthHandler(t, httpapi.HealthConfig{
		Inner:   &healthDelegate{status: http.StatusTeapot},
		Started: alwaysStarted,
		Closing: neverClosing,
		Checks: []httpapi.ReadinessCheck{
			func(ctx context.Context) error {
				<-ctx.Done()
				close(observed)
				return ctx.Err()
			},
		},
		Timeout: time.Minute,
	})

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, httpapi.ReadinessPath, nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	startedAt := time.Now()
	handler.ServeHTTP(recorder, request)
	elapsed := time.Since(startedAt)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with a cancelled request = %d; want 503", recorder.Code)
	}
	if elapsed > time.Second {
		t.Fatalf("readyz ignored request cancellation for %s", elapsed)
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("the readiness check never observed request cancellation")
	}
}

func TestHealthHandlerDelegatesEveryOtherRequest(t *testing.T) {
	delegate := &healthDelegate{status: http.StatusTeapot}
	handler := newHealthHandler(t, httpapi.HealthConfig{
		Inner:   delegate,
		Started: alwaysStarted,
		Closing: neverClosing,
		Timeout: time.Second,
	})

	for _, target := range []string{
		"/",
		"/api/v1/tenants",
		"/healthz/",
		"//healthz",
		"/healthz?probe=1",
		"/readyz?probe=1",
		"/mcp",
		"/readyz/extra",
	} {
		recorder := healthCall(t, handler, http.MethodGet, target)
		if recorder.Code != http.StatusTeapot {
			t.Fatalf("GET %s status = %d; want the composed handler's 418", target, recorder.Code)
		}
	}
	if calls := delegate.calls.Load(); calls != 8 {
		t.Fatalf("delegated requests = %d; want 8 (the exact health routes must not shadow another path)", calls)
	}
}

func TestHealthRoutesRefuseUnsupportedMethods(t *testing.T) {
	var checkCalls atomic.Int64
	delegate := &healthDelegate{status: http.StatusTeapot}
	handler := newHealthHandler(t, httpapi.HealthConfig{
		Inner:   delegate,
		Started: alwaysStarted,
		Closing: neverClosing,
		Checks: []httpapi.ReadinessCheck{
			func(context.Context) error { checkCalls.Add(1); return nil },
		},
		Timeout: time.Second,
	})

	for _, path := range []string{httpapi.HealthPath, httpapi.ReadinessPath} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			recorder := healthCall(t, handler, method, path)
			if recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s status = %d; want 405", method, path, recorder.Code)
			}
			if allow := recorder.Header().Get("Allow"); allow != "GET, HEAD" {
				t.Fatalf("%s %s Allow = %q; want GET, HEAD", method, path, allow)
			}
		}
	}
	if calls := delegate.calls.Load(); calls != 0 {
		t.Fatalf("unsupported health methods delegated %d requests; want 0", calls)
	}
	if calls := checkCalls.Load(); calls != 0 {
		t.Fatalf("unsupported readiness methods ran %d checks; want 0", calls)
	}
}

func TestHealthHandlerRefusesIncompleteConfiguration(t *testing.T) {
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	cases := []struct {
		name   string
		config httpapi.HealthConfig
		want   string
	}{
		{
			name:   "nil inner handler",
			config: httpapi.HealthConfig{Started: alwaysStarted, Closing: neverClosing, Timeout: time.Second},
			want:   "inner",
		},
		{
			name:   "missing started checker",
			config: httpapi.HealthConfig{Inner: inner, Closing: neverClosing, Timeout: time.Second},
			want:   "started",
		},
		{
			name:   "missing closing checker",
			config: httpapi.HealthConfig{Inner: inner, Started: alwaysStarted, Timeout: time.Second},
			want:   "closing",
		},
		{
			name:   "non-positive timeout",
			config: httpapi.HealthConfig{Inner: inner, Started: alwaysStarted, Closing: neverClosing},
			want:   "timeout",
		},
		{
			name: "nil readiness check",
			config: httpapi.HealthConfig{
				Inner: inner, Started: alwaysStarted, Closing: neverClosing,
				Checks: []httpapi.ReadinessCheck{nil}, Timeout: time.Second,
			},
			want: "check",
		},
		{
			name: "liveness path collides with the composed route",
			config: httpapi.HealthConfig{
				Inner: inner, Started: alwaysStarted, Closing: neverClosing,
				ReservedPaths: []string{httpapi.HealthPath}, Timeout: time.Second,
			},
			want: "reserved",
		},
		{
			name: "readiness path collides with the composed route",
			config: httpapi.HealthConfig{
				Inner: inner, Started: alwaysStarted, Closing: neverClosing,
				ReservedPaths: []string{httpapi.ReadinessPath}, Timeout: time.Second,
			},
			want: "reserved",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := httpapi.NewHealthHandler(testCase.config); err == nil {
				t.Fatalf("NewHealthHandler accepted an incomplete configuration")
			} else if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refusal = %q; want substring %q", err.Error(), testCase.want)
			}
		})
	}
}

func TestReadinessFailureBodyCarriesNoSecretOrVersion(t *testing.T) {
	handler := newHealthHandler(t, httpapi.HealthConfig{
		Inner:   &healthDelegate{status: http.StatusTeapot},
		Started: alwaysStarted,
		Closing: neverClosing,
		Checks: []httpapi.ReadinessCheck{
			func(context.Context) error {
				return errors.New("postgres://runtime:SECRET-PASSWORD@db:5432/kodama version 9.9.9")
			},
		},
		Timeout: time.Second,
	})

	recorder := healthCall(t, handler, http.MethodGet, httpapi.ReadinessPath)
	body := recorder.Body.String()
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d; want 503", recorder.Code)
	}
	if body != httpapi.ReadinessUnavailableBody {
		t.Fatalf("readyz body = %q; want the fixed %q", body, httpapi.ReadinessUnavailableBody)
	}
	for _, leaked := range []string{"SECRET", "postgres", "9.9.9", "version"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("readyz body leaked %q: %q", leaked, body)
		}
	}
}

func TestReadinessIsConcurrencySafe(t *testing.T) {
	var started, closing atomic.Bool
	started.Store(true)
	handler := newHealthHandler(t, httpapi.HealthConfig{
		Inner:   &healthDelegate{status: http.StatusTeapot},
		Started: started.Load,
		Closing: closing.Load,
		Checks: []httpapi.ReadinessCheck{
			func(context.Context) error { return nil },
			func(context.Context) error { return nil },
		},
		Timeout: time.Second,
	})

	var wait sync.WaitGroup
	unexpected := atomic.Int64{}
	for range 16 {
		wait.Go(func() {
			for range 16 {
				recorder := healthCall(t, handler, http.MethodGet, httpapi.ReadinessPath)
				if recorder.Code != http.StatusOK && recorder.Code != http.StatusServiceUnavailable {
					unexpected.Add(1)
				}
			}
		})
	}
	closing.Store(true)
	wait.Wait()
	if got := unexpected.Load(); got != 0 {
		t.Fatalf("%d concurrent readiness responses were neither 200 nor 503", got)
	}
	if recorder := healthCall(t, handler, http.MethodGet, httpapi.ReadinessPath); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz while closing = %d; want 503", recorder.Code)
	}
}
