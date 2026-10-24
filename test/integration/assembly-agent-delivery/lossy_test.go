package assemblyagentdelivery_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// lossyFront forwards requests to the real server, and for requests whose path
// matches drops the ACKNOWLEDGMENT: the server has processed and committed the
// request when the connection is closed without a response, which is exactly an
// acknowledgment loss (the caller cannot know the outcome). It never touches
// the request.
type lossyFront struct {
	URL     string
	dropped atomic.Int64
	srv     *httptest.Server
}

func newLossyFront(t testing.TB, upstream string, drop func(r *http.Request) bool) *lossyFront {
	t.Helper()
	f := &lossyFront{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if drop(r) {
			f.dropped.Add(1)
			if h, ok := w.(http.Hijacker); ok {
				if conn, _, err := h.Hijack(); err == nil {
					conn.Close() // the response is lost after the server committed
					return
				}
			}
		}
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(out)
	}))
	t.Cleanup(f.srv.Close)
	f.URL = f.srv.URL
	return f
}

func isDecision(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/decisions")
}
