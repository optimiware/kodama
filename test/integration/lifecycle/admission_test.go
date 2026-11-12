//go:build integration

package lifecycle

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/id"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	pipelineSecretName  = "PIPELINE_SECRET"
	pipelineSecretValue = "fixture-pipeline-secret-01"
	deniedSecretName    = "DENIED_SECRET"
	deniedSecretValue   = "fixture-denied-secret-02"

	seedValue     = "seed-value"
	artifactValue = "built:" + seedValue
	finalValue    = "consumed:" + artifactValue
)

// TestAdmissionParity proves the in-scope admission/execution parity on the
// real composition:
//
//   - an API-posted task-free pipeline runs to completion. Inline YAML on the
//     task-free executions route is covered by TestInlineTaskFreeStart; this
//     test keeps the stored-pipeline admission path.
//   - the admitted YAML bytes and SHA-256 are immutable and a later pipeline
//     edit changes neither the run nor its actions;
//   - ordered action materialization (inputs, earlier succeeded outputs and the
//     shared workspace) and declared outputs submitted by the workload with its
//     mounted execution credential are promoted after runtime success (the
//     failure/discard counterpart is proved by TestOutputsStagedUntilSuccess);
//   - REST, MCP and the public Go client invoke equivalent start/inspect
//     operations and share one receipt;
//   - a lost response after the server committed the start replays to exactly
//     one execution.
func TestAdmissionParity(t *testing.T) {
	harness := factory.Start(t,
		factory.WithSecretRules(secret.NameRule{
			Scope: tenancy.Scope{
				TenantID:  id.TenantID(factory.TenantID),
				ProjectID: id.ProjectID(factory.ProjectID),
			},
			Names: []id.SecretName{pipelineSecretName},
		}),
		factory.WithSecrets(map[string]string{
			pipelineSecretName: pipelineSecretValue,
		}),
	)
	ctx := t.Context()

	originalYAML := marshalPipeline(t, pipelineDoc{
		Inputs: []string{"seed"},
		Actions: []pipelineActionDoc{
			{
				Name:    "prepare",
				Image:   harness.Image.Reference,
				Outputs: []string{"artifact"},
				Secrets: []string{pipelineSecretName},
				Commands: []string{
					prepareChecks(seedValue, len(pipelineSecretValue), pipelineSecretValue[:8]),
					submitScript("artifact", "ARTIFACT"),
				},
			},
			{
				Name:    "consume",
				Image:   harness.Image.Reference,
				Outputs: []string{"final"},
				Commands: []string{
					consumeChecks(artifactValue),
					submitScript("final", "FINAL"),
				},
			},
		},
	})

	applied := applyPipeline(t, harness, "admission", originalYAML)
	first := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID,
		map[string]string{"seed": seedValue}, "start-admission")

	firstExecution := waitExecution(t, harness, harness.ProjectToken, first.ExecutionID)
	requireSucceeded(t, firstExecution)
	if firstExecution.TaskID != "" {
		t.Fatalf("task-free execution reported task_id %q", firstExecution.TaskID)
	}
	if len(firstExecution.Actions) != 2 {
		t.Fatalf("execution has %d actions; want 2", len(firstExecution.Actions))
	}
	prepare := actionByName(t, firstExecution, "prepare")
	consume := actionByName(t, firstExecution, "consume")
	if prepare.Index != 0 || consume.Index != 1 {
		t.Fatalf("action order prepare=%d consume=%d; want 0/1", prepare.Index, consume.Index)
	}
	if prepare.State != "succeeded" || consume.State != "succeeded" {
		t.Fatalf("action states prepare=%q consume=%q", prepare.State, consume.State)
	}
	if prepare.Outputs["artifact"] != artifactValue {
		t.Fatalf("prepare artifact = %q; want %q", prepare.Outputs["artifact"], artifactValue)
	}
	if consume.Outputs["final"] != finalValue {
		t.Fatalf("consume final = %q; want %q", consume.Outputs["final"], finalValue)
	}

	// Immutable admitted YAML bytes and hash.
	admittedYAML, admittedSHA, err := harness.ExecutionRow(ctx, first.ExecutionID)
	if err != nil {
		t.Fatalf("read admitted execution row: %v", err)
	}
	if !bytes.Equal(admittedYAML, originalYAML) {
		t.Fatalf("admitted yaml differs from the posted bytes:\nposted=%s\nstored=%s", originalYAML, admittedYAML)
	}
	wantSHA := sha256.Sum256(originalYAML)
	if !bytes.Equal(admittedSHA, wantSHA[:]) {
		t.Fatalf("admitted yaml_sha256 = %x; want %x", admittedSHA, wantSHA)
	}
	if read := readPipeline(t, harness, applied.PipelineID); !bytes.Equal(read.YAML, originalYAML) {
		t.Fatalf("pipeline.read bytes differ from the posted bytes")
	}

	// REST start and the MCP replay share one receipt and one execution. The
	// replay keeps the original start's expected pipeline version.
	mcpReplay := harness.MustMCP(harness.ProjectToken, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: applied.PipelineID,
		Inputs:     map[string]string{"seed": seedValue},
	}, &applied.Version, "start-admission")
	var replayed api.PipelineStartResult
	harness.Decode(mcpReplay, &replayed)
	if replayed.ExecutionID != first.ExecutionID {
		t.Fatalf("MCP replay execution = %q; want %q", replayed.ExecutionID, first.ExecutionID)
	}
	receipts, err := harness.ReceiptCount(ctx, "start-admission")
	if err != nil {
		t.Fatalf("count start receipts: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("start receipt count = %d; want 1", receipts)
	}

	// REST/MCP/client parity for inspect.
	restInspect := harness.MustREST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: first.ExecutionID,
	}, nil, "")
	mcpInspect := harness.MustMCP(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: first.ExecutionID,
	}, nil, "")
	clientInspect := inspectViaClient(t, harness.Client(harness.ProjectToken), first.ExecutionID)
	assertSameBody(t, restInspect, mcpInspect)
	assertSameBody(t, restInspect, clientInspect)

	// A pipeline edit cannot mutate the already admitted run.
	updatedYAML := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:     "updated",
			Image:    harness.Image.Reference,
			Commands: []string{"echo updated"},
		}},
	})
	updated := updatePipeline(t, harness, applied.PipelineID, "admission", updatedYAML, applied.Version)
	if updated.Version <= applied.Version {
		t.Fatalf("pipeline update version = %d; want > %d", updated.Version, applied.Version)
	}
	if read := readPipeline(t, harness, applied.PipelineID); !bytes.Equal(read.YAML, updatedYAML) {
		t.Fatalf("pipeline.read bytes differ from the updated bytes")
	}
	afterEditYAML, afterEditSHA, err := harness.ExecutionRow(ctx, first.ExecutionID)
	if err != nil {
		t.Fatalf("read admitted execution row after edit: %v", err)
	}
	if !bytes.Equal(afterEditYAML, originalYAML) || !bytes.Equal(afterEditSHA, wantSHA[:]) {
		t.Fatalf("a pipeline edit mutated the admitted run")
	}
	afterEdit := inspectExecution(t, harness, harness.ProjectToken, first.ExecutionID)
	if len(afterEdit.Actions) != 2 ||
		afterEdit.Actions[0].Name != "prepare" || afterEdit.Actions[1].Name != "consume" {
		t.Fatalf("a pipeline edit mutated the admitted actions: %+v", afterEdit.Actions)
	}
	if afterEdit.Actions[0].Outputs["artifact"] != artifactValue ||
		afterEdit.Actions[1].Outputs["final"] != finalValue {
		t.Fatalf("a pipeline edit mutated recorded outputs: %+v", afterEdit.Actions)
	}

	// A new start of the updated definition uses the new exact bytes.
	second := startViaClient(t, harness.Client(harness.ProjectToken), applied.PipelineID, nil, "start-admission-v2")
	if second.ExecutionID == first.ExecutionID {
		t.Fatalf("a distinct idempotency key returned the earlier execution %q", second.ExecutionID)
	}
	secondExecution := waitExecution(t, harness, harness.ProjectToken, second.ExecutionID)
	requireSucceeded(t, secondExecution)
	secondYAML, secondSHA, err := harness.ExecutionRow(ctx, second.ExecutionID)
	if err != nil {
		t.Fatalf("read second execution row: %v", err)
	}
	if !bytes.Equal(secondYAML, updatedYAML) {
		t.Fatalf("second admitted yaml differs from the updated bytes")
	}
	wantSecondSHA := sha256.Sum256(updatedYAML)
	if !bytes.Equal(secondSHA, wantSecondSHA[:]) {
		t.Fatalf("second yaml_sha256 = %x; want %x", secondSHA, wantSecondSHA)
	}
	secondInspect := inspectExecution(t, harness, harness.ProjectToken, second.ExecutionID)
	if len(secondInspect.Actions) != 1 || secondInspect.Actions[0].Name != "updated" {
		t.Fatalf("second execution actions = %+v; want the updated action", secondInspect.Actions)
	}

	// A lost response after the server committed the start replays to exactly
	// one execution through the public client retry.
	droppedYAML := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:     "dropped",
			Image:    harness.Image.Reference,
			Commands: []string{"echo dropped"},
		}},
	})
	dropped := applyPipeline(t, harness, "dropped", droppedYAML)
	droppedClient := harness.Client(harness.ProjectToken)
	droppedClient.RoundTripper = &dropOnceRoundTripper{base: http.DefaultTransport}
	droppedClient.Retry = client.RetryPolicy{
		MaxAttempts: 2,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
	}
	droppedStart := startViaClient(t, droppedClient, dropped.PipelineID, nil, "start-dropped")
	droppedExecution := waitExecution(t, harness, harness.ProjectToken, droppedStart.ExecutionID)
	requireSucceeded(t, droppedExecution)

	count, err := harness.ExecutionCount(ctx, dropped.PipelineID)
	if err != nil {
		t.Fatalf("count dropped executions: %v", err)
	}
	if count != 1 {
		t.Fatalf("dropped-response replay created %d executions; want 1", count)
	}
	receipts, err = harness.ReceiptCount(ctx, "start-dropped")
	if err != nil {
		t.Fatalf("count dropped receipts: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("dropped-response receipt count = %d; want 1", receipts)
	}
}

// TestAdmissionRefusals proves the real admission and activation boundaries
// fail closed: an undeclared activation secret fails preparation before any
// container exists, a mutable image tag is refused rather than given a
// synthesized digest, and a missing declared input is refused before any
// execution row is written.
func TestAdmissionRefusals(t *testing.T) {
	harness := factory.Start(t,
		factory.WithSecretRules(secret.NameRule{
			Scope: tenancy.Scope{
				TenantID:  id.TenantID(factory.TenantID),
				ProjectID: id.ProjectID(factory.ProjectID),
			},
			Names: []id.SecretName{pipelineSecretName},
		}),
		factory.WithSecrets(map[string]string{
			pipelineSecretName: pipelineSecretValue,
			deniedSecretName:   deniedSecretValue,
		}),
	)
	ctx := t.Context()

	t.Run("undeclared secret fails preparation", func(t *testing.T) {
		document := marshalPipeline(t, pipelineDoc{
			Actions: []pipelineActionDoc{{
				Name:     "denied",
				Image:    harness.Image.Reference,
				Secrets:  []string{deniedSecretName},
				Commands: []string{"true"},
			}},
		})
		applied := applyPipeline(t, harness, "denied", document)
		started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "start-denied")

		execution := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
		if execution.State != "failed" {
			t.Fatalf("denied-secret execution state = %q; want failed", execution.State)
		}
		if execution.Failure == nil || execution.Failure.Kind != "preparation" {
			t.Fatalf("denied-secret failure = %+v; want preparation", execution.Failure)
		}
		// The workspace is retained before preparation. A preparation failure
		// that never issued Start must still release it and keep the cleanup
		// record attributable instead of stranding it forever.
		if execution.Cleanup.State != "complete" {
			t.Fatalf("denied-secret cleanup = %+v; want complete", execution.Cleanup)
		}
		if execution.Cleanup.WorkloadID == "" {
			t.Fatal("denied-secret cleanup did not name the failed identity")
		}
		if execution.Cleanup.WorkspaceID == "" {
			t.Fatal("denied-secret cleanup did not name the retained workspace")
		}
		present, err := harness.WorkspaceVolumePresent(ctx, execution.Cleanup.WorkspaceID)
		if err != nil {
			t.Fatalf("probe released workspace volume: %v", err)
		}
		if present {
			t.Fatalf("preparation failure left the workspace volume for %s", execution.Cleanup.WorkspaceID)
		}
		containers, err := harness.Containers(ctx, runtimepodman.LabelExecution, started.ExecutionID)
		if err != nil {
			t.Fatalf("list execution containers: %v", err)
		}
		if len(containers) != 0 {
			t.Fatalf("preparation failure started containers %v", containers)
		}
	})

	t.Run("mutable tag is refused not synthesized", func(t *testing.T) {
		document := marshalPipeline(t, pipelineDoc{
			Actions: []pipelineActionDoc{{
				Name:     "tagged",
				Image:    "docker.io/library/alpine:3.24",
				Commands: []string{"true"},
			}},
		})
		applied := applyPipeline(t, harness, "tagged", document)
		_, publicError := harness.REST(harness.ProjectToken, "pipeline.start", api.PipelineStartRequest{
			ProjectID:  factory.ProjectID,
			PipelineID: applied.PipelineID,
		}, &applied.Version, "start-tagged")
		if publicError.Code != api.CodeDependencyUnavailable {
			t.Fatalf("mutable-tag start code = %q; want %q", publicError.Code, api.CodeDependencyUnavailable)
		}
		count, err := harness.ExecutionCount(ctx, applied.PipelineID)
		if err != nil {
			t.Fatalf("count tagged executions: %v", err)
		}
		if count != 0 {
			t.Fatalf("mutable-tag start wrote %d executions; want 0", count)
		}
	})

	t.Run("missing declared input is refused", func(t *testing.T) {
		document := marshalPipeline(t, pipelineDoc{
			Inputs: []string{"required"},
			Actions: []pipelineActionDoc{{
				Name:     "input",
				Image:    harness.Image.Reference,
				Commands: []string{"true"},
			}},
		})
		applied := applyPipeline(t, harness, "missing-input", document)
		_, publicError := harness.REST(harness.ProjectToken, "pipeline.start", api.PipelineStartRequest{
			ProjectID:  factory.ProjectID,
			PipelineID: applied.PipelineID,
		}, &applied.Version, "start-missing-input")
		if publicError.Code != api.CodeInvalidInput {
			t.Fatalf("missing-input start code = %q; want %q", publicError.Code, api.CodeInvalidInput)
		}
		count, err := harness.ExecutionCount(ctx, applied.PipelineID)
		if err != nil {
			t.Fatalf("count missing-input executions: %v", err)
		}
		if count != 0 {
			t.Fatalf("missing-input start wrote %d executions; want 0", count)
		}
	})
}
