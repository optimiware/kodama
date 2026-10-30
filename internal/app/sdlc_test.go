package app

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/artifactstest"
	cliauth "github.com/optimiware/kodama/internal/cli/auth"
	clirelease "github.com/optimiware/kodama/internal/cli/release"
	"github.com/optimiware/kodama/internal/cli/root"
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

func Test_unit_container_sdlc_composition(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}

	// 1. Production environment refuses the development-only memory release store.
	if _, err := AssembleContainerSDLC(ContainerSDLCInputs{
		Env:               Production,
		Artifacts:         e.Service,
		ReleaseStore:      e.Store,
		ReleaseAuthority:  sdlcTestAuth{scope: scope},
		PublicationVerify: sdlcTestPubVerifier{},
	}); err == nil {
		t.Fatal("expected Production to refuse development-only release store")
	}

	// 2. Development composition wires cleanly and SDLCManifest preserves the
	//    exact ledger prefix of Contributions().
	slice, err := AssembleContainerSDLC(ContainerSDLCInputs{
		Env:               Development,
		Artifacts:         e.Service,
		ReleaseStore:      e.Store,
		ReleaseAuthority:  sdlcTestAuth{scope: scope},
		PublicationVerify: sdlcTestPubVerifier{},
	})
	if err != nil || slice.Handler == nil || slice.Effects == nil {
		t.Fatalf("AssembleContainerSDLC: %v", err)
	}
	m, err := SDLCManifest()
	if err != nil {
		t.Fatalf("SDLCManifest: %v", err)
	}
	if len(m.Migrations()) != 5 {
		t.Fatalf("expected 5 migrations in SDLCManifest, got %d", len(m.Migrations()))
	}
}

func Test_unit_generic_tools_without_catalogues(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	slice, err := AssembleContainerSDLC(ContainerSDLCInputs{
		Env:               Development,
		Artifacts:         e.Service,
		ReleaseStore:      e.Store,
		ReleaseAuthority:  sdlcTestAuth{scope: scope},
		PublicationVerify: sdlcTestPubVerifier{},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Generic tool-neutral deployment registration and effect reconciliation work
	// with zero ToolQualification / delivery-tools.json or model-tools.json catalogue.
	// An absent record returns VerdictStillUnknown (never blind replay).
	ev, err := slice.Effects.Reconcile(t.Context(), execution.EffectQuery{
		Scope:     scope,
		RunID:     "run_wrangler",
		StepID:    "deploy",
		AttemptID: "att_wrangler_1",
		ActionKey: "run_wrangler.deploy",
	})
	if err != nil || ev.Verdict != execution.VerdictStillUnknown {
		t.Fatalf("expected VerdictStillUnknown without a settled generic deployment, got %+v err=%v", ev, err)
	}
}

func Test_unit_sdlc_cli_roundtrip(t *testing.T) {
	e := artifactstest.NewMemEnv(t)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}

	// Seed one usable file artifact in the store.
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

	slice, err := AssembleContainerSDLC(ContainerSDLCInputs{
		Env:               Development,
		Artifacts:         e.Service,
		ReleaseStore:      e.Store,
		ReleaseAuthority:  sdlcTestAuth{scope: scope},
		PublicationVerify: sdlcTestPubVerifier{},
	})
	if err != nil {
		t.Fatal(err)
	}
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

	// 1. ParseReleaseFlag validates --release <name>@<version> and --bind <input>=<entry>.
	sel, err := clirelease.ParseReleaseFlag("application@1.4.2", []string{"bundle=web-bundle", "image=api-image"})
	if err != nil || sel.Name != "application" || sel.VersionLabel != "1.4.2" || len(sel.Bindings) != 2 {
		t.Fatalf("ParseReleaseFlag: %+v err=%v", sel, err)
	}

	// 2. Create release via CLI, resolve it, and retire it.
	manifestFile := filepath.Join(dir, "release.json")
	manifestJSON := `{
		"name": "application",
		"versionLabel": "1.4.2",
		"artifacts": {
			"bundle": {"artifactId": "` + string(res.ID) + `", "digest": "` + string(dg) + `", "kind": "file", "mediaType": "application/x-tar"}
		},
		"sources": [{"repositoryUrl": "https://git.example.test/acme/app.git", "commit": "` + strings.Repeat("a", 40) + `"}],
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
	slice, err := AssembleContainerSDLC(ContainerSDLCInputs{
		Env:               Development,
		Artifacts:         e.Service,
		ReleaseStore:      e.Store,
		ReleaseAuthority:  sdlcTestAuth{scope: scope},
		PublicationVerify: sdlcTestPubVerifier{},
		LegacyMode:        LegacyDeliveryDrain,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := slice.AdmitLegacyDelivery(); err == nil {
		t.Fatal("expected LegacyDeliveryDrain to refuse new legacy single-image/Kubernetes delivery admissions")
	}
}
