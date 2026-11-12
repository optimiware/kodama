//go:build integration

package factorytest

import (
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// pushTriggerHumanRef and pushTriggerExecutionRef are the two refs the push
	// pipeline subscribes to. The fixture policy lets both the engineer and a
	// live execution update refs under refs/heads/work/.
	pushTriggerHumanRef     = "refs/heads/work/trigger-human"
	pushTriggerExecutionRef = "refs/heads/work/trigger-execution"

	// pushTriggerStartTimeout bounds the wait for the trigger loop to queue and
	// the runtime to finish the pushed pipeline's one short container action.
	pushTriggerStartTimeout = 5 * time.Minute
)

// TestFactoryHumanPushStartsSubscribedPipeline proves the KODAMA.md push path
// end to end: external Git push → Git service → verified report ingress →
// event table → on.push → queued execution. A human push through the served
// Git service starts the pipeline subscribed to the pushed ref with the pushed
// commit as its input, while an earlier push of a subscribed ref with a live
// execution credential starts nothing.
//
// The execution-originated report is committed (and awaited) before the human
// push, and the trigger processor handles accepted events in event-table order,
// so by the time the human push's execution exists the execution push has been
// handled; exactly one execution of the pipeline proves it started nothing.
func TestFactoryHumanPushStartsSubscribedPipeline(t *testing.T) {
	fixture := startGitHarness(t)
	resourceID := fixture.git.ProjectResourceID

	const starter = "trigger-push-starter"
	fixture.MustREST(fixture.ProjectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: factory.ProjectID,
		ServiceID: starter,
		Roles:     []string{"engineer"},
	}, nil, "create-"+starter)

	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "trigger")
	executionCommit := fixture.commit(t, directory, "trigger execution push")
	humanCommit := fixture.commit(t, directory, "trigger human push")

	document := fmt.Sprintf(`repository: %s
service: %s
on:
  push: [%s, %s]
inputs: [repository, ref, commit, previous_commit]
actions:
  - name: check
    image: %s
    timeout: 10m
    commands:
      - 'test "$(cat /kodama/inputs/repository)" = %s'
      - 'test "$(cat /kodama/inputs/ref)" = %s'
      - 'test "$(cat /kodama/inputs/commit)" = %s'
`, resourceID, starter, pushTriggerExecutionRef, pushTriggerHumanRef,
		fixture.Image.Reference, resourceID, pushTriggerHumanRef, humanCommit)
	applied := fixture.applyPipeline("trigger-push", []byte(document))

	// An execution-originated push of a subscribed ref: the report carries the
	// verified pusher's execution as its correlation and starts nothing.
	executionID, executionToken := fixture.startExecution(t, "trigger-pusher")
	if text, code := fixture.push(directory, executionToken, executionCommit+":"+pushTriggerExecutionRef); code != 0 {
		t.Fatalf("execution push %s: exit %d: %s", pushTriggerExecutionRef, code, text)
	}
	executionEntry := fixture.waitJournal(t, pushTriggerExecutionRef, executionCommit)
	executionReport := fixture.waitReportEvent(t, executionEntry.VerificationID)
	if executionReport.CausationID != executionEntry.VerificationID || executionReport.CorrelationID != executionID {
		t.Fatalf("execution push report = %+v; want causation %s and correlation %s",
			executionReport, executionEntry.VerificationID, executionID)
	}

	// The human push: causation is the verification, no execution correlation.
	if text, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+pushTriggerHumanRef); code != 0 {
		t.Fatalf("human push %s: exit %d: %s", pushTriggerHumanRef, code, text)
	}
	humanEntry := fixture.waitJournal(t, pushTriggerHumanRef, humanCommit)
	humanReport := fixture.waitReportEvent(t, humanEntry.VerificationID)
	if humanReport.CausationID != humanEntry.VerificationID || humanReport.CorrelationID != "" {
		t.Fatalf("human push report = %+v; want causation %s and no correlation", humanReport, humanEntry.VerificationID)
	}

	var started api.ExecutionSummary
	fixture.git.WaitFor(t,
		fmt.Sprintf("pipeline %s execution started by the human push", applied.PipelineID),
		pushTriggerStartTimeout, func() bool {
			executions := fixture.pipelineExecutions(t, applied.PipelineID)
			if len(executions) == 0 {
				return false
			}
			started = executions[0]
			return true
		})
	t.Cleanup(func() { fixture.stopExecution(t, started.ExecutionID) })

	// The action checks the admitted inputs, so success proves the pipeline
	// was started for the human push's exact repository, ref and commit.
	fixture.git.WaitFor(t,
		fmt.Sprintf("pushed execution %s terminal", started.ExecutionID),
		pushTriggerStartTimeout, func() bool {
			result, publicError := fixture.REST(fixture.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
				ExecutionID: started.ExecutionID,
			}, nil, "")
			if publicError.Code != "" {
				return false
			}
			var current api.ExecutionInspectResult
			fixture.Decode(result, &current)
			switch current.State {
			case "succeeded":
				return true
			case "failed", "cancelled":
				t.Fatalf("pushed execution %s ended %s: %+v", started.ExecutionID, current.State, current.Failure)
			}
			return false
		})

	if executions := fixture.pipelineExecutions(t, applied.PipelineID); len(executions) != 1 {
		t.Fatalf("pipeline %s has %d executions %+v; want exactly the human push's one", applied.PipelineID, len(executions), executions)
	}
}

// TestFactoryHumanPushAfterManualStartAndUnmatchedReport is the real-stack
// regression for the trigger scanner mistaking a completed ordinary
// pipeline.start receipt for an unfinished push selection. Through public
// operations only: one ordinary inline start; a human push to a ref no pipeline
// subscribes to yet; the subscription applied afterwards; an
// execution-originated push to a subscribed ref; the matching human push; then
// a second distinct human push. The seed report must complete (or the later
// reports never run), the execution-originated push must start nothing, exactly
// one human-triggered execution must start with the pushed inputs, and the
// second human push must start a second execution.
func TestFactoryHumanPushAfterManualStartAndUnmatchedReport(t *testing.T) {
	fixture := startGitHarness(t)
	resourceID := fixture.git.ProjectResourceID

	const starter = "scanner-push-starter"
	fixture.MustREST(fixture.ProjectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: factory.ProjectID,
		ServiceID: starter,
		Roles:     []string{"engineer"},
	}, nil, "create-"+starter)

	// One ordinary inline start. Its completed pipeline.start receipt is the
	// production shape the trigger scanner must not mistake for an unfinished
	// push selection.
	inline := factoryInlineStart(t, fixture.Harness, fixture.ProjectToken, marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:     "inline",
			Image:    fixture.Image.Reference,
			Commands: []string{"true"},
		}},
	}), "scanner-inline-start")
	if inline.ExecutionID == "" || inline.Version != 1 {
		t.Fatalf("inline start = %+v; want execution version 1", inline)
	}

	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "scanner")
	seedCommit := fixture.commit(t, directory, "scanner unmatched seed")
	executionCommit := fixture.commit(t, directory, "scanner execution push")
	humanCommit := fixture.commit(t, directory, "scanner human push")
	secondCommit := fixture.commit(t, directory, "scanner second human push")

	// The human seed push targets a ref no pipeline subscribes to yet. The
	// accepted report precedes the subscription; it must start nothing but must
	// still complete, which the later reports prove.
	const seedRef = "refs/heads/work/scanner-unsubscribed"
	if text, code := fixture.push(directory, fixture.engineerToken, seedCommit+":"+seedRef); code != 0 {
		t.Fatalf("seed human push %s: exit %d: %s", seedRef, code, text)
	}
	seedEntry := fixture.waitJournal(t, seedRef, seedCommit)
	seedReport := fixture.waitReportEvent(t, seedEntry.VerificationID)
	if seedReport.CausationID != seedEntry.VerificationID || seedReport.CorrelationID != "" {
		t.Fatalf("seed push report = %+v; want causation %s and no correlation", seedReport, seedEntry.VerificationID)
	}

	// Apply the subscription after the seed report. The action pins the exact
	// pushed commit, so a successful execution proves the admitted inputs.
	applied := fixture.applyPipeline("scanner-push", []byte(factoryPushDocument(
		resourceID, starter, fixture.Image.Reference, humanCommit)))

	// An execution-originated push of a subscribed ref: the report carries the
	// verified pusher's execution as its correlation and starts nothing.
	executionID, executionToken := fixture.startExecution(t, "scanner-pusher")
	if text, code := fixture.push(directory, executionToken, executionCommit+":"+pushTriggerExecutionRef); code != 0 {
		t.Fatalf("execution push %s: exit %d: %s", pushTriggerExecutionRef, code, text)
	}
	executionEntry := fixture.waitJournal(t, pushTriggerExecutionRef, executionCommit)
	executionReport := fixture.waitReportEvent(t, executionEntry.VerificationID)
	if executionReport.CausationID != executionEntry.VerificationID || executionReport.CorrelationID != executionID {
		t.Fatalf("execution push report = %+v; want causation %s and correlation %s",
			executionReport, executionEntry.VerificationID, executionID)
	}

	// The matching human push: causation is the verification, no execution
	// correlation.
	if text, code := fixture.push(directory, fixture.engineerToken, humanCommit+":"+pushTriggerHumanRef); code != 0 {
		t.Fatalf("human push %s: exit %d: %s", pushTriggerHumanRef, code, text)
	}
	humanEntry := fixture.waitJournal(t, pushTriggerHumanRef, humanCommit)
	humanReport := fixture.waitReportEvent(t, humanEntry.VerificationID)
	if humanReport.CausationID != humanEntry.VerificationID || humanReport.CorrelationID != "" {
		t.Fatalf("human push report = %+v; want causation %s and no correlation", humanReport, humanEntry.VerificationID)
	}

	var first api.ExecutionSummary
	fixture.git.WaitFor(t,
		fmt.Sprintf("pipeline %s execution started by the matching human push", applied.PipelineID),
		pushTriggerStartTimeout, func() bool {
			executions := fixture.pipelineExecutions(t, applied.PipelineID)
			if len(executions) == 0 {
				return false
			}
			first = executions[0]
			return true
		})
	t.Cleanup(func() { fixture.stopExecution(t, first.ExecutionID) })
	fixture.waitExecutionSucceeded(t, first.ExecutionID)
	if executions := fixture.pipelineExecutions(t, applied.PipelineID); len(executions) != 1 {
		t.Fatalf("pipeline %s has %d executions %+v; want exactly the matching human push's one",
			applied.PipelineID, len(executions), executions)
	}

	// A second distinct human push proves the scanner kept progressing rather
	// than completing one event and stalling behind a poisoned receipt. The
	// definition is updated first so its action pins the second commit exactly.
	fixture.MustREST(fixture.ProjectToken, "pipeline.update", api.PipelineUpdateRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: applied.PipelineID,
		Name:       "scanner-push",
		YAML:       []byte(factoryPushDocument(resourceID, starter, fixture.Image.Reference, secondCommit)),
	}, &applied.Version, "scanner-update")
	if text, code := fixture.push(directory, fixture.engineerToken, secondCommit+":"+pushTriggerHumanRef); code != 0 {
		t.Fatalf("second human push %s: exit %d: %s", pushTriggerHumanRef, code, text)
	}
	secondEntry := fixture.waitJournal(t, pushTriggerHumanRef, secondCommit)
	secondReport := fixture.waitReportEvent(t, secondEntry.VerificationID)
	if secondReport.CorrelationID != "" {
		t.Fatalf("second human push report = %+v; want no execution correlation", secondReport)
	}
	var second api.ExecutionSummary
	fixture.git.WaitFor(t,
		fmt.Sprintf("pipeline %s second execution started by the second human push", applied.PipelineID),
		pushTriggerStartTimeout, func() bool {
			executions := fixture.pipelineExecutions(t, applied.PipelineID)
			if len(executions) != 2 {
				return false
			}
			for _, execution := range executions {
				if execution.ExecutionID != first.ExecutionID {
					second = execution
				}
			}
			return second.ExecutionID != ""
		})
	t.Cleanup(func() { fixture.stopExecution(t, second.ExecutionID) })
	fixture.waitExecutionSucceeded(t, second.ExecutionID)
	if executions := fixture.pipelineExecutions(t, applied.PipelineID); len(executions) != 2 {
		t.Fatalf("pipeline %s has %d executions %+v; want the two human pushes only",
			applied.PipelineID, len(executions), executions)
	}
}

// factoryPushDocument renders the push pipeline whose action succeeds only when
// the admitted repository, ref and commit are the exact pushed values.
func factoryPushDocument(resourceID, starter, image, commit string) string {
	return fmt.Sprintf(`repository: %s
service: %s
on:
  push: [%s, %s]
inputs: [repository, ref, commit, previous_commit]
actions:
  - name: check
    image: %s
    timeout: 10m
    commands:
      - 'test "$(cat /kodama/inputs/repository)" = %s'
      - 'test "$(cat /kodama/inputs/ref)" = %s'
      - 'test "$(cat /kodama/inputs/commit)" = %s'
`, resourceID, starter, pushTriggerExecutionRef, pushTriggerHumanRef, image, resourceID, pushTriggerHumanRef, commit)
}

// factoryInlineStart posts one inline-YAML pipeline.start through the public
// client transport, which selects the task-free alternate REST binding
// (/api/v1/projects/{project}/executions) when the input carries YAML. The
// shared harness edge helper expands only canonical routes, and this regression
// needs a real ordinary start receipt.
func factoryInlineStart(t *testing.T, harness *factory.Harness, token string, document []byte, key string) api.PipelineStartResult {
	t.Helper()
	result, publicError, err := harness.Invoke(t.Context(), token, "pipeline.start", api.PipelineStartRequest{
		ProjectID: factory.ProjectID,
		YAML:      document,
	}, nil, key)
	if err != nil {
		t.Fatalf("inline start: %v", err)
	}
	if publicError.Code != "" {
		t.Fatalf("inline start refused: %s (%s)", publicError.Code, publicError.Message)
	}
	var started api.PipelineStartResult
	harness.Decode(result, &started)
	if err := started.Validate(); err != nil {
		t.Fatalf("inline start body: %v", err)
	}
	return started
}

// waitExecutionSucceeded waits for one execution to reach the succeeded state
// and returns its final inspection. A failed or cancelled state fails the test.
func (fixture *gitHarness) waitExecutionSucceeded(t *testing.T, executionID string) api.ExecutionInspectResult {
	t.Helper()
	var found api.ExecutionInspectResult
	fixture.git.WaitFor(t,
		fmt.Sprintf("execution %s succeeded", executionID),
		pushTriggerStartTimeout, func() bool {
			current, ok := fixture.inspectExecution(t, executionID)
			if !ok {
				return false
			}
			switch current.State {
			case "succeeded":
				found = current
				return true
			case "failed", "cancelled":
				t.Fatalf("execution %s ended %s: %+v", executionID, current.State, current.Failure)
			}
			return false
		})
	return found
}

// pipelineExecutions lists the executions of one pipeline.
func (fixture *gitHarness) pipelineExecutions(t *testing.T, pipelineID string) []api.ExecutionSummary {
	t.Helper()
	result := fixture.MustREST(fixture.ProjectToken, "execution.list", api.ExecutionListRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	}, nil, "")
	var list api.ExecutionListResult
	fixture.Decode(result, &list)
	return list.Executions
}
