package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	"github.com/optimiware/kodama/internal/artifacts/oci"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryadapters "github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	gatesadapters "github.com/optimiware/kodama/internal/gates/adapters"
	"github.com/optimiware/kodama/internal/results"
	resultsadapters "github.com/optimiware/kodama/internal/results/adapters"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/execbridge"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

// v2Fx is a run_shared attempt produced by execution itself and served by the
// production collaborators of the runtime routes: the results service with its
// execution adapters, the artifact service with the runtime-grant authority, the
// secret route and the runtime authority's v2 admission, over the memory
// persistence of each module. Nothing of the attempt is hand-built.
type v2Fx struct {
	t      *testing.T
	m      *rtest.MemStack
	s      executiontest.SharedSubject
	run    *executiontest.SharedRun
	spec   *execution.AttemptSpec
	ctx2   runtimev2.ContextV2
	art    *artifactstest.MemEnv
	artSvc *artifacts.Service
	v1, v2 http.Handler
	token  string
	secret string
}

type noSnapshots struct{}

func (noSnapshots) GetSnapshot(context.Context, domain.Scope, domain.EnvironmentID, runtimev1.Digest) (delivery.ConfigurationSnapshot, error) {
	return delivery.ConfigurationSnapshot{}, delivery.ErrNotFound
}

type noSource struct{}

func (noSource) Propose(context.Context, domain.Scope, domain.AttemptID, runtimev1.SourceProposalRequest) (results.SourceOutcome, error) {
	return results.SourceOutcome{}, results.Fail(results.ErrUnavailable, nil, "no source in this fixture")
}

func newV2Fx(t *testing.T) *v2Fx {
	t.Helper()
	ctx := context.Background()
	f := &v2Fx{t: t, secret: "s3cr3t-value"}
	mem, subject := executiontest.NewSharedMem(t)
	f.s = subject
	mem.World.CreateSecret(executiontest.SharedScope, "token")
	f.run = executiontest.NewSharedRun(t, f.s, executiontest.SharedSpecDoc(false, executiontest.SharedStep("build",
		`env: {TOKEN: {secretRef: "secret://project/token"}}`, `outputs: {report: {type: artifact, path: /kodama/outputs/report.txt}}`)))
	f.run.Start()
	f.run.Up()
	spec, err := f.run.Claim()
	if err != nil || spec == nil {
		t.Fatalf("claim: %v", err)
	}
	f.spec = spec
	f.s.Drive.Start(spec)
	_, c2, err := resultsadapters.DecodePinned(spec.Context)
	if err != nil || c2 == nil {
		t.Fatalf("the attempt execution produced is not a v2 context: %v", err)
	}
	f.ctx2 = *c2
	if len(c2.Secrets) != 1 || len(c2.Outputs) != 1 {
		t.Fatalf("the context declares %d secrets and %d outputs", len(c2.Secrets), len(c2.Outputs))
	}

	// The runtime authority: execution's committed attempt is seeded as the
	// authority's attempt (the PostgreSQL composition reads it from execution).
	f.m = rtest.NewMemStack(t)
	attempt := f.m.SeedAttemptV2(f.ctx2, rtest.AllActions())
	cred, err := runtimeauth.GenerateCredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.RegisterV2With(attempt, f.ctx2, cred, rtest.AllActions()); err != nil {
		t.Fatalf("register: %v", err)
	}
	id := f.m.Workload(attempt)
	admit := f.m.AdmitRequest(attempt, cred, "nnnnnnnnnnnnnnnnnnnnnn")
	admit.Job = id

	// The production collaborators.
	f.art = artifactstest.NewMemEnv(t)
	// The object store's clock is the runtime authority's: the grant's lifetime bounds the transfer.
	objects := artifactstest.NewMemObjects(f.m.Clock.Now)
	if _, err := objects.Serve(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = objects.Close() })
	artAuthority, err := resultsadapters.NewArtifactAuthority(f.m.Env.Grants, f.m.Ring, f.m.Clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	begins, err := resultsadapters.NewExecutionBegins(f.s.Store)
	if err != nil {
		t.Fatal(err)
	}
	artSvc, err := artifacts.NewService(artifacts.ServiceConfig{
		Repository: f.art.Store, Authority: artAuthority.WithBegins(begins), Reader: f.art.Reader, Objects: objects, Registry: f.art.Registry,
		Clock: f.m.Clock.Now, UploadTTL: 10 * time.Minute, DownloadTTL: 5 * time.Minute, ClaimLease: 30 * time.Second, SpoolDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fin, err := resultsadapters.NewExecution(f.s.Svc, f.s.Store)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := resultsadapters.NewArtifacts(artSvc)
	if err != nil {
		t.Fatal(err)
	}
	resAuth, err := resultsadapters.NewAuthority(f.m.Verifier, f.m.Ring, f.m.Env.Grants, f.m.Clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	resSvc, err := results.NewService(results.Dependencies{Authority: resAuth, Contexts: fin, Evidence: ev, Finalizer: fin, Source: noSource{}}, results.Options{})
	if err != nil {
		t.Fatal(err)
	}
	route, err := execadapters.NewSecretRoute(f.m.Verifier, f.m.Ring, f.m.Env.Grants, noSnapshots{}, pubSecrets{values: map[string]string{"token": f.secret}}, f.m.Clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	f.artSvc = artSvc
	f.v1, f.v2, err = newRuntimeAPI(runtimeAPIConfig{
		Auth: f.m.Service, Results: resSvc, Verifier: f.m.Verifier, Artifacts: artSvc, Secrets: route, RateLimit: 100000,
		V2: &runtimeV2Config{Service: f.m.Service, Begins: execbridge.Begins{Exec: f.s.Svc, Recheck: f.m.Service}, Receipts: execbridge.Receipts{Exec: f.s.Svc, Recheck: f.m.Service}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Authenticated admission over the v2 route.
	body, _ := json.Marshal(admit)
	rec := f.do(f.v2, "POST", "/runtime/v2/attempts/"+string(f.ctx2.AttemptID)+"/admit", body, "")
	if rec.Code != 200 {
		t.Fatalf("admit: %d %s", rec.Code, rec.Body)
	}
	resp, err := runtimev2.DecodeResponse[runtimev2.AdmitResponseV2](rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	f.token = resp.Token.Token
	_ = ctx
	return f
}

func (f *v2Fx) do(h http.Handler, method, path string, body []byte, token string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (f *v2Fx) path(suffix string) string {
	return "/runtime/v2/attempts/" + string(f.ctx2.AttemptID) + suffix
}

// Test_unit_app_run_shared_v2_round_trips: an attempt of a run_shared run, as
// execution produced it, completes the authenticated v2 exchanges through the
// production collaborators: the secret it declared resolves, its artifact is
// reserved, uploaded and completed under the grant's pinned v2 context, and its
// result is accepted against that context; the same bearer is refused on the v1
// routes, and another attempt's, an unpinned reference and an undeclared output
// are refused on the v2 ones.
func Test_unit_app_run_shared_v2_round_trips(t *testing.T) {
	f := newV2Fx(t)
	attempt := string(f.ctx2.AttemptID)

	// Secret resolution: only what the attempt's context pins.
	pin := f.ctx2.Secrets[0]
	req, _ := json.Marshal(runtimev1.SecretResolveRequest{Ref: pin.Ref, Version: pin.Version})
	rec := f.do(f.v2, "POST", f.path("/secrets/resolve"), req, f.token)
	if rec.Code != 200 {
		t.Fatalf("resolve: %d %s", rec.Code, rec.Body)
	}
	var sec runtimev1.SecretResolveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sec); err != nil {
		t.Fatal(err)
	}
	if v, _ := base64.StdEncoding.DecodeString(sec.ValueBase64); string(v) != f.secret || sec.Name != pin.Name {
		t.Fatalf("resolved %+v", sec)
	}
	other, _ := json.Marshal(runtimev1.SecretResolveRequest{Ref: "secret://project/other", Version: 1})
	if rec := f.do(f.v2, "POST", f.path("/secrets/resolve"), other, f.token); rec.Code != 403 {
		t.Fatalf("an unpinned reference: %d %s", rec.Code, rec.Body)
	}

	// The command begins (execution's own record), then the artifact round trip.
	begin, _ := json.Marshal(runtimev2.BeginRequest{Workspace: &runtimev2.BeginWorkspace{Head: f.ctx2.Workspace.Source.HeadCommit, TrackedClean: true}})
	if rec := f.do(f.v2, "POST", f.path("/begin"), begin, f.token); rec.Code != 200 {
		t.Fatalf("begin: %d %s", rec.Code, rec.Body)
	}
	data := []byte("coverage report")
	sum := sha256.Sum256(data)
	digest := runtimev1.Digest("sha256:" + hex.EncodeToString(sum[:]))
	mt, size := "text/plain", int64(len(data))
	reserve, _ := json.Marshal(runtimev1.ArtifactReserveRequest{Name: "report", Kind: runtimev1.KindFile, Role: runtimev1.RoleOutput, Digest: digest, MediaType: &mt, SizeBytes: &size})
	rec = f.do(f.v2, "POST", f.path("/artifacts"), reserve, f.token)
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("reserve: %d %s", rec.Code, rec.Body)
	}
	var res runtimev1.ArtifactReserveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.Upload == nil {
		t.Fatalf("reserve response: %v %s", err, rec.Body)
	}
	up, err := http.NewRequest(res.Upload.Method, res.Upload.URL, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range res.Upload.Headers {
		if !strings.EqualFold(k, "Content-Length") && !strings.EqualFold(k, "Host") {
			up.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(up)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	complete, _ := json.Marshal(runtimev1.ArtifactCompleteRequest{Digest: digest, SizeBytes: size})
	if rec := f.do(f.v2, "POST", f.path("/artifacts/"+string(res.ArtifactID)+"/complete"), complete, f.token); rec.Code != 200 {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body)
	}
	undeclared, _ := json.Marshal(runtimev1.ArtifactReserveRequest{Name: "other", Kind: runtimev1.KindFile, Role: runtimev1.RoleOutput, Digest: digest, MediaType: &mt, SizeBytes: &size})
	if rec := f.do(f.v2, "POST", f.path("/artifacts"), undeclared, f.token); rec.Code < 400 {
		t.Fatalf("an undeclared output was reserved: %d %s", rec.Code, rec.Body)
	}

	// The result is accepted against the pinned v2 context and acknowledged durably.
	zero := int64(0)
	result := runtimev1.Result{
		Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, DiagnosticArtifactIDs: []domain.ArtifactID{},
		Outputs: map[string]runtimev1.Value{"report": runtimev1.StringValue(string(res.ArtifactID))},
	}
	hash, err := runtimev1.ResultHash(result)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(runtimev1.ResultRequest{ResultHash: hash, Result: result})
	rec = f.do(f.v2, "POST", f.path("/result"), body, f.token)
	if rec.Code != 200 {
		t.Fatalf("result: %d %s", rec.Code, rec.Body)
	}
	var ack runtimev1.ResultResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil || ack.ResultHash != hash || ack.AttemptStatus != runtimev1.AttemptSucceeded {
		t.Fatalf("ack %+v %v", ack, err)
	}
	agg := f.run.Get()
	if at := agg.Attempt(f.ctx2.AttemptID); at == nil || at.ResultHash != string(hash) {
		t.Fatal("execution did not record the accepted result")
	}
	// A replay of the accepted hash is acknowledged again.
	if rec := f.do(f.v2, "POST", f.path("/result"), body, f.token); rec.Code != 200 {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body)
	}

	// Fences: the v2 attempt token on the v1 routes, and another attempt's path.
	for _, p := range []string{"/result", "/secrets/resolve", "/artifacts"} {
		if rec := f.do(f.v1, "POST", "/runtime/v1/attempts/"+attempt+p, []byte(`{}`), f.token); rec.Code != 409 && rec.Code != 403 {
			t.Errorf("v1 %s with a v2 token: %d %s", p, rec.Code, rec.Body)
		}
	}
	if rec := f.do(f.v2, "POST", "/runtime/v2/attempts/att_other/secrets/resolve", req, f.token); rec.Code < 400 {
		t.Fatalf("another attempt's path: %d", rec.Code)
	}
}

// begin records the command begin through the v2 route; the workspace is modified
// when untracked files are present (execution derives the state, never the
// entrypoint).
func (f *v2Fx) begin(untracked bool) {
	f.t.Helper()
	body, _ := json.Marshal(runtimev2.BeginRequest{Workspace: &runtimev2.BeginWorkspace{Head: f.ctx2.Workspace.Source.HeadCommit, TrackedClean: true, UntrackedPresent: untracked, IgnoredPresent: untracked}})
	if rec := f.do(f.v2, "POST", f.path("/begin"), body, f.token); rec.Code != 200 {
		f.t.Fatalf("begin: %d %s", rec.Code, rec.Body)
	}
}

// reserve reserves the declared file output and returns its artifact ID.
func (f *v2Fx) reserve() (domain.ArtifactID, int) {
	f.t.Helper()
	data := []byte("generated")
	sum := sha256.Sum256(data)
	mt, size := "text/plain", int64(len(data))
	body, _ := json.Marshal(runtimev1.ArtifactReserveRequest{Name: "report", Kind: runtimev1.KindFile, Role: runtimev1.RoleOutput,
		Digest: runtimev1.Digest("sha256:" + hex.EncodeToString(sum[:])), MediaType: &mt, SizeBytes: &size})
	rec := f.do(f.v2, "POST", f.path("/artifacts"), body, f.token)
	var res runtimev1.ArtifactReserveResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	return res.ArtifactID, rec.Code
}

// Test_unit_app_run_shared_artifact_provenance: what a run_shared attempt
// produces carries the workspace state the SERVER recorded when its command began
// (DQ-16): clean or modified, whether ignored files were present and a reference
// to the lineage its context carried, for file artifacts and OCI registrations
// alike. An attempt whose begin is not recorded produces nothing; an artifact
// built in a modified workspace is not the approved commit for a gate or a
// deployment, while a clean one is.
func Test_unit_app_run_shared_artifact_provenance(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		untracked bool
		state     string
	}{{"clean", false, artifacts.WorkspaceClean}, {"modified", true, artifacts.WorkspaceModified}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newV2Fx(t)
			scope := domain.Scope{TenantID: f.ctx2.TenantID, ProjectID: f.ctx2.ProjectID}
			// Before the begin there is no workspace state: nothing is reserved, and no registration resolves.
			if _, code := f.reserve(); code < 400 {
				t.Fatalf("an artifact was reserved before the command began: %d", code)
			}
			oci := attemptProvenance{runs: f.s.Store}
			q := ociQuery(scope, f.ctx2)
			if _, err := oci.Resolve(ctx, q); err == nil {
				t.Fatal("a registration resolved before the command began")
			}

			f.begin(tc.untracked)
			id, code := f.reserve()
			if code != 200 && code != 201 {
				t.Fatalf("reserve: %d", code)
			}
			want, err := resultsadapters.LineageRef(f.ctx2.Workspace.Lineage)
			if err != nil {
				t.Fatal(err)
			}
			a, err := f.art.Store.Get(ctx, scope, id)
			if err != nil {
				t.Fatal(err)
			}
			p := a.Provenance()
			if p.RepositoryURL != f.ctx2.Workspace.Source.RepositoryURL || p.Commit != string(f.ctx2.Workspace.Source.HeadCommit) ||
				p.WorkspaceState != tc.state || p.IgnoredPresent != tc.untracked || p.Lineage != want || p.Modified() != (tc.state == artifacts.WorkspaceModified) {
				t.Fatalf("file provenance %+v", p)
			}
			r, err := oci.Resolve(ctx, q)
			if err != nil || r != p {
				t.Fatalf("OCI provenance %+v (%v), want the file's %+v", r, err, p)
			}

			// Consumers: only a clean workspace stands for the approved commit.
			ev, err := gatesadapters.NewEvidence(f.art.Store, f.art.Objects)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := ev.Describe(ctx, scope, id)
			if err != nil {
				t.Fatal(err)
			}
			lookup, err := deliveryadapters.NewArtifacts(f.artSvc)
			if err != nil {
				t.Fatal(err)
			}
			img, err := lookup.Evidence(ctx, scope, id)
			if err != nil {
				t.Fatal(err)
			}
			if (img.Provenance == delivery.Source{}) != (tc.state == artifacts.WorkspaceModified) {
				t.Fatalf("a %s workspace's artifact has delivery provenance %+v", tc.state, img.Provenance)
			}
			connected := meta.Repository != "" && meta.Commit == p.Commit
			if connected == (tc.state == artifacts.WorkspaceModified) {
				t.Fatalf("a %s workspace's artifact connected=%v to its commit: %+v", tc.state, connected, meta)
			}
		})
	}
}

// ociQuery is the registration resolver's query for the attempt.
func ociQuery(scope domain.Scope, c runtimev2.ContextV2) oci.ProvenanceQuery {
	return oci.ProvenanceQuery{Scope: scope, Attempt: c.AttemptID, Run: c.RunID, Repository: "registry.example.test/app", Digest: artifacts.DigestOf([]byte("image"))}
}
