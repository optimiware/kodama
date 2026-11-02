package deliverycleanup_test

import (
	"context"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// Test_unit_cleanup_preserves_git_storage (GR-13, GR-14, DQ-17): deployment cleanup
// deletes the deployment's external objects and nothing in Git. Repositories,
// branches and the commits a deployment's source set pins are retention's, so
// the scheduler has no path to them: it imports no Git, repository or workspace
// module, its three ports expose no Git operation, and the cleanup of a
// deployment of a source set leaves the record's source set exactly as it was.
func Test_unit_cleanup_preserves_git_storage(t *testing.T) {
	t.Run("the module reaches no Git storage", func(t *testing.T) {
		forbidden := []string{
			"github.com/optimiware/kodama/internal/repository", "github.com/optimiware/kodama/internal/gitservice", "github.com/optimiware/kodama/internal/gitops",
			"github.com/optimiware/kodama/internal/gitpush", "github.com/optimiware/kodama/internal/gitauth", "github.com/optimiware/kodama/internal/workspace",
			"github.com/optimiware/kodama/internal/kube", "github.com/optimiware/kodama/contracts/git", "os/exec",
		}
		files, err := filepath.Glob("*.go")
		if err != nil || len(files) == 0 {
			t.Fatalf("sources: %v %v", files, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			parsed, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range parsed.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				for _, bad := range forbidden {
					if path == bad || strings.HasPrefix(path, bad+"/") {
						t.Errorf("%s imports %s: cleanup must not reach Git storage", f, path)
					}
				}
			}
		}
	})

	t.Run("the ports expose no Git operation", func(t *testing.T) {
		allowed := map[string][]string{
			"Records":         {"Get", "RequestCleanup", "Scan"},
			"Runs":            {"AdmitCleanupRound", "CleanupRounds"},
			"DeliveryService": {"GetDeployment", "ListDeployments", "RequestCleanup"},
		}
		for name, typ := range map[string]reflect.Type{
			"Records":         reflect.TypeOf((*deliverycleanup.Records)(nil)).Elem(),
			"Runs":            reflect.TypeOf((*deliverycleanup.Runs)(nil)).Elem(),
			"DeliveryService": reflect.TypeOf((*deliverycleanup.DeliveryService)(nil)).Elem(),
		} {
			var got []string
			for i := 0; i < typ.NumMethod(); i++ {
				got = append(got, typ.Method(i).Name)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, allowed[name]) {
				t.Errorf("%s exposes %v, want %v", name, got, allowed[name])
			}
		}
	})

	t.Run("the cleanup of a source-set deployment keeps its source set", func(t *testing.T) {
		now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
		exp := now.Add(time.Hour)
		set, err := delivery.NewSourceSet("sha256:"+strings.Repeat("d", 64), []delivery.SourceCommit{
			{RepositoryID: "repo_api", Commit: strings.Repeat("1", 40)}, {RepositoryID: "repo_web", Commit: strings.Repeat("2", 40)}})
		if err != nil {
			t.Fatal(err)
		}
		source := delivery.Source{RepositoryURL: "https://git.example.invalid/git/t/p/api.git", Commit: strings.Repeat("1", 40), SourceSet: set}
		d, err := delivery.NewDeployment(delivery.Registration{
			Scope: scopeA, Environment: "env_1", ConfigDigest: runtimev1.Digest("sha256:" + strings.Repeat("a", 64)), Namespace: "preview",
			Source:   source,
			Image:    delivery.Image{ArtifactID: "art_1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("b", 64))},
			Producer: delivery.Producer{Attempt: "att_build", Run: "run_build", Step: "build", ToolImage: "registry.example.invalid/tools@sha256:" + strings.Repeat("c", 64)},
			Run:      "run_deploy", Step: "deploy", ActionKey: "act:0123456789abcdef", Attempt: "att_deploy", ExpiresAt: &exp,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		objs := []delivery.ObjectRef{{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "preview", Name: "web"}}
		for _, step := range []func() error{
			func() error { _, err := d.BeginApply(objs, now); return err },
			func() error { _, err := d.Settle(delivery.Settlement{Reason: "apply_failed"}, now); return err },
			func() error { _, err := d.RequestCleanup(now); return err },
		} {
			if err := step(); err != nil {
				t.Fatal(err)
			}
		}
		f := &fakeDelivery{deps: []delivery.Deployment{d}}
		recs, err := deliverycleanup.NewRecords(f)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		page, _, err := recs.Scan(ctx, scopeA, "", 10)
		if err != nil || len(page) != 1 || page[0].State != deliverycleanup.RecordRequested || page[0].Scope != (domain.Scope{TenantID: scopeA.TenantID, ProjectID: scopeA.ProjectID}) {
			t.Fatalf("a source-set deployment is cleanup work like any other: %+v %v", page, err)
		}
		if err := recs.RequestCleanup(ctx, scopeA, string(d.ID())); err != nil {
			t.Fatal(err)
		}
		// The report that the owned objects are gone ends the cleanup: the record
		// keeps the set that identifies the commits retention still owns.
		if _, err := d.RecordCleanup(delivery.CleanupResult{State: delivery.CleanupDone, Deleted: objs}, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		rec := d.Record()
		if !d.Deleted() || rec.Source != source {
			t.Fatalf("cleanup changed the recorded source: deleted=%v %+v", d.Deleted(), rec.Source)
		}
		if got := rec.Source.SourceSet.Commits(); len(got) != 2 || got[0].RepositoryID != "repo_api" || got[1].RepositoryID != "repo_web" {
			t.Fatalf("the per-repository commits survive cleanup: %+v", got)
		}
	})
}

// Test_unit_cleanup_round_leaves_git_storage_alone (GR-13, GR-14): a real cleanup
// round for the deployment of a source-set run (expiry, request, run admission,
// the run's attempt, the report that the objects are gone) runs against an
// execution service that has the whole internal Git service wired as spies: the
// repository and pin seam, the workspace seam and the Git grant issuer. Nothing of
// it is called: no repository is read or created, no branch or retained ref is
// created, moved or released, no workspace is provisioned or checkpointed and no
// Git grant is issued or revoked.
func Test_unit_cleanup_round_leaves_git_storage_alone(t *testing.T) {
	repos, git, internal := executiontest.NewFakeSourceSets(), executiontest.NewFakeGitAuthority(), executiontest.NewFakeInternalWorkspaces("")
	rec := newRecords(nil)
	m := executiontest.NewMem(t, executiontest.MemOptions{Cleanup: rec, SourceSets: repos, GitGrants: git, InternalWorkspaces: internal, InternalPeer: internal, GitRevoker: git.MemRevoker()})
	rec.clock = m.Clock
	m.GrantAll()
	w := &world{t: t, m: m, rec: rec, proj: m.NewProject(scopeA)}
	w.def = m.Define(scopeA, pipelinetest.Doc(pipelinetest.Container("cleanup", "workspace: {access: none}"))).ID
	w.sched = w.scheduler(deliverycleanup.Policy{})

	// The repositories and pins the deployment's source set names exist and are
	// the spies' own state: any touch is a recorded call.
	repo := repos.AddRepository(scopeA, "backend")
	repos.SetTip(repo, "refs/heads/main", executiontest.CommitB)
	baseline := repos.Calls()

	const dep = "dep_srcset"
	w.rec.add(scopeA, dep, time.Hour)
	w.m.Advance(2 * time.Hour)
	it := one(t, w.sweep(w.sched))
	wantOutcome(t, it, deliverycleanup.OutcomeAdmitted)
	in := w.claim()
	if in.Access != execution.AccessNone || in.WorkspaceID != "" {
		t.Fatalf("a cleanup attempt carries a workspace: %+v", in)
	}
	if _, err := w.m.Succeed(in, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := w.m.Terminate(in, execution.LaunchStarted); err != nil {
		t.Fatal(err)
	}
	w.rec.report(scopeA, dep, deliverycleanup.RecordDone)
	if rep := w.sweep(w.sched); len(rep.Items) != 0 {
		t.Fatalf("a cleaned-up deployment is not cleanup work again: %+v", rep.Items)
	}

	r := w.cleanupRuns()
	if len(r) != 1 || r[0].Sources != nil || !r[0].Origin.IsZero() || r[0].Status != execution.RunSucceeded {
		t.Fatalf("the cleanup run is a source-free run of the earlier model: %+v", r)
	}
	if got := repos.Calls(); !reflect.DeepEqual(got, baseline) {
		t.Fatalf("cleanup called the repository or pin seam: %v (before the round: %v)", got, baseline)
	}
	if _, retained := repos.Retained(r[0].ID); retained {
		t.Fatal("cleanup retained or released refs")
	}
	if len(internal.Provisioned()) != 0 || len(internal.Checkpoints()) != 0 || internal.Holder() != "" {
		t.Fatalf("cleanup touched a workspace: %+v %+v %q", internal.Provisioned(), internal.Checkpoints(), internal.Holder())
	}
	if len(git.Issued()) != 0 || git.Revokes() != 0 || len(git.DirectRevocations()) != 0 {
		t.Fatalf("cleanup issued or revoked a Git grant: %+v %d", git.Issued(), git.Revokes())
	}
}
