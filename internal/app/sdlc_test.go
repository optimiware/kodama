package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	cliauth "github.com/optimiware/kodama/internal/cli/auth"
	clirelease "github.com/optimiware/kodama/internal/cli/release"
	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryadapters "github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

type sdlcTestAuth struct {
	scope domain.Scope
}

func (a sdlcTestAuth) AuthorizeReleaseRead(_ context.Context, caller artifacts.Caller, scope domain.Scope) error {
	if caller.Subject == "" {
		return artifacts.ErrAuthorityMissing
	}
	if scope != a.scope {
		return artifacts.ErrNotFound
	}
	return nil
}

func (a sdlcTestAuth) AuthorizeReleaseWrite(_ context.Context, caller artifacts.Caller, scope domain.Scope) (artifacts.Actor, error) {
	if caller.Subject == "" {
		return artifacts.Actor{}, artifacts.ErrAuthorityMissing
	}
	if scope != a.scope {
		return artifacts.Actor{}, artifacts.ErrForbidden
	}
	return artifacts.Actor{Type: "user", ID: caller.Subject}, nil
}

func (a sdlcTestAuth) AuthorizeInputFetch(_ context.Context, _ artifacts.Caller, _ domain.AttemptID, _ artifactsv1.InputFetchRequest) (domain.Scope, time.Time, error) {
	return a.scope, time.Now().UTC().Add(time.Hour), nil
}

func (a sdlcTestAuth) AuthorizePublication(_ context.Context, _ artifacts.Caller, attempt domain.AttemptID, _ domain.ArtifactID, _ artifactsv1.DestinationRef) (artifacts.AttemptGrant, error) {
	return artifacts.AttemptGrant{
		Scope:     a.scope,
		Attempt:   attempt,
		Run:       "run_pub",
		Step:      "publish",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}, nil
}

type sdlcTestPubVerifier struct{}

func (sdlcTestPubVerifier) VerifyCopy(_ context.Context, _ domain.Scope, _ artifacts.Artifact, _ artifactsv1.DestinationRef) error {
	return nil
}

type sdlcDeliveryAuth struct {
	scope   domain.Scope
	attempt domain.AttemptID
	run     domain.RunID
	token   string
}

func (a sdlcDeliveryAuth) Authenticate(_ context.Context, attempt domain.AttemptID, bearer string) (delivery.Principal, error) {
	if bearer == "" || bearer != a.token {
		return delivery.Principal{}, delivery.Fail(delivery.ErrUnauthenticated, nil, "invalid runtime token")
	}
	if attempt != a.attempt {
		return delivery.Principal{}, delivery.Fail(delivery.ErrDenied, nil, "token does not match route attempt")
	}
	return delivery.Principal{Scope: a.scope, Run: a.run, Attempt: a.attempt}, nil
}

func (a sdlcDeliveryAuth) Bind(ctx context.Context, _ delivery.Principal) (context.Context, error) {
	return ctx, nil
}

type sdlcDeliveryAccess struct {
	scope domain.Scope
}

func (a sdlcDeliveryAccess) Authorize(_ context.Context, actor delivery.Actor, scope domain.Scope, _ delivery.Permission) error {
	if actor.ID == "" || scope != a.scope {
		return delivery.Fail(delivery.ErrDenied, nil, "forbidden")
	}
	return nil
}

type sdlcPinnedContexts struct {
	ctx runtimev1.Context
}

func (p sdlcPinnedContexts) Pinned(_ context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimev1.Context, error) {
	if p.ctx.Scope() != scope || p.ctx.AttemptID != attempt {
		return runtimev1.Context{}, delivery.Fail(delivery.ErrPinRequired, nil, "missing pinned context")
	}
	return p.ctx, nil
}

func newSDLCSliceForTest(t *testing.T, e *artifactstest.MemEnv, scope domain.Scope, env Env, mode LegacyDeliveryMode) (*ContainerSDLCSlice, *deliverytest.GenericMemStore) {
	t.Helper()
	gStore, err := deliverytest.NewGenericMemStore(e.DB)
	if err != nil {
		t.Fatal(err)
	}
	delArts, err := deliveryadapters.NewArtifacts(e.Service)
	if err != nil {
		t.Fatal(err)
	}
	pin := runtimev1.EnvironmentPin{ID: "env_prod", ConfigDigest: runtimev1.Digest("sha256:" + strings.Repeat("e", 64))}
	pCtx := runtimev1.Context{
		TenantID:    scope.TenantID,
		ProjectID:   scope.ProjectID,
		RunID:       "run_wrangler",
		StepID:      "deploy",
		AttemptID:   "att_wrangler_1",
		ActionKey:   "run_wrangler.deploy",
		Environment: &pin,
		Command: runtimev1.Command{
			Image:   "registry.example.invalid/wrangler@sha256:" + strings.Repeat("c", 64),
			Command: []string{"wrangler", "deploy"},
		},
		Workspace: runtimev1.Workspace{
			Access: runtimev1.AccessRead,
			Source: &runtimev1.Source{
				RepositoryURL: artifactstest.Source.RepositoryURL,
				Commit:        runtimev1.GitCommit(artifactstest.Source.Commit),
			},
		},
	}
	gSvc, err := delivery.NewGenericService(delivery.GenericServiceConfig{
		Store:     gStore,
		Access:    sdlcDeliveryAccess{scope: scope},
		Authority: sdlcDeliveryAuth{scope: scope, attempt: "att_wrangler_1", run: "run_wrangler", token: "rt_tok_wrangler"},
		Contexts:  sdlcPinnedContexts{ctx: pCtx},
		Artifacts: delArts,
		Clock:     e.Clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	slice, err := AssembleContainerSDLC(ContainerSDLCInputs{
		Env:               env,
		Artifacts:         e.Service,
		ReleaseStore:      e.Store,
		ReleaseAuthority:  sdlcTestAuth{scope: scope},
		PublicationVerify: sdlcTestPubVerifier{},
		GenericStore:      gStore,
		GenericDelivery:   gSvc,
		LegacyMode:        mode,
	})
	return slice, gStore
}

func Test_unit_container_sdlc_composition(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}

	// 1. Production environment refuses development-only memory stores.
	if slice, _ := newSDLCSliceForTest(t, e, scope, Production, LegacyDeliveryDrain); slice != nil {
		t.Fatal("expected Production to refuse development-only release and generic delivery stores")
	}

	// 2. Development composition wires cleanly and SDLCManifest preserves the
	//    exact ledger prefix of Contributions().
	slice, _ := newSDLCSliceForTest(t, e, scope, Development, LegacyDeliveryDrain)
	if slice == nil || slice.Handler == nil || slice.Effects == nil {
		t.Fatal("AssembleContainerSDLC returned nil slice or collaborators")
	}
	m, err := SDLCManifest()
	if err != nil {
		t.Fatalf("SDLCManifest: %v", err)
	}
	var ids []string
	for _, mig := range m.Migrations() {
		ids = append(ids, mig.ID)
	}
	if len(ids) != 6 || fmt.Sprint(ids[:5]) != "[001_schema.sql 002_grants.sql 003_git_hosting.sql 004_pipeline_runtime_expand.sql 005_durable_run_state.sql]" {
		t.Fatalf("SDLCManifest must be the five-file baseline plus the release migration, got %v", ids)
	}
}

func Test_unit_generic_tools_without_catalogues(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	slice, _ := newSDLCSliceForTest(t, e, scope, Development, LegacyDeliveryDrain)
	srv := httptest.NewServer(slice.Handler)
	t.Cleanup(srv.Close)

	// Seed a usable bundle artifact that the deployment will reference and pin.
	caller := e.Attempt(scope, "att_build", artifacts.OutputDecl{Name: "bundle", Kind: artifacts.KindFile})
	data := []byte("wrangler-worker-bundle-bytes")
	dg := artifacts.DigestOf(data)
	sz := int64(len(data))
	mt := "application/x-tar"
	res, err := e.Service.Reserve(t.Context(), caller, "att_build", artifacts.ReserveInput{
		Name: "bundle", Kind: artifacts.KindFile, Role: artifacts.RoleOutput, Digest: dg, MediaType: &mt, Size: &sz,
	}, "req_r")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := e.Store.Get(t.Context(), scope, res.ID)
	_ = e.Objects.Put(t.Context(), artifacts.PutSpec{Key: stored.StagingKey(), Body: bytes.NewReader(data), Size: sz, Digest: dg, MediaType: mt})
	if _, err := e.Service.Complete(t.Context(), caller, "att_build", res.ID, artifacts.CompleteInput{Digest: dg, Size: sz}, "req_c"); err != nil {
		t.Fatal(err)
	}

	// 1. Unauthenticated or mismatched-attempt generic deployment registration is rejected!
	unauthReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/runtime/v2/attempts/att_wrangler_1/deployments/generic", strings.NewReader(`{}`))
	unauthResp, err := srv.Client().Do(unauthReq)
	if err != nil || unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated deployment registration, got %v %v", unauthResp, err)
	}
	wrongAttemptReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/runtime/v2/attempts/att_other/deployments/generic", strings.NewReader(`{}`))
	wrongAttemptReq.Header.Set("Authorization", "Bearer rt_tok_wrangler")
	wrongAttemptResp, err := srv.Client().Do(wrongAttemptReq)
	if err != nil || wrongAttemptResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for mismatched route attempt, got %v %v", wrongAttemptResp, err)
	}

	// 2. Absent record returns VerdictStillUnknown (never blind replay).
	q := execution.EffectQuery{
		Scope:     scope,
		RunID:     "run_wrangler",
		StepID:    "deploy",
		AttemptID: "att_wrangler_1",
		ActionKey: "run_wrangler.deploy",
	}
	ev, err := slice.Effects.Reconcile(t.Context(), q)
	if err != nil || ev.Verdict != execution.VerdictStillUnknown {
		t.Fatalf("expected VerdictStillUnknown without a settled generic deployment, got %+v err=%v", ev, err)
	}

	// 3. Authenticated registration derives scope/attempt/run/actionKey/pins from
	//    the grant's pinned context AND pins the referenced artifact against retention!
	regBody := `{"artifacts":[{"id":"` + string(res.ID) + `","digest":"` + string(dg) + `"}]}`
	regReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/runtime/v2/attempts/att_wrangler_1/deployments/generic", strings.NewReader(regBody))
	regReq.Header.Set("Authorization", "Bearer rt_tok_wrangler")
	regResp, err := srv.Client().Do(regReq)
	if err != nil || regResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created for authenticated generic deployment, got %v %v", regResp, err)
	}
	var regDoc struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(regResp.Body).Decode(&regDoc)
	_ = regResp.Body.Close()

	// Verify the deployment pinned the artifact against retention deletion!
	if err := e.Store.BeginRetentionDeletion(t.Context(), scope, res.ID, e.Clock.Now(), artifacts.Actor{Type: "system", ID: "ret"}); !errors.Is(err, artifacts.ErrPinned) {
		t.Fatalf("expected generic deployment registration to pin its artifact, got %v", err)
	}

	// 4. Report unknown outcome, then settle explicitly as an authorized engineer -> VerdictSettled!
	unkReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/runtime/v2/attempts/att_wrangler_1/deployments/generic/"+regDoc.ID+"/outcome",
		strings.NewReader(`{"status":"unknown","attribution":"command_reported"}`))
	unkReq.Header.Set("Authorization", "Bearer rt_tok_wrangler")
	unkResp, err := srv.Client().Do(unkReq)
	if err != nil || unkResp.StatusCode != http.StatusOK {
		t.Fatalf("outcome unknown report failed: %v %v", unkResp, err)
	}
	_ = unkResp.Body.Close()

	settleReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/tenants/t_acme/projects/p_inventory/deployments/generic/"+regDoc.ID+"/settle",
		strings.NewReader(`{"status":"succeeded","reason":"ready","externalRefs":{"worker_id":"w_1"}}`))
	settleReq.Header.Set("Authorization", "Bearer usr_eng")
	settleResp, err := srv.Client().Do(settleReq)
	if err != nil || settleResp.StatusCode != http.StatusOK {
		t.Fatalf("explicit settlement failed: %v %v", settleResp, err)
	}
	_ = settleResp.Body.Close()

	evSettled, err := slice.Effects.Reconcile(t.Context(), q)
	if err != nil || evSettled.Verdict != execution.VerdictSettled {
		t.Fatalf("expected VerdictSettled after explicit settlement, got %+v err=%v", evSettled, err)
	}
}

func Test_unit_sdlc_cli_roundtrip(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}

	caller := e.Attempt(scope, "att_build", artifacts.OutputDecl{Name: "bundle", Kind: artifacts.KindFile})
	data := []byte("cli-roundtrip-archive-bytes")
	dg := artifacts.DigestOf(data)
	sz := int64(len(data))
	mt := "application/x-tar"
	res, err := e.Service.Reserve(t.Context(), caller, "att_build", artifacts.ReserveInput{
		Name: "bundle", Kind: artifacts.KindFile, Role: artifacts.RoleOutput, Digest: dg, MediaType: &mt, Size: &sz,
	}, "req_r")
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := e.Store.Get(t.Context(), scope, res.ID)
	_ = e.Objects.Put(t.Context(), artifacts.PutSpec{Key: stored.StagingKey(), Body: bytes.NewReader(data), Size: sz, Digest: dg, MediaType: mt})
	if _, err := e.Service.Complete(t.Context(), caller, "att_build", res.ID, artifacts.CompleteInput{Digest: dg, Size: sz}, "req_c"); err != nil {
		t.Fatal(err)
	}

	slice, _ := newSDLCSliceForTest(t, e, scope, Development, LegacyDeliveryDrain)
	srv := httptest.NewServer(slice.Handler)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	ctxFile := filepath.Join(dir, "contexts.json")
	credDir := filepath.Join(dir, "creds")
	_ = os.MkdirAll(credDir, 0o700)

	key := (root.Context{Factory: srv.URL}).CredentialKey()
	if err := cliauth.NewFileStore(credDir).Save(t.Context(), key, cliauth.Credential{
		AccessToken: "usr_eng", TokenType: "Bearer", ObtainedAt: time.Now(), Expiry: time.Now().Add(10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	runCLI := func(args ...string) (string, int) {
		var out, errBuf bytes.Buffer
		runner, err := NewSDLCCLI(CLIConfig{
			Streams: root.Streams{In: strings.NewReader(""), Out: &out, Err: &errBuf},
			HTTP:    srv.Client(),
			Getenv: func(k string) string {
				switch k {
				case "KODAMA_CONTEXT_FILE":
					return ctxFile
				case "KODAMA_CREDENTIAL_DIR":
					return credDir
				case "KODAMA_ALLOW_INSECURE_LOOPBACK":
					return "true"
				}
				return ""
			},
		})
		if err != nil {
			t.Fatalf("NewSDLCCLI: %v", err)
		}
		fullArgs := append(args, "--factory", srv.URL, "--tenant", string(scope.TenantID), "--project", string(scope.ProjectID), "--credential-store", "file", "--non-interactive", "--output", "json")
		code := runner.Run(t.Context(), fullArgs)
		return out.String(), int(code)
	}

	sel, err := clirelease.ParseReleaseFlag("application@1.4.2", []string{"bundle=web-bundle", "image=api-image"})
	if err != nil || sel.Name != "application" || sel.VersionLabel != "1.4.2" || len(sel.Bindings) != 2 {
		t.Fatalf("ParseReleaseFlag: %+v err=%v", sel, err)
	}

	manifestFile := filepath.Join(dir, "release.json")
	manifestJSON := `{
		"name": "application",
		"versionLabel": "1.4.2",
		"artifacts": {
			"bundle": {"artifactId": "` + string(res.ID) + `", "digest": "` + string(dg) + `", "kind": "file", "mediaType": "application/x-tar"}
		},
		"sources": [{"repositoryUrl": "https://git.example.test/acme/app.git", "commit": "` + strings.Repeat("1", 40) + `"}],
		"producers": [{"artifactId": "` + string(res.ID) + `", "attemptId": "att_build", "runId": "run_1", "stepId": "build"}]
	}`
	_ = os.WriteFile(manifestFile, []byte(manifestJSON), 0o600)

	out, code := runCLI("release", "create", "--file", manifestFile, "--idempotency-key", "rel-142")
	if code != 0 || !strings.Contains(out, `"versionLabel":"1.4.2"`) {
		t.Fatalf("release create failed (code %d): %s", code, out)
	}

	resolvedOut, code := runCLI("release", "resolve", "--name", "application", "--version", "1.4.2")
	if code != 0 || !strings.Contains(resolvedOut, string(res.ID)) {
		t.Fatalf("release resolve failed (code %d): %s", code, resolvedOut)
	}
}

func Test_unit_legacy_delivery_drain(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	slice, _ := newSDLCSliceForTest(t, e, scope, Development, LegacyDeliveryDrain)
	if err := slice.AdmitLegacyDelivery(); err == nil {
		t.Fatal("expected LegacyDeliveryDrain to refuse new legacy single-image/Kubernetes delivery admissions")
	}
}
