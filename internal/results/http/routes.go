package resultshttp

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// NewHandler returns the handler of the two routes. Any other path or method is
// a contract not_found envelope that names nothing internal.
func NewHandler(cfg Config) (http.Handler, error) {
	if cfg.Service == nil {
		return nil, errors.New("resultshttp: a service is required")
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
		case "proposeSource":
			fn = h.source
		case "submitResult":
			fn = h.result
		default:
			continue
		}
		mux.HandleFunc(op.Method+" "+runtimev1.BasePath+op.Path, fn)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		h.writeError(w, newRequestID(), domain.CodeNotFound, nil)
	})
	return mux, nil
}
