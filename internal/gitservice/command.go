package gitservice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// Command is the runnable behind `kodama git serve`. T-132 registers it in the
// binary (together with `git-hook`, which calls HookMain).
type Command struct {
	cfg Config
}

// ServeCommand validates the configuration and returns the command. It starts
// nothing; Run or Serve does.
func ServeCommand(cfg Config) (*Command, error) {
	if _, err := cfg.resolve(); err != nil {
		return nil, err
	}
	return &Command{cfg: cfg}, nil
}

// Run listens on Config.ListenAddr and serves until ctx is cancelled.
func (c *Command) Run(ctx context.Context) error {
	if c.cfg.ListenAddr == "" {
		return errors.New("gitservice: ListenAddr is required")
	}
	ln, err := net.Listen("tcp", c.cfg.ListenAddr)
	if err != nil {
		return err
	}
	return c.Serve(ctx, ln)
}

// Serve serves the Git endpoints, the control-plane RPC and the readiness
// endpoint on ln, polls the control plane's assignments when configured, and
// shuts down gracefully when ctx is cancelled: it stops accepting, gives running
// requests a bounded time and then terminates the remaining receives' process
// trees (a terminated receive is settled by reconciliation from the journal).
func (c *Command) Serve(ctx context.Context, ln net.Listener) error {
	s, err := New(c.cfg)
	if err != nil {
		ln.Close()
		return err
	}
	defer s.Close()
	srv := &http.Server{
		Handler: s.Handler(),
		// Bodies and responses are bounded per request (Limits), not by a
		// server-wide timeout that would cut a long clone.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		if c.cfg.TLSCertFile != "" && c.cfg.TLSKeyFile != "" {
			errc <- srv.ServeTLS(ln, c.cfg.TLSCertFile, c.cfg.TLSKeyFile)
			return
		}
		errc <- srv.Serve(ln)
	}()
	pollCtx, stopPolling := context.WithCancel(ctx)
	defer stopPolling() // runs before s.Close, which waits for the poll loop
	if c.cfg.PollInterval > 0 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.pollAssignments(pollCtx)
		}()
	}
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	return nil
}

// pollAssignments asks the control plane for work on an interval until ctx ends.
func (s *Service) pollAssignments(ctx context.Context) {
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	for {
		if err := s.processAssignments(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Warn("assignment poll failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// processAssignments performs the assignments of one poll. An assignment repeats
// until its completion is confirmed, so every step is idempotent: initialize is
// confirmed with Initialized, maintenance runs again harmlessly and reconcile
// re-reports the receives after the acknowledged journal sequence (the control
// plane deduplicates them by receive ID). Archive is the control plane's own
// decision (it refuses pushes); there is nothing for the worker to do.
func (s *Service) processAssignments(ctx context.Context) error {
	resp, err := s.cfg.ControlPlane.Assignments(ctx)
	if err != nil {
		return err
	}
	var first error
	for _, a := range resp.Items {
		var err error
		switch a.Kind {
		case gitv1.AssignInitialize:
			err = s.initializeAssigned(ctx, a)
		case gitv1.AssignReconcile:
			err = s.reconcileRepository(ctx, a)
		case gitv1.AssignMaintain:
			_, err = s.maintain(ctx, a.RepositoryID, gitv1.MaintainRequest{RepositoryID: a.RepositoryID, OperationID: randomID("op", 8)})
		}
		if err != nil {
			s.cfg.Logger.Warn("assignment failed", "kind", a.Kind, "repositoryId", a.RepositoryID, "error", err)
			if first == nil {
				first = fmt.Errorf("%s %s: %w", a.Kind, a.RepositoryID, err)
			}
		}
	}
	return first
}

func (s *Service) initializeAssigned(ctx context.Context, a gitv1.Assignment) error {
	ictx, cancel := context.WithTimeout(ctx, s.cfg.Limits.OperationTimeout)
	defer cancel()
	res, err := s.initialize(ictx, a.RepositoryID, gitv1.InitializeRepositoryRequest{RepositoryID: a.RepositoryID, DefaultBranch: a.DefaultBranch})
	if err != nil {
		return err
	}
	_, err = s.cfg.ControlPlane.Initialized(ictx, gitv1.InitializedRequest{RepositoryID: a.RepositoryID, DefaultBranch: a.DefaultBranch, Empty: res.Empty})
	return err
}
