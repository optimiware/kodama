//go:build integration

package lifecycle

// Same-process fixture isolation. Two real factory fixtures live in one test
// process. Closing one must remove only that fixture's workload containers and
// workspace volumes; the sibling fixture's workload, volume contents and served
// API must remain usable. The labels the factory adds for fixture selection sit
// alongside the process-wide run labels that crash recovery sweeps.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/test/support/factory"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// heldFixtureWorkload is one live fixture workload retained for the whole test.
type heldFixtureWorkload struct {
	executionID string
	workspaceID string
	container   string
}

// TestFactoryClosePreservesSiblingFixture proves closing one factory fixture is
// safe while another fixture is alive in the same test process. Both fixtures
// positively observe a running workload and a retained workspace volume before
// either is closed; then only the closed fixture's resources disappear.
func TestFactoryClosePreservesSiblingFixture(t *testing.T) {
	first := factory.Start(t)
	second := factory.Start(t)
	if first.FixtureID() == second.FixtureID() {
		t.Fatalf("two fixtures in one process share fixture id %q", first.FixtureID())
	}

	firstWorkload := startHeldFixtureWorkload(t, first, "fixture-a")
	secondWorkload := startHeldFixtureWorkload(t, second, "fixture-b")
	if firstWorkload.executionID == secondWorkload.executionID {
		t.Fatalf("two fixtures share execution %q", firstWorkload.executionID)
	}
	if firstWorkload.workspaceID == secondWorkload.workspaceID {
		t.Fatalf("two fixtures share workspace %q", firstWorkload.workspaceID)
	}
	if firstWorkload.container == secondWorkload.container {
		t.Fatalf("two fixtures share workload container %q", firstWorkload.container)
	}

	// Positive observation of both fixtures before either close: both workloads
	// run, both workspace volumes exist and both serve their own contents.
	assertHeldFixtureWorkload(t, first, firstWorkload, "fixture-a")
	assertHeldFixtureWorkload(t, second, secondWorkload, "fixture-b")
	assertFixtureOwnsResources(t, first, "first")
	assertFixtureOwnsResources(t, second, "second")

	first.Close()

	// The closed fixture's containers and volumes are gone. The process-wide
	// execution label probe is deliberately not fixture-filtered: it catches a
	// container left behind because the fixture label was missing from the
	// container itself.
	ctx := t.Context()
	if containers, err := first.FixtureContainers(ctx); err != nil {
		t.Fatalf("list closed fixture containers: %v", err)
	} else if len(containers) != 0 {
		t.Fatalf("closed fixture left containers %v", containers)
	}
	if volumes, err := first.FixtureVolumes(ctx); err != nil {
		t.Fatalf("list closed fixture volumes: %v", err)
	} else if len(volumes) != 0 {
		t.Fatalf("closed fixture left volumes %v", volumes)
	}
	if containers, err := runtimesupport.ContainerNamesByLabel(ctx, runtimepodman.LabelExecution, firstWorkload.executionID); err != nil {
		t.Fatalf("list closed execution containers by process label: %v", err)
	} else if len(containers) != 0 {
		t.Fatalf("closed fixture left run-labelled execution containers %v", containers)
	}
	if present, err := first.WorkspaceVolumePresent(ctx, firstWorkload.workspaceID); err != nil {
		t.Fatalf("probe closed fixture workspace volume: %v", err)
	} else if present {
		t.Fatalf("closed fixture retained workspace volume %s", firstWorkload.workspaceID)
	}

	// The sibling fixture is untouched: its workload still runs, its workspace
	// volume still serves its contents and its served API still answers.
	assertHeldFixtureWorkload(t, second, secondWorkload, "fixture-b")
	assertFixtureOwnsResources(t, second, "second")
	running, err := second.RunningExecutionContainer(ctx, secondWorkload.executionID)
	if err != nil {
		t.Fatalf("list sibling running workload: %v", err)
	}
	if running != secondWorkload.container {
		t.Fatalf("sibling running container = %q; want %q", running, secondWorkload.container)
	}
	present, err := second.WorkspaceVolumePresent(ctx, secondWorkload.workspaceID)
	if err != nil {
		t.Fatalf("probe sibling workspace volume: %v", err)
	}
	if !present {
		t.Fatalf("sibling workspace volume %s disappeared after the other fixture closed", secondWorkload.workspaceID)
	}
	result, publicError := second.REST(second.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: secondWorkload.executionID,
	}, nil, "")
	if publicError.Code != "" {
		t.Fatalf("sibling served API refused execution.inspect after the other fixture closed: code=%q message=%q",
			publicError.Code, publicError.Message)
	}
	var inspected api.ExecutionInspectResult
	second.Decode(result, &inspected)
	if inspected.State != "running" {
		t.Fatalf("sibling execution state after the other fixture closed = %q; want running", inspected.State)
	}
}

// startHeldFixtureWorkload starts one pipeline whose action writes a marker to
// the workspace and then blocks until a release file appears. The workload stays
// live while the other fixture is closed.
func startHeldFixtureWorkload(t *testing.T, harness *factory.Harness, marker string) heldFixtureWorkload {
	t.Helper()
	document := marshalPipeline(t, pipelineDoc{Actions: []pipelineActionDoc{{
		Name:  "hold",
		Image: harness.Image.Reference,
		Commands: []string{
			fmt.Sprintf("printf '%s\\n' > /workspace/marker", marker),
			"while [ ! -f /workspace/release ]; do sleep 1; done",
		},
	}}})
	applied := applyPipeline(t, harness, "fixture-isolation-"+marker, document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "start-"+marker)

	var container string
	ctx := t.Context()
	harness.Eventually(t, 2*time.Minute, func() bool {
		result, publicError := harness.REST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: started.ExecutionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var execution api.ExecutionInspectResult
		harness.Decode(result, &execution)
		if execution.State != "running" {
			return false
		}
		running, err := harness.RunningExecutionContainer(ctx, started.ExecutionID)
		if err != nil || running == "" {
			return false
		}
		container = running
		return true
	})

	harness.Eventually(t, 30*time.Second, func() bool {
		content, err := harness.ExecContainer(ctx, container, "cat", "/workspace/marker")
		return err == nil && strings.TrimSpace(content) == marker
	})
	workspaceID, err := harness.ExecutionWorkspaceID(ctx, started.ExecutionID)
	if err != nil {
		t.Fatalf("read held workload workspace id: %v", err)
	}
	present, err := harness.WorkspaceVolumePresent(ctx, workspaceID)
	if err != nil {
		t.Fatalf("probe held workload workspace volume: %v", err)
	}
	if !present {
		t.Fatalf("held workload workspace volume %s does not exist", workspaceID)
	}
	return heldFixtureWorkload{executionID: started.ExecutionID, workspaceID: workspaceID, container: container}
}

// assertHeldFixtureWorkload proves one fixture still serves its live workload
// and workspace contents.
func assertHeldFixtureWorkload(t *testing.T, harness *factory.Harness, workload heldFixtureWorkload, marker string) {
	t.Helper()
	ctx := t.Context()
	running, err := harness.RunningExecutionContainer(ctx, workload.executionID)
	if err != nil {
		t.Fatalf("list running workload for %s: %v", marker, err)
	}
	if running != workload.container {
		t.Fatalf("running workload container for %s = %q; want %q", marker, running, workload.container)
	}
	content, err := harness.ExecContainer(ctx, workload.container, "cat", "/workspace/marker")
	if err != nil {
		t.Fatalf("read %s workspace contents through its workload: %v", marker, err)
	}
	if strings.TrimSpace(content) != marker {
		t.Fatalf("%s workspace marker = %q; want %q", marker, strings.TrimSpace(content), marker)
	}
	present, err := harness.WorkspaceVolumePresent(ctx, workload.workspaceID)
	if err != nil {
		t.Fatalf("probe %s workspace volume: %v", marker, err)
	}
	if !present {
		t.Fatalf("%s workspace volume %s is absent while its workload runs", marker, workload.workspaceID)
	}
}

// assertFixtureOwnsResources proves the fixture's own selector observes its live
// workload container and workspace volume. It is the positive control for the
// empty results asserted after a sibling fixture closes.
func assertFixtureOwnsResources(t *testing.T, harness *factory.Harness, name string) {
	t.Helper()
	containers, err := harness.FixtureContainers(t.Context())
	if err != nil {
		t.Fatalf("list %s fixture containers: %v", name, err)
	}
	if len(containers) == 0 {
		t.Fatalf("%s fixture selector observes no containers while its workload runs", name)
	}
	volumes, err := harness.FixtureVolumes(t.Context())
	if err != nil {
		t.Fatalf("list %s fixture volumes: %v", name, err)
	}
	if len(volumes) == 0 {
		t.Fatalf("%s fixture selector observes no volumes while its workspace is retained", name)
	}
}
