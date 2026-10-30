package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const sourceSQLDoc = "[{repository: backend, path: backend, access: write}, {repository: frontend, path: frontend, access: read}]"

// Test_source_run_sql_guards: what only the database can prove about origins and
// source sets (PX-23, PX-A19): forced row level security, the composite foreign
// key to the project's own repositories, the write-once pins, the whole-set path
// rule, the shapes of runs and attempts without a work item or workspace, and one
// unsettled attempt per run.
func Test_source_run_sql_guards(t *testing.T) {
	ctx := context.Background()
	e, s := executiontest.NewSourcePG(t)
	backend := s.Repos.AddRepository(executiontest.ScopeA, "backend")
	s.Repos.AddRepository(executiontest.ScopeA, "frontend")
	def := s.DefineSource(executiontest.SourceDoc(sourceSQLDoc, executiontest.SharedStep("build", "workspace: {access: write}")))
	run, err := s.Svc.AdmitSourceRun(ctx, execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_sql"}, execution.SourceRunCommand{
		Scope: executiontest.ScopeA, Origin: execution.Origin{Kind: execution.OriginManual}, DefinitionID: def.ID})
	if err != nil {
		t.Fatal(err)
	}
	owner := func(sql string, args ...any) error {
		_, err := e.Migration.Exec(ctx, sql, args...)
		return err
	}
	refuses := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: the database accepted it", name)
		}
	}
	tenant, project, rid := string(executiontest.ScopeA.TenantID), string(executiontest.ScopeA.ProjectID), string(run.ID)

	t.Run("the pins are persisted and read back whole", func(t *testing.T) {
		got, err := e.Store.Get(ctx, executiontest.ScopeA, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Run.Sources == nil || len(got.Run.Sources.Entries) != 2 || got.Run.Sources.Digest != run.Sources.Digest || got.Run.Origin.Kind != execution.OriginManual {
			t.Fatalf("%+v", got.Run.Sources)
		}
		list, err := e.Store.List(ctx, executiontest.ScopeA, execution.ListQuery{Limit: 10})
		if err != nil || len(list) != 1 || list[0].Sources == nil || len(list[0].Sources.Entries) != 2 {
			t.Fatalf("a listed run lost its source set: %+v %v", list, err)
		}
	})

	t.Run("forced row level security", func(t *testing.T) {
		for _, table := range []string{"run_sources", "attempt_commits"} {
			var foreign, none int
			if err := e.RuntimeTx(executiontest.ScopeB.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&foreign)
			}); err != nil {
				t.Fatal(err)
			}
			if err := e.Runtime.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&none); err != nil {
				t.Fatal(err)
			}
			if foreign != 0 || none != 0 {
				t.Errorf("%s: tenant B sees %d rows, no context sees %d", table, foreign, none)
			}
		}
		if e.Count(`SELECT count(*) FROM run_sources WHERE run_id = $1`, rid) != 2 {
			t.Fatal("the owner sees the entries")
		}
		got, err := e.Store.Get(ctx, executiontest.ScopeB, run.ID)
		if !errors.Is(err, execution.ErrNotFound) || got != nil {
			t.Fatalf("a foreign tenant resolves the run: %v", err)
		}
		if rs, err := e.Store.List(ctx, executiontest.ScopeB, execution.ListQuery{Limit: 10}); err != nil || len(rs) != 0 {
			t.Fatalf("a foreign tenant lists %d runs", len(rs))
		}
	})

	t.Run("a source can only name a repository of the run's own project", func(t *testing.T) {
		e.OwnerExec(`INSERT INTO repositories (tenant_id, project_id, id, name, default_branch, hosting, status, created_by_type, created_by_id, created_at, updated_at)
			VALUES ($1,$2,'repo_other_project','elsewhere','main','internal','ready','user','u',now(),now())`, tenant, string(executiontest.ScopeA2.ProjectID))
		def2 := s.DefineSource(executiontest.SourceDoc("", executiontest.SharedStep("only", "workspace: {access: read}")))
		manual, err := s.Svc.AdmitSourceRun(ctx, execution.Caller{PrincipalID: executiontest.Engineer}, execution.SourceRunCommand{
			Scope: executiontest.ScopeA, Origin: execution.Origin{Kind: execution.OriginManual}, DefinitionID: def2.ID, Repository: "backend"})
		if err != nil {
			t.Fatal(err)
		}
		refuses("a foreign repository as a source", owner(`INSERT INTO run_sources (tenant_id, project_id, run_id, ordinal, path, repository_id, repository_name, repository_url, commit, access)
			VALUES ($1,$2,$3,5,'other','repo_other_project','elsewhere','https://git.example.invalid/git/x/y/z.git',$4,'read')`, tenant, project, string(manual.ID), executiontest.CommitA))
		refuses("a foreign repository as the push origin", owner(`UPDATE runs SET origin_repository_id = 'repo_other_project' WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, tenant, project, rid))
	})

	t.Run("the origin and the source set are written once", func(t *testing.T) {
		refuses("another origin", owner(`UPDATE runs SET origin_kind = 'provision' WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, tenant, project, rid))
		refuses("another digest", owner(`UPDATE runs SET source_set_digest = $4 WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, tenant, project, rid, "sha256:"+strings.Repeat("c", 64)))
		refuses("another endpoint", owner(`UPDATE runs SET source_base_url = 'https://other.invalid' WHERE tenant_id=$1 AND project_id=$2 AND id=$3`, tenant, project, rid))
		refuses("another commit", owner(`UPDATE run_sources SET commit = $3 WHERE tenant_id=$1 AND run_id=$2`, tenant, rid, executiontest.CommitE))
		refuses("a deleted source", owner(`DELETE FROM run_sources WHERE tenant_id=$1 AND run_id=$2`, tenant, rid))
		// The retention confirmation, the one change a pending run takes, is allowed.
	})

	t.Run("the set is valid as a whole", func(t *testing.T) {
		for name, path := range map[string]string{"a duplicate": "backend", "a nested path": "backend/sub", "a parent": ".", "a hidden component": ".git", "a parent reference": "a/../b"} {
			refuses(name, owner(`INSERT INTO run_sources (tenant_id, project_id, run_id, ordinal, path, repository_id, repository_name, repository_url, commit, access)
				VALUES ($1,$2,$3,9,$4,$5,'backend','https://git.example.invalid/git/x/y/z.git',$6,'read')`, tenant, project, rid, path, backend.ID, executiontest.CommitA))
		}
		refuses("a source of a run without a set", owner(`INSERT INTO run_sources (tenant_id, project_id, run_id, ordinal, path, repository_id, repository_name, repository_url, commit, access)
			VALUES ($1,$2,$3,0,'x',$4,'backend','https://git.example.invalid/git/x/y/z.git',$5,'read')`, tenant, project, "run_without_set", backend.ID, executiontest.CommitA))
		refuses("a write source without its branch", owner(`UPDATE run_sources SET branch = NULL WHERE tenant_id=$1 AND run_id=$2 AND access='write'`, tenant, rid))
	})

	t.Run("the shapes of a run without a work item", func(t *testing.T) {
		// A work run without an origin still needs its work item and workspace.
		refuses("a legacy work run without a work item", owner(`INSERT INTO runs (tenant_id, project_id, id, purpose, definition_id, definition_digest, inputs, snapshot_digest, source_retention,
			requested_by_type, requested_by_id, status, status_reason, version, created_at, updated_at)
			SELECT tenant_id, project_id, 'run_shape_1', 'work', definition_id, definition_digest, inputs, snapshot_digest, 'not_required', requested_by_type, requested_by_id,
			'queued', 'created', 1, now(), now() FROM runs WHERE id = $1`, rid))
		// An origin run cannot carry a work item's members for another kind.
		refuses("a manual origin with a work item", owner(`INSERT INTO runs (tenant_id, project_id, id, purpose, work_item_id, definition_id, definition_digest, inputs, snapshot_digest, source_retention,
			requested_by_type, requested_by_id, status, status_reason, version, created_at, updated_at, origin_kind, source_base_url, source_declaration, run_shared)
			SELECT tenant_id, project_id, 'run_shape_2', 'work', 'wi_x', definition_id, definition_digest, inputs, snapshot_digest, 'not_required', requested_by_type, requested_by_id,
			'queued', 'created', 1, now(), now(), 'manual', 'https://git.example.invalid/git', $2, run_shared FROM runs WHERE id = $1`, rid, "sha256:"+strings.Repeat("d", 64)))
		// An origin run needs the run_shared pins (context version 3 is run_shared).
		refuses("an origin without run_shared", owner(`INSERT INTO runs (tenant_id, project_id, id, purpose, definition_id, definition_digest, inputs, snapshot_digest, source_retention,
			requested_by_type, requested_by_id, status, status_reason, version, created_at, updated_at, origin_kind, source_base_url, source_declaration)
			SELECT tenant_id, project_id, 'run_shape_3', 'work', definition_id, definition_digest, inputs, snapshot_digest, 'not_required', requested_by_type, requested_by_id,
			'queued', 'created', 1, now(), now(), 'manual', 'https://git.example.invalid/git', $2 FROM runs WHERE id = $1`, rid, "sha256:"+strings.Repeat("d", 64)))
		// A push origin names its record, repository and a ref outside refs/kodama/.
		refuses("a push origin without its record", owner(`INSERT INTO runs (tenant_id, project_id, id, purpose, definition_id, definition_digest, inputs, snapshot_digest, source_retention,
			requested_by_type, requested_by_id, status, status_reason, version, created_at, updated_at, origin_kind, source_base_url, source_declaration, run_shared)
			SELECT tenant_id, project_id, 'run_shape_4', 'work', definition_id, definition_digest, inputs, snapshot_digest, 'not_required', requested_by_type, requested_by_id,
			'queued', 'created', 1, now(), now(), 'push', 'https://git.example.invalid/git', $2, run_shared FROM runs WHERE id = $1`, rid, "sha256:"+strings.Repeat("d", 64)))
		refuses("a server ref as the pushed ref", owner(`UPDATE runs SET origin_ref = 'refs/kodama/runs/x/1' WHERE id = $1`, rid))
	})

	t.Run("one unsettled attempt per shared run", func(t *testing.T) {
		r := executiontest.NewSourceRunSteps(t, s, executiontest.SharedStep("one", "workspace: {access: read}"), executiontest.SharedStep("two", "workspace: {access: read}"))
		first := r.Claim()
		// A second attempt of ANOTHER step (so the per-step guard is not the one that
		// fires) while the first is unsettled.
		err := owner(`INSERT INTO step_attempts (tenant_id, project_id, id, run_id, step_id, ordinal, action_key, status, reason, job_name,
			spec_fingerprint, access, timeout_seconds, startup_deadline_at, context_digest, context, authority_revision, version, created_at, updated_at)
			SELECT tenant_id, project_id, 'att_second', run_id, 'two', 1, action_key, 'running', 'starting', 'kdm-second', spec_fingerprint, 'none', 60, now(), context_digest, context, 1, 1, now(), now()
			FROM step_attempts WHERE id = $1`, string(first.AttemptID))
		if err == nil || !strings.Contains(err.Error(), "run_shared run has an unsettled attempt") {
			t.Errorf("the run-level guard did not fire: %v", err)
		}
	})

	t.Run("an attempt of a run without a workspace binds none", func(t *testing.T) {
		r := executiontest.NewSourceRun(t, s, "write")
		in := r.Claim()
		if in.WorkspaceID != "" || in.Access != execution.AccessWrite {
			t.Fatalf("%+v", in)
		}
		refuses("an attempt with an input commit and no workspace", owner(`UPDATE step_attempts SET input_commit = $2 WHERE id = $1`, string(in.AttemptID), executiontest.CommitA))
	})

	// insertSecond inserts a second attempt (ordinal 2, after the first settled) of
	// the run of the attempt, copying its row: the shape guards are the only rules
	// left to fire.
	t.Run("a run without an origin keeps its workspace shape and its receipt cap", func(t *testing.T) {
		// A fresh environment: the guards, not the capacity the earlier runs hold, decide.
		e, s := executiontest.NewSourcePG(t)
		owner := func(sql string, args ...any) error {
			_, err := e.Migration.Exec(ctx, sql, args...)
			return err
		}
		tenant := string(executiontest.ScopeA.TenantID)
		insertSecond := func(first domain.AttemptID, id, access string) error {
			return owner(`INSERT INTO step_attempts (tenant_id, project_id, id, run_id, step_id, ordinal, action_key, status, reason, job_name,
				spec_fingerprint, access, timeout_seconds, startup_deadline_at, context_digest, context, authority_revision, version, created_at, updated_at)
				SELECT tenant_id, project_id, $2::text, run_id, step_id, 2, action_key, 'running', 'starting', 'kdm-'||md5($2::text), spec_fingerprint, $3::text, 60, now(), context_digest, context, 1, 1, now(), now()
				FROM step_attempts WHERE id = $1`, string(first), id, access)
		}
		// An origin run's attempt without a workspace is valid, once the first settled.
		r := executiontest.NewSourceRun(t, s, "write")
		first := r.Claim()
		r.Succeed(first, gitv1.RunBranchPrefix+string(r.Run.ID)+"/work", executiontest.CommitD)
		if err := insertSecond(first.AttemptID, "att_origin_two", "read"); err != nil {
			t.Fatalf("an origin run's attempt without a workspace: %v", err)
		}

		// An earlier run (no origin) still needs its workspace for any access but none.
		legacyDef := e.Define(executiontest.ScopeA, pipelinetest.Doc(pipelinetest.Container("a", "workspace: {access: read, from: run}")))
		e.MustAdmit(s.Project, legacyDef, nil)
		in := e.MustClaim(executiontest.ScopeA.TenantID)
		e.Start(in)
		if _, err := e.Succeed(in, nil, ""); err != nil {
			t.Fatal(err)
		}
		err := insertSecond(in.AttemptID, "att_legacy_two", "read")
		if err == nil || !strings.Contains(err.Error(), "without an origin binds a workspace") {
			t.Errorf("an attempt of an earlier run without a workspace: %v", err)
		}

		// Receipts: 4096 bytes for a run without an origin.
		s.Resources.AutoReady = true
		shared := executiontest.NewSharedRun(t, s.SharedSubject, executiontest.SharedSpecDoc(true, executiontest.SharedStep("job")))
		shared.Start()
		for i := 0; i < 4; i++ {
			shared.Pass()
		}
		big := `{"pad": "` + strings.Repeat("x", 5000) + `"}`
		err = owner(`UPDATE run_resources SET receipt = $3::jsonb, receipt_digest = $4, receipt_at = now()
			WHERE tenant_id = $1 AND run_id = $2 AND key = 'preparation'`, tenant, string(shared.Run().ID), big, "sha256:"+strings.Repeat("e", 64))
		if err == nil || !strings.Contains(err.Error(), "at most 4096 bytes") {
			t.Errorf("a 5000-byte receipt of a run without an origin: %v", err)
		}
	})

	t.Run("a refused admission rolls back with the run: no run, no sources", func(t *testing.T) {
		before := e.Count(`SELECT count(*) FROM run_sources`)
		runs := e.Count(`SELECT count(*) FROM runs`)
		s.FailEvent("run.state_changed", errors.New("outbox down"))
		defer s.FailEvent("", nil)
		def3 := s.DefineSource(executiontest.SourceDoc(sourceSQLDoc, executiontest.SharedStep("build", "workspace: {access: write}")))
		_, err := s.Svc.AdmitSourceRun(ctx, execution.Caller{PrincipalID: executiontest.Engineer}, execution.SourceRunCommand{
			Scope: executiontest.ScopeA, Origin: execution.Origin{Kind: execution.OriginManual}, DefinitionID: def3.ID})
		if err == nil {
			t.Fatal("the admission survived a refused outbox append")
		}
		if e.Count(`SELECT count(*) FROM run_sources`) != before || e.Count(`SELECT count(*) FROM runs`) != runs {
			t.Fatal("a refused outbox append left a run or its sources behind")
		}
	})

	t.Run("a store that admits source runs must be able to revoke their Git grants", func(t *testing.T) {
		peers := execpg.Peers{Authority: adapters.BindAuthority, Workspaces: e.Workspaces, Secrets: e.Secrets}
		if _, err := execpg.NewStore(e.Runtime, e.Events, peers, execpg.Options{Clock: e.Clock.Now, RunSources: true}); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("run sources without a revoker: %v", err)
		}
		if _, err := execpg.NewRuntimeStore(e.Runtime, e.Events, peers, adapters.RevokeAttemptGrants, execpg.Options{Clock: e.Clock.Now, RunSources: true}); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("a runtime store with run sources and no Git revoker: %v", err)
		}
		peers.GitGrants = executiontest.TableGitRevoker
		if st, err := execpg.NewStore(e.Runtime, e.Events, peers, execpg.Options{Clock: e.Clock.Now, RunSources: true}); err != nil || !st.RevokesGitGrants() {
			t.Fatalf("with the revoker: %v", err)
		}
		// A Git grant issuer over a store that cannot revoke is refused.
		legacy, err := execpg.NewStore(e.Runtime, e.Events, execpg.Peers{Authority: adapters.BindAuthority, Workspaces: e.Workspaces, Secrets: e.Secrets}, execpg.Options{Clock: e.Clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.BuildService(executiontest.Options{Store: legacy, SourceSets: s.Repos, GitGrants: s.Git,
			Shared: &execution.SharedConfig{Resources: s.Resources, Policies: s.Policies}}); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("an issuer over a store that cannot revoke: %v", err)
		}
	})
	t.Run("a store whose composition did not apply the migration refuses an origin run", func(t *testing.T) {
		legacy, err := execpg.NewStore(e.Runtime, e.Events, execpg.Peers{Authority: adapters.BindAuthority, Workspaces: e.Workspaces, Secrets: e.Secrets}, execpg.Options{Clock: e.Clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		def4 := s.DefineSource(executiontest.SourceDoc(sourceSQLDoc, executiontest.SharedStep("build", "workspace: {access: write}")))
		plan, err := s.Svc.PrepareSourceRun(ctx, execution.Caller{PrincipalID: executiontest.Engineer}, execution.SourceRunCommand{
			Scope: executiontest.ScopeA, Origin: execution.Origin{Kind: execution.OriginManual}, DefinitionID: def4.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := legacy.Admit(ctx, plan); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("an origin run without the migration: %v", err)
		}
		// It still reads every run it knows: the earlier schema is what it uses.
		if _, err := legacy.Get(ctx, executiontest.ScopeA, run.ID); err != nil {
			t.Fatalf("a store without the migration cannot read the run: %v", err)
		}
	})

	t.Run("the commits an attempt reported are append-only and tenant scoped", func(t *testing.T) {
		r := executiontest.NewSourceRun(t, s, "write")
		in := r.Claim()
		r.Succeed(in, gitv1.RunBranchPrefix+string(r.Run.ID)+"/work", executiontest.CommitD)
		if e.Count(`SELECT count(*) FROM attempt_commits WHERE attempt_id = $1`, string(in.AttemptID)) != 1 {
			t.Fatal("the reported commit was not recorded")
		}
		refuses("rewrite a reported commit", owner(`UPDATE attempt_commits SET commit = $2 WHERE attempt_id = $1`, string(in.AttemptID), executiontest.CommitE))
		refuses("delete a reported commit", owner(`DELETE FROM attempt_commits WHERE attempt_id = $1`, string(in.AttemptID)))
		agg, err := e.Store.Get(ctx, executiontest.ScopeA, r.Run.ID)
		if err != nil || len(agg.Attempt(in.AttemptID).Commits) != 1 || agg.Attempt(in.AttemptID).Commits[0].Commit != executiontest.CommitD {
			t.Fatalf("the attempt does not read its commits back: %v", err)
		}
	})
}
