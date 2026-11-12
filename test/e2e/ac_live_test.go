//go:build e2e

package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
)

// TestE2E_AC01InstalledPipeline proves a pipeline executes on an installed
// Kodama through the public API with an ordinary shell command: an inline
// task-free start reaches succeeded with cleanup=complete, and its retained
// logs are still readable through execution.logs.read after cleanup.
func TestE2E_AC01InstalledPipeline(t *testing.T) {
	e2eParallel(t)
	instance := startLive(t)
	started := time.Now().UTC()

	document, err := (harness.Document{
		Actions: []harness.Action{{
			Name:     "ac01-build",
			Image:    instance.image,
			Timeout:  "10m",
			Commands: []string{`echo "ac01-pipeline-ok"`, `test -d /workspace`},
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build inline pipeline: %v", err)
	}
	admitted := instance.startInline(t, document, instance.key("ac01-start"))
	view := instance.waitComplete(t, admitted.ExecutionID)
	requireSucceeded(t, view)

	action := actionByName(t, view, "ac01-build")
	if action.State != "succeeded" {
		t.Fatalf("action ac01-build state = %q; want succeeded", action.State)
	}
	if action.Index != 0 {
		t.Fatalf("action ac01-build index = %d; want 0", action.Index)
	}
	if view.PipelineID != "" || view.TaskID != "" {
		t.Fatalf("inline task-free execution identity = pipeline %q task %q; want both empty", view.PipelineID, view.TaskID)
	}
	log := instance.readLogs(t, admitted.ExecutionID, action.ActionID, "stdout")
	if err := harness.ScanCredentials(string(log), append(instance.config.PersonaSecrets(), instance.token)...); err != nil {
		t.Fatalf("ac01 retained log: %v", err)
	}
	// The exact marker proves the ordinary shell command ran; no runtime
	// banner is part of the assertion.
	requireLogContains(t, "ac01-build", log, "ac01-pipeline-ok\n")

	finished := time.Now().UTC()
	instance.record(t, "AC01", "1.2", "TestE2E_AC01InstalledPipeline", &view, started, finished, nil)
	instance.record(t, "AC01", "1.3", "TestE2E_AC01InstalledPipeline", &view, started, finished, nil)
}

// TestE2E_AC02ImmutableAdmission proves API-only admission keeps the exact
// pipeline bytes a stored definition was posted with, an inline task-free run
// carries no pipeline or task identity, and a pipeline edit made WHILE the
// admitted execution is running does not change its later action.
func TestE2E_AC02ImmutableAdmission(t *testing.T) {
	e2eParallel(t)
	instance := startLive(t)
	started := time.Now().UTC()

	name := "ac02-" + instance.runID
	admittedDocument, err := (harness.Document{
		Actions: []harness.Action{
			{
				Name:     "ac02-first",
				Image:    instance.image,
				Timeout:  "10m",
				Commands: []string{"echo ac02-first-started", "sleep 30", "echo ac02-first-done"},
			},
			{
				Name:     "ac02-second",
				Image:    instance.image,
				Timeout:  "10m",
				Commands: []string{"echo ac02-second-original"},
			},
		},
	}).Build()
	if err != nil {
		t.Fatalf("build admitted pipeline: %v", err)
	}
	applied := instance.applyPipeline(t, name, admittedDocument, instance.key("ac02-apply-1"))
	read := instance.readPipeline(t, applied.PipelineID)
	if read.Name != name {
		t.Fatalf("pipeline.read name = %q; want %q", read.Name, name)
	}
	if string(read.YAML) != string(admittedDocument) {
		t.Fatalf("pipeline.read yaml differs from the posted bytes")
	}
	if read.Version != applied.Version {
		t.Fatalf("pipeline.read version = %d; want the applied version %d", read.Version, applied.Version)
	}
	if harness.DocumentDigest(read.YAML) != harness.DocumentDigest(admittedDocument) {
		t.Fatal("pipeline.read returned different bytes with an equal visible shape")
	}

	admitted := instance.startStored(t, applied.PipelineID, read.Version, instance.key("ac02-start"))
	// Observe the first action running before editing the stored pipeline. The
	// later action must still come from the admitted snapshot.
	_, observations, err := harness.Poll(t.Context(), harness.RealClock{}, harness.PollOptions{
		Interval: 100 * time.Millisecond,
		Timeout:  3 * time.Minute,
		Stop: func(result api.ExecutionInspectResult) bool {
			for _, action := range result.Actions {
				if action.Name == "ac02-first" && action.State == "running" {
					return true
				}
			}
			return false
		},
	}, func(ctx context.Context) (api.ExecutionInspectResult, error) {
		return instance.inspectResult(ctx, admitted.ExecutionID)
	})
	if err != nil {
		t.Fatalf("execution %s never showed its first action running: %v", admitted.ExecutionID, err)
	}
	running := instance.inspect(t, admitted.ExecutionID)
	if got := actionByName(t, running, "ac02-first"); got.State != "running" {
		t.Fatalf("first action state = %q; want running before the pipeline edit", got.State)
	}
	if got := actionByName(t, running, "ac02-second"); got.State != "pending" {
		t.Fatalf("second action state = %q; want pending before the pipeline edit", got.State)
	}
	sawRunning := false
	for _, observation := range observations {
		if action, ok := findAction(observation, "ac02-first"); ok && action.State == "running" {
			sawRunning = true
		}
	}
	if !sawRunning {
		t.Fatal("the poll did not observe the first action running")
	}

	// Edit the stored pipeline while the admitted execution is still running:
	// the second action is renamed and its command is replaced.
	updatedDocument, err := (harness.Document{
		Actions: []harness.Action{
			{
				Name:     "ac02-first",
				Image:    instance.image,
				Timeout:  "10m",
				Commands: []string{"echo ac02-first-v2"},
			},
			{
				Name:     "ac02-changed",
				Image:    instance.image,
				Timeout:  "10m",
				Commands: []string{"echo ac02-second-replaced"},
			},
		},
	}).Build()
	if err != nil {
		t.Fatalf("build updated pipeline: %v", err)
	}
	updated := instance.updatePipeline(t, applied.PipelineID, name, updatedDocument, read.Version, instance.key("ac02-update"))
	readSecond := instance.readPipeline(t, applied.PipelineID)
	if string(readSecond.YAML) != string(updatedDocument) {
		t.Fatalf("pipeline.read after update differs from the new posted bytes")
	}
	if readSecond.Version != updated.Version {
		t.Fatalf("pipeline.read version = %d; want the updated version %d", readSecond.Version, updated.Version)
	}

	// The in-flight execution finishes the ADMITTED actions, not the edit.
	view := instance.waitComplete(t, admitted.ExecutionID)
	requireSucceeded(t, view)
	second := actionByName(t, view, "ac02-second")
	secondLog := instance.readLogs(t, admitted.ExecutionID, second.ActionID, "stdout")
	if err := harness.ScanCredentials(string(secondLog), instance.token); err != nil {
		t.Fatalf("ac02 admitted action log: %v", err)
	}
	if err := harness.VerifyAdmittedSnapshot(
		[]string{"ac02-first", "ac02-second"},
		actionNames(view),
		secondLog,
		"ac02-second-original",
		"ac02-second-replaced",
	); err != nil {
		t.Fatalf("the running execution did not stay on its admitted snapshot: %v", err)
	}
	afterEdit := instance.inspect(t, admitted.ExecutionID)
	if _, changed := findAction(afterEdit, "ac02-changed"); changed {
		t.Fatal("pipeline edit rewrote the admitted execution's action set")
	}

	// Positive control: a NEW start of the updated pipeline really uses the new
	// definition, so the edit landed and the assertion above is not vacuous.
	control := instance.startStored(t, applied.PipelineID, readSecond.Version, instance.key("ac02-control"))
	controlView := instance.waitComplete(t, control.ExecutionID)
	requireSucceeded(t, controlView)
	if got, want := actionNames(controlView), []string{"ac02-first", "ac02-changed"}; !equalNames(got, want) {
		t.Fatalf("post-edit execution actions = %v; want %v", got, want)
	}
	changed := actionByName(t, controlView, "ac02-changed")
	controlLog := instance.readLogs(t, control.ExecutionID, changed.ActionID, "stdout")
	if err := harness.ScanCredentials(string(controlLog), instance.token); err != nil {
		t.Fatalf("ac02 control action log: %v", err)
	}
	requireLogContains(t, "ac02-changed", controlLog, "ac02-second-replaced")

	inlineDocument, err := (harness.Document{
		Actions: []harness.Action{{
			Name:     "ac02-inline",
			Image:    instance.image,
			Timeout:  "10m",
			Commands: []string{"echo ac02-inline"},
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build inline pipeline: %v", err)
	}
	inline := instance.startInline(t, inlineDocument, instance.key("ac02-inline"))
	inlineView := instance.waitComplete(t, inline.ExecutionID)
	requireSucceeded(t, inlineView)
	if inlineView.PipelineID != "" || inlineView.TaskID != "" {
		t.Fatalf("inline execution identity = pipeline %q task %q; want both empty", inlineView.PipelineID, inlineView.TaskID)
	}
	if got := actionByName(t, inlineView, "ac02-inline"); got.State != "succeeded" {
		t.Fatalf("inline action state = %q; want succeeded", got.State)
	}

	finished := time.Now().UTC()
	admittedDigest := harness.DocumentDigest(admittedDocument)
	instance.record(t, "AC02", "2.1", "TestE2E_AC02ImmutableAdmission", &view, started, finished, map[string]any{"yaml_sha256": admittedDigest})
	instance.record(t, "AC02", "2.2", "TestE2E_AC02ImmutableAdmission", &inlineView, started, finished, map[string]any{"yaml_sha256": harness.DocumentDigest(inlineDocument)})
	instance.record(t, "AC02", "2.3", "TestE2E_AC02ImmutableAdmission", &afterEdit, started, finished, map[string]any{"yaml_sha256": admittedDigest})
}

// TestE2E_AC09CleanupAttributable proves a terminal execution keeps an
// attributable cleanup record: cleanup completes and retains the workload and
// workspace identities that were owned by the run.
func TestE2E_AC09CleanupAttributable(t *testing.T) {
	e2eParallel(t)
	instance := startLive(t)
	started := time.Now().UTC()

	document, err := (harness.Document{
		Actions: []harness.Action{{
			Name:     "ac09-build",
			Image:    instance.image,
			Timeout:  "10m",
			Commands: []string{"echo ac09-cleanup"},
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build inline pipeline: %v", err)
	}
	admitted := instance.startInline(t, document, instance.key("ac09-start"))
	view := instance.waitComplete(t, admitted.ExecutionID)
	requireSucceeded(t, view)

	if view.Cleanup.State != "complete" {
		t.Fatalf("cleanup state = %q; want complete", view.Cleanup.State)
	}
	requireOpaqueIdentity(t, "cleanup workspace", view.Cleanup.WorkspaceID)
	requireOpaqueIdentity(t, "cleanup workload", view.Cleanup.WorkloadID)
	reinspected := instance.inspect(t, admitted.ExecutionID)
	requireStableIdentity(t, "cleanup workspace", view.Cleanup.WorkspaceID, reinspected.Cleanup.WorkspaceID)
	requireStableIdentity(t, "cleanup workload", view.Cleanup.WorkloadID, reinspected.Cleanup.WorkloadID)
	if view.Cleanup.LastError != "" {
		t.Fatalf("complete cleanup carries an error: %q", view.Cleanup.LastError)
	}
	action := actionByName(t, view, "ac09-build")
	if action.Stop == nil || action.Stop.State != "confirmed" {
		t.Fatalf("action %s stop evidence = %+v; want confirmed termination", action.Name, action.Stop)
	}

	finished := time.Now().UTC()
	instance.record(t, "AC09", "9.2", "TestE2E_AC09CleanupAttributable", &view, started, finished, nil)
}

// TestE2E_AC11Materialization proves literal environment, working directory,
// the reserved runtime variables and a shared workspace are materialized in the
// run, and the action's exit is observable as succeeded.
func TestE2E_AC11Materialization(t *testing.T) {
	e2eParallel(t)
	instance := startLive(t)
	started := time.Now().UTC()

	document, err := (harness.Document{
		Env: map[string]string{"AC11_ENV": "ac11-value"},
		Actions: []harness.Action{
			{
				Name:    "ac11-write",
				Image:   instance.image,
				Timeout: "10m",
				Commands: []string{
					`test "$AC11_ENV" = "ac11-value"`,
					`case "$(pwd)" in /*) ;; *) exit 3 ;; esac`,
					`test -w /workspace`,
					`echo "ac11-env=$AC11_ENV"`,
					`echo "ac11-pwd=$(pwd)"`,
					`echo "ac11-execution=$KODAMA_EXECUTION"`,
					`echo "ac11-action=$KODAMA_ACTION"`,
					`echo "ac11-materialized" > /workspace/ac11.txt`,
				},
			},
			{
				Name:    "ac11-read",
				Image:   instance.image,
				Timeout: "10m",
				Env:     map[string]string{"AC11_ENV": "ac11-override"},
				Commands: []string{
					`test "$AC11_ENV" = "ac11-override"`,
					`test "$(cat /workspace/ac11.txt)" = "ac11-materialized"`,
					`echo "ac11-override=$AC11_ENV"`,
					`echo "ac11-shared-workspace-ok"`,
				},
			},
		},
	}).Build()
	if err != nil {
		t.Fatalf("build inline pipeline: %v", err)
	}
	admitted := instance.startInline(t, document, instance.key("ac11-start"))
	view := instance.waitComplete(t, admitted.ExecutionID)
	requireSucceeded(t, view)

	writeAction := actionByName(t, view, "ac11-write")
	readAction := actionByName(t, view, "ac11-read")
	if writeAction.State != "succeeded" || readAction.State != "succeeded" {
		t.Fatalf("materialization actions = %q/%q; want succeeded/succeeded", writeAction.State, readAction.State)
	}
	writeLog := instance.readLogs(t, admitted.ExecutionID, writeAction.ActionID, "stdout")
	readLog := instance.readLogs(t, admitted.ExecutionID, readAction.ActionID, "stdout")
	for _, log := range [][]byte{writeLog, readLog} {
		if err := harness.ScanCredentials(string(log), instance.token); err != nil {
			t.Fatalf("ac11 retained log: %v", err)
		}
	}
	requireLogContains(t, "ac11-write", writeLog, "ac11-env=ac11-value")
	requireLogContains(t, "ac11-write", writeLog, "ac11-execution="+admitted.ExecutionID)
	requireLogContains(t, "ac11-write", writeLog, "ac11-action=ac11-write")
	requireAbsolutePwd(t, writeLog)
	requireLogContains(t, "ac11-read", readLog, "ac11-override=ac11-override")
	requireLogContains(t, "ac11-read", readLog, "ac11-shared-workspace-ok")

	finished := time.Now().UTC()
	instance.record(t, "AC11", "11.1", "TestE2E_AC11Materialization", &view, started, finished, nil)
	instance.record(t, "AC11", "11.2", "TestE2E_AC11Materialization", &view, started, finished, nil)
}

// TestE2E_AC12Outputs proves a successful action's submitted output is promoted
// and visible, while an output submitted by an action that then fails is
// discarded and never exposed.
func TestE2E_AC12Outputs(t *testing.T) {
	e2eParallel(t)
	instance := startLive(t)
	started := time.Now().UTC()

	ca := instance.materializeCA(t)
	caPath := ca.path
	submission, err := harness.OutputSubmissionScript(map[string]string{"artifact": "ac12-ok"}, caPath)
	if err != nil {
		t.Fatalf("build output submission: %v", err)
	}
	successDocument, err := (harness.Document{
		Actions: []harness.Action{{
			Name:     "ac12-publish",
			Image:    instance.image,
			Timeout:  "10m",
			Outputs:  []string{"artifact"},
			Env:      ca.environment(),
			Commands: ca.commands(submission, `echo ac12-submitted`),
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build success pipeline: %v", err)
	}
	successStart := instance.startInline(t, successDocument, instance.key("ac12-success"))
	successView := instance.waitComplete(t, successStart.ExecutionID)
	requireSucceeded(t, successView)
	published := actionByName(t, successView, "ac12-publish")
	if published.Outputs["artifact"] != "ac12-ok" {
		t.Fatalf("promoted output = %q; want ac12-ok (outputs=%+v)", published.Outputs["artifact"], published.Outputs)
	}
	log := instance.readLogs(t, successStart.ExecutionID, published.ActionID, "stdout")
	if err := harness.ScanCredentials(string(log), instance.token); err != nil {
		t.Fatalf("ac12 retained log: %v", err)
	}
	requireLogContains(t, "ac12-publish", log, "ac12-submitted")

	discardedSubmission, err := harness.OutputSubmissionScript(map[string]string{"artifact": "ac12-discarded"}, caPath)
	if err != nil {
		t.Fatalf("build discard submission: %v", err)
	}
	failureDocument, err := (harness.Document{
		Actions: []harness.Action{{
			Name:     "ac12-fail",
			Image:    instance.image,
			Timeout:  "10m",
			Outputs:  []string{"artifact"},
			Env:      ca.environment(),
			Commands: ca.commands(discardedSubmission, `echo ac12-failing`, `exit 9`),
		}},
	}).Build()
	if err != nil {
		t.Fatalf("build failure pipeline: %v", err)
	}
	failureStart := instance.startInline(t, failureDocument, instance.key("ac12-failure"))
	failureView := instance.waitComplete(t, failureStart.ExecutionID)
	failureStatus, err := harness.Interpret(failureView)
	if err != nil {
		t.Fatalf("invalid failed execution view: %v", err)
	}
	if failureView.State != "failed" {
		t.Fatalf("failing execution state = %q; want failed", failureView.State)
	}
	if !failureStatus.Terminal || !failureStatus.CleanupComplete {
		t.Fatalf("failed execution terminal=%v cleanup=%q", failureStatus.Terminal, failureView.Cleanup.State)
	}
	if failureView.Failure == nil {
		t.Fatal("failed execution carries no typed failure diagnostic")
	}
	failed := actionByName(t, failureView, "ac12-fail")
	if _, promoted := failed.Outputs["artifact"]; promoted {
		t.Fatalf("failed action exposed a staged output: %+v", failed.Outputs)
	}
	if failed.State != "failed" {
		t.Fatalf("failing action state = %q; want failed", failed.State)
	}
	// The script runs under /bin/sh -ec, so a failed submission would also fail
	// the action. Require the explicit exit-9 marker to prove the action failed
	// after the successful staged submission, not at the submission step.
	failureLog := instance.readLogs(t, failureStart.ExecutionID, failed.ActionID, "stdout")
	if err := harness.ScanCredentials(string(failureLog), instance.token); err != nil {
		t.Fatalf("ac12 failure log: %v", err)
	}
	requireLogContains(t, "ac12-fail", failureLog, "ac12-failing")

	finished := time.Now().UTC()
	instance.record(t, "AC12", "12.2", "TestE2E_AC12Outputs", &successView, started, finished, nil)
}

// TestE2E_AC13OrderedActions proves two actions run in declaration order, at
// most one is active at a time, and the second never activates before the first
// succeeded.
func TestE2E_AC13OrderedActions(t *testing.T) {
	e2eParallel(t)
	instance := startLive(t)
	started := time.Now().UTC()

	document, err := (harness.Document{
		Actions: []harness.Action{
			{
				Name:     "ac13-first",
				Image:    instance.image,
				Timeout:  "10m",
				Commands: []string{"sleep 3", "echo ac13-first-done"},
			},
			{
				Name:     "ac13-second",
				Image:    instance.image,
				Timeout:  "10m",
				Commands: []string{"echo ac13-second-done"},
			},
		},
	}).Build()
	if err != nil {
		t.Fatalf("build inline pipeline: %v", err)
	}
	admitted := instance.startInline(t, document, instance.key("ac13-start"))
	final, observations := instance.pollTerminal(t, admitted.ExecutionID)
	requireSucceeded(t, instance.waitComplete(t, admitted.ExecutionID))

	if len(final.Actions) != 2 {
		t.Fatalf("execution has %d actions; want 2", len(final.Actions))
	}
	if final.Actions[0].Name != "ac13-first" || final.Actions[1].Name != "ac13-second" {
		t.Fatalf("action order = %q/%q; want ac13-first/ac13-second", final.Actions[0].Name, final.Actions[1].Name)
	}
	if final.Actions[0].State != "succeeded" || final.Actions[1].State != "succeeded" {
		t.Fatalf("terminal action states = %q/%q; want succeeded/succeeded", final.Actions[0].State, final.Actions[1].State)
	}
	if final.Actions[0].ActivatedAt == nil || final.Actions[1].ActivatedAt == nil {
		t.Fatalf("action activation timestamps are not both recorded: %+v", final.Actions)
	}
	if final.Actions[1].ActivatedAt.Before(*final.Actions[0].ActivatedAt) {
		t.Fatalf("second action activated before the first: %s < %s", final.Actions[1].ActivatedAt, final.Actions[0].ActivatedAt)
	}

	sawActive := false
	sawSecondPending := false
	for _, observation := range observations {
		status, err := harness.Interpret(observation)
		if err != nil {
			t.Fatalf("intermediate inspection violated the single-active invariant: %v", err)
		}
		if status.RunningActions == 1 {
			sawActive = true
		}
		if len(observation.Actions) == 2 {
			first, second := observation.Actions[0], observation.Actions[1]
			if second.State == "running" && first.State != "succeeded" {
				t.Fatalf("second action ran while the first was %q", first.State)
			}
			if first.State == "running" && second.State == "pending" {
				sawSecondPending = true
			}
		}
	}
	if !sawActive {
		t.Fatal("no observation captured an active action; the ordering evidence is vacuous")
	}
	if !sawSecondPending {
		t.Fatal("no observation captured the second action pending behind the first")
	}

	finished := time.Now().UTC()
	instance.record(t, "AC13", "13.2", "TestE2E_AC13OrderedActions", &final, started, finished, nil)
}

// TestE2E_AC16PlannedWait observes a planned request wait without resolving it:
// the request action is running with its assignment, subject and deadline, no
// automatic advancement occurs, and cancellation settles the wait and its
// cleanup. Pass/rejection advancement is not exercised here.
func TestE2E_AC16PlannedWait(t *testing.T) {
	e2eParallel(t)
	instance := startLive(t)
	started := time.Now().UTC()

	document, err := (harness.Document{
		Actions: []harness.Action{
			{
				Name:     "ac16-prepare",
				Image:    instance.image,
				Timeout:  "10m",
				Commands: []string{"echo ac16-prepared"},
			},
			{
				Name:    "ac16-approve",
				Request: "role/operator",
				Timeout: "5m",
				Subject: map[string]any{"question": "q01-ac16"},
			},
		},
	}).Build()
	if err != nil {
		t.Fatalf("build inline pipeline: %v", err)
	}
	admitted := instance.startInline(t, document, instance.key("ac16-start"))

	var waiting api.ExecutionInspectResult
	_, _, err = harness.Poll(t.Context(), harness.RealClock{}, harness.PollOptions{
		Interval: 200 * time.Millisecond,
		Timeout:  6 * time.Minute,
		Stop: func(result api.ExecutionInspectResult) bool {
			for _, action := range result.Actions {
				if action.Name == "ac16-approve" && action.State == "running" {
					return true
				}
			}
			return false
		},
	}, func(ctx context.Context) (api.ExecutionInspectResult, error) {
		return instance.inspectResult(ctx, admitted.ExecutionID)
	})
	if err != nil {
		t.Fatalf("execution %s never reached the planned wait: %v", admitted.ExecutionID, err)
	}
	waiting = instance.inspect(t, admitted.ExecutionID)
	if waiting.State != "running" {
		t.Fatalf("waiting execution state = %q; want running", waiting.State)
	}
	approve := actionByName(t, waiting, "ac16-approve")
	if approve.Kind != "request" || approve.State != "running" {
		t.Fatalf("planned wait action = kind %q state %q; want request/running", approve.Kind, approve.State)
	}
	if approve.Assignment != "role/operator" {
		t.Fatalf("planned wait assignment = %q; want role/operator", approve.Assignment)
	}
	if approve.Subject == nil || len(approve.Subject.Fields) == 0 {
		t.Fatal("planned wait exposes no immutable subject")
	}
	if approve.Deadline == nil || approve.ActivatedAt == nil {
		t.Fatalf("planned wait deadline/activation = %v/%v; want both persisted", approve.Deadline, approve.ActivatedAt)
	}
	prepared := actionByName(t, waiting, "ac16-prepare")
	if prepared.State != "succeeded" {
		t.Fatalf("prepared action state = %q; want succeeded", prepared.State)
	}
	if prepared.Stop == nil || prepared.Stop.State != "confirmed" {
		t.Fatalf("predecessor stop evidence = %+v; want confirmed termination", prepared.Stop)
	}
	if waiting.Cleanup.State == "complete" {
		t.Fatal("a running request wait reported cleanup=complete")
	}
	if waiting.Cleanup.WorkspaceID != "" {
		requireOpaqueIdentity(t, "waiting cleanup workspace", waiting.Cleanup.WorkspaceID)
	}

	// Without a resolve the wait must not advance on its own. Observe a bounded
	// window of several polls rather than one sleep.
	_, observations, err := harness.Poll(t.Context(), harness.RealClock{}, harness.PollOptions{
		Interval: 2 * time.Second,
		Timeout:  12 * time.Second,
		Stop:     func(api.ExecutionInspectResult) bool { return false },
	}, func(ctx context.Context) (api.ExecutionInspectResult, error) {
		return instance.inspectResult(ctx, admitted.ExecutionID)
	})
	if !errors.Is(err, harness.ErrPollTimeout) {
		t.Fatalf("bounded wait observation returned %v; want the window to expire", err)
	}
	if len(observations) < 5 {
		t.Fatalf("bounded wait observation collected %d polls; want at least 5", len(observations))
	}
	for index, observation := range observations {
		if observation.State != "running" {
			t.Fatalf("wait poll %d state = %q; the wait advanced without an answer", index, observation.State)
		}
		if after := actionByName(t, observation, "ac16-approve"); after.State != "running" {
			t.Fatalf("wait poll %d request state = %q; want running", index, after.State)
		}
		earlier := actionByName(t, observation, "ac16-prepare")
		if earlier.State != "succeeded" || earlier.Stop == nil || earlier.Stop.State != "confirmed" {
			t.Fatalf("wait poll %d predecessor = state %q stop %+v; want succeeded/confirmed", index, earlier.State, earlier.Stop)
		}
		if observation.Cleanup.State == "complete" {
			t.Fatalf("wait poll %d reported cleanup=complete during a live wait", index)
		}
	}

	instance.cancelCurrent(t, admitted.ExecutionID, instance.key("ac16-cancel"))
	cancelled := instance.waitComplete(t, admitted.ExecutionID)
	if cancelled.State != "cancelled" {
		t.Fatalf("cancelled execution state = %q; want cancelled", cancelled.State)
	}
	if after := actionByName(t, cancelled, "ac16-approve"); after.State != "cancelled" {
		t.Fatalf("cancelled request action state = %q; want cancelled", after.State)
	}
	requireOpaqueIdentity(t, "cancelled cleanup workspace", cancelled.Cleanup.WorkspaceID)
	reinspected := instance.inspect(t, admitted.ExecutionID)
	requireStableIdentity(t, "cancelled cleanup workspace", cancelled.Cleanup.WorkspaceID, reinspected.Cleanup.WorkspaceID)
	if earlier := actionByName(t, cancelled, "ac16-prepare"); earlier.Stop == nil || earlier.Stop.State != "confirmed" {
		t.Fatalf("cancelled predecessor stop evidence = %+v; want confirmed termination", earlier.Stop)
	}

	finished := time.Now().UTC()
	instance.record(t, "AC16", "16.1", "TestE2E_AC16PlannedWait", &cancelled, started, finished, nil)
}
