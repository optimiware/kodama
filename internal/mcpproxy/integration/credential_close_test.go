package integration_test

import (
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest"
)

// Test_ended_credential_never_presented_on_close: a credential that was revoked
// or released, but whose end no use check has observed yet, is not sent to the
// remote when the session ends: not by a direct close, not by a workload's
// terminate request through the handler, not by the sweep after expiry. A
// close of a session whose credential is still usable terminates the remote
// session, which is the control showing the assertions can see a request.
func Test_ended_credential_never_presented_on_close(t *testing.T) {
	ends := map[string]func(s *mcptest.Stack, r mcptest.Ready){
		"revoked": func(s *mcptest.Stack, r mcptest.Ready) { s.RevokeCredential(r.W.Scope, r.Sec.Name, r.Sec.Version) },
		"released": func(s *mcptest.Stack, r mcptest.Ready) {
			s.RT.OwnerExec(`UPDATE secret_pins SET released_at = now() WHERE consumer_id = $1`, string(r.W.Run))
		},
	}
	deletes := func(s *mcptest.Stack, from int) (n int) {
		for _, q := range s.Fixture.Requests()[from:] {
			if q.Method == "DELETE" {
				n++
			}
		}
		return n
	}

	t.Run("control: a close with a usable credential terminates the remote session", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{})
		r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
		id := openSession(t, s, r)
		before := len(s.Fixture.Requests())
		if err := s.Service.CloseSession(s.Context(), r.W.Caller, r.Reg.ID, id); err != nil {
			t.Fatal(err)
		}
		if got := deletes(s, before); got != 1 {
			t.Fatalf("%d terminate requests reached the remote, want 1", got)
		}
	})

	for name, end := range ends {
		t.Run(name+": a direct close sends nothing", func(t *testing.T) {
			s := mcptest.New(t, mcptest.Config{})
			r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
			id := openSession(t, s, r)
			end(s, r)
			before := len(s.Fixture.Requests())
			if err := s.Service.CloseSession(s.Context(), r.W.Caller, r.Reg.ID, id); err != nil {
				t.Fatalf("a close must succeed even though the credential ended: %v", err)
			}
			if n := len(s.Fixture.Requests()) - before; n != 0 {
				t.Fatalf("%d requests reached the remote after the credential ended", n)
			}
			if st := sessionState(t, s, string(id)); st != "invalid" && st != "closed" {
				t.Fatalf("session state %s, want ended", st)
			}
		})

		t.Run(name+": a workload terminate request sends nothing", func(t *testing.T) {
			s := mcptest.New(t, mcptest.Config{})
			r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
			cs, err := s.Connect(s.Context(), s.Endpoint(r.W.Attempt, r.Reg.ID), r.W.Token)
			if err != nil {
				t.Fatal(err)
			}
			end(s, r)
			before := len(s.Fixture.Requests())
			if err := cs.Close(); err != nil {
				t.Logf("close: %v", err)
			}
			if n := len(s.Fixture.Requests()) - before; n != 0 {
				t.Fatalf("%d requests reached the remote after the credential ended", n)
			}
		})

		t.Run(name+": the sweep after expiry sends nothing", func(t *testing.T) {
			s := mcptest.New(t, mcptest.Config{Tune: func(o *mcpproxy.Options) { o.SessionTTL = 2 * time.Minute }})
			r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
			id := openSession(t, s, r)
			end(s, r)
			s.RT.Clock.Advance(3 * time.Minute)
			before := len(s.Fixture.Requests())
			if err := s.Service.Sweep(s.Context(), r.W.Scope.TenantID, 100); err != nil {
				t.Fatal(err)
			}
			if st := sessionState(t, s, string(id)); st != "expired" {
				t.Fatalf("session state %s", st)
			}
			if n := len(s.Fixture.Requests()) - before; n != 0 {
				t.Fatalf("%d requests reached the remote after the credential ended", n)
			}
		})
	}
}

// Test_ended_credential_not_presented_by_in_flight_cancellation: when a
// credential ends during a slow call, the stop that follows (the SDK would
// send a cancellation notification) presents nothing: the credential is erased
// before the call is cancelled, so no request reaches the remote after the end.
func Test_ended_credential_not_presented_by_in_flight_cancellation(t *testing.T) {
	for name, end := range map[string]func(s *mcptest.Stack, r mcptest.Ready){
		"revoked": func(s *mcptest.Stack, r mcptest.Ready) { s.RevokeCredential(r.W.Scope, r.Sec.Name, r.Sec.Version) },
		"released": func(s *mcptest.Stack, r mcptest.Ready) {
			s.RT.OwnerExec(`UPDATE secret_pins SET released_at = now() WHERE consumer_id = $1`, string(r.W.Run))
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := mcptest.New(t, mcptest.Config{})
			r := s.Setup([]string{"slow"}, []string{"slow"}, nil)
			id := openSession(t, s, r)
			errc := make(chan error, 1)
			go func() {
				_, err := s.Service.Invoke(s.Context(), r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: "req_t033_inflight", Tool: "slow", Arguments: []byte(`{"label":"inflight"}`)})
				errc <- err
			}()
			waitFor(t, "the remote to start", func() bool { return len(s.Fixture.Started()) == 1 })
			end(s, r)
			before := len(s.Fixture.Requests())
			select {
			case err := <-errc:
				if !errors.Is(err, mcpproxy.ErrOutcomeUnknown) {
					t.Fatalf("got %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("forwarding continued after the credential ended")
			}
			inv, err := s.Store.GetInvocation(s.Context(), r.W.Scope, id, "req_t033_inflight")
			if err != nil || inv.State != mcpproxy.InvocationUnknown || inv.Code != "authority_ended" {
				t.Fatalf("receipt %+v err %v, want unknown/authority_ended", inv, err)
			}
			time.Sleep(300 * time.Millisecond) // a late cancellation notification would land here
			if n := len(s.Fixture.Requests()) - before; n != 0 {
				t.Fatalf("%d requests carrying the credential reached the remote after it ended", n)
			}
		})
	}
}
