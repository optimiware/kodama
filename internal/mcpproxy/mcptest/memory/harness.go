package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest/mcpcontract"
)

// Fixture identities.
var (
	ScopeA = domain.Scope{TenantID: "t_mcp_a", ProjectID: "p_mcp"}
	ScopeB = domain.Scope{TenantID: "t_mcp_b", ProjectID: "p_mcp"}
)

// Harness runs the shared MCP proxy store contract over the memory adapters.
type Harness struct {
	T testing.TB
	W *World
}

var _ mcpcontract.Harness = (*Harness)(nil)

// NewHarness builds an isolated World.
func NewHarness(t testing.TB, o ...Options) *Harness { return &Harness{T: t, W: New(o...)} }

func (h *Harness) Store() mcpproxy.Store                 { return h.W.Store }
func (h *Harness) Checker() mcpproxy.SecretChecker       { return h.W.Store }
func (h *Harness) Scope() domain.Scope                   { return ScopeA }
func (h *Harness) Foreign() domain.Scope                 { return ScopeB }
func (h *Harness) Incarnation() string                   { return h.W.Opts.Incarnation }
func (h *Harness) Now() time.Time                        { return h.W.Clock.Now() }
func (h *Harness) Advance(_ testing.TB, d time.Duration) { h.W.Clock.Advance(d) }

func fatal(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// NewWorkload implements mcpcontract.Harness.
func (h *Harness) NewWorkload(t testing.TB, scope domain.Scope) mcpproxy.Caller {
	t.Helper()
	n := h.W.seq.Add(1)
	w, err := h.W.Authority.NewWorkload(context.Background(), scope, domain.RunID(fmt.Sprintf("run_mcp_%d", n)), domain.AttemptID(fmt.Sprintf("att_mcp_%d", n)))
	fatal(t, err)
	return w.Caller
}

// NewAttempt implements mcpcontract.Harness.
func (h *Harness) NewAttempt(t testing.TB, c mcpproxy.Caller) mcpproxy.Caller {
	t.Helper()
	n := h.W.seq.Add(1)
	w, err := h.W.Authority.NewWorkload(context.Background(), c.Scope, c.RunID, domain.AttemptID(fmt.Sprintf("att_mcp_%d", n)))
	fatal(t, err)
	return w.Caller
}

// EndAttempt implements mcpcontract.Harness.
func (h *Harness) EndAttempt(t testing.TB, c mcpproxy.Caller) {
	t.Helper()
	fatal(t, h.W.Authority.End(context.Background(), c.Scope, c.AttemptID))
}

// AuthorityOutage implements mcpcontract.Harness.
func (h *Harness) AuthorityOutage(_ testing.TB, on bool) {
	if on {
		h.W.Authority.Fail(ErrOutage())
		return
	}
	h.W.Authority.Fail(nil)
}

// Credential implements mcpcontract.Harness.
func (h *Harness) Credential(t testing.TB, scope domain.Scope, name string) mcpproxy.SecretRef {
	t.Helper()
	ref, err := h.W.Secrets.Create(context.Background(), scope, name, DefaultOrigin, "bearer-"+name)
	fatal(t, err)
	return ref
}

// RotateCredential implements mcpcontract.Harness.
func (h *Harness) RotateCredential(t testing.TB, scope domain.Scope, name string) mcpproxy.SecretRef {
	t.Helper()
	ref, err := h.W.Secrets.Rotate(context.Background(), scope, name, DefaultOrigin, "bearer-rotated")
	fatal(t, err)
	return ref
}

// RevokeCredential implements mcpcontract.Harness.
func (h *Harness) RevokeCredential(t testing.TB, scope domain.Scope, ref mcpproxy.SecretRef) {
	t.Helper()
	fatal(t, h.W.Secrets.Revoke(context.Background(), scope, ref))
}

// ReleasePins implements mcpcontract.Harness.
func (h *Harness) ReleasePins(t testing.TB, c mcpproxy.Caller) {
	t.Helper()
	fatal(t, h.W.Secrets.ReleasePins(context.Background(), c.Scope, c.RunID))
}

// Audit implements mcpcontract.Harness.
func (h *Harness) Audit(t testing.TB, scope domain.Scope, action string) int {
	t.Helper()
	rows, err := h.W.Audit(context.Background(), scope.TenantID)
	fatal(t, err)
	n := 0
	for _, r := range rows {
		if r.Action == action {
			n++
		}
	}
	return n
}
