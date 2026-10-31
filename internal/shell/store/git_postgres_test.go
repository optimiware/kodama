package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/storage"
	"github.com/optimiware/kodama/test/support/postgres"
)

// gitDB migrates a fresh database (skipped without PostgreSQL) and returns the
// kernel store over the runtime role.
func gitDB(t *testing.T) (*store.Store, *postgres.Database, context.Context) {
	t.Helper()
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
		`INSERT INTO tenants (id,key) VALUES ('t_a','a'),('t_b','b')`,
		`INSERT INTO users (id,issuer,subject) VALUES ('u1','iss','s1')`,
		`INSERT INTO projects (tenant_id,id,key,task_workflow) VALUES ('t_a','p_a1','erp','{}'),('t_a','p_a2','web','{}'),('t_b','p_b1','erp','{}')`,
		`INSERT INTO memberships (tenant_id,id,user_id,role,project_id) VALUES ('t_a','m1','u1','engineer','p_a1')`,
	} {
		if _, err := mig.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return store.New(db.RuntimePool(ctx, 4)), db, ctx
}

func newRepo(t *testing.T, s *store.Store, ctx context.Context, tenant domain.TenantID, project domain.ProjectID, id, name string) store.Repository {
	t.Helper()
	r, created, err := s.CreateRepository(ctx, store.Repository{TenantID: tenant, ProjectID: project, ID: id, Name: name, DefaultBranch: "main"})
	if err != nil || !created {
		t.Fatalf("create %s: created=%v err=%v", name, created, err)
	}
	return r
}

func TestGitPostgresRepositoryLifecycle(t *testing.T) {
	s, _, ctx := gitDB(t)
	r := newRepo(t, s, ctx, "t_a", "p_a1", "repo_1", "backend")
	if r.Status != store.RepoProvisioning || r.ProjectID != "p_a1" || !r.Empty || r.JournalAck != 0 || r.Policy.DirectPushToDefault {
		t.Fatalf("reserved repository = %+v", r)
	}
	// Idempotent on (tenant, project, name): one repository, the same ID.
	again, created, err := s.CreateRepository(ctx, store.Repository{TenantID: "t_a", ProjectID: "p_a1", ID: "repo_x", Name: "backend", DefaultBranch: "main"})
	if err != nil || created || again.ID != "repo_1" {
		t.Fatalf("retry = %+v created=%v err=%v", again, created, err)
	}
	// The same name in another project is another repository.
	if _, created, err := s.CreateRepository(ctx, store.Repository{TenantID: "t_a", ProjectID: "p_a2", ID: "repo_2", Name: "backend", DefaultBranch: "main"}); err != nil || !created {
		t.Fatalf("other project: %v %v", created, err)
	}
	// A project of another tenant, and an unknown one, are not found; the tenant-level repository is context.
	if _, _, err := s.CreateRepository(ctx, store.Repository{TenantID: "t_a", ProjectID: "p_b1", ID: "repo_3", Name: "x", DefaultBranch: "main"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign project: %v", err)
	}
	if _, _, err := s.CreateRepository(ctx, store.Repository{TenantID: "t_a", ID: "repo_4", Name: "notes", DefaultBranch: "main"}); !errors.Is(err, domain.ErrInvalidValue) {
		t.Fatalf("tenant-level name: %v", err)
	}
	tenantLevel := newRepo(t, s, ctx, "t_a", "", "repo_5", "context")
	if tenantLevel.ProjectID != "" {
		t.Fatalf("tenant-level repository has project %q", tenantLevel.ProjectID)
	}
	if _, created, _ := s.CreateRepository(ctx, store.Repository{TenantID: "t_a", ID: "repo_6", Name: "context", DefaultBranch: "main"}); created {
		t.Fatal("a second tenant-level context repository was created")
	}

	// Path resolution and tenant isolation.
	got, err := s.RepositoryByPath(ctx, "t_a", "erp", "backend")
	if err != nil || got.ID != "repo_1" {
		t.Fatalf("by path = %+v, %v", got, err)
	}
	if got, err = s.RepositoryByPath(ctx, "t_a", "", "context"); err != nil || got.ID != "repo_5" {
		t.Fatalf("tenant context by path = %+v, %v", got, err)
	}
	if _, err := s.RepositoryByPath(ctx, "t_b", "erp", "backend"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign tenant path: %v", err)
	}
	if _, err := s.GetRepository(ctx, "t_b", "repo_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign tenant id: %v", err)
	}

	// Confirmation: provisioning to ready with one repository.created, idempotent.
	for i := 0; i < 2; i++ {
		c, err := s.ConfirmInitialized(ctx, "t_a", "repo_1", "main", true)
		if err != nil || c.Status != store.RepoReady || c.Version != 2 {
			t.Fatalf("confirm %d = %+v, %v", i, c, err)
		}
	}
	if _, err := s.ConfirmInitialized(ctx, "t_a", "repo_1", "trunk", true); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("confirm with another default branch: %v", err)
	}
	events := auditEvents(t, s, ctx, "t_a", "repository.created")
	if events != 1 {
		t.Fatalf("repository.created events = %d", events)
	}

	// Patch under If-Match; a stale version is a conflict.
	policy := store.RepositoryPolicy{DirectPushToDefault: true, IntegrationPipeline: "integrate"}
	p, err := s.UpdateRepository(ctx, "t_a", "repo_1", 2, store.RepositoryPatch{Policy: &policy})
	if err != nil || p.Version != 3 || p.Policy != policy {
		t.Fatalf("patch = %+v, %v", p, err)
	}
	if _, err := s.UpdateRepository(ctx, "t_a", "repo_1", 2, store.RepositoryPatch{Policy: &policy}); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("stale patch: %v", err)
	}
	trunk := "trunk"
	if p, err = s.UpdateRepository(ctx, "t_a", "repo_1", 3, store.RepositoryPatch{DefaultBranch: &trunk}); err != nil || p.DefaultBranch != "trunk" ||
		p.Status != store.RepoProvisioning || p.Reason != store.ReasonReconfiguration {
		t.Fatalf("branch change of an empty repository: %+v, %v", p, err)
	}
	if p, err = s.ConfirmInitialized(ctx, "t_a", "repo_1", "trunk", true); err != nil || p.Status != store.RepoReady || p.Version != 5 {
		t.Fatalf("worker confirmation after the change: %+v, %v", p, err)
	}
	if _, err := s.CommitReceive(ctx, store.ReceiveCommit{TenantID: "t_a", RepositoryID: "repo_1", ReceiveID: "rcv_1", ActorType: "user", ActorID: "u1",
		Updates: []store.ReceiveUpdate{{Ref: "refs/heads/trunk", Old: zero, New: c1, Kind: "create"}}}); err != nil {
		t.Fatal(err)
	}
	main := "main"
	if _, err := s.UpdateRepository(ctx, "t_a", "repo_1", 5, store.RepositoryPatch{DefaultBranch: &main}); !errors.Is(err, store.ErrRepositoryNotEmpty) {
		t.Fatalf("branch change of a repository with refs: %v", err)
	}

	// Archive and restore commit repository.state_changed and are repeatable.
	a, err := s.SetRepositoryStatus(ctx, "t_a", "repo_1", store.RepoArchived, "archived", "user", "u1")
	if err != nil || a.Status != store.RepoArchived {
		t.Fatalf("archive = %+v, %v", a, err)
	}
	if again, err := s.SetRepositoryStatus(ctx, "t_a", "repo_1", store.RepoArchived, "archived", "user", "u1"); err != nil || again.Version != a.Version {
		t.Fatalf("repeated archive = %+v, %v", again, err)
	}
	if _, err := s.SetRepositoryStatus(ctx, "t_a", "repo_5", store.RepoArchived, "archived", "user", "u1"); !errors.Is(err, store.ErrRepositoryNotReady) {
		t.Fatalf("archiving a provisioning repository: %v", err)
	}
	if r, err := s.SetRepositoryStatus(ctx, "t_a", "repo_1", store.RepoReady, "", "user", "u1"); err != nil || r.Status != store.RepoReady {
		t.Fatalf("restore = %+v, %v", r, err)
	}
	if n := auditEvents(t, s, ctx, "t_a", "repository.state_changed"); n != 3 { // the reconfiguration, the archive and the restore
		t.Fatalf("repository.state_changed events = %d", n)
	}
	list, err := s.ListRepositories(ctx, "t_a", "p_a1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

const (
	zero = "0000000000000000000000000000000000000000"
	c1   = "1111111111111111111111111111111111111111"
	c2   = "2222222222222222222222222222222222222222"
)

func auditEvents(t *testing.T, s *store.Store, ctx context.Context, tenant domain.TenantID, typ string) int {
	t.Helper()
	var n int
	err := storage.WithTenant(ctx, s.DB, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE tenant_id=$1 AND event_type=$2`, tenant, typ).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func cred(id, repo, project, kind, digest string, ttl time.Duration) store.Credential {
	c := store.Credential{TenantID: "t_a", ProjectID: domain.ProjectID(project), ID: id, RepoID: repo, Kind: kind, Digest: digest,
		Claims: json.RawMessage(`{"kind":"` + kind + `"}`), IssuedAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(ttl)}
	if kind == store.CredHuman || kind == store.CredSeed {
		c.UserID = "u1"
	} else {
		c.RunID = "run_1"
	}
	return c
}

func digest(n string) string { return store.DigestOf("gitc_" + n) }

func TestGitPostgresCredentials(t *testing.T) {
	s, db, ctx := gitDB(t)
	newRepo(t, s, ctx, "t_a", "p_a1", "repo_1", "backend")
	newRepo(t, s, ctx, "t_a", "", "repo_5", "context")

	if err := s.IssueCredential(ctx, cred("gg_1", "repo_1", "p_a1", store.CredHuman, digest("one"), time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := s.CredentialByDigest(ctx, "t_a", digest("one"))
	if err != nil || got.ID != "gg_1" || got.Digest != digest("one") || !got.Live(time.Now()) {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	if _, err := s.CredentialByDigest(ctx, "t_b", digest("one")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a credential resolved in another tenant: %v", err)
	}
	if err := s.IssueCredential(ctx, cred("gg_dup", "repo_1", "p_a1", store.CredHuman, digest("one"), time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate digest: %v", err)
	}
	// The credential's scope must name its repository's project, in both
	// directions (the tenant-level repository has no project).
	if err := s.IssueCredential(ctx, cred("gg_bad1", "repo_1", "p_a2", store.CredHuman, digest("bad1"), time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wrong project: %v", err)
	}
	if err := s.IssueCredential(ctx, cred("gg_bad2", "repo_1", "", store.CredHuman, digest("bad2"), time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing project on a project repository: %v", err)
	}
	if err := s.IssueCredential(ctx, cred("gg_bad3", "repo_5", "p_a1", store.CredHuman, digest("bad3"), time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project on a tenant-level repository: %v", err)
	}
	if err := s.IssueCredential(ctx, cred("gg_ok", "repo_5", "", store.CredHuman, digest("tenant"), time.Minute)); err != nil {
		t.Fatalf("tenant-level credential: %v", err)
	}
	// The secret is not stored anywhere in the row.
	mig := db.MigrationPool(ctx, 1)
	var leaked bool
	if err := mig.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM git_credentials WHERE claims::text LIKE '%gitc_%' OR secret_digest LIKE 'gitc_%')`).Scan(&leaked); err != nil || leaked {
		t.Fatalf("a secret leaked into git_credentials: %v %v", leaked, err)
	}
	// The runtime role cannot rewrite the digest or the claims, or delete a row.
	if _, err := db.RuntimePool(ctx, 1).Exec(ctx, `UPDATE git_credentials SET secret_digest=$1`, digest("x")); err == nil {
		t.Fatal("the runtime role rewrote a credential digest")
	}
	if _, err := db.RuntimePool(ctx, 1).Exec(ctx, `DELETE FROM git_credentials`); err == nil {
		t.Fatal("the runtime role deleted credentials")
	}
	if got.Kind != store.CredHuman {
		t.Fatalf("kind = %s", got.Kind)
	}
}

func TestGitPostgresSeedSingleUse(t *testing.T) {
	s, _, ctx := gitDB(t)
	newRepo(t, s, ctx, "t_a", "p_a1", "repo_1", "backend")
	if err := s.IssueSeed(ctx, cred("gg_s0", "repo_1", "p_a1", store.CredSeed, digest("s0"), time.Minute)); !errors.Is(err, store.ErrRepositoryNotReady) {
		t.Fatalf("seed for a provisioning repository: %v", err)
	}
	if _, err := s.ConfirmInitialized(ctx, "t_a", "repo_1", "main", true); err != nil {
		t.Fatal(err)
	}
	if err := s.IssueSeed(ctx, cred("gg_s1", "repo_1", "p_a1", store.CredSeed, digest("s1"), time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A new request replaces an outstanding unconsumed seed.
	if err := s.IssueSeed(ctx, cred("gg_s2", "repo_1", "p_a1", store.CredSeed, digest("s2"), time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthorizeSeed(ctx, "t_a", "gg_s1", "rcv_a"); !errors.Is(err, store.ErrSeedConsumed) {
		t.Fatalf("a replaced seed authorized a receive: %v", err)
	}
	// Concurrent receives with the one seed authorize exactly one.
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- s.AuthorizeSeed(ctx, "t_a", "gg_s2", "rcv_"+string(rune('a'+i)))
		}(i)
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, store.ErrSeedConsumed):
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d concurrent receives were authorized with one seed", ok)
	}
	// An unrecorded consumed seed blocks a new grant until its outcome is recorded.
	if err := s.IssueSeed(ctx, cred("gg_s3", "repo_1", "p_a1", store.CredSeed, digest("s3"), time.Minute)); !errors.Is(err, store.ErrSeedPending) {
		t.Fatalf("new seed while one is unrecorded: %v", err)
	}
	// A refused seed (outcome with nothing applied) leaves the repository empty and a new grant possible.
	consumed, err := s.GetCredential(ctx, "t_a", "gg_s2")
	if err != nil || consumed.ConsumedAt == nil {
		t.Fatalf("consumed = %+v, %v", consumed, err)
	}
	if _, err := s.CommitReceive(ctx, store.ReceiveCommit{TenantID: "t_a", RepositoryID: "repo_1", ReceiveID: consumed.ConsumedReceive, ActorType: "user", ActorID: "u1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthorizeSeed(ctx, "t_a", "gg_s2", "rcv_other"); !errors.Is(err, store.ErrSeedConsumed) {
		t.Fatalf("a consumed seed authorized a second receive: %v", err)
	}
	if err := s.AuthorizeSeed(ctx, "t_a", "gg_s2", consumed.ConsumedReceive); err != nil {
		t.Fatalf("the same receive repeats idempotently: %v", err)
	}
	if err := s.IssueSeed(ctx, cred("gg_s3", "repo_1", "p_a1", store.CredSeed, digest("s3"), time.Minute)); err != nil {
		t.Fatalf("seed after a settled refused seed: %v", err)
	}
	// An applied seed makes the repository non-empty: no more seeds.
	if err := s.AuthorizeSeed(ctx, "t_a", "gg_s3", "rcv_seed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitReceive(ctx, store.ReceiveCommit{TenantID: "t_a", RepositoryID: "repo_1", ReceiveID: "rcv_seed", ActorType: "user", ActorID: "u1",
		Updates: []store.ReceiveUpdate{{Ref: "refs/heads/main", Old: zero, New: c1, Kind: "create"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.IssueSeed(ctx, cred("gg_s4", "repo_1", "p_a1", store.CredSeed, digest("s4"), time.Minute)); !errors.Is(err, store.ErrRepositoryNotEmpty) {
		t.Fatalf("seed for a repository with refs: %v", err)
	}
}

func TestGitPostgresReceiveCommittedOnce(t *testing.T) {
	s, _, ctx := gitDB(t)
	newRepo(t, s, ctx, "t_a", "p_a1", "repo_1", "backend")
	if _, err := s.ConfirmInitialized(ctx, "t_a", "repo_1", "main", true); err != nil {
		t.Fatal(err)
	}
	commit := store.ReceiveCommit{TenantID: "t_a", RepositoryID: "repo_1", ReceiveID: "rcv_1", ActorType: "workload", ActorID: "run_1", OriginRunID: "run_1",
		Updates: []store.ReceiveUpdate{{Ref: "refs/heads/kodama/runs/run_1/x", Old: zero, New: c1, Kind: "create"}}, JournalSequence: 3}
	var wg sync.WaitGroup
	created := make(chan bool, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.CommitReceive(ctx, commit)
			if err != nil {
				t.Error(err)
				return
			}
			created <- res.Created
		}()
	}
	wg.Wait()
	close(created)
	n := 0
	for c := range created {
		if c {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d concurrent reports created the event", n)
	}
	if got := auditEvents(t, s, ctx, "t_a", "git.refs_updated"); got != 1 {
		t.Fatalf("git.refs_updated rows = %d", got)
	}
	// The acknowledged sequence never moves down, and a lower report keeps the row.
	commit.JournalSequence = 1
	res, err := s.CommitReceive(ctx, commit)
	if err != nil || res.Created || res.JournalAck != 3 {
		t.Fatalf("re-report = %+v, %v", res, err)
	}
	repo, _ := s.GetRepository(ctx, "t_a", "repo_1")
	if repo.JournalAck != 3 || repo.Empty || repo.Version != 2 {
		t.Fatalf("repository = %+v (journal_ack, emptiness; version unchanged)", repo)
	}
	var payload []byte
	err = storage.WithTenant(ctx, s.DB, "t_a", func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT payload::jsonb FROM audit_events WHERE tenant_id='t_a' AND id=$1`, store.GitReceiveEventID("repo_1", "rcv_1")).Scan(&payload)
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		OriginRunID string `json:"originRunId"`
		Updates     []struct{ Ref, Kind string }
	}
	if err := json.Unmarshal(payload, &got); err != nil || got.OriginRunID != "run_1" || len(got.Updates) != 1 || got.Updates[0].Kind != "create" {
		t.Fatalf("payload = %s, %v", payload, err)
	}
	// An outcome for an unknown repository is not found.
	commit.RepositoryID = "repo_nope"
	if _, err := s.CommitReceive(ctx, commit); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown repository: %v", err)
	}
}

func TestGitPostgresRunCredentialsEndWithTheRun(t *testing.T) {
	s, db, ctx := gitDB(t)
	newRepo(t, s, ctx, "t_a", "p_a1", "repo_1", "backend")
	mig := db.MigrationPool(ctx, 1)
	for _, q := range []string{
		`INSERT INTO pipelines (tenant_id,project_id,id,name,definition,source) VALUES ('t_a','p_a1','pl_1','integrate','{}','api')`,
		`INSERT INTO runs (tenant_id,project_id,id,pipeline_id,definition_snapshot,sources)
		 VALUES ('t_a','p_a1','run_1','pl_1','{}','[{"repositoryId":"repo_1","commit":"` + c1 + `","path":".","access":"write","branch":"refs/heads/kodama/work/tsk_1"}]')`,
	} {
		if _, err := mig.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	info, err := s.RunGitInfo(ctx, "t_a", "p_a1", "run_1")
	if err != nil || !info.Active() || info.PipelineName != "integrate" || info.Status != "queued" {
		t.Fatalf("run = %+v, %v", info, err)
	}
	if _, err := s.RunGitInfo(ctx, "t_b", "p_b1", "run_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a run of another tenant: %v", err)
	}
	if err := s.IssueCredential(ctx, cred("gg_r1", "repo_1", "p_a1", store.CredRun, digest("run"), time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, "t_a", "p_a1", "run_1", "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetCredential(ctx, "t_a", "gg_r1")
	if err != nil || got.RevokedAt == nil || got.Live(time.Now()) {
		t.Fatalf("a run's credential outlived the run: %+v, %v", got, err)
	}
	if info, _ = s.RunGitInfo(ctx, "t_a", "p_a1", "run_1"); info.Active() {
		t.Fatal("a finished run is active")
	}
}

// TestGitPostgresRunEvidenceIgnoresTasks: the Git evidence the store reads of a
// run is its pipeline name, its state and its sources, scoped by tenant and
// project. A task row, however it was resolved, changes none of it and gives the
// store nothing to derive integration authority from (that is decided from the
// run's durable record by internal/shell/git).
func TestGitPostgresRunEvidenceIgnoresTasks(t *testing.T) {
	s, db, ctx := gitDB(t)
	newRepo(t, s, ctx, "t_a", "p_a1", "repo_1", "backend")
	mig := db.MigrationPool(ctx, 1)
	for _, q := range []string{
		`INSERT INTO pipelines (tenant_id,project_id,id,name,definition,source) VALUES ('t_a','p_a1','pl_1','integrate','{}','api')`,
		`INSERT INTO runs (tenant_id,project_id,id,pipeline_id,definition_snapshot,sources)
		 VALUES ('t_a','p_a1','run_1','pl_1','{}','[{"repositoryId":"repo_1","commit":"` + c1 + `","path":".","access":"write","branch":"refs/heads/kodama/work/tsk_1"}]')`,
	} {
		if _, err := mig.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	before, err := s.RunGitInfo(ctx, "t_a", "p_a1", "run_1")
	if err != nil || !before.Active() || before.PipelineName != "integrate" {
		t.Fatalf("run = %+v, %v", before, err)
	}
	// An approved promotion request resolved behind the run's task, and an
	// approved decision: neither is evidence of anything about the run.
	for _, q := range []string{
		`INSERT INTO tasks (tenant_id,project_id,id,kind,title,state) VALUES ('t_a','p_a1','tsk_1','task','t','review')`,
		`INSERT INTO tasks (tenant_id,project_id,id,parent_id,kind,title,state,assigned_role,resolution,resolved_at)
		 VALUES ('t_a','p_a1','tsk_p1','tsk_1','promotion','t','done','reviewer',
		 '{"outcome":"approved","actorId":"u1","role":"reviewer","answer":"ok"}'::jsonb,now())`,
	} {
		if _, err := mig.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	after, err := s.RunGitInfo(ctx, "t_a", "p_a1", "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if string(after.Sources) != string(before.Sources) {
		t.Fatalf("task rows changed the run's evidence: %+v vs %+v", after, before)
	}
	after.Sources, before.Sources = nil, nil
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("task rows changed the run's evidence: %+v vs %+v", after, before)
	}
	// The evidence has no approval field to read.
	typ := reflect.TypeOf(after)
	for i := 0; i < typ.NumField(); i++ {
		if strings.Contains(strings.ToLower(typ.Field(i).Name), "approv") {
			t.Fatalf("RunGitInfo carries %s", typ.Field(i).Name)
		}
	}
	// Another tenant or project sees no run.
	for _, c := range []struct{ tenant, project string }{{"t_b", "p_b1"}, {"t_a", "p_a2"}} {
		if _, err := s.RunGitInfo(ctx, domain.TenantID(c.tenant), domain.ProjectID(c.project), "run_1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("run seen from %v: %v", c, err)
		}
	}
}

func TestGitPostgresReconfigurationAndAssignmentPages(t *testing.T) {
	s, _, ctx := gitDB(t)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		newRepo(t, s, ctx, "t_a", "p_a1", "repo_"+n, n)
	}
	prov, err := s.ProvisioningRepositories(ctx, "t_a", 10)
	if err != nil || len(prov) != 5 {
		t.Fatalf("provisioning = %d, %v", len(prov), err)
	}
	if rec, _ := s.ReconcilableRepositories(ctx, "t_a", "", 10); len(rec) != 0 {
		t.Fatal("a provisioning repository is reconcilable")
	}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if _, err := s.ConfirmInitialized(ctx, "t_a", "repo_"+n, "main", true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetRepositoryStatus(ctx, "t_a", "repo_b", store.RepoArchived, "archived", "user", "u1"); err != nil {
		t.Fatal(err)
	}
	// Pages in ID order, archived repositories included, each repository exactly once.
	var seen []string
	after := ""
	for {
		page, err := s.ReconcilableRepositories(ctx, "t_a", after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			seen = append(seen, r.ID)
		}
		after = page[len(page)-1].ID
	}
	if len(seen) != 5 || seen[0] != "repo_a" || seen[1] != "repo_b" || seen[4] != "repo_e" {
		t.Fatalf("pages = %v", seen)
	}

	// A default branch change reconfigures: provisioning with its reason, the seed grant
	// of the old branch revoked, then ready again with a state change and no second creation.
	if err := s.IssueSeed(ctx, cred("gg_old", "repo_c", "p_a1", store.CredSeed, digest("old"), time.Minute)); err != nil {
		t.Fatal(err)
	}
	trunk := "trunk"
	got, err := s.UpdateRepository(ctx, "t_a", "repo_c", 2, store.RepositoryPatch{DefaultBranch: &trunk})
	if err != nil || got.Status != store.RepoProvisioning || got.Reason != store.ReasonReconfiguration || got.DefaultBranch != "trunk" || got.Version != 3 {
		t.Fatalf("reconfigured = %+v, %v", got, err)
	}
	if old, _ := s.GetCredential(ctx, "t_a", "gg_old"); old.RevokedAt == nil {
		t.Fatal("the seed grant of the old default branch survived")
	}
	if prov, _ := s.ProvisioningRepositories(ctx, "t_a", 10); len(prov) != 1 || prov[0].DefaultBranch != "trunk" {
		t.Fatalf("provisioning after the change = %+v", prov)
	}
	if _, err := s.UpdateRepository(ctx, "t_a", "repo_c", 3, store.RepositoryPatch{DefaultBranch: &trunk}); err != nil {
		t.Fatalf("repeating the same branch is a policy-free patch: %v", err)
	}
	created := auditEvents(t, s, ctx, "t_a", "repository.created")
	back, err := s.ConfirmInitialized(ctx, "t_a", "repo_c", "trunk", true)
	if err != nil || back.Status != store.RepoReady || back.Reason != "" {
		t.Fatalf("confirmed = %+v, %v", back, err)
	}
	if auditEvents(t, s, ctx, "t_a", "repository.created") != created || auditEvents(t, s, ctx, "t_a", "repository.state_changed") != 2 {
		t.Fatal("reconfiguration committed the wrong events")
	}
	// Refused: archived, provisioning, with an unacknowledged seed, with refs.
	if _, err := s.UpdateRepository(ctx, "t_a", "repo_b", 3, store.RepositoryPatch{DefaultBranch: &trunk}); !errors.Is(err, store.ErrRepositoryArchived) {
		t.Fatalf("archived: %v", err)
	}
	if err := s.IssueSeed(ctx, cred("gg_new", "repo_d", "p_a1", store.CredSeed, digest("new"), time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthorizeSeed(ctx, "t_a", "gg_new", "rcv_x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRepository(ctx, "t_a", "repo_d", 2, store.RepositoryPatch{DefaultBranch: &trunk}); !errors.Is(err, store.ErrSeedPending) {
		t.Fatalf("unacknowledged seed: %v", err)
	}
	if _, err := s.CommitReceive(ctx, store.ReceiveCommit{TenantID: "t_a", RepositoryID: "repo_d", ReceiveID: "rcv_x", ActorType: "user", ActorID: "u1",
		Updates: []store.ReceiveUpdate{{Ref: "refs/heads/main", Old: zero, New: c1, Kind: "create"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRepository(ctx, "t_a", "repo_d", 2, store.RepositoryPatch{DefaultBranch: &trunk}); !errors.Is(err, store.ErrRepositoryNotEmpty) {
		t.Fatalf("with refs: %v", err)
	}
}

// TestGitPostgresGetRunLoadsPinnedSources: the run carries its pinned sources in
// the shape of gitv1.SourceSet.Sources and the keys addressing its repositories;
// a run without a repository has none, and a run of another tenant is not found.
func TestGitPostgresGetRunLoadsPinnedSources(t *testing.T) {
	s, db, ctx := gitDB(t)
	mig := db.MigrationPool(ctx, 1)
	for _, q := range []string{
		`INSERT INTO pipelines (tenant_id,project_id,id,name,definition,source) VALUES ('t_a','p_a1','pl_1','build','{}','api')`,
		`INSERT INTO runs (tenant_id,project_id,id,pipeline_id,definition_snapshot,sources)
		 VALUES ('t_a','p_a1','run_src','pl_1','{}','[{"repositoryId":"repo_1","commit":"` + c1 + `","path":"backend","access":"write","branch":"refs/heads/kodama/work/tsk_1"},
		   {"repositoryId":"repo_2","commit":"` + c1 + `","path":"web","access":"read","branch":null}]')`,
		`INSERT INTO runs (tenant_id,project_id,id,pipeline_id,definition_snapshot) VALUES ('t_a','p_a1','run_none','pl_1','{}')`,
		`INSERT INTO runs (tenant_id,project_id,id,pipeline_id,definition_snapshot,sources)
		 VALUES ('t_a','p_a1','run_bad','pl_1','{}','[{"repositoryId":"repo_1","commit":"nope","path":".","access":"read","branch":null}]')`,
	} {
		if _, err := mig.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	run, err := s.GetRun(ctx, "t_a", "p_a1", "run_src")
	if err != nil {
		t.Fatal(err)
	}
	branch := "refs/heads/kodama/work/tsk_1"
	want := []gitv1.Source{
		{RepositoryID: "repo_1", Commit: c1, Path: "backend", Access: gitv1.AccessWrite, Branch: &branch},
		{RepositoryID: "repo_2", Commit: c1, Path: "web", Access: gitv1.AccessRead},
	}
	if len(run.Sources) != 2 || run.Sources[0].RepositoryID != "repo_1" || run.Sources[0].Branch == nil || *run.Sources[0].Branch != branch ||
		run.Sources[1] != want[1] || run.TenantKey != "a" || run.ProjectKey != "erp" {
		t.Fatalf("run = %+v", run)
	}
	none, err := s.GetRun(ctx, "t_a", "p_a1", "run_none")
	if err != nil || len(none.Sources) != 0 {
		t.Fatalf("run without sources = %+v, %v", none, err)
	}
	// A pinned set that is not valid is an error, never a partial run.
	if _, err := s.GetRun(ctx, "t_a", "p_a1", "run_bad"); err == nil {
		t.Fatal("a run with an invalid pinned set was loaded")
	}
	if _, err := s.GetRun(ctx, "t_b", "p_b1", "run_src"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a run of another tenant: %v", err)
	}
}
