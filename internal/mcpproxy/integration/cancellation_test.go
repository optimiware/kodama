package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest"
)

func openSession(t *testing.T, s *mcptest.Stack, r mcptest.Ready) mcpproxy.SessionID {
	t.Helper()
	id, err := s.Service.OpenSession(s.Context(), r.W.Caller, r.Reg.ID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Test_cancellation_race: cancellation preceding admission prevents dispatch;
// cancellation during forwarding stops local work and propagates remotely
// without asserting rollback.
func Test_cancellation_race(t *testing.T) {
	t.Run("cancelled before admission: nothing is dispatched or charged", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{})
		max := int64(1)
		r := s.Setup([]string{"mutate"}, []string{"mutate"}, &max)
		id := openSession(t, s, r)
		ctx, cancel := context.WithCancel(s.Context())
		cancel()
		_, err := s.Service.Invoke(ctx, r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: "req_t033_pre", Tool: "mutate", Arguments: []byte(`{"key":"k"}`)})
		if err == nil {
			t.Fatal("a cancelled call succeeded")
		}
		if n := s.Fixture.Mutations.Load(); n != 0 {
			t.Fatalf("a call cancelled before admission was dispatched (%d)", n)
		}
		// The ceiling of one call is still available: the cancelled call was not charged.
		if _, err := s.Service.Invoke(s.Context(), r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: "req_t033_ok", Tool: "mutate", Arguments: []byte(`{"key":"k"}`)}); err != nil {
			t.Fatalf("the only permitted call was refused: %v", err)
		}
	})

	t.Run("cancelled during forwarding: the remote is told and the outcome is unknown", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{})
		r := s.Setup([]string{"slow"}, []string{"slow"}, nil)
		id := openSession(t, s, r)
		ctx, cancel := context.WithCancel(s.Context())
		errc := make(chan error, 1)
		go func() {
			_, err := s.Service.Invoke(ctx, r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: "req_t033_mid", Tool: "slow", Arguments: []byte(`{"label":"mid"}`)})
			errc <- err
		}()
		waitFor(t, "the remote to start", func() bool { return len(s.Fixture.Started()) == 1 })
		cancel()
		err := <-errc
		if !errors.Is(err, mcpproxy.ErrOutcomeUnknown) {
			t.Fatalf("got %v, want an unknown outcome (cancellation after dispatch cannot assert no effect)", err)
		}
		waitFor(t, "the remote to observe cancellation", func() bool { return len(s.Fixture.Cancelled()) == 1 })
		inv, err := s.Store.GetInvocation(s.Context(), r.W.Scope, id, "req_t033_mid")
		if err != nil || inv.State != mcpproxy.InvocationUnknown || inv.Code != "cancelled" {
			t.Fatalf("receipt %+v err %v", inv, err)
		}
	})

	t.Run("authority ending during a long call stops local forwarding", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{})
		r := s.Setup([]string{"slow"}, []string{"slow"}, nil)
		id := openSession(t, s, r)
		errc := make(chan error, 1)
		go func() {
			_, err := s.Service.Invoke(s.Context(), r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: "req_t033_rev", Tool: "slow", Arguments: []byte(`{"label":"rev"}`)})
			errc <- err
		}()
		waitFor(t, "the remote to start", func() bool { return len(s.Fixture.Started()) == 1 })
		if err := s.RT.Authority.Cancel(s.Context(), r.W.Scope, r.W.Attempt, true); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-errc:
			if !errors.Is(err, mcpproxy.ErrOutcomeUnknown) {
				t.Fatalf("got %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("forwarding continued after the attempt's authority ended")
		}
		inv, _ := s.Store.GetInvocation(s.Context(), r.W.Scope, id, "req_t033_rev")
		if inv.Code != "authority_ended" {
			t.Fatalf("receipt code %q", inv.Code)
		}
	})

	t.Run("the call ceiling is run-wide and uncertain calls stay charged", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{})
		max := int64(2)
		r := s.Setup([]string{"mutate"}, []string{"mutate"}, &max)
		id := openSession(t, s, r)
		s.Fixture.LoseNextReplies(1)
		_, err := s.Service.Invoke(s.Context(), r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: "req_t033_c1", Tool: "mutate", Arguments: []byte(`{"key":"k"}`)})
		if !errors.Is(err, mcpproxy.ErrOutcomeUnknown) {
			t.Fatalf("got %v", err)
		}
		if _, err := s.Service.Invoke(s.Context(), r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: "req_t033_c2", Tool: "mutate", Arguments: []byte(`{"key":"k"}`)}); err != nil {
			t.Fatal(err)
		}
		before := s.Fixture.Mutations.Load()
		// A retry with a new request ID does not reset the ceiling.
		if _, err := s.Service.Invoke(s.Context(), r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: "req_t033_c3", Tool: "mutate", Arguments: []byte(`{"key":"k"}`)}); !errors.Is(err, mcpproxy.ErrLimitReached) {
			t.Fatalf("got %v, want the ceiling", err)
		}
		if s.Fixture.Mutations.Load() != before {
			t.Fatal("a call beyond the ceiling was dispatched")
		}
	})
}

// Test_session_sweep: the sweep expires sessions past their lifetime, closes
// their live handles and recovers stale receipts as unknown; it never resends.
func Test_session_sweep(t *testing.T) {
	s := mcptest.New(t, mcptest.Config{Tune: func(o *mcpproxy.Options) { o.SessionTTL = 2 * time.Minute; o.StaleAfter = time.Second }})
	r := s.Setup([]string{"mutate"}, []string{"mutate"}, nil)
	id := openSession(t, s, r)
	sess, err := s.Store.GetSession(s.Context(), r.W.Scope, id)
	if err != nil {
		t.Fatal(err)
	}
	// An admitted receipt whose outcome was never recorded.
	inv, _ := mcpproxy.NewInvocation("req_t033_sweep", sess, "mutate", s.RT.Clock.Now())
	_ = inv.Dispatch(s.RT.Clock.Now())
	if _, err := s.Store.Admit(s.Context(), mcpproxy.AdmitRequest{Invocation: inv, Incarnation: s.Opts.Incarnation, Now: s.RT.Clock.Now()}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	s.RT.Clock.Advance(3 * time.Minute)
	// The service clock is the fixture clock; receipts age by wall time in the database.
	if err := s.Service.Sweep(s.Context(), r.W.Scope.TenantID, 100); err != nil {
		t.Fatal(err)
	}
	if st := sessionState(t, s, string(id)); st != "expired" {
		t.Fatalf("session state %s", st)
	}
	var state string
	if err := s.RT.Owner.QueryRow(context.Background(), `SELECT state FROM mcp_invocations WHERE id = 'req_t033_sweep'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "unknown" {
		t.Fatalf("receipt state %s", state)
	}
	if n := s.Fixture.Mutations.Load(); n != 0 {
		t.Fatalf("the sweep dispatched a call: %d", n)
	}
	if _, err := s.Service.Invoke(s.Context(), r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: "req_t033_after", Tool: "mutate", Arguments: []byte(`{"key":"k"}`)}); err == nil {
		t.Fatal("an expired session was used")
	}
}
