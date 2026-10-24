package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

func healthHandler(ready *atomic.Bool) http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body + "\n"))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			write(w, http.StatusServiceUnavailable, `{"status":"not_ready"}`)
			return
		}
		write(w, http.StatusOK, `{"status":"ready"}`)
	})
	return mux
}

// Serve owns listener and drains in-flight requests when ctx is cancelled.
func Serve(ctx context.Context, listener net.Listener, cfg Config, logger *slog.Logger) error {
	defer listener.Close()
	var ready atomic.Bool
	server := &http.Server{
		Handler: healthHandler(&ready), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second,
		IdleTimeout: time.Minute, MaxHeaderBytes: 1 << 20,
	}
	done := make(chan error, 1)
	ready.Store(true)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		ready.Store(false)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		ready.Store(false)
		logger.Info("draining server")
		shutdown, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			<-done
			return err
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
