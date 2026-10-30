package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/api"
	"github.com/optimiware/kodama/internal/kernel/store"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/postgres"
)

type noWorkers struct{ api.GitWorkers }

type noSigner struct{}

func (noSigner) SignForwarding(context.Context, gitv1.ForwardingAssertion) (string, error) {
	return "", errors.New("not used")
}

type pgAuth struct{}

func (pgAuth) Authenticate(context.Context, *http.Request) (api.Principal, error) {
	return api.Principal{}, api.ErrUnauthenticated
}

// Test_unit_kernel_git_run_context_credential_postgres: against PostgreSQL (skipped
// without it) a run's credential for the tenant-level context repository, whose
// project_id is null, is stored with the run's project, is read with the run's
// liveness, ends with the run, and the schema still refuses every other
// mismatch of a credential's scope with its repository.
func Test_unit_kernel_git_run_context_credential_postgres(t *testing.T) {
	ctx := t.Context()
	db := postgres.New(t)
	mig := db.MigrationPool(ctx, 2)
	m, err := storage.NewManifest(storage.BaseContribution())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Migrate(ctx, mig, m, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO tenants (id,key) VALUES ('t_a','a')`,
		`INSERT INTO users (id,issuer,subject) VALUES ('u1','iss','s1')`,
		`INSERT INTO projects (tenant_id,id,key,task_workflow) VALUES ('t_a','p_a1','erp','{}'),('t_a','p_a2','web','{}')`,
		`INSERT INTO pipelines (tenant_id,project_id,id,name,definition,source) VALUES ('t_a','p_a1','pl_1','build','{}','api'),('t_a','p_a2','pl_2','build','{}','api')`,
	} {
		if _, err := mig.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s := store.New(db.RuntimePool(ctx, 4))
	repo := func(project domain.ProjectID, id, name string) store.Repository {
		t.Helper()
		r, created, err := s.CreateRepository(ctx, store.Repository{TenantID: "t_a", ProjectID: project, ID: id, Name: name, DefaultBranch: "main"})
		if err != nil || !created {
			t.Fatalf("create %s: %v", id, err)
		}
		return r
	}
	repo("", "repo_tc", "context")
	repo("p_a1", "repo_pc", "context")
	repo("p_a1", "repo_src", "backend")
	repo("p_a2", "repo_other", "backend")
	src := `[{"repositoryId":"repo_src","commit":"` + c1 + `","path":".","access":"write","branch":"refs/heads/kodama/work/tsk_1"}]`
	for _, q := range []string{
		`INSERT INTO runs (tenant_id,project_id,id,pipeline_id,definition_snapshot,sources) VALUES ('t_a','p_a1','run_1','pl_1','{}','` + src + `')`,
		`INSERT INTO runs (tenant_id,project_id,id,pipeline_id,definition_snapshot) VALUES ('t_a','p_a2','run_2','pl_2','{}')`,
	} {
		if _, err := mig.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	git, err := api.NewGit(api.GitConfig{Store: s, Auth: pgAuth{}, Workers: noWorkers{}, Signer: noSigner{}})
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"repo_tc", "repo_pc", "repo_src"} {
		issued, err := git.IssueRunCredential(ctx, "t_a", "p_a1", "run_1", id)
		if err != nil {
			t.Fatalf("issue for %s: %v", id, err)
		}
		cred, err := s.CredentialByDigest(ctx, "t_a", store.DigestOf(issued.Secret))
		if err != nil || cred.RepoID != id || cred.ProjectID != "p_a1" || cred.RunID != "run_1" || cred.Kind != store.CredRun || !cred.Live(time.Now()) {
			t.Fatalf("credential for %s = %+v, %v", id, cred, err)
		}
		// The run is read with the credential's project, also for the tenant-level repository.
		if info, err := s.RunGitInfo(ctx, "t_a", cred.ProjectID, cred.RunID); err != nil || !info.Active() {
			t.Fatalf("run of the %s credential = %+v, %v", id, info, err)
		}
	}
	// The tenant context of a project that has no repository of its own.
	if _, err := git.IssueRunCredential(ctx, "t_a", "p_a2", "run_2", "repo_tc"); err != nil {
		t.Fatalf("the tenant's context for another project's run: %v", err)
	}
	// Not another project's repositories, and not an unpinned source.
	if _, err := git.IssueRunCredential(ctx, "t_a", "p_a2", "run_2", "repo_pc"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another project's context: %v", err)
	}
	if _, err := git.IssueRunCredential(ctx, "t_a", "p_a1", "run_1", "repo_other"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another project's repository: %v", err)
	}

	// The schema keeps every other scope mismatch out.
	run := func(id, repoID string, project domain.ProjectID, kind string) store.Credential {
		c := store.Credential{TenantID: "t_a", ProjectID: project, ID: id, RepoID: repoID, Kind: kind, Digest: store.DigestOf(id),
			Claims: []byte(`{}`), IssuedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Hour)}
		if kind == store.CredHuman {
			c.UserID = "u1"
		} else {
			c.RunID = "run_1"
		}
		return c
	}
	for name, c := range map[string]store.Credential{
		"a run credential for a project repository of another project": run("gg_x1", "repo_other", "p_a1", store.CredRun),
		"an integration credential for the tenant repository":          run("gg_x2", "repo_tc", "p_a1", store.CredIntegration),
		"a human credential with a project for the tenant repository":  run("gg_x3", "repo_tc", "p_a1", store.CredHuman),
		"a run credential without a project":                           run("gg_x4", "repo_tc", "", store.CredRun),
	} {
		if err := s.IssueCredential(ctx, c); err == nil {
			t.Errorf("%s was stored", name)
		}
	}
	// A human credential keeps the equality rule, both ways.
	if err := s.IssueCredential(ctx, run("gg_h1", "repo_tc", "", store.CredHuman)); err != nil {
		t.Errorf("a human credential for the tenant repository: %v", err)
	}
	if err := s.IssueCredential(ctx, run("gg_h2", "repo_pc", "p_a1", store.CredHuman)); err != nil {
		t.Errorf("a human credential for a project repository: %v", err)
	}
	if err := s.IssueCredential(ctx, run("gg_h3", "repo_pc", "", store.CredHuman)); err == nil {
		t.Error("a human credential without the repository's project was stored")
	}

	// The credentials end with the run, the tenant-level one included.
	issued, err := git.IssueRunCredential(ctx, "t_a", "p_a1", "run_1", "repo_tc")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, "t_a", "p_a1", "run_1", "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	cred, err := s.CredentialByDigest(ctx, "t_a", store.DigestOf(issued.Secret))
	if err != nil || cred.RevokedAt == nil || cred.Live(time.Now()) {
		t.Fatalf("the tenant context credential outlived the run: %+v, %v", cred, err)
	}
	if _, err := git.IssueRunCredential(ctx, "t_a", "p_a1", "run_1", "repo_tc"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a finished run was issued a credential: %v", err)
	}
}
