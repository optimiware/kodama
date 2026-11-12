//go:build integration

package lifecycle

import (
	"fmt"
	"strings"
	"testing"

	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestOutputsStagedUntilSuccess proves the KODAMA.md rule that declared
// values remain staged until runtime-confirmed success and a failure discards
// them (KODAMA.md line 100, AC 12):
//
//   - an action submits its declared output through the mounted execution
//     credential and then exits non-zero;
//   - the execution fails with failure kind runtime/workload_exited carrying
//     the post-submission exit status, which proves the submission was accepted
//     before the workload failed rather than refused or never delivered;
//   - the action is failed;
//   - the submitted output is neither promoted into the action observation nor
//     left in the durable staging columns;
//   - the following action that consumes that output is cancelled before it is
//     ever activated: no launch intent, no container, and no materialized
//     /kodama/outputs/<action>/ directory in any workload activation root.
//
// The positive promotion path is covered by TestAdmissionParity; this test is
// the failing-path counterpart that would fail if staged outputs were published
// on failure.
func TestOutputsStagedUntilSuccess(t *testing.T) {
	harness := factory.Start(t)
	ctx := t.Context()

	const (
		stagedValue = "staged-but-never-promoted"
		// failureStatus is the workload exit status after the accepted
		// submission. submitScript exits 1 on any refusal or delivery failure,
		// so requiring this exact status makes the test fail when nothing was
		// ever staged: a refused or unsent submission cannot masquerade as the
		// post-submission failure this test is about.
		failureStatus = 7
	)
	document := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{
			{
				Name:    "produce",
				Image:   harness.Image.Reference,
				Outputs: []string{"artifact"},
				Commands: []string{
					"ARTIFACT=" + stagedValue,
					submitScript("artifact", "ARTIFACT"),
					// The submission above was accepted, then the workload
					// fails. A correct composition must discard the staging.
					fmt.Sprintf("exit %d", failureStatus),
				},
			},
			{
				Name:  "consume",
				Image: harness.Image.Reference,
				Commands: []string{
					"test -f /kodama/outputs/produce/artifact",
					"FINAL=$(cat /kodama/outputs/produce/artifact)",
				},
			},
		},
	})

	applied := applyPipeline(t, harness, "staged", document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "start-staged")
	execution := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)

	if execution.State != "failed" {
		t.Fatalf("execution state = %q; want failed (failure=%+v)", execution.State, execution.Failure)
	}
	if execution.Failure == nil || execution.Failure.Kind != "runtime" || execution.Failure.Reason != "workload_exited" {
		t.Fatalf("execution failure = %+v; want kind runtime reason workload_exited", execution.Failure)
	}
	// The failure detail carries the workload's own exit status. Requiring the
	// post-submission status keeps the test about discarding an accepted
	// submission: a refused or never-delivered submission exits 1 inside
	// submitScript and would fail here instead of passing vacuously.
	if want := fmt.Sprintf("status %d", failureStatus); !strings.Contains(execution.Failure.Detail, want) {
		t.Fatalf("execution failure detail = %q; want it to contain %q so the failing action ran after an accepted submission",
			execution.Failure.Detail, want)
	}

	produce := actionByName(t, execution, "produce")
	if produce.State != "failed" {
		t.Fatalf("produce action state = %q; want failed", produce.State)
	}
	if len(produce.Outputs) != 0 {
		t.Fatalf("failed action published outputs %+v; staged values must be discarded", produce.Outputs)
	}

	// The durable row must show the same thing: no promoted response outputs,
	// no remaining staging, and the staging flag cleared.
	produceRow, err := harness.ActionRow(ctx, execution.ExecutionID, "produce")
	if err != nil {
		t.Fatalf("read produce action row: %v", err)
	}
	if produceRow.State != "failed" {
		t.Fatalf("produce action row state = %q; want failed", produceRow.State)
	}
	if produceRow.Outputs != "" {
		t.Fatalf("produce action row outputs = %q; want NULL after failure", produceRow.Outputs)
	}
	if produceRow.StagedOutputs != "" || produceRow.StagedOutputsSet {
		t.Fatalf("produce action row kept staged outputs %q (set=%v); want cleared",
			produceRow.StagedOutputs, produceRow.StagedOutputsSet)
	}

	// The consumer of the discarded output never ran.
	consume := actionByName(t, execution, "consume")
	if consume.State != "cancelled" {
		t.Fatalf("consume action state = %q; want cancelled", consume.State)
	}
	if consume.ActivatedAt != nil {
		t.Fatalf("consume action was activated at %v; it must never run", consume.ActivatedAt)
	}
	consumeRow, err := harness.ActionRow(ctx, execution.ExecutionID, "consume")
	if err != nil {
		t.Fatalf("read consume action row: %v", err)
	}
	if consumeRow.State != "cancelled" {
		t.Fatalf("consume action row state = %q; want cancelled", consumeRow.State)
	}
	if consumeRow.WorkloadID != "" {
		t.Fatalf("consume action has workload id %q; a cancelled action must have no launch intent",
			consumeRow.WorkloadID)
	}
	containers, err := harness.Containers(ctx, runtimepodman.LabelAction, consume.ActionID)
	if err != nil {
		t.Fatalf("list consume containers: %v", err)
	}
	if len(containers) != 0 {
		t.Fatalf("cancelled action had containers %v", containers)
	}

	// Corroborating filesystem evidence: no /kodama/outputs/produce/ was
	// materialized into any workload root, so the consumer never received the
	// discarded value. The adapter removes the workload root during settlement,
	// so an empty result is not independent proof by itself; the durable row,
	// the cancelled action, its absent workload id and the absent container
	// above are the primary proofs. Harness.OutputMaterializations is
	// positively controlled by TestOutputMaterializationsSeesMaterialization,
	// which proves the helper reports a directory laid out exactly like this.
	materialized, err := harness.OutputMaterializations("produce")
	if err != nil {
		t.Fatalf("inspect output materializations: %v", err)
	}
	if len(materialized) != 0 {
		t.Fatalf("failed action outputs were materialized at %v; want none", materialized)
	}

	// The rejected retired literals are not accepted by this served edge.
	harness.AssertRetiredFixtureTokensRefused(t)
}
