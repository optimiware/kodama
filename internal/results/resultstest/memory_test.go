package resultstest_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/results"
	rt "github.com/optimiware/kodama/internal/results/resultstest"
)

var bg = context.Background()

func submit(t *testing.T, m *rt.Memory, svc *results.Service, a rt.Attempt, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
	t.Helper()
	return svc.Submit(bg, results.SubmitCommand{Attempt: a.ID, Bearer: m.Bearer(t, a, results.ActionResultSubmit), Request: req})
}

func propose(t *testing.T, m *rt.Memory, svc *results.Service, a rt.Attempt, req runtimev1.SourceProposalRequest) (results.SourceOutcome, error) {
	t.Helper()
	return svc.ProposeSource(bg, results.ProposeCommand{Attempt: a.ID, Bearer: m.Bearer(t, a, results.ActionSourcePropose), Request: req})
}

func report(id domain.ArtifactID) map[string]runtimev1.Value {
	return map[string]runtimev1.Value{"report": runtimev1.StringValue(string(id))}
}

const published = runtimev1.GitCommit("3333333333333333333333333333333333333333")

// writer returns a writer attempt with a bundle and its source proposal.
func writer(t *testing.T, m *rt.Memory) (rt.Attempt, runtimev1.SourceProposalRequest) {
	t.Helper()
	w := m.NewAttempt(t, rt.ScopeA, rt.AttemptOptions{Writer: true, NoOutputs: true})
	bundle := m.Artifact(t, w, rt.ArtifactSpec{Role: runtimev1.RoleBundle})
	return w, runtimev1.SourceProposalRequest{Commit: published, ExpectedTip: rt.BaseCommit, BundleArtifactID: bundle}
}

// Test_unit_results_logs_contract runs the shared Contract of the results ports
// on the memory adapters, with no service (SD-13, SD-A11).
func Test_unit_results_logs_contract(t *testing.T) {
	rt.Contract(t, func(testing.TB) rt.Subject { return rt.NewMemory().Subject() })
}

// Test_unit_result_replay proves duplicate callbacks converge and an unknown
// publication or commit outcome never fabricates success (PX-05, IT-06, AC-06).
func Test_unit_result_replay(t *testing.T) {
	t.Run("a lost acknowledgment is replayed with the original acceptance time", func(t *testing.T) {
		m := rt.NewMemory()
		svc := m.Service(t)
		a := m.NewAttempt(t, rt.ScopeA, rt.AttemptOptions{})
		req := rt.Request(t, rt.Completed(report(m.Artifact(t, a, rt.ArtifactSpec{})), nil))
		m.LoseNextAcknowledgments(1)
		if _, err := submit(t, m, svc, a, req); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("a lost response is unavailable to the caller: %v", err)
		}
		st := m.State(t, a)
		if st.Status != "succeeded" || st.ResultHash != req.ResultHash {
			t.Fatalf("the commit is durable although the response was lost: %+v", st)
		}
		m.Clock.Advance(10 * time.Minute)
		resp, err := submit(t, m, svc, a, req)
		if err != nil || string(resp.AcceptedAt) != st.FinishedAt.UTC().Format(time.RFC3339Nano) {
			t.Fatalf("the replay must repeat the persisted acceptance time: %+v %v (want %s)", resp, err, st.FinishedAt)
		}
	})

	t.Run("an unknown commit outcome acknowledges nothing and the retry decides once", func(t *testing.T) {
		m := rt.NewMemory()
		svc := m.Service(t)
		a := m.NewAttempt(t, rt.ScopeA, rt.AttemptOptions{})
		req := rt.Request(t, rt.Completed(report(m.Artifact(t, a, rt.ArtifactSpec{})), nil))
		m.FailNext(rt.OpCommit, 1)
		if resp, err := submit(t, m, svc, a, req); !errors.Is(err, results.ErrUnavailable) || resp != (runtimev1.ResultResponse{}) {
			t.Fatalf("%+v %v", resp, err)
		}
		if st := m.State(t, a); st.Status != "active" || st.ResultHash != "" || len(st.Late) != 0 {
			t.Fatalf("a failed commit must leave no trace: %+v", st)
		}
		first, err := submit(t, m, svc, a, req)
		if err != nil || first.AttemptStatus != runtimev1.AttemptSucceeded {
			t.Fatalf("%+v %v", first, err)
		}
		if again, err := submit(t, m, svc, a, req); err != nil || again != first {
			t.Fatalf("%+v %v", again, err)
		}
	})

	t.Run("concurrent duplicates and a competing result leave exactly one receipt", func(t *testing.T) {
		m := rt.NewMemory()
		svc := m.Service(t)
		a := m.NewAttempt(t, rt.ScopeA, rt.AttemptOptions{})
		good := rt.Request(t, rt.Completed(report(m.Artifact(t, a, rt.ArtifactSpec{})), nil))
		rival := rt.Request(t, rt.Failed())
		var wg sync.WaitGroup
		type outcome struct {
			resp runtimev1.ResultResponse
			err  error
			req  runtimev1.ResultRequest
		}
		out := make([]outcome, 12)
		for i := range out {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := good
				if i%3 == 0 {
					req = rival
				}
				resp, err := submit(t, m, svc, a, req)
				out[i] = outcome{resp, err, req}
			}()
		}
		wg.Wait()
		winner := m.State(t, a).ResultHash
		if winner == "" {
			t.Fatal("nothing was accepted")
		}
		for i, o := range out {
			switch {
			case o.req.ResultHash == winner && (o.err != nil || o.resp.ResultHash != winner):
				t.Errorf("%d: the winning hash must converge: %+v %v", i, o.resp, o.err)
			case o.req.ResultHash != winner && !errors.Is(o.err, results.ErrResultConflict):
				t.Errorf("%d: the losing hash must conflict: %+v %v", i, o.resp, o.err)
			}
		}
	})

	t.Run("an unknown push outcome stays pending and never lets the writer succeed", func(t *testing.T) {
		m := rt.NewMemory()
		svc := m.Service(t)
		w, req := writer(t, m)
		m.FailNextPush(false) // the remote never saw the push
		out, err := propose(t, m, svc, w, req)
		if err != nil || out.HTTPStatus != 202 || out.Response.Status != runtimev1.PublishPending || out.Response.AcceptedCommit != nil {
			t.Fatalf("an unknown outcome is pending, not accepted: %+v %v", out, err)
		}
		if p, ok := m.Publication(t, w); !ok || p.Status != "pending" {
			t.Fatalf("the intent was recorded before the push: %+v %v", p, ok)
		}
		src := published
		result := rt.Request(t, rt.Completed(map[string]runtimev1.Value{}, &src))
		if _, err := submit(t, m, svc, w, result); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("an unsettled publication blocks success: %v", err)
		}
		if n, err := m.Reconcile(bg, w.Scope.TenantID); err != nil || n != 0 {
			t.Fatalf("a push that never happened must not be recorded as published: %d %v", n, err)
		}
		// The repeated identical proposal pushes again and settles once.
		out, err = propose(t, m, svc, w, req)
		if err != nil || out.HTTPStatus != 200 {
			t.Fatalf("%+v %v", out, err)
		}
		if resp, err := submit(t, m, svc, w, result); err != nil || resp.AttemptStatus != runtimev1.AttemptSucceeded {
			t.Fatalf("%+v %v", resp, err)
		}
	})

	t.Run("git success with a database failure is settled from the remote", func(t *testing.T) {
		m := rt.NewMemory()
		svc := m.Service(t)
		w, req := writer(t, m)
		m.FailNext(rt.OpSettle, 1)
		out, err := propose(t, m, svc, w, req)
		if err != nil || out.HTTPStatus != 202 {
			t.Fatalf("%+v %v", out, err)
		}
		if tip := m.Remote.Tip(w.Pinned.Workspace.Source.RepositoryURL, rt.Branch); tip != published {
			t.Fatalf("the push reached the remote: %s", tip)
		}
		src := published
		result := rt.Request(t, rt.Completed(map[string]runtimev1.Value{}, &src))
		if _, err := submit(t, m, svc, w, result); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("a pending receipt blocks the result even though the remote has the commit: %v", err)
		}
		if n, err := m.Reconcile(bg, w.Scope.TenantID); err != nil || n != 1 {
			t.Fatalf("reconcile: %d %v", n, err)
		}
		if n, _ := m.Reconcile(bg, w.Scope.TenantID); n != 0 {
			t.Fatalf("a settled publication is settled once: %d", n)
		}
		if again, err := propose(t, m, svc, w, req); err != nil || again.HTTPStatus != 200 {
			t.Fatalf("the repeated proposal observes the settled receipt: %+v %v", again, err)
		}
		first, err := submit(t, m, svc, w, result)
		if err != nil || first.AttemptStatus != runtimev1.AttemptSucceeded {
			t.Fatalf("%+v %v", first, err)
		}
		if again, err := submit(t, m, svc, w, result); err != nil || again != first {
			t.Fatalf("the result settles once: %+v %v", again, err)
		}
	})

	t.Run("a failed intent write pushes nothing", func(t *testing.T) {
		m := rt.NewMemory()
		svc := m.Service(t)
		w, req := writer(t, m)
		m.FailNext(rt.OpRecord, 1)
		if _, err := propose(t, m, svc, w, req); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		if _, ok := m.Publication(t, w); ok {
			t.Fatal("an intent survived its failed transaction")
		}
		if tip := m.Remote.Tip(w.Pinned.Workspace.Source.RepositoryURL, rt.Branch); tip != rt.BaseCommit {
			t.Fatalf("the remote moved before the intent was durable: %s", tip)
		}
	})

	t.Run("a pending publication whose tip moved elsewhere becomes a conflict, never success", func(t *testing.T) {
		m := rt.NewMemory()
		svc := m.Service(t)
		w, req := writer(t, m)
		m.FailNextPush(false)
		if out, err := propose(t, m, svc, w, req); err != nil || out.HTTPStatus != 202 {
			t.Fatalf("%+v %v", out, err)
		}
		other := runtimev1.GitCommit("2222222222222222222222222222222222222222")
		m.MoveTip(t, w, other)
		if n, err := m.Reconcile(bg, w.Scope.TenantID); err != nil || n != 1 {
			t.Fatalf("%d %v", n, err)
		}
		var pc *results.PublicationConflict
		if _, err := propose(t, m, svc, w, req); !errors.As(err, &pc) || pc.ObservedTip != string(other) {
			t.Fatalf("%v", err)
		}
		src := published
		if _, err := submit(t, m, svc, w, rt.Request(t, rt.Completed(map[string]runtimev1.Value{}, &src))); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("a conflicted publication never lets the result succeed: %v", err)
		}
	})
}

// Test_unit_result_revocation proves stale, foreign and revoked attempts are
// denied before any effect (PX-07, IT-03).
func Test_unit_result_revocation(t *testing.T) {
	t.Run("a revoked attempt is denied before evidence is read, pinned or committed", func(t *testing.T) {
		m := rt.NewMemory()
		svc := m.Service(t)
		a := m.NewAttempt(t, rt.ScopeA, rt.AttemptOptions{})
		art := m.Artifact(t, a, rt.ArtifactSpec{})
		m.Revoke(t, a)
		if _, err := submit(t, m, svc, a, rt.Request(t, rt.Completed(report(art), nil))); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("%v", err)
		}
		if st := m.State(t, a); st.Status != "cancelled" || st.ResultHash != "" || len(st.Late) != 0 {
			t.Fatalf("a denied callback changed state: %+v", st)
		}
		if m.Pinned(t, a, art) {
			t.Fatal("a denied callback pinned artifacts")
		}
		if !m.Retain(t, a, art) {
			t.Fatal("retention must be free to remove an unprotected artifact")
		}
	})

	t.Run("cancellation between the first check and the commit loses to the guard", func(t *testing.T) {
		m := rt.NewMemory()
		a := m.NewAttempt(t, rt.ScopeA, rt.AttemptOptions{})
		deps := m.Dependencies()
		p, err := deps.Authority.Authenticate(bg, results.AuthRequest{Attempt: a.ID, Bearer: m.Bearer(t, a, results.ActionResultSubmit), Action: results.ActionResultSubmit})
		if err != nil {
			t.Fatal(err)
		}
		bound, err := deps.Authority.Bind(bg, p)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Cancel(bg, a.Scope, a.ID); err != nil {
			t.Fatal(err)
		}
		req := rt.Request(t, rt.Failed())
		if _, err := deps.Finalizer.Commit(bound, results.Commit{Scope: a.Scope, AttemptID: a.ID, Request: req}); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("a first check is not a permit: %v", err)
		}
		if st := m.State(t, a); st.ResultHash != "" || st.Status != "cancelled" || len(st.Late) != 0 {
			t.Fatalf("%+v", st)
		}
	})

	t.Run("cancellation between the first check and the intent publishes nothing", func(t *testing.T) {
		m := rt.NewMemory()
		w, req := writer(t, m)
		deps := m.Dependencies()
		p, err := deps.Authority.Authenticate(bg, results.AuthRequest{Attempt: w.ID, Bearer: m.Bearer(t, w, results.ActionSourcePropose), Action: results.ActionSourcePropose})
		if err != nil {
			t.Fatal(err)
		}
		bound, err := deps.Authority.Bind(bg, p)
		if err != nil {
			t.Fatal(err)
		}
		m.Revoke(t, w)
		if _, err := deps.Source.Propose(bound, w.Scope, w.ID, req); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("%v", err)
		}
		if _, ok := m.Publication(t, w); ok {
			t.Fatal("a callback that lost the race initiated a publication")
		}
		if tip := m.Remote.Tip(w.Pinned.Workspace.Source.RepositoryURL, rt.Branch); tip != rt.BaseCommit {
			t.Fatalf("the remote moved: %s", tip)
		}
	})

	t.Run("a late result is kept as evidence and rejected", func(t *testing.T) {
		m := rt.NewMemory()
		svc := m.Service(t)
		a := m.NewAttempt(t, rt.ScopeA, rt.AttemptOptions{Timeout: 30 * time.Second})
		req := rt.Request(t, rt.Completed(report(m.Artifact(t, a, rt.ArtifactSpec{})), nil))
		m.Clock.Advance(31 * time.Second)
		if _, err := submit(t, m, svc, a, req); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("%v", err)
		}
		st := m.State(t, a)
		if st.Status != "timed_out" || st.ResultHash != "" || len(st.Late) != 1 || st.Late[0] != req.ResultHash {
			t.Fatalf("the late result is evidence, never the receipt: %+v", st)
		}
		if _, err := submit(t, m, svc, a, req); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("the timeout revoked the grant: %v", err)
		}
	})

	t.Run("an accepted attempt still answers its receipt until it is revoked", func(t *testing.T) {
		m := rt.NewMemory()
		svc := m.Service(t)
		a := m.NewAttempt(t, rt.ScopeA, rt.AttemptOptions{})
		req := rt.Request(t, rt.Completed(report(m.Artifact(t, a, rt.ArtifactSpec{})), nil))
		first, err := submit(t, m, svc, a, req)
		if err != nil {
			t.Fatal(err)
		}
		if again, err := submit(t, m, svc, a, req); err != nil || again != first {
			t.Fatalf("the receipt exception: %+v %v", again, err)
		}
		// result.submit keeps its receipt exception; source.propose does not.
		if _, err := svc.ProposeSource(bg, results.ProposeCommand{Attempt: a.ID, Bearer: m.Bearer(t, a, results.ActionSourcePropose),
			Request: runtimev1.SourceProposalRequest{Commit: published, ExpectedTip: rt.BaseCommit, BundleArtifactID: "art_x"}}); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("a terminal attempt may not publish source: %v", err)
		}
		m.Revoke(t, a)
		if _, err := submit(t, m, svc, a, req); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("explicit revocation denies even a replay: %v", err)
		}
	})

	t.Run("expired, forged and cross-tenant credentials are unauthenticated", func(t *testing.T) {
		m := rt.NewMemory()
		svc := m.Service(t)
		a := m.NewAttempt(t, rt.ScopeA, rt.AttemptOptions{})
		other := m.NewAttempt(t, rt.ScopeB, rt.AttemptOptions{})
		req := rt.Request(t, rt.Failed())
		forged := "rtm." + string(rt.ScopeB.TenantID) + ".ses_" + string(a.ID)
		for name, bearer := range map[string]string{"forged": forged, "cross tenant": m.Bearer(t, other, results.ActionResultSubmit), "wrong prefix": "rt.t081_acme.ses_" + string(a.ID)} {
			if _, err := svc.Submit(bg, results.SubmitCommand{Attempt: a.ID, Bearer: bearer, Request: req}); !errors.Is(err, results.ErrUnauthenticated) {
				t.Errorf("%s: %v", name, err)
			}
		}
		m.Clock.Advance(2 * time.Hour)
		if _, err := submit(t, m, svc, a, req); !errors.Is(err, results.ErrUnauthenticated) {
			t.Fatalf("an expired credential: %v", err)
		}
		if st := m.State(t, a); st.ResultHash != "" || len(st.Late) != 0 {
			t.Fatalf("%+v", st)
		}
	})

	t.Run("a principal the authority did not issue, or an unbound context, reaches nothing", func(t *testing.T) {
		m, foreign := rt.NewMemory(), rt.NewMemory()
		a := m.NewAttempt(t, rt.ScopeA, rt.AttemptOptions{})
		deps := m.Dependencies()
		p, err := deps.Authority.Authenticate(bg, results.AuthRequest{Attempt: a.ID, Bearer: m.Bearer(t, a, results.ActionResultSubmit), Action: results.ActionResultSubmit})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := foreign.Dependencies().Authority.Bind(bg, p); !errors.Is(err, results.ErrUnauthenticated) {
			t.Fatalf("a principal of another authority: %v", err)
		}
		forgedProof := p
		forgedProof.Proof = "not a session"
		if _, err := deps.Authority.Bind(bg, forgedProof); !errors.Is(err, results.ErrUnauthenticated) {
			t.Fatalf("a forged proof: %v", err)
		}
		if _, err := deps.Finalizer.Commit(bg, results.Commit{Scope: a.Scope, AttemptID: a.ID, Request: rt.Request(t, rt.Failed())}); !errors.Is(err, results.ErrUnauthenticated) {
			t.Fatalf("an unbound context must fail closed: %v", err)
		}
		if st := m.State(t, a); st.ResultHash != "" {
			t.Fatalf("%+v", st)
		}
	})
}

// Test_unit_result_protection proves consumed evidence is protected from
// retention only after a verified result and that a pin is idempotent (IT-06).
func Test_unit_result_protection(t *testing.T) {
	m := rt.NewMemory()
	svc := m.Service(t)
	a := m.NewAttempt(t, rt.ScopeA, rt.AttemptOptions{})
	art := m.Artifact(t, a, rt.ArtifactSpec{})
	stray := m.Artifact(t, a, rt.ArtifactSpec{})
	req := rt.Request(t, rt.Completed(report(art), nil))
	if _, err := submit(t, m, svc, a, req); err != nil {
		t.Fatal(err)
	}
	if _, err := submit(t, m, svc, a, req); err != nil {
		t.Fatal(err)
	}
	if !m.Pinned(t, a, art) || m.Pinned(t, a, stray) {
		t.Fatal("only the consumed artifact is pinned")
	}
	if m.Retain(t, a, art) {
		t.Fatal("retention removed a pinned artifact")
	}
	if !m.Retain(t, a, stray) {
		t.Fatal("an unreferenced artifact stays removable")
	}
	// A removed artifact is not evidence for a new result.
	b := m.NewAttempt(t, rt.ScopeA, rt.AttemptOptions{})
	gone := m.Artifact(t, b, rt.ArtifactSpec{})
	m.Retain(t, b, gone)
	if _, err := submit(t, m, svc, b, rt.Request(t, rt.Completed(report(gone), nil))); !errors.Is(err, results.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}
