package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest"
	"github.com/optimiware/kodama/internal/secrets"
)

func pinCount(t *testing.T, s *mcptest.Stack, run domain.RunID) int {
	t.Helper()
	var n int
	if err := s.RT.Owner.QueryRow(context.Background(), `SELECT count(*) FROM secret_pins WHERE consumer_id = $1`, string(run)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Test_credential_lifecycle: exact pins survive rotation without substitution;
// revocation, foreign kinds and scopes, key failure and finalization failure deny
// use; credentials never enter workload bindings.
func Test_credential_lifecycle(t *testing.T) {
	t.Run("the first use freezes the exact version and pins it for the run", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{})
		r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
		if n := pinCount(t, s, r.W.Run); n != 0 {
			t.Fatalf("pins before first use: %d", n)
		}
		id := openSession(t, s, r)
		if n := pinCount(t, s, r.W.Run); n != 1 {
			t.Fatalf("pins after first use: %d", n)
		}
		// Rotating the secret and registering the new version does not substitute
		// the credential of the run that froze version 1.
		v2 := s.RotateCredential(r.W.Scope, r.Sec.Name, s.Fixture.Origin(), "rotated-token")
		if v2.Version != 2 {
			t.Fatalf("rotation produced %+v", v2)
		}
		if _, err := s.Service.Edit(s.Context(), r.W.Scope, mcptest.Manage(), r.Reg.ID, r.Reg.Version,
			mcpproxy.Draft{Endpoint: s.Fixture.URL(), Secret: v2, Tools: []string{"echo"}}); err != nil {
			t.Fatal(err)
		}
		_, err := s.Service.Invoke(s.Context(), r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: "req_t033_rot", Tool: "echo", Arguments: []byte(`{"text":"x"}`)})
		if err == nil {
			t.Fatal("a session of the old revision kept working after the registration changed")
		}
		if _, err := s.Service.OpenSession(s.Context(), r.W.Caller, r.Reg.ID); !errors.Is(err, mcpproxy.ErrBindingInvalid) {
			t.Fatalf("the frozen binding was silently replaced: %v", err)
		}
		for _, q := range s.Fixture.Requests() {
			if strings.Contains(q.Authorization, "rotated-token") {
				t.Fatal("the rotated credential was substituted into a frozen run")
			}
		}
	})

	t.Run("a new run uses the new exact version", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{})
		r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
		v2 := s.RotateCredential(r.W.Scope, r.Sec.Name, s.Fixture.Origin(), s.Fixture.Token())
		reg2, err := s.Service.Edit(s.Context(), r.W.Scope, mcptest.Manage(), r.Reg.ID, r.Reg.Version,
			mcpproxy.Draft{Endpoint: s.Fixture.URL(), Secret: v2, Tools: []string{"echo"}})
		if err != nil {
			t.Fatal(err)
		}
		w2 := s.NewWorkload(mcptest.WorkloadSpec{Run: "run_t033_second"})
		s.Policy.Set(w2.Attempt, []string{mcpproxy.Qualify(reg2.ID, "echo")}, nil, false)
		if _, err := s.Service.OpenSession(s.Context(), w2.Caller, reg2.ID); err != nil {
			t.Fatal(err)
		}
		var ver int64
		if err := s.RT.Owner.QueryRow(context.Background(), `SELECT secret_version FROM mcp_run_bindings WHERE run_id = $1`, string(w2.Run)).Scan(&ver); err != nil || ver != 2 {
			t.Fatalf("binding version %d err %v", ver, err)
		}
	})

	t.Run("a revoked version is never released", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{})
		r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
		openSession(t, s, r) // pin and use version 1
		s.RevokeCredential(r.W.Scope, r.Sec.Name, 1)
		w2 := s.NewWorkload(mcptest.WorkloadSpec{}) // same run: its pin is revoked
		s.Policy.Set(w2.Attempt, []string{mcpproxy.Qualify(r.Reg.ID, "echo")}, nil, false)
		before := len(s.Fixture.Requests())
		if _, err := s.Service.OpenSession(s.Context(), w2.Caller, r.Reg.ID); err == nil {
			t.Fatal("a revoked credential opened a session")
		}
		if len(s.Fixture.Requests()) != before {
			t.Fatal("a revoked credential reached the remote")
		}
	})

	t.Run("a secret of another kind or project cannot be registered", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{})
		w := s.NewWorkload(mcptest.WorkloadSpec{})
		s.Human.Grant(mcptest.Admin, w.Scope, mcpproxy.PermList, mcpproxy.PermMutate)
		other := w.Scope
		other.ProjectID = "p033_other"
		s.Credential(other, "foreign-token", s.Fixture.Origin(), s.Fixture.Token())
		createGeneric(t, s, w.Scope, "generic-secret")
		for name, ref := range map[string]mcpproxy.SecretRef{
			"foreign project": {Name: "foreign-token", Version: 1}, "generic kind": {Name: "generic-secret", Version: 1},
			"missing": {Name: "missing-secret", Version: 1}, "missing version": {Name: "generic-secret", Version: 9},
		} {
			_, err := s.Service.Create(s.Context(), w.Scope, mcptest.Manage(), mcpproxy.Draft{Endpoint: s.Fixture.URL(), Secret: ref, Tools: []string{"echo"}})
			if !errors.Is(err, mcpproxy.ErrCredential) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("a key provider failure denies use and reaches nothing", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{})
		r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
		s.Wrapper.FailUnwrap(errors.New("kms outage SECRET-CAUSE"))
		_, err := s.Service.OpenSession(s.Context(), r.W.Caller, r.Reg.ID)
		if !errors.Is(err, mcpproxy.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if strings.Contains(err.Error(), "SECRET-CAUSE") {
			// The cause may be kept for the caller's own diagnostics but must be
			// the service's classified error text, which includes it only through
			// wrapping; the safe log and HTTP layers never print causes.
			t.Log("cause is retained for errors.Is/As only")
		}
		if n := len(s.Fixture.Requests()); n != 0 {
			t.Fatalf("%d requests reached the remote", n)
		}
	})

	t.Run("a released run pin means nothing is released", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{})
		r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
		openSession(t, s, r) // the first use registers the pin
		s.RT.OwnerExec(`UPDATE secret_pins SET released_at = now() WHERE consumer_id = $1`, string(r.W.Run))
		before := len(s.Fixture.Requests())
		w2 := s.NewWorkload(mcptest.WorkloadSpec{})
		s.Policy.Set(w2.Attempt, []string{mcpproxy.Qualify(r.Reg.ID, "echo")}, nil, false)
		if _, err := s.Service.OpenSession(s.Context(), w2.Caller, r.Reg.ID); !errors.Is(err, mcpproxy.ErrCredential) {
			t.Fatalf("got %v", err)
		}
		if len(s.Fixture.Requests()) != before {
			t.Fatal("a credential was released after the run released its pins")
		}
	})

	// Credential availability is checked at every protected use of an already
	// open session, not only when the session was opened: the live handle holds
	// no permission of its own.
	t.Run("an already-open session stops at use once its credential ends", func(t *testing.T) {
		ends := map[string]func(t *testing.T, s *mcptest.Stack, r mcptest.Ready){
			"the exact version is revoked": func(t *testing.T, s *mcptest.Stack, r mcptest.Ready) {
				s.RevokeCredential(r.W.Scope, r.Sec.Name, r.Sec.Version)
			},
			"the run released its pins": func(t *testing.T, s *mcptest.Stack, r mcptest.Ready) {
				s.RT.OwnerExec(`UPDATE secret_pins SET released_at = now() WHERE consumer_id = $1`, string(r.W.Run))
			},
			"the pinned version is missing": func(t *testing.T, s *mcptest.Stack, r mcptest.Ready) {
				s.RT.OwnerExec(`ALTER TABLE secret_pins DISABLE TRIGGER secret_pins_guard`)
				s.RT.OwnerExec(`DELETE FROM secret_pins WHERE consumer_id = $1`, string(r.W.Run))
			},
		}
		for name, end := range ends {
			for _, op := range []string{"discovery", "invocation"} {
				t.Run(name+"/"+op, func(t *testing.T) {
					s := mcptest.New(t, mcptest.Config{})
					max := int64(5)
					r := s.Setup([]string{"echo", "mutate"}, []string{"echo", "mutate"}, &max)
					id := openSession(t, s, r)
					// Usable before the credential ends.
					if tools, err := s.Service.Discover(s.Context(), r.W.Caller, r.Reg.ID, id); err != nil || len(tools) != 2 {
						t.Fatalf("discovery before: %v %v", tools, err)
					}
					end(t, s, r)
					requests := len(s.Fixture.Requests())
					var err error
					if op == "discovery" {
						_, err = s.Service.Discover(s.Context(), r.W.Caller, r.Reg.ID, id)
					} else {
						_, err = s.Service.Invoke(s.Context(), r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: "req_t033_end", Tool: "mutate", Arguments: []byte(`{"key":"k"}`)})
					}
					if !errors.Is(err, mcpproxy.ErrCredential) {
						t.Fatalf("%s on an ended credential: %v", op, err)
					}
					if n := len(s.Fixture.Requests()); n != requests {
						t.Fatalf("%d requests reached the remote after the credential ended", n-requests)
					}
					if n := s.Fixture.Mutations.Load(); n != 0 {
						t.Fatalf("a mutation ran on an ended credential: %d", n)
					}
					if st := sessionState(t, s, string(id)); st != "invalid" {
						t.Fatalf("session state %s, want invalid", st)
					}
					// Nothing was charged or recorded as dispatched, and the session
					// is gone for every later use.
					var rows int
					if err := s.RT.Owner.QueryRow(context.Background(), `SELECT count(*) FROM mcp_invocations WHERE run_id = $1`, string(r.W.Run)).Scan(&rows); err != nil || rows != 0 {
						t.Fatalf("receipts %d err %v", rows, err)
					}
					if _, err := s.Service.Discover(s.Context(), r.W.Caller, r.Reg.ID, id); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
						t.Fatalf("an invalidated session was usable again: %v", err)
					}
				})
			}
		}
	})

	t.Run("revoking another version never affects the frozen run and a deleted secret keeps its pinned version", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{})
		r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
		id := openSession(t, s, r)
		v2 := s.RotateCredential(r.W.Scope, r.Sec.Name, s.Fixture.Origin(), "rotated-token")
		s.RevokeCredential(r.W.Scope, r.Sec.Name, v2.Version)
		if _, err := s.Service.Invoke(s.Context(), r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: "req_t033_v2", Tool: "echo", Arguments: []byte(`{"text":"x"}`)}); err != nil {
			t.Fatalf("a revoked unrelated version ended the frozen run's credential: %v", err)
		}
		var secretID string
		var version int64
		if err := s.RT.Owner.QueryRow(context.Background(), `SELECT id, version FROM secrets WHERE name = $1`, r.Sec.Name).Scan(&secretID, &version); err != nil {
			t.Fatal(err)
		}
		m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: mcptest.Admin}, RequestID: "req_t033_del", Now: time.Now().UTC()}
		if err := s.SecretsSvc.Atomically(context.Background(), r.W.Scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
			_, _, err := s.SecretsSvc.DeleteSecret(ctx, w, m, r.W.Scope, secretID, domain.Version(version))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// Deletion closes new pins; the retained, pinned version stays available
		// to the run that already holds it.
		if _, err := s.Service.Invoke(s.Context(), r.W.Caller, r.Reg.ID, id, mcpproxy.InvokeRequest{ID: "req_t033_del1", Tool: "echo", Arguments: []byte(`{"text":"x"}`)}); err != nil {
			t.Fatalf("a deleted secret ended its retained pinned version: %v", err)
		}
	})

	t.Run("credentials never enter workload bindings", func(t *testing.T) {
		s := mcptest.New(t, mcptest.Config{})
		r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
		openSession(t, s, r)
		var doc string
		if err := s.RT.Owner.QueryRow(context.Background(), `SELECT context_doc::text FROM runtime_grants WHERE attempt_id = $1`, string(r.W.Attempt)).Scan(&doc); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(doc, r.Sec.Name) || strings.Contains(doc, s.Fixture.Token()) {
			t.Fatal("the pinned workload context names the integration credential")
		}
		var audit string
		if err := s.RT.Owner.QueryRow(context.Background(), `SELECT coalesce(string_agg(metadata::text, ' '), '') FROM audit`).Scan(&audit); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(audit, s.Fixture.Token()) || strings.Contains(audit, r.W.Token) {
			t.Fatal("a credential reached the audit trail")
		}
	})
}

// createGeneric creates a generic-kind secret.
func createGeneric(t *testing.T, s *mcptest.Stack, scope domain.Scope, name string) {
	t.Helper()
	ctx := context.Background()
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: mcptest.Admin}, RequestID: "req_t033", Now: time.Now().UTC()}
	p, err := s.SecretsSvc.PrepareSecret(ctx, m, scope, name, secrets.KindGeneric, []byte("not-an-mcp-credential"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SecretsSvc.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		_, err := s.SecretsSvc.CreateSecret(ctx, w, m, p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
