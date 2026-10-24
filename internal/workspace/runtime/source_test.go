package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	wsruntime "github.com/optimiware/kodama/internal/workspace/runtime"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

func asRequest(in workspace.ProposeInput) runtimev1.SourceProposalRequest {
	return runtimev1.SourceProposalRequest{
		Commit: runtimev1.GitCommit(in.Commit), ExpectedTip: runtimev1.GitCommit(in.ExpectedTip), BundleArtifactID: in.BundleArtifactID,
	}
}

func contractError(t *testing.T, err error) *wsruntime.Error {
	t.Helper()
	var ce *wsruntime.Error
	if !errors.As(err, &ce) {
		t.Fatalf("want a contract error, got %v", err)
	}
	// The rendered body is a valid frozen ErrorResponse.
	raw, _ := json.Marshal(ce.Body("req_01"))
	if _, derr := runtimev1.DecodeResponse[runtimev1.ErrorResponse](raw); derr != nil {
		t.Fatalf("%v: %s", derr, raw)
	}
	return ce
}

// Test_source_runtime_compatibility: the frozen accepted, pending and conflict
// shapes, details.observedTip, repeated proposals and no-change writers remain
// compatible with the runtime contract (T-003) and the entrypoint (T-016).
func Test_source_runtime_compatibility(t *testing.T) {
	ctx := context.Background()

	t.Run("accepted, with the exact frozen shape", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		src, err := wsruntime.NewSource(e.Svc)
		if err != nil {
			t.Fatal(err)
		}
		res, err := src.Propose(ctx, w.Scope, "att_1", asRequest(in))
		if err != nil || res.Status != 200 {
			t.Fatalf("%+v %v", res, err)
		}
		raw, _ := json.Marshal(res.Response)
		got, err := runtimev1.DecodeResponse[runtimev1.SourceProposalResponse](raw)
		if err != nil || got.Status != runtimev1.PublishAccepted || got.Commit != runtimev1.GitCommit(in.Commit) || got.AcceptedCommit == nil || *got.AcceptedCommit != got.Commit {
			t.Fatalf("%+v %v (%s)", got, err, raw)
		}
		// A repeated proposal observes the same publication.
		again, err := src.Propose(ctx, w.Scope, "att_1", asRequest(in))
		if err != nil || again.Status != 200 || again.Response != res.Response && *again.Response.AcceptedCommit != *res.Response.AcceptedCommit {
			t.Fatalf("%+v %v", again, err)
		}
	})

	t.Run("pending then accepted after a lost response", func(t *testing.T) {
		e := wt.New(t, wt.Config{Git: func(o *gitad.Options) { o.CommandTimeout = 3 * time.Second }})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		src, _ := wsruntime.NewSource(e.Svc)
		hold := e.Git.HoldReceivePack()
		res, err := src.Propose(ctx, w.Scope, "att_1", asRequest(in))
		if err != nil || res.Status != 202 {
			t.Fatalf("%+v %v", res, err)
		}
		raw, _ := json.Marshal(res.Response)
		got, err := runtimev1.DecodeResponse[runtimev1.SourceProposalResponse](raw)
		if err != nil || got.Status != runtimev1.PublishPending || got.AcceptedCommit != nil {
			t.Fatalf("%+v %v (%s)", got, err, raw)
		}
		hold.Release()
		e.Git.WaitIdle(t)
		// The entrypoint repeats the identical proposal until it is accepted.
		e.Clock.Advance(2 * time.Hour)
		res, err = src.Propose(ctx, w.Scope, "att_1", asRequest(in))
		if err != nil || res.Status != 200 || res.Response.Status != runtimev1.PublishAccepted {
			t.Fatalf("%+v %v", res, err)
		}
	})

	t.Run("conflict carries details.observedTip", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		human := e.Repo.HumanPush(t, w.WorkBranch, "human", gitfx.File{Path: "h.txt", Content: "h"})
		src, _ := wsruntime.NewSource(e.Svc)
		_, err := src.Propose(ctx, w.Scope, "att_1", asRequest(in))
		ce := contractError(t, err)
		if ce.Status != 409 || ce.Code != runtimev1.CodePublicationConflict || ce.Details["observedTip"] != human {
			t.Fatalf("%+v", ce)
		}
		// Repeating gives the same conflict, never a retry.
		_, err = src.Propose(ctx, w.Scope, "att_1", asRequest(in))
		if ce := contractError(t, err); ce.Details["observedTip"] != human {
			t.Fatalf("%+v", ce)
		}
	})

	t.Run("authority errors use the contract's codes and the attempt's own scope", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		e.Writer(w, "att_1")
		in := e.Proposal(w, "att_1", e.Repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "a"})
		src, _ := wsruntime.NewSource(e.Svc)

		_, err := src.Propose(ctx, wt.ScopeB, "att_1", asRequest(in))
		if ce := contractError(t, err); ce.Status != 404 || ce.Code != domain.CodeNotFound {
			t.Fatalf("foreign scope: %+v", ce)
		}
		e.Attempts.Revoke("att_1")
		_, err = src.Propose(ctx, w.Scope, "att_1", asRequest(in))
		if ce := contractError(t, err); ce.Status != 403 || ce.Code != runtimev1.CodeAttemptRevoked {
			t.Fatalf("revoked: %+v", ce)
		}
		_, err = src.Propose(ctx, w.Scope, "att_1", runtimev1.SourceProposalRequest{Commit: "short", ExpectedTip: runtimev1.GitCommit(in.ExpectedTip), BundleArtifactID: in.BundleArtifactID})
		if ce := contractError(t, err); ce.Status != 400 {
			t.Fatalf("malformed: %+v", ce)
		}
	})

	t.Run("a no-change writer proposes nothing and is verified against its expected tip", func(t *testing.T) {
		e := wt.New(t, wt.Config{})
		w := e.Ready(wt.ScopeA, "wi_1")
		w = e.Writer(w, "att_1")
		src, _ := wsruntime.NewSource(e.Svc)
		// The entrypoint never proposes an unchanged commit; if it did, it is refused as invalid.
		_, err := src.Propose(ctx, w.Scope, "att_1", runtimev1.SourceProposalRequest{
			Commit: runtimev1.GitCommit(e.Repo.Initial), ExpectedTip: runtimev1.GitCommit(e.Repo.Initial), BundleArtifactID: "art_none"})
		if ce := contractError(t, err); ce.Status != 404 && ce.Status != 400 {
			t.Fatalf("%+v", ce)
		}
		// Its reported sourceCommit is the expected tip, which the result path verifies.
		if _, err := e.Svc.SettleWriter(ctx, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: "att_1", Outcome: workspace.OutcomeSucceeded, SourceCommit: w.Writer.ExpectedTip}); err != nil {
			t.Fatal(err)
		}
	})
}
