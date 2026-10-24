package results_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	wst "github.com/optimiware/kodama/internal/workspace/workspacetest"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

func completedWith(commit string, outputs map[string]runtimev1.Value) runtimev1.ResultRequest {
	return Completed2(commit, outputs)
}

// Completed2 builds a completed writer result.
func Completed2(commit string, outputs map[string]runtimev1.Value) runtimev1.ResultRequest {
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), SourceCommit: ptr(runtimev1.GitCommit(commit)), Outputs: outputs, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	h, err := runtimev1.ResultHash(res)
	if err != nil {
		panic(err)
	}
	return runtimev1.ResultRequest{ResultHash: h, Result: res}
}

func detail(t testing.TB, body []byte, key string) string {
	t.Helper()
	var r runtimev1.ErrorResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	s, _ := r.Error.Details[key].(string)
	return s
}

// TestPublicationConflictAndRecovery: WS-04, WS-06, WS-11, IT-06, IT-A04. A human
// CAS race, a changed proposal, a foreign or reader bundle, a malformed bundle,
// an injected credential and a failed writer: every refusal leaves the remote
// branch alone, the response carries the observed tip but no credentials, and a
// failed writer never yields a successful descendant source. Real PostgreSQL,
// S3 and Git through the real adapters; no NATS.
func TestPublicationConflictAndRecovery(t *testing.T) {
	e := New(t, Options{Artifacts: true, Git: true})
	ctx := context.Background()

	t.Run("a human push wins the compare-and-swap and the proposal is a conflict with the observed tip", func(t *testing.T) {
		p := e.NewProject(ScopeA)
		a := e.Writer(p, labelJob)
		commit, art := e.Bundle(a, "agent change", gitfx.File{Path: "agent.txt", Content: "agent"})
		human := e.Repo.HumanPush(t, e.Branch(a), "human change", gitfx.File{Path: "human.txt", Content: "human"})
		pushes := e.ReceivePacks()

		code, body := e.ProposeHTTP(a, e.Proposal(a, commit, art))
		if code != 409 || errCode(t, body) != runtimev1.CodePublicationConflict || detail(t, body, "observedTip") != human {
			t.Fatalf("%d %s", code, body)
		}
		if e.Repo.Tip(t, e.Branch(a)) != human || e.ReceivePacks() != pushes {
			t.Fatal("the human's commit was overwritten or a push was attempted")
		}
		// Repeating the proposal observes the same recorded conflict.
		if code, body := e.ProposeHTTP(a, e.Proposal(a, commit, art)); code != 409 || detail(t, body, "observedTip") != human {
			t.Fatalf("repeat: %d %s", code, body)
		}
		if e.Publications(p.Workspace) != 1 {
			t.Fatalf("%d receipts", e.Publications(p.Workspace))
		}
		// A completed result cannot succeed on top of a publication that did not.
		if code, _ := e.SubmitHTTP(a, completedWith(commit, outputsOf("v1"))); code == 200 {
			t.Fatal("a conflicted publication became a successful result")
		}
		if e.Persisted(a).Status != execution.AttemptRunning {
			t.Fatalf("attempt %s", e.Persisted(a).Status)
		}
		// The writer fails honestly and is settled as failed.
		failed := Request(t, runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(1)), Reason: ptr("publication_conflict"), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}})
		if ack, err := e.Submit(a, failed); err != nil || ack.AttemptStatus != runtimev1.AttemptFailed {
			t.Fatalf("%+v %v", ack, err)
		}
		if _, err := e.WSPlain.SelectWriterOutput(ctx, wst.Engineer, p.Scope, p.Workspace, a.ID()); err == nil {
			t.Fatal("a failed writer's source was selectable")
		}
	})

	t.Run("a changed proposal cannot replace the recorded one", func(t *testing.T) {
		p := e.NewProject(ScopeA)
		a := e.Writer(p, labelJob)
		commit, art := e.Bundle(a, "first", gitfx.File{Path: "a.txt", Content: "a"})
		if code, body := e.ProposeHTTP(a, e.Proposal(a, commit, art)); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		other, otherArt := e.Bundle(a, "second", gitfx.File{Path: "b.txt", Content: "b"})
		code, body := e.ProposeHTTP(a, e.Proposal(a, other, otherArt))
		if code != 409 || errCode(t, body) != runtimev1.CodePublicationConflict || detail(t, body, "reason") != "proposal_changed" {
			t.Fatalf("%d %s", code, body)
		}
		if e.Repo.Tip(t, e.Branch(a)) != commit || e.Publications(p.Workspace) != 1 {
			t.Fatal("the recorded publication was replaced")
		}
		// The identical repeat still observes the accepted outcome.
		if code, _ := e.ProposeHTTP(a, e.Proposal(a, commit, art)); code != 200 {
			t.Fatalf("repeat %d", code)
		}
	})

	t.Run("a reader, a foreign bundle and a non-bundle are refused without touching Git", func(t *testing.T) {
		p := e.NewProject(ScopeA)
		reader := e.Run(e.NewProject(ScopeA), Job("scan", readRun))
		writer := e.Writer(p, labelJob)
		own, ownBundle := e.Bundle(writer, "own", gitfx.File{Path: "a.txt", Content: "a"})
		_ = own
		// Another writer (another project) and another tenant's writer produce bundles.
		sibling := e.Writer(e.NewProject(ScopeA), labelJob)
		_, siblingBundle := e.Bundle(sibling, "sibling", gitfx.File{Path: "s.txt", Content: "s"})
		stranger := e.Writer(e.NewProject(ScopeB), labelJob)
		_, strangerBundle := e.Bundle(stranger, "stranger", gitfx.File{Path: "x.txt", Content: "x"})
		notBundle := e.Upload(writer, "not-a-bundle", artifacts.RoleDiagnostic, []byte("log\n"))
		pushes, tip := e.ReceivePacks(), e.Repo.Tip(t, e.Branch(writer))
		commit := e.Repo.Tip(t, e.Branch(writer)) // any well-formed commit ID

		if code, body := e.ProposeHTTP(reader, runtimev1.SourceProposalRequest{Commit: runtimev1.GitCommit(commit), ExpectedTip: runtimev1.GitCommit(commit), BundleArtifactID: ownBundle}); code != 401 {
			t.Fatalf("a reader proposed source: %d %s", code, body)
		}
		missing := "" // the answer for an ID that does not exist
		if code, body := e.ProposeHTTP(writer, e.Proposal(writer, commit, "art_does_not_exist")); code != 404 {
			t.Fatalf("%d %s", code, body)
		} else {
			missing = normalized(t, body)
		}
		for name, id := range map[string]domain.ArtifactID{"another attempt's bundle": siblingBundle, "another tenant's bundle": strangerBundle} {
			code, body := e.ProposeHTTP(writer, e.Proposal(writer, commit, id))
			if code != 404 || normalized(t, body) != missing {
				t.Fatalf("%s is distinguishable from a missing artifact: %d %s", name, code, body)
			}
		}
		if code, body := e.ProposeHTTP(writer, e.Proposal(writer, commit, notBundle)); code != 409 || errCode(t, body) != runtimev1.CodeArtifactIncomplete {
			t.Fatalf("a diagnostic used as a bundle: %d %s", code, body)
		}
		if e.Repo.Tip(t, e.Branch(writer)) != tip || e.ReceivePacks() != pushes || e.Publications(p.Workspace) != 0 {
			t.Fatal("a refused proposal reached Git or recorded a publication")
		}
	})

	t.Run("a malformed bundle is rejected, bounded and unrecorded", func(t *testing.T) {
		p := e.NewProject(ScopeA)
		a := e.Writer(p, labelJob)
		commit, _ := e.Bundle(a, "good", gitfx.File{Path: "a.txt", Content: "a"})
		garbage := e.Upload(a, "garbage-bundle", artifacts.RoleBundle, bytes.Repeat([]byte("not a git bundle "), 64))
		pushes := e.ReceivePacks()
		code, body := e.ProposeHTTP(a, e.Proposal(a, commit, garbage))
		if code != 400 || errCode(t, body) != domain.CodeInvalidRequest {
			t.Fatalf("%d %s", code, body)
		}
		if len(body) > 1024 || strings.Contains(string(body), "not a git bundle") {
			t.Fatalf("the rejection echoes bundle content or is unbounded: %d bytes", len(body))
		}
		if e.ReceivePacks() != pushes || e.Publications(p.Workspace) != 0 || e.Repo.Tip(t, e.Branch(a)) != *ptrString(a.Context.Workspace.Source.ExpectedTip) {
			t.Fatal("a malformed bundle changed the remote or recorded a publication")
		}
	})

	t.Run("an injected credential in new source is refused and no credential reaches the workload", func(t *testing.T) {
		p := e.NewProject(ScopeA)
		a := e.Writer(p, labelJob)
		e.Secrets.Set(a.ID(), "injected-deploy-key-0001")
		commit, art := e.Bundle(a, "leak", gitfx.File{Path: "cfg", Content: "key=injected-deploy-key-0001"})
		code, body := e.ProposeHTTP(a, e.Proposal(a, commit, art))
		if code != 400 || errCode(t, body) != domain.CodeInvalidRequest {
			t.Fatalf("%d %s", code, body)
		}
		if strings.Contains(string(body), "injected-deploy-key") {
			t.Fatal("the response echoes the secret")
		}
		if e.Publications(p.Workspace) != 0 || e.Repo.Tip(t, e.Branch(a)) != *ptrString(a.Context.Workspace.Source.ExpectedTip) {
			t.Fatal("source containing a secret was published or recorded")
		}
		// The workload's context and the runtime responses carry no Git credential.
		ctxJSON, _ := json.Marshal(a.Context)
		for _, tok := range []string{e.Git.WriteToken, e.Git.ReadToken} {
			if bytes.Contains(ctxJSON, []byte(tok)) || strings.Contains(string(body), tok) {
				t.Fatal("a Git credential reached the workload")
			}
		}
		if strings.Contains(a.Context.Workspace.Source.RepositoryURL, "@") {
			t.Fatalf("the repository URL carries credentials: %s", a.Context.Workspace.Source.RepositoryURL)
		}
		// A clean proposal then publishes.
		clean, cleanArt := e.Bundle(a, "clean", gitfx.File{Path: "a.txt", Content: "a"})
		code, body = e.ProposeHTTP(a, e.Proposal(a, clean, cleanArt))
		if code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		log := e.Repo.Git(t, "log", "-p", "--all")
		for _, tok := range []string{e.Git.WriteToken, e.Git.ReadToken} {
			if strings.Contains(log, tok) {
				t.Fatal("a Git credential is inside the published history")
			}
		}
	})

	t.Run("a failed writer never yields a successful descendant source", func(t *testing.T) {
		p := e.NewProject(ScopeA)
		a := e.Writer(p, labelJob)
		commit, art := e.Bundle(a, "change", gitfx.File{Path: "a.txt", Content: "a"})
		if code, body := e.ProposeHTTP(a, e.Proposal(a, commit, art)); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		failed := Request(t, runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(1)), Reason: ptr("command_failed"), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}})
		if ack, err := e.Submit(a, failed); err != nil || ack.AttemptStatus != runtimev1.AttemptFailed {
			t.Fatalf("%+v %v", ack, err)
		}
		// Published history is retained, but it is not a successful output.
		if _, err := e.WSPlain.SelectWriterOutput(ctx, wst.Engineer, p.Scope, p.Workspace, a.ID()); err == nil {
			t.Fatal("the failed writer's commit became a selectable successful output")
		}
		if got := e.Aggregate(a).Step("build").Outputs; len(got) != 0 {
			t.Fatalf("outputs of a failed writer: %v", got)
		}
		// A later success claim for the same attempt conflicts with the recorded failure.
		if code, _ := e.SubmitHTTP(a, completedWith(commit, outputsOf("v1"))); code != 409 {
			t.Fatalf("success after failure: %d", code)
		}
	})
}

func ptrString(c *runtimev1.GitCommit) *string {
	s := string(*c)
	return &s
}
