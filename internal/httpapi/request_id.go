package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
)

// RequestIDHeader carries the server-generated request identifier.
const RequestIDHeader = "X-Request-ID"

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxSettings
)

// settings are the per-router values handlers need through the context.
type settings struct {
	maxRequestBytes  int64
	maxResponseBytes int
	logger           *slog.Logger
}

// RequestIDFrom returns the request ID established by the router, or "" when
// the request did not pass through one.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

func settingsFrom(ctx context.Context) settings {
	if s, ok := ctx.Value(ctxSettings).(settings); ok {
		return s
	}
	return settings{maxRequestBytes: DefaultMaxRequestBytes, maxResponseBytes: DefaultMaxResponseBytes}
}

func loggerFrom(ctx context.Context) *slog.Logger {
	if l := settingsFrom(ctx).logger; l != nil {
		return l
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newRequestID returns "req_" followed by 32 lowercase hex characters. A
// client-supplied X-Request-ID is deliberately ignored: accepting it would let a
// caller choose the identifier that appears in operator logs.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("httpapi: random source unavailable: " + err.Error())
	}
	return "req_" + hex.EncodeToString(b[:])
}

// withRequestID establishes the request ID, the settings and the safe response
// headers before anything else can respond.
func withRequestID(s settings, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), ctxRequestID, id)
		ctx = context.WithValue(ctx, ctxSettings, s)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
