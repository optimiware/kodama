package resultstest_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/results"
	resultshttp "github.com/optimiware/kodama/internal/results/http"
	rt "github.com/optimiware/kodama/internal/results/resultstest"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// The source-set result route (T-150) over the v3 fixture: commit outputs are
// accepted only from applied push records of the attempt (WS-19, GR-06), a result
// the control plane already accepted is answered from its receipt without reading
// anything, and the earlier routes are not touched.

const (
	taskBranch = "refs/heads/kodama/work/wi_1042"
	agentRef   = "refs/heads/kodama/agents/wi_1042/run_01/impl"
	commitC    = "3333333333333333333333333333333333333333"
	commitD    = "4444444444444444444444444444444444444444"
)

// writerResult is a completed result of the writer attempt with its declared
// output and the given commit outputs.
func writerResult(t *testing.T, w *rt.V3World, a rt.V3Attempt, commits ...runtimev3.CommitOutput) runtimev3.ResultRequestV3 {
	t.Helper()
	w.AddArtifact(a, "art_0001")
	return rt.Completed3(t, map[string]runtimev1.Value{"report": runtimev1.StringValue("art_0001")}, commits...)
}

func taskOutput(push string) runtimev3.CommitOutput {
	return runtimev3.CommitOutput{RepositoryID: "repo_01", Ref: taskBranch, Commit: commitC, PushID: push}
}

func Test_unit_result_requires_applied_push_record(t *testing.T) {
	t.Run("an applied push of this attempt is accepted", func(t *testing.T) {
		w := rt.NewV3World(t)
		svc, a := w.Service(), w.NewAttempt(rtest.WriterContext)
		w.SetPush(rt.PushOf(a, "repo_01", "pu_07", taskBranch, commitC, gitv1.EntryApplied))
		req := writerResult(t, w, a, taskOutput("pu_07"))
		resp, err := w.Submit(svc, a, req)
		if err != nil || resp.AttemptStatus != runtimev1.AttemptSucceeded || resp.ResultHash != req.ResultHash {
			t.Fatalf("submit: %+v %v", resp, err)
		}
		if w.PushReads != 1 || len(w.Commits) != 1 || len(w.Pinned) != 1 {
			t.Fatalf("the record was read once and the result committed once: reads=%d commits=%d pinned=%v", w.PushReads, len(w.Commits), w.Pinned)
		}
	})

	t.Run("an unsettled push is not accepted yet and nothing changes; the repeat is accepted when it applies", func(t *testing.T) {
		for _, state := range []gitv1.EntryState{gitv1.EntryAuthorized, gitv1.EntryCommitting, gitv1.EntryUnknown} {
			w := rt.NewV3World(t)
			svc, a := w.Service(), w.NewAttempt(rtest.WriterContext)
			w.SetPush(rt.PushOf(a, "repo_01", "pu_07", taskBranch, commitC, state))
			req := writerResult(t, w, a, taskOutput("pu_07"))
			if _, err := w.Submit(svc, a, req); !errors.Is(err, results.ErrUnavailable) {
				t.Fatalf("%s: %v (a retryable answer, not a decision)", state, err)
			}
			if len(w.Commits) != 0 || len(w.Pinned) != 0 {
				t.Fatalf("%s: something was committed or pinned: %v %v", state, w.Commits, w.Pinned)
			}
			if _, ok := w.Accept(a); ok {
				t.Fatalf("%s: the attempt has a receipt", state)
			}
			w.SetPush(rt.PushOf(a, "repo_01", "pu_07", taskBranch, commitC, gitv1.EntryApplied))
			if resp, err := w.Submit(svc, a, req); err != nil || resp.AttemptStatus != runtimev1.AttemptSucceeded {
				t.Fatalf("%s: the repeated result: %+v %v", state, resp, err)
			}
		}
	})

	t.Run("an output that can never count is rejected and nothing changes", func(t *testing.T) {
		other := func(a rt.V3Attempt) rt.V3Attempt { b := a; b.ID, b.Run = "att_other", "run_other"; return b }
		user := func(rec gitv1.PushRecord) gitv1.PushRecord {
			rec.Actor, rec.OriginRunID = gitv1.Actor{Type: gitv1.ActorUser, ID: "u_1"}, nil
			return rec
		}
		for name, mk := range map[string]func(a rt.V3Attempt) gitv1.PushRecord{
			"refused": func(a rt.V3Attempt) gitv1.PushRecord {
				return rt.PushOf(a, "repo_01", "pu_07", taskBranch, commitC, gitv1.EntryRefused)
			},
			"another attempt's push": func(a rt.V3Attempt) gitv1.PushRecord {
				return rt.PushOf(other(a), "repo_01", "pu_07", taskBranch, commitC, gitv1.EntryApplied)
			},
			"another run's push": func(a rt.V3Attempt) gitv1.PushRecord {
				r := rt.PushOf(a, "repo_01", "pu_07", taskBranch, commitC, gitv1.EntryApplied)
				run := "run_other"
				r.Actor.RunID, r.OriginRunID = &run, &run
				return r
			},
			"a person's push": func(a rt.V3Attempt) gitv1.PushRecord {
				return user(rt.PushOf(a, "repo_01", "pu_07", taskBranch, commitC, gitv1.EntryApplied))
			},
			"another repository's push": func(a rt.V3Attempt) gitv1.PushRecord {
				return rt.PushOf(a, "repo_02", "pu_07", taskBranch, commitC, gitv1.EntryApplied)
			},
			"a push of another commit": func(a rt.V3Attempt) gitv1.PushRecord {
				return rt.PushOf(a, "repo_01", "pu_07", taskBranch, commitD, gitv1.EntryApplied)
			},
			"a push of another ref": func(a rt.V3Attempt) gitv1.PushRecord {
				return rt.PushOf(a, "repo_01", "pu_07", agentRef, commitC, gitv1.EntryApplied)
			},
		} {
			w := rt.NewV3World(t)
			svc, a := w.Service(), w.NewAttempt(rtest.WriterContext)
			rec := mk(a)
			w.SetPush(rec)
			if _, err := w.Submit(svc, a, writerResult(t, w, a, taskOutput("pu_07"))); !errors.Is(err, results.ErrOutputInvalid) {
				t.Errorf("%s: %v", name, err)
			}
			if len(w.Commits) != 0 || len(w.Pinned) != 0 {
				t.Errorf("%s: something was committed or pinned", name)
			}
			if _, ok := w.Accept(a); ok {
				t.Errorf("%s: the attempt has a receipt", name)
			}
		}
		// A push that does not exist (or another tenant's) is the same answer.
		w := rt.NewV3World(t)
		svc, a := w.Service(), w.NewAttempt(rtest.WriterContext)
		if _, err := w.Submit(svc, a, writerResult(t, w, a, taskOutput("pu_nowhere"))); !errors.Is(err, results.ErrOutputInvalid) {
			t.Fatalf("an unknown push: %v", err)
		}
	})

	t.Run("an outage reading the record decides nothing", func(t *testing.T) {
		w := rt.NewV3World(t)
		svc, a := w.Service(), w.NewAttempt(rtest.WriterContext)
		w.SetPush(rt.PushOf(a, "repo_01", "pu_07", taskBranch, commitC, gitv1.EntryApplied))
		w.PushErr = errors.New("push store down")
		req := writerResult(t, w, a, taskOutput("pu_07"))
		if _, err := w.Submit(svc, a, req); !errors.Is(err, results.ErrUnavailable) || len(w.Commits) != 0 {
			t.Fatalf("outage: %v", err)
		}
		w.PushErr = nil
		if _, err := w.Submit(svc, a, req); err != nil {
			t.Fatalf("after the outage: %v", err)
		}
		w.IssuedErr = errors.New("records down")
		w2 := rt.NewV3World(t)
		w2.IssuedErr = errors.New("records down")
		a2 := w2.NewAttempt(rtest.WriterContext)
		w2.SetPush(rt.PushOf(a2, "repo_01", "pu_07", taskBranch, commitC, gitv1.EntryApplied))
		if _, err := w2.Submit(w2.Service(), a2, writerResult(t, w2, a2, taskOutput("pu_07"))); !errors.Is(err, results.ErrUnavailable) || len(w2.Commits) != 0 {
			t.Fatalf("the issued grants unreadable: %v", err)
		}
	})

	t.Run("a refusal outranks a pending output; every output is checked", func(t *testing.T) {
		w := rt.NewV3World(t)
		svc, a := w.Service(), w.NewAttempt(rtest.WriterContext)
		agent := runtimev3.CommitOutput{RepositoryID: "repo_01", Ref: agentRef, Commit: commitD, PushID: "pu_06"}
		w.SetPush(rt.PushOf(a, "repo_01", "pu_06", agentRef, commitD, gitv1.EntryApplied))
		w.SetPush(rt.PushOf(a, "repo_01", "pu_07", taskBranch, commitC, gitv1.EntryCommitting))
		req := writerResult(t, w, a, agent, taskOutput("pu_07"))
		if _, err := w.Submit(svc, a, req); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("one pending: %v", err)
		}
		w.SetPush(rt.PushOf(a, "repo_01", "pu_06", agentRef, commitD, gitv1.EntryRefused))
		if _, err := w.Submit(svc, a, req); !errors.Is(err, results.ErrOutputInvalid) {
			t.Fatalf("one refused beside a pending one: %v", err)
		}
		w.SetPush(rt.PushOf(a, "repo_01", "pu_06", agentRef, commitD, gitv1.EntryApplied))
		w.SetPush(rt.PushOf(a, "repo_01", "pu_07", taskBranch, commitC, gitv1.EntryApplied))
		if resp, err := w.Submit(svc, a, req); err != nil || resp.AttemptStatus != runtimev1.AttemptSucceeded {
			t.Fatalf("both applied: %+v %v", resp, err)
		}
	})

	t.Run("a result without commit outputs reads no push record", func(t *testing.T) {
		w := rt.NewV3World(t)
		svc, a := w.Service(), w.NewAttempt(rtest.WriterContext)
		resp, err := w.Submit(svc, a, writerResult(t, w, a))
		if err != nil || resp.AttemptStatus != runtimev1.AttemptSucceeded || w.PushReads != 0 || w.IssuedReads != 0 {
			t.Fatalf("%+v %v pushes=%d issued=%d", resp, err, w.PushReads, w.IssuedReads)
		}
	})

	t.Run("an output on the default branch needs a runtime grant of the control plane's records", func(t *testing.T) {
		out := runtimev3.CommitOutput{RepositoryID: "repo_01", Ref: "refs/heads/main", Commit: commitC, PushID: "pu_09"}
		// Without a recorded integration grant the output is not covered by anything
		// the attempt holds: the contract check fails before any push record is read,
		// and execution records the durable failure.
		w := rt.NewV3World(t)
		svc, a := w.Service(), w.NewAttempt(rtest.WriterContext)
		w.SetPush(rt.PushOf(a, "repo_01", "pu_09", "refs/heads/main", commitC, gitv1.EntryApplied))
		resp, err := w.Submit(svc, a, writerResult(t, w, a, out))
		if err != nil || resp.AttemptStatus != runtimev1.AttemptFailed || w.PushReads != 0 {
			t.Fatalf("an uncovered output: %+v %v (push reads %d)", resp, err, w.PushReads)
		}
		// With the control plane's record of the integration grant of this attempt, to
		// this commit: accepted, and the claim the finalizer received is the record.
		w = rt.NewV3World(t)
		svc, a = w.Service(), w.NewAttempt(rtest.WriterContext)
		claim := rtest.IssuedClaims(gitv1.GrantIntegration, a.Scope, a.Run, a.ID, "gg_int01", "repo_01")
		w.SetIssued(a, claim)
		w.SetPush(rt.PushOf(a, "repo_01", "pu_09", "refs/heads/main", commitC, gitv1.EntryApplied))
		resp, err = w.Submit(svc, a, writerResult(t, w, a, out))
		if err != nil || resp.AttemptStatus != runtimev1.AttemptSucceeded || len(w.Commits) != 1 || len(w.Commits[0].Issued) != 1 || w.Commits[0].Issued[0].GrantID != "gg_int01" {
			t.Fatalf("a covered output: %+v %v %+v", resp, err, w.Commits)
		}
		// The grant covers the approved candidate only.
		w = rt.NewV3World(t)
		svc, a = w.Service(), w.NewAttempt(rtest.WriterContext)
		w.SetIssued(a, rtest.IssuedClaims(gitv1.GrantIntegration, a.Scope, a.Run, a.ID, "gg_int01", "repo_01"))
		other := out
		other.Commit = commitD
		w.SetPush(rt.PushOf(a, "repo_01", "pu_09", "refs/heads/main", commitD, gitv1.EntryApplied))
		if resp, err := w.Submit(svc, a, writerResult(t, w, a, other)); err != nil || resp.AttemptStatus != runtimev1.AttemptFailed {
			t.Fatalf("another commit than the approved candidate: %+v %v", resp, err)
		}
		// Another attempt's recorded grant does not cover this attempt's output.
		w = rt.NewV3World(t)
		svc, a = w.Service(), w.NewAttempt(rtest.WriterContext)
		w.SetIssued(a, rtest.IssuedClaims(gitv1.GrantIntegration, a.Scope, a.Run, "att_other", "gg_int01", "repo_01"))
		w.SetPush(rt.PushOf(a, "repo_01", "pu_09", "refs/heads/main", commitC, gitv1.EntryApplied))
		if resp, err := w.Submit(svc, a, writerResult(t, w, a, out)); err != nil || resp.AttemptStatus != runtimev1.AttemptFailed {
			t.Fatalf("another attempt's grant: %+v %v", resp, err)
		}
	})

	t.Run("a provisioning attempt's seed output", func(t *testing.T) {
		w := rt.NewV3World(t)
		svc, a := w.Service(), w.NewAttempt(rtest.ProvisionContext)
		out := runtimev3.CommitOutput{RepositoryID: "repo_new", Ref: "refs/heads/main", Commit: commitC, PushID: "pu_s1"}
		w.SetIssued(a, rtest.IssuedClaims(gitv1.GrantSeed, a.Scope, a.Run, a.ID, "gg_seed01", "repo_new"))
		w.SetPush(rt.PushOf(a, "repo_new", "pu_s1", "refs/heads/main", commitC, gitv1.EntryCommitting))
		req := writerResult(t, w, a, out)
		if _, err := w.Submit(svc, a, req); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("the seed push is not settled: %v", err)
		}
		w.SetPush(rt.PushOf(a, "repo_new", "pu_s1", "refs/heads/main", commitC, gitv1.EntryApplied))
		if resp, err := w.Submit(svc, a, req); err != nil || resp.AttemptStatus != runtimev1.AttemptSucceeded {
			t.Fatalf("seed: %+v %v", resp, err)
		}
	})
}

func Test_unit_result_receipt_first_replay(t *testing.T) {
	setup := func(t *testing.T) (*rt.V3World, *results.Service, rt.V3Attempt, runtimev3.ResultRequestV3) {
		w := rt.NewV3World(t)
		svc, a := w.Service(), w.NewAttempt(rtest.WriterContext)
		w.SetPush(rt.PushOf(a, "repo_01", "pu_07", taskBranch, commitC, gitv1.EntryApplied))
		return w, svc, a, writerResult(t, w, a, taskOutput("pu_07"))
	}

	t.Run("the same result is answered from the receipt without reading anything", func(t *testing.T) {
		w, svc, a, req := setup(t)
		first, err := w.Submit(svc, a, req)
		if err != nil {
			t.Fatal(err)
		}
		w.Clock.Advance(time.Minute)
		// The world moves on: the record is refused, the artifact is gone, the
		// grants unreadable. A replay reads none of it.
		w.SetPush(rt.PushOf(a, "repo_01", "pu_07", taskBranch, commitC, gitv1.EntryRefused))
		w.PushErr, w.IssuedErr = errors.New("down"), errors.New("down")
		w.ForbidReads = true
		reads, pins := w.PushReads, len(w.Pinned)
		for i := 0; i < 3; i++ {
			again, err := w.Submit(svc, a, req)
			if err != nil || again != first {
				t.Fatalf("replay %d: %+v %v, want %+v", i, again, err, first)
			}
		}
		if w.PushReads != reads || len(w.Pinned) != pins || len(w.Commits) != 1 {
			t.Fatalf("a replay read, pinned or committed again: reads %d/%d pins %d/%d commits %d", w.PushReads, reads, len(w.Pinned), pins, len(w.Commits))
		}
	})

	t.Run("a different result conflicts without reading anything", func(t *testing.T) {
		w, svc, a, req := setup(t)
		if _, err := w.Submit(svc, a, req); err != nil {
			t.Fatal(err)
		}
		w.ForbidReads = true
		different := rt.Completed3(t, map[string]runtimev1.Value{"report": runtimev1.StringValue("art_0001")})
		if _, err := w.Submit(svc, a, different); !errors.Is(err, results.ErrResultConflict) {
			t.Fatalf("a different result: %v", err)
		}
		if len(w.Commits) != 1 {
			t.Fatal("the conflicting result was committed")
		}
	})

	t.Run("a lost acknowledgment is replayed with the original acceptance time", func(t *testing.T) {
		w, svc, a, req := setup(t)
		committedAt := w.Clock.Now()
		w.LoseAck = 1
		if _, err := w.Submit(svc, a, req); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("a lost response is unavailable to the caller: %v", err)
		}
		if _, ok := w.Accept(a); !ok {
			t.Fatal("the result was committed")
		}
		w.Clock.Advance(time.Hour)
		w.ForbidReads = true
		resp, err := w.Submit(svc, a, req)
		if err != nil || resp.AcceptedAt != runtimev1.Timestamp(committedAt.UTC().Format(time.RFC3339Nano)) {
			t.Fatalf("replay: %+v %v", resp, err)
		}
	})

	t.Run("an unknown commit outcome is repeated and converges", func(t *testing.T) {
		w, svc, a, req := setup(t)
		w.CommitErr = results.Fail(results.ErrUnavailable, nil, "unknown")
		if _, err := w.Submit(svc, a, req); !errors.Is(err, results.ErrUnavailable) {
			t.Fatal(err)
		}
		if _, ok := w.Accept(a); ok {
			t.Fatal("an unknown outcome fabricated a receipt")
		}
		if resp, err := w.Submit(svc, a, req); err != nil || resp.AttemptStatus != runtimev1.AttemptSucceeded {
			t.Fatalf("repeat: %+v %v", resp, err)
		}
	})

	t.Run("the receipt exception follows the grant: replay after cancellation, never after revocation", func(t *testing.T) {
		w, svc, a, req := setup(t)
		first, err := w.Submit(svc, a, req)
		if err != nil {
			t.Fatal(err)
		}
		w.Cancel(a, false)
		w.ForbidReads = true
		if again, err := w.Submit(svc, a, req); err != nil || again != first {
			t.Fatalf("replay after the attempt ended: %+v %v", again, err)
		}
		w.Cancel(a, true)
		if _, err := w.Submit(svc, a, req); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("replay with a revoked grant: %v", err)
		}
	})

	t.Run("a late result for a cancelled attempt is rejected, not accepted", func(t *testing.T) {
		w, svc, a, req := setup(t)
		w.Cancel(a, false)
		if _, err := w.Submit(svc, a, req); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("late result: %v", err)
		}
		if len(w.Commits) != 0 {
			t.Fatal("a cancelled attempt's result was committed")
		}
	})
}

// forbiddenV3 fails the test when the earlier routes touch the source-set ports.
func forbiddenV3(t testing.TB) (*results.DependenciesV3, *rt.V3World) {
	w := rt.NewV3World(t)
	w.ForbidReads = true
	return &results.DependenciesV3{Contexts: w, Pushes: w, Issued: w, Finalizer: w}, w
}

func Test_unit_result_external_publication_unchanged(t *testing.T) {
	t.Run("the earlier Contract holds with the source-set route configured and never reaches it", func(t *testing.T) {
		var worlds []*rt.V3World
		t.Cleanup(func() {
			for _, w := range worlds {
				if w.PushReads+w.IssuedReads+w.ContextReads+w.EvidenceReads != 0 || len(w.Commits) != 0 {
					t.Errorf("the external publication path reached the source-set ports: %+v", w)
				}
			}
		})
		rt.Contract(t, func(tb testing.TB) rt.Subject {
			s := rt.NewMemory().Subject()
			var w *rt.V3World
			s.Deps.V3, w = forbiddenV3(tb)
			worlds = append(worlds, w)
			return s
		})
	})

	t.Run("the v1 result hash is the golden of the unchanged contract", func(t *testing.T) {
		zero := int64(0)
		c := runtimev1.GitCommit(commitC)
		res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, SourceCommit: &c,
			Outputs: map[string]runtimev1.Value{"report": runtimev1.StringValue("art_0001")}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		h, err := runtimev1.ResultHash(res)
		if err != nil || h != "sha256:6c1c46ec87fc9e07292a8deb9044d5b7825e09dc0fa5dc537de6466859d4f867" {
			t.Fatalf("the v1 result hash moved: %s %v", h, err)
		}
		// A v3 result of the same content never has the v1 bytes: the two wires
		// cannot be confused by their hashes.
		v3 := rt.Completed3(t, map[string]runtimev1.Value{"report": runtimev1.StringValue("art_0001")}, runtimev3.CommitOutput{RepositoryID: "repo_01", Ref: taskBranch, Commit: commitC, PushID: "pu_07"})
		if v3.ResultHash == h {
			t.Fatal("a v3 result hashes like a v1 result")
		}
	})

	t.Run("the wires reject each other's bodies and record nothing", func(t *testing.T) {
		// v1 handler over the memory adapters, v3 handler over the v3 fixture.
		m := rt.NewMemory()
		svc1 := m.Service(t)
		a1 := m.NewAttempt(t, rt.ScopeA, rt.AttemptOptions{NoOutputs: true})
		h1, err := resultshttp.NewHandler(resultshttp.Config{Service: svc1})
		if err != nil {
			t.Fatal(err)
		}
		w := rt.NewV3World(t)
		a3 := w.NewAttempt(rtest.WriterContext)
		h3, err := resultshttp.NewV3Handler(resultshttp.ConfigV3{Service: w.Service()})
		if err != nil {
			t.Fatal(err)
		}
		post := func(h http.Handler, path, token string, body any) *httptest.ResponseRecorder {
			raw, _ := json.Marshal(body)
			r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			return rec
		}
		v3body := writerResult(t, w, a3, taskOutput("pu_07"))
		v1res := rt.Completed(map[string]runtimev1.Value{}, nil)
		v1body := rt.Request(t, v1res)

		// A v3 body on the v1 route and a v1 body on the v3 route are invalid requests.
		if rec := post(h1, "/runtime/v1/attempts/"+string(a1.ID)+"/result", m.Bearer(t, a1, results.ActionResultSubmit), v3body); rec.Code != 400 && rec.Code != 401 {
			t.Fatalf("a v3 body on the v1 route: %d %s", rec.Code, rec.Body)
		}
		if rec := post(h3, "/runtime/v3/attempts/"+string(a3.ID)+"/result", a3.Token, v1body); rec.Code != 400 {
			t.Fatalf("a v1 body on the v3 route: %d %s", rec.Code, rec.Body)
		}
		if _, ok := w.Accept(a3); ok || len(w.Commits) != 0 {
			t.Fatal("a refused body was committed")
		}
		// The v3 route does not serve the v1 path and the reverse.
		if rec := post(h3, "/runtime/v1/attempts/"+string(a3.ID)+"/result", a3.Token, v1body); rec.Code != 404 {
			t.Fatalf("v3 handler on a v1 path: %d", rec.Code)
		}
		if rec := post(h1, "/runtime/v3/attempts/"+string(a1.ID)+"/result", m.Bearer(t, a1, results.ActionResultSubmit), v3body); rec.Code != 404 {
			t.Fatalf("v1 handler on a v3 path: %d", rec.Code)
		}
		// A v1 attempt on the v3 route is a version mismatch (409), nothing read.
		old := w.NewV1Attempt("att_v1")
		w.ForbidReads = true
		rec := post(h3, "/runtime/v3/attempts/att_v1/result", old.Token, v3body)
		var env struct {
			Error struct {
				Code domain.ErrorCode `json:"code"`
			} `json:"error"`
		}
		if rec.Code != 409 || json.Unmarshal(rec.Body.Bytes(), &env) != nil || env.Error.Code != runtimev3.CodeContextVersionMismatch {
			t.Fatalf("a v1 attempt on the v3 route: %d %s", rec.Code, rec.Body)
		}
		// The v1 route still accepts a v1 result with its envelope unchanged.
		if rec := post(h1, "/runtime/v1/attempts/"+string(a1.ID)+"/result", m.Bearer(t, a1, results.ActionResultSubmit), v1body); rec.Code != 200 {
			t.Fatalf("the v1 result: %d %s", rec.Code, rec.Body)
		}
	})
}

func Test_unit_result_source_set_route_fails_closed(t *testing.T) {
	// A service without the source-set ports refuses the route; with partial ports
	// it is not constructed.
	m := rt.NewMemory()
	svc := m.Service(t)
	if _, err := svc.SubmitV3(bg, results.SubmitV3Command{Attempt: "att_01"}); !errors.Is(err, results.ErrUnavailable) {
		t.Fatalf("an unconfigured route: %v", err)
	}
	w := rt.NewV3World(t)
	d := w.Deps()
	d.V3 = &results.DependenciesV3{Contexts: w, Pushes: w, Issued: w}
	if _, err := results.NewService(d, results.Options{}); err == nil {
		t.Fatal("a source-set configuration missing its finalizer was accepted")
	}
	// A bad token, a bad attempt, another attempt's token and an unknown attempt.
	svc, a := w.Service(), w.NewAttempt(rtest.WriterContext)
	req := writerResult(t, w, a)
	for name, cmd := range map[string]results.SubmitV3Command{
		"no token":      {Attempt: a.ID, Request: req},
		"wrong token":   {Attempt: a.ID, Bearer: "tok_other", Request: req},
		"bad attempt":   {Attempt: "bad attempt", Bearer: a.Token, Request: req},
		"other attempt": {Attempt: "att_nowhere", Bearer: a.Token, Request: req},
	} {
		if _, err := svc.SubmitV3(bg, cmd); !errors.Is(err, results.ErrUnauthenticated) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A hash that does not cover the result is refused before anything is read.
	bad := req
	bad.ResultHash = runtimev3.Digest("sha256:" + strings.Repeat("0", 64))
	w.ForbidReads = true
	if _, err := w.Submit(svc, a, bad); !errors.Is(err, results.ErrResultHashMismatch) {
		t.Fatalf("a wrong hash: %v", err)
	}
}
