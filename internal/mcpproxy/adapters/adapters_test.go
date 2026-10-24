package adapters_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/adapters"
	"github.com/optimiware/kodama/internal/secrets"
)

var scope = domain.Scope{TenantID: "t033_acme", ProjectID: "p033_inventory"}

// ---- identity ----

type reader struct{ bindings []identity.Binding }

func (r reader) CurrentBindings(_ context.Context, tenant domain.TenantID, user identity.UserID) ([]identity.Binding, error) {
	var out []identity.Binding
	for _, b := range r.bindings {
		if b.Tenant == tenant && b.User == user {
			out = append(out, b)
		}
	}
	return out, nil
}

func humans(t *testing.T, bindings ...identity.Binding) *adapters.HumanAuthority {
	t.Helper()
	rd := reader{bindings: bindings}
	auth, err := identity.NewStandaloneAuthority(rd)
	if err != nil {
		t.Fatal(err)
	}
	h, err := adapters.NewHumanAuthority(auth, rd)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func grant(t *testing.T, user string, role identity.Role) identity.Binding {
	t.Helper()
	b, err := identity.Grant(identity.BindingID("b_"+user), scope.TenantID, identity.UserID(user), scope.ProjectID, role, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func Test_management_permission_mapping(t *testing.T) {
	h := humans(t, grant(t, "u_admin", identity.RoleAdmin), grant(t, "u_viewer", identity.RoleViewer), grant(t, "u_engineer", identity.RoleEngineer))
	ctx := context.Background()
	cases := []struct {
		user string
		perm mcpproxy.Permission
		want error
	}{
		{"u_admin", mcpproxy.PermList, nil},
		{"u_admin", mcpproxy.PermMutate, nil},
		{"u_viewer", mcpproxy.PermList, nil},
		{"u_viewer", mcpproxy.PermMutate, mcpproxy.ErrDenied},
		// An engineer holds credential:read but neither project:settings nor credential:manage.
		{"u_engineer", mcpproxy.PermMutate, mcpproxy.ErrDenied},
		{"u_stranger", mcpproxy.PermList, mcpproxy.ErrNotFound},
		{"u_admin", mcpproxy.Permission("mcp_server.unknown"), mcpproxy.ErrDenied},
	}
	for _, c := range cases {
		if err := h.Authorize(ctx, c.user, scope, c.perm); !errors.Is(err, c.want) && !(c.want == nil && err == nil) {
			t.Errorf("%s %s: got %v, want %v", c.user, c.perm, err, c.want)
		}
	}
	other := domain.Scope{TenantID: scope.TenantID, ProjectID: "p033_other"}
	if err := h.Authorize(ctx, "u_admin", other, mcpproxy.PermList); !errors.Is(err, mcpproxy.ErrNotFound) {
		t.Errorf("a project binding does not cover another project: %v", err)
	}
}

// ---- credentials ----

func Test_credential_payload_is_strict(t *testing.T) {
	rec, tok, err := adapters.DecodeCredential(adapters.EncodeCredential("https://mcp.example.com", "tok-123"))
	if err != nil || rec != "https://mcp.example.com" || string(tok) != "tok-123" {
		t.Fatalf("round trip: %q %q %v", rec, tok, err)
	}
	for name, raw := range map[string]string{
		"empty":           ``,
		"unknown field":   `{"version":1,"recipient":"https://a","token":"t","extra":1}`,
		"wrong version":   `{"version":2,"recipient":"https://a","token":"t"}`,
		"no recipient":    `{"version":1,"token":"t"}`,
		"no token":        `{"version":1,"recipient":"https://a"}`,
		"whitespace":      `{"version":1,"recipient":"https://a","token":"a b"}`,
		"non ascii":       `{"version":1,"recipient":"https://a","token":"tök"}`,
		"trailing":        `{"version":1,"recipient":"https://a","token":"t"} {}`,
		"not json":        `token`,
		"control":         "{\"version\":1,\"recipient\":\"https://a\",\"token\":\"a\\u0000b\"}",
		"oversized token": `{"version":1,"recipient":"https://a","token":"` + string(make([]byte, 5000)) + `"}`,
	} {
		if _, _, err := adapters.DecodeCredential([]byte(raw)); !errors.Is(err, mcpproxy.ErrCredential) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

type fakeSource struct {
	value    string
	openErr  error
	finalErr error
	finished int
	opened   int
	discard  int
}

type fakePending struct {
	src *fakeSource
	val []byte
}

func (s *fakeSource) Open(_ context.Context, _ secrets.VersionRequest) (secrets.PendingValue, error) {
	s.opened++
	if s.openErr != nil {
		return nil, s.openErr
	}
	return &fakePending{src: s, val: []byte(s.value)}, nil
}
func (p *fakePending) Value() []byte { return p.val }
func (p *fakePending) Discard()      { p.src.discard++; clear(p.val) }
func (p *fakePending) Finalize(context.Context, secrets.FinalizeInfo) error {
	if p.src.finalErr != nil {
		return p.src.finalErr
	}
	p.src.finished++
	return nil
}

func request() mcpproxy.CredentialRequest {
	reg, _ := mcpproxy.NewRegistration(scope, "mcp_t033_a", mcpproxy.Draft{Endpoint: "https://mcp.example.com/mcp", Secret: mcpproxy.SecretRef{Name: "remote-token", Version: 1}, Tools: []string{"echo"}},
		mcpproxy.Actor{ID: "u"}, time.Now())
	return mcpproxy.CredentialRequest{Caller: mcpproxy.Caller{Scope: scope, RunID: "run_t033", AttemptID: "att_t033", Session: "rts"}, Registration: reg, RequestID: "req_t033"}
}

func Test_credentials_open(t *testing.T) {
	ctx := context.Background()
	good := string(adapters.EncodeCredential("https://mcp.example.com", "tok"))
	t.Run("releases only after finalization and clears its buffers", func(t *testing.T) {
		src := &fakeSource{value: good}
		c, _ := adapters.NewCredentials(src)
		cred, err := c.Open(ctx, request())
		if err != nil || string(cred.Bearer()) != "tok" || cred.Recipient() != "https://mcp.example.com" {
			t.Fatalf("%v", err)
		}
		if src.finished != 1 || src.discard != 1 {
			t.Fatalf("finalized %d discarded %d", src.finished, src.discard)
		}
		cred.Discard()
		if string(cred.Bearer()) == "tok" {
			t.Fatal("the credential buffer was not cleared")
		}
	})
	t.Run("a recipient that is not the approved origin releases nothing", func(t *testing.T) {
		src := &fakeSource{value: string(adapters.EncodeCredential("https://other.example.com", "tok"))}
		c, _ := adapters.NewCredentials(src)
		if _, err := c.Open(ctx, request()); !errors.Is(err, mcpproxy.ErrCredential) {
			t.Fatalf("%v", err)
		}
		if src.finished != 0 {
			t.Fatal("finalized a mis-addressed credential")
		}
	})
	t.Run("failures are classified and release nothing", func(t *testing.T) {
		for name, tc := range map[string]struct {
			src  *fakeSource
			want error
		}{
			"revoked":                {&fakeSource{value: good, openErr: secrets.ErrRevoked}, mcpproxy.ErrCredential},
			"not pinned":             {&fakeSource{value: good, openErr: secrets.ErrNotPinned}, mcpproxy.ErrCredential},
			"missing":                {&fakeSource{value: good, openErr: secrets.ErrNotFound}, mcpproxy.ErrCredential},
			"pins released":          {&fakeSource{value: good, openErr: secrets.ErrAttemptInactive}, mcpproxy.ErrCredential},
			"key provider down":      {&fakeSource{value: good, openErr: &secrets.KeyError{Class: secrets.KeyUnavailable, Op: "unwrap"}}, mcpproxy.ErrUnavailable},
			"finalization uncertain": {&fakeSource{value: good, finalErr: secrets.ErrFinalizationUncertain}, mcpproxy.ErrUnavailable},
			"finalization revoked":   {&fakeSource{value: good, finalErr: secrets.ErrRevoked}, mcpproxy.ErrCredential},
			"malformed payload":      {&fakeSource{value: "not json"}, mcpproxy.ErrCredential},
		} {
			c, _ := adapters.NewCredentials(tc.src)
			cred, err := c.Open(ctx, request())
			if !errors.Is(err, tc.want) || cred != nil {
				t.Errorf("%s: %v %v", name, cred, err)
			}
		}
	})
	t.Run("another project's registration is refused before any read", func(t *testing.T) {
		src := &fakeSource{value: good}
		c, _ := adapters.NewCredentials(src)
		req := request()
		req.Caller.Scope.ProjectID = "p033_other"
		if _, err := c.Open(ctx, req); !errors.Is(err, mcpproxy.ErrCredential) || src.opened != 0 {
			t.Fatalf("%v opened %d", err, src.opened)
		}
	})
}

// ---- policy ----

type steps struct {
	step string
	err  error
}

func (s steps) StepOf(context.Context, domain.Scope, domain.RunID, domain.AttemptID) (string, error) {
	return s.step, s.err
}

type policyReader struct {
	pol agents.Policy
	err error
	got string
}

func (r *policyReader) PolicyOf(_ context.Context, _ domain.Scope, _ domain.RunID, step string, _ domain.AttemptID) (agents.Policy, error) {
	r.got = step
	return r.pol, r.err
}

func Test_policy_bridge(t *testing.T) {
	max := int64(7)
	caller := mcpproxy.Caller{Scope: scope, RunID: "run_t033", AttemptID: "att_t033", Session: "rts"}
	pr := &policyReader{pol: agents.Policy{Tools: []string{"mcp_t033_a/echo", "echo"}, Limits: agents.Limits{MaxToolCalls: &max}, Transcript: agents.Transcript{Record: true}}}
	p, err := adapters.NewPolicy(steps{step: "implement"}, pr)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.PolicyOf(context.Background(), caller)
	if err != nil || pr.got != "implement" || got.Names() != 1 || got.MaxToolCalls == nil || *got.MaxToolCalls != 7 || !got.RecordTranscript {
		t.Fatalf("policy %+v step %q err %v", got, pr.got, err)
	}
	for name, tc := range map[string]struct {
		s    steps
		r    *policyReader
		want error
	}{
		"unknown attempt":   {steps{err: execution.ErrNotFound}, &policyReader{}, mcpproxy.ErrNotFound},
		"hidden attempt":    {steps{err: execution.ErrHidden}, &policyReader{}, mcpproxy.ErrNotFound},
		"non agent attempt": {steps{step: "build"}, &policyReader{err: agents.ErrNotFound}, mcpproxy.ErrNotFound},
		"execution outage":  {steps{err: errors.New("db down")}, &policyReader{}, mcpproxy.ErrUnavailable},
		"policy outage":     {steps{step: "build"}, &policyReader{err: agents.ErrUnavailable}, mcpproxy.ErrUnavailable},
	} {
		p, _ := adapters.NewPolicy(tc.s, tc.r)
		if _, err := p.PolicyOf(context.Background(), caller); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := adapters.NewPolicy(nil, pr); err == nil {
		t.Error("a missing step resolver must fail closed")
	}
}

// ---- approvals ----

type regs struct {
	m map[mcpproxy.RegistrationID]mcpproxy.Registration
}

func (r regs) GetRegistration(_ context.Context, s domain.Scope, id mcpproxy.RegistrationID) (mcpproxy.Registration, error) {
	reg, ok := r.m[id]
	if !ok || reg.Scope != s {
		return mcpproxy.Registration{}, mcpproxy.ErrNotFound
	}
	return reg, nil
}

func Test_tool_approvals(t *testing.T) {
	reg := request().Registration
	a, err := adapters.NewToolApprovals(regs{m: map[mcpproxy.RegistrationID]mcpproxy.Registration{reg.ID: reg}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.ApproveTools(ctx, scope, []string{string(reg.ID) + "/echo"}); err != nil {
		t.Fatalf("approved tool: %v", err)
	}
	for name, tools := range map[string][]string{
		"bare name": {"echo"}, "wildcard": {string(reg.ID) + "/*"}, "not enabled": {string(reg.ID) + "/mutate"},
		"unknown registration": {"mcp_t033_zzz/echo"}, "one bad among good": {string(reg.ID) + "/echo", "echo"},
	} {
		if err := a.ApproveTools(ctx, scope, tools); !errors.Is(err, agents.ErrDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
	other := domain.Scope{TenantID: scope.TenantID, ProjectID: "p033_other"}
	if err := a.ApproveTools(ctx, other, []string{string(reg.ID) + "/echo"}); !errors.Is(err, agents.ErrDenied) {
		t.Errorf("another project's registration: %v", err)
	}
	c := adapters.ComposeApprovals{Tools: a}
	if err := c.ApproveModel(ctx, scope, agents.Model{}); !errors.Is(err, agents.ErrUnavailable) {
		t.Errorf("a missing model approval must fail closed: %v", err)
	}
	if err := (adapters.ComposeApprovals{}).ApproveTools(ctx, scope, []string{"x/y"}); !errors.Is(err, agents.ErrUnavailable) {
		t.Errorf("a missing tool approval must fail closed: %v", err)
	}
}
