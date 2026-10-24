package results_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/results"
	"github.com/optimiware/kodama/internal/results/adapters"
)

const reportJob = "outputs: {report: {type: artifact, path: /kodama/outputs/report.json}}"

func reportOutput(id domain.ArtifactID) map[string]runtimev1.Value {
	return map[string]runtimev1.Value{"report": runtimev1.StringValue(string(id))}
}

// Test_foreign_artifact: PX-10, IT-06, IT-A04 (acceptance `foreign_artifact`).
// Output and diagnostic IDs copied from another attempt, project or tenant are
// the same 404 as missing IDs; an incomplete upload cannot finalize; nothing of
// the other tenant's content or provenance is disclosed; no rejected request
// changes durable state. Real PostgreSQL and S3 through the real adapters; no
// NATS or Git.
func Test_foreign_artifact(t *testing.T) {
	e := New(t, Options{Artifacts: true})
	mine := e.Run(e.NewProject(ScopeA), Job("build", readRun, reportJob))
	sibling := e.Run(e.NewProject(ScopeA), Job("build", readRun, reportJob)) // same tenant and project, another attempt
	foreign := e.Run(e.NewProject(ScopeB), Job("build", readRun, reportJob)) // another tenant
	secret := []byte("tenant-b confidential report, sha-bearing\n")

	siblingReport := e.Upload(sibling, "report", artifacts.RoleOutput, []byte("sibling report\n"))
	siblingLog := e.Upload(sibling, "sibling.log", artifacts.RoleDiagnostic, []byte("sibling log\n"))
	foreignReport := e.Upload(foreign, "report", artifacts.RoleOutput, secret)
	foreignLog := e.Upload(foreign, "foreign.log", artifacts.RoleDiagnostic, secret)

	denied := func(name string, res runtimev1.Result) (int, string) {
		t.Helper()
		code, body := e.SubmitHTTP(mine, Request(t, res))
		if code != 404 || errCode(t, body) != domain.CodeNotFound {
			t.Fatalf("%s: %d %s", name, code, body)
		}
		return code, normalized(t, body)
	}
	_, missing := denied("missing output", Completed(reportOutput("art_does_not_exist")))
	for name, res := range map[string]runtimev1.Result{
		"another attempt's output":     Completed(reportOutput(siblingReport)),
		"another tenant's output":      Completed(reportOutput(foreignReport)),
		"another attempt's diagnostic": Completed(reportOutput("art_does_not_exist"), siblingLog),
		"another tenant's diagnostic":  Completed(reportOutput("art_does_not_exist"), foreignLog),
	} {
		if _, got := denied(name, res); got != missing {
			t.Fatalf("%s is distinguishable from a missing ID:\n%s\nvs\n%s", name, got, missing)
		}
	}
	// Failed results get the same answer for a foreign diagnostic.
	failed := runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(1)), Reason: ptr("command_failed"), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{foreignLog}}
	if code, body := e.SubmitHTTP(mine, Request(t, failed)); code != 404 || normalized(t, body) != missing {
		t.Fatalf("failed result with a foreign diagnostic: %d %s", code, body)
	}
	// A completed output with a foreign ID and a missing sibling output is still a
	// 404, never a durable failure that records an unverified reference.
	if e.Persisted(mine).ResultHash != "" || e.Persisted(mine).Status != execution.AttemptRunning || e.Count(`SELECT count(*) FROM artifact_pins WHERE owner_id = $1`, string(mine.ID())) != 0 {
		t.Fatal("a rejected request changed durable state or pinned artifacts")
	}
	if e.Commits.Count() != 0 {
		t.Fatalf("%d rejected requests reached execution", e.Commits.Count())
	}

	// Nothing about the other tenant's content leaks through any response or log.
	for _, leak := range []string{string(foreignReport), string(foreignLog), "confidential", e.ArtStoreDigest(secret)} {
		if strings.Contains(missing, leak) {
			t.Fatalf("response discloses %q", leak)
		}
	}

	// An incomplete upload (reserved, bytes sent, never completed) cannot finalize.
	data := []byte("not completed yet\n")
	pending := e.Reserve(mine, "report", artifacts.RoleOutput, data)
	code, body := e.SubmitHTTP(mine, Request(t, Completed(reportOutput(pending))))
	if code != 409 || errCode(t, body) != runtimev1.CodeArtifactIncomplete {
		t.Fatalf("incomplete upload: %d %s", code, body)
	}
	if e.Persisted(mine).ResultHash != "" {
		t.Fatal("an incomplete upload finalized")
	}
	// A completion with the wrong digest cannot make it usable either.
	if _, err := e.ArtSvc.Complete(context.Background(), mine.Principal(), mine.ID(), pending, artifacts.CompleteInput{Digest: artifacts.DigestOf([]byte("other")), Size: int64(len(data))}, "req_t022"); !errors.Is(err, artifacts.ErrDigestMismatch) {
		t.Fatalf("wrong digest: %v", err)
	}
	if code, _ := e.SubmitHTTP(mine, Request(t, Completed(reportOutput(pending)))); code != 409 {
		t.Fatalf("after a failed completion: %d", code)
	}

	// Completing the upload makes the same reference acceptable.
	e.PutAndComplete(mine, pending, data)
	ownLog := e.Upload(mine, "build.log", artifacts.RoleDiagnostic, []byte("ok\n"))
	req := Request(t, Completed(reportOutput(pending), ownLog))
	ack, err := e.Submit(mine, req)
	if err != nil || ack.AttemptStatus != runtimev1.AttemptSucceeded {
		t.Fatalf("%+v %v", ack, err)
	}
	for _, id := range []domain.ArtifactID{pending, ownLog} {
		if n := e.Count(`SELECT count(*) FROM artifact_pins WHERE artifact_id = $1 AND owner_kind = 'attempt_finalization' AND owner_id = $2`, string(id), string(mine.ID())); n != 1 {
			t.Fatalf("artifact %s: %d pins", id, n)
		}
	}
	if got := e.Aggregate(mine).Step("build").Outputs["report"].String(); got != string(pending) {
		t.Fatalf("accepted output %q", got)
	}
	// The sibling's and the other tenant's artifacts were never pinned by this attempt.
	if n := e.Count(`SELECT count(*) FROM artifact_pins WHERE artifact_id IN ($1, $2)`, string(siblingReport), string(foreignReport)); n != 0 {
		t.Fatalf("%d pins on other attempts' artifacts", n)
	}
	// The foreign tenant's attempt can still use its own artifacts.
	if _, err := e.Submit(foreign, Request(t, Completed(reportOutput(foreignReport), foreignLog))); err != nil {
		t.Fatalf("the owner of the artifacts: %v", err)
	}
}

// normalized is an error body without its per-request identifier.
func normalized(t testing.TB, body []byte) string {
	t.Helper()
	var r runtimev1.ErrorResponse
	if err := jsonUnmarshal(body, &r); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	r.Error.RequestID = ""
	return string(mustJSON(t, r))
}

// TestArtifactAuthorityBridge: the artifact module's AttemptAuthority is derived
// from the verified grant and the attempt's pinned context, never from the
// request: producer identity, declared outputs, limits and provenance (the
// attempt's input source). A cancelled or revoked attempt cannot reserve.
func TestArtifactAuthorityBridge(t *testing.T) {
	e := New(t, Options{Artifacts: true})
	a := e.Run(e.NewProject(ScopeA), Job("build", readRun, reportJob))
	id := e.Upload(a, "report", artifacts.RoleOutput, []byte("report\n"))

	refs, err := e.ArtSvc.References(context.Background(), a.Scope, a.ID(), []domain.ArtifactID{id})
	if err != nil || !refs[id].Usable {
		t.Fatalf("%+v %v", refs, err)
	}
	src := a.Context.Workspace.Source
	if src == nil || refs[id].Provenance.RepositoryURL != src.RepositoryURL || refs[id].Provenance.Commit != string(src.Commit) {
		t.Fatalf("provenance %+v is not the attempt's pinned source %+v", refs[id].Provenance, src)
	}
	if refs[id].Attempt != a.ID() || refs[id].Scope != a.Scope {
		t.Fatalf("producer %+v", refs[id])
	}

	// An output the context does not declare is refused by the artifact module.
	mt, size := "text/plain", int64(1)
	if _, err := e.ArtSvc.Reserve(context.Background(), a.Principal(), a.ID(), artifacts.ReserveInput{Name: "extra", Kind: artifacts.KindFile, Role: artifacts.RoleOutput, Digest: artifacts.DigestOf([]byte("x")), MediaType: &mt, Size: &size}, "req"); !errors.Is(err, artifacts.ErrOutputInvalid) {
		t.Fatalf("an undeclared output: %v", err)
	}
	// A principal that is not a runtime session of this attempt has no authority.
	for name, caller := range map[string]artifacts.Caller{
		"human":         {Subject: "u020_engineer"},
		"other session": {Subject: adapters.RuntimePrincipal(a.Scope.TenantID, "ses_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		"other tenant":  {Subject: adapters.RuntimePrincipal(ScopeB.TenantID, a.Session)},
	} {
		if _, err := e.ArtSvc.Reserve(context.Background(), caller, a.ID(), artifacts.ReserveInput{Name: "report", Kind: artifacts.KindFile, Role: artifacts.RoleOutput, Digest: artifacts.DigestOf([]byte("x")), MediaType: &mt, Size: &size}, "req"); err == nil {
			t.Fatalf("%s reserved an artifact", name)
		}
	}
	// Cancellation ends the authority to reserve and complete.
	if _, err := e.ExecPlain.Cancel(context.Background(), execution.Caller{PrincipalID: "u020_engineer", RequestID: "req"}, a.Scope, a.Run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ArtSvc.Reserve(context.Background(), a.Principal(), a.ID(), artifacts.ReserveInput{Name: "report", Kind: artifacts.KindFile, Role: artifacts.RoleOutput, Digest: artifacts.DigestOf([]byte("report\n")), MediaType: &mt, Size: ptr(int64(7))}, "req"); !errors.Is(err, artifacts.ErrAttemptInactive) {
		t.Fatalf("a cancelled attempt reserved: %v", err)
	}
	_ = results.ErrNotFound
}
