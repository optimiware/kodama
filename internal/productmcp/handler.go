package productmcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HandlerConfig configures the authenticated MCP endpoint.
type HandlerConfig struct {
	Service       *Service
	Authenticator Authenticator
	// MaxRequestBytes bounds every request body.
	MaxRequestBytes int64
	Logger          *slog.Logger
}

// MaxHeaderBytes is for the composition root's http.Server.
const MaxHeaderBytes = 16 << 10

type callerKey struct{}

// NewHandler returns the product MCP endpoint. It serves one MCP request per
// HTTP request (the SDK's stateless mode): every request authenticates its own
// bearer and builds a server bound to that caller, so there is no session that
// another credential could reuse. Mounting it on a route belongs to the
// composition (T-132); this package registers none.
func NewHandler(cfg HandlerConfig) (http.Handler, error) {
	if cfg.Service == nil || cfg.Authenticator == nil || cfg.MaxRequestBytes <= 0 {
		return nil, errors.New("productmcp: a service, an authenticator and a request bound are required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	sdk := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		c, ok := r.Context().Value(callerKey{}).(Caller)
		if !ok {
			return nil
		}
		return NewServer(cfg.Service, c)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: cfg.MaxRequestBytes})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, err := bearer(r.Header)
		var c Caller
		if err == nil {
			c, err = cfg.Authenticator.Authenticate(r.Context(), tok)
		}
		if err == nil {
			err = c.Validate()
		}
		if err != nil {
			if errors.Is(err, ErrUnauthenticated) {
				writeError(w, http.StatusUnauthorized, "invalid_credential")
			} else {
				log.LogAttrs(r.Context(), slog.LevelWarn, "product mcp authentication unavailable")
				writeError(w, http.StatusServiceUnavailable, "unavailable")
			}
			return
		}
		// Every response may carry a live credential (the seed tool): none is
		// cached, sniffed or shared.
		// MaxBytesReader gets the server's own writer (it asks it to close the
		// connection after an oversize body); the wrapper only forces headers.
		r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxRequestBytes)
		w = &safeHeaders{ResponseWriter: w}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		sdk.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, c)))
	}), nil
}

// safeHeaders forces the response headers at the moment they are sent: the SDK
// sets its own Cache-Control for the response, which must not win.
type safeHeaders struct{ http.ResponseWriter }

func (s *safeHeaders) force() {
	s.Header().Set("Cache-Control", "no-store")
	s.Header().Set("X-Content-Type-Options", "nosniff")
}

func (s *safeHeaders) WriteHeader(code int) {
	s.force()
	s.ResponseWriter.WriteHeader(code)
}

func (s *safeHeaders) Write(b []byte) (int, error) {
	s.force()
	return s.ResponseWriter.Write(b)
}

func (s *safeHeaders) Flush() {
	s.force()
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *safeHeaders) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func bearer(h http.Header) (string, error) {
	vs := h.Values("Authorization")
	if len(vs) != 1 || len(vs[0]) > 1100 {
		return "", ErrUnauthenticated
	}
	scheme, tok, ok := strings.Cut(vs[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" || strings.ContainsAny(tok, " \t") {
		return "", ErrUnauthenticated
	}
	return tok, nil
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch status {
	case http.StatusUnauthorized:
		w.Header().Set("WWW-Authenticate", `Bearer realm="kodama-product-mcp"`)
	case http.StatusServiceUnavailable:
		w.Header().Set("Retry-After", "1")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
}
