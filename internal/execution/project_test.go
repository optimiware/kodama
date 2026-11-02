package execution_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

func TestProjectRunAdmitsWithoutWorkItemOrWorkspace(t *testing.T) {
	m, subject := executiontest.NewSharedMem(t)
	def := m.Define(executiontest.ScopeA, `apiVersion: kodama/v1alpha3
kind: Pipeline
metadata: {name: source-free}
spec:
  timeoutSeconds: 600
  steps:
    - id: check
      kind: container
      image: `+pipelinetest.Image+`
      command: [/bin/true]
      timeoutSeconds: 60
`)
	svc := subject.Restart()
	run, err := svc.AdmitProject(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_project"}, execution.ProjectAdmitCommand{
		Scope: executiontest.ScopeA, DefinitionID: def.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Purpose != execution.PurposeProject || run.WorkItemID != "" || run.WorkspaceID != "" ||
		run.DeploymentID != "" || run.SourceRetention != execution.RetentionNotRequired || run.Shared == nil {
		t.Fatalf("project run = %+v", run)
	}
}

type fakeProjectSources struct {
	commit string
	calls  int
}

func (f *fakeProjectSources) VerifyProject(_ context.Context, _ domain.Scope, commit string) (execution.SourceInfo, error) {
	f.calls++
	f.commit = commit
	return execution.SourceInfo{RepositoryURL: "https://git.example.invalid/project.git", Branch: "refs/heads/main", AcceptedTip: commit}, nil
}

func TestProjectRunPinsAnExactProjectCommit(t *testing.T) {
	sources := &fakeProjectSources{}
	m, subject := executiontest.NewSharedMem(t, func(o *executiontest.MemOptions) { o.ProjectSources = sources })
	def := m.Define(executiontest.ScopeA, `apiVersion: kodama/v1alpha3
kind: Pipeline
metadata: {name: source-backed}
spec:
  timeoutSeconds: 600
  steps:
    - id: check
      kind: container
      image: `+pipelinetest.Image+`
      command: [/bin/true]
      workspace: {access: read}
      timeoutSeconds: 60
`)
	svc := subject.Restart()
	commit := "1111111111111111111111111111111111111111"
	run, err := svc.AdmitProject(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_source_project"}, execution.ProjectAdmitCommand{
		Scope: executiontest.ScopeA, DefinitionID: def.ID, SourceCommit: commit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.SourceCommit != commit || run.WorkspaceID != "" || run.Shared == nil || !run.Shared.SourceBacked || sources.calls != 1 {
		t.Fatalf("run = %+v sources=%+v", run, sources)
	}
}
