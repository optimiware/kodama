package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/api"
	"github.com/optimiware/kodama/internal/shell/store"
)

// Test_unit_kernel_git_run_context_read_only: a run reads its tenant's and its
// project's context repositories with its own credential, whether or not a
// context is also pinned, and can never update one: the credential names no
// update, the Git endpoint refuses its receive, and the ref policy refuses every
// ref even for a record that was forged to name one.
func Test_unit_kernel_git_run_context_read_only(t *testing.T) {
	e := newEnv(t)
	_, backendID := e.seeded("backend", nil)
	_, projCtxID := e.seeded("context", nil)
	tenantCtx := e.createRepo("tok-admin", "", "context", nil)
	tenantCtxID := str(tenantCtx["id"])
	if _, err := e.mem.CommitReceive(t.Context(), store.ReceiveCommit{TenantID: tenantID, RepositoryID: tenantCtxID, ReceiveID: "rcv_seed_tenant_context",
		ActorType: "user", ActorID: "u_admin", Updates: []store.ReceiveUpdate{{Ref: "refs/heads/main", Old: z0, New: c1, Kind: "create"}}}); err != nil {
		t.Fatal(err)
	}
	// The run has write access to its own source and, in the second run, to a
	// "source" that is the project's context itself.
	e.mem.SetRun(runInfo("run_1", "build", "tsk_1", backendID))
	e.mem.SetRun(runInfo("run_2", "build", "tsk_2", projCtxID))

	type ctxRepo struct {
		name, path, id string
	}
	contexts := []ctxRepo{{"tenant", "/git/acme/context.git", tenantCtxID}, {"project", "/git/acme/erp/context.git", projCtxID}}

	for _, run := range []string{"run_1", "run_2"} {
		for _, c := range contexts {
			t.Run(run+" "+c.name+" context", func(t *testing.T) {
				issued, err := e.git.IssueRunCredential(t.Context(), tenantID, projectID, run, c.id)
				if err != nil {
					t.Fatalf("issue: %v", err)
				}
				rec, err := e.mem.GetCredential(t.Context(), tenantID, issued.GrantID)
				if err != nil || rec.Kind != store.CredRun || rec.ProjectID != projectID || rec.RunID != run || rec.RepoID != c.id {
					t.Fatalf("record = %+v, %v", rec, err)
				}
				var claims api.GitClaims
				if err := json.Unmarshal(rec.Claims, &claims); err != nil {
					t.Fatal(err)
				}
				if len(claims.Repositories) != 1 || !claims.Repositories[0].Read || len(claims.Repositories[0].Update) != 0 ||
					claims.Repositories[0].Seed || claims.Repositories[0].Integration != nil || claims.ProjectID != string(projectID) {
					t.Fatalf("claims = %+v: a context is read-only", claims)
				}
				// Readable, never receivable.
				if r := e.gitGet(c.path+"/info/refs?service=git-upload-pack", issued.Secret, http.MethodGet); r.status != http.StatusOK {
					t.Fatalf("fetch = %d", r.status)
				}
				if r := e.gitGet(c.path+"/info/refs?service=git-receive-pack", issued.Secret, http.MethodGet); r.status != http.StatusNotFound {
					t.Fatalf("receive-pack advertisement = %d, want 404", r.status)
				}
				// The policy refuses every ref, whatever the record says.
				for _, ref := range []string{"refs/heads/kodama/runs/" + run + "/work", "refs/heads/kodama/work/tsk_1", "refs/heads/feature/x", "refs/tags/v1", "refs/heads/main"} {
					u := create(ref, c2)
					resp, err := e.authorize(issued.GrantID, c.id, []gitv1.RefUpdate{u}, factsFor(false, u))
					if err != nil || strings.Join(reasons(resp), ",") != gitv1.ReasonRefProtected {
						t.Fatalf("push of %s = %v, %v; want %s", ref, reasons(resp), err, gitv1.ReasonRefProtected)
					}
				}
				// A record forged to name the run's own branches is refused as well.
				forged := api.GitClaims{GrantID: "gg_forged_" + run + c.name, Audience: gitv1.GrantAudience, TenantID: string(tenantID), ProjectID: string(projectID),
					Actor: api.GitActor{Type: "workload", ID: run, RunID: &run}, Kind: store.CredRun,
					Repositories: []api.GitGrantRepoClaim{{ID: c.id, Read: true, Update: []string{"refs/heads/kodama/runs/" + run + "/*", "refs/heads/kodama/work/tsk_1"}}},
					IssuedAt:     time.Now().UTC().Format(time.RFC3339), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
				raw, _ := json.Marshal(forged)
				if err := e.mem.IssueCredential(t.Context(), store.Credential{TenantID: tenantID, ProjectID: projectID, ID: forged.GrantID, RepoID: c.id, Kind: store.CredRun,
					Digest: store.DigestOf("gitc_forged" + run + c.name), Claims: raw, RunID: run, IssuedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
				u := create("refs/heads/kodama/runs/"+run+"/work", c2)
				resp, err := e.authorize(forged.GrantID, c.id, []gitv1.RefUpdate{u}, factsFor(false, u))
				if err != nil || strings.Join(reasons(resp), ",") != gitv1.ReasonRefProtected {
					t.Fatalf("forged record: %v, %v", reasons(resp), err)
				}
			})
		}
	}

	t.Run("the credential ends with the run and is limited to the run's project", func(t *testing.T) {
		issued, err := e.git.IssueRunCredential(t.Context(), tenantID, projectID, "run_1", tenantCtxID)
		if err != nil {
			t.Fatal(err)
		}
		if r := e.gitGet("/git/acme/context.git/info/refs?service=git-upload-pack", issued.Secret, http.MethodGet); r.status != http.StatusOK {
			t.Fatalf("fetch = %d", r.status)
		}
		// Another repository of the project is not readable with it.
		if r := e.gitGet("/git/acme/erp/backend.git/info/refs?service=git-upload-pack", issued.Secret, http.MethodGet); r.status != http.StatusNotFound {
			t.Fatalf("a context credential read the project's source: %d", r.status)
		}
		e.mem.SetRun(store.RunGitInfo{TenantID: tenantID, ProjectID: projectID, RunID: "run_1", PipelineName: "build", Status: "running", Cancelling: true})
		if r := e.gitGet("/git/acme/context.git/info/refs?service=git-upload-pack", issued.Secret, http.MethodGet); r.status != http.StatusNotFound {
			t.Fatalf("fetch by a cancelled run = %d", r.status)
		}
		if _, err := e.git.IssueRunCredential(t.Context(), tenantID, projectID, "run_1", tenantCtxID); err == nil {
			t.Fatal("a cancelled run was issued a context credential")
		}
		_ = e.mem.RevokeRunCredentials(t.Context(), tenantID, projectID, "run_1")
		if r := e.gitGet("/git/acme/context.git/info/refs?service=git-upload-pack", issued.Secret, http.MethodGet); r.status != http.StatusUnauthorized {
			t.Fatalf("fetch with a revoked credential = %d", r.status)
		}
	})

	t.Run("another project's context is not the run's", func(t *testing.T) {
		e.mem.AddProject(tenantID, "p_other", "other")
		e.mem.SetRun(store.RunGitInfo{TenantID: tenantID, ProjectID: "p_other", RunID: "run_x", PipelineName: "build", Status: "running", Sources: []byte(`[]`)})
		if _, err := e.git.IssueRunCredential(t.Context(), tenantID, "p_other", "run_x", projCtxID); err == nil {
			t.Fatal("a run of another project was issued credentials for this project's context")
		}
		if _, err := e.git.IssueRunCredential(t.Context(), tenantID, "p_other", "run_x", tenantCtxID); err != nil {
			t.Fatalf("the tenant's context is every project's: %v", err)
		}
	})
}

// flakyRepositories fails repository reads while down is set, as a database
// outage would.
type flakyRepositories struct {
	store.GitBackend
	down atomic.Bool
}

var errOutage = errors.New("the database is unavailable")

func (f *flakyRepositories) GetRepository(ctx context.Context, tenant domain.TenantID, id string) (store.Repository, error) {
	if f.down.Load() {
		return store.Repository{}, errOutage
	}
	return f.GitBackend.GetRepository(ctx, tenant, id)
}

// Test_unit_kernel_git_run_credential_outage_is_not_revocation: the issuer answers
// not found only for a repository that does not exist. A failed read is returned
// as itself, so a caller that treats not found (or a refusal) as the end of the
// run's authority never takes an outage for one; it recovers when the read does.
func Test_unit_kernel_git_run_credential_outage_is_not_revocation(t *testing.T) {
	e := newEnv(t)
	_, backendID := e.seeded("backend", nil)
	e.mem.SetRun(runInfo("run_1", "build", "tsk_1", backendID))
	flaky := &flakyRepositories{GitBackend: e.mem}
	git, err := api.NewGit(api.GitConfig{Store: flaky, Runs: &runRecords{}, Auth: tokens, Workers: e.workers, Signer: e.signer, Identities: e.verifier, Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := git.IssueRunCredential(t.Context(), tenantID, projectID, "run_1", backendID); err != nil {
		t.Fatalf("issue: %v", err)
	}
	flaky.down.Store(true)
	for name, issue := range map[string]func() error{
		"run credential": func() error {
			_, err := git.IssueRunCredential(t.Context(), tenantID, projectID, "run_1", backendID)
			return err
		},
		"integration credential": func() error {
			_, err := git.IssueIntegrationCredential(t.Context(), tenantID, projectID, "run_1", "integrate", backendID, c1, c2)
			return err
		},
	} {
		err := issue()
		if !errors.Is(err, errOutage) || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
			t.Fatalf("%s during an outage = %v: an outage is neither not found nor a refusal", name, err)
		}
	}
	flaky.down.Store(false)
	if _, err := git.IssueRunCredential(t.Context(), tenantID, projectID, "run_1", backendID); err != nil {
		t.Fatalf("after the outage: %v", err)
	}
	// A repository that does not exist is still not found.
	if _, err := git.IssueRunCredential(t.Context(), tenantID, projectID, "run_1", "repo_nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown repository: %v", err)
	}
}
