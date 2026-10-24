package resultstest

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/results"
)

// World is the fixture surface a Contract subject provides: it creates the
// attempts, grants, artifacts and branch state the use cases act on. The
// memory adapter implements it over memdb; the PostgreSQL composition (see
// postgres/contract_test.go) implements it over the execution, runtime-authority,
// artifact and workspace fixtures, a real Git server and S3.
type World interface {
	// NewAttempt creates an active attempt in scope with a pinned context and a
	// grant covering the actions its access permits.
	NewAttempt(tb testing.TB, scope domain.Scope, o AttemptOptions) Attempt
	// Bearer returns the attempt's credential for the action.
	Bearer(tb testing.TB, a Attempt, action results.Action) string
	// Artifact registers a file artifact of the attempt and returns its ID.
	Artifact(tb testing.TB, a Attempt, s ArtifactSpec) domain.ArtifactID
	// Bundle returns a new commit on the writer's pinned expected tip and a
	// completed bundle artifact of the attempt that carries it.
	Bundle(tb testing.TB, a Attempt) (runtimev1.GitCommit, domain.ArtifactID)
	// Revoke cancels the attempt and revokes its grants, as execution's
	// cancellation transaction does.
	Revoke(tb testing.TB, a Attempt)
	// MoveTip advances the writer's source branch as a human push would and
	// returns the new tip.
	MoveTip(tb testing.TB, a Attempt) runtimev1.GitCommit
	// Tip reads the writer's source branch on the remote.
	Tip(tb testing.TB, a Attempt) runtimev1.GitCommit
}

// Scopes are the tenants and projects a subject has seeded: two projects of
// one tenant and a project of another tenant. A subject provisions them
// because only it knows what its fixtures can create.
type Scopes struct{ A, A2, B domain.Scope }

// Subject is one adapter set under the Contract.
type Subject struct {
	Deps   results.Dependencies
	World  World
	Scopes Scopes
}

// Scopes the memory subject uses.
var (
	ScopeA  = domain.Scope{TenantID: "t081_acme", ProjectID: "p081_web"}
	ScopeA2 = domain.Scope{TenantID: "t081_acme", ProjectID: "p081_api"}
	ScopeB  = domain.Scope{TenantID: "t081_globex", ProjectID: "p081_ops"}
)

// Completed returns a completed result with the given outputs.
func Completed(outputs map[string]runtimev1.Value, source *runtimev1.GitCommit, diag ...domain.ArtifactID) runtimev1.Result {
	if diag == nil {
		diag = []domain.ArtifactID{}
	}
	zero := int64(0)
	return runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, SourceCommit: source, Outputs: outputs, DiagnosticArtifactIDs: diag}
}

// Failed returns a failed result with diagnostics.
func Failed(diag ...domain.ArtifactID) runtimev1.Result {
	if diag == nil {
		diag = []domain.ArtifactID{}
	}
	one, reason := int64(1), "command_failed"
	return runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: &one, Reason: &reason, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: diag}
}

// Request pairs a result with its canonical hash.
func Request(tb testing.TB, res runtimev1.Result) runtimev1.ResultRequest {
	tb.Helper()
	h, err := runtimev1.ResultHash(res)
	if err != nil {
		tb.Fatal(err)
	}
	return runtimev1.ResultRequest{ResultHash: h, Result: res}
}

// Contract is the behavior every adapter set of the results ports promises
// (SD-13): duplicate callbacks converge on one receipt, a different result
// conflicts, stale, foreign and revoked attempts are denied before any effect,
// artifact references stay inside the attempt's scope, and a publication that
// is not settled never reads as success. It asserts what callers rely on, not
// how a backend does it: lock order and isolation levels belong to the
// PostgreSQL checkpoints (SD-14).
func Contract(t *testing.T, open func(testing.TB) Subject) {
	t.Helper()
	ctx := context.Background()
	run := func(name string, body func(t *testing.T, svc *results.Service, s Subject)) {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			svc, err := results.NewService(s.Deps, results.Options{})
			if err != nil {
				t.Fatal(err)
			}
			body(t, svc, s)
		})
	}
	submit := func(svc *results.Service, s Subject, a Attempt, req runtimev1.ResultRequest) (runtimev1.ResultResponse, error) {
		return svc.Submit(ctx, results.SubmitCommand{Attempt: a.ID, Bearer: s.World.Bearer(t, a, results.ActionResultSubmit), Request: req})
	}
	propose := func(svc *results.Service, s Subject, a Attempt, req runtimev1.SourceProposalRequest) (results.SourceOutcome, error) {
		return svc.ProposeSource(ctx, results.ProposeCommand{Attempt: a.ID, Bearer: s.World.Bearer(t, a, results.ActionSourcePropose), Request: req})
	}
	accepted := func(s Subject, a Attempt) (runtimev1.Digest, bool) {
		h, ok, err := s.Deps.Finalizer.Accepted(ctx, a.Scope, a.ID)
		if err != nil {
			t.Fatalf("accepted: %v", err)
		}
		return h, ok
	}
	report := func(id domain.ArtifactID) map[string]runtimev1.Value {
		return map[string]runtimev1.Value{"report": runtimev1.StringValue(string(id))}
	}
	writer := func(s Subject, scope domain.Scope) Attempt {
		return s.World.NewAttempt(t, scope, AttemptOptions{Writer: true, NoOutputs: true})
	}
	// proposal names the writer's reserved expected tip, as the entrypoint does.
	proposal := func(a Attempt, commit runtimev1.GitCommit, bundle domain.ArtifactID) runtimev1.SourceProposalRequest {
		return runtimev1.SourceProposalRequest{Commit: commit, ExpectedTip: *a.Pinned.Workspace.Source.ExpectedTip, BundleArtifactID: bundle}
	}

	run("duplicate callbacks converge on one receipt", func(t *testing.T, svc *results.Service, s Subject) {
		a := s.World.NewAttempt(t, s.Scopes.A, AttemptOptions{})
		art := s.World.Artifact(t, a, ArtifactSpec{})
		req := Request(t, Completed(report(art), nil))
		first, err := submit(svc, s, a, req)
		if err != nil || first.AttemptStatus != runtimev1.AttemptSucceeded || first.ResultHash != req.ResultHash || first.AcceptedAt == "" {
			t.Fatalf("first: %+v %v", first, err)
		}
		again, err := submit(svc, s, a, req)
		if err != nil || again != first {
			t.Fatalf("a replay must repeat the original acknowledgment: %+v, then %+v (%v)", first, again, err)
		}
		var wg sync.WaitGroup
		got := make([]runtimev1.ResultResponse, 8)
		errs := make([]error, 8)
		for i := range got {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got[i], errs[i] = submit(svc, s, a, req)
			}()
		}
		wg.Wait()
		for i := range got {
			if errs[i] != nil || got[i] != first {
				t.Fatalf("concurrent duplicate %d: %+v %v", i, got[i], errs[i])
			}
		}
		other := Request(t, Failed())
		if _, err := submit(svc, s, a, other); !errors.Is(err, results.ErrResultConflict) {
			t.Fatalf("a different result for an accepted attempt: %v", err)
		}
		if h, ok := accepted(s, a); !ok || h != req.ResultHash {
			t.Fatalf("a conflicting result changed the receipt: %v %v", h, ok)
		}
	})

	run("a hash that does not cover the result is rejected before any effect", func(t *testing.T, svc *results.Service, s Subject) {
		a := s.World.NewAttempt(t, s.Scopes.A, AttemptOptions{})
		art := s.World.Artifact(t, a, ArtifactSpec{})
		req := Request(t, Completed(report(art), nil))
		req.ResultHash = runtimev1.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")
		if _, err := submit(svc, s, a, req); !errors.Is(err, results.ErrResultHashMismatch) {
			t.Fatalf("%v", err)
		}
		if _, ok := accepted(s, a); ok {
			t.Fatal("a mismatched hash was accepted")
		}
	})

	run("a missing declared output fails the attempt durably and replays identically", func(t *testing.T, svc *results.Service, s Subject) {
		a := s.World.NewAttempt(t, s.Scopes.A, AttemptOptions{})
		req := Request(t, Completed(map[string]runtimev1.Value{}, nil))
		first, err := submit(svc, s, a, req)
		if err != nil || first.AttemptStatus != runtimev1.AttemptFailed {
			t.Fatalf("a result without its declared output must be a failed attempt, never success: %+v %v", first, err)
		}
		if again, err := submit(svc, s, a, req); err != nil || again != first {
			t.Fatalf("replay: %+v %v", again, err)
		}
	})

	run("artifact references stay inside the attempt's scope", func(t *testing.T, svc *results.Service, s Subject) {
		a := s.World.NewAttempt(t, s.Scopes.A, AttemptOptions{})
		sibling := s.World.NewAttempt(t, s.Scopes.A, AttemptOptions{})
		otherProject := s.World.NewAttempt(t, s.Scopes.A2, AttemptOptions{})
		otherTenant := s.World.NewAttempt(t, s.Scopes.B, AttemptOptions{})
		for name, foreign := range map[string]domain.ArtifactID{
			"another attempt":  s.World.Artifact(t, sibling, ArtifactSpec{}),
			"another project":  s.World.Artifact(t, otherProject, ArtifactSpec{}),
			"another tenant":   s.World.Artifact(t, otherTenant, ArtifactSpec{}),
			"an unknown ident": "art_does_not_exist",
		} {
			if _, err := submit(svc, s, a, Request(t, Completed(report(foreign), nil))); !errors.Is(err, results.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
		incomplete := s.World.Artifact(t, a, ArtifactSpec{Incomplete: true})
		if _, err := submit(svc, s, a, Request(t, Completed(report(incomplete), nil))); !errors.Is(err, results.ErrArtifactIncomplete) {
			t.Errorf("an unfinished upload: %v", err)
		}
		if _, ok := accepted(s, a); ok {
			t.Fatal("a rejected reference left an accepted result")
		}
		// Diagnostics follow the same rules, independently of outputs.
		if _, err := submit(svc, s, a, Request(t, Failed(s.World.Artifact(t, sibling, ArtifactSpec{Role: runtimev1.RoleDiagnostic})))); !errors.Is(err, results.ErrNotFound) {
			t.Errorf("a foreign diagnostic: %v", err)
		}
		diag := s.World.Artifact(t, a, ArtifactSpec{Role: runtimev1.RoleDiagnostic})
		if resp, err := submit(svc, s, a, Request(t, Failed(diag))); err != nil || resp.AttemptStatus != runtimev1.AttemptFailed {
			t.Fatalf("an own diagnostic: %+v %v", resp, err)
		}
	})

	run("stale, foreign and revoked attempts are denied before any effect", func(t *testing.T, svc *results.Service, s Subject) {
		a := s.World.NewAttempt(t, s.Scopes.A, AttemptOptions{})
		b := s.World.NewAttempt(t, s.Scopes.A, AttemptOptions{})
		c := s.World.NewAttempt(t, s.Scopes.B, AttemptOptions{})
		art := s.World.Artifact(t, a, ArtifactSpec{})
		req := Request(t, Completed(report(art), nil))

		for name, bearer := range map[string]string{
			"another attempt's token": s.World.Bearer(t, b, results.ActionResultSubmit),
			"another tenant's token":  s.World.Bearer(t, c, results.ActionResultSubmit),
			"a malformed token":       "rt_not_a_token",
			"an empty token":          "",
		} {
			_, err := svc.Submit(ctx, results.SubmitCommand{Attempt: a.ID, Bearer: bearer, Request: req})
			if !errors.Is(err, results.ErrUnauthenticated) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, ok := accepted(s, a); ok {
			t.Fatal("a denied callback was accepted")
		}

		s.World.Revoke(t, a)
		if _, err := submit(svc, s, a, req); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("a revoked attempt's result: %v", err)
		}
		w := writer(s, s.Scopes.A)
		commit, bundle := s.World.Bundle(t, w)
		tip := s.World.Tip(t, w)
		s.World.Revoke(t, w)
		if _, err := propose(svc, s, w, proposal(w, commit, bundle)); !errors.Is(err, results.ErrAttemptRevoked) {
			t.Fatalf("a revoked attempt's source proposal: %v", err)
		}
		if s.World.Tip(t, w) != tip {
			t.Fatal("a revoked attempt moved the remote")
		}
		if _, ok := accepted(s, a); ok {
			t.Fatal("a revoked attempt accepted a result")
		}
	})

	run("a pinned context is immutable whatever a caller does to its copy", func(t *testing.T, svc *results.Service, s Subject) {
		a := s.World.NewAttempt(t, s.Scopes.A, AttemptOptions{})
		art := s.World.Artifact(t, a, ArtifactSpec{})
		want, err := s.Deps.Contexts.Pinned(ctx, a.Scope, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(want, a.Pinned) {
			t.Fatalf("the pinned context differs from the dispatched one:\n%+v\n%+v", want, a.Pinned)
		}
		vandalize := func(c *runtimev1.Context) {
			c.Outputs = append(c.Outputs[:0], runtimev1.OutputDecl{Name: "other", Type: runtimev1.OutputString})
			if c.Command.Env == nil {
				c.Command.Env = map[string]string{}
			}
			c.Command.Env["INJECTED"] = "1"
			if len(c.Command.Args) > 0 {
				c.Command.Args[0] = "--changed"
			}
			c.Command.Command = append(c.Command.Command[:0], "/bin/evil")
			if c.Workspace.Source != nil {
				c.Workspace.Source.Commit = "ffffffffffffffffffffffffffffffffffffffff"
				if c.Workspace.Source.Branch != nil {
					*c.Workspace.Source.Branch = "elsewhere"
				}
			}
			if c.Inputs == nil {
				c.Inputs = map[string]runtimev1.Value{}
			}
			c.Inputs["x"] = runtimev1.StringValue("y")
		}
		// Neither the copy returned by the port nor the fixture's own alias may
		// reach the stored document.
		got, err := s.Deps.Contexts.Pinned(ctx, a.Scope, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		vandalize(&got)
		vandalize(&a.Pinned)
		after, err := s.Deps.Contexts.Pinned(ctx, a.Scope, a.ID)
		if err != nil || !reflect.DeepEqual(after, want) {
			t.Fatalf("a caller changed the stored context (%v):\n%+v\n%+v", err, after, want)
		}
		// Finalization still judges the result against the original document.
		resp, err := submit(svc, s, a, Request(t, Completed(report(art), nil)))
		if err != nil || resp.AttemptStatus != runtimev1.AttemptSucceeded {
			t.Fatalf("finalization used a modified context: %+v %v", resp, err)
		}
	})

	run("source proposals publish once and report a lost race with the observed tip", func(t *testing.T, svc *results.Service, s Subject) {
		reader := s.World.NewAttempt(t, s.Scopes.A, AttemptOptions{})
		const unused = domain.ArtifactID("art_unused")
		if _, err := propose(svc, s, reader, runtimev1.SourceProposalRequest{Commit: "3333333333333333333333333333333333333333",
			ExpectedTip: "1111111111111111111111111111111111111111", BundleArtifactID: unused}); !errors.Is(err, results.ErrForbidden) {
			t.Fatalf("a reader may not propose source: %v", err)
		}

		w := writer(s, s.Scopes.A)
		commit, bundle := s.World.Bundle(t, w)
		req := proposal(w, commit, bundle)
		out, err := propose(svc, s, w, req)
		if err != nil || out.HTTPStatus != 200 || out.Response.Status != runtimev1.PublishAccepted || out.Response.AcceptedCommit == nil || *out.Response.AcceptedCommit != commit {
			t.Fatalf("accepted: %+v %v", out, err)
		}
		if s.World.Tip(t, w) != commit {
			t.Fatalf("the accepted commit is the branch tip: %s", s.World.Tip(t, w))
		}
		if again, err := propose(svc, s, w, req); err != nil || again.HTTPStatus != out.HTTPStatus || again.Response.Status != out.Response.Status || again.Response.AcceptedCommit == nil || *again.Response.AcceptedCommit != commit {
			t.Fatalf("an identical repeat observes the same publication: %+v %v", again, err)
		}
		changed := req
		changed.Commit = "4444444444444444444444444444444444444444"
		if _, err := propose(svc, s, w, changed); !errors.Is(err, results.ErrPublicationConflict) {
			t.Fatalf("a changed proposal: %v", err)
		}
		// The settled commit lets the writer succeed.
		src := commit
		resp, err := submit(svc, s, w, Request(t, Completed(map[string]runtimev1.Value{}, &src)))
		if err != nil || resp.AttemptStatus != runtimev1.AttemptSucceeded {
			t.Fatalf("a settled writer: %+v %v", resp, err)
		}

		racer := writer(s, s.Scopes.A)
		rc, rb := s.World.Bundle(t, racer)
		moved := s.World.MoveTip(t, racer)
		_, err = propose(svc, s, racer, proposal(racer, rc, rb))
		var pc *results.PublicationConflict
		if !errors.As(err, &pc) || pc.ObservedTip != string(moved) {
			t.Fatalf("a lost expected-tip race reports the observed tip: %v", err)
		}
		if s.World.Tip(t, racer) != moved {
			t.Fatal("the human's commit was overwritten")
		}
	})

	run("a proposal is held to the writer's reservation and bundle, with no publication or remote effect", func(t *testing.T, svc *results.Service, s Subject) {
		// The expected tip is the writer's pinned reservation. A caller that
		// replaces it with the tip a human just moved the branch to is refused.
		w := writer(s, s.Scopes.A)
		commit, bundle := s.World.Bundle(t, w)
		moved := s.World.MoveTip(t, w)
		forged := proposal(w, commit, bundle)
		forged.ExpectedTip = moved
		if _, err := propose(svc, s, w, forged); !errors.Is(err, results.ErrInvalidRequest) {
			t.Fatalf("a replaced expected tip: %v", err)
		}
		if s.World.Tip(t, w) != moved {
			t.Fatal("a refused proposal moved the remote")
		}
		// Nothing was recorded: the honest proposal still decides, and it
		// loses the race at the remote with the observed tip.
		var pc *results.PublicationConflict
		if _, err := propose(svc, s, w, proposal(w, commit, bundle)); !errors.As(err, &pc) || pc.ObservedTip != string(moved) {
			t.Fatalf("the refused proposal left a receipt behind: %v", err)
		}

		// Only a bundle is a bundle, and only this attempt's.
		// (This writer declares the output "report", so an output artifact can
		// exist for it at all.)
		w2 := s.World.NewAttempt(t, s.Scopes.A, AttemptOptions{Writer: true})
		commit2, bundle2 := s.World.Bundle(t, w2)
		tip := s.World.Tip(t, w2)
		for name, art := range map[string]domain.ArtifactID{
			"an output artifact":    s.World.Artifact(t, w2, ArtifactSpec{Role: runtimev1.RoleOutput}),
			"a diagnostic artifact": s.World.Artifact(t, w2, ArtifactSpec{Role: runtimev1.RoleDiagnostic}),
			"an unfinished bundle":  s.World.Artifact(t, w2, ArtifactSpec{Role: runtimev1.RoleBundle, Incomplete: true}),
		} {
			if _, err := propose(svc, s, w2, proposal(w2, commit2, art)); !errors.Is(err, results.ErrArtifactIncomplete) {
				t.Errorf("%s as the bundle: %v", name, err)
			}
		}
		_, foreign := s.World.Bundle(t, w)
		if _, err := propose(svc, s, w2, proposal(w2, commit2, foreign)); !errors.Is(err, results.ErrNotFound) {
			t.Errorf("another attempt's bundle: %v", err)
		}
		if s.World.Tip(t, w2) != tip {
			t.Fatal("a refused proposal moved the remote")
		}
		out, err := propose(svc, s, w2, proposal(w2, commit2, bundle2))
		if err != nil || out.HTTPStatus != 200 || s.World.Tip(t, w2) != commit2 {
			t.Fatalf("the refusals recorded nothing, so the real bundle publishes: %+v %v", out, err)
		}
	})

	run("an unpublished source commit never reads as success", func(t *testing.T, svc *results.Service, s Subject) {
		w := writer(s, s.Scopes.A)
		unpublished := runtimev1.GitCommit("5555555555555555555555555555555555555555")
		if _, err := submit(svc, s, w, Request(t, Completed(map[string]runtimev1.Value{}, &unpublished))); !errors.Is(err, results.ErrUnavailable) {
			t.Fatalf("an unsettled writer must retry, not succeed: %v", err)
		}
		if _, ok := accepted(s, w); ok {
			t.Fatal("an unpublished commit was accepted as a result")
		}
		// A writer that reports no commit violates its source rule.
		if resp, err := submit(svc, s, w, Request(t, Completed(map[string]runtimev1.Value{}, nil))); err != nil || resp.AttemptStatus != runtimev1.AttemptFailed {
			t.Fatalf("a completed writer without sourceCommit: %+v %v", resp, err)
		}
	})
}
