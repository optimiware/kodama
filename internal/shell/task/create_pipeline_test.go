package task_test

import (
	"context"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// No foreign key backs tasks.pipeline_id, so a plain create is the one
// admission-free path that must read its selected pipeline itself.

func missingPipeline(env *testEnv) {
	env.pipelines.get = func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
		return pipeline.Definition{}, fault.New(fault.NotFound, "pipeline is not present in this scope")
	}
}

// pinnedPipelineRefusal is the create port's answer when the pipeline row its
// unit pins is absent.
func pinnedPipelineRefusal(task.CreateUnit) (task.CreateResult, error) {
	return task.CreateResult{}, fault.New(fault.VersionConflict, "a locked row is missing or out of scope")
}

func TestCreateReadsTheSelectedPipelineInScope(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)

	result, err := env.service.Create(t.Context(), createCommand(actor, "Known pipeline"))
	requireNoError(t, err, "Create")
	if result.TaskID == "" || result.Replayed {
		t.Fatalf("create result = %+v; want a fresh task", result)
	}
	if calls := env.pipelines.calledIDs(); len(calls) != 1 || calls[0] != pipelineID {
		t.Fatalf("pipeline reads = %v; want exactly one read of %q", calls, pipelineID)
	}
	unit := env.ports.lastCreate(t)
	if unit.SelectedPipeline != pipelineID {
		t.Fatalf("unit selected pipeline = %q; want %q", unit.SelectedPipeline, pipelineID)
	}
	// The registry row requires membership only: the existence read is not an
	// authority read and pins no pipeline version.
	requireAuthority(t, unit.Authority, "task.create", authorize.CheckMembership)
	requireNoAuthority(t, unit.Authority, authorize.CheckPipelineVersion)
	if len(unit.Authority) != 1 {
		t.Fatalf("unit authority = %+v; want the membership read only", unit.Authority)
	}
}

func TestCreateRefusesAnUnknownSelectedPipeline(t *testing.T) {
	env := newTestEnv(t)
	missingPipeline(env)
	env.ports.createFn = pinnedPipelineRefusal
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)

	_, err := env.service.Create(t.Context(), createCommand(actor, "Unknown pipeline"))
	requireFaultClass(t, err, fault.NotFound)
	if env.admission.callCount() != 0 {
		t.Fatalf("admission calls = %d; a plain create admits nothing", env.admission.callCount())
	}
	// The unit still reaches the receipt-first port, and only the create port.
	if len(env.ports.creates) != 1 || env.ports.totalCalls() != 1 {
		t.Fatalf("port calls = %d (creates %d); want exactly one create", env.ports.totalCalls(), len(env.ports.creates))
	}
	requireNoAuthority(t, env.ports.lastCreate(t).Authority, authorize.CheckPipelineVersion)
}

func TestCreateRefusesAPipelineInAnotherProject(t *testing.T) {
	env := newTestEnv(t)
	seen := make([]tenancy.Scope, 0, 1)
	env.pipelines.get = func(_ context.Context, scope tenancy.Scope, _ pipeline.PipelineID) (pipeline.Definition, error) {
		seen = append(seen, scope)
		// The pipeline exists, but only in the other project.
		if scope != otherScope() {
			return pipeline.Definition{}, fault.New(fault.NotFound, "pipeline is not present in this scope")
		}
		return pipeline.Definition{ID: pipelineID, Version: 3}, nil
	}
	env.ports.createFn = pinnedPipelineRefusal
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)

	_, err := env.service.Create(t.Context(), createCommand(actor, "Foreign pipeline"))
	requireFaultClass(t, err, fault.NotFound)
	if len(seen) != 1 || seen[0] != actor.Scope {
		t.Fatalf("pipeline read scopes = %+v; want one read in the command scope %+v", seen, actor.Scope)
	}
}

func TestCreateStopsOnAPipelineReadFailure(t *testing.T) {
	env := newTestEnv(t)
	env.pipelines.get = func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
		return pipeline.Definition{}, fault.New(fault.DependencyUnavailable, "store is unavailable")
	}
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)

	_, err := env.service.Create(t.Context(), createCommand(actor, "Unreadable pipeline"))
	requireFaultClass(t, err, fault.DependencyUnavailable)
	requireNoPortCalls(t, env.ports)
}

// A create that committed, lost its response and is retried after the pipeline
// was deleted converges on the original receipt instead of failing not_found.
func TestCreateReplaysAfterTheSelectedPipelineWasDeleted(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	command := createCommand(actor, "Lost response")

	first, err := env.service.Create(t.Context(), command)
	requireNoError(t, err, "first Create")

	missingPipeline(env)
	env.ports.createFn = func(task.CreateUnit) (task.CreateResult, error) {
		return task.CreateResult{TaskID: first.TaskID, ContentVersion: 1, LifecycleVersion: 1, Replayed: true}, nil
	}
	retried, err := env.service.Create(t.Context(), command)
	requireNoError(t, err, "retried Create")
	if !retried.Replayed || retried.TaskID != first.TaskID {
		t.Fatalf("retried create = %+v; want the replayed original task %q", retried, first.TaskID)
	}
	original, retry := env.ports.creates[0], env.ports.creates[1]
	if retry.Receipt.Key != original.Receipt.Key || retry.Receipt.ContentHash != original.Receipt.ContentHash {
		t.Fatalf("retry receipt identity = %+v/%q; want the original %+v/%q",
			retry.Receipt.Key, retry.Receipt.ContentHash, original.Receipt.Key, original.Receipt.ContentHash)
	}
}

// Only the pinned-row refusal is reported as the missing pipeline; any other
// port answer keeps its own class.
func TestCreateWithAMissingPipelineKeepsOtherPortRefusals(t *testing.T) {
	env := newTestEnv(t)
	missingPipeline(env)
	env.ports.createFn = func(task.CreateUnit) (task.CreateResult, error) {
		return task.CreateResult{}, fault.New(fault.ChangedIdempotencyContent, "a receipt with the same caller key already holds different request content")
	}
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)

	_, err := env.service.Create(t.Context(), createCommand(actor, "Changed content"))
	requireFaultClass(t, err, fault.ChangedIdempotencyContent)
}

func TestCreateWithoutAPipelineReadsNoPipeline(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	command := createCommand(actor, "No pipeline")
	command.PipelineID = ""

	result, err := env.service.Create(t.Context(), command)
	requireNoError(t, err, "Create")
	if result.TaskID == "" {
		t.Fatal("create result has no task id")
	}
	if env.pipelines.callCount() != 0 {
		t.Fatalf("pipeline reads = %v; a create without a selection reads none", env.pipelines.calledIDs())
	}
	if unit := env.ports.lastCreate(t); unit.SelectedPipeline != "" {
		t.Fatalf("unit selected pipeline = %q; want none", unit.SelectedPipeline)
	}
}

// create_and_start reads the pipeline through admission, which pins its
// version; it takes no second existence read.
func TestCreateAndStartReadsThePipelineThroughAdmissionOnly(t *testing.T) {
	env := newTestEnv(t)
	actor := humanActor(engineerPrincipal, tenancy.RoleEngineer)
	command := createCommand(actor, "Run release")
	command.Start = true

	_, err := env.service.Create(t.Context(), command)
	requireNoError(t, err, "CreateAndStart")
	if env.pipelines.callCount() != 0 {
		t.Fatalf("pipeline reads = %v; create_and_start reads through admission only", env.pipelines.calledIDs())
	}
	if env.admission.callCount() != 1 {
		t.Fatalf("admission calls = %d; want exactly one", env.admission.callCount())
	}
	requireAuthority(t, env.ports.lastCreate(t).Authority, "task.create_and_start", authorize.CheckPipelineVersion)
}
