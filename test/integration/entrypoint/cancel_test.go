package entrypoint_test

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
)

var treePIDs = []string{"leader", "child", "grand"}

// Test_cancel_child_tree covers PX-08 and PX-A06: when the attempt's authority
// ends (revoked, refresh denied) or the command exceeds its timeout, the whole
// process tree dies, no source/result/artifact action follows the revocation,
// and a result is submitted only where the contract allows it.
func Test_cancel_child_tree(t *testing.T) {
	t.Run("revoked_by_refresh_attempt_revoked", func(t *testing.T) {
		for _, ignoreTerm := range []bool{false, true} {
			name := "term_honored"
			if ignoreTerm {
				name = "term_ignored_needs_sigkill"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t, treeScript(ignoreTerm))
				f.fastRenewal()
				f.declare("count", runtimev1.OutputInteger)
				wait := f.start(context.Background())
				pids := f.waitPIDs(treePIDs...)
				for n, pid := range pids {
					if !alive(pid) {
						t.Fatalf("%s (%d) is not running before the revocation", n, pid)
					}
				}
				before := len(f.srv.Calls())
				began := time.Now()
				f.srv.Revoke()
				code := wait()
				f.wantExit(code, entrypoint.ExitRevoked)
				if took := time.Since(began); took > 15*time.Second {
					t.Fatalf("revocation took %s to stop the tree", took)
				}
				wantDead(t, pids)

				// Nothing was finalized: no artifact, source or result action, no
				// accepted result; only token renewals reached the revoked server.
				if f.srv.Accepted() != nil {
					t.Fatal("a result was accepted for a revoked attempt")
				}
				for _, op := range f.srv.CallsAfterRevoke() {
					if op != "renewToken" {
						t.Fatalf("action %q after revocation; calls %v", op, f.srv.Ops())
					}
				}
				if got := f.srv.Ops()[:before]; len(got) < 2 || got[0] != "admit" || got[1] != "getContext" {
					t.Fatalf("calls before revocation = %v", got)
				}
				if f.srv.Count("submitResult")+f.srv.Count("reserveArtifact")+f.srv.Count("proposeSource") != 0 {
					t.Fatalf("finalization was attempted: %v", f.srv.Ops())
				}
			})
		}
	})

	t.Run("refresh_denied_by_any_authority_error", func(t *testing.T) {
		for _, code := range []domain.ErrorCode{runtimev1.CodeAttemptRevoked, domain.CodeInvalidCredential, domain.CodeForbidden} {
			t.Run(string(code), func(t *testing.T) {
				f := newFixture(t, treeScript(false))
				f.fastRenewal()
				var armed atomic.Bool
				f.srv.Intercept(func(op string, n int, body []byte) action {
					if op == "renewToken" && armed.Load() {
						return action{errCode: code}
					}
					return action{}
				})
				wait := f.start(context.Background())
				pids := f.waitPIDs(treePIDs...)
				armed.Store(true)
				f.wantExit(wait(), entrypoint.ExitRevoked)
				wantDead(t, pids)
				if f.srv.Accepted() != nil || f.srv.Count("submitResult") != 0 {
					t.Fatalf("finalization after a denied refresh: %v", f.srv.Ops())
				}
			})
		}
	})

	t.Run("timeout_kills_tree_and_reports_failed", func(t *testing.T) {
		f := newFixture(t, treeScript(true))
		f.srv.ctx.Limits.TimeoutSeconds = 1
		f.declare("count", runtimev1.OutputInteger)
		code := f.run()
		f.wantExit(code, entrypoint.ExitOK)
		pids := f.waitPIDs(treePIDs...)
		wantDead(t, pids)
		r := f.acceptedResult()
		f.wantFailed(r, entrypoint.ReasonTimeout)
		if r.ExitCode != nil {
			t.Fatalf("exit code = %d for a killed command, want null", *r.ExitCode)
		}
		if _, ok := f.diagnostics(r)["entrypoint.log"]; !ok {
			t.Fatal("the timeout is not explained in the diagnostics")
		}
	})

	t.Run("entrypoint_interrupt_kills_tree_and_reports_interrupted", func(t *testing.T) {
		f := newFixture(t, treeScript(false))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		wait := f.start(ctx)
		pids := f.waitPIDs(treePIDs...)
		cancel()
		f.wantExit(wait(), entrypoint.ExitOK)
		wantDead(t, pids)
		f.wantFailed(f.acceptedResult(), entrypoint.ReasonInterrupted)
	})

	t.Run("command_exit_reaps_orphaned_grandchildren", func(t *testing.T) {
		// The leader exits at once and leaves background processes behind.
		f := newFixture(t, `sleep 300 &
echo $! > "$PIDDIR/child"
sleep 1 &
exit 0`)
		f.wantExit(f.run(), entrypoint.ExitOK)
		pids := f.waitPIDs("child")
		wantDead(t, pids)
		if r := f.acceptedResult(); r.Outcome != runtimev1.OutcomeCompleted {
			t.Fatalf("result = %+v", r)
		}
	})

	t.Run("revoked_during_secret_delivery_never_launches", func(t *testing.T) {
		f := newFixture(t, `touch "$PIDDIR/ran"`)
		f.secret("erp-token", "secret://project/erp-token", secretValue, runtimev1.DeliveryFile)
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "resolveSecret" {
				return action{errCode: runtimev1.CodeAttemptRevoked, pre: func() { f.srv.revoked = true }}
			}
			return action{}
		})
		f.wantExit(f.run(), entrypoint.ExitRevoked)
		f.assertNotLaunched()
		if got := f.srv.Ops(); len(got) != 3 || got[2] != "resolveSecret" {
			t.Fatalf("calls = %v", got)
		}
	})

	t.Run("revoked_during_finalization_stops_all_actions", func(t *testing.T) {
		f := newFixture(t, `echo hi`)
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "reserveArtifact" && n == 1 {
				return action{errCode: runtimev1.CodeAttemptRevoked, pre: func() { f.srv.revoked = true }}
			}
			return action{}
		})
		f.wantExit(f.run(), entrypoint.ExitRevoked)
		if f.srv.Accepted() != nil || f.srv.Count("submitResult") != 0 || f.srv.Count("reserveArtifact") != 1 {
			t.Fatalf("calls = %v", f.srv.Ops())
		}
	})

	t.Run("late_result_after_revocation_is_not_retried", func(t *testing.T) {
		f := newFixture(t, `echo hi`)
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "submitResult" {
				return action{errCode: runtimev1.CodeAttemptRevoked, pre: func() { f.srv.revoked = true }}
			}
			return action{}
		})
		f.wantExit(f.run(), entrypoint.ExitRevoked)
		if f.srv.Accepted() != nil || f.srv.Count("submitResult") != 1 {
			t.Fatalf("calls = %v", f.srv.Ops())
		}
	})
}

// assertNotLaunched fails if the command wrote its marker file.
func (f *fixture) assertNotLaunched() {
	f.t.Helper()
	if _, err := os.Stat(f.pids + "/ran"); err == nil {
		f.t.Fatal("the command was launched")
	}
}
