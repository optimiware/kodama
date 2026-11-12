package httpapi_test

// The operations metrics route is the third fixed operational route. It serves
// the metrics source's bounded exposition, never its error text, accepts only
// GET and HEAD, and stays out of the public operation edge.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
)

func metricsHandler(t *testing.T, metrics httpapi.MetricsSource) http.Handler {
	t.Helper()
	return newHealthHandler(t, httpapi.HealthConfig{
		Inner:   &healthDelegate{status: http.StatusTeapot},
		Started: alwaysStarted,
		Closing: neverClosing,
		Metrics: metrics,
		Timeout: time.Second,
	})
}

func TestMetricsRouteServesTheSourceExposition(t *testing.T) {
	body := []byte("# TYPE kodama_publication_backlog gauge\nkodama_publication_backlog 2\n")
	handler := metricsHandler(t, func(context.Context) ([]byte, error) { return body, nil })

	get := healthCall(t, handler, http.MethodGet, httpapi.MetricsPath)
	if get.Code != http.StatusOK || get.Body.String() != string(body) {
		t.Fatalf("GET %s = %d %q; want 200 %q", httpapi.MetricsPath, get.Code, get.Body.String(), body)
	}
	if contentType := get.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") ||
		!strings.Contains(contentType, "version=0.0.4") {
		t.Fatalf("metrics Content-Type = %q; want the Prometheus text exposition type", contentType)
	}
	if got, want := get.Header().Get("Content-Length"), strconv.Itoa(len(body)); got != want {
		t.Fatalf("metrics Content-Length = %q; want %q", got, want)
	}

	head := healthCall(t, handler, http.MethodHead, httpapi.MetricsPath)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD %s = %d with %d body bytes; want 200 and no body", httpapi.MetricsPath, head.Code, head.Body.Len())
	}
	if got, want := head.Header().Get("Content-Length"), strconv.Itoa(len(body)); got != want {
		t.Fatalf("HEAD metrics Content-Length = %q; want %q", got, want)
	}

	post := healthCall(t, handler, http.MethodPost, httpapi.MetricsPath)
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST %s = %d Allow=%q; want 405 GET, HEAD", httpapi.MetricsPath, post.Code, post.Header().Get("Allow"))
	}
}

func TestMetricsRouteHidesSourceErrors(t *testing.T) {
	handler := metricsHandler(t, func(context.Context) ([]byte, error) {
		return nil, errors.New("SENTINEL dependency credential")
	})
	recorder := healthCall(t, handler, http.MethodGet, httpapi.MetricsPath)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Body.String() != httpapi.MetricsUnavailableBody {
		t.Fatalf("failed metrics = %d %q; want 503 %q", recorder.Code, recorder.Body.String(), httpapi.MetricsUnavailableBody)
	}
	if strings.Contains(recorder.Body.String(), "SENTINEL") {
		t.Fatalf("metrics failure rendered the source error: %q", recorder.Body.String())
	}
}

func TestMetricsRouteWithoutASourceIsDelegated(t *testing.T) {
	delegate := &healthDelegate{status: http.StatusTeapot}
	handler := newHealthHandler(t, httpapi.HealthConfig{
		Inner:   delegate,
		Started: alwaysStarted,
		Closing: neverClosing,
		Timeout: time.Second,
	})
	recorder := healthCall(t, handler, http.MethodGet, httpapi.MetricsPath)
	if recorder.Code != http.StatusTeapot || delegate.calls.Load() != 1 {
		t.Fatalf("metrics without a source = %d with %d delegations; want the composed 418", recorder.Code, delegate.calls.Load())
	}
}

func TestMetricsPathIsReserved(t *testing.T) {
	_, err := httpapi.NewHealthHandler(httpapi.HealthConfig{
		Inner:         &healthDelegate{},
		Started:       alwaysStarted,
		Closing:       neverClosing,
		Timeout:       time.Second,
		ReservedPaths: []string{httpapi.MetricsPath},
	})
	if err == nil {
		t.Fatal("NewHealthHandler accepted a composed route at the metrics path")
	}
}
