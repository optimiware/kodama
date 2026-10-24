package memory_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const (
	readRun  = "workspace: {access: read, from: run}"
	writeRun = "workspace: {access: write, from: run}"
	// retryInfra retries infrastructure failures, with a persisted backoff.
	retryInfra = "retry: {maxAttempts: 3, backoffSeconds: 30, on: [infrastructure]}"
)

var scopeA = executiontest.ScopeA

// fixture is a granted memory fixture with one project.
func fixture(t *testing.T, opts ...executiontest.MemOptions) (*executiontest.Mem, executiontest.Project) {
	t.Helper()
	m := executiontest.NewMem(t, opts...)
	m.GrantAll()
	return m, m.NewProject(scopeA)
}

// define registers a document of container steps in the project's scope.
func define(m *executiontest.Mem, steps ...string) pipeline.Definition {
	m.T.Helper()
	return m.Define(scopeA, pipelinetest.Doc(steps...))
}

func job(id string, extra ...string) string { return pipelinetest.Container(id, extra...) }

// held counts the tenant's capacity reservations that are not released.
func held(m *executiontest.Mem) int {
	m.T.Helper()
	rs, err := m.Mem.Reservations(context.Background(), scopeA.TenantID)
	if err != nil {
		m.T.Fatal(err)
	}
	n := 0
	for _, r := range rs {
		if r.Held() {
			n++
		}
	}
	return n
}

// writerOf reports which attempt holds the project's writer slot ("" if free).
func writerOf(m *executiontest.Mem, p executiontest.Project) domain.AttemptID {
	m.T.Helper()
	ws, err := m.World.WorkspaceOf(context.Background(), p.Scope, string(p.Workspace))
	if err != nil {
		m.T.Fatal(err)
	}
	return ws.Writer
}

func attempts(m *executiontest.Mem, run *execution.Run) int { return len(m.Get(run).Attempts) }

// infraFail reports an infrastructure failure the executor can prove never
// launched the command, and confirms the workload gone.
func infraFail(t *testing.T, m *executiontest.Mem, in *execution.AttemptSpec) {
	t.Helper()
	if err := m.Observe(in, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchNotStarted}); err != nil {
		t.Fatal(err)
	}
	if err := m.Terminate(in, execution.LaunchNotStarted); err != nil {
		t.Fatal(err)
	}
}

// ambiguous reports an infrastructure failure whose launch the executor cannot
// establish, then confirms the workload gone without evidence: the effect is
// unknown.
func ambiguous(t *testing.T, m *executiontest.Mem, in *execution.AttemptSpec) {
	t.Helper()
	if err := m.Observe(in, execution.Observation{Kind: execution.ObservedFailed, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := m.Terminate(in, execution.LaunchUnknown); err != nil {
		t.Fatal(err)
	}
}

func versionOf(n int) domain.Version { return domain.Version(n) }
