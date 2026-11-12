package execution_test

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestStartReturnsCommittedExecutionVersion pins the additive result field: a
// start reports the committed EXECUTION version (always 1 for a fresh
// execution), never the pipeline version, for both selection shapes.
func TestStartReturnsCommittedExecutionVersion(t *testing.T) {
	scope := projectScope("tenant-1", "project-1")
	actor := humanActor("tenant-1", "project-1", "principal-1", tenancy.RoleEngineer)

	t.Run("stored start reports the execution version, not the pipeline version", func(t *testing.T) {
		fixture := newTestService(t)
		result, err := fixture.service.Start(context.Background(), execution.StartCommand{
			Actor:     actor,
			Scope:     scope,
			Admission: pipelineAdmission(t, []byte("actions:\n  - name: build\n    image: registry.example.com/tool:1\n"), 3, admittedContainerAction(0, "build", time.Minute)),
			CallerKey: "version-stored",
			Content:   receiptContent(`{"pipeline_id":"pipeline-1","pipeline_version":3,"inputs":{}}`),
		})
		requireNoError(t, err, "stored Start")
		var _ uint64 = result.ExecutionVersion
		if result.ExecutionVersion != 1 {
			t.Fatalf("stored result execution version = %d; want 1", result.ExecutionVersion)
		}
		if result.PipelineVersion != 3 {
			t.Fatalf("stored result pipeline version = %d; want 3", result.PipelineVersion)
		}
		if result.ExecutionVersion == result.PipelineVersion {
			t.Fatal("stored result conflated the execution version with the pipeline version")
		}
	})

	t.Run("inline start reports the execution version with no pipeline version", func(t *testing.T) {
		fixture := newTestService(t)
		result, err := fixture.service.Start(context.Background(), execution.StartCommand{
			Actor:     actor,
			Scope:     scope,
			Admission: inlineAdmission(t, []byte("actions:\n  - name: build\n    image: registry.example.com/tool:1\n"), admittedContainerAction(0, "build", time.Minute)),
			CallerKey: "version-inline",
			Content:   receiptContent(`{"yaml_sha256":"sha256:inline","inputs":{}}`),
		})
		requireNoError(t, err, "inline Start")
		if result.ExecutionVersion != 1 {
			t.Fatalf("inline result execution version = %d; want 1", result.ExecutionVersion)
		}
		if result.PipelineVersion != 0 {
			t.Fatalf("inline result pipeline version = %d; want 0", result.PipelineVersion)
		}
	})
}
