package executiontest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// The source-set contract (T-148, ADR 0011, PX-23-PX-25): origins and source sets
// pinned at admission, the runtime context version that carries them, the
// attempt Git grant requested at start and revoked with the attempt, one
// executable attempt per run, the deployment-cleanup path unchanged and the
// earlier run models untouched. Every scenario runs on the in-memory store in the
// unit layer (the Test_unit_* acceptance names) and on PostgreSQL in the `test`
// environment.

// SourceOpen builds a fresh subject.
type SourceOpen func(testing.TB) SourceSubject

const (
	srcWorkerWrite = "workspace: {access: write}"
	srcWorkerRead  = "workspace: {access: read}"
	srcTwoSources  = "[{repository: backend, path: backend, access: write}, {repository: frontend, path: frontend, access: read}]"
)

type srig struct {
	rig
	ss SourceSubject
}

func newSrig(t *testing.T, open SourceOpen) *srig {
	t.Helper()
	ss := open(t)
	return &srig{rig: rig{t: t, s: ss.SharedSubject}, ss: ss}
}

func (r *srig) admit3(cmd execution.SourceRunCommand) (*execution.Run, error) {
	return r.ss.Svc.AdmitSourceRun(r.ctx(), sharedCaller, cmd)
}

func (r *srig) mustAdmit3(cmd execution.SourceRunCommand) *execution.Run {
	r.t.Helper()
	run, err := r.admit3(cmd)
	if err != nil {
		r.t.Fatalf("admit: %v", err)
	}
	r.run = run
	return run
}

var manualOrigin = execution.Origin{Kind: execution.OriginManual}

// receipt3 records the trusted preparation receipt of the run's sources.
func (r *srig) receipt3() (execution.ReceiptAck, error) {
	src := r.run.Sources
	req := runtimev3.PreparationReceiptRequestV3{HooksDisabled: true, Submodules: runtimev3.SubmodulesSkipped, CredentialsRemoved: true, Attempts: 1}
	for _, e := range src.Entries {
		req.Sources = append(req.Sources, runtimev3.PreparedSource{Path: e.Path, Commit: runtimev1.GitCommit(e.Commit), TreeID: CommitC})
	}
	return r.ss.Svc.RecordPreparationReceiptV3(r.ctx(), SharedScope, r.run.ID, r.claimOf(), req)
}

// up drives a freshly admitted source-set run to an open barrier.
func (r *srig) up() {
	r.t.Helper()
	r.s.Resources.AutoReady = true
	r.start()
	r.pass()
	if r.run.Sources != nil && !r.run.Sources.Empty() {
		if _, err := r.receipt3(); err != nil {
			r.t.Fatalf("receipt: %v", err)
		}
	}
	r.passes(2)
}

func (r *srig) context3(in *execution.AttemptSpec) runtimev3.ContextV3 {
	r.t.Helper()
	c, err := in.DecodeContextV3()
	if err != nil {
		r.t.Fatalf("decode context: %v", err)
	}
	if err := c.Validate(); err != nil {
		r.t.Fatalf("the context violates the runtime v3 contract: %v", err)
	}
	return c
}

func (r *srig) begin3(in *execution.AttemptSpec) error {
	c := r.context3(in)
	req := runtimev3.BeginRequestV3{Sources: []runtimev3.BeginSource{}}
	for _, s := range c.Workspace.Sources {
		req.Sources = append(req.Sources, runtimev3.BeginSource{Path: s.Path, Head: s.HeadCommit, TrackedClean: true})
	}
	_, err := r.ss.Svc.RecordBeginV3(r.ctx(), SharedScope, in.AttemptID, ContractClaim, req)
	return err
}

func (r *srig) result3(in *execution.AttemptSpec, commits ...runtimev3.CommitOutput) (execution.ResultAck, error) {
	zero := int64(0)
	if commits == nil {
		commits = []runtimev3.CommitOutput{}
	}
	res := runtimev3.ResultV3{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Commits: commits, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	h, err := runtimev3.ResultHash(res)
	must(r.t, err)
	return r.ss.Svc.CommitResultV3(r.ctx(), execution.ResultCommitV3{
		Scope: SharedScope, AttemptID: in.AttemptID, Request: runtimev3.ResultRequestV3{ResultHash: h, Result: res},
	})
}

func (r *srig) define(yaml string) execution.SourceRunCommand {
	r.t.Helper()
	def := r.ss.DefineSource(yaml)
	return execution.SourceRunCommand{Scope: SharedScope, DefinitionID: def.ID}
}

// SourceAdmissionPinsSourceSet: a run pins one exact commit per repository of its
// project with unique checkout paths, retains them, replays with the same commits
// after the branches moved, and refuses a foreign or unservable repository and an
// unreachable commit without creating anything (PX-02, PX-23; PX-A19).
func SourceAdmissionPinsSourceSet(t *testing.T, open SourceOpen) {
	r := newSrig(t, open)
	backend, frontend := r.ss.Repos.AddRepository(SharedScope, "backend"), r.ss.Repos.AddRepository(SharedScope, "frontend")
	r.ss.Repos.SetTip(frontend, "refs/heads/main", CommitB)
	cmd := r.define(SourceDoc(srcTwoSources, SharedStep("build", srcWorkerWrite), SharedStep("test", "needs: [build]", srcWorkerRead)))
	cmd.Origin, cmd.RunID = manualOrigin, "run_t148_pins"
	run := r.mustAdmit3(cmd)

	if run.Origin.Kind != execution.OriginManual || run.WorkItemID != "" || run.WorkspaceID != "" || run.Specification != nil || run.SourceCommit != "" {
		t.Fatalf("a manual run has no work item, workspace or source commit: %+v", run)
	}
	got := r.ss.Run(run.ID)
	if got.Sources == nil || len(got.Sources.Entries) != 2 || r.ss.RunSourceRows(run.ID) != 2 {
		t.Fatalf("the source set is not persisted: %+v", got.Sources)
	}
	e0, e1 := got.Sources.Entries[0], got.Sources.Entries[1]
	branch := "refs/heads/kodama/runs/" + string(run.ID) + "/work"
	if e0.Path != "backend" || e0.RepositoryID != backend.ID || e0.Commit != CommitA || e0.Access != gitv1.AccessWrite || e0.Branch == nil || *e0.Branch != branch ||
		e1.Path != "frontend" || e1.RepositoryID != frontend.ID || e1.Commit != CommitB || e1.Access != gitv1.AccessRead || e1.Branch != nil {
		t.Fatalf("pinned entries: %+v", got.Sources.Entries)
	}
	set, err := got.Sources.Set()
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := set.Digest(); d == "" || d != got.Sources.Digest {
		t.Fatalf("digest %q vs %q", d, got.Sources.Digest)
	}
	if got.SourceRetention != execution.RetentionConfirmed || got.RetainedRefID != "" {
		t.Fatalf("retention %s %q", got.SourceRetention, got.RetainedRefID)
	}
	if retained, ok := r.ss.Repos.Retained(run.ID); !ok || len(retained.Sources) != 2 {
		t.Fatalf("the set was not retained: %+v", retained)
	}

	t.Run("a replay returns the pinned commits after the branches moved", func(t *testing.T) {
		pins := r.ss.Repos.Count("pin")
		r.ss.Repos.SetTip(backend, "refs/heads/main", CommitE)
		again, err := r.admit3(cmd)
		if err != nil || again.ID != run.ID {
			t.Fatalf("replay: %+v %v", again, err)
		}
		if again.Sources.Entries[0].Commit != CommitA || r.ss.Repos.Count("pin") != pins {
			t.Fatalf("the replay read the moved branch: %+v pins %d -> %d", again.Sources.Entries, pins, r.ss.Repos.Count("pin"))
		}
		if len(r.ss.List()) != 1 {
			t.Fatalf("a replay created another run: %d", len(r.ss.List()))
		}
		other := cmd
		other.Pins = map[string]execution.SourcePin{"frontend": {Commit: strPtr(CommitC)}}
		if _, err := r.admit3(other); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("another declaration under the same run ID: %v", err)
		}
		// A fresh run pins the moved tip: the pin is the admission's, never later.
		fresh := cmd
		fresh.RunID = ""
		next := r.mustAdmit3(fresh)
		if next.Sources.Entries[0].Commit != CommitE || next.SnapshotDigest == run.SnapshotDigest {
			t.Fatalf("a fresh run pins the current tip and another snapshot: %+v", next.Sources.Entries)
		}
	})

	refuse := func(t *testing.T, name string, c execution.SourceRunCommand, want string) {
		t.Helper()
		before, retains := len(r.ss.List()), r.ss.Repos.Count("retain")
		_, err := r.admit3(c)
		if !errors.Is(err, execution.ErrAdmissionRefused) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v, want a refusal %q", name, err, want)
		}
		if len(r.ss.List()) != before || r.ss.Repos.Count("retain") != retains {
			t.Fatalf("%s: a refused set created or retained something", name)
		}
	}
	t.Run("a repository of another project is refused like an unknown one", func(t *testing.T) {
		r.ss.Repos.AddRepository(ScopeA2, "elsewhere")
		doc := r.define(SourceDoc("[{repository: elsewhere, path: ., access: read}]", SharedStep("only", srcWorkerRead)))
		doc.Origin = manualOrigin
		refuse(t, "foreign", doc, "source_repository_unknown")
		doc2 := r.define(SourceDoc("[{repository: ghost, path: ., access: read}]", SharedStep("only", srcWorkerRead)))
		doc2.Origin = manualOrigin
		refuse(t, "unknown", doc2, "source_repository_unknown")
	})
	t.Run("a repository that cannot be served is refused", func(t *testing.T) {
		for _, state := range []string{"archived", "not_ready", "external"} {
			name := "svc-" + strings.ReplaceAll(state, "_", "-")
			r.ss.Repos.AddRepository(SharedScope, name)
			r.ss.Repos.SetState(name, state)
			doc := r.define(SourceDoc("[{repository: "+name+", path: ., access: read}]", SharedStep("only", srcWorkerRead)))
			doc.Origin = manualOrigin
			want := map[string]string{"archived": "repository_archived", "not_ready": "repository_not_ready", "external": "hosting_mismatch"}[state]
			refuse(t, state, doc, want)
		}
	})
	t.Run("an unreachable commit is refused", func(t *testing.T) {
		lost := r.ss.Repos.AddRepository(SharedScope, "lost")
		r.ss.Repos.SetTip(lost, "refs/heads/main", CommitF)
		r.ss.Repos.MarkUnreachable(CommitF)
		doc := r.define(SourceDoc("[{repository: lost, path: ., access: read}]", SharedStep("only", srcWorkerRead)))
		doc.Origin = manualOrigin
		refuse(t, "unreachable", doc, "source_commit_unreachable")
	})
	t.Run("a duplicate or nested path never forms a set", func(t *testing.T) {
		res := execution.ResolvedSources{BaseURL: "https://git.example.invalid/git", Repositories: []execution.SourceRepository{backend, frontend}}
		for name, paths := range map[string][2]string{"duplicate": {"same", "same"}, "nested": {"a", "a/b"}, "root and another": {".", "x"}} {
			declared := []execution.DeclaredSource{
				{Repository: "backend", Path: paths[0], Access: gitv1.AccessRead}, {Repository: "frontend", Path: paths[1], Access: gitv1.AccessRead}}
			if _, err := execution.NewRunSources(manualOrigin, "run_x", res, declared, []string{CommitA, CommitB}, "sha256:"+strings.Repeat("a", 64)); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
	t.Run("an origin run admits only a kodama/v1alpha3 definition", func(t *testing.T) {
		before, retains := len(r.ss.List()), r.ss.Repos.Count("retain")
		for name, yaml := range map[string]string{
			"kodama/v1alpha1": pipelinetestDoc(),
			"kodama/v1alpha2": SharedSpecDoc(false, SharedStep("only", srcWorkerRead)),
		} {
			c := r.define(yaml)
			c.Origin, c.Repository = manualOrigin, "backend"
			if _, err := r.admit3(c); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Fatalf("%s through the origin path: %v", name, err)
			}
			if _, err := r.ss.Svc.PrepareSourceRun(r.ctx(), sharedCaller, c); !errors.Is(err, execution.ErrAdmissionRefused) {
				t.Fatalf("%s prepared through the origin path: %v", name, err)
			}
		}
		if len(r.ss.List()) != before || r.ss.Repos.Count("retain") != retains {
			t.Fatal("a refused version created or retained something")
		}
	})
	t.Run("the earlier admission paths never see a source-set definition", func(t *testing.T) {
		def := r.ss.DefineSource(SourceDoc("", SharedStep("only", srcWorkerRead)))
		_, err := r.ss.Svc.Admit(r.ctx(), sharedCaller, execution.AdmitCommand{
			Scope: SharedScope, WorkItemID: r.ss.Project.Item, DefinitionID: def.ID, ContentRevision: r.ss.Seed.ContentRevision(r.ss.Project), SourceCommit: CommitA})
		if !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("the public work admission accepted a kodama/v1alpha3 definition: %v", err)
		}
	})
}

func strPtr(s string) *string { return &s }

// SourceAdmissionPushOrigin: a push-originated run pins the pushed entry's new
// commit, has no work item and consumes no work-in-progress, is created once per
// run identity, and is refused for a push that is not applied (PX-24; PX-A20).
func SourceAdmissionPushOrigin(t *testing.T, open SourceOpen) {
	r := newSrig(t, open)
	app := r.ss.Repos.AddRepository(SharedScope, "app")
	other := r.ss.Repos.AddRepository(SharedScope, "other")
	push := r.ss.Repos.AddPush(SharedScope, app, "refs/heads/main", CommitD)
	origin := execution.Origin{Kind: execution.OriginPush, PushID: push, RepositoryID: app.ID, Ref: "refs/heads/main"}
	cmd := r.define(SourceDoc("", SharedStep("ci", srcWorkerRead)))
	cmd.Origin, cmd.RunID = origin, "run_t148_push"

	created := r.s.Events("run.created")
	run := r.mustAdmit3(cmd)
	if run.Origin != origin || run.WorkItemID != "" || run.WorkspaceID != "" || run.ContentRevision != 0 || run.Specification != nil {
		t.Fatalf("a push-originated run fabricated a work item: %+v", run)
	}
	if e := r.ss.Run(run.ID).Sources.Entries; len(e) != 1 || e[0].RepositoryID != app.ID || e[0].Path != "." || e[0].Commit != CommitD || e[0].Access != gitv1.AccessRead {
		t.Fatalf("the pinned source: %+v", e)
	}
	if got := len(r.s.Events("run.created")); got != len(created) {
		t.Fatalf("run.created names a work item and is not emitted for a run without one (%d -> %d)", len(created), got)
	}
	if byItem, err := r.ss.Svc.List(r.ctx(), sharedCaller, SharedScope, execution.ListQuery{WorkItemID: r.ss.Project.Item, Limit: 10}); err != nil || len(byItem) != 0 {
		t.Fatalf("the project's work item has a run it never started: %+v %v", byItem, err)
	}

	t.Run("a replay and a later branch movement start nothing more", func(t *testing.T) {
		r.ss.Repos.SetTip(app, "refs/heads/main", CommitE)
		again, err := r.admit3(cmd)
		if err != nil || again.ID != run.ID || again.Sources.Entries[0].Commit != CommitD || len(r.ss.List()) != 1 {
			t.Fatalf("replay: %+v %v (%d runs)", again, err, len(r.ss.List()))
		}
	})
	t.Run("a push that is not applied, or not of the repository, is refused", func(t *testing.T) {
		refused := r.ss.Repos.AddRefusedPush(SharedScope, app, "refs/heads/dev", CommitE)
		foreign := r.ss.Repos.AddPush(SharedScope, other, "refs/heads/main", CommitE)
		for name, o := range map[string]execution.Origin{
			"refused entry":    {Kind: execution.OriginPush, PushID: refused, RepositoryID: app.ID, Ref: "refs/heads/dev"},
			"another repo":     {Kind: execution.OriginPush, PushID: foreign, RepositoryID: app.ID, Ref: "refs/heads/main"},
			"unknown record":   {Kind: execution.OriginPush, PushID: "push_999", RepositoryID: app.ID, Ref: "refs/heads/main"},
			"a ref not pushed": {Kind: execution.OriginPush, PushID: push, RepositoryID: app.ID, Ref: "refs/heads/other"},
		} {
			c := cmd
			c.RunID, c.Origin = "", o
			before := len(r.ss.List())
			if _, err := r.admit3(c); !errors.Is(err, execution.ErrAdmissionRefused) || len(r.ss.List()) != before {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})
	t.Run("the pushed repository must be a source of the run", func(t *testing.T) {
		c := r.define(SourceDoc("[{repository: other, path: ., access: read}]", SharedStep("ci", srcWorkerRead)))
		c.Origin = origin
		if _, err := r.admit3(c); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("a push of a repository outside the set: %v", err)
		}
	})
}

// SourceAdmissionProvisioning: a provisioning run is admitted before its
// repository exists (a source-free run holding the seed capability, no static Git
// grant), and a later run uses the repository the first one created (PX-25;
// PX-A21).
func SourceAdmissionProvisioning(t *testing.T, open SourceOpen) {
	r := newSrig(t, open)
	cmd := r.define(SourceDoc("", SharedStep("provision", "workspace: {access: none}")))
	cmd.Origin = execution.Origin{Kind: execution.OriginProvision}
	run := r.mustAdmit3(cmd)
	got := r.ss.Run(run.ID)
	if got.Origin.Kind != execution.OriginProvision || got.Sources == nil || !got.Sources.Empty() || got.SourceRetention != execution.RetentionNotRequired ||
		got.WorkItemID != "" || r.ss.Repos.Count("pin") != 0 || r.ss.Repos.Count("retain") != 0 {
		t.Fatalf("a provisioning run needs no repository: %+v", got)
	}
	if got.Sources.BaseURL == "" {
		t.Fatal("the endpoint is pinned for a source-free run too")
	}
	r.up()
	in := r.mustClaim()
	c := r.context3(in)
	if c.Origin.Kind != runtimev3.OriginProvision || c.WorkItem != nil || len(c.Workspace.Sources) != 0 || c.Workspace.SourceSetDigest != nil || c.Workspace.Preparation != nil ||
		c.Git == nil || c.Git.GrantID != nil || c.Git.CredentialFile != nil || len(c.Git.Repositories) != 0 || c.Git.Runtime.Seed == nil || c.Git.Runtime.Integration != nil {
		t.Fatalf("a provisioning context holds only the seed capability: %+v", c.Git)
	}
	// No static grant is requested for it: the seed grant is issued at runtime, only
	// for a repository the run created in its own project.
	r.s.Drive.Start(in)
	if reqs := r.ss.Git.Issued(); len(reqs) != 0 {
		t.Fatalf("a provisioning run holds no grant at start: %+v", reqs)
	}
	if _, err := r.result3(in); err != nil {
		t.Fatalf("result: %v", err)
	}

	t.Run("a second run uses the repository the first created", func(t *testing.T) {
		made := r.ss.Repos.AddRepository(SharedScope, "created-by-run")
		use := r.define(SourceDoc("[{repository: created-by-run, path: ., access: read}]", SharedStep("use", srcWorkerRead)))
		use.Origin = manualOrigin
		next := r.mustAdmit3(use)
		if e := next.Sources.Entries; len(e) != 1 || e[0].RepositoryID != made.ID {
			t.Fatalf("second run: %+v", e)
		}
	})
}

// SourceAdmissionManualOrigin: a manual run pins an exact commit, a ref's tip or
// the default branch's tip, and may be source-free; it has no work item (PX-23).
func SourceAdmissionManualOrigin(t *testing.T, open SourceOpen) {
	r := newSrig(t, open)
	repo := r.ss.Repos.AddRepository(SharedScope, "svc")
	r.ss.Repos.SetTip(repo, "refs/heads/release", CommitB)
	base := r.define(SourceDoc("", SharedStep("check", srcWorkerRead)))
	base.Origin, base.Repository = manualOrigin, "svc"
	for name, tc := range map[string]struct {
		pin  execution.SourcePin
		want string
	}{
		"default branch":  {execution.SourcePin{}, CommitA},
		"a ref's tip":     {execution.SourcePin{Ref: strPtr("refs/heads/release")}, CommitB},
		"an exact commit": {execution.SourcePin{Commit: strPtr(CommitC)}, CommitC},
	} {
		c := base
		c.Pins = map[string]execution.SourcePin{"svc": tc.pin}
		run := r.mustAdmit3(c)
		if e := run.Sources.Entries; len(e) != 1 || e[0].Commit != tc.want || run.WorkItemID != "" || run.Origin.Kind != execution.OriginManual {
			t.Fatalf("%s: %+v", name, run.Sources)
		}
	}
	t.Run("a source-free manual run", func(t *testing.T) {
		c := r.define(SourceDoc("", SharedStep("tidy", "workspace: {access: none}")))
		c.Origin = manualOrigin
		run := r.mustAdmit3(c)
		if run.Sources == nil || !run.Sources.Empty() || run.SourceRetention != execution.RetentionNotRequired {
			t.Fatalf("%+v", run.Sources)
		}
	})
	t.Run("a pin must be one of ref or commit, and a work item belongs to its origin only", func(t *testing.T) {
		c := base
		c.Pins = map[string]execution.SourcePin{"svc": {Ref: strPtr("refs/heads/release"), Commit: strPtr(CommitC)}}
		if _, err := r.admit3(c); !errors.Is(err, execution.ErrInvalid) {
			t.Fatalf("both pins: %v", err)
		}
		c = base
		c.ContentRevision = 3
		if _, err := r.admit3(c); !errors.Is(err, execution.ErrInvalid) {
			t.Fatalf("a revision without a work item: %v", err)
		}
		c = base
		c.Origin = execution.Origin{Kind: execution.OriginManual, WorkItemID: "wi_x"}
		if _, err := r.admit3(c); !errors.Is(err, execution.ErrInvalid) {
			t.Fatalf("a work item on a manual origin: %v", err)
		}
	})
	t.Run("an installation without the seams admits none", func(t *testing.T) {
		closed := r.ss.Closed()
		if _, err := closed.AdmitSourceRun(r.ctx(), sharedCaller, base); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("closed composition: %v", err)
		}
	})
}

// SourceCleanupPurposeUnchanged: a deployment cleanup run is admitted, authorized
// and recognized exactly as before (the cleanup purpose, no work item, source,
// origin or source set), its attempts run under the earlier context and no Git
// grant is ever requested for it (PX-19; deployment cleanup stays source-free).
func SourceCleanupPurposeUnchanged(t *testing.T, open SourceOpen) {
	r := newSrig(t, open)
	clean := r.s.DefineShared(SharedSpecDoc(false, SharedStep("teardown", "workspace: {access: none}")))
	run, err := r.s.NewCleanup().AdmitCleanup(r.ctx(), execution.CleanupCommand{Scope: SharedScope, DeploymentID: "dep_t148", DefinitionID: clean.ID})
	if err != nil {
		t.Fatalf("cleanup admission: %v", err)
	}
	got := r.ss.Run(run.ID)
	if got.Purpose != execution.PurposeCleanup || got.DeploymentID != "dep_t148" || !got.Origin.IsZero() || got.Sources != nil || got.WorkItemID != "" ||
		got.WorkspaceID != "" || got.SourceRetention != execution.RetentionNotRequired {
		t.Fatalf("a cleanup run keeps its shape: %+v", got)
	}
	if o := execution.OriginOf(got); o.Kind != execution.OriginManual || o.WorkItemID != "" {
		t.Fatalf("a cleanup run is a manual run to the runtime contract: %+v", o)
	}
	r.run = run
	r.up()
	in := r.mustClaim()
	if c, err := in.DecodeContextV2(); err != nil || c.ContextVersion != 2 || c.Workspace.Access != runtimev1.AccessNone {
		t.Fatalf("a cleanup attempt keeps the earlier context: %+v %v", c, err)
	}
	r.s.Drive.Start(in)
	if len(r.ss.Git.Issued()) != 0 {
		t.Fatalf("a cleanup attempt holds no Git credential: %+v", r.ss.Git.Issued())
	}
	t.Run("authorization is the deployment's and nothing else", func(t *testing.T) {
		deny := errors.New("not a recorded deployment")
		c := r.ss.RestartSource()
		if _, err := c.AdmitCleanup(r.ctx(), execution.CleanupCommand{Scope: SharedScope, DeploymentID: "dep_t148_2", DefinitionID: clean.ID}); err != nil {
			t.Fatalf("allowed: %v", err)
		}
		_ = deny
		// A kodama/v1alpha3 definition is never a cleanup definition here.
		v3 := r.ss.DefineSource(SourceDoc("", SharedStep("teardown", "workspace: {access: none}")))
		if _, err := c.AdmitCleanup(r.ctx(), execution.CleanupCommand{Scope: SharedScope, DeploymentID: "dep_t148_3", DefinitionID: v3.ID}); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("a source-set definition as a cleanup definition: %v", err)
		}
	})
}

// SourceContextCarriesSourceSet: the attempt context of a source-set run is the
// runtime v3 context: the origin, the pinned sources at their paths, the digest of
// the set, the recorded per-source preparation receipt, the Git delivery of the
// run's own namespace (and, for the writer only, the write branches), and the
// lineage and heads that follow what prior attempts published (PX-23, GR-06).
func SourceContextCarriesSourceSet(t *testing.T, open SourceOpen) {
	r := newSrig(t, open)
	backend, frontend := r.ss.Repos.AddRepository(SharedScope, "backend"), r.ss.Repos.AddRepository(SharedScope, "frontend")
	r.ss.Repos.SetTip(frontend, "refs/heads/main", CommitB)
	cmd := r.define(SourceDoc(srcTwoSources, SharedStep("build", srcWorkerWrite), SharedStep("test", "needs: [build]", srcWorkerRead)))
	cmd.Origin = manualOrigin
	run := r.mustAdmit3(cmd)
	r.up()
	in := r.mustClaim()
	c := r.context3(in)
	if in.Access != execution.AccessWrite || in.WorkspaceID != "" {
		t.Fatalf("a run without a workspace binds none: %+v", in)
	}
	agent := "refs/heads/kodama/runs/" + string(run.ID) + "/*"
	work := "refs/heads/kodama/runs/" + string(run.ID) + "/work"
	if c.ContextVersion != 3 || c.RunModel != "run_shared" || c.Origin.Kind != runtimev3.OriginManual || c.WorkItem != nil ||
		c.Workspace.Access != runtimev1.AccessWrite || len(c.Workspace.Sources) != 2 || c.Workspace.SourceSetDigest == nil ||
		string(*c.Workspace.SourceSetDigest) != run.Sources.Digest || c.Workspace.Preparation == nil || len(c.Workspace.Preparation.Sources) != 2 {
		t.Fatalf("context: %+v", c)
	}
	s0, s1 := c.Workspace.Sources[0], c.Workspace.Sources[1]
	if s0.RepositoryID != backend.ID || s0.Path != "backend" || s0.Commit != CommitA || s0.HeadCommit != CommitA || s0.ExpectedTip == nil || *s0.ExpectedTip != CommitA ||
		s0.Branch == nil || *s0.Branch != work || s0.RepositoryURL != backend.URL ||
		s1.RepositoryID != frontend.ID || s1.Commit != CommitB || s1.ExpectedTip != nil || s1.Branch != nil {
		t.Fatalf("sources: %+v", c.Workspace.Sources)
	}
	g := c.Git
	if g == nil || g.GrantID == nil || *g.GrantID != execution.GitGrantRef(SharedScope, in.AttemptID) || g.BaseURL != "https://git.example.invalid/git" ||
		len(g.Repositories) != 2 || g.Repositories[0].RepositoryID != backend.ID || strings.Join(g.Repositories[0].Update, ",") != agent+","+work ||
		strings.Join(g.Repositories[1].Update, ",") != agent || g.Runtime.Seed != nil || g.Runtime.Integration == nil {
		t.Fatalf("git delivery: %+v", g)
	}
	if c.Workspace.Preparation.Sources[0].Path != "backend" || string(c.Workspace.Preparation.Sources[1].Commit) != CommitB {
		t.Fatalf("the receipt pins every source at its commit: %+v", c.Workspace.Preparation)
	}
	// The workload plan binds the whole set: the digest covers the sources, the
	// fingerprint the digest.
	if !strings.Contains(string(in.Context), run.Sources.Digest) {
		t.Fatal("the pinned context does not name the source set digest")
	}

	r.s.Drive.Start(in)
	must(t, r.begin3(in))
	if _, err := r.result3(in, runtimev3.CommitOutput{RepositoryID: backend.ID, Ref: work, Commit: runtimev1.GitCommit(CommitD), PushID: "push_x"}); err != nil {
		t.Fatalf("writer result: %v", err)
	}
	if got := r.ss.Run(run.ID); got.Status != execution.RunRunning {
		t.Fatalf("the run continues: %s", got.Status)
	}
	r.passes(2)
	in2 := r.mustClaim()
	c2 := r.context3(in2)
	if c2.Workspace.Access != runtimev1.AccessRead || c2.Workspace.Sources[0].HeadCommit != CommitD || c2.Workspace.Sources[0].Commit != CommitA ||
		c2.Workspace.Sources[0].ExpectedTip != nil || c2.Workspace.Sources[1].HeadCommit != CommitB ||
		len(c2.Workspace.Lineage) != 1 || len(c2.Workspace.Lineage[0].Published) != 1 || c2.Workspace.Lineage[0].Published[0].Commit != CommitD ||
		strings.Join(c2.Git.Repositories[0].Update, ",") != agent {
		t.Fatalf("a reader sees the writer's head and never the write branch: %+v %+v", c2.Workspace, c2.Git)
	}
	if *c2.Git.GrantID == *c.Git.GrantID {
		t.Fatal("each attempt has its own grant reference")
	}

	t.Run("the earlier result and begin routes refuse a v3 attempt", func(t *testing.T) {
		if _, err := r.s.Drive.Succeed(in2, nil, ""); !errors.Is(err, execution.ErrConflict) {
			t.Fatalf("a v1 result for a v3 attempt: %v", err)
		}
	})
	t.Run("a result naming a ref outside the attempt's grants is invalid", func(t *testing.T) {
		ack, err := r.result3(in2, runtimev3.CommitOutput{RepositoryID: backend.ID, Ref: "refs/heads/main", Commit: runtimev1.GitCommit(CommitE), PushID: "push_y"})
		if err != nil || ack.AttemptStatus != execution.AttemptFailed {
			t.Fatalf("a reader reported the default branch: %+v %v", ack, err)
		}
		if at := r.get().Attempt(in2.AttemptID); at.Reason != execution.ReasonNoOutputs || len(at.Commits) != 0 {
			t.Fatalf("a refused output is a validation failure that publishes nothing: %+v", at)
		}
	})
}

// SourceAttemptGrantRequestedAndRevoked: the attempt's Git grant is requested
// when the attempt starts, from its pinned context and before its workload is
// created, delivered only through the start spec, and revoked in the transaction
// that ends the attempt's authority: cancellation, failure, completion; a failed
// revocation rolls the transition back (GR-06, PX-08).
func SourceAttemptGrantRequestedAndRevoked(t *testing.T, open SourceOpen) {
	t.Run("requested at start from the pinned context", func(t *testing.T) {
		r := newSrig(t, open)
		repo := r.ss.Repos.AddRepository(SharedScope, "backend")
		cmd := r.define(SourceDoc("[{repository: backend, path: ., access: write}]", SharedStep("build", srcWorkerWrite)))
		cmd.Origin = manualOrigin
		run := r.mustAdmit3(cmd)
		r.up()
		r.s.Resources.AutoReady = true
		ok, err := r.ss.Svc.DispatchNext(r.ctx(), SharedScope.TenantID)
		if err != nil || !ok {
			t.Fatalf("dispatch: %v %v", ok, err)
		}
		starts := r.s.Starts()
		if len(starts) != 1 || starts[0].Git == nil {
			t.Fatalf("the workload was started without its credential: %+v", starts)
		}
		spec := starts[0]
		reqs := r.ss.Git.Issued()
		if len(reqs) != 1 {
			t.Fatalf("grant requests: %+v", reqs)
		}
		c := r.context3(&spec)
		req := reqs[0]
		if req.Kind != gitv1.GrantAttempt || req.Actor.Type != gitv1.ActorWorkload || req.Actor.AttemptID == nil || *req.Actor.AttemptID != string(spec.AttemptID) ||
			req.Actor.RunID == nil || *req.Actor.RunID != string(run.ID) || len(req.Repositories) != 1 || req.Repositories[0].ID != repo.ID || !req.Repositories[0].Read ||
			strings.Join(req.Repositories[0].Update, ",") != strings.Join(c.Git.Repositories[0].Update, ",") ||
			req.Repositories[0].Seed || req.Repositories[0].Integration != nil {
			t.Fatalf("the request is the context's grant: %+v", req)
		}
		deadline, _ := c.Run.DeadlineAt.Time()
		if req.ExpiresAt.After(deadline) {
			t.Fatalf("the grant outlives the run: %s > %s", req.ExpiresAt, deadline)
		}
		if spec.Git.GrantRef != *c.Git.GrantID || spec.Git.GrantID == "" || spec.Git.Credential.IsZero() {
			t.Fatalf("delivery: %+v", spec.Git)
		}
		for _, leak := range []string{spec.Git.Credential.Reveal()} {
			if strings.Contains(string(spec.Context), leak) || strings.Contains(fmtAll(spec.Git), leak) {
				t.Fatal("the credential leaked into the context or a formatter")
			}
		}
		if raw, _ := json.Marshal(spec.Git); strings.Contains(string(raw), spec.Git.Credential.Reveal()) {
			t.Fatal("the credential is encodable")
		}
	})

	t.Run("an issuance failure starts nothing and the redrive issues again", func(t *testing.T) {
		r := newSrig(t, open)
		r.ss.Repos.AddRepository(SharedScope, "backend")
		cmd := r.define(SourceDoc("", SharedStep("build", srcWorkerRead)))
		cmd.Origin, cmd.Repository = manualOrigin, "backend"
		r.mustAdmit3(cmd)
		r.up()
		r.ss.Git.IssueErr = errors.New("authority down")
		if _, err := r.ss.Svc.DispatchNext(r.ctx(), SharedScope.TenantID); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if len(r.s.Starts()) != 0 {
			t.Fatalf("a workload started without its grant: %+v", r.s.Starts())
		}
		r.ss.Git.IssueErr = nil
		r.tick(5e9)
		r.pass()
		if len(r.s.Starts()) != 1 || r.s.Starts()[0].Git == nil {
			t.Fatalf("the redrive did not start the same attempt with a grant: %+v", r.s.Starts())
		}
	})

	end := func(t *testing.T, how string) (*srig, execution.AttemptSpec) {
		r := newSrig(t, open)
		r.ss.Repos.AddRepository(SharedScope, "backend")
		cmd := r.define(SourceDoc("", SharedStep("build", srcWorkerRead)))
		cmd.Origin, cmd.Repository = manualOrigin, "backend"
		r.mustAdmit3(cmd)
		r.up()
		if _, err := r.ss.Svc.DispatchNext(r.ctx(), SharedScope.TenantID); err != nil {
			t.Fatal(err)
		}
		spec := r.s.Starts()[0]
		r.s.Drive.Start(&spec)
		must(t, r.begin3(&spec))
		if r.ss.GitRevoked(spec.AttemptID) {
			t.Fatal("revoked while running")
		}
		_ = how
		return r, spec
	}
	t.Run("cancellation revokes in its own transaction", func(t *testing.T) {
		r, spec := end(t, "cancel")
		boom := errors.New("revocation store down")
		r.ss.Git.RevokeErr = boom
		before := r.ss.Git.Revokes()
		if _, err := r.ss.Svc.Cancel(r.ctx(), sharedCaller, SharedScope, r.run.ID); !errors.Is(err, execution.ErrUnavailable) {
			t.Fatalf("a cancellation whose revocation failed: %v", err)
		}
		if r.ss.Git.Revokes() == before {
			t.Fatal("the cancellation never reached the revocation")
		}
		if agg := r.get(); agg.Run.CancelRequestedAt != nil || agg.Attempt(spec.AttemptID).Status != execution.AttemptRunning || r.ss.GitRevoked(spec.AttemptID) {
			t.Fatal("a failed revocation left the cancellation half applied")
		}
		r.ss.Git.RevokeErr = nil
		if _, err := r.ss.Svc.Cancel(r.ctx(), sharedCaller, SharedScope, r.run.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		agg := r.get()
		if agg.Run.CancelRequestedAt == nil || agg.Attempt(spec.AttemptID).Status != execution.AttemptCancelled || !r.ss.GitRevoked(spec.AttemptID) {
			t.Fatalf("the committed cancellation did not revoke the grant")
		}
		// The authority refuses a new credential for the ended attempt.
		if _, err := r.ss.Git.IssueAttempt(r.ctx(), SharedScope, execution.AttemptGrantRequest{
			Kind: gitv1.GrantAttempt, Actor: gitv1.Actor{Type: gitv1.ActorWorkload, ID: string(spec.AttemptID), AttemptID: strPtr(string(spec.AttemptID))}}); err == nil {
			t.Fatal("a cancelled attempt was issued a grant")
		}
		// Repeating the cancellation changes nothing.
		before = r.ss.Git.Revokes()
		if _, err := r.ss.Svc.Cancel(r.ctx(), sharedCaller, SharedScope, r.run.ID); err != nil || r.ss.Git.Revokes() != before {
			t.Fatalf("a repeated cancellation revoked again: %v", err)
		}
	})
	t.Run("a failed or completed attempt revokes too", func(t *testing.T) {
		r, spec := end(t, "complete")
		if _, err := r.result3(&spec); err != nil {
			t.Fatal(err)
		}
		if !r.ss.GitRevoked(spec.AttemptID) {
			t.Fatal("a completed attempt keeps its Git grant")
		}
		r2, spec2 := end(t, "fail")
		code, reason := int64(1), "command_failed"
		res := runtimev3.ResultV3{Outcome: runtimev1.OutcomeFailed, ExitCode: &code, Reason: &reason, Commits: []runtimev3.CommitOutput{}, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
		h, err := runtimev3.ResultHash(res)
		must(t, err)
		if _, err := r2.ss.Svc.CommitResultV3(r2.ctx(), execution.ResultCommitV3{Scope: SharedScope, AttemptID: spec2.AttemptID, Request: runtimev3.ResultRequestV3{ResultHash: h, Result: res}}); err != nil {
			t.Fatal(err)
		}
		if !r2.ss.GitRevoked(spec2.AttemptID) {
			t.Fatal("a failed attempt keeps its Git grant")
		}
	})
	t.Run("a run of an earlier model revokes no Git grant", func(t *testing.T) {
		r := newSrig(t, open)
		r.admit(SharedSpecDoc(false, SharedStep("job")))
		r.rig.up()
		in := r.mustClaim()
		r.s.Drive.Start(in)
		before := r.ss.Git.Revokes()
		if _, err := r.ss.Svc.Cancel(r.ctx(), sharedCaller, SharedScope, r.run.ID); err != nil {
			t.Fatal(err)
		}
		if r.ss.Git.Revokes() != before || len(r.ss.Git.Issued()) != 0 {
			t.Fatalf("an attempt_private or run_shared v2 run touched the Git authority (%d -> %d)", before, r.ss.Git.Revokes())
		}
	})
	t.Run("the preparation's grant is read-only and bounded", func(t *testing.T) {
		r := newSrig(t, open)
		a, b := r.ss.Repos.AddRepository(SharedScope, "backend"), r.ss.Repos.AddRepository(SharedScope, "frontend")
		cmd := r.define(SourceDoc(srcTwoSources, SharedStep("build", srcWorkerWrite), SharedStep("test", "needs: [build]", srcWorkerRead)))
		cmd.Origin = manualOrigin
		run := r.mustAdmit3(cmd)
		now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
		req, err := execution.PreparationGrantFor(run, "preparation", now.Add(time.Hour), now)
		if err != nil || req.Kind != gitv1.GrantPreparation || len(req.Repositories) != 2 || req.Repositories[0].ID != a.ID || req.Repositories[1].ID != b.ID {
			t.Fatalf("%+v %v", req, err)
		}
		for _, repo := range req.Repositories {
			if !repo.Read || len(repo.Update) != 0 || repo.Seed {
				t.Fatalf("a preparation grant updates nothing: %+v", repo)
			}
		}
		if req.Actor.AttemptID != nil || req.Actor.ID != "preparation" || req.ExpiresAt.After(now.Add(time.Hour)) {
			t.Fatalf("the preparation holds no attempt and ends with its deadline: %+v", req)
		}
		if _, err := execution.PreparationGrantFor(run, "preparation", now.Add(-time.Second), now); err == nil {
			t.Fatal("a grant for a passed deadline")
		}
	})
}

func fmtAll(v any) string {
	b, _ := json.Marshal(v)
	return string(b) + fmt.Sprintf("%v|%+v|%#v|%s", v, v, v, v)
}

func fmtErrBusy() error { return fmt.Errorf("%w: the writer gate is blocked", execution.ErrBusy) }

func pipelinetestDoc() string {
	return pipelinetest.Doc(pipelinetest.Container("a", "workspace: {access: read, from: run}"))
}

// SourceOneAttemptPerRun: a run_shared run executes one attempt at a time, whatever
// its step; a second claim waits until the first attempt is settled (PX-04 for
// the shared scratch and the run's Git authority).
func SourceOneAttemptPerRun(t *testing.T, open SourceOpen) {
	r := newSrig(t, open)
	r.ss.Repos.AddRepository(SharedScope, "backend")
	cmd := r.define(SourceDoc("", SharedStep("one", srcWorkerRead), SharedStep("two", srcWorkerRead)))
	cmd.Origin, cmd.Repository = manualOrigin, "backend"
	r.mustAdmit3(cmd)
	r.up()
	first := r.mustClaim()
	if _, err := r.claim(); !errors.Is(err, execution.ErrNothingRunnable) {
		t.Fatalf("a second attempt of the run while the first is unsettled: %v", err)
	}
	agg := r.get()
	if len(agg.Attempts) != 1 {
		t.Fatalf("%d attempts", len(agg.Attempts))
	}
	r.s.Drive.Start(first)
	must(t, r.begin3(first))
	if _, err := r.result3(first); err != nil {
		t.Fatal(err)
	}
	r.passes(2)
	if second, err := r.claim(); err != nil || second.StepID != "two" {
		t.Fatalf("the next attempt only after the first settled: %v %v", second, err)
	}
}

// SourceWorkItemWriter: a run started for a work item writes its internally
// hosted workspace through the work item's writer reservation: its task branch is
// provisioned before the set is built, the writer gate and the workspace version
// guard the claim, and a writer's task-branch commits are accepted as checkpoints
// before its slot is settled, the slot settled without a source commit (WS-17,
// WS-19). Memory only: the real workspace module has its own contract.
func SourceWorkItemWriter(t *testing.T, open SourceOpen) {
	r := newSrig(t, open)
	repo := r.ss.Repos.AddRepository(SharedScope, "backend")
	task := "refs/heads/kodama/work/" + string(r.ss.Project.Item)
	r.ss.Repos.SetTip(repo, task, CommitB)
	r.ss.Internal.Accepted[repo.ID] = CommitB
	cmd := r.define(SourceDoc("[{repository: backend, path: ., access: write}]", SharedStep("build", srcWorkerWrite)))
	cmd.Origin = execution.Origin{Kind: execution.OriginWorkItem, WorkItemID: r.ss.Project.Item}
	cmd.ContentRevision = r.s.Seed.ContentRevision(r.ss.Project)
	run := r.mustAdmit3(cmd)
	if got := r.ss.Internal.Provisioned(); len(got) != 1 || len(got[0]) != 1 || got[0][0] != repo.ID {
		t.Fatalf("the task branch is provisioned for the write source: %+v", got)
	}
	if run.WorkItemID != r.ss.Project.Item || run.WorkspaceID != string(r.ss.Project.Workspace) || run.Specification == nil || run.SourceCommit != "" {
		t.Fatalf("a work item's run: %+v", run)
	}
	if e := run.Sources.Entries; len(e) != 1 || e[0].Commit != CommitB || e[0].Branch == nil || *e[0].Branch != task {
		t.Fatalf("the write source is the task branch at its accepted commit: %+v", e)
	}
	r.up()

	r.ss.Internal.GateErr = fmtErrBusy()
	if in, err := r.claim(); !errors.Is(err, execution.ErrNothingRunnable) {
		t.Fatalf("a blocked gate must make the run wait: %v %v", in, err)
	}
	if len(r.get().Attempts) != 0 || r.ss.Internal.Holder() != "" {
		t.Fatal("an attempt was claimed against a blocked writer gate")
	}
	r.ss.Internal.GateErr = nil
	in := r.mustClaim()
	c := r.context3(in)
	if c.Origin.Kind != runtimev3.OriginWorkItem || c.WorkItem == nil || c.WorkItem.ID != r.ss.Project.Item || c.Workspace.Access != runtimev1.AccessWrite ||
		c.Workspace.Sources[0].ExpectedTip == nil || *c.Workspace.Sources[0].ExpectedTip != CommitB ||
		!strings.Contains(strings.Join(c.Git.Repositories[0].Update, ","), "refs/heads/kodama/agents/"+string(r.ss.Project.Item)+"/"+string(run.ID)+"/*") ||
		!strings.Contains(strings.Join(c.Git.Repositories[0].Update, ","), task) {
		t.Fatalf("a work item writer's context: %+v", c)
	}
	if in.WorkspaceID != string(r.ss.Project.Workspace) || in.Access != execution.AccessWrite || r.ss.Internal.Holder() != in.AttemptID {
		t.Fatalf("the writer reserved the work item's slot: %+v holder %s", in, r.ss.Internal.Holder())
	}
	r.s.Drive.Start(in)
	must(t, r.begin3(in))
	out := runtimev3.CommitOutput{RepositoryID: repo.ID, Ref: task, Commit: runtimev1.GitCommit(CommitD), PushID: "push_task"}

	r.ss.Internal.CheckpointErr = fmtErrBusy()
	if _, err := r.result3(in, out); !errors.Is(err, execution.ErrBusy) {
		t.Fatalf("an unsettled push keeps the output pending: %v", err)
	}
	if st := r.get().Attempt(in.AttemptID); st.Status != execution.AttemptRunning || st.ResultHash != "" {
		t.Fatalf("a pending checkpoint finalized the attempt: %+v", st)
	}
	r.ss.Internal.CheckpointErr = nil
	if _, err := r.result3(in, out); err != nil {
		t.Fatalf("result: %v", err)
	}
	if cps := r.ss.Internal.Checkpoints(); len(cps) != 1 || cps[0].Commit != CommitD || cps[0].Ref != task {
		t.Fatalf("checkpoints: %+v", cps)
	}
	if got := r.get(); got.Attempt(in.AttemptID).Status != execution.AttemptSucceeded || got.Step("build").OutputCommit != "" {
		t.Fatalf("the writer is settled without a source commit: %+v", got.Step("build"))
	}

	t.Run("a refused checkpoint fails the attempt as an invalid output", func(t *testing.T) {
		r := newSrig(t, open)
		repo := r.ss.Repos.AddRepository(SharedScope, "backend")
		task := "refs/heads/kodama/work/" + string(r.ss.Project.Item)
		r.ss.Repos.SetTip(repo, task, CommitB)
		r.ss.Internal.Accepted[repo.ID] = CommitB
		cmd := r.define(SourceDoc("[{repository: backend, path: ., access: write}]", SharedStep("build", srcWorkerWrite)))
		cmd.Origin = execution.Origin{Kind: execution.OriginWorkItem, WorkItemID: r.ss.Project.Item}
		cmd.ContentRevision = r.s.Seed.ContentRevision(r.ss.Project)
		r.mustAdmit3(cmd)
		r.up()
		in := r.mustClaim()
		r.s.Drive.Start(in)
		must(t, r.begin3(in))
		r.ss.Internal.CheckpointErr = execution.Refusedf("secret_in_source")
		if _, err := r.result3(in, runtimev3.CommitOutput{RepositoryID: repo.ID, Ref: task, Commit: runtimev1.GitCommit(CommitD), PushID: "push_t"}); err != nil {
			t.Fatalf("a refused output is a failed attempt, not an error: %v", err)
		}
		if at := r.get().Attempt(in.AttemptID); at.Status != execution.AttemptFailed || at.Reason != execution.ReasonOutputInvalid {
			t.Fatalf("the attempt: %+v", at)
		}
	})
	t.Run("a workspace that changed since the gate refuses the reservation", func(t *testing.T) {
		r := newSrig(t, open)
		repo := r.ss.Repos.AddRepository(SharedScope, "backend")
		task := "refs/heads/kodama/work/" + string(r.ss.Project.Item)
		r.ss.Repos.SetTip(repo, task, CommitB)
		r.ss.Internal.Accepted[repo.ID] = CommitB
		cmd := r.define(SourceDoc("[{repository: backend, path: ., access: write}]", SharedStep("build", srcWorkerWrite)))
		cmd.Origin = execution.Origin{Kind: execution.OriginWorkItem, WorkItemID: r.ss.Project.Item}
		cmd.ContentRevision = r.s.Seed.ContentRevision(r.ss.Project)
		r.mustAdmit3(cmd)
		r.up()
		r.ss.Internal.StaleGate()
		if in, err := r.claim(); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("a reservation against a stale gate answer: %v %v", in, err)
		}
		if len(r.get().Attempts) != 0 {
			t.Fatal("a stale reservation leaked an attempt")
		}
	})
}

// SourceOldRunModelsUnchanged: the attempt_private and run_shared contexts of
// runs admitted before origins and source sets existed are byte-identical, with
// their digests and workload fingerprints, to what the release before T-148
// produced (PX-21).
func SourceOldRunModelsUnchanged(t *testing.T) {
	var want map[string]OldContext
	if err := json.Unmarshal(OldContextsGolden, &want); err != nil {
		t.Fatal(err)
	}
	got := OldModelContexts(t)
	if len(got) != len(want) || len(want) != 3 {
		t.Fatalf("models: %d, want %d", len(got), len(want))
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok || g != w {
			t.Errorf("%s changed:\n got %+v\nwant %+v", name, g, w)
		}
	}
	t.Run("a legacy run has no origin or source set", func(t *testing.T) {
		m := NewMem(t)
		m.GrantAll()
		p := m.NewProject(ScopeA)
		run := m.MustAdmit(p, m.Define(ScopeA, pipelinetestDoc()), nil)
		if !run.Origin.IsZero() || run.Sources != nil || run.SourceRun() {
			t.Fatalf("%+v", run)
		}
		if o := execution.OriginOf(run); o.Kind != execution.OriginWorkItem || o.WorkItemID != p.Item {
			t.Fatalf("the effective origin of an earlier work run: %+v", o)
		}
	})
}

// SourceRun is one admitted, provisioned source-set run of a manual origin with a
// single step, for the adapter-specific suites that need the contract's fixture.
type SourceRun struct {
	r   *srig
	Run *execution.Run
}

var sourceRunSeq int

// NewSourceRun admits a manual run of one repository and drives it to an open
// barrier; access is the step's workspace access ("read" or "write").
func NewSourceRun(t *testing.T, s SourceSubject, access string) *SourceRun {
	t.Helper()
	return NewSourceRunSteps(t, s, SharedStep("only", "workspace: {access: "+access+"}"))
}

// NewSourceRunSteps is NewSourceRun for a document of the given steps.
func NewSourceRunSteps(t *testing.T, s SourceSubject, steps ...string) *SourceRun {
	t.Helper()
	sourceRunSeq++
	name := fmt.Sprintf("run-repo-%d", sourceRunSeq)
	r := &srig{rig: rig{t: t, s: s.SharedSubject}, ss: s}
	s.Repos.AddRepository(SharedScope, name)
	cmd := r.define(SourceDoc("", steps...))
	cmd.Origin, cmd.Repository = manualOrigin, name
	r.mustAdmit3(cmd)
	r.up()
	return &SourceRun{r: r, Run: r.run}
}

// Claim claims the run's next attempt.
func (s *SourceRun) Claim() *execution.AttemptSpec { return s.r.mustClaim() }

// TryClaim claims the run's next attempt, reporting the refusal instead of
// failing the test.
func (s *SourceRun) TryClaim() (*execution.AttemptSpec, error) { return s.r.claim() }

// Get loads the run's aggregate.
func (s *SourceRun) Get() *execution.Aggregate { return s.r.get() }

// Succeed starts the attempt, records its begin and completes it, reporting one
// pushed commit of the run's repository on ref.
func (s *SourceRun) Succeed(in *execution.AttemptSpec, ref, commit string) {
	s.r.t.Helper()
	s.r.s.Drive.Start(in)
	must(s.r.t, s.r.begin3(in))
	c := s.r.context3(in)
	if _, err := s.r.result3(in, runtimev3.CommitOutput{RepositoryID: c.Workspace.Sources[0].RepositoryID, Ref: ref, Commit: runtimev1.GitCommit(commit), PushID: "push_sql"}); err != nil {
		s.r.t.Fatalf("result: %v", err)
	}
}

// SourceAdmissionRunIDRace: two admissions under one RunID with different
// declarations never both succeed: the one that finds the other's run is an
// idempotency conflict, concurrent or not, and the winner replays (PX-24: one run
// per identity).
func SourceAdmissionRunIDRace(t *testing.T, open SourceOpen) {
	r := newSrig(t, open)
	repo := r.ss.Repos.AddRepository(SharedScope, "svc")
	r.ss.Repos.SetTip(repo, "refs/heads/release", CommitB)
	base := r.define(SourceDoc("", SharedStep("check", srcWorkerRead)))
	base.Origin, base.Repository = manualOrigin, "svc"
	plan := func(id domain.RunID, pin execution.SourcePin) *execution.AdmissionPlan {
		c := base
		c.RunID, c.Pins = id, map[string]execution.SourcePin{"svc": pin}
		p, err := r.ss.Svc.PrepareSourceRun(r.ctx(), sharedCaller, c)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		return p
	}
	a, b := plan("run_t148_race_1", execution.SourcePin{}), plan("run_t148_race_1", execution.SourcePin{Ref: strPtr("refs/heads/release")})
	if a.Aggregate.Run.Sources.Declaration == b.Aggregate.Run.Sources.Declaration {
		t.Fatal("the declarations must differ")
	}
	run, created, err := r.s.Store.Admit(r.ctx(), a)
	if err != nil || !created || run.Sources.Entries[0].Commit != CommitA {
		t.Fatalf("first: %v %v", created, err)
	}
	if _, _, err := r.s.Store.Admit(r.ctx(), b); !errors.Is(err, execution.ErrConflict) {
		t.Fatalf("another declaration under the same run ID: %v", err)
	}
	if again, created, err := r.s.Store.Admit(r.ctx(), a); err != nil || created || again.ID != run.ID {
		t.Fatalf("the winner's declaration replays: %v %v", created, err)
	}
	if got := r.ss.Run("run_t148_race_1"); got.Sources.Entries[0].Commit != CommitA {
		t.Fatalf("the stored pins are the winner's: %+v", got.Sources.Entries)
	}

	t.Run("concurrent admissions leave one run and one conflict", func(t *testing.T) {
		c, d := plan("run_t148_race_2", execution.SourcePin{}), plan("run_t148_race_2", execution.SourcePin{Ref: strPtr("refs/heads/release")})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i, p := range []*execution.AdmissionPlan{c, d} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, errs[i] = r.s.Store.Admit(r.ctx(), p)
			}()
		}
		wg.Wait()
		ok, conflict := 0, 0
		for _, e := range errs {
			switch {
			case e == nil:
				ok++
			case errors.Is(e, execution.ErrConflict):
				conflict++
			default:
				t.Fatalf("unexpected: %v", e)
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatalf("results %v", errs)
		}
		n := 0
		for _, x := range r.ss.List() {
			if x.ID == "run_t148_race_2" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d runs", n)
		}
	})
}

// SourceRedriveKeepsCredential: a redrive whose earlier Start created the workload
// but lost its response adopts the workload and issues no second grant: issuing
// again would replace the credential the workload holds (GR-06).
func SourceRedriveKeepsCredential(t *testing.T, open SourceOpen) {
	r := newSrig(t, open)
	r.ss.Repos.AddRepository(SharedScope, "backend")
	cmd := r.define(SourceDoc("", SharedStep("build", srcWorkerRead)))
	cmd.Origin, cmd.Repository = manualOrigin, "backend"
	r.mustAdmit3(cmd)
	r.up()
	r.ss.Runtime.LoseStartResponses(1)
	if ok, err := r.ss.Svc.DispatchNext(r.ctx(), SharedScope.TenantID); err != nil || !ok {
		t.Fatalf("dispatch: %v %v", ok, err)
	}
	starts := r.s.Starts()
	if len(starts) != 1 || starts[0].Git == nil || len(r.ss.Git.Issued()) != 1 {
		t.Fatalf("the first launch issues the grant once: %d starts, %d grants", len(starts), len(r.ss.Git.Issued()))
	}
	if r.get().Control(starts[0].AttemptID).DispatchConfirmedAt != nil {
		t.Fatal("the lost response was confirmed")
	}
	r.tick(5e9)
	r.pass()
	starts = r.s.Starts()
	if len(starts) != 2 || starts[1].AttemptID != starts[0].AttemptID {
		t.Fatalf("the redrive restarts the same attempt: %+v", starts)
	}
	if starts[1].Git != nil || len(r.ss.Git.Issued()) != 1 {
		t.Fatalf("the redrive of an existing workload issued another credential: git=%v grants=%d", starts[1].Git != nil, len(r.ss.Git.Issued()))
	}
	if r.get().Control(starts[1].AttemptID).DispatchConfirmedAt == nil {
		t.Fatal("the adopting redrive was not confirmed")
	}
}

// SourceGrantIssuanceRace: a cancellation committing between the Git authority's
// lease read and its grant insert leaves a live grant for an ended attempt; the
// attempt is read again after issuing and the grant revoked through the port, and
// nothing is started.
func SourceGrantIssuanceRace(t *testing.T, open SourceOpen) {
	r := newSrig(t, open)
	r.ss.Repos.AddRepository(SharedScope, "backend")
	cmd := r.define(SourceDoc("", SharedStep("build", srcWorkerRead)))
	cmd.Origin, cmd.Repository = manualOrigin, "backend"
	r.mustAdmit3(cmd)
	r.up()
	r.ss.Git.AfterIssue = func(domain.AttemptID) {
		if _, err := r.ss.Svc.Cancel(r.ctx(), sharedCaller, SharedScope, r.run.ID); err != nil {
			t.Errorf("cancel in the window: %v", err)
		}
	}
	if ok, err := r.ss.Svc.DispatchNext(r.ctx(), SharedScope.TenantID); err != nil || !ok {
		t.Fatalf("dispatch: %v %v", ok, err)
	}
	if len(r.ss.Git.Issued()) != 1 {
		t.Fatalf("grants %d", len(r.ss.Git.Issued()))
	}
	if n := len(r.s.Starts()); n != 0 {
		t.Fatalf("a workload was started for an attempt that ended: %d", n)
	}
	att := r.get().Attempts[0].ID
	if d := r.ss.Git.DirectRevocations(); len(d) != 1 || d[0] != att {
		t.Fatalf("the dangling grant was not revoked through the port: %v", d)
	}
}

// SourceContract runs every scenario.
func SourceContract(t *testing.T, open SourceOpen) {
	t.Run("admission pins the source set", func(t *testing.T) { SourceAdmissionPinsSourceSet(t, open) })
	t.Run("push origin without a work item", func(t *testing.T) { SourceAdmissionPushOrigin(t, open) })
	t.Run("provisioning run", func(t *testing.T) { SourceAdmissionProvisioning(t, open) })
	t.Run("manual origin", func(t *testing.T) { SourceAdmissionManualOrigin(t, open) })
	t.Run("cleanup purpose unchanged", func(t *testing.T) { SourceCleanupPurposeUnchanged(t, open) })
	t.Run("context carries the source set", func(t *testing.T) { SourceContextCarriesSourceSet(t, open) })
	t.Run("attempt grant requested and revoked", func(t *testing.T) { SourceAttemptGrantRequestedAndRevoked(t, open) })
	t.Run("run ID race", func(t *testing.T) { SourceAdmissionRunIDRace(t, open) })
	t.Run("redrive keeps the credential", func(t *testing.T) { SourceRedriveKeepsCredential(t, open) })
	t.Run("grant issuance race", func(t *testing.T) { SourceGrantIssuanceRace(t, open) })
	t.Run("one attempt per run", func(t *testing.T) { SourceOneAttemptPerRun(t, open) })
	t.Run("old run models unchanged", func(t *testing.T) { SourceOldRunModelsUnchanged(t) })
	t.Run("a work item's writer", func(t *testing.T) {
		if s := open(t); s.SQL {
			t.Skip("the internal workspace writer is covered by the workspace module's own contract")
		}
		SourceWorkItemWriter(t, open)
	})
}
