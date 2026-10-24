package artifactshttp_test

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	artifactshttp "github.com/optimiware/kodama/internal/artifacts/http"
	"github.com/optimiware/kodama/internal/domain"
)

var requestIDPattern = regexp.MustCompile(`req_[0-9a-f]{32}`)

func newEnv(t *testing.T) (*artifactstest.Env, *stack) {
	t.Helper()
	env := artifactstest.New(t, artifactstest.Options{})
	return env, newStack(t, env, nil)
}

// putStaging writes directly to the provider with storage credentials, bypassing
// any instruction, optionally with misleading object metadata.
func putStaging(t *testing.T, env *artifactstest.Env, id domain.ArtifactID, data []byte, meta map[string]string) {
	t.Helper()
	a, err := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.S3.Client.PutObject(context.Background(), &awss3.PutObjectInput{
		Bucket: aws.String(env.S3.Bucket()), Key: aws.String(env.Objects.Prefix() + a.StagingKey()), Body: bytes.NewReader(data), Metadata: meta,
		ContentType: aws.String("text/plain"),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func resultCtx(attempt domain.AttemptID, outputs ...runtimev1.OutputDecl) runtimev1.Context {
	return runtimev1.Context{TenantID: artifactstest.TenantA, ProjectID: artifactstest.ProjectA, AttemptID: attempt,
		Workspace: runtimev1.Workspace{Access: runtimev1.AccessNone}, Outputs: outputs}
}

// leakMarkers must never appear in a response about a foreign or unusable
// artifact: instructions, endpoints and storage keys.
func leakMarkers(env *artifactstest.Env) []string {
	return []string{"X-Amz", "Signature", env.S3.Config.Endpoint, env.S3.Bucket(), "/staging/", "/sealed/", env.S3.Prefix, "uploadId", `"url"`}
}

func assertNoLeak(t *testing.T, env *artifactstest.Env, what string, raw []byte) {
	t.Helper()
	for _, m := range leakMarkers(env) {
		if strings.Contains(string(raw), m) {
			t.Errorf("%s discloses %q: %s", what, m, raw)
		}
	}
}

// Test_foreign_object: copied cross-tenant, cross-project and cross-attempt IDs
// return the nondisclosing not-found for completion, metadata, download and
// trusted lookup, and no storage instruction leaks. Real PostgreSQL and S3.
// (IT-03, IT-11, DQ-01; IT-A04, DQ-A01.)
func Test_foreign_object(t *testing.T) {
	env, s := newEnv(t)
	dataA, dataB := []byte("tenant A evidence"), []byte("tenant B evidence")

	cA := env.Attempt(artifactstest.ScopeA, "att_a", artifacts.OutputDecl{Name: "report", Kind: artifacts.KindFile})
	cA2 := env.Attempt(artifactstest.ScopeA, "att_a2", artifacts.OutputDecl{Name: "report", Kind: artifacts.KindFile})
	cB := env.Attempt(artifactstest.ScopeB, "att_b", artifacts.OutputDecl{Name: "report", Kind: artifacts.KindFile})
	env.Reader.Allow("user:alice", artifactstest.ScopeA)
	env.Reader.Allow("user:alice-billing", artifactstest.ScopeA2)
	env.Reader.Allow("user:bob", artifactstest.ScopeB)

	// A usable artifact and a reserved one per side.
	usableA, upA := s.reserveOK(t, "att_a", cA.Subject, "report", artifacts.RoleOutput, dataA)
	s.upload(t, upA, dataA)
	if r := s.complete(t, "att_a", usableA, cA.Subject, completeBody(dataA)); r.status != http.StatusOK {
		t.Fatalf("complete: %d %s", r.status, r.raw)
	}
	reservedA, _ := s.reserveOK(t, "att_a", cA.Subject, "log", artifacts.RoleDiagnostic, []byte("pending log"))
	usableB, upB := s.reserveOK(t, "att_b", cB.Subject, "report", artifacts.RoleOutput, dataB)
	s.upload(t, upB, dataB)
	if r := s.complete(t, "att_b", usableB, cB.Subject, completeBody(dataB)); r.status != http.StatusOK {
		t.Fatalf("complete B: %d %s", r.status, r.raw)
	}
	// An ID that never existed, for comparison.
	const missing = domain.ArtifactID("art_00000000000000000000000000000000")

	notFound := func(t *testing.T, what string, r response) {
		t.Helper()
		if r.status != http.StatusNotFound || r.errorCode(t) != "not_found" {
			t.Errorf("%s: %d %s", what, r.status, r.raw)
			return
		}
		assertNoLeak(t, env, what, r.raw)
	}
	normalize := func(raw []byte) string { return requestIDPattern.ReplaceAllString(string(raw), "req_X") }

	t.Run("completion", func(t *testing.T) {
		// Tenant B's attempt, holding a valid credential, completes tenant A's artifact.
		bodyB := s.complete(t, "att_b", usableA, cB.Subject, completeBody(dataA))
		notFound(t, "B completes A's usable artifact", bodyB)
		notFound(t, "B completes A's reserved artifact", s.complete(t, "att_b", reservedA, cB.Subject, completeBody([]byte("pending log"))))
		// Another attempt of the same project.
		notFound(t, "attempt 2 completes attempt 1's artifact", s.complete(t, "att_a2", usableA, cA2.Subject, completeBody(dataA)))
		// A credential addressing another attempt's path.
		notFound(t, "B's token on A's attempt path", s.complete(t, "att_a", usableA, cB.Subject, completeBody(dataA)))
		// Indistinguishable from a missing ID.
		miss := s.complete(t, "att_b", missing, cB.Subject, completeBody(dataA))
		notFound(t, "missing id", miss)
		if normalize(bodyB.raw) != normalize(miss.raw) {
			t.Errorf("a foreign id and a missing id are distinguishable:\n%s\n%s", bodyB.raw, miss.raw)
		}
		// Nothing about A's artifact changed.
		for _, id := range []domain.ArtifactID{usableA, reservedA} {
			a, err := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
			if err != nil || a.Version() > 2 {
				t.Errorf("artifact %s was touched: %v", id, err)
			}
		}
		// Reserving or replaying on a foreign attempt path discloses nothing either.
		notFound(t, "B reserves on A's attempt", s.reserve(t, "att_a", cB.Subject, reserveBody("report", artifacts.RoleOutput, dataA)))
		if r := s.reserve(t, "att_a", "", reserveBody("report", artifacts.RoleOutput, dataA)); r.status != http.StatusUnauthorized {
			t.Errorf("an unauthenticated reserve: %d", r.status)
		}
		if r := s.reserve(t, "att_a", "user:alice", reserveBody("report", artifacts.RoleOutput, dataA)); r.status != http.StatusUnauthorized {
			t.Errorf("a person's token is not an attempt credential: %d %s", r.status, r.raw)
		}
	})

	t.Run("metadata and download", func(t *testing.T) {
		type tc struct {
			what    string
			scope   domain.Scope
			id      domain.ArtifactID
			subject string
		}
		for _, c := range []tc{
			{"B reads A's artifact in A's project", artifactstest.ScopeA, usableA, "user:bob"},
			{"B reads A's artifact in B's project", artifactstest.ScopeB, usableA, "user:bob"},
			{"A reads B's artifact in A's project", artifactstest.ScopeA, usableB, "user:alice"},
			{"A reads B's artifact in B's project", artifactstest.ScopeB, usableB, "user:alice"},
			{"another project of the same tenant", artifactstest.ScopeA2, usableA, "user:alice-billing"},
			{"missing id", artifactstest.ScopeA, missing, "user:alice"},
			{"mixed tenant and project", domain.Scope{TenantID: artifactstest.TenantA, ProjectID: artifactstest.ProjectB}, usableA, "user:alice"},
		} {
			for _, q := range []string{"", "download=true"} {
				notFound(t, c.what+" ?"+q, s.get(t, c.scope, c.id, c.subject, q))
			}
		}
		// Identical bodies for foreign and missing references.
		a := s.get(t, artifactstest.ScopeA, usableB, "user:alice", "")
		b := s.get(t, artifactstest.ScopeA, missing, "user:alice", "")
		if normalize(a.raw) != normalize(b.raw) {
			t.Errorf("foreign and missing metadata lookups differ:\n%s\n%s", a.raw, b.raw)
		}
		// Control: the owner sees the artifact.
		if r := s.get(t, artifactstest.ScopeA, usableA, "user:alice", ""); r.status != http.StatusOK {
			t.Fatalf("owner: %d %s", r.status, r.raw)
		}
		// Revoking the reader's membership denies at once, with the same not-found.
		env.Reader.Revoke("user:alice", artifactstest.ScopeA)
		notFound(t, "revoked member", s.get(t, artifactstest.ScopeA, usableA, "user:alice", "download=true"))
		env.Reader.Allow("user:alice", artifactstest.ScopeA)
	})

	t.Run("trusted lookup", func(t *testing.T) {
		refs, err := env.Service.References(env.Ctx(), artifactstest.ScopeA, "att_a", []domain.ArtifactID{usableA, reservedA, usableB, missing})
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != 2 || !refs[usableA].Usable || refs[reservedA].Usable {
			t.Fatalf("scoped references: %+v", refs)
		}
		if _, ok := refs[usableB]; ok {
			t.Fatal("a foreign artifact was returned")
		}
		if _, err := env.Service.Reference(env.Ctx(), artifactstest.ScopeA, usableB); err == nil {
			t.Fatal("a foreign artifact resolved in scope")
		}
		// A result naming a foreign artifact is rejected by the contract check
		// exactly like a missing one.
		ctx := resultCtx("att_a", runtimev1.OutputDecl{Name: "report", Type: runtimev1.OutputArtifact, Path: strptr("/kodama/outputs/report")})
		res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: int64ptr(0), Outputs: map[string]runtimev1.Value{"report": runtimev1.StringValue(string(usableB))}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		err = runtimev1.CheckResult(res, ctx, artifactshttp.RuntimeRefs(refs))
		if ce, ok := err.(*runtimev1.ContractError); !ok || ce.Code != domain.CodeNotFound {
			t.Fatalf("foreign output: %v", err)
		}
		res.Outputs["report"] = runtimev1.StringValue(string(missing))
		err = runtimev1.CheckResult(res, ctx, artifactshttp.RuntimeRefs(refs))
		if ce, ok := err.(*runtimev1.ContractError); !ok || ce.Code != domain.CodeNotFound {
			t.Fatalf("missing output: %v", err)
		}
		res.Outputs["report"] = runtimev1.StringValue(string(usableA))
		if err := runtimev1.CheckResult(res, ctx, artifactshttp.RuntimeRefs(refs)); err != nil {
			t.Fatalf("own usable output: %v", err)
		}
	})

	t.Run("no response about another tenant carries a storage instruction", func(t *testing.T) {
		r := s.get(t, artifactstest.ScopeB, usableA, "user:bob", "download=true")
		assertNoLeak(t, env, "download attempt", r.raw)
		if r.header.Get("Cache-Control") != "no-store" {
			t.Errorf("Cache-Control: %q", r.header.Get("Cache-Control"))
		}
	})
}

func strptr(s string) *string { return &s }
func int64ptr(n int64) *int64 { return &n }

// Test_digest_mismatch: completion-request and actual-content digest
// mismatches return 422 and never establish usability, including with
// misleading object metadata. Real PostgreSQL and S3. (PX-10, DQ-01.)
func Test_digest_mismatch(t *testing.T) {
	env, s := newEnv(t)
	good, other := []byte("the declared bytes"), []byte("THE DECLARED BYTES")
	c := env.Attempt(artifactstest.ScopeA, "att_a", artifacts.OutputDecl{Name: "report", Kind: artifacts.KindFile})
	env.Reader.Allow("user:alice", artifactstest.ScopeA)

	id, up := s.reserveOK(t, "att_a", c.Subject, "report", artifacts.RoleOutput, good)
	expect422 := func(t *testing.T, what string, r response) {
		t.Helper()
		if r.status != http.StatusUnprocessableEntity || r.errorCode(t) != "digest_mismatch" {
			t.Fatalf("%s: %d %s", what, r.status, r.raw)
		}
		assertNoLeak(t, env, what, r.raw)
		a, err := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		if err != nil || a.Usable() || a.State() != artifacts.StateReserved || a.SealedKey() != "" {
			t.Fatalf("%s: a mismatch established usability: %v %v", what, a, err)
		}
		if env.Count(`SELECT count(*) FROM artifact_objects WHERE artifact_id = $1 AND winner`, string(id)) != 0 {
			t.Fatalf("%s: a winner exists", what)
		}
	}

	t.Run("the completion request names different content than the reservation", func(t *testing.T) {
		s.upload(t, up, good)
		expect422(t, "different digest", s.complete(t, "att_a", id, c.Subject, completeBody(other)))
		expect422(t, "different size", s.complete(t, "att_a", id, c.Subject, runtimev1.ArtifactCompleteRequest{Digest: runtimev1.Digest(artifacts.DigestOf(good)), SizeBytes: int64(len(good)) + 1}))
	})

	t.Run("actual bytes differ from the declaration even when metadata claims otherwise", func(t *testing.T) {
		putStaging(t, env, id, other, map[string]string{
			"sha256": string(artifacts.DigestOf(good)), "digest": string(artifacts.DigestOf(good)), "x-amz-checksum-sha256": artifacts.DigestOf(good).Base64(),
		})
		expect422(t, "other bytes with a claimed digest", s.complete(t, "att_a", id, c.Subject, completeBody(good)))
		// The caller also asserting the actual bytes' digest does not help: the
		// declaration was frozen at reservation.
		expect422(t, "request digest equals the actual bytes", s.complete(t, "att_a", id, c.Subject, completeBody(other)))
		if env.ObjectExists(sealedOf(t, env, id)) {
			t.Fatal("bytes were sealed for a mismatch")
		}
		// The unverified artifact offers no download and no usable reference.
		if r := s.get(t, artifactstest.ScopeA, id, "user:alice", "download=true"); r.status != http.StatusConflict || r.errorCode(t) != "artifact_incomplete" {
			t.Fatalf("download of unverified bytes: %d %s", r.status, r.raw)
		}
		refs, _ := env.Service.References(env.Ctx(), artifactstest.ScopeA, "att_a", []domain.ArtifactID{id})
		ctx := resultCtx("att_a", runtimev1.OutputDecl{Name: "report", Type: runtimev1.OutputArtifact, Path: strptr("/kodama/outputs/report")})
		res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: int64ptr(0), Outputs: map[string]runtimev1.Value{"report": runtimev1.StringValue(string(id))}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		err := runtimev1.CheckResult(res, ctx, artifactshttp.RuntimeRefs(refs))
		if ce, ok := err.(*runtimev1.ContractError); !ok || ce.Code != runtimev1.CodeArtifactIncomplete {
			t.Fatalf("a result referencing a mismatched artifact: %v", err)
		}
	})

	t.Run("an ETag or checksum is never the proof", func(t *testing.T) {
		// A byte-different object that nevertheless carries an ETag-shaped
		// metadata value still fails; only the bytes count.
		putStaging(t, env, id, bytes.Repeat([]byte("x"), len(good)), map[string]string{"etag": "d41d8cd98f00b204e9800998ecf8427e"})
		expect422(t, "same size, different bytes", s.complete(t, "att_a", id, c.Subject, completeBody(good)))
	})

	t.Run("the corrected upload completes", func(t *testing.T) {
		putStaging(t, env, id, good, nil)
		r := s.complete(t, "att_a", id, c.Subject, completeBody(good))
		if r.status != http.StatusOK {
			t.Fatalf("%d %s", r.status, r.raw)
		}
		var done runtimev1.ArtifactCompleteResponse
		r.json(t, &done)
		if done.State != runtimev1.StateUsable || done.Digest != runtimev1.Digest(artifacts.DigestOf(good)) || done.SizeBytes != int64(len(good)) || done.ArtifactID != id {
			t.Fatalf("completion: %+v", done)
		}
		if r := s.get(t, artifactstest.ScopeA, id, "user:alice", "download=true"); r.status != http.StatusOK {
			t.Fatalf("download after completion: %d %s", r.status, r.raw)
		}
	})
}

func sealedOf(t *testing.T, env *artifactstest.Env, id domain.ArtifactID) string {
	t.Helper()
	a, err := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
	if err != nil {
		t.Fatal(err)
	}
	if a.SealedKey() == "" {
		return "artifacts/none/none/none/sealed/none"
	}
	return a.SealedKey()
}

// Test_incomplete_upload: absent, truncated and wrong-size objects remain
// unusable; CheckResult rejects them while separately verified diagnostics stay
// readable. Real PostgreSQL and S3. (PX-10, PX-A07.)
func Test_incomplete_upload(t *testing.T) {
	env, s := newEnv(t)
	data := []byte("complete content of the report")
	c := env.Attempt(artifactstest.ScopeA, "att_a", artifacts.OutputDecl{Name: "report", Kind: artifacts.KindFile}, artifacts.OutputDecl{Name: "other", Kind: artifacts.KindFile})
	env.Reader.Allow("user:alice", artifactstest.ScopeA)

	id, up := s.reserveOK(t, "att_a", c.Subject, "report", artifacts.RoleOutput, data)
	expectIncomplete := func(t *testing.T, what string) {
		t.Helper()
		r := s.complete(t, "att_a", id, c.Subject, completeBody(data))
		if r.status != http.StatusConflict || r.errorCode(t) != "artifact_incomplete" {
			t.Fatalf("%s: %d %s", what, r.status, r.raw)
		}
		assertNoLeak(t, env, what, r.raw)
		a, _ := env.Store.Get(env.Ctx(), artifactstest.ScopeA, id)
		if a.Usable() || a.State() != artifacts.StateReserved {
			t.Fatalf("%s: became usable", what)
		}
	}

	t.Run("absent", func(t *testing.T) { expectIncomplete(t, "no object") })
	t.Run("truncated", func(t *testing.T) {
		putStaging(t, env, id, data[:len(data)/2], nil)
		expectIncomplete(t, "half the bytes")
		putStaging(t, env, id, nil, nil)
		expectIncomplete(t, "an empty object")
	})
	t.Run("wrong size", func(t *testing.T) {
		putStaging(t, env, id, append(append([]byte{}, data...), "trailing"...), nil)
		expectIncomplete(t, "extra bytes")
	})

	t.Run("an incomplete or unverified output fails success finalization while diagnostics stay readable", func(t *testing.T) {
		logBytes := []byte("exit 2: the build failed\n")
		diag, dup := s.reserveOK(t, "att_a", c.Subject, "log", artifacts.RoleDiagnostic, logBytes)
		s.upload(t, dup, logBytes)
		if r := s.complete(t, "att_a", diag, c.Subject, completeBody(logBytes)); r.status != http.StatusOK {
			t.Fatalf("diagnostic: %d %s", r.status, r.raw)
		}
		refs, err := env.Service.References(env.Ctx(), artifactstest.ScopeA, "att_a", []domain.ArtifactID{id, diag})
		if err != nil {
			t.Fatal(err)
		}
		contract := artifactshttp.RuntimeRefs(refs)
		ctx := resultCtx("att_a", runtimev1.OutputDecl{Name: "report", Type: runtimev1.OutputArtifact, Path: strptr("/kodama/outputs/report")})

		completed := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: int64ptr(0), Outputs: map[string]runtimev1.Value{"report": runtimev1.StringValue(string(id))}, DiagnosticArtifactIDs: []domain.ArtifactID{diag}}
		err = runtimev1.CheckResult(completed, ctx, contract)
		if ce, ok := err.(*runtimev1.ContractError); !ok || ce.Code != runtimev1.CodeArtifactIncomplete {
			t.Fatalf("success with an incomplete output must fail: %v", err)
		}
		failed := runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: int64ptr(2), Reason: strptr("exit_nonzero"), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{diag}}
		if err := runtimev1.CheckResult(failed, ctx, contract); err != nil {
			t.Fatalf("a failed result keeps its verified diagnostic: %v", err)
		}
		// A diagnostic whose upload was not completed is not accepted as evidence.
		pending, _ := s.reserveOK(t, "att_a", c.Subject, "log2", artifacts.RoleDiagnostic, []byte("not uploaded"))
		refs, _ = env.Service.References(env.Ctx(), artifactstest.ScopeA, "att_a", []domain.ArtifactID{pending})
		failed.DiagnosticArtifactIDs = []domain.ArtifactID{pending}
		err = runtimev1.CheckResult(failed, ctx, artifactshttp.RuntimeRefs(refs))
		if ce, ok := err.(*runtimev1.ContractError); !ok || ce.Code != runtimev1.CodeArtifactIncomplete {
			t.Fatalf("an unfinished diagnostic: %v", err)
		}

		// The verified diagnostic is readable and downloadable.
		r := s.get(t, artifactstest.ScopeA, diag, "user:alice", "download=true")
		if r.status != http.StatusOK {
			t.Fatalf("diagnostic read: %d %s", r.status, r.raw)
		}
		var dto artifactshttp.Artifact
		r.json(t, &dto)
		if dto.State != artifactshttp.Usable || dto.Download == nil {
			t.Fatalf("diagnostic: %+v", dto)
		}
		if code, body := env.Download(artifacts.Transfer{Method: string(dto.Download.Method), URL: dto.Download.Url}); code != 200 || !bytes.Equal(body, logBytes) {
			t.Fatalf("diagnostic download: %d", code)
		}
		// The incomplete output has metadata but never a download.
		r = s.get(t, artifactstest.ScopeA, id, "user:alice", "")
		var inc artifactshttp.Artifact
		r.json(t, &inc)
		if r.status != http.StatusOK || inc.State != artifactshttp.Reserved || inc.Download != nil {
			t.Fatalf("incomplete output metadata: %d %+v", r.status, inc)
		}
	})

	t.Run("a late correct upload completes the same reservation", func(t *testing.T) {
		s.upload(t, up, data)
		if r := s.complete(t, "att_a", id, c.Subject, completeBody(data)); r.status != http.StatusOK {
			t.Fatalf("%d %s", r.status, r.raw)
		}
	})
}
