package execution_test

import (
	"context"
	"testing"

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
