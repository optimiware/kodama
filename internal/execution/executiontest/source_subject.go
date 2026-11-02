package executiontest

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/storage"
)

// SourceSubject is one backend under the source-set contract: the run_shared
// subject (the service is composed with the run_shared ports AND the internal Git
// service seams) plus the doubles of those seams.
type SourceSubject struct {
	SharedSubject
	Repos    *FakeSourceSets
	Git      *FakeGitAuthority
	Internal *FakeInternalWorkspaces
	// GitRevoked reports whether the attempt's Git grants were revoked in a
	// committed transaction.
	GitRevoked func(attempt domain.AttemptID) bool
	// DefineSource registers a kodama/v1alpha3 definition (or any other version
	// the fixture's validator accepts) in the shared scope.
	DefineSource func(yaml string) pipeline.Definition
	// Restart builds another Service over the same store, ports and seams.
	RestartSource func() *execution.Service
	// RunSourceRows counts the stored source entries of a run (the persisted pin).
	RunSourceRows func(run domain.RunID) int
	// Run reads a run through the store (with its source set).
	Run func(run domain.RunID) *execution.Run
	// List reads the project's runs through the store.
	List func() []*execution.Run
	// Runtime tracks the workloads the attempts' runtime created.
	Runtime *TrackingRuntime
	// SQL is true for the PostgreSQL subject, whose internal workspace is not the
	// real workspace module (its writer reservation is exercised in memory).
	SQL bool
}

// sourceValidator is the registration validator with both run_shared formats on.
func sourceValidator(t testing.TB) *pipeline.Validator {
	t.Helper()
	v, err := pipeline.NewValidator(pipeline.Config{
		Capabilities:   pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate},
		Profiles:       pipelinetest.Profiles{"coding-v1": true},
		References:     pipelinetest.References{Secrets: map[string]bool{"token": true}, Environments: map[string]bool{"env_staging": true}},
		EnableV1Alpha2: true, EnableV1Alpha3: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// SourceDoc returns a kodama/v1alpha3 document. sources is a YAML flow list of
// {repository, path, access} entries, or "" for a document without sources (the
// implicit default source); steps are YAML flow maps (SharedStep).
func SourceDoc(sources string, steps ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: kodama/v1alpha3\nkind: Pipeline\nmetadata: {name: sources}\nspec:\n  timeoutSeconds: 3600\n")
	if sources != "" {
		b.WriteString("  sources: " + sources + "\n")
	}
	b.WriteString("  steps:\n")
	for _, s := range steps {
		b.WriteString("    - " + s + "\n")
	}
	return b.String()
}

// SourceOptions tune the source fixtures.
type SourceOptions struct {
	// NoGitGrants leaves the Git authority seam unset.
	NoGitGrants bool
	// NoSourceSets leaves the source set seam unset.
	NoSourceSets bool
	// GitGrants replaces the fake Git authority as the service's issuer (PostgreSQL
	// only: a test wiring the real grant store), GitRevoker the store's revoker and
	// Contributions the extra schema it needs.
	GitGrants     execution.GitGrants
	GitRevoker    execpg.AttemptGrantRevoker
	Contributions []storage.Contribution
	// Hooks, WrapStore and AgentPins compose agent steps into the memory service:
	// Hooks enables the agent step kind for dispatch, WrapStore decorates the
	// memory store (the agent admission chain's store) and AgentPins is
	// Dependencies.AgentPins. The zero options keep the container-only service.
	Hooks     execution.Hooks
	WrapStore func(execution.Store) execution.Store
	AgentPins execution.AgentPins
}

// NewSourceMem builds the memory fixture with the internal Git service seams
// composed from test doubles.
func NewSourceMem(t testing.TB, o ...SourceOptions) (*Mem, SourceSubject) {
	t.Helper()
	var opt SourceOptions
	if len(o) > 0 {
		opt = o[0]
	}
	repos, git, internal := NewFakeSourceSets(), NewFakeGitAuthority(), NewFakeInternalWorkspaces("")
	trk := NewTrackingRuntime()
	m, s := NewSharedMem(t, func(mo *MemOptions) {
		mo.Runtime = trk
		mo.SourceSets = sourceSetsIf(repos, !opt.NoSourceSets)
		mo.GitGrants = gitIf(git, !opt.NoGitGrants)
		mo.GitRevoker = git.MemRevoker()
		mo.InternalWorkspaces, mo.InternalPeer = internal, internal
		mo.Hooks, mo.WrapStore, mo.AgentPins = opt.Hooks, opt.WrapStore, opt.AgentPins
	})
	trk.Runtime = m.Manual
	m.Validator = sourceValidator(t)
	internal.WorkspaceID = string(s.Project.Workspace)
	so := execution.Options{ClaimLease: 2 * 1e9, Batch: 10, ResourceFreshness: 20 * 1e9, ResourceRecheck: 1e9, ResourceBackoffBase: 1e9, ResourceBackoffMax: 8 * 1e9, TerminationDelay: -1}
	mk := func() *execution.Service {
		return m.NewService(MemOptions{
			Shared: &execution.SharedConfig{Resources: s.Resources, Policies: s.Policies}, ServiceOptions: &so, Cleanup: AllowCleanup{},
			SourceSets: sourceSetsIf(repos, !opt.NoSourceSets), GitGrants: gitIf(git, !opt.NoGitGrants), InternalWorkspaces: internal,
			Hooks: opt.Hooks, WrapStore: opt.WrapStore, AgentPins: opt.AgentPins,
		})
	}
	ss := SourceSubject{SharedSubject: s, Repos: repos, Git: git, Internal: internal, Runtime: trk, DefineSource: func(yaml string) pipeline.Definition { return m.Define(ScopeA, yaml) }}
	ss.RestartSource = mk
	ss.GitRevoked = func(a domain.AttemptID) bool { return git.MemRevoked(m.DB, ScopeA, a) }
	git.Revoked = ss.GitRevoked
	ss.RunSourceRows = func(run domain.RunID) int {
		r := ss.Run(run)
		if r.Sources == nil {
			return 0
		}
		return len(r.Sources.Entries)
	}
	ss.Run = func(run domain.RunID) *execution.Run {
		agg, err := m.Mem.Get(context.Background(), ScopeA, run)
		must(t, err)
		return agg.Run
	}
	ss.List = func() []*execution.Run {
		runs, err := m.Mem.List(context.Background(), ScopeA, execution.ListQuery{Limit: 100})
		must(t, err)
		return runs
	}
	return m, ss
}

func sourceSetsIf(f *FakeSourceSets, on bool) execution.SourceSets {
	if !on {
		return nil
	}
	return f
}

func gitIf(f *FakeGitAuthority, on bool) execution.GitGrants {
	if !on {
		return nil
	}
	return f
}

// NewSourcePG builds the PostgreSQL fixture with the same seams. It skips in the
// unit layer exactly like New.
func NewSourcePG(t testing.TB, o ...SourceOptions) (*Env, SourceSubject) {
	t.Helper()
	var opt SourceOptions
	if len(o) > 0 {
		opt = o[0]
	}
	repos, git := NewFakeSourceSets(), NewFakeGitAuthority()
	var env *Env
	repos.OnRepository = func(scope domain.Scope, r execution.SourceRepository) {
		env.OwnerExec(`INSERT INTO repositories (tenant_id, project_id, id, name, default_branch, hosting, status, created_by_type, created_by_id, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,'internal','ready','user','u',now(),now())`, string(scope.TenantID), string(scope.ProjectID), r.ID, r.Name, r.DefaultBranch)
	}
	internal := NewFakeInternalWorkspaces("")
	trk := NewTrackingRuntime()
	issuer, revoker := gitIf(git, !opt.NoGitGrants), execpg.AttemptGrantRevoker(git.PGRevoker())
	if opt.GitGrants != nil {
		issuer = opt.GitGrants
	}
	if opt.GitRevoker != nil {
		revoker = opt.GitRevoker
	}
	e, s := NewSharedPG(t, Options{
		SourceSets: sourceSetsIf(repos, !opt.NoSourceSets), GitGrants: issuer, InternalWorkspaces: internal,
		GitRevoker: revoker, Runtime: trk, Contributions: opt.Contributions,
	})
	env = e
	trk.Runtime = e.Manual
	e.Validator = sourceValidator(t)
	internal.WorkspaceID = string(s.Project.Workspace)
	ss := SourceSubject{SharedSubject: s, Repos: repos, Git: git, Internal: internal, Runtime: trk, DefineSource: func(yaml string) pipeline.Definition { return e.Define(ScopeA, yaml) }}
	so := execution.Options{ClaimLease: 2 * 1e9, Batch: 10, ResourceFreshness: 20 * 1e9, ResourceRecheck: 1e9, ResourceBackoffBase: 1e9, ResourceBackoffMax: 8 * 1e9, TerminationDelay: -1}
	ss.RestartSource = func() *execution.Service {
		return e.NewService(Options{
			Shared: &execution.SharedConfig{Resources: s.Resources, Policies: s.Policies}, ServiceOptions: &so, Cleanup: AllowCleanup{},
			SourceSets: sourceSetsIf(repos, !opt.NoSourceSets), GitGrants: issuer, InternalWorkspaces: internal,
		})
	}
	ss.GitRevoked = func(a domain.AttemptID) bool { return e.GitRevoked(ScopeA, a) }
	git.Revoked = ss.GitRevoked
	ss.Run = func(run domain.RunID) *execution.Run {
		agg, err := e.Store.Get(context.Background(), ScopeA, run)
		must(t, err)
		return agg.Run
	}
	ss.RunSourceRows = func(run domain.RunID) int {
		return e.CountTenant(ScopeA.TenantID, `SELECT count(*) FROM run_sources WHERE run_id = $1`, string(run))
	}
	ss.List = func() []*execution.Run {
		runs, err := e.Store.List(context.Background(), ScopeA, execution.ListQuery{Limit: 100})
		must(t, err)
		return runs
	}
	ss.SQL = true
	return e, ss
}
