package composition_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/results"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
)

// outageEvidence is the artifact evidence port with a switch that makes the
// artifact owner unavailable. It wraps the real adapter over real S3.
type outageEvidence struct {
	inner results.Evidence
	down  atomic.Bool
	reads atomic.Int64
}

func (o *outageEvidence) References(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) (map[domain.ArtifactID]results.ArtifactEvidence, error) {
	o.reads.Add(1)
	if o.down.Load() {
		return nil, results.Fail(results.ErrUnavailable, errInjected, "artifact store outage")
	}
	return o.inner.References(ctx, scope, attempt, ids)
}

func (o *outageEvidence) Protect(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) error {
	if o.down.Load() {
		return results.Fail(results.ErrUnavailable, errInjected, "artifact store outage")
	}
	return o.inner.Protect(ctx, scope, attempt, ids)
}

func (e *Env) transitions(a *Attempt) int {
	return e.Count(`SELECT count(*) FROM run_history WHERE run_id = $1 AND subject_kind = 'attempt' AND to_status IN ('succeeded', 'failed')`, string(a.Run.ID))
}

// Test_guarded_result_replay: SD-04, SD-10, PX-10, PX-12, IT-03, IT-06 (acceptance
// `guarded_result_replay`). The real results HTTP handler is composed over the
// guarded execution store: a terminal retry of the accepted hash returns the
// original acknowledgment (acceptedAt included) for successful, failed,
// needs-input and validation-failure receipts, a different hash is
// `409 result_conflict`, and receipt decisions never depend on the artifact
// owner; explicit revocation, expiry and signing rotation deny the callback;
// an acknowledgment is only ever sent for a durable commit. Real PostgreSQL
// (and S3 in the artifact-backed case); no NATS or Git.
func Test_guarded_result_replay(t *testing.T) {
	other := Request(t, Completed(outputsOf("other")))

	t.Run("every accepted terminal receipt replays byte for byte and conflicts on a different hash", func(t *testing.T) {
		e := New(t, Options{})
		cases := []struct {
			name   string
			result runtimev1.Result
			status runtimev1.AttemptStatus
		}{
			{"successful", Completed(outputsOf("v1")), runtimev1.AttemptSucceeded},
			{"failed", Failed(), runtimev1.AttemptFailed},
			{"needs input", NeedsInput(), runtimev1.AttemptFailed},
			{"validation failure", Completed(nil), runtimev1.AttemptFailed}, // declared output missing
		}
		for _, c := range cases {
			a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
			req := Request(t, c.result)
			code, first := e.SubmitHTTP(a, req)
			if code != 200 || decodeAck(t, first).AttemptStatus != c.status {
				t.Fatalf("%s: %d %s", c.name, code, first)
			}
			history, events := e.HistoryRows(a), outbox(e, ScopeA.TenantID)
			for range 3 {
				if code, again := e.SubmitHTTP(a, req); code != 200 || !bytes.Equal(again, first) {
					t.Fatalf("%s: the replay differs:\n%d %s\nvs\n%s", c.name, code, again, first)
				}
			}
			otherHash := other
			if c.name == "successful" {
				otherHash = Request(t, Completed(outputsOf("different")))
			}
			if code, body := e.SubmitHTTP(a, otherHash); code != 409 || errCode(t, body) != runtimev1.CodeResultConflict {
				t.Fatalf("%s: a different hash: %d %s", c.name, code, body)
			}
			if e.HistoryRows(a) != history || outbox(e, ScopeA.TenantID) != events || e.transitions(a) != 1 {
				t.Fatalf("%s: a replay or conflict wrote history or events", c.name)
			}
			if e.GrantRevoked(a) {
				t.Fatalf("%s: accepting the result revoked the grant that serves its receipt", c.name)
			}
			if !e.Persisted(a).Status.Terminal() || e.Aggregate(a).Control(a.ID()).Live() {
				t.Fatalf("%s: execution authority must end with the terminal outcome", c.name)
			}
		}
	})

	// Each denial cause is exercised on its own fixture, so a passing assertion
	// can only be explained by that cause: rotating the signing key first would
	// also make an expiry assertion pass.
	denied := func(t *testing.T, name string, deny func(e *Env, a *Attempt), want func(code int, body []byte) bool) {
		t.Run(name, func(t *testing.T) {
			e := New(t, Options{})
			a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
			req := Request(t, Completed(outputsOf("v1")))
			if code, body := e.SubmitHTTP(a, req); code != 200 {
				t.Fatalf("%d %s", code, body)
			}
			history, events := e.HistoryRows(a), outbox(e, ScopeA.TenantID)
			deny(e, a)
			if code, body := e.SubmitHTTP(a, req); !want(code, body) {
				t.Fatalf("a replay after %s: %d %s", name, code, body)
			}
			if at := e.Persisted(a); at.ResultHash != string(req.ResultHash) || at.Status != execution.AttemptSucceeded {
				t.Fatalf("a denied replay changed the receipt: %s %q", at.Status, at.ResultHash)
			}
			if e.HistoryRows(a) != history || outbox(e, ScopeA.TenantID) != events {
				t.Fatal("a denied replay wrote history or events")
			}
		})
	}
	denied(t, "explicit revocation denies a replay without changing the receipt",
		func(e *Env, a *Attempt) {
			if err := e.Auth.Revoke(context.Background(), a.Scope, a.ID()); err != nil {
				t.Fatal(err)
			}
		},
		func(code int, body []byte) bool {
			return code == 403 && errCode(t, body) == runtimev1.CodeAttemptRevoked
		})
	denied(t, "signing rotation denies a replay without changing the receipt",
		func(e *Env, _ *Attempt) {
			next, err := rtjwt.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Ring.Rotate(next); err != nil {
				t.Fatal(err)
			}
		},
		func(code int, _ []byte) bool { return code == 401 })
	denied(t, "expiry denies a replay without changing the receipt",
		func(e *Env, _ *Attempt) { e.X.Clock.Advance(3 * time.Hour) },
		func(code int, _ []byte) bool { return code == 401 })

	t.Run("a grant revoked after authentication is denied inside the mutation, for a new result and for a receipt replay", func(t *testing.T) {
		e := New(t, Options{})
		req := Request(t, Completed(outputsOf("v1")))
		revokeAfterAuth := func(a *Attempt) {
			var once sync.Once
			e.Ctx.SetAfter(func() {
				once.Do(func() {
					if err := e.Auth.Revoke(context.Background(), a.Scope, a.ID()); err != nil {
						t.Error(err)
					}
				})
			})
		}
		t.Cleanup(func() { e.Ctx.SetAfter(nil) })

		// A new result: authentication succeeded, the grant was then revoked, and
		// execution's own state (the attempt is still running and live) cannot
		// explain the denial: only the repeated session check can.
		fresh := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		before := e.Snap(ScopeA.TenantID)
		commits := e.Commits.Count()
		revokeAfterAuth(fresh)
		if _, err := e.Submit(fresh, req); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("a result after a post-authentication revocation: %v", err)
		}
		e.Ctx.SetAfter(nil)
		if e.Commits.Count() != commits+1 {
			t.Fatalf("the request did not reach the guarded execution mutation: %d commits", e.Commits.Count()-commits)
		}
		if at := e.Persisted(fresh); at.ResultHash != "" || at.Status != execution.AttemptRunning {
			t.Fatalf("a revoked session accepted a result: %s %q", at.Status, at.ResultHash)
		}
		after := e.Snap(ScopeA.TenantID)
		if after.GrantsRevoked != before.GrantsRevoked+1 {
			t.Fatalf("only the explicit revocation may have changed state:\n before %+v\n after  %+v", before, after)
		}

		// An accepted receipt: the grant that serves the receipt is explicitly
		// revoked after authentication; the replay is refused, the receipt stays.
		accepted := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		if _, err := e.Submit(accepted, req); err != nil {
			t.Fatal(err)
		}
		history, events := e.HistoryRows(accepted), outbox(e, ScopeA.TenantID)
		commits = e.Commits.Count()
		revokeAfterAuth(accepted)
		if _, err := e.Submit(accepted, req); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("a receipt replay after a post-authentication revocation: %v", err)
		}
		e.Ctx.SetAfter(nil)
		if e.Commits.Count() != commits+1 {
			t.Fatal("the replay did not reach the guarded execution mutation")
		}
		if at := e.Persisted(accepted); at.ResultHash != string(req.ResultHash) || at.Status != execution.AttemptSucceeded {
			t.Fatalf("the receipt changed: %s %q", at.Status, at.ResultHash)
		}
		if e.HistoryRows(accepted) != history || outbox(e, ScopeA.TenantID) != events {
			t.Fatal("a denied replay wrote history or events")
		}

		// A cancellation that commits after authentication is denied the same way.
		cancelled := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		var once sync.Once
		e.Ctx.SetAfter(func() { once.Do(func() { must(t, e.CancelErr(cancelled)) }) })
		if _, err := e.Submit(cancelled, req); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("a result after a post-authentication cancellation: %v", err)
		}
		e.Ctx.SetAfter(nil)
		if at := e.Persisted(cancelled); at.ResultHash != "" || at.Status != execution.AttemptCancelled {
			t.Fatalf("%s %q", at.Status, at.ResultHash)
		}
	})

	t.Run("concurrent duplicates converge on one acceptance and one acknowledgment", func(t *testing.T) {
		e := New(t, Options{})
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		req := Request(t, Completed(outputsOf("v1")))
		bodies := make([][]byte, 8)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range bodies {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				code, body := e.SubmitHTTP(a, req)
				if code != 200 {
					t.Errorf("%d %s", code, body)
				}
				bodies[i] = body
			}()
		}
		close(start)
		wg.Wait()
		for _, b := range bodies[1:] {
			if !bytes.Equal(b, bodies[0]) {
				t.Fatalf("duplicates disagree:\n%s\nvs\n%s", b, bodies[0])
			}
		}
		if e.transitions(a) != 1 {
			t.Fatalf("%d acceptances", e.transitions(a))
		}
	})

	t.Run("nothing is acknowledged before it is durable, and a lost acknowledgment is unavailable before the replay", func(t *testing.T) {
		e := New(t, Options{})
		req := Request(t, Completed(outputsOf("v1")))

		// Failure before the commit: rolled back, unavailable, then accepted once.
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		before := e.Snap(ScopeA.TenantID)
		for _, inject := range []func(){func() { e.Faults.Commit(2) }, func() { e.Faults.Fail(PointEventAppend) }} {
			inject()
			code, body := e.SubmitHTTP(a, req)
			e.Faults.Clear()
			if code != 503 || errCode(t, body) != domain.CodeUnavailable {
				t.Fatalf("%d %s", code, body)
			}
			if got := e.Snap(ScopeA.TenantID); got != before {
				t.Fatalf("an unacknowledged result left state behind:\n before %+v\n after  %+v", before, got)
			}
		}
		if code, body := e.SubmitHTTP(a, req); code != 200 {
			t.Fatalf("%d %s", code, body)
		}

		// Committed but the acknowledgment was lost: unavailable first, then the
		// replay returns the original acknowledgment with the persisted finish time.
		b := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		e.Faults.Commit(1)
		if code, body := e.SubmitHTTP(b, req); code != 503 || errCode(t, body) != domain.CodeUnavailable {
			t.Fatalf("a lost commit acknowledgment must not read as success: %d %s", code, body)
		}
		e.Faults.Clear()
		persisted := e.Persisted(b)
		if persisted.ResultHash != string(req.ResultHash) || persisted.FinishedAt == nil {
			t.Fatalf("the commit should have applied: %+v", persisted)
		}
		code, body := e.SubmitHTTP(b, req)
		if code != 200 || string(decodeAck(t, body).AcceptedAt) != persisted.FinishedAt.UTC().Format(time.RFC3339Nano) {
			t.Fatalf("the replay must repeat the original receipt: %d %s", code, body)
		}
		if e.transitions(b) != 1 {
			t.Fatalf("%d acceptances", e.transitions(b))
		}
	})

	t.Run("receipt decisions survive an artifact outage and need no artifact read", func(t *testing.T) {
		var ev *outageEvidence
		e := New(t, Options{Artifacts: true, Evidence: func(in results.Evidence) results.Evidence { ev = &outageEvidence{inner: in}; return ev }})
		const reportJob = "outputs: {report: {type: artifact, path: /kodama/outputs/report.json}}"
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, reportJob))
		id := e.Upload(a, "report", artifacts.RoleOutput, []byte("report\n"))
		req := Request(t, Completed(map[string]runtimev1.Value{"report": runtimev1.StringValue(string(id))}))
		code, first := e.SubmitHTTP(a, req)
		if code != 200 {
			t.Fatalf("%d %s", code, first)
		}
		ev.down.Store(true)
		reads := ev.reads.Load()
		if code, again := e.SubmitHTTP(a, req); code != 200 || !bytes.Equal(again, first) {
			t.Fatalf("a replay depended on the artifact owner: %d %s", code, again)
		}
		conflict := Request(t, Completed(map[string]runtimev1.Value{"report": runtimev1.StringValue("art_other")}))
		if code, body := e.SubmitHTTP(a, conflict); code != 409 || errCode(t, body) != runtimev1.CodeResultConflict {
			t.Fatalf("a conflict depended on the artifact owner: %d %s", code, body)
		}
		if ev.reads.Load() != reads {
			t.Fatal("a receipt decision read artifacts")
		}
		// A new result fails closed during the outage and never reaches execution.
		b := e.Run(e.NewProject(ScopeA), Job("build", readRun, reportJob))
		attempts := e.Commits.Count()
		if code, body := e.SubmitHTTP(b, req); code != 503 {
			t.Fatalf("%d %s", code, body)
		}
		if e.Commits.Count() != attempts || e.Persisted(b).ResultHash != "" {
			t.Fatal("an artifact outage reached execution")
		}
	})

	t.Run("the service path is the guarded one: a session of a cancelled attempt cannot mutate", func(t *testing.T) {
		e := New(t, Options{})
		a := e.Run(e.NewProject(ScopeA), Job("build", readRun, labelJob))
		if err := e.CancelErr(a); err != nil {
			t.Fatal(err)
		}
		code, body := e.SubmitHTTP(a, Request(t, Completed(outputsOf("v1"))))
		if code != 403 || errCode(t, body) != runtimev1.CodeAttemptRevoked {
			t.Fatalf("%d %s", code, body)
		}
		if _, err := e.Submit(a, Request(t, Completed(outputsOf("v1")))); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatal(err)
		}
		if at := e.Persisted(a); at.ResultHash != "" {
			t.Fatal("a revoked session accepted a result")
		}
	})
}
