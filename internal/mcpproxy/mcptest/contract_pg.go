package mcptest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest/mcpcontract"
)

// PostgresHarness runs mcpcontract.Contract over the real PostgreSQL store, the
// real runtime-authority and secrets modules. It is wired by
// internal/mcpproxy/integration behind layer.SkipService: the unit layer runs
// the memory side (mcptest/memory) and the test environment runs both (SD-A11).
type PostgresHarness struct {
	S   *Stack
	seq int
}

var _ mcpcontract.Harness = (*PostgresHarness)(nil)

// NewPostgresHarness wraps a stack.
func NewPostgresHarness(s *Stack) *PostgresHarness { return &PostgresHarness{S: s} }

func (h *PostgresHarness) Store() mcpproxy.Store           { return h.S.Store }
func (h *PostgresHarness) Checker() mcpproxy.SecretChecker { return h.S.Store }
func (h *PostgresHarness) Scope() domain.Scope             { return h.S.Scope() }
func (h *PostgresHarness) Foreign() domain.Scope {
	return domain.Scope{TenantID: "t_t033_contract_other", ProjectID: "p_t033_contract_other"}
}
func (h *PostgresHarness) Incarnation() string { return h.S.Opts.Incarnation }
func (h *PostgresHarness) Now() time.Time      { return h.S.RT.Clock.Now() }

func (h *PostgresHarness) Advance(_ testing.TB, d time.Duration) { h.S.RT.Clock.Advance(d) }

func (h *PostgresHarness) NewWorkload(t testing.TB, scope domain.Scope) mcpproxy.Caller {
	t.Helper()
	h.seq++
	return h.S.NewWorkload(WorkloadSpec{Scope: &scope, Run: domain.RunID(fmt.Sprintf("run_t033_contract_%d", h.seq))}).Caller
}

func (h *PostgresHarness) NewAttempt(t testing.TB, c mcpproxy.Caller) mcpproxy.Caller {
	t.Helper()
	h.seq++
	return h.S.NewWorkload(WorkloadSpec{Scope: &c.Scope, Run: c.RunID, Attempt: domain.AttemptID(fmt.Sprintf("att_t033_contract_%d", h.seq))}).Caller
}

func (h *PostgresHarness) EndAttempt(t testing.TB, c mcpproxy.Caller) {
	t.Helper()
	if err := h.S.RT.Authority.Cancel(context.Background(), c.Scope, c.AttemptID, true); err != nil {
		t.Fatalf("end the attempt: %v", err)
	}
}

func (h *PostgresHarness) AuthorityOutage(_ testing.TB, on bool) {
	if on {
		h.S.Faults.FailRecheck(errors.New("authority store unreachable"))
		return
	}
	h.S.Faults.FailRecheck(nil)
}

func (h *PostgresHarness) Credential(t testing.TB, scope domain.Scope, name string) mcpproxy.SecretRef {
	t.Helper()
	return h.S.Credential(scope, name, h.S.Fixture.Origin(), h.S.Fixture.Token())
}

func (h *PostgresHarness) RotateCredential(t testing.TB, scope domain.Scope, name string) mcpproxy.SecretRef {
	t.Helper()
	return h.S.RotateCredential(scope, name, h.S.Fixture.Origin(), h.S.Fixture.Token())
}

func (h *PostgresHarness) RevokeCredential(t testing.TB, scope domain.Scope, ref mcpproxy.SecretRef) {
	t.Helper()
	h.S.RevokeCredential(scope, ref.Name, ref.Version)
}

func (h *PostgresHarness) ReleasePins(t testing.TB, c mcpproxy.Caller) {
	t.Helper()
	h.S.RT.OwnerExec(`UPDATE secret_pins SET released_at = now() WHERE consumer_id = $1`, string(c.RunID))
}

func (h *PostgresHarness) Audit(t testing.TB, scope domain.Scope, action string) int {
	t.Helper()
	var n int
	if err := h.S.RT.Owner.QueryRow(context.Background(), `SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = $2`, string(scope.TenantID), action).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}
