package runtimehttp

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// NewHandler returns the handler for the three routes this module owns, under
// the frozen attempt base path. Other runtime routes belong to their owners and
// are not registered here; an unregistered path or method is a contract
// not_found / invalid_request envelope, never a body naming internals.
func NewHandler(cfg Config) (http.Handler, error) {
	if cfg.Service == nil {
		return nil, errors.New("runtimehttp: a service is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h := &handler{svc: cfg.Service, log: logger}
	mux := http.NewServeMux()
	for _, op := range runtimev1.Operations {
		var fn http.HandlerFunc
		switch op.ID {
		case "admit":
			fn = h.admit
		case "renewToken":
			fn = h.renew
		case "getContext":
			fn = h.context
		default:
			continue
		}
		mux.HandleFunc(op.Method+" "+runtimev1.BasePath+op.Path, fn)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rid := newRequestID()
		h.writeError(w, "", rid, domain.CodeNotFound)
	})
	return mux, nil
}
