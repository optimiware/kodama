package memory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest/memory"
)

var bg = context.Background()

const token = "bearer-secret-token"

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// rig is a World with one project, one registered remote server and one
// attempt whose pinned profile names some of its tools.
type rig struct {
	t      *testing.T
	w      *memory.World
	scope  domain.Scope
	wl     memory.Workload
	reg    mcpproxy.Registration
	ref    mcpproxy.SecretRef
	caller mcpproxy.Caller
}

func manage() mcpproxy.ManageCaller {
	return mcpproxy.ManageCaller{PrincipalID: memory.Admin, RequestID: "req_unit"}
}

// newRig registers a server enabling regTools and pins the qualified pinned
// tools (and an optional ceiling) in the attempt's profile. Every remote tool
// answers "ok" unless a test scripts it.
func newRig(t *testing.T, w *memory.World, regTools, pinned []string, max *int64) *rig {
	t.Helper()
	r := &rig{t: t, w: w, scope: memory.ScopeA}
	r.ref = mustRef(t, w, r.scope, "remote-token")
	w.Humans.Grant(memory.Admin, r.scope, mcpproxy.PermList, mcpproxy.PermMutate)
	reg, err := w.Service.Create(bg, r.scope, manage(), mcpproxy.Draft{Endpoint: memory.DefaultEndpoint, Secret: r.ref, Tools: regTools})
	must(t, err)
	r.reg = reg
	r.attempt("att_unit_1", "run_unit_1", pinned, max)
	for _, name := range []string{"echo", "add", "hidden", "mutate", "fail"} {
		w.Remote.Script(name, memory.Tool{})
	}
	return r
}

func mustRef(t *testing.T, w *memory.World, scope domain.Scope, name string) mcpproxy.SecretRef {
	t.Helper()
	ref, err := w.Secrets.Create(bg, scope, name, memory.DefaultOrigin, token)
	must(t, err)
	return ref
}

// attempt admits an attempt of the run and pins its profile.
func (r *rig) attempt(attempt domain.AttemptID, run domain.RunID, pinned []string, max *int64) mcpproxy.Caller {
	r.t.Helper()
	wl, err := r.w.Authority.NewWorkload(bg, r.scope, run, attempt)
	must(r.t, err)
	var q []string
	for _, p := range pinned {
		q = append(q, mcpproxy.Qualify(r.reg.ID, p))
	}
	r.w.Policies.Set(attempt, q, max, true)
	if r.caller == (mcpproxy.Caller{}) {
		r.wl, r.caller = wl, wl.Caller
	}
	return wl.Caller
}

func (r *rig) open() mcpproxy.SessionID {
	r.t.Helper()
	id, err := r.w.Service.OpenSession(bg, r.caller, r.reg.ID)
	must(r.t, err)
	return id
}

func (r *rig) invoke(id mcpproxy.SessionID, req, tool string) (mcpproxy.RemoteResult, error) {
	return r.w.Service.Invoke(bg, r.caller, r.reg.ID, id, mcpproxy.InvokeRequest{ID: mcpproxy.RequestID(req), Tool: tool, Arguments: json.RawMessage(`{"v":1}`)})
}

func (r *rig) receipt(id mcpproxy.SessionID, req string) mcpproxy.Invocation {
	r.t.Helper()
	inv, err := r.w.Store.GetInvocation(bg, r.scope, id, mcpproxy.RequestID(req))
	must(r.t, err)
	return inv
}

func (r *rig) noReceipt(id mcpproxy.SessionID, req string) {
	r.t.Helper()
	if _, err := r.w.Store.GetInvocation(bg, r.scope, id, mcpproxy.RequestID(req)); !errors.Is(err, mcpproxy.ErrNotFound) {
		r.t.Fatalf("a refused call %s left a receipt: %v", req, err)
	}
}

func (r *rig) calls() int64 {
	r.t.Helper()
	n, err := r.w.Calls(bg, r.scope, r.caller.RunID)
	must(r.t, err)
	return n
}

func outcome(t *testing.T, err error, kind error, state mcpproxy.InvocationState, code string) {
	t.Helper()
	var oe *mcpproxy.OutcomeError
	if !errors.As(err, &oe) || !errors.Is(err, kind) || oe.State != state || (code != "" && oe.Code != code) {
		t.Fatalf("want %v/%s/%s, got %v", kind, state, code, err)
	}
}

// TestUnitToolAuthority (GM-07..GM-12, AR-04, SD-05): a tool is reachable only
// in the intersection of the registration's enabled tools and the attempt's
// pinned profile, only by the exact caller, only while its authority, session,
// registration revision and exact credential version are current, and only
// within the run's ceiling. Foreign and disallowed tools are not found, and
// every failure class is accounted for without claiming a rollback.
func TestUnitToolAuthority(t *testing.T) {
	t.Run("management is human, scoped and fails closed", func(t *testing.T) {
		w := memory.New()
		scope := memory.ScopeA
		ref := mustRef(t, w, scope, "remote-token")
		draft := mcpproxy.Draft{Endpoint: memory.DefaultEndpoint, Secret: ref, Tools: []string{"echo"}}
		if _, err := w.Service.Create(bg, scope, manage(), draft); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("an unbound principal: %v", err)
		}
		w.Humans.Grant(memory.Admin, scope, mcpproxy.PermList)
		if _, err := w.Service.Create(bg, scope, manage(), draft); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("a reader creating: %v", err)
		}
		w.Humans.Grant(memory.Admin, scope, mcpproxy.PermMutate)

		bad := draft
		bad.Endpoint = "https://elsewhere.example.test/mcp"
		if _, err := w.Service.Create(bg, scope, manage(), bad); !errors.Is(err, mcpproxy.ErrDestination) {
			t.Fatalf("an unapproved destination: %v", err)
		}
		other, err := w.Secrets.CreateKind(bg, scope, "db-password", "generic")
		must(t, err)
		bad = draft
		bad.Secret = other
		if _, err := w.Service.Create(bg, scope, manage(), bad); !errors.Is(err, mcpproxy.ErrCredential) {
			t.Fatalf("a non-MCP secret: %v", err)
		}
		bad.Secret = mcpproxy.SecretRef{Name: "remote-token", Version: 9}
		if _, err := w.Service.Create(bg, scope, manage(), bad); !errors.Is(err, mcpproxy.ErrCredential) {
			t.Fatalf("a version that does not exist: %v", err)
		}
		foreignRef := mustRef(t, w, memory.ScopeB, "remote-token-b")
		bad.Secret = foreignRef
		if _, err := w.Service.Create(bg, scope, manage(), bad); !errors.Is(err, mcpproxy.ErrCredential) {
			t.Fatalf("another tenant's secret: %v", err)
		}
		if _, err := w.Service.Create(bg, scope, manage(), mcpproxy.Draft{Endpoint: "http://mcp.example.test/mcp", Secret: ref}); !errors.Is(err, mcpproxy.ErrInvalid) {
			t.Fatalf("a plaintext endpoint: %v", err)
		}
		must(t, w.Secrets.Revoke(bg, scope, ref))
		if _, err := w.Service.Create(bg, scope, manage(), draft); !errors.Is(err, mcpproxy.ErrCredential) {
			t.Fatalf("a revoked credential: %v", err)
		}
		if got, err := w.Service.List(bg, scope, manage(), "", 10); err != nil || len(got) != 0 {
			t.Fatalf("failed creations left registrations: %v %v", got, err)
		}
	})

	t.Run("edits and deletion are versioned and end every use", func(t *testing.T) {
		w := memory.New()
		r := newRig(t, w, []string{"echo"}, []string{"echo"}, nil)
		id := r.open()
		if _, err := r.invoke(id, "req_a", "echo"); err != nil {
			t.Fatal(err)
		}
		next, err := w.Service.Edit(bg, r.scope, manage(), r.reg.ID, 1, mcpproxy.Draft{Endpoint: memory.DefaultEndpoint, Secret: r.ref, Tools: []string{"echo", "add"}})
		if err != nil || next.Version != 2 {
			t.Fatalf("edit: %+v %v", next, err)
		}
		if _, err := w.Service.Edit(bg, r.scope, manage(), r.reg.ID, 1, mcpproxy.Draft{Endpoint: memory.DefaultEndpoint, Secret: r.ref}); !errors.Is(err, mcpproxy.ErrVersionConflict) {
			t.Fatalf("a stale edit: %v", err)
		}
		// The old session names a revision that no longer exists.
		if _, err := r.invoke(id, "req_b", "echo"); !errors.Is(err, mcpproxy.ErrSessionInvalid) && !errors.Is(err, mcpproxy.ErrBindingInvalid) {
			t.Fatalf("a session of the old revision: %v", err)
		}
		r.noReceipt(id, "req_b")
		// The run froze the old revision: it is not silently moved to the new one.
		if _, err := w.Service.OpenSession(bg, r.caller, r.reg.ID); !errors.Is(err, mcpproxy.ErrBindingInvalid) {
			t.Fatalf("the run's binding after an edit: %v", err)
		}
		// A new run freezes the new revision.
		c2 := r.attempt("att_unit_2", "run_unit_2", []string{"echo", "add"}, nil)
		id2, err := w.Service.OpenSession(bg, c2, r.reg.ID)
		must(t, err)
		if _, err := w.Service.Invoke(bg, c2, r.reg.ID, id2, mcpproxy.InvokeRequest{ID: "req_c", Tool: "add", Arguments: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		// Deletion is final.
		must(t, w.Service.Delete(bg, r.scope, manage(), r.reg.ID, 2))
		if _, err := w.Service.Invoke(bg, c2, r.reg.ID, id2, mcpproxy.InvokeRequest{ID: "req_d", Tool: "echo", Arguments: json.RawMessage(`{}`)}); err == nil {
			t.Fatal("a deleted registration was invoked")
		}
		if _, err := w.Service.OpenSession(bg, c2, r.reg.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("opening a deleted registration: %v", err)
		}
		if _, err := w.Service.Get(bg, r.scope, manage(), r.reg.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("reading a deleted registration: %v", err)
		}
		if err := w.Service.Delete(bg, r.scope, manage(), r.reg.ID, 3); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("deleting twice: %v", err)
		}
	})

	t.Run("only the intersection of registration and profile is visible", func(t *testing.T) {
		w := memory.New()
		r := newRig(t, w, []string{"echo", "add", "mutate"}, []string{"echo", "add", "hidden"}, nil)
		// Bare names, wildcards and another registration's names grant nothing.
		r.w.Policies.Set(r.caller.AttemptID, []string{
			mcpproxy.Qualify(r.reg.ID, "echo"), mcpproxy.Qualify(r.reg.ID, "add"), mcpproxy.Qualify(r.reg.ID, "hidden"),
			"mutate", string(r.reg.ID) + "/*", "*", "mcp_other/mutate", string(r.reg.ID) + "/",
		}, nil, true)
		id := r.open()
		tools, err := w.Service.Discover(bg, r.caller, r.reg.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tl := range tools {
			names = append(names, tl.Name)
		}
		if strings.Join(names, ",") != "add,echo" {
			t.Fatalf("discovery lists %v, want exactly the intersection add,echo", names)
		}
		for _, hidden := range []string{"hidden", "mutate", "fail", "unknown", "a/b", "../x", ""} {
			req := "req_h_" + fmt.Sprint(len(hidden)) + hidden
			if _, err := r.invoke(id, strings.NewReplacer("/", "_", ".", "_").Replace(req), hidden); !errors.Is(err, mcpproxy.ErrToolHidden) {
				t.Errorf("tool %q: %v", hidden, err)
			}
		}
		// A hidden tool is never dispatched, admitted or charged.
		if n := w.Remote.CallsOf("hidden") + w.Remote.CallsOf("mutate") + w.Remote.CallsOf("fail"); n != 0 || r.calls() != 0 {
			t.Fatalf("hidden tools reached the remote (%d) or were charged (%d)", n, r.calls())
		}
		if _, err := r.invoke(id, "req_ok", "echo"); err != nil {
			t.Fatal(err)
		}

		// A profile that names nothing the registration enables opens no session.
		other := r.attempt("att_unit_9", "run_unit_9", []string{"hidden"}, nil)
		if _, err := w.Service.OpenSession(bg, other, r.reg.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("a profile with no visible tool: %v", err)
		}
		// No pinned policy is no tool at all.
		wl, err := w.Authority.NewWorkload(bg, r.scope, "run_unit_8", "att_unit_8")
		must(t, err)
		if _, err := w.Service.OpenSession(bg, wl.Caller, r.reg.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("an attempt without a pinned policy: %v", err)
		}
	})

	t.Run("a tampered registration value never widens the allow-list", func(t *testing.T) {
		w := memory.New()
		r := newRig(t, w, []string{"echo"}, []string{"echo", "mutate"}, nil)
		id := r.open()
		// Everything a caller can hold of the registration is changed in place.
		got, err := w.Store.GetRegistration(bg, r.scope, r.reg.ID)
		must(t, err)
		got.Tools = append(got.Tools[:0], "mutate")
		r.reg.Tools[0] = "mutate"
		list, err := w.Service.List(bg, r.scope, manage(), "", 10)
		must(t, err)
		list[0].Tools[0] = "mutate"
		if _, err := r.invoke(id, "req_mutate", "mutate"); !errors.Is(err, mcpproxy.ErrToolHidden) {
			t.Fatalf("a tool the registration never enabled: %v", err)
		}
		if _, err := r.invoke(id, "req_echo", "echo"); err != nil {
			t.Fatalf("the registration's tool: %v", err)
		}
		if w.Remote.CallsOf("mutate") != 0 {
			t.Fatalf("a tampered allow-list reached the remote")
		}
		tools, err := w.Service.Discover(bg, r.caller, r.reg.ID, id)
		must(t, err)
		if len(tools) != 1 || tools[0].Name != "echo" {
			t.Fatalf("discovery %v", tools)
		}
		if reg, _ := w.Store.GetRegistration(bg, r.scope, r.reg.ID); reg.Version != 1 {
			t.Fatalf("version %d", reg.Version)
		}
	})

	t.Run("foreign tenants, runs and copied sessions are not found", func(t *testing.T) {
		w := memory.New()
		r := newRig(t, w, []string{"echo"}, []string{"echo"}, nil)
		id := r.open()
		// Another tenant's attempt, even one pinning the same qualified tool.
		fwl, err := w.Authority.NewWorkload(bg, memory.ScopeB, "run_unit_f", "att_unit_f")
		must(t, err)
		w.Policies.Set("att_unit_f", []string{mcpproxy.Qualify(r.reg.ID, "echo")}, nil, true)
		if _, err := w.Service.OpenSession(bg, fwl.Caller, r.reg.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("another tenant opened the registration: %v", err)
		}
		if _, err := w.Service.Invoke(bg, fwl.Caller, r.reg.ID, id, mcpproxy.InvokeRequest{ID: "req_f", Tool: "echo", Arguments: json.RawMessage(`{}`)}); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("another tenant used a copied session: %v", err)
		}
		// Another attempt of the same run, and another run of the same project.
		sibling := r.attempt("att_unit_s", "run_unit_1", []string{"echo"}, nil)
		stranger := r.attempt("att_unit_t", "run_unit_t", []string{"echo"}, nil)
		for name, c := range map[string]mcpproxy.Caller{"sibling attempt": sibling, "another run": stranger} {
			if _, err := w.Service.Invoke(bg, c, r.reg.ID, id, mcpproxy.InvokeRequest{ID: "req_x", Tool: "echo", Arguments: json.RawMessage(`{}`)}); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
				t.Errorf("%s: %v", name, err)
			}
			if _, err := w.Service.Discover(bg, c, r.reg.ID, id); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
				t.Errorf("%s discovery: %v", name, err)
			}
		}
		if w.Remote.CallsOf("echo") != 0 {
			t.Fatalf("a copied session reached the remote")
		}
		// The session also names its registration.
		if _, err := w.Service.Invoke(bg, r.caller, "mcp_other", id, mcpproxy.InvokeRequest{ID: "req_y", Tool: "echo", Arguments: json.RawMessage(`{}`)}); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("a session of another registration: %v", err)
		}
		// Bearers authenticate one attempt only.
		if _, err := w.Service.Authenticate(bg, r.wl.Token, r.caller.AttemptID); err != nil {
			t.Fatalf("the attempt's own bearer: %v", err)
		}
		if _, err := w.Service.Authenticate(bg, r.wl.Token, "att_unit_f"); !errors.Is(err, mcpproxy.ErrUnauthenticated) {
			t.Fatalf("a bearer on another attempt's route: %v", err)
		}
		if _, err := w.Service.Authenticate(bg, "made-up", r.caller.AttemptID); !errors.Is(err, mcpproxy.ErrUnauthenticated) {
			t.Fatalf("an unknown bearer: %v", err)
		}
		// A registration read is project scoped too.
		if _, err := w.Store.GetRegistration(bg, memory.ScopeB, r.reg.ID); !errors.Is(err, mcpproxy.ErrNotFound) {
			t.Fatalf("another tenant read the registration: %v", err)
		}
	})

	t.Run("an ended or unreachable authority dispatches nothing", func(t *testing.T) {
		w := memory.New()
		r := newRig(t, w, []string{"echo"}, []string{"echo"}, nil)
		id := r.open()
		if _, err := r.invoke(id, "req_ok", "echo"); err != nil {
			t.Fatal(err)
		}
		w.Authority.Fail(memory.ErrOutage())
		if _, err := r.invoke(id, "req_outage", "echo"); !errors.Is(err, mcpproxy.ErrUnavailable) {
			t.Fatalf("an authority outage: %v", err)
		}
		r.noReceipt(id, "req_outage")
		w.Authority.Fail(nil)
		// The outage proved nothing: the session works again.
		if _, err := r.invoke(id, "req_back", "echo"); err != nil {
			t.Fatalf("after the outage: %v", err)
		}
		before := w.Remote.CallsOf("echo")

		must(t, w.Authority.End(bg, r.scope, r.caller.AttemptID))
		if _, err := r.invoke(id, "req_ended", "echo"); !errors.Is(err, mcpproxy.ErrDenied) {
			t.Fatalf("an ended attempt: %v", err)
		}
		r.noReceipt(id, "req_ended")
		if _, err := w.Service.Discover(bg, r.caller, r.reg.ID, id); !errors.Is(err, mcpproxy.ErrDenied) {
			t.Fatalf("discovery by an ended attempt: %v", err)
		}
		if _, err := w.Service.Authenticate(bg, r.wl.Token, r.caller.AttemptID); !errors.Is(err, mcpproxy.ErrDenied) {
			t.Fatalf("the bearer of an ended attempt: %v", err)
		}
		if _, err := w.Service.OpenSession(bg, r.caller, r.reg.ID); !errors.Is(err, mcpproxy.ErrDenied) {
			t.Fatalf("a session for an ended attempt: %v", err)
		}
		if got := w.Remote.CallsOf("echo"); got != before {
			t.Fatalf("an ended attempt reached the remote: %d -> %d", before, got)
		}
		// Closing sends no credential: nothing has just confirmed it may be used.
		must(t, w.Service.CloseSession(bg, r.caller, r.reg.ID, id))
		for _, c := range w.Remote.Conns() {
			if c.Terminated() != 0 {
				t.Fatalf("a session of an ended attempt was terminated with its credential")
			}
		}
	})

	t.Run("the exact credential version ends use when revoked, released or rotated away", func(t *testing.T) {
		for name, end := range map[string]func(*rig){
			"revoked":  func(r *rig) { must(t, r.w.Secrets.Revoke(bg, r.scope, r.ref)) },
			"released": func(r *rig) { must(t, r.w.Secrets.ReleasePins(bg, r.scope, r.caller.RunID)) },
		} {
			t.Run(name, func(t *testing.T) {
				w := memory.New()
				r := newRig(t, w, []string{"echo"}, []string{"echo"}, nil)
				id := r.open()
				if _, err := r.invoke(id, "req_ok", "echo"); err != nil {
					t.Fatal(err)
				}
				end(r)
				before := w.Remote.CallsOf("echo")
				if _, err := r.invoke(id, "req_after", "echo"); !errors.Is(err, mcpproxy.ErrCredential) {
					t.Fatalf("after the credential ended: %v", err)
				}
				r.noReceipt(id, "req_after")
				if got := w.Remote.CallsOf("echo"); got != before {
					t.Fatalf("a call used an ended credential")
				}
				sess, err := w.Store.GetSession(bg, r.scope, id)
				must(t, err)
				if sess.State != mcpproxy.SessionInvalid {
					t.Fatalf("a definitive verdict must invalidate the session: %s", sess.State)
				}
				if _, err := r.invoke(id, "req_later", "echo"); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
					t.Fatalf("an invalidated session: %v", err)
				}
				for _, c := range w.Remote.Conns() {
					if c.Terminated() != 0 {
						t.Fatalf("an invalidated session was terminated with its credential")
					}
				}
			})
		}
		t.Run("rotation never changes what the run froze", func(t *testing.T) {
			w := memory.New()
			r := newRig(t, w, []string{"echo"}, []string{"echo"}, nil)
			id := r.open()
			_, err := w.Secrets.Rotate(bg, r.scope, "remote-token", memory.DefaultOrigin, "a-newer-token")
			must(t, err)
			if _, err := r.invoke(id, "req_ok", "echo"); err != nil {
				t.Fatalf("after a rotation: %v", err)
			}
			for _, tg := range w.Remote.Opened() {
				if string(tg.Bearer) != token {
					t.Fatalf("the session was opened with another credential version")
				}
			}
		})
		t.Run("a deleted secret stays usable by the run that pinned it", func(t *testing.T) {
			w := memory.New()
			r := newRig(t, w, []string{"echo"}, []string{"echo"}, nil)
			id := r.open()
			must(t, w.Secrets.Delete(bg, r.scope, "remote-token"))
			if _, err := r.invoke(id, "req_ok", "echo"); err != nil {
				t.Fatalf("a retained pinned version: %v", err)
			}
			// A new run cannot newly bind a deleted secret.
			c2 := r.attempt("att_unit_2", "run_unit_2", []string{"echo"}, nil)
			if _, err := w.Service.OpenSession(bg, c2, r.reg.ID); !errors.Is(err, mcpproxy.ErrCredential) {
				t.Fatalf("binding a deleted secret: %v", err)
			}
		})
		t.Run("a credential addressed to another recipient is never presented", func(t *testing.T) {
			w := memory.New()
			scope := memory.ScopeA
			wrong, err := w.Secrets.Create(bg, scope, "wrong-recipient", "https://evil.example.test", token)
			must(t, err)
			w.Humans.Grant(memory.Admin, scope, mcpproxy.PermList, mcpproxy.PermMutate)
			reg, err := w.Service.Create(bg, scope, manage(), mcpproxy.Draft{Endpoint: memory.DefaultEndpoint, Secret: wrong, Tools: []string{"echo"}})
			must(t, err)
			wl, err := w.Authority.NewWorkload(bg, scope, "run_unit_r", "att_unit_r")
			must(t, err)
			w.Policies.Set("att_unit_r", []string{mcpproxy.Qualify(reg.ID, "echo")}, nil, true)
			if _, err := w.Service.OpenSession(bg, wl.Caller, reg.ID); !errors.Is(err, mcpproxy.ErrCredential) {
				t.Fatalf("a mis-addressed credential: %v", err)
			}
			if n := len(w.Remote.Opened()); n != 0 {
				t.Fatalf("a mis-addressed credential reached the remote (%d opens)", n)
			}
		})
	})

	t.Run("the credential reaches only the approved origin and only the remote", func(t *testing.T) {
		w := memory.New()
		r := newRig(t, w, []string{"echo"}, []string{"echo"}, nil)
		id := r.open()
		if _, err := r.invoke(id, "req_ok", "echo"); err != nil {
			t.Fatal(err)
		}
		opened := w.Remote.Opened()
		if len(opened) != 1 || opened[0].Endpoint != memory.DefaultEndpoint || opened[0].Recipient != memory.DefaultOrigin || string(opened[0].Bearer) != token {
			t.Fatalf("opened %+v", opened)
		}
		// Nothing the workload or an auditor can read carries it.
		rows, err := w.Audit(bg, r.scope.TenantID)
		must(t, err)
		for _, a := range rows {
			if s := fmt.Sprint(a.Meta); strings.Contains(s, token) || strings.Contains(s, `"v":1`) {
				t.Fatalf("audit entry %s leaks content: %s", a.Action, s)
			}
		}
		// A deliberate close by the confirmed caller terminates with the credential.
		must(t, w.Service.CloseSession(bg, r.caller, r.reg.ID, id))
		if got := w.Remote.Conns()[0]; got.Terminated() != 1 || got.Closed() != 1 {
			t.Fatalf("graceful close: terminated=%d closed=%d", got.Terminated(), got.Closed())
		}
		if _, err := r.invoke(id, "req_closed", "echo"); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("a closed session: %v", err)
		}
	})

	t.Run("the run's call ceiling is enforced and refused calls cost nothing", func(t *testing.T) {
		w := memory.New()
		max := int64(2)
		r := newRig(t, w, []string{"echo"}, []string{"echo"}, &max)
		id := r.open()
		for _, req := range []string{"req_1", "req_2"} {
			if _, err := r.invoke(id, req, "echo"); err != nil {
				t.Fatalf("%s: %v", req, err)
			}
		}
		for i := 0; i < 2; i++ {
			if _, err := r.invoke(id, fmt.Sprintf("req_over_%d", i), "echo"); !errors.Is(err, mcpproxy.ErrLimitReached) {
				t.Fatalf("over the ceiling: %v", err)
			}
		}
		if n := w.Remote.CallsOf("echo"); n != 2 || r.calls() != 2 {
			t.Fatalf("remote calls %d, charged %d: refused calls must neither dispatch nor be charged", n, r.calls())
		}
		r.noReceipt(id, "req_over_0")
		// A replay of an admitted call is a duplicate, not a third charge.
		_, err := r.invoke(id, "req_2", "echo")
		outcome(t, err, mcpproxy.ErrDuplicate, mcpproxy.InvocationSucceeded, "ok")
		if n := w.Remote.CallsOf("echo"); n != 2 {
			t.Fatalf("a replayed request dispatched again (%d)", n)
		}
		// The ceiling is run-wide: a sibling attempt and a second session share it.
		sibling := r.attempt("att_unit_s", "run_unit_1", []string{"echo"}, &max)
		sid, err := w.Service.OpenSession(bg, sibling, r.reg.ID)
		must(t, err)
		if _, err := w.Service.Invoke(bg, sibling, r.reg.ID, sid, mcpproxy.InvokeRequest{ID: "req_s", Tool: "echo", Arguments: json.RawMessage(`{}`)}); !errors.Is(err, mcpproxy.ErrLimitReached) {
			t.Fatalf("the sibling attempt escaped the run's ceiling: %v", err)
		}
		// Another run has its own ceiling.
		c2 := r.attempt("att_unit_2", "run_unit_2", []string{"echo"}, &max)
		id2, err := w.Service.OpenSession(bg, c2, r.reg.ID)
		must(t, err)
		if _, err := w.Service.Invoke(bg, c2, r.reg.ID, id2, mcpproxy.InvokeRequest{ID: "req_n", Tool: "echo", Arguments: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("another run: %v", err)
		}
		admitted := 0
		rows, err := w.Audit(bg, r.scope.TenantID)
		must(t, err)
		for _, a := range rows {
			if a.Action == "mcp_tool.admitted" {
				admitted++
			}
		}
		if admitted != 3 {
			t.Fatalf("%d admission audit entries, want 3 (2 + 1)", admitted)
		}
	})

	t.Run("an unclaimed ceiling is unlimited", func(t *testing.T) {
		w := memory.New()
		r := newRig(t, w, []string{"echo"}, []string{"echo"}, nil)
		id := r.open()
		for i := 0; i < 25; i++ {
			if _, err := r.invoke(id, fmt.Sprintf("req_%d", i), "echo"); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
		if r.calls() != 25 {
			t.Fatalf("charged %d", r.calls())
		}
	})

	t.Run("failed tool responses are classified and never retried", func(t *testing.T) {
		w := memory.New()
		r := newRig(t, w, []string{"echo", "fail", "mutate"}, []string{"echo", "fail", "mutate"}, nil)
		id := r.open()

		// A definitive protocol error reply: failed, no rollback claimed.
		w.Remote.Script("fail", memory.Tool{Reply: "invalid_params"})
		_, err := r.invoke(id, "req_reply", "fail")
		outcome(t, err, mcpproxy.ErrRemoteFailed, mcpproxy.InvocationFailed, "invalid_params")
		if got := r.receipt(id, "req_reply"); got.State != mcpproxy.InvocationFailed || got.Code != "invalid_params" {
			t.Fatalf("receipt %+v", got)
		}
		// An error code the remote makes up is bounded to a classified token.
		w.Remote.Script("fail", memory.Tool{Reply: "Bad Code! \x00 with spaces"})
		_, err = r.invoke(id, "req_junk", "fail")
		outcome(t, err, mcpproxy.ErrRemoteFailed, mcpproxy.InvocationFailed, "remote_error")

		// An application error result is a result the remote chose to send.
		w.Remote.Script("echo", memory.Tool{Result: json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"nope"}]}`), IsError: true})
		res, err := r.invoke(id, "req_apperr", "echo")
		if err != nil || !res.IsError {
			t.Fatalf("an application error result is returned as is: %+v %v", res, err)
		}
		if got := r.receipt(id, "req_apperr"); got.State != mcpproxy.InvocationSucceeded {
			t.Fatalf("receipt %+v", got)
		}

		// A lost reply is unknown, final, and never replayed.
		w.Remote.Script("mutate", memory.Tool{Transport: true})
		_, err = r.invoke(id, "req_lost", "mutate")
		outcome(t, err, mcpproxy.ErrOutcomeUnknown, mcpproxy.InvocationUnknown, "transport")
		for i := 0; i < 2; i++ {
			_, err = r.invoke(id, "req_lost", "mutate")
			outcome(t, err, mcpproxy.ErrDuplicate, mcpproxy.InvocationUnknown, "transport")
		}
		if n := w.Remote.CallsOf("mutate"); n != 1 {
			t.Fatalf("a call with an unknown outcome was sent %d times", n)
		}
		if got := r.receipt(id, "req_lost"); got.State != mcpproxy.InvocationUnknown {
			t.Fatalf("receipt %+v", got)
		}

		// An oversized result: the effect happened, the reply cannot be delivered.
		w.Remote.Script("echo", memory.Tool{Result: json.RawMessage(`"` + strings.Repeat("x", 5000) + `"`)})
		_, err = r.invoke(id, "req_big", "echo")
		outcome(t, err, mcpproxy.ErrOutcomeUnknown, mcpproxy.InvocationUnknown, "result_too_large")
		// Oversized arguments are refused before admission: nothing sent or charged.
		before, callsBefore := w.Remote.CallsOf("echo"), r.calls()
		_, err = w.Service.Invoke(bg, r.caller, r.reg.ID, id, mcpproxy.InvokeRequest{ID: "req_args", Tool: "echo", Arguments: json.RawMessage(`"` + strings.Repeat("a", 5000) + `"`)})
		if !errors.Is(err, mcpproxy.ErrTooLarge) || w.Remote.CallsOf("echo") != before || r.calls() != callsBefore {
			t.Fatalf("oversized arguments: %v", err)
		}
		r.noReceipt(id, "req_args")

		// Every dispatched failure is accounted for: one admission and one
		// completion per call, with the classified outcome and no content.
		rows, err := w.Audit(bg, r.scope.TenantID)
		must(t, err)
		completed := map[string]string{}
		for _, a := range rows {
			if a.Action == "mcp_tool.completed" {
				completed[a.RequestID] = fmt.Sprint(a.Meta["outcome"], "/", a.Meta["code"])
			}
		}
		want := map[string]string{
			"req_reply": "failed/invalid_params", "req_junk": "failed/remote_error", "req_apperr": "succeeded/ok",
			"req_lost": "unknown/transport", "req_big": "unknown/result_too_large",
		}
		for k, v := range want {
			if completed[k] != v {
				t.Errorf("completion of %s is %q, want %q", k, completed[k], v)
			}
		}
	})

	t.Run("a call cancelled while the remote holds it is unknown", func(t *testing.T) {
		w := memory.New()
		r := newRig(t, w, []string{"echo"}, []string{"echo"}, nil)
		id := r.open()
		w.Remote.Script("echo", memory.Tool{Hold: true})
		ctx, cancel := context.WithCancel(bg)
		errc := make(chan error, 1)
		go func() {
			_, err := w.Service.Invoke(ctx, r.caller, r.reg.ID, id, mcpproxy.InvokeRequest{ID: "req_hold", Tool: "echo", Arguments: json.RawMessage(`{}`)})
			errc <- err
		}()
		select {
		case <-w.Remote.Started():
		case <-time.After(5 * time.Second):
			t.Fatal("the remote never received the call")
		}
		cancel()
		outcome(t, <-errc, mcpproxy.ErrOutcomeUnknown, mcpproxy.InvocationUnknown, "cancelled")
		if got := r.receipt(id, "req_hold"); got.State != mcpproxy.InvocationUnknown || got.Code != "cancelled" {
			t.Fatalf("receipt %+v", got)
		}
		// Cancelled before admission: nothing is sent and the ceiling is intact.
		dead, stop := context.WithCancel(bg)
		stop()
		before := w.Remote.CallsOf("echo")
		if _, err := w.Service.Invoke(dead, r.caller, r.reg.ID, id, mcpproxy.InvokeRequest{ID: "req_pre", Tool: "echo", Arguments: json.RawMessage(`{}`)}); err == nil {
			t.Fatal("a cancelled call succeeded")
		}
		r.noReceipt(id, "req_pre")
		if w.Remote.CallsOf("echo") != before {
			t.Fatalf("a call cancelled before admission was dispatched")
		}
	})

	t.Run("an admission that may not have committed never dispatches", func(t *testing.T) {
		w := memory.New()
		r := newRig(t, w, []string{"echo"}, []string{"echo"}, nil)
		id := r.open()
		w.SetCommitFault(func() error { return errors.New("commit lost") })
		_, err := r.invoke(id, "req_unc", "echo")
		w.SetCommitFault(nil)
		if !errors.Is(err, mcpproxy.ErrUnavailable) || !errors.Is(err, mcpproxy.ErrAdmissionUncertain) {
			t.Fatalf("an uncertain admission: %v", err)
		}
		if w.Remote.CallsOf("echo") != 0 || r.calls() != 0 {
			t.Fatalf("an uncertain admission dispatched or charged")
		}
		r.noReceipt(id, "req_unc")
		if _, err := r.invoke(id, "req_unc", "echo"); err != nil {
			t.Fatalf("the retry of an admission that did not commit: %v", err)
		}
	})

	t.Run("a lost outcome write is recovered as unknown, never resent", func(t *testing.T) {
		w := memory.New()
		r := newRig(t, w, []string{"echo"}, []string{"echo"}, nil)
		id := r.open()
		var commits atomic.Int32
		// The admission commits; the outcome write that follows the call does not.
		w.SetCommitFault(func() error {
			if commits.Add(1) >= 2 {
				return errors.New("outcome commit lost")
			}
			return nil
		})
		if _, err := r.invoke(id, "req_crash", "echo"); err != nil {
			t.Fatalf("the caller still gets the remote's answer: %v", err)
		}
		w.SetCommitFault(nil)
		if got := r.receipt(id, "req_crash"); got.State != mcpproxy.InvocationDispatching {
			t.Fatalf("receipt %+v", got)
		}
		// Too fresh to recover.
		must(t, w.Service.Sweep(bg, r.scope.TenantID, 10))
		if got := r.receipt(id, "req_crash"); got.State != mcpproxy.InvocationDispatching {
			t.Fatalf("a fresh receipt was recovered: %+v", got)
		}
		w.Clock.Advance(w.Opts.StaleAfter + time.Second)
		must(t, w.Service.Sweep(bg, r.scope.TenantID, 10))
		got := r.receipt(id, "req_crash")
		if got.State != mcpproxy.InvocationUnknown || got.Code != "recovered" {
			t.Fatalf("receipt %+v", got)
		}
		_, err := r.invoke(id, "req_crash", "echo")
		outcome(t, err, mcpproxy.ErrDuplicate, mcpproxy.InvocationUnknown, "recovered")
		if n := w.Remote.CallsOf("echo"); n != 1 {
			t.Fatalf("a recovered call was sent %d times", n)
		}
	})

	t.Run("sessions expire and are never adopted by another process", func(t *testing.T) {
		w := memory.New()
		r := newRig(t, w, []string{"echo"}, []string{"echo"}, nil)
		id := r.open()
		// A restarted process (another incarnation) never adopts the session.
		other := w.Restart("inc-memory-b")
		if _, err := other.Invoke(bg, r.caller, r.reg.ID, id, mcpproxy.InvokeRequest{ID: "req_o", Tool: "echo", Arguments: json.RawMessage(`{}`)}); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("another process used the session: %v", err)
		}
		if sess, _ := w.Store.GetSession(bg, r.scope, id); sess.State != mcpproxy.SessionInvalid {
			t.Fatalf("an orphaned session must be invalidated: %s", sess.State)
		}
		if w.Remote.CallsOf("echo") != 0 {
			t.Fatalf("an orphaned session reached the remote")
		}

		id2 := r.open()
		w.Clock.Advance(w.Opts.SessionTTL + time.Second)
		if _, err := r.invoke(id2, "req_e", "echo"); !errors.Is(err, mcpproxy.ErrSessionInvalid) {
			t.Fatalf("an expired session: %v", err)
		}
		r.noReceipt(id2, "req_e")
		if sess, _ := w.Store.GetSession(bg, r.scope, id2); sess.State != mcpproxy.SessionExpired {
			t.Fatalf("state %s", sess.State)
		}
		for _, c := range w.Remote.Conns() {
			if c.Terminated() != 0 {
				t.Fatalf("an expired session presented its credential")
			}
		}
	})

	t.Run("the pinned protocol and the remote's availability are enforced", func(t *testing.T) {
		w := memory.New()
		r := newRig(t, w, []string{"echo"}, []string{"echo"}, nil)
		w.Remote.SetProtocol("2024-01-01")
		if _, err := w.Service.OpenSession(bg, r.caller, r.reg.ID); !errors.Is(err, mcpproxy.ErrProtocol) {
			t.Fatalf("another protocol revision: %v", err)
		}
		w.Remote.SetProtocol("2025-11-25")
		w.Remote.FailOpen(errors.New("connection refused"))
		if _, err := w.Service.OpenSession(bg, r.caller, r.reg.ID); !errors.Is(err, mcpproxy.ErrUnavailable) {
			t.Fatalf("an unreachable remote: %v", err)
		}
		w.Remote.FailOpen(nil)
		id := r.open()
		if _, err := r.invoke(id, "req_ok", "echo"); err != nil {
			t.Fatal(err)
		}
	})
}
