//go:build integration

package lifecycle

// Lifecycle S11 rows (Podman).
//
// Each test asserts one acceptance-row invariant over the real factory
// composition and pins the precise product behavior that would otherwise be
// silently lost.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	s11ReleasePath = "/workspace/s11-release"
)

// s11WaitRunningAction is the readiness barrier for one running container
// action. It returns the observed workload container name.
func s11WaitRunningAction(t *testing.T, harness *factory.Harness, executionID, actionName string) string {
	t.Helper()
	harness.Eventually(t, 2*time.Minute, func() bool {
		current := inspectExecution(t, harness, harness.ProjectToken, executionID)
		if current.State != "running" {
			return false
		}
		for _, action := range current.Actions {
			if action.Name == actionName && action.State == "running" {
				return true
			}
		}
		return false
	})
	var container string
	harness.Eventually(t, time.Minute, func() bool {
		found, err := harness.RunningExecutionContainer(t.Context(), executionID)
		if err != nil || found == "" {
			return false
		}
		container = found
		return true
	})
	return container
}

// s11WaitWorkspaceFile waits until a workload-side readiness file exists.
func s11WaitWorkspaceFile(t *testing.T, harness *factory.Harness, container, path string) {
	t.Helper()
	harness.Eventually(t, time.Minute, func() bool {
		_, err := harness.ExecContainer(t.Context(), container, "test", "-f", path)
		return err == nil
	})
}

// s11ReleaseWorkload creates the deterministic release file and returns after
// the Podman exec succeeded.
func s11ReleaseWorkload(t *testing.T, harness *factory.Harness, container, path string) {
	t.Helper()
	if err := harness.TouchContainerFile(t.Context(), container, path); err != nil {
		t.Fatalf("release workload: %v", err)
	}
}

// TestExitedWorkloadStopEvidenceSurvivesRestart (AC6) proves a workload that
// exits on its own records affirmative stop evidence, and that the confirmed
// evidence and its diagnosed exit status survive a controller restart rather
// than being re-derived from a container that cleanup has already removed.
func TestExitedWorkloadStopEvidenceSurvivesRestart(t *testing.T) {
	harness := factory.Start(t)
	document := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "exited",
			Image:   harness.Image.Reference,
			Timeout: "1h",
			Commands: []string{
				`printf 'exited-once\n' >> /workspace/runs`,
				`printf 'runs-before-exit=%s\n' "$(wc -l < /workspace/runs)"`,
				"exit 3",
			},
		}},
	})
	applied := applyPipeline(t, harness, "exited-stop", document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "exited-stop-start")
	execution := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if execution.State != "failed" {
		t.Fatalf("execution state = %q; want failed (failure=%+v)", execution.State, execution.Failure)
	}
	if execution.Failure == nil || execution.Failure.Kind != "runtime" || execution.Failure.Reason != "workload_exited" {
		t.Fatalf("execution failure = %+v; want kind runtime reason workload_exited", execution.Failure)
	}
	if !strings.Contains(execution.Failure.Detail, "status 3") {
		t.Fatalf("execution failure detail = %q; want the observed exit status", execution.Failure.Detail)
	}
	before := actionByName(t, execution, "exited")
	if before.Stop == nil || before.Stop.State != "confirmed" {
		t.Fatalf("exited action stop evidence = %+v; want confirmed", before.Stop)
	}
	if !strings.Contains(before.Stop.Detail, "exited with code 3") {
		t.Fatalf("exited action stop detail = %q; want the observed exit code", before.Stop.Detail)
	}
	beforeLogs := executionLogs(t, harness, harness.ProjectToken, started.ExecutionID, "exited")
	if !strings.Contains(beforeLogs, "runs-before-exit=1") {
		t.Fatalf("retained logs before restart do not show exactly one command execution:\n%s", beforeLogs)
	}

	harness.RestartController()

	after := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if after.State != "failed" || after.Failure == nil || after.Failure.Detail != execution.Failure.Detail {
		t.Fatalf("terminal failure changed across restart: before=%+v after=%+v", execution.Failure, after.Failure)
	}
	afterAction := actionByName(t, after, "exited")
	if afterAction.Stop == nil || afterAction.Stop.State != "confirmed" {
		t.Fatalf("exited action stop evidence after restart = %+v; want the persisted confirmed barrier", afterAction.Stop)
	}
	if afterAction.Stop.Detail != before.Stop.Detail {
		t.Fatalf("stop detail changed across restart: before=%q after=%q", before.Stop.Detail, afterAction.Stop.Detail)
	}
	afterLogs := executionLogs(t, harness, harness.ProjectToken, started.ExecutionID, "exited")
	if !strings.Contains(afterLogs, "runs-before-exit=1") || strings.Contains(afterLogs, "runs-before-exit=2") {
		t.Fatalf("retained logs after restart show a replay or lost command execution:\n%s", afterLogs)
	}
	containers, err := harness.Containers(t.Context(), runtimepodman.LabelExecution, started.ExecutionID)
	if err != nil {
		t.Fatalf("list execution containers after restart: %v", err)
	}
	if len(containers) != 0 {
		t.Fatalf("exited workload left containers after terminal cleanup: %v", containers)
	}
	count, err := harness.ExecutionCount(t.Context(), applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions: %v", err)
	}
	if count != 1 {
		t.Fatalf("execution count = %d; want exactly the original execution", count)
	}
}

// TestCleanupRecoveryAfterControllerCrash (AC9) proves a terminal execution's
// pending cleanup survives a controller crash and that the successor completes
// it from durable evidence once the external obstruction is gone. The
// obstruction is a real container that references the retained workspace
// volume, so the first release attempt genuinely fails and the pending record
// names its attempt and error.
func TestCleanupRecoveryAfterControllerCrash(t *testing.T) {
	// A long scan cadence keeps the first controller from spending its bounded
	// retry budget while the replacement controller is being built.
	harness := factory.Start(t, factory.WithLoopInterval(10*time.Second))
	document := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "work",
			Image:   harness.Image.Reference,
			Timeout: "1h",
			Commands: []string{
				`touch /workspace/cleanup-ready`,
				`while [ ! -e ` + s11ReleasePath + ` ]; do sleep 1; done`,
			},
		}},
	})
	applied := applyPipeline(t, harness, "cleanup-recovery", document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "cleanup-recovery-start")
	container := s11WaitRunningAction(t, harness, started.ExecutionID, "work")
	s11WaitWorkspaceFile(t, harness, container, "/workspace/cleanup-ready")

	workspaceID, err := harness.ExecutionWorkspaceID(t.Context(), started.ExecutionID)
	if err != nil {
		t.Fatalf("read execution workspace: %v", err)
	}
	if workspaceID == "" {
		t.Fatal("running execution has no retained workspace")
	}

	// A live container that mounts the retained volume makes workspace release
	// fail with an affirmative reference, so cleanup cannot fabricate success.
	probeName := factory.ProbeContainerName("s11-cleanup-probe")
	_ = harness.RemoveProbeContainer(t.Context(), probeName)
	if err := harness.StartWorkspaceProbeContainer(t.Context(), probeName, workspaceID); err != nil {
		t.Fatalf("start workspace reference probe: %v", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = harness.RemoveProbeContainer(cleanup, probeName)
	})

	s11ReleaseWorkload(t, harness, container, s11ReleasePath)

	// Wait for the terminal outcome and the first attributable failed cleanup
	// attempt before crashing the controller.
	var pending api.ExecutionInspectResult
	harness.Eventually(t, 2*time.Minute, func() bool {
		current := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
		switch current.State {
		case "succeeded", "failed", "cancelled":
		default:
			return false
		}
		if current.Cleanup.State != "pending" || current.Cleanup.Attempts < 1 {
			return false
		}
		pending = current
		return true
	})
	if pending.Cleanup.LastError == "" {
		t.Fatalf("pending cleanup has no actionable last error: %+v", pending.Cleanup)
	}
	if pending.Cleanup.WorkspaceID != workspaceID {
		t.Fatalf("pending cleanup workspace = %q; want %q", pending.Cleanup.WorkspaceID, workspaceID)
	}

	harness.RestartController()

	// The pending record is durable state, not controller memory: it must be
	// the same evidence after the crash, still owed and still attributable.
	afterRestart := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if afterRestart.State != "succeeded" {
		t.Fatalf("terminal outcome changed across restart: %q", afterRestart.State)
	}
	if afterRestart.Cleanup.State != "pending" || afterRestart.Cleanup.Attempts < pending.Cleanup.Attempts {
		t.Fatalf("cleanup after restart = %+v; want the persisted pending record with at least %d attempts",
			afterRestart.Cleanup, pending.Cleanup.Attempts)
	}
	if afterRestart.Cleanup.LastError != pending.Cleanup.LastError {
		t.Fatalf("cleanup error changed across restart: before=%q after=%q",
			pending.Cleanup.LastError, afterRestart.Cleanup.LastError)
	}
	if afterRestart.Cleanup.WorkspaceID != workspaceID {
		t.Fatalf("cleanup workspace after restart = %q; want %q", afterRestart.Cleanup.WorkspaceID, workspaceID)
	}

	// Remove the external obstruction and make the durable retry schedule due.
	if err := harness.RemoveProbeContainer(t.Context(), probeName); err != nil {
		t.Fatalf("remove workspace reference probe: %v", err)
	}
	harness.Clock.Advance(2 * time.Minute)

	recovered := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if recovered.Cleanup.State != "complete" {
		t.Fatalf("cleanup after recovery = %+v; want complete", recovered.Cleanup)
	}
	assertExecutionResourcesReleased(t, harness, started.ExecutionID)
}

// TestFilesystemValuesNeverBecomeOutputs (AC12) proves declared outputs are
// only what the workload submits through the execution credential. Files the
// workload writes into its shared workspace, including files named exactly like
// the declared output, are never promoted and never materialized as earlier
// outputs for a following action.
func TestFilesystemValuesNeverBecomeOutputs(t *testing.T) {
	harness := factory.Start(t)
	ctx := t.Context()
	const filesystemValue = "filesystem-value-never-an-output"
	document := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{
			{
				Name:    "produce",
				Image:   harness.Image.Reference,
				Outputs: []string{"artifact"},
				Commands: []string{
					`mkdir -p /workspace/outputs`,
					`printf '` + filesystemValue + `\n' > /workspace/artifact`,
					`printf '` + filesystemValue + `\n' > /workspace/outputs/artifact`,
					`printf 'produce-done\n'`,
				},
			},
			{
				Name:  "consume",
				Image: harness.Image.Reference,
				Commands: []string{
					`test ! -e /kodama/outputs/produce/artifact`,
					`test "$(cat /workspace/artifact)" = "` + filesystemValue + `"`,
					`printf 'consume-saw-workspace-only\n'`,
				},
			},
		},
	})
	applied := applyPipeline(t, harness, "filesystem-outputs", document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "filesystem-outputs-start")
	execution := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	requireSucceeded(t, execution)

	produce := actionByName(t, execution, "produce")
	if len(produce.Outputs) != 0 {
		t.Fatalf("produce published outputs %+v from the filesystem; want none", produce.Outputs)
	}
	consume := actionByName(t, execution, "consume")
	if consume.State != "succeeded" {
		t.Fatalf("consume state = %q; want succeeded only because the output was never materialized", consume.State)
	}
	if len(consume.Outputs) != 0 {
		t.Fatalf("consume published outputs %+v; want none", consume.Outputs)
	}
	produceRow, err := harness.ActionRow(ctx, execution.ExecutionID, "produce")
	if err != nil {
		t.Fatalf("read produce action row: %v", err)
	}
	if produceRow.Outputs != "" {
		t.Fatalf("produce action row outputs = %q; want NULL", produceRow.Outputs)
	}
	if produceRow.StagedOutputs != "" || produceRow.StagedOutputsSet {
		t.Fatalf("produce action row staged filesystem values %q (set=%v)", produceRow.StagedOutputs, produceRow.StagedOutputsSet)
	}
	materialized, err := harness.OutputMaterializations("produce")
	if err != nil {
		t.Fatalf("inspect output materializations: %v", err)
	}
	if len(materialized) != 0 {
		t.Fatalf("filesystem values were materialized as outputs at %v", materialized)
	}
	logs := executionLogs(t, harness, harness.ProjectToken, started.ExecutionID, "consume")
	if !strings.Contains(logs, "consume-saw-workspace-only") {
		t.Fatalf("retained consume logs = %q; want the workspace-only marker", logs)
	}
}

// TestSecretChangeIsolationAcrossActions (AC11) proves a mounted secret value
// stays fixed for the workload that read it while a later write takes effect
// only at the next activation.
func TestSecretChangeIsolationAcrossActions(t *testing.T) {
	const (
		secretName = "ROTATE_SECRET"
		oldValue   = "rotation-old-value"
		newValue   = "rotation-new-value"
	)
	harness := factory.Start(t,
		factory.WithSecretRules(secret.NameRule{
			Scope: tenancy.Scope{
				TenantID:  id.TenantID(factory.TenantID),
				ProjectID: id.ProjectID(factory.ProjectID),
			},
			Names: []id.SecretName{secretName},
		}),
		factory.WithSecrets(map[string]string{secretName: oldValue}),
	)
	document := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{
			{
				Name:    "work",
				Image:   harness.Image.Reference,
				Timeout: "1h",
				Secrets: []string{secretName},
				Commands: []string{
					`value=$(cat /kodama/secrets/ROTATE_SECRET)`,
					`if [ "$value" = "` + oldValue + `" ]; then printf 'hold-initial=old\n'; else printf 'hold-initial=unexpected\n'; exit 1; fi`,
					`touch /workspace/hold-ready`,
					`while [ ! -e ` + s11ReleasePath + ` ]; do sleep 1; done`,
					`value=$(cat /kodama/secrets/ROTATE_SECRET)`,
					`if [ "$value" = "` + oldValue + `" ]; then printf 'hold-final=old\n'; else printf 'hold-final=unexpected\n'; exit 1; fi`,
				},
			},
			{
				Name:    "later",
				Image:   harness.Image.Reference,
				Secrets: []string{secretName},
				Commands: []string{
					`value=$(cat /kodama/secrets/ROTATE_SECRET)`,
					`if [ "$value" = "` + newValue + `" ]; then printf 'later-value=new\n'; else printf 'later-value=unexpected\n'; exit 1; fi`,
				},
			},
		},
	})
	applied := applyPipeline(t, harness, "secret-rotation", document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "secret-rotation-start")
	container := s11WaitRunningAction(t, harness, started.ExecutionID, "work")
	s11WaitWorkspaceFile(t, harness, container, "/workspace/hold-ready")

	// A replacement is a versioned write of the existing project-scoped name.
	// The fixture wrote the initial value once, so version 1 is the current row.
	initialVersion := uint64(1)
	written := harness.MustREST(harness.ProjectToken, "secret.write", api.SecretWriteRequest{
		ProjectID: factory.ProjectID,
		SecretID:  secretName,
		Value:     newValue,
	}, &initialVersion, "secret-rotation-write")
	var writeResult api.SecretWriteResult
	harness.Decode(written, &writeResult)
	if writeResult.Version != initialVersion+1 {
		t.Fatalf("secret version after replacement = %d; want %d", writeResult.Version, initialVersion+1)
	}
	metadata := harness.MustREST(harness.ProjectToken, "secret.metadata.read", api.SecretMetadataReadRequest{
		ProjectID: factory.ProjectID,
		SecretID:  writeResult.SecretID,
	}, nil, "")
	var readResult api.SecretMetadataReadResult
	harness.Decode(metadata, &readResult)
	if readResult.Version != writeResult.Version {
		t.Fatalf("secret metadata version = %d; want the committed replacement %d", readResult.Version, writeResult.Version)
	}

	s11ReleaseWorkload(t, harness, container, s11ReleasePath)
	execution := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	requireSucceeded(t, execution)

	workLogs := executionLogs(t, harness, harness.ProjectToken, started.ExecutionID, "work")
	if !strings.Contains(workLogs, "hold-initial=old") {
		t.Fatalf("first action did not read the activation value:\n%s", workLogs)
	}
	if !strings.Contains(workLogs, "hold-final=old") {
		t.Fatalf("running action observed a later secret write instead of its fixed mounted value:\n%s", workLogs)
	}
	if strings.Contains(workLogs, "unexpected") {
		t.Fatalf("running action observed the replacement value:\n%s", workLogs)
	}
	laterLogs := executionLogs(t, harness, harness.ProjectToken, started.ExecutionID, "later")
	if !strings.Contains(laterLogs, "later-value=new") {
		t.Fatalf("later activation did not resolve the current secret value:\n%s", laterLogs)
	}
	if strings.Contains(laterLogs, "unexpected") {
		t.Fatalf("later activation reused the earlier mounted value:\n%s", laterLogs)
	}
}

// TestForgedEventsCannotSettleExecution (AC15) proves the authenticated public
// event ingress cannot impersonate a lifecycle fact and that an observation
// carrying fabricated settlement data changes no execution or action state.
func TestForgedEventsCannotSettleExecution(t *testing.T) {
	harness := factory.Start(t)
	document := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:    "work",
			Image:   harness.Image.Reference,
			Timeout: "1h",
			Outputs: []string{"artifact"},
			Commands: []string{
				`printf 'forged-hold\n'`,
				`touch /workspace/forged-ready`,
				`while [ ! -e ` + s11ReleasePath + ` ]; do sleep 1; done`,
				`printf 'forged-released\n'`,
			},
		}},
	})
	applied := applyPipeline(t, harness, "forged-events", document)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "forged-events-start")
	container := s11WaitRunningAction(t, harness, started.ExecutionID, "work")
	s11WaitWorkspaceFile(t, harness, container, "/workspace/forged-ready")

	running := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	action := actionByName(t, running, "work")
	if action.State != "running" {
		t.Fatalf("work action state = %q; want running", action.State)
	}

	for _, forged := range []string{"execution.succeeded", "execution.failed", "execution.cancelled", "action.transition", "request.resolved"} {
		_, publicError := harness.REST(harness.ProjectToken, "event.publish", api.EventPublishRequest{
			ProjectID:      factory.ProjectID,
			Type:           forged,
			SubjectID:      started.ExecutionID,
			SubjectVersion: running.Version,
			Data:           []byte(`{"state":"succeeded","decision":"pass"}`),
		}, nil, "forged-"+forged)
		if publicError.Code != api.CodeInvalidInput {
			t.Fatalf("event.publish type %q = %q (%s); want %q: a custom ingress can never forge a lifecycle fact",
				forged, publicError.Code, publicError.Message, api.CodeInvalidInput)
		}
	}

	forgedData := []byte(`{"execution_id":"` + started.ExecutionID + `","state":"succeeded","action_id":"` +
		action.ActionID + `","outputs":{"artifact":"forged-output"}}`)
	accepted := harness.MustREST(harness.ProjectToken, "event.publish", api.EventPublishRequest{
		ProjectID:      factory.ProjectID,
		Type:           "observation.custom",
		SubjectID:      started.ExecutionID,
		SubjectVersion: running.Version,
		Data:           forgedData,
	}, nil, "forged-observation")
	var published api.EventPublishResult
	harness.Decode(accepted, &published)
	if published.EventID == "" || published.Replayed {
		t.Fatalf("custom observation result = %+v; want one fresh committed event", published)
	}

	afterForgery := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if afterForgery.State != "running" {
		t.Fatalf("execution state after forged events = %q; want running", afterForgery.State)
	}
	afterAction := actionByName(t, afterForgery, "work")
	if afterAction.State != "running" {
		t.Fatalf("action state after forged events = %q; want running", afterAction.State)
	}
	if len(afterAction.Outputs) != 0 {
		t.Fatalf("forged settlement data became outputs %+v", afterAction.Outputs)
	}
	if afterAction.Decision != "" || afterAction.Actor != "" {
		t.Fatalf("forged settlement recorded a decision actor=%q decision=%q", afterAction.Actor, afterAction.Decision)
	}

	s11ReleaseWorkload(t, harness, container, s11ReleasePath)
	finished := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	requireSucceeded(t, finished)
	finalAction := actionByName(t, finished, "work")
	if finalAction.State != "succeeded" {
		t.Fatalf("work action state after real success = %q; want succeeded", finalAction.State)
	}
	if len(finalAction.Outputs) != 0 {
		t.Fatalf("real run promoted outputs %+v that were never submitted", finalAction.Outputs)
	}
	logs := executionLogs(t, harness, harness.ProjectToken, started.ExecutionID, "work")
	if strings.Count(logs, "forged-hold") != 1 || strings.Count(logs, "forged-released") != 1 {
		t.Fatalf("retained logs do not show exactly one real workload execution:\n%s", logs)
	}
}

// TestStartReceiptReplayAfterPipelineEdit (AC13) proves pipeline.start is
// receipt-first: after the stored pipeline advances, an equal retry of an
// already committed start returns the original execution without admitting the
// edited definition or writing a second execution. The edit adds a required
// input, so any implementation that admits the current definition before
// consulting the receipt fails this test.
func TestStartReceiptReplayAfterPipelineEdit(t *testing.T) {
	harness := factory.Start(t)
	original := marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:     "original",
			Image:    harness.Image.Reference,
			Commands: []string{`printf 'original-definition\n'`},
		}},
	})
	applied := applyPipeline(t, harness, "start-replay", original)
	started := startPipeline(t, harness, harness.ProjectToken, applied.PipelineID, nil, "start-replay-key")
	first := waitExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	requireSucceeded(t, first)
	if started.Version != 1 {
		t.Fatalf("start result version = %d; want the execution version 1", started.Version)
	}

	// The edit is valid but would refuse a fresh start without the new input.
	edited := marshalPipeline(t, pipelineDoc{
		Inputs: []string{"edited-required"},
		Actions: []pipelineActionDoc{{
			Name:     "edited",
			Image:    harness.Image.Reference,
			Commands: []string{`printf 'edited-definition\n'`},
		}},
	})
	updated := updatePipeline(t, harness, applied.PipelineID, "start-replay", edited, applied.Version)
	if updated.Version != applied.Version+1 {
		t.Fatalf("pipeline version after edit = %d; want %d", updated.Version, applied.Version+1)
	}

	// An equal retry uses the original envelope expected version and the same
	// caller key. It must replay rather than re-admit the edited definition.
	// The shared start helper always reads the current version, so this exact
	// original request is issued through the raw public envelope.
	replayResult, publicError := harness.REST(harness.ProjectToken, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: applied.PipelineID,
		Inputs:     nil,
	}, &applied.Version, "start-replay-key")
	if publicError.Code != "" {
		t.Fatalf("pipeline.start replay after edit refused: %s (%s)", publicError.Code, publicError.Message)
	}
	var replayed api.PipelineStartResult
	harness.Decode(replayResult, &replayed)
	if replayed.ExecutionID != started.ExecutionID {
		t.Fatalf("replayed start returned execution %q; want the original %q", replayed.ExecutionID, started.ExecutionID)
	}
	if replayed.Version != started.Version {
		t.Fatalf("replayed start version = %d; want the original %d", replayed.Version, started.Version)
	}

	count, err := harness.ExecutionCount(t.Context(), applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions: %v", err)
	}
	if count != 1 {
		t.Fatalf("execution count after replay = %d; want exactly 1", count)
	}
	receipts, err := harness.ReceiptCount(t.Context(), "start-replay-key")
	if err != nil {
		t.Fatalf("count start receipts: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("start receipt count = %d; want exactly 1", receipts)
	}

	// The admitted bytes are still the original definition, not the edit.
	admitted, _, err := harness.ExecutionRow(t.Context(), started.ExecutionID)
	if err != nil {
		t.Fatalf("read admitted execution row: %v", err)
	}
	if string(admitted) != string(original) {
		t.Fatalf("admitted yaml changed after the pipeline edit:\nadmitted=%s\noriginal=%s", admitted, original)
	}

	// The edit is real: a fresh key at the stale expected version is refused.
	_, publicError = harness.REST(harness.ProjectToken, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: applied.PipelineID,
		Inputs:     map[string]string{"edited-required": "value"},
	}, &applied.Version, "start-replay-stale")
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("fresh start at the stale pipeline version = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeVersionConflict)
	}
	count, err = harness.ExecutionCount(t.Context(), applied.PipelineID)
	if err != nil {
		t.Fatalf("count executions after stale start: %v", err)
	}
	if count != 1 {
		t.Fatalf("stale start wrote an execution: count = %d", count)
	}
}
