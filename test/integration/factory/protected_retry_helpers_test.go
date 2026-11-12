//go:build integration

package factorytest

// Failed-execution retry and cross-execution approval refusal on the protected
// first-factory path.
//
// The shared scenario composes the protected-path utilities. It never injects
// an approval, a verification row or a Git ref: one real task runs the protected
// pipeline through real containers, the human pass is a served
// execution.request.resolve, and every protected push is decided by the managed
// hook against the Shell verification fact.
//
// Two execution-scoped approval properties are exercised here:
//
//   - a retry of the same task is a NEW execution. The approval recorded by the
//     earlier execution is not read as evidence for the retry, even though the
//     retry can present the earlier execution's exact candidate;
//   - the approval read is scoped to the asking execution, so a genuine pass
//     recorded for another execution (foreign, or an earlier attempt of the
//     same task) never authorizes the protected ref.
//
// The scenario stamps each implement commit with its own execution identity, so
// a retry always produces a distinct candidate even within one wall-clock
// second. The earlier candidate is retained on a separate granted work ref so
// the retry can attempt the exact prior effect without inventing an object.
//
// The one injected failure is a test-side file in the first attempt's protected
// integrate container. It runs only after the genuine pass, so the execution
// fails with its approval already durable. No product failure is simulated.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// protectedRetryWorkRef is the granted work branch each attempt pushes.
	protectedRetryWorkRef = "refs/heads/work/protected-retry"
	// protectedRetryPriorRef retains the first attempt's candidate so the retry
	// can present the exact prior effect to the protected ref.
	protectedRetryPriorRef = "refs/heads/work/protected-retry-prior"
	// protectedRetryControlRef is the granted work ref a refused attempt pushes
	// afterwards. It proves the live credential and the served edge still allow
	// a write, so the refusal belongs to the approval, not to broken authority.
	protectedRetryControlRef = "refs/heads/work/protected-retry-control"
	// protectedRetryPipelineName and protectedRetryTaskTitle name the one
	// retried pipeline and task.
	protectedRetryPipelineName = "protected-retry-pipeline"
	protectedRetryTaskTitle    = "protected failed-execution retry"

	// protectedRetryImplementGate, TestGate and IntegrateGate are the per-action
	// release files. The test creates one only after observing the predecessor's
	// durable state.
	protectedRetryImplementGate = "/workspace/protected-retry-implement-ready"
	protectedRetryTestGate      = "/workspace/protected-retry-test-ready"
	protectedRetryIntegrateGate = "/workspace/protected-retry-integrate-ready"

	// protectedRetryRetainPrior tells the first implement action to keep its
	// candidate on protectedRetryPriorRef.
	protectedRetryRetainPrior = "/workspace/protected-retry-retain-prior"
	// protectedRetryProbe tells a retry's test action to attempt the prior
	// execution's exact candidate on the protected ref.
	protectedRetryProbe = "/workspace/protected-retry-probe-prior"
	// protectedRetryProbeError captures the managed-hook diagnostic of that
	// attempt.
	protectedRetryProbeError = "/workspace/protected-retry-probe.err"
	// protectedRetryProbeStop fails the retry's test action after the refused
	// probe, so the prior-approval case has a live-execution refusal without a
	// cancel operation of its own.
	protectedRetryProbeStop = "/workspace/protected-retry-probe-stop"
	// protectedRetryFailIntegrate makes the first attempt's integrate action
	// fail after the genuine pass, before any protected push.
	protectedRetryFailIntegrate = "/workspace/protected-retry-fail-integrate"
	// protectedRetryAttemptFile is committed into every implement candidate so
	// two attempts can never produce the same object identity.
	protectedRetryAttemptFile = "kodama-attempt.txt"
)

// protectedRetryAttemptOptions selects the durable checkpoint one attempt
// drives.
type protectedRetryAttemptOptions struct {
	// retainPrior keeps this attempt's candidate on protectedRetryPriorRef.
	retainPrior bool
	// probePrior attempts the earlier execution's exact candidate on the
	// protected ref from this attempt's live test action.
	probePrior bool
	// stopAfterProbe fails the test action after the refused probe. The
	// prior-approval subtest uses it instead of cancelling the execution.
	stopAfterProbe bool
	// failAfterPass fails the integrate action after the genuine review pass.
	failAfterPass bool
	// finish resolves the review and integrates this attempt's own candidate.
	finish bool
}

// protectedRetryAttempt is the durable observation of one task execution.
type protectedRetryAttempt struct {
	executionID string
	candidate   string

	testAction      api.ExecutionAction
	reviewAction    api.ExecutionAction
	integrateAction api.ExecutionAction

	probeRefused         factory.VerificationRow
	probeRefusedApproval factory.ApprovalAssignment

	finished api.ExecutionInspectResult
	logs     string
}

// protectedRetryPipeline renders the one protected pipeline both attempts run.
// The review subject is the canonical five-field effect of this attempt's own
// implement output, so a fresh pass is always a fresh subject.
func protectedRetryPipeline(t *testing.T, fixture *gitHarness, base string) []byte {
	t.Helper()
	image := fixture.git.EnsureWorkloadImage(t)
	if image.RunnableReference == "" || !strings.Contains(image.RunnableReference, "@sha256:") {
		t.Fatalf("workload image reference %q is not digest-pinned", image.RunnableReference)
	}

	spec := factory.WorkloadCandidateSpec{
		WorkRef:      protectedRetryWorkRef,
		ExpectedBase: base,
		RequireTask:  true,
	}
	implement := protectedRetryImplementCommands(t, fixture, spec)
	tested := protectedRetryTestCommands(t, fixture, spec, base)
	integrated := protectedRetryIntegrateCommands(t, fixture, spec)

	document := protectedGoldenPipelineDoc{
		Inputs: []string{"repository", "target_ref", "expected_base"},
		Actions: []pipelineActionDoc{
			{
				Name:     "implement",
				Image:    image.RunnableReference,
				Timeout:  protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(protectedRetryImplementGate, implement),
				Outputs:  []string{"candidate"},
			},
			{
				Name:     "test",
				Image:    image.RunnableReference,
				Timeout:  protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(protectedRetryTestGate, tested),
				Outputs:  []string{"candidate", "test_result"},
			},
			{
				Name:    "review",
				Request: "role/reviewer",
				Timeout: protectedGoldenRequestTimeout,
				Subject: protectedGoldenReviewSubject(),
			},
			{
				Name:     "integrate",
				Image:    image.RunnableReference,
				Timeout:  protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(protectedRetryIntegrateGate, integrated),
			},
		},
	}
	return marshalProtectedGoldenPipeline(t, document)
}

// protectedRetryForeignPipeline renders the victim pipeline of the foreign
// approval case: real implement and test actions, then one protected push by a
// live execution that holds no approval of its own.
func protectedRetryForeignPipeline(t *testing.T, fixture *gitHarness, base string) []byte {
	t.Helper()
	image := fixture.git.EnsureWorkloadImage(t)
	if image.RunnableReference == "" || !strings.Contains(image.RunnableReference, "@sha256:") {
		t.Fatalf("workload image reference %q is not digest-pinned", image.RunnableReference)
	}
	spec := factory.WorkloadCandidateSpec{
		WorkRef:      protectedRetryWorkRef,
		ExpectedBase: base,
		RequireTask:  true,
	}
	document := protectedGoldenPipelineDoc{
		Inputs: []string{"repository", "target_ref", "expected_base"},
		Actions: []pipelineActionDoc{
			{
				Name:    "implement",
				Image:   image.RunnableReference,
				Timeout: protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(protectedRetryImplementGate,
					protectedRetryImplementCommands(t, fixture, spec)),
				Outputs: []string{"candidate"},
			},
			{
				Name:    "test",
				Image:   image.RunnableReference,
				Timeout: protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(protectedRetryTestGate,
					protectedRetryTestCommands(t, fixture, spec, base)),
				Outputs: []string{"candidate", "test_result"},
			},
			{
				Name:    "integrate",
				Image:   image.RunnableReference,
				Timeout: protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(protectedRetryIntegrateGate,
					protectedSubjectRefusalIntegrateCommands(fixture, spec, protectedRetryControlRef)),
			},
		},
	}
	return marshalProtectedGoldenPipeline(t, document)
}

// protectedRetryStartTask starts one task at the supplied observed lifecycle
// version and waits for the task's current execution to advance.
func protectedRetryStartTask(t *testing.T, fixture *gitHarness, token, taskID, key string, expected *uint64) string {
	t.Helper()
	started := api.TaskStartResult{}
	fixture.Decode(fixture.MustREST(token, "task.start", api.TaskStartRequest{
		TaskID: taskID,
	}, expected, key), &started)
	if started.ExecutionID == "" {
		t.Fatalf("task.start %q returned no execution", key)
	}
	waitProtectedGoldenTask(t, fixture, fixture.engineerToken, taskID,
		"task execution="+started.ExecutionID+" after "+key, func(read api.TaskReadResult) bool {
			return read.CurrentExecution == started.ExecutionID
		})
	return started.ExecutionID
}

// protectedRetryImplementCommands authors the implement action. It adds the
// execution stamp before the one commit, force-updates the granted work branch
// on a retry (RW+ allows the rewind), and retains its candidate for the retry's
// prior-approval probe only when the test asks it to.
func protectedRetryImplementCommands(t *testing.T, fixture *gitHarness, spec factory.WorkloadCandidateSpec) []string {
	t.Helper()
	commands, err := fixture.git.WorkloadCandidateCommands(t, spec)
	if err != nil {
		t.Fatalf("author the retry implement workload commands: %v", err)
	}

	stamped := make([]string, 0, len(commands)+2)
	for _, command := range commands {
		switch {
		case strings.HasPrefix(command, "chmod 0755 ") && strings.Contains(command, "kodama-test.sh"):
			stamped = append(stamped, command)
			stamped = append(stamped, "printf '%s\\n' \"$KODAMA_EXECUTION\" > "+protectedRetryAttemptFile)
			continue
		case strings.HasPrefix(command, "git add -- "):
			command += " " + protectedRetryAttemptFile
		case strings.Contains(command, "push --quiet origin") && strings.Contains(command, spec.WorkRef):
			command = strings.Replace(command, "push --quiet origin", "push --quiet --force origin", 1)
		}
		stamped = append(stamped, command)
	}
	stamped = append(stamped,
		"if [ -e "+protectedRetryRetainPrior+" ]; then\n"+
			"  git_kodama push --quiet origin \"HEAD:"+protectedRetryPriorRef+"\"\n"+
			"  printf 'prior-candidate-retained\\n'\n"+
			"fi",
	)
	return stamped
}

// protectedRetryTestCommands authors the test action. When the test creates the
// probe flag in this container, the action fetches the earlier attempt's exact
// candidate and attempts it on the protected ref. The attempt must be refused;
// an accepted push fails the action so a broken boundary cannot pass silently.
func protectedRetryTestCommands(t *testing.T, fixture *gitHarness, spec factory.WorkloadCandidateSpec, base string) []string {
	t.Helper()
	commands, err := fixture.git.WorkloadTestCandidateCommands(t, spec)
	if err != nil {
		t.Fatalf("author the retry test workload commands: %v", err)
	}
	commands = append(commands,
		"if [ -e "+protectedRetryProbe+" ]; then\n"+
			"  git_kodama fetch --quiet origin "+shellQuoteForSubject(protectedRetryPriorRef)+"\n"+
			"  kodama_retry_prior=$(git rev-parse FETCH_HEAD)\n"+
			"  test -n \"$kodama_retry_prior\"\n"+
			"  if git_kodama push --quiet "+
			shellQuoteForSubject("--force-with-lease="+gitMainRef+":"+base)+
			" origin \"$kodama_retry_prior\":"+gitMainRef+" 2>"+protectedRetryProbeError+"; then\n"+
			"    printf 'prior-approval-push-accepted\\n' >&2\n"+
			"    exit 1\n"+
			"  fi\n"+
			"  printf 'prior-approval-push-refused\\n'\n"+
			"  if [ -e "+protectedRetryProbeStop+" ]; then\n"+
			"    printf 'prior-approval-refused-stop\\n' >&2\n"+
			"    exit 1\n"+
			"  fi\n"+
			"fi",
	)
	return commands
}

// protectedRetryIntegrateCommands authors the integrate action. When the test
// creates the failure flag in this container, the action fails before any
// protected push, after the already-recorded genuine pass.
func protectedRetryIntegrateCommands(t *testing.T, fixture *gitHarness, spec factory.WorkloadCandidateSpec) []string {
	t.Helper()
	commands, err := fixture.git.WorkloadIntegrateCandidateCommands(t, spec)
	if err != nil {
		t.Fatalf("author the retry integrate workload commands: %v", err)
	}
	failure := "if [ -e " + protectedRetryFailIntegrate + " ]; then\n" +
		"  printf 'retry-injected-integrate-failure\\n' >&2\n" +
		"  exit 1\n" +
		"fi"
	return append([]string{failure}, commands...)
}

// protectedRetryCreateTask creates the one open task and returns its lifetime
// start version plus the declared inputs.
func protectedRetryCreateTask(t *testing.T, fixture *gitHarness, pipelineID string) (string, uint64, map[string]string) {
	t.Helper()
	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded project main ref is missing")
	}
	inputs := map[string]string{
		"repository":    fixture.git.ProjectResourceID,
		"target_ref":    gitMainRef,
		"expected_base": base,
	}
	created := api.TaskCreateResult{}
	fixture.Decode(fixture.MustREST(fixture.engineerToken, "task.create", api.TaskCreateRequest{
		ProjectID:  factory.ProjectID,
		Title:      protectedRetryTaskTitle,
		PipelineID: pipelineID,
		Inputs:     inputs,
	}, nil, "create-protected-retry"), &created)
	if created.TaskID == "" || created.LifecycleVersion == 0 {
		t.Fatalf("task.create returned %+v; want a committed task", created)
	}
	read := readProtectedGoldenTask(t, fixture, fixture.engineerToken, created.TaskID)
	if read.State != "open" || read.CurrentExecution != "" || read.SelectedPipeline != pipelineID {
		t.Fatalf("created retry task = %+v; want open with the selected pipeline and no execution", read)
	}
	if !reflectEqualInputs(read.Inputs, inputs) {
		t.Fatalf("created retry task inputs = %v; want %v", read.Inputs, inputs)
	}
	return created.TaskID, created.LifecycleVersion, inputs
}

// protectedRetryRunAttempt starts one execution of the task and drives it to
// the selected checkpoint. It never retries a refused operation: the only
// retry is the existing version-conflict convergence for a resolve envelope.
func protectedRetryRunAttempt(
	t *testing.T,
	fixture *gitHarness,
	reviewerToken string,
	taskID string,
	startToken string,
	startKey string,
	expectedVersion *uint64,
	base string,
	options protectedRetryAttemptOptions,
	priorCandidate string,
) protectedRetryAttempt {
	t.Helper()
	started := api.TaskStartResult{}
	fixture.Decode(fixture.MustREST(startToken, "task.start", api.TaskStartRequest{
		TaskID: taskID,
	}, expectedVersion, startKey), &started)
	if started.ExecutionID == "" {
		t.Fatalf("task.start %q returned no execution", startKey)
	}
	attempt := protectedRetryAttempt{executionID: started.ExecutionID}
	waitProtectedGoldenTask(t, fixture, fixture.engineerToken, taskID,
		"task execution="+started.ExecutionID+" after "+startKey, func(read api.TaskReadResult) bool {
			return read.CurrentExecution == started.ExecutionID
		})

	// Implement: task context and mounted credential, then the candidate push.
	waitProtectedActionRunning(t, fixture, attempt.executionID, "implement")
	implementContainer := waitProtectedGoldenContainer(t, fixture, attempt.executionID, "implement")
	implementToken := waitProtectedGoldenCredential(t, fixture, attempt.executionID, "implement")
	if got := protectedGoldenExec(t, fixture, implementContainer, "printenv", "KODAMA_TASK"); got != taskID {
		t.Fatalf("%s implement KODAMA_TASK = %q; want the task %q", startKey, got, taskID)
	}
	if snapshot := protectedGoldenRead(t, fixture, implementContainer, protectedGoldenTaskFile); !json.Valid([]byte(snapshot)) {
		t.Fatalf("%s implement /kodama/task.json is not valid JSON: %q", startKey, snapshot)
	}
	if options.retainPrior {
		if err := fixture.TouchContainerFile(t.Context(), implementContainer, protectedRetryRetainPrior); err != nil {
			t.Fatalf("%s: arm the prior-candidate retention: %v", startKey, err)
		}
	}
	requireProtectedGoldenCredentialUsable(t, fixture, attempt.executionID, implementToken)
	releaseProtectedGoldenGate(t, fixture, implementContainer, protectedRetryImplementGate)

	// Test: the retained checkout and materialized implement output.
	waitProtectedActionRunning(t, fixture, attempt.executionID, "test")
	testRunning := inspectProtectedGoldenExecution(t, fixture, fixture.ProjectToken, attempt.executionID)
	attempt.testAction = actionByName(t, testRunning, "test")
	if attempt.testAction.ActionID == "" {
		t.Fatalf("%s test action has no identity", startKey)
	}
	testContainer := waitProtectedGoldenContainer(t, fixture, attempt.executionID, "test")
	_ = waitProtectedGoldenCredential(t, fixture, attempt.executionID, "test")
	requireProtectedGoldenMarker(t, fixture, testContainer, attempt.executionID)
	attempt.candidate = protectedGoldenRead(t, fixture, testContainer, protectedGoldenImplementCandidatePath)
	if attempt.candidate == "" || attempt.candidate == base {
		t.Fatalf("%s implement candidate = %q; want a commit different from the base %q",
			startKey, attempt.candidate, base)
	}
	requireProtectedGoldenHead(t, fixture, testContainer, attempt.candidate)
	if options.probePrior {
		if err := fixture.TouchContainerFile(t.Context(), testContainer, protectedRetryProbe); err != nil {
			t.Fatalf("%s: arm the prior-approval probe: %v", startKey, err)
		}
		if options.stopAfterProbe {
			if err := fixture.TouchContainerFile(t.Context(), testContainer, protectedRetryProbeStop); err != nil {
				t.Fatalf("%s: arm the post-probe stop: %v", startKey, err)
			}
		}
	}
	releaseProtectedGoldenGate(t, fixture, testContainer, protectedRetryTestGate)

	if options.stopAfterProbe {
		attempt.finished = waitProtectedRetryTerminal(t, fixture, attempt.executionID)
		attempt.logs = fixture.executionLogs(t, attempt.finished)
		attempt.probeRefused, attempt.probeRefusedApproval = protectedRetryWireEvidence(t, fixture,
			attempt.executionID, attempt.testAction.ActionID, priorCandidate)
		return attempt
	}

	// The retry's test action must reach the review only if the probe refused
	// the prior execution's approval.
	if options.probePrior {
		waitProtectedActionRunning(t, fixture, attempt.executionID, "review")
		requireProtectedGoldenNoWorkload(t, fixture, attempt.executionID)
		attempt.probeRefused, attempt.probeRefusedApproval = protectedRetryWireEvidence(t, fixture,
			attempt.executionID, attempt.testAction.ActionID, priorCandidate)
	}

	if options.failAfterPass || options.finish {
		waitProtectedActionRunning(t, fixture, attempt.executionID, "review")
		requireProtectedGoldenNoWorkload(t, fixture, attempt.executionID)
		attempt.reviewAction = protectedRetryResolve(t, fixture, reviewerToken,
			attempt.executionID, "review", "resolve-"+attempt.executionID)
	}

	if options.failAfterPass {
		waitProtectedActionRunning(t, fixture, attempt.executionID, "integrate")
		integrateContainer := waitProtectedGoldenContainer(t, fixture, attempt.executionID, "integrate")
		_ = waitProtectedGoldenCredential(t, fixture, attempt.executionID, "integrate")
		if err := fixture.TouchContainerFile(t.Context(), integrateContainer, protectedRetryFailIntegrate); err != nil {
			t.Fatalf("%s: arm the post-pass integrate failure: %v", startKey, err)
		}
		releaseProtectedGoldenGate(t, fixture, integrateContainer, protectedRetryIntegrateGate)
		attempt.finished = waitProtectedGoldenTerminal(t, fixture, attempt.executionID)
		attempt.integrateAction = actionByName(t, attempt.finished, "integrate")
		attempt.logs = fixture.executionLogs(t, attempt.finished)
		return attempt
	}

	if !options.finish {
		t.Fatalf("%s: attempt options select no checkpoint", startKey)
	}

	// Integrate: retained workspace, both settled outputs, exact candidate.
	waitProtectedActionRunning(t, fixture, attempt.executionID, "integrate")
	integrateContainer := waitProtectedGoldenContainer(t, fixture, attempt.executionID, "integrate")
	_ = waitProtectedGoldenCredential(t, fixture, attempt.executionID, "integrate")
	requireProtectedGoldenMarker(t, fixture, integrateContainer, attempt.executionID)
	requireProtectedGoldenHead(t, fixture, integrateContainer, attempt.candidate)
	if got := protectedGoldenRead(t, fixture, integrateContainer, protectedGoldenTestCandidatePath); got != attempt.candidate {
		t.Fatalf("%s materialized test candidate = %q; want %q", startKey, got, attempt.candidate)
	}
	if got := protectedGoldenRead(t, fixture, integrateContainer, protectedGoldenTestResultPath); got != "pass:"+attempt.candidate {
		t.Fatalf("%s materialized test_result = %q; want pass:%s", startKey, got, attempt.candidate)
	}
	releaseProtectedGoldenGate(t, fixture, integrateContainer, protectedRetryIntegrateGate)
	attempt.finished = waitProtectedGoldenTerminal(t, fixture, attempt.executionID)
	attempt.integrateAction = actionByName(t, attempt.finished, "integrate")
	attempt.logs = fixture.executionLogs(t, attempt.finished)
	return attempt
}

// protectedRetryResolve answers one outstanding request with the exact human
// pass. Only an explicit version conflict is retried.
func protectedRetryResolve(t *testing.T, fixture *gitHarness, token, executionID, actionName, key string) api.ExecutionAction {
	t.Helper()
	for range 30 {
		current := inspectProtectedGoldenExecution(t, fixture, token, executionID)
		if current.State != "running" {
			t.Fatalf("execution %s state = %q before resolving %q; want running",
				executionID, current.State, actionName)
		}
		action := actionByName(t, current, actionName)
		result, publicError := fixture.REST(token, "execution.request.resolve", api.ExecutionRequestResolveRequest{
			ExecutionID:           executionID,
			ActionID:              action.ActionID,
			ExpectedActionVersion: action.Version,
			Decision:              "pass",
		}, &current.Version, key)
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("execution.request.resolve %q refused: %s (%s)",
				actionName, publicError.Code, publicError.Message)
		}
		var resolved api.ExecutionRequestResolveResult
		fixture.Decode(result, &resolved)
		if resolved.ExecutionVersion <= current.Version || resolved.ActionVersion <= action.Version {
			t.Fatalf("resolve %q versions = %+v; want both advanced past execution=%d action=%d",
				actionName, resolved, current.Version, action.Version)
		}
		return actionByName(t, inspectProtectedGoldenExecution(t, fixture, token, executionID), actionName)
	}
	t.Fatalf("execution.request.resolve %q did not converge on a current version", actionName)
	return api.ExecutionAction{}
}

// waitProtectedRetryTerminal waits for any terminal execution state without
// requiring scheduled cleanup. The prior-approval retry intentionally stops its
// test action after the refused probe; terminal state is the evidence and the
// subtest makes no cleanup-completion claim.
func waitProtectedRetryTerminal(t *testing.T, fixture *gitHarness, executionID string) api.ExecutionInspectResult {
	t.Helper()
	var found api.ExecutionInspectResult
	fixture.git.WaitFor(t, "terminal execution="+executionID, protectedGoldenTimeout, func() bool {
		current, ok := fixture.inspectExecution(t, executionID)
		if !ok {
			return false
		}
		switch current.State {
		case "succeeded", "failed", "cancelled":
			found = current
			return true
		}
		return false
	})
	return found
}

// protectedRetryFailedTask reads the operator's failed_task discovery for the
// exact task. Its Version is the task lifecycle version task.start must name.
func protectedRetryFailedTask(t *testing.T, fixture *gitHarness, taskID string) api.InboxItem {
	t.Helper()
	var found api.InboxItem
	fixture.git.WaitFor(t, "failed_task inbox item for "+taskID, protectedGoldenTimeout, func() bool {
		result, publicError := fixture.REST(fixture.OperatorToken, "task.inbox", api.TaskInboxRequest{
			ProjectID: factory.ProjectID,
			Kinds:     []string{"failed_task"},
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		var inbox api.TaskInboxResult
		fixture.Decode(result, &inbox)
		for _, item := range inbox.Items {
			if item.Kind == "failed_task" && item.TaskID == taskID {
				found = item
				return true
			}
		}
		return false
	})
	return found
}

// protectedRetryRunForeignApproval starts one task-free execution whose only
// action is a genuine role/reviewer pass for exactly this effect, then returns
// the durable request action. The asking execution is deliberately different.
func protectedRetryRunForeignApproval(t *testing.T, fixture *gitHarness, reviewerToken string, base, candidate string) (string, api.ExecutionAction) {
	t.Helper()
	document := protectedGoldenPipelineDoc{
		Actions: []pipelineActionDoc{
			{
				Name:    "approve",
				Request: "role/reviewer",
				Timeout: protectedGoldenRequestTimeout,
				Subject: protectedRefEffectSubject(fixture, base, candidate),
			},
		},
	}
	applied := fixture.applyPipeline("protected-retry-foreign-approval", marshalProtectedGoldenPipeline(t, document))
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("foreign approval pipeline.apply returned %+v", applied)
	}
	started := fixture.startPipeline(applied.PipelineID, "start-protected-retry-foreign-approval")
	if started.ExecutionID == "" {
		t.Fatalf("foreign approval pipeline.start returned no execution")
	}
	waitProtectedActionRunning(t, fixture, started.ExecutionID, "approve")
	protectedRetryResolve(t, fixture, reviewerToken, started.ExecutionID, "approve",
		"resolve-foreign-"+started.ExecutionID)
	finished := waitProtectedGoldenTerminal(t, fixture, started.ExecutionID)
	action := actionByName(t, finished, "approve")
	if action.State != "succeeded" || action.Decision != "pass" {
		t.Fatalf("foreign approval action = %+v; want a succeeded pass", action)
	}
	return started.ExecutionID, action
}

// assertProtectedRetryApproval asserts one genuine human pass for the exact
// canonical effect of the supplied candidate.
func assertProtectedRetryApproval(t *testing.T, fixture *gitHarness, label string, action api.ExecutionAction, reviewerPrincipal, base, candidate string) {
	t.Helper()
	if action.Assignment != "role/reviewer" {
		t.Fatalf("%s assignment = %q; want role/reviewer", label, action.Assignment)
	}
	if action.State != "succeeded" || action.Decision != "pass" || action.AnsweredAt == nil {
		t.Fatalf("%s action = %+v; want a succeeded pass with an answered-at fact", label, action)
	}
	if action.Actor == "" || action.Actor != reviewerPrincipal {
		t.Fatalf("%s actor = %q; want the current reviewer %q", label, action.Actor, reviewerPrincipal)
	}
	if action.Subject == nil {
		t.Fatalf("%s carries no resolved subject", label)
	}
	requireProtectedGoldenCanonicalSubject(t, label, *action.Subject, protectedGoldenEvidence{
		fixture:   fixture,
		base:      base,
		candidate: candidate,
	})
}

// assertProtectedRetryRefusedEffect asserts the durable refused
// git.ref.update row for the exact effect named by base/candidate.
func assertProtectedRetryRefusedEffect(t *testing.T, fixture *gitHarness, label string, row factory.VerificationRow, approval factory.ApprovalAssignment, executionID, actionID, base, candidate string) {
	t.Helper()
	if row.ID == "" || row.Result != "refused" || row.Approved {
		t.Fatalf("%s refused row = %+v; want one refused, unapproved row", label, row)
	}
	if row.Operation != approvalOperation {
		t.Fatalf("%s refused operation = %q; want %q", label, row.Operation, approvalOperation)
	}
	if row.ExecutionID != executionID || row.ActionID != actionID {
		t.Fatalf("%s refused execution/action = %q/%q; want %q/%q",
			label, row.ExecutionID, row.ActionID, executionID, actionID)
	}
	if row.ServiceID != fixture.git.ServiceID ||
		row.ResourceID != fixture.git.ProjectResourceID ||
		row.BindingID != fixture.git.ProjectBindingID {
		t.Fatalf("%s refused service/resource/binding = %q/%q/%q; want the registered Git binding",
			label, row.ServiceID, row.ResourceID, row.BindingID)
	}
	var subject api.Subject
	if err := json.Unmarshal([]byte(row.Subject), &subject); err != nil {
		t.Fatalf("%s: decode refused subject %s: %v", label, row.Subject, err)
	}
	requireProtectedGoldenCanonicalSubject(t, label, subject, protectedGoldenEvidence{
		fixture:   fixture,
		base:      base,
		candidate: candidate,
	})
	if got := approval.String(); got != "role/reviewer" {
		t.Fatalf("%s requested approval = %q; want role/reviewer", label, got)
	}
	if approval.Kind != "role" || approval.Role != "reviewer" || approval.ServiceID != "" {
		t.Fatalf("%s requested approval = %+v; want exactly role/reviewer", label, approval)
	}
	if _, ok, err := fixture.git.ReportEvent(t.Context(), row.ID); err != nil {
		t.Fatalf("%s: read the refused verification's report event: %v", label, err)
	} else if ok {
		t.Fatalf("%s: refused verification %s produced a verified event", label, row.ID)
	}
}

// assertProtectedRetryProtectedRefUnchanged asserts the protected ref and its
// reported journal did not move between the two observations.
func assertProtectedRetryProtectedRefUnchanged(t *testing.T, fixture *gitHarness, label, base string, journalBefore int, candidate string) {
	t.Helper()
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != base {
		t.Fatalf("%s protected main = %q; want the unchanged base %q", label, main, base)
	}
	if after := protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef); after != journalBefore {
		t.Fatalf("%s reported protected journal entries = %d; want the unchanged %d", label, after, journalBefore)
	}
	if protectedEdgeHasAcceptedJournal(t, fixture, gitMainRef, candidate) {
		t.Fatalf("%s candidate %s was accepted or reported on %s despite the refusal",
			label, candidate, gitMainRef)
	}
}

// assertProtectedRetryControlPush asserts the refused execution's live
// credential still moved a granted work ref to the exact candidate.
func assertProtectedRetryControlPush(t *testing.T, fixture *gitHarness, ref, candidate string) {
	t.Helper()
	if got := fixture.git.RemoteRef(t, factory.GitProjectRepository, ref); got != candidate {
		t.Fatalf("control work ref %s = %q; want the candidate %q", ref, got, candidate)
	}
}

// protectedRetryWireEvidence reads the exact refused row and approval for one
// action effect.
func protectedRetryWireEvidence(t *testing.T, fixture *gitHarness, executionID, actionID, candidate string) (factory.VerificationRow, factory.ApprovalAssignment) {
	t.Helper()
	row := waitProtectedEdgeRefusedUpdate(t, fixture, executionID, actionID, candidate)
	approval, err := fixture.git.RequestedApproval(t.Context(), row.ID)
	if err != nil {
		t.Fatalf("read refused verification %s's requested approval: %v", row.ID, err)
	}
	return row, approval
}

// protectedRetryJournalCount is a named read of the protected reported count.
func protectedRetryJournalCount(t *testing.T, fixture *gitHarness) int {
	t.Helper()
	return protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef)
}

// protectedRetryUserlandJournalEntry finds the reported journal entry for one
// ref and candidate.
func protectedRetryUserlandJournalEntry(t *testing.T, fixture *gitHarness, ref, candidate string) git.JournalEntry {
	t.Helper()
	return waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, ref, candidate)
}
