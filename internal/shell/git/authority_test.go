package git_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	rt "github.com/optimiware/kodama/internal/kernel/runtime"
	gitauth "github.com/optimiware/kodama/internal/shell/git"
)

var (
	scope = domain.Scope{TenantID: "t1", ProjectID: "p1"}
	t0    = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
)

func oid(c string) string { return strings.Repeat(c, 40) }

const (
	runID    domain.RunID  = "run_1"
	stepName domain.StepID = "integrate"
	repoID                 = "repo_1"
	ref                    = "refs/heads/main"
)

var (
	tip       = oid("a") // the default branch tip
	candidate = oid("b") // the reviewed candidate
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// world is the durable evidence a decision may read, plus the things it must
// not: a task status and a bus of events.
type world struct {
	rec      rt.RunRecord
	policy   gitauth.Policy
	pipeline string
	obs      gitauth.Observation
	// taskStatus and events stand for a task field and bare events. No port
	// reads them; the tests set them to show they confer nothing.
	taskStatus string
	events     []string
}

type fakeRuns struct{ w *world }

func (f fakeRuns) Load(_ context.Context, s domain.Scope, id domain.RunID) (rt.RunRecord, error) {
	if s != f.w.rec.Run.Scope || id != f.w.rec.Run.ID {
		return rt.RunRecord{}, &domain.Error{Code: domain.CodeNotFound, Message: "run"}
	}
	return f.w.rec, nil
}

type fakeRepos struct{ w *world }

func (f fakeRepos) IntegrationPolicy(context.Context, domain.Scope, string) (gitauth.Policy, error) {
	return f.w.policy, nil
}

type fakePipelines struct{ w *world }

func (f fakePipelines) PipelineName(context.Context, domain.Scope, domain.PipelineID) (string, error) {
	return f.w.pipeline, nil
}

type fakeRefs struct{ w *world }

func (f fakeRefs) Observe(context.Context, domain.Scope, string, string, string) (gitauth.Observation, error) {
	return f.w.obs, nil
}

func (w *world) authorizer() gitauth.Authorizer {
	return gitauth.Authorizer{Runs: fakeRuns{w}, Repositories: fakeRepos{w}, Pipelines: fakePipelines{w}, Refs: fakeRefs{w}}
}

func reviewSpec() rt.WaitSpec {
	return rt.WaitSpec{Assign: "reviewer", Correlate: map[string]string{"candidate": "implement.outputs.commit"},
		Input: "approval", Outcomes: []string{"approved", "rejected", "needs_work"}, Approving: "approved",
		Timeout: time.Hour, OnExpiry: rt.ExpiryFail}
}

func plan() rt.Plan {
	spec := reviewSpec()
	return rt.Plan{Digest: "sha256:p", Steps: []rt.PlanStep{
		{Key: "implement", Kind: rt.StepContainer, Workload: &rt.WorkloadSpec{Image: "agent:1", Commands: []string{"agent"}}},
		{Key: "review", Kind: rt.StepWait, DependsOn: []domain.StepID{"implement"}, Wait: &spec},
		{Key: stepName, Kind: rt.StepContainer, DependsOn: []domain.StepID{"review"}, Authority: rt.AuthorityIntegrate,
			Requires: []rt.Requirement{{Wait: "review", Outcome: "approved"}},
			Workload: &rt.WorkloadSpec{Image: "integrator:1", Commands: []string{"integrate-candidate"}}},
	}}
}

// run builds the record of a run whose implement step succeeded, whose review
// wait opened for the candidate and was decided with outcome (empty: left open),
// and whose integrate step is running when outcome approves.
func run(t *testing.T, candidate, outcome string) rt.RunRecord {
	t.Helper()
	p := plan()
	r, err := rt.NewRun(scope, runID, "pl_1", p, 3)
	must(t, err)
	must(t, r.AdvanceStep("implement", rt.StepRunning, rt.ReasonNone))
	must(t, r.FinishStep("implement", rt.StepSucceeded, rt.ReasonNone, rt.Result{Code: rt.ResultSucceeded}))
	must(t, r.AdvanceStep("review", rt.StepWaiting, rt.ReasonNone))
	w, err := rt.NewWait(scope, "wait_1", runID, "review", *p.Steps[1].Wait)
	must(t, err)
	must(t, w.Open(t0, map[string]string{"candidate": candidate}, ""))
	if outcome != "" {
		_, err = w.Resolve(rt.SignalRequest{Identity: rt.SignalIdentity{Scope: scope, Caller: "user-bob", Key: "k1"},
			WaitID: w.ID, Outcome: outcome, Actor: "user:bob"}, t0.Add(time.Minute))
		must(t, err)
		must(t, r.AdvanceStep("review", rt.StepSucceeded, rt.ReasonNone))
		must(t, r.AdvanceStep(stepName, rt.StepRunning, rt.ReasonNone))
	}
	rec := rt.RunRecord{Run: r, Plan: p, Waits: []rt.Wait{w}}
	must(t, rec.Validate())
	return rec
}

func approvedWorld(t *testing.T) *world {
	return &world{
		rec:      run(t, candidate, "approved"),
		policy:   gitauth.Policy{DefaultBranch: "main", IntegrationPipeline: "delivery"},
		pipeline: "delivery",
		obs:      gitauth.Observation{Current: tip, FastForward: true},
	}
}

func request() gitauth.Request {
	return gitauth.Request{Scope: scope, Repository: repoID, Run: runID, Step: stepName,
		Update: gitv1.RefUpdate{Ref: ref, Old: tip, New: candidate}}
}

func yes() *bool { b := true; return &b }

// Test_unit_git_integration_authority is the accepting path: the run's pipeline
// is the integration pipeline, the integration step is running, the review wait
// was approved for exactly the pushed commit, and the update fast-forwards the
// protected default branch.
func Test_unit_git_integration_authority(t *testing.T) {
	ctx := context.Background()
	w := approvedWorld(t)
	az := w.authorizer()

	auth, err := az.Issue(ctx, request())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	want := gitauth.Authority{Scope: scope, Repository: repoID, Run: runID, Step: stepName, Ref: ref, Old: tip, New: candidate}
	if auth != want {
		t.Fatalf("authority = %+v, want %+v", auth, want)
	}

	t.Run("it is scoped to the exact ref and commit pair", func(t *testing.T) {
		exact := gitv1.RefUpdate{Ref: ref, Old: tip, New: candidate}
		if !auth.Permits(exact) {
			t.Fatal("the named update is not permitted")
		}
		for name, u := range map[string]gitv1.RefUpdate{
			"another candidate": {Ref: ref, Old: tip, New: oid("c")},
			"another old value": {Ref: ref, Old: oid("d"), New: candidate},
			"another ref":       {Ref: "refs/heads/release", Old: tip, New: candidate},
			"a delete":          {Ref: ref, Old: tip, New: strings.Repeat("0", 40)},
		} {
			if auth.Permits(u) {
				t.Errorf("%s is permitted", name)
			}
			if err := az.Recheck(ctx, auth, u, gitv1.GitFact{Ref: u.Ref, FastForward: yes()}); !hasCode(err, gitauth.CodeOutsideAuthority) {
				t.Errorf("%s: recheck = %v", name, err)
			}
		}
		if (gitauth.Authority{}).Permits(gitv1.RefUpdate{}) {
			t.Error("the zero authority permits an update")
		}
	})

	t.Run("it is rechecked at use", func(t *testing.T) {
		if err := az.Recheck(ctx, auth, want2(), gitv1.GitFact{Ref: ref, FastForward: yes()}); err != nil {
			t.Fatalf("recheck: %v", err)
		}
		// The worker did not find the fast-forward: refused at use.
		no := false
		if err := az.Recheck(ctx, auth, want2(), gitv1.GitFact{Ref: ref, FastForward: &no}); !hasCode(err, gitauth.CodeNotFastForward) {
			t.Errorf("not a fast-forward at use: %v", err)
		}
		if err := az.Recheck(ctx, auth, want2(), gitv1.GitFact{Ref: ref}); !hasCode(err, gitauth.CodeNotFastForward) {
			t.Errorf("no fast-forward fact at use: %v", err)
		}
	})

	t.Run("authority ends with the step", func(t *testing.T) {
		w := approvedWorld(t)
		az := w.authorizer()
		auth, err := az.Issue(ctx, request())
		must(t, err)
		must(t, w.rec.Run.FinishStep(stepName, rt.StepSucceeded, rt.ReasonNone, rt.Result{Code: rt.ResultSucceeded}))
		if err := az.Recheck(ctx, auth, want2(), gitv1.GitFact{Ref: ref, FastForward: yes()}); !hasCode(err, gitauth.CodeStepIneligible) {
			t.Errorf("settled step: %v", err)
		}
	})

	t.Run("authority ends with the run", func(t *testing.T) {
		w := approvedWorld(t)
		az := w.authorizer()
		auth, err := az.Issue(ctx, request())
		must(t, err)
		w.rec.Run.RequestCancel()
		if err := az.Recheck(ctx, auth, want2(), gitv1.GitFact{Ref: ref, FastForward: yes()}); !hasCode(err, gitauth.CodeRunInactive) {
			t.Errorf("cancelling run: %v", err)
		}
	})

	t.Run("a moved branch outdates the granted candidate and a replay is refused", func(t *testing.T) {
		w := approvedWorld(t)
		az := w.authorizer()
		auth, err := az.Issue(ctx, request())
		must(t, err)
		// The push lands: the worker's ref is now the candidate. The same update
		// again expects the old tip, which the ref no longer has.
		moved := gitv1.RefUpdate{Ref: ref, Old: candidate, New: candidate}
		if err := az.Recheck(ctx, auth, moved, gitv1.GitFact{Ref: ref, FastForward: yes()}); err == nil {
			t.Error("a replayed push was accepted")
		}
		// The branch moved before the push: the worker reports the new tip as the
		// old value, which is not the pair the authority names.
		other := gitv1.RefUpdate{Ref: ref, Old: oid("e"), New: candidate}
		if err := az.Recheck(ctx, auth, other, gitv1.GitFact{Ref: ref, FastForward: yes()}); !hasCode(err, gitauth.CodeOutsideAuthority) {
			t.Errorf("stale candidate after the branch moved: %v", err)
		}
		// A new authority for the moved branch needs a new wait and decision.
		w.obs = gitauth.Observation{Current: oid("e"), FastForward: true}
		recomputed := oid("f")
		r := request()
		r.Update = gitv1.RefUpdate{Ref: ref, Old: oid("e"), New: recomputed}
		if _, err := az.Issue(ctx, r); !hasCode(err, gitauth.CodeCandidateMismatch) {
			t.Errorf("a recomputed candidate under the earlier approval: %v", err)
		}
		w.rec = run(t, recomputed, "approved")
		if _, err := az.Issue(ctx, r); err != nil {
			t.Errorf("a fresh approval for the recomputed candidate: %v", err)
		}
	})

	t.Run("an authority from another tenant's run is unknown", func(t *testing.T) {
		foreign := auth
		foreign.Scope = domain.Scope{TenantID: "t2", ProjectID: "p1"}
		if err := az.Recheck(ctx, foreign, want2(), gitv1.GitFact{Ref: ref, FastForward: yes()}); !hasCode(err, gitauth.CodeUnknown) {
			t.Errorf("foreign scope: %v", err)
		}
	})
}

func want2() gitv1.RefUpdate { return gitv1.RefUpdate{Ref: ref, Old: tip, New: candidate} }

func hasCode(err error, c gitauth.Code) bool {
	code, ok := gitauth.IsRefusal(err)
	return ok && code == c
}

// Test_unit_git_authority_refusals has one case for each way the decision must
// refuse.
func Test_unit_git_authority_refusals(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		mutate func(t *testing.T, w *world, r *gitauth.Request)
		want   gitauth.Code
		reason string
	}{
		{"wrong pipeline", func(_ *testing.T, w *world, _ *gitauth.Request) { w.pipeline = "ci" },
			gitauth.CodeWrongPipeline, gitv1.ReasonIntegrationMismatch},
		{"no integration pipeline configured", func(_ *testing.T, w *world, _ *gitauth.Request) { w.policy.IntegrationPipeline = "" },
			gitauth.CodeWrongPipeline, gitv1.ReasonIntegrationMismatch},
		{"ineligible step: an earlier step", func(_ *testing.T, _ *world, r *gitauth.Request) { r.Step = "implement" },
			gitauth.CodeStepIneligible, gitv1.ReasonIntegrationMismatch},
		{"ineligible step: the review wait", func(_ *testing.T, _ *world, r *gitauth.Request) { r.Step = "review" },
			gitauth.CodeStepIneligible, gitv1.ReasonIntegrationMismatch},
		{"ineligible step: not in the plan", func(_ *testing.T, _ *world, r *gitauth.Request) { r.Step = "publish" },
			gitauth.CodeStepIneligible, gitv1.ReasonIntegrationMismatch},
		{"ineligible step: the protected step is not running", func(t *testing.T, w *world, _ *gitauth.Request) {
			w.rec = run(t, candidate, "")
		}, gitauth.CodeStepIneligible, gitv1.ReasonIntegrationMismatch},
		{"missing wait", func(t *testing.T, w *world, _ *gitauth.Request) { w.rec.Waits = nil },
			gitauth.CodeWaitMissing, gitv1.ReasonIntegrationMismatch},
		{"non-approving outcome: rejection", func(t *testing.T, w *world, _ *gitauth.Request) {
			w.rec = run(t, candidate, "rejected")
		}, gitauth.CodeNotApproved, gitv1.ReasonIntegrationMismatch},
		{"non-approving outcome: another declared outcome", func(t *testing.T, w *world, _ *gitauth.Request) {
			w.rec = run(t, candidate, "needs_work")
		}, gitauth.CodeNotApproved, gitv1.ReasonIntegrationMismatch},
		{"non-approving outcome: still open", func(t *testing.T, w *world, _ *gitauth.Request) {
			w.rec = run(t, candidate, "")
			// even a step forced to running cannot make an open wait approve
			w.rec.Run.Steps[2].State = rt.StepRunning
			w.rec.Run.Steps[2].Attempt = 1
		}, gitauth.CodeNotApproved, gitv1.ReasonIntegrationMismatch},
		{"non-approving outcome: expired", func(t *testing.T, w *world, _ *gitauth.Request) {
			w.rec = run(t, candidate, "")
			wt := w.rec.Waits[0]
			_, err := wt.Expire(t0.Add(2 * time.Hour))
			must(t, err)
			w.rec.Waits[0] = wt
			w.rec.Run.Steps[1].State = rt.StepSucceeded
			w.rec.Run.Steps[2].State = rt.StepRunning
			w.rec.Run.Steps[2].Attempt = 1
		}, gitauth.CodeNotApproved, gitv1.ReasonIntegrationMismatch},
		{"wrong candidate correlation", func(t *testing.T, w *world, _ *gitauth.Request) {
			w.rec = run(t, oid("9"), "approved")
		}, gitauth.CodeCandidateMismatch, gitv1.ReasonIntegrationMismatch},
		{"missing candidate correlation", func(t *testing.T, w *world, _ *gitauth.Request) {
			w.rec.Waits[0].Correlation = map[string]string{"candidate": ""}
		}, gitauth.CodeCandidateMismatch, gitv1.ReasonIntegrationMismatch},
		{"unprotected ref: another branch", func(_ *testing.T, _ *world, r *gitauth.Request) { r.Update.Ref = "refs/heads/feature" },
			gitauth.CodeRefUnprotected, gitv1.ReasonRefProtected},
		{"unprotected ref: a tag", func(_ *testing.T, _ *world, r *gitauth.Request) { r.Update.Ref = "refs/tags/v1" },
			gitauth.CodeRefUnprotected, gitv1.ReasonRefProtected},
		{"unprotected ref: the default branch renamed", func(_ *testing.T, w *world, _ *gitauth.Request) { w.policy.DefaultBranch = "trunk" },
			gitauth.CodeRefUnprotected, gitv1.ReasonRefProtected},
		{"creating the ref", func(_ *testing.T, w *world, r *gitauth.Request) {
			r.Update.Old = strings.Repeat("0", 40)
			w.obs.Current = r.Update.Old
		}, gitauth.CodeMalformed, gitv1.ReasonRefProtected},
		{"expected-old mismatch", func(_ *testing.T, w *world, _ *gitauth.Request) { w.obs.Current = oid("e") },
			gitauth.CodeExpectedOldChanged, gitv1.ReasonIntegrationMismatch},
		{"non-fast-forward", func(_ *testing.T, w *world, _ *gitauth.Request) { w.obs.FastForward = false },
			gitauth.CodeNotFastForward, gitv1.ReasonNotFastForward},
		{"stale candidate: the branch moved after the approval", func(_ *testing.T, w *world, r *gitauth.Request) {
			// The branch moved to e; the approved candidate (built on the old tip)
			// no longer descends from it, and a recomputed one has no approval.
			w.obs = gitauth.Observation{Current: oid("e"), FastForward: false}
			r.Update.Old = oid("e")
		}, gitauth.CodeNotFastForward, gitv1.ReasonNotFastForward},
		{"stale candidate: recomputed after the branch moved", func(_ *testing.T, w *world, r *gitauth.Request) {
			w.obs = gitauth.Observation{Current: oid("e"), FastForward: true}
			r.Update = gitv1.RefUpdate{Ref: ref, Old: oid("e"), New: oid("f")}
		}, gitauth.CodeCandidateMismatch, gitv1.ReasonIntegrationMismatch},
		{"task status string", func(t *testing.T, w *world, _ *gitauth.Request) {
			// The wait is open; a task that says approved does not change that.
			w.rec = run(t, candidate, "")
			w.rec.Run.Steps[2].State = rt.StepRunning
			w.rec.Run.Steps[2].Attempt = 1
			w.taskStatus = "approved"
		}, gitauth.CodeNotApproved, gitv1.ReasonIntegrationMismatch},
		{"bare event", func(t *testing.T, w *world, _ *gitauth.Request) {
			// A wait.resolved event, or a container's approved=true, without the
			// durable decision is nothing.
			w.rec = run(t, candidate, "")
			w.rec.Run.Steps[2].State = rt.StepRunning
			w.rec.Run.Steps[2].Attempt = 1
			w.events = append(w.events, "wait.resolved", "approved=true")
		}, gitauth.CodeNotApproved, gitv1.ReasonIntegrationMismatch},
		{"run cancelling", func(_ *testing.T, w *world, _ *gitauth.Request) { w.rec.Run.RequestCancel() },
			gitauth.CodeRunInactive, gitv1.ReasonRunInactive},
		{"run of another project", func(_ *testing.T, _ *world, r *gitauth.Request) {
			r.Scope = domain.Scope{TenantID: "t1", ProjectID: "p2"}
		}, gitauth.CodeUnknown, gitv1.ReasonIntegrationMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := approvedWorld(t)
			r := request()
			c.mutate(t, w, &r)
			_, err := w.authorizer().Issue(ctx, r)
			if err == nil {
				t.Fatal("authority was issued")
			}
			if !hasCode(err, c.want) {
				t.Fatalf("error = %v, want code %s", err, c.want)
			}
			var ref *gitauth.Refusal
			if !errors.As(err, &ref) || ref.PushReason() != c.reason || !gitv1.IsPushRefusalReason(ref.PushReason()) {
				t.Fatalf("push reason = %v, want %s", ref, c.reason)
			}
		})
	}

	t.Run("an unreadable run is an error, never authority", func(t *testing.T) {
		w := approvedWorld(t)
		az := w.authorizer()
		az.Runs = failingRuns{}
		_, err := az.Issue(ctx, request())
		if err == nil {
			t.Fatal("authority was issued")
		}
		if _, ok := gitauth.IsRefusal(err); ok {
			t.Fatalf("a read failure is reported as a refusal: %v", err)
		}
	})

	t.Run("the refusal text names no commit or answer", func(t *testing.T) {
		w := approvedWorld(t)
		w.rec = run(t, oid("9"), "approved")
		_, err := w.authorizer().Issue(ctx, request())
		if err == nil || strings.Contains(err.Error(), oid("9")) || strings.Contains(err.Error(), candidate) {
			t.Fatalf("refusal text: %v", err)
		}
	})
}

type failingRuns struct{}

func (failingRuns) Load(context.Context, domain.Scope, domain.RunID) (rt.RunRecord, error) {
	return rt.RunRecord{}, errors.New("state store unavailable")
}
