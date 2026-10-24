package entrypoint_test

import (
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/entrypoint"
)

// Test_duplicate_result covers PX-10 and AC-12 for the finalization step: the
// result hash is computed once and the identical payload is retried after a
// lost response, the server returns the original acknowledgment, and a
// different result already accepted is never rewritten.
func Test_duplicate_result(t *testing.T) {
	identical := func(t *testing.T, f *fixture, want int) {
		t.Helper()
		var bodies [][]byte
		for _, c := range f.srv.Calls() {
			if c.op == "submitResult" {
				bodies = append(bodies, c.body)
			}
		}
		if len(bodies) != want {
			t.Fatalf("result submissions = %d, want %d; calls %v", len(bodies), want, f.srv.Ops())
		}
		for i, b := range bodies {
			if string(b) != string(bodies[0]) {
				t.Fatalf("submission %d differs from the first:\n%s\n%s", i+1, bodies[0], b)
			}
		}
	}

	t.Run("first_response_lost_identical_retry_returns_original_ack", func(t *testing.T) {
		f := newFixture(t, `echo done`)
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "submitResult" && n == 1 {
				return action{lose: true} // processed and accepted, response dropped
			}
			return action{}
		})
		f.wantExit(f.run(), entrypoint.ExitOK)
		identical(t, f, 2)
		if f.srv.Replays() != 1 {
			t.Fatalf("replays = %d, want the original acknowledgment returned once", f.srv.Replays())
		}
		if r := f.acceptedResult(); r.Outcome != runtimev1.OutcomeCompleted {
			t.Fatalf("result = %+v", r)
		}
		// Retrying the result does not re-upload or re-reserve anything: the
		// command log and its completeness sidecar were reserved once each.
		if n := f.srv.Count("reserveArtifact"); n != 2 {
			t.Fatalf("reserveArtifact calls = %d, want 2 (command log and completeness sidecar)", n)
		}
	})

	t.Run("repeated_loss_is_retried_by_the_step_with_the_same_payload", func(t *testing.T) {
		// More losses than the client's own bounded retries (2): the step keeps
		// resubmitting the identical payload within its finalization budget.
		f := newFixture(t, `echo done`)
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "submitResult" && n <= 3 {
				return action{lose: true}
			}
			return action{}
		})
		f.wantExit(f.run(), entrypoint.ExitOK)
		identical(t, f, 4)
		if f.srv.Replays() != 3 {
			t.Fatalf("replays = %d, want 3", f.srv.Replays())
		}
		if got := f.srv.Accepted(); got == nil {
			t.Fatal("no result accepted")
		}
	})

	t.Run("different_accepted_hash_is_conflict_and_never_rewritten", func(t *testing.T) {
		f := newFixture(t, `echo done`)
		other := runtimev1.Digest("sha256:" + "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5"[:64])
		f.srv.preAccept(other)
		f.wantExit(f.run(), entrypoint.ExitResultConflict)
		if n := f.srv.Count("submitResult"); n != 1 {
			t.Fatalf("submitResult calls = %d: a conflicting result must not be retried", n)
		}
		if got := f.srv.Accepted(); got == nil || got.ResultHash != other {
			t.Fatalf("the accepted result was rewritten: %+v", got)
		}
		if f.srv.Replays() != 0 {
			t.Fatal("a conflict was served as a replay")
		}
	})

	t.Run("conflict_after_lost_response_is_not_hidden", func(t *testing.T) {
		// The first submission is accepted but its response is lost; before the
		// retry another result (different hash) replaces the acceptance view.
		f := newFixture(t, `echo done`)
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op != "submitResult" {
				return action{}
			}
			if n == 1 {
				return action{lose: true}
			}
			return action{pre: func() {
				f.srv.accepted.req.ResultHash = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			}}
		})
		f.wantExit(f.run(), entrypoint.ExitResultConflict)
		if n := f.srv.Count("submitResult"); n != 2 {
			t.Fatalf("submitResult calls = %d, want 2", n)
		}
	})

	t.Run("persistent_server_failure_ends_with_bounded_finalization_failure", func(t *testing.T) {
		f := newFixture(t, `echo done`)
		f.cfg.FinalizeTimeout = 1500 * time.Millisecond
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "submitResult" {
				return action{status: 503}
			}
			return action{}
		})
		began := time.Now()
		f.wantExit(f.run(), entrypoint.ExitFinalization)
		if took := time.Since(began); took > 20*time.Second {
			t.Fatalf("finalization was not bounded: %s", took)
		}
		if f.srv.Accepted() != nil {
			t.Fatal("a result was accepted")
		}
		if n := f.srv.Count("submitResult"); n < 3 {
			t.Fatalf("submitResult calls = %d: the same payload was not retried", n)
		}
		identical(t, f, f.srv.Count("submitResult"))
	})

	t.Run("definitive_rejection_is_replaced_once_by_a_failed_result", func(t *testing.T) {
		f := newFixture(t, `echo done`)
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "submitResult" && n == 1 {
				return action{errCode: runtimev1.CodeOutputInvalid}
			}
			return action{}
		})
		f.wantExit(f.run(), entrypoint.ExitOK)
		r := f.acceptedResult()
		f.wantFailed(r, entrypoint.ReasonResultRejected)
		if f.srv.Count("submitResult") != 2 {
			t.Fatalf("calls = %v", f.srv.Ops())
		}
	})

	t.Run("second_rejection_is_a_finalization_failure", func(t *testing.T) {
		f := newFixture(t, `echo done`)
		f.srv.Intercept(func(op string, n int, body []byte) action {
			if op == "submitResult" {
				return action{errCode: runtimev1.CodeOutputInvalid}
			}
			return action{}
		})
		f.wantExit(f.run(), entrypoint.ExitFinalization)
		if f.srv.Count("submitResult") != 2 || f.srv.Accepted() != nil {
			t.Fatalf("calls = %v", f.srv.Ops())
		}
	})

	t.Run("authentication_failure_on_result_means_authority_lost", func(t *testing.T) {
		for _, code := range []domain.ErrorCode{domain.CodeInvalidCredential, domain.CodeForbidden} {
			f := newFixture(t, `echo done`)
			f.srv.Intercept(func(op string, n int, body []byte) action {
				if op == "submitResult" {
					return action{errCode: code}
				}
				return action{}
			})
			f.wantExit(f.run(), entrypoint.ExitRevoked)
			if f.srv.Count("submitResult") != 1 || f.srv.Accepted() != nil {
				t.Fatalf("%s: calls = %v", code, f.srv.Ops())
			}
		}
	})
}
