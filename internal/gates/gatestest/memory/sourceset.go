package memory

import (
	"context"
	"fmt"
	"sync"
	"testing"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/gates/gatestest/gatescontract"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Fixture repositories and commits of a source-set run.
const (
	RepoAPI = "repo_api"
	RepoWeb = "repo_web"

	// APIPinned and WebPinned are the commits admission pinned; APINew is the
	// commit the writer published to the task branch of the API repository (the
	// candidate); APIBase and WebBase are the default branches' tips.
	APIPinned = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	WebPinned = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	APINew    = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	APIBase   = "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4"
	WebBase   = "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5"
	MovedTip  = "f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6"

	// DefaultRef is the default branch of both repositories.
	DefaultRef = "refs/heads/main"
)

// Bases is the fake default-branch tip reader: a controllable tip per
// repository. It answers the repositories it knows and ErrNotFound for the rest.
type Bases struct {
	mu    sync.Mutex
	tips  map[string]gates.Tip
	err   error
	calls []string
}

var _ gates.Bases = (*Bases)(nil)

// NewBases starts with both fixture repositories at their base tips.
func NewBases() *Bases {
	return &Bases{tips: map[string]gates.Tip{
		RepoAPI: {Ref: DefaultRef, Commit: APIBase}, RepoWeb: {Ref: DefaultRef, Commit: WebBase},
	}}
}

// Set moves a repository's default branch.
func (b *Bases) Set(repository, ref, commit string) {
	b.mu.Lock()
	b.tips[repository] = gates.Tip{Ref: ref, Commit: commit}
	b.mu.Unlock()
}

// Remove makes a repository unknown.
func (b *Bases) Remove(repository string) {
	b.mu.Lock()
	delete(b.tips, repository)
	b.mu.Unlock()
}

// Fail makes every read an outage with err; nil restores service.
func (b *Bases) Fail(err error) {
	b.mu.Lock()
	b.err = err
	b.mu.Unlock()
}

// Calls lists the repositories asked for, in call order.
func (b *Bases) Calls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.calls...)
}

// DefaultTips implements gates.Bases.
func (b *Bases) DefaultTips(_ context.Context, _ domain.Scope, repositories []string) (map[string]gates.Tip, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, repositories...)
	if b.err != nil {
		return nil, b.err
	}
	out := map[string]gates.Tip{}
	for _, r := range repositories {
		t, ok := b.tips[r]
		if !ok {
			return nil, gates.ErrNotFound
		}
		out[r] = t
	}
	return out, nil
}

// SourceSetSpec tunes NewSourceSetCase.
type SourceSetSpec struct {
	// ReadOnly makes both repositories read sources (nothing to integrate).
	ReadOnly bool
	// Deployment also binds a deployment of the source set to the gate step.
	Deployment bool
	// EvidenceFrom overrides the provenance of the evidence artifact: the commit
	// of each repository its producing attempt worked from (default: the
	// candidate's commits).
	EvidenceFrom map[string]string
	// EvidenceSingle makes the evidence artifact carry a single repository and
	// commit instead of a source set.
	EvidenceSingle bool
	// NoEvidenceProvenance leaves the artifact without any provenance.
	NoEvidenceProvenance bool
}

// SourceSetCase is a seeded source-set run whose gate step is ready.
type SourceSetCase struct {
	gatescontract.Case
	Digest string
	// Candidate is the candidate commit of each repository.
	Candidate map[string]string
	// Set is the source set provenance of the evidence artifact (zero when the
	// spec overrides it).
	Set gates.SourceSetRef
}

// NewSourceSetCase seeds the work item, the artifacts and a run of the internal
// Git service with two repositories (api written, web read; both read with
// ReadOnly) and the steps
//
//	build (writer, succeeded: published APINew to the task branch of api)
//	  -> test (reader, succeeded: report artifact produced from the heads)
//	  -> review (gate, ready) -> integrate
//
// The candidate is therefore api at APINew and web at WebPinned; the default
// branches are at APIBase and WebBase.
func (h *Harness) NewSourceSetCase(t testing.TB, spec SourceSetSpec) SourceSetCase {
	t.Helper()
	n := h.E.seq.Add(1)
	ctx := context.Background()
	scope := ScopeA
	c := gatescontract.Case{
		Scope: scope, Run: domain.RunID(fmt.Sprintf("run_s%d", n)), Step: "review", Item: domain.WorkItemID(fmt.Sprintf("wi_s%d", n)),
		Evidence: domain.ArtifactID(fmt.Sprintf("art_s%d", n)),
	}
	origin := execution.Origin{Kind: execution.OriginWorkItem, WorkItemID: c.Item}
	writeAccess := gitv1.AccessWrite
	if spec.ReadOnly {
		writeAccess = gitv1.AccessRead
	}
	res := execution.ResolvedSources{BaseURL: "https://git.example.invalid/git", Repositories: []execution.SourceRepository{
		{ID: RepoAPI, Name: "api", URL: "https://git.example.invalid/git/t/p/api.git", DefaultBranch: "main"},
		{ID: RepoWeb, Name: "web", URL: "https://git.example.invalid/git/t/p/web.git", DefaultBranch: "main"},
	}}
	declared := []execution.DeclaredSource{
		{Repository: "api", Path: "backend", Access: writeAccess}, {Repository: "web", Path: "frontend", Access: gitv1.AccessRead},
	}
	sources, err := execution.NewRunSources(origin, c.Run, res, declared, []string{APIPinned, WebPinned}, digestOf([]byte(fmt.Sprintf("declaration %d", n))))
	h.fatal(t, err)
	taskBranch, err := gitv1.TaskBranch(string(c.Item))
	h.fatal(t, err)

	now := h.E.Clock.Now()
	run := &execution.Run{
		Scope: scope, ID: c.Run, Purpose: execution.PurposeWork, WorkItemID: c.Item, DefinitionID: "pd_sources",
		DefinitionDigest: digestOf([]byte(fmt.Sprintf("definition %d", n))), ContentRevision: 1,
		SourceRetention: execution.RetentionConfirmed, Status: execution.RunRunning, Version: 1, Origin: origin, Sources: sources,
	}
	gate := &pipeline.GateSpec{Evidence: []string{"${{ steps.test.outputs.report }}"}}
	outputs := map[string]runtimev1.Value{"report": runtimev1.StringValue(string(c.Evidence))}
	if spec.Deployment {
		c.Deployment = fmt.Sprintf("dep_s%d", n)
		gate.DeploymentID = "${{ steps.test.outputs.deployment }}"
		outputs["deployment"] = runtimev1.StringValue(c.Deployment)
	}
	specs := []pipeline.Step{
		{ID: "build", Kind: pipeline.KindContainer, Workspace: &pipeline.Workspace{Access: pipeline.AccessWrite, From: pipeline.RunSource}},
		{ID: "test", Kind: pipeline.KindContainer, Needs: []string{"build"}, Workspace: &pipeline.Workspace{Access: pipeline.AccessRead, From: "build"},
			Outputs: map[string]pipeline.Output{"report": {Type: pipeline.TypeArtifact}, "deployment": {Type: pipeline.TypeString}}},
		{ID: "review", Kind: pipeline.KindGate, Needs: []string{"test"}, Gate: gate},
		{ID: "integrate", Kind: pipeline.KindContainer, Needs: []string{"review"}},
	}
	agg := &execution.Aggregate{Run: run}
	for i, s := range specs {
		agg.Steps = append(agg.Steps, execution.NewStepRun(run.ID, i, s, now))
	}
	build, test := agg.Step("build"), agg.Step("test")
	build.Status, build.AttemptsStarted = execution.StepSucceeded, 1
	test.Status, test.Outputs, test.AttemptsStarted = execution.StepSucceeded, outputs, 1
	buildAttempt := domain.AttemptID(fmt.Sprintf("att_sb%d", n))
	testAttempt := domain.AttemptID(fmt.Sprintf("att_st%d", n))
	agg.Attempts = append(agg.Attempts,
		&execution.Attempt{Scope: scope, ID: buildAttempt, RunID: run.ID, StepID: "build", Ordinal: 1, Status: execution.AttemptSucceeded,
			Commits: []execution.CommitOutput{{RepositoryID: RepoAPI, Ref: taskBranch, Commit: APINew, PushID: "push_1"}}},
		&execution.Attempt{Scope: scope, ID: testAttempt, RunID: run.ID, StepID: "test", Ordinal: 1, Status: execution.AttemptSucceeded})
	run.MarkClean()
	for _, st := range agg.Steps {
		st.MarkClean()
	}

	out := SourceSetCase{Case: c, Digest: sources.Digest, Candidate: map[string]string{RepoAPI: APINew, RepoWeb: WebPinned}}
	if spec.ReadOnly {
		// A reader publishes nothing: the candidate is the pinned set.
		agg.Attempts[0].Commits = nil
		out.Candidate[RepoAPI] = APIPinned
	}
	worked := out.Candidate
	if spec.EvidenceFrom != nil {
		worked = spec.EvidenceFrom
	}
	commits := make([]gates.SourceCommit, 0, len(worked))
	for repo, commit := range worked {
		commits = append(commits, gates.SourceCommit{RepositoryID: repo, Commit: commit})
	}
	data := []byte(fmt.Sprintf("all tests passed %d\n", n))
	meta := gates.EvidenceMeta{ID: c.Evidence, Scope: scope, RunID: run.ID, StepID: "test", Attempt: testAttempt, Digest: digestOf(data), Usable: true, Size: int64(len(data))}
	switch {
	case spec.NoEvidenceProvenance:
	case spec.EvidenceSingle:
		meta.Repository, meta.Commit = Repository, out.Candidate[RepoAPI]
	default:
		ref, err := gates.NewSourceSetRef(sources.Digest, commits)
		h.fatal(t, err)
		meta.SourceSet = ref
		out.Set = ref
	}
	h.fatal(t, h.E.DB.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if err := workTable.Put(tx, workRow{Scope: scope, ID: c.Item, Revision: 1}); err != nil {
			return err
		}
		if err := artTable.Put(tx, artifactRow{Data: data, Meta: meta}); err != nil {
			return err
		}
		return runTable.Put(tx, runRow{agg: agg})
	}))
	if spec.Deployment {
		deployed := make([]gates.SourceCommit, 0, len(out.Candidate))
		for repo, commit := range out.Candidate {
			deployed = append(deployed, gates.SourceCommit{RepositoryID: repo, Commit: commit})
		}
		ref, err := gates.NewSourceSetRef(sources.Digest, deployed)
		h.fatal(t, err)
		h.fatal(t, h.E.Deployments.SetSourceSet(ctx, scope, c.Deployment, ref, digestOf([]byte("config-1"))))
	}
	return out
}

// PublishToTask records one more commit on the task branch of repository in the
// run's writer attempt: a writer changing the candidate after the gate was
// requested.
func (h *Harness) PublishToTask(t testing.TB, c SourceSetCase, repository, commit string) {
	t.Helper()
	taskBranch, err := gitv1.TaskBranch(string(c.Item))
	h.fatal(t, err)
	h.fatal(t, h.E.DB.Update(context.Background(), c.Scope.TenantID, func(tx memdb.Tx) error {
		row, ok := runTable.Get(tx, projectKey(c.Scope, string(c.Run)))
		if !ok {
			return gates.ErrNotFound
		}
		row.agg.Attempts[0].Commits = append(row.agg.Attempts[0].Commits, execution.CommitOutput{RepositoryID: repository, Ref: taskBranch, Commit: commit, PushID: "push_2"})
		return runTable.Put(tx, row)
	}))
}
