package mcpproxy_test

import (
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
)

var (
	scope = domain.Scope{TenantID: "t033_acme", ProjectID: "p033_inventory"}
	t0    = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	actor = mcpproxy.Actor{ID: "u033_admin"}
)

func draft() mcpproxy.Draft {
	return mcpproxy.Draft{Endpoint: "https://MCP.Example.com/mcp", Secret: mcpproxy.SecretRef{Name: "remote-token", Version: 1}, Tools: []string{"echo", "add"}}
}

func newReg(t *testing.T) mcpproxy.Registration {
	t.Helper()
	r, err := mcpproxy.NewRegistration(scope, "mcp_t033_a", draft(), actor, t0)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func Test_entity_transitions(t *testing.T) {
	t.Run("registration normalizes content and starts at version 1", func(t *testing.T) {
		r := newReg(t)
		if r.Version != 1 || r.Endpoint != "https://mcp.example.com/mcp" || r.Tools[0] != "add" {
			t.Fatalf("unexpected registration %+v", r)
		}
	})
	t.Run("endpoint forms that could carry authority are rejected", func(t *testing.T) {
		for _, e := range []string{"http://mcp.example.com/", "https://user:pw@mcp.example.com/", "https://mcp.example.com/?token=x",
			"https://mcp.example.com/#frag", "mcp.example.com", "https://", "https://mcp.example.com:0/", "https://mcp.exämple.com/", ""} {
			d := draft()
			d.Endpoint = e
			if _, err := mcpproxy.NewRegistration(scope, "mcp_t033_a", d, actor, t0); !errors.Is(err, mcpproxy.ErrInvalid) {
				t.Errorf("%q: got %v, want ErrInvalid", e, err)
			}
		}
	})
	t.Run("tool names and secret references are strict", func(t *testing.T) {
		for _, tools := range [][]string{{"a/b"}, {"*"}, {""}, {"x", "x"}} {
			d := draft()
			d.Tools = tools
			if _, err := d.Validate(); !errors.Is(err, mcpproxy.ErrInvalid) {
				t.Errorf("%v: got %v", tools, err)
			}
		}
		d := draft()
		d.Secret.Version = 0
		if _, err := d.Validate(); !errors.Is(err, mcpproxy.ErrInvalid) {
			t.Errorf("version 0: %v", err)
		}
	})
	t.Run("edits use the expected version and keep ownership", func(t *testing.T) {
		r := newReg(t)
		if _, err := r.Edit(2, draft(), actor, t0); !errors.Is(err, mcpproxy.ErrVersionConflict) {
			t.Fatalf("stale edit: %v", err)
		}
		d := draft()
		d.Tools = []string{"echo"}
		n, err := r.Edit(1, d, actor, t0.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if n.Version != 2 || n.Scope != r.Scope || n.ID != r.ID || len(n.Tools) != 1 || n.CreatedAt != r.CreatedAt {
			t.Fatalf("edit changed ownership or lost data: %+v", n)
		}
	})
	t.Run("deletion is an irreversible tombstone", func(t *testing.T) {
		r := newReg(t)
		d, err := r.Delete(1, actor, t0)
		if err != nil || !d.Deleted() || d.Version != 2 || d.Enables("echo") {
			t.Fatalf("delete: %+v %v", d, err)
		}
		if _, err := d.Edit(2, draft(), actor, t0); !errors.Is(err, mcpproxy.ErrInvalidTransition) {
			t.Errorf("edit of tombstone: %v", err)
		}
		if _, err := d.Delete(2, actor, t0); !errors.Is(err, mcpproxy.ErrInvalidTransition) {
			t.Errorf("second delete: %v", err)
		}
	})
	t.Run("a stale expected version never deletes", func(t *testing.T) {
		if _, err := newReg(t).Delete(5, actor, t0); !errors.Is(err, mcpproxy.ErrVersionConflict) {
			t.Fatal(err)
		}
	})
}

func caller() mcpproxy.Caller {
	return mcpproxy.Caller{Scope: scope, RunID: "run_t033", AttemptID: "att_t033", Session: "rts_t033"}
}

func newSession(t *testing.T) mcpproxy.RemoteSession {
	t.Helper()
	s, err := mcpproxy.NewRemoteSession("mcs_t033_0123456789abcdef", caller(), newReg(t), "2025-11-25", "inc-1", t0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func Test_session_lifecycle(t *testing.T) {
	t.Run("only an active, unexpired session of this process is usable", func(t *testing.T) {
		s := newSession(t)
		if err := s.Usable(t0, "inc-1"); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("initializing session usable: %v", err)
		}
		if err := s.Activate(t0); err != nil {
			t.Fatal(err)
		}
		if err := s.Usable(t0.Add(time.Minute), "inc-1"); err != nil {
			t.Fatal(err)
		}
		if err := s.Usable(t0.Add(time.Hour), "inc-1"); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("expired: %v", err)
		}
		if err := s.Usable(t0, "inc-2"); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("another process: %v", err)
		}
	})
	t.Run("ownership is the complete caller tuple", func(t *testing.T) {
		s := newSession(t)
		if !s.Owns(caller()) {
			t.Fatal("owner rejected")
		}
		for name, mod := range map[string]func(*mcpproxy.Caller){
			"tenant":  func(c *mcpproxy.Caller) { c.Scope.TenantID = "t033_globex" },
			"project": func(c *mcpproxy.Caller) { c.Scope.ProjectID = "p033_other" },
			"run":     func(c *mcpproxy.Caller) { c.RunID = "run_other" },
			"attempt": func(c *mcpproxy.Caller) { c.AttemptID = "att_other" },
			"grant":   func(c *mcpproxy.Caller) { c.Session = "rts_other" },
		} {
			c := caller()
			mod(&c)
			if s.Owns(c) {
				t.Errorf("a different %s owns the session", name)
			}
		}
	})
	t.Run("terminal states never revive", func(t *testing.T) {
		for name, end := range map[string]func(*mcpproxy.RemoteSession) error{
			"closed":  func(s *mcpproxy.RemoteSession) error { return s.Close(t0) },
			"invalid": func(s *mcpproxy.RemoteSession) error { return s.Invalidate(t0) },
			"expired": func(s *mcpproxy.RemoteSession) error { return s.Expire(t0.Add(2 * time.Hour)) },
		} {
			s := newSession(t)
			if err := s.Activate(t0); err != nil {
				t.Fatal(err)
			}
			if err := end(&s); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !s.State.Terminal() {
				t.Fatalf("%s is not terminal", name)
			}
			for opName, op := range map[string]func() error{
				"activate": func() error { return s.Activate(t0) }, "close": func() error { return s.Close(t0) },
				"invalidate": func() error { return s.Invalidate(t0) }, "expire": func() error { return s.Expire(t0.Add(3 * time.Hour)) },
				"fail": func() error { return s.Fail(t0) },
			} {
				if err := op(); !errors.Is(err, mcpproxy.ErrInvalidTransition) {
					t.Errorf("%s then %s: %v", name, opName, err)
				}
			}
		}
	})
	t.Run("a session cannot expire early or activate after expiry", func(t *testing.T) {
		s := newSession(t)
		if err := s.Activate(t0.Add(2 * time.Hour)); !errors.Is(err, mcpproxy.ErrInvalidTransition) {
			t.Fatalf("late activation: %v", err)
		}
		if err := s.Expire(t0); !errors.Is(err, mcpproxy.ErrInvalidTransition) {
			t.Fatalf("early expiry: %v", err)
		}
	})
	t.Run("a session cannot bind a tombstoned or foreign registration", func(t *testing.T) {
		r := newReg(t)
		d, _ := r.Delete(1, actor, t0)
		if _, err := mcpproxy.NewRemoteSession("mcs_t033_0123456789abcdef", caller(), d, "2025-11-25", "inc-1", t0, time.Hour); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("tombstone: %v", err)
		}
		c := caller()
		c.Scope.ProjectID = "p033_other"
		if _, err := mcpproxy.NewRemoteSession("mcs_t033_0123456789abcdef", c, r, "2025-11-25", "inc-1", t0, time.Hour); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("foreign project: %v", err)
		}
	})
}

func Test_invocation_terminality(t *testing.T) {
	s := newSession(t)
	_ = s.Activate(t0)
	inv, err := mcpproxy.NewInvocation("req_t033_1", s, "echo", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := inv.MarkUnknown("x", t0); !errors.Is(err, mcpproxy.ErrInvalidTransition) {
		t.Fatalf("an admitted call has nothing to lose yet: %v", err)
	}
	if err := inv.Dispatch(t0); err != nil {
		t.Fatal(err)
	}
	if err := inv.Dispatch(t0); !errors.Is(err, mcpproxy.ErrInvalidTransition) {
		t.Fatalf("a receipt must be dispatchable only once: %v", err)
	}
	if err := inv.MarkUnknown("deadline", t0); err != nil {
		t.Fatal(err)
	}
	for name, op := range map[string]func() error{
		"succeed": func() error { return inv.Succeed(t0) }, "fail": func() error { return inv.Fail("x", t0) },
		"unknown": func() error { return inv.MarkUnknown("y", t0) }, "dispatch": func() error { return inv.Dispatch(t0) },
	} {
		if err := op(); !errors.Is(err, mcpproxy.ErrInvalidTransition) {
			t.Errorf("unknown then %s: %v (an unknown outcome is final)", name, err)
		}
	}
	if inv.State != mcpproxy.InvocationUnknown || inv.Code != "deadline" {
		t.Fatalf("outcome rewritten: %+v", inv)
	}
}

func Test_tool_policy(t *testing.T) {
	r := newReg(t)
	max := int64(3)
	p := mcpproxy.NewToolPolicy([]string{"mcp_t033_a/echo", "echo", "*", "mcp_t033_a/*", "mcp_t033_a/hidden", "mcp_t033_other/add"}, &max, true)
	if p.Names() != 3 {
		t.Fatalf("bare names and wildcards must grant nothing, got %d granted", p.Names())
	}
	if !mcpproxy.Visible(r, p, "echo") {
		t.Error("echo is enabled and pinned")
	}
	if mcpproxy.Visible(r, p, "add") {
		t.Error("add is enabled by the registration but not pinned by the profile")
	}
	if mcpproxy.Visible(r, p, "hidden") {
		t.Error("hidden is pinned by the profile but not enabled by the registration")
	}
	if !mcpproxy.AnyVisible(r, p) {
		t.Error("expected a visible tool")
	}
	if mcpproxy.AnyVisible(r, mcpproxy.NewToolPolicy(nil, nil, false)) {
		t.Error("an empty policy exposes nothing")
	}
	del, _ := r.Delete(1, actor, t0)
	if mcpproxy.Visible(del, p, "echo") {
		t.Error("a tombstone exposes nothing")
	}
	if id, tool, err := mcpproxy.ParseQualified("mcp_t033_a/echo"); err != nil || id != "mcp_t033_a" || tool != "echo" {
		t.Fatalf("parse: %v %v %v", id, tool, err)
	}
}
