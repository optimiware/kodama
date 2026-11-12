//go:build integration

package factorytest

// Shared scenario and evidence helpers for terminal decisions, moved refs and
// current committed rules on the protected first-factory path.
//
// Everything here composes the shared protected-path utilities and the authored
// workload commands. It never injects an approval, a verification row or a ref:
// the scenario drives one real task-associated execution (implement -> test ->
// review -> integrate), and the ref-rule helpers obtain real human request
// passes for exact effect subjects through execution.request.resolve, then push
// with the live execution credential through the served Git edge. The only
// non-container pushes are the host-side ones that exercise the same served
// smart-HTTP listener, managed hooks and Shell verification; they are labelled
// host-side in the test evidence.
//
// The checkpoints are deliberately composable so each named test can
// stop at the exact fact it asserts:
//
//   - runProtectedRefScenario drives implement/test to the review wait and
//     returns every durable identity;
//   - finishProtectedRef answers pass (the control), prepares and releases the
//     integrate checkpoint, then collects the positive-flow evidence;
//   - prepareProtectedRefIntegrate / collectProtectedRefFlow expose those two
//     halves so a test can change a committed rule or move the target between
//     the approval and the push;
//   - commitProtectedRefPolicy installs a changed admin/conf/git.conf through a
//     real served admin push, and the helper asserts the accepted blob after
//     every change.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// protectedRefReviewAction, protectedRefIntegrateAction and the workload
	// action names are the declared action names these tests drive.
	protectedRefReviewAction    = "review"
	protectedRefSecondReview    = "review-second"
	protectedRefIntegrateAction = "integrate"
	protectedRefHoldAction      = "hold"
	protectedRefSecondHold      = "hold-second"

	// protectedRefRequestTimeout keeps the request well beyond the real
	// PostgreSQL statement clock; the expiry scenario overrides it with the
	// one-minute minimum.
	protectedRefRequestTimeout = protectedGoldenRequestTimeout

	// protectedRefPolicyErrorPath, protectedRefPolicyRefusedPath and
	// protectedRefPolicyRestoreGate are the two-phase integrate checkpoint for
	// the committed-rule scenario. The container writes the managed-hook
	// refusal, the test restores the accepted policy, then releases the second
	// push.
	protectedRefPolicyErrorPath   = "/workspace/protected-policy.err"
	protectedRefPolicyRefusedPath = "/workspace/protected-policy-refused"
	protectedRefPolicyRestoreGate = "/workspace/protected-policy-restore"

	// protectedRefMoved* are the moved-target checkpoint paths: the client-side
	// lease refusal, the server-side hook refusal, and the finish gate that
	// keeps the container alive while the test reads both diagnostics.
	protectedRefMovedLeaseError    = "/workspace/protected-moved-lease.err"
	protectedRefMovedLeaseRefused  = "/workspace/protected-moved-lease-refused"
	protectedRefMovedHookError     = "/workspace/protected-moved-hook.err"
	protectedRefMovedHookRefused   = "/workspace/protected-moved-hook-refused"
	protectedRefMovedCheckpoint    = "/workspace/protected-moved-checkpoint"
	protectedRefMovedFinishGate    = "/workspace/protected-moved-finish"
	protectedRefHumanControlPrefix = "refs/heads/work/protected-"
)

// protectedRefHumanMainPolicy grants the engineer an ordinary write of the
// approval-guarded main ref while keeping the reviewer approval option and the
// admin repository's write rule. It is installed through a served admin push,
// so the human permission is real committed RBAC, not a test bypass.
const protectedRefHumanMainPolicy = "repo admin\n" +
	"    RW = @admin\n" +
	"\n" +
	"repo project-i04/web\n" +
	"    R = @all\n" +
	"    RW+ refs/heads/work/ = @engineer @execution\n" +
	"    RW refs/heads/main = @engineer @execution\n" +
	"    option approve.refs/heads/main = reviewer\n"

// protectedRefAdminOnlyMainPolicy removes execution write permission from the
// protected main ref after an approval was recorded. The admin write rule and
// the project read rule are preserved so the change can be committed and later
// restored through the same served admin path.
const protectedRefAdminOnlyMainPolicy = "repo admin\n" +
	"    RW = @admin\n" +
	"\n" +
	"repo project-i04/web\n" +
	"    R = @all\n" +
	"    RW+ refs/heads/work/ = @engineer @execution\n" +
	"    RW refs/heads/main = @admin\n" +
	"    option approve.refs/heads/main = reviewer\n"

// protectedRefGates is the per-scenario set of release files. The test creates
// one only after it observed the predecessor's durable state.
type protectedRefGates struct {
	implement string
	test      string
	integrate string
}

// protectedRefScenarioSpec configures one protected ref scenario.
type protectedRefScenarioSpec struct {
	pipelineName   string
	taskTitle      string
	workRef        string
	requestTimeout string
	// integrate replaces the standard authored push command when non-nil. It
	// receives the standard command list and the reviewed base so the
	// checkpoint keeps every earlier exact-candidate and ancestry check.
	integrate func(t *testing.T, standard []string, base string) []string
}

// protectedRefEvidence is every durable observation these tests need. It never
// carries a synthesized fact.
type protectedRefEvidence struct {
	fixture *gitHarness
	spec    protectedRefScenarioSpec
	gates   protectedRefGates

	pipelineID  string
	taskID      string
	executionID string
	base        string
	candidate   string
	workRef     string

	engineerToken     string
	engineerPrincipal string
	reviewerToken     string
	reviewerPrincipal string

	executionToken string
	testToken      string
	integrateToken string

	implement api.ExecutionAction
	test      api.ExecutionAction
	review    api.ExecutionAction
	integrate api.ExecutionAction
	finished  api.ExecutionInspectResult
	logs      string

	// pendingVersion is the execution version observed while the review waits;
	// a terminal decision or a stale-version attempt quotes it.
	pendingVersion uint64
}

// protectedRefGatePaths derives the release paths from one fixture-unique
// pipeline name.
func protectedRefGatePaths(pipelineName string) protectedRefGates {
	root := "/workspace/" + pipelineName
	return protectedRefGates{
		implement: root + "-implement-ready",
		test:      root + "-test-ready",
		integrate: root + "-integrate-ready",
	}
}

// runProtectedRefScenario drives one task-associated protected execution to the
// review wait. The implement container pushes C to the granted work ref and
// submits its declared output; the test container proves it retained exactly C
// and submits the passing result. The review request then waits with no
// workload and main still at B.
func runProtectedRefScenario(t *testing.T, fixture *gitHarness, spec protectedRefScenarioSpec) protectedRefEvidence {
	t.Helper()
	if fixture == nil || fixture.git == nil {
		t.Fatalf("the protected ref scenario needs the Git fixture")
	}
	if spec.pipelineName == "" || spec.workRef == "" {
		t.Fatalf("the protected ref scenario needs a pipeline name and a work ref")
	}
	image := fixture.git.EnsureWorkloadImage(t)
	if image.RunnableReference == "" || !strings.Contains(image.RunnableReference, "@sha256:") {
		t.Fatalf("workload image reference %q is not digest-pinned", image.RunnableReference)
	}
	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded project main ref is missing")
	}

	evidence := protectedRefEvidence{
		fixture:           fixture,
		spec:              spec,
		gates:             protectedRefGatePaths(spec.pipelineName),
		base:              base,
		workRef:           spec.workRef,
		engineerToken:     fixture.engineerToken,
		engineerPrincipal: fixture.engineerPrincipal,
	}
	evidence.reviewerToken, evidence.reviewerPrincipal = grantProtectedReviewer(t, fixture, protectedEdgeReviewerSubject)
	if evidence.reviewerPrincipal == evidence.engineerPrincipal {
		t.Fatalf("the reviewer must be a separate human from the engineer")
	}

	workload := factory.WorkloadCandidateSpec{
		WorkRef:      spec.workRef,
		ExpectedBase: base,
		RequireTask:  true,
	}
	implement, err := fixture.git.WorkloadCandidateCommands(t, workload)
	if err != nil {
		t.Fatalf("author the implement workload commands: %v", err)
	}
	tested, err := fixture.git.WorkloadTestCandidateCommands(t, workload)
	if err != nil {
		t.Fatalf("author the test workload commands: %v", err)
	}
	integrated, err := fixture.git.WorkloadIntegrateCandidateCommands(t, workload)
	if err != nil {
		t.Fatalf("author the integrate workload commands: %v", err)
	}
	if spec.integrate != nil {
		integrated = spec.integrate(t, integrated, base)
	}

	requestTimeout := spec.requestTimeout
	if requestTimeout == "" {
		requestTimeout = protectedRefRequestTimeout
	}
	inputs := map[string]string{
		"repository":    fixture.git.ProjectResourceID,
		"target_ref":    gitMainRef,
		"expected_base": base,
	}
	document := protectedGoldenPipelineDoc{
		Inputs: []string{"repository", "target_ref", "expected_base"},
		Actions: []pipelineActionDoc{
			{
				Name:     "implement",
				Image:    image.RunnableReference,
				Timeout:  protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(evidence.gates.implement, implement),
				Outputs:  []string{"candidate"},
			},
			{
				Name:     "test",
				Image:    image.RunnableReference,
				Timeout:  protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(evidence.gates.test, tested),
				Outputs:  []string{"candidate", "test_result"},
			},
			{
				Name:    protectedRefReviewAction,
				Request: "role/reviewer",
				Timeout: requestTimeout,
				Subject: protectedGoldenReviewSubject(),
			},
			{
				Name:     protectedRefIntegrateAction,
				Image:    image.RunnableReference,
				Timeout:  protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(evidence.gates.integrate, integrated),
			},
		},
	}
	applied := fixture.applyPipeline(spec.pipelineName, marshalProtectedGoldenPipeline(t, document))
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("pipeline.apply returned %+v; want an admitted pipeline and version", applied)
	}
	evidence.pipelineID = applied.PipelineID

	created := api.TaskCreateResult{}
	fixture.Decode(fixture.MustREST(evidence.engineerToken, "task.create", api.TaskCreateRequest{
		ProjectID:  factory.ProjectID,
		Title:      spec.taskTitle,
		PipelineID: applied.PipelineID,
		Inputs:     inputs,
	}, nil, "create-"+spec.pipelineName), &created)
	evidence.taskID = created.TaskID
	if created.TaskID == "" || created.ContentVersion == 0 || created.LifecycleVersion == 0 {
		t.Fatalf("task.create returned %+v; want a committed task", created)
	}
	createdRead := readProtectedGoldenTask(t, fixture, evidence.engineerToken, evidence.taskID)
	if createdRead.State != "open" || createdRead.CurrentExecution != "" || len(createdRead.ExecutionHistory) != 0 {
		t.Fatalf("created task = %+v; want open with no execution", createdRead)
	}
	if createdRead.SelectedPipeline != applied.PipelineID || !reflectEqualInputs(createdRead.Inputs, inputs) {
		t.Fatalf("created task pipeline/inputs = %q/%v; want %q/%v",
			createdRead.SelectedPipeline, createdRead.Inputs, applied.PipelineID, inputs)
	}

	started := api.TaskStartResult{}
	fixture.Decode(fixture.MustREST(evidence.engineerToken, "task.start", api.TaskStartRequest{
		TaskID: evidence.taskID,
	}, &createdRead.LifecycleVersion, "start-"+spec.pipelineName), &started)
	evidence.executionID = started.ExecutionID
	if started.ExecutionID == "" || started.LifecycleVersion <= createdRead.LifecycleVersion {
		t.Fatalf("task.start returned %+v; want a new execution and an advanced lifecycle version", started)
	}
	waitProtectedGoldenTask(t, fixture, evidence.engineerToken, evidence.taskID,
		"task execution="+started.ExecutionID+" after task.start", func(read api.TaskReadResult) bool {
			return read.CurrentExecution == started.ExecutionID
		})

	// Implement: prove the task context and the mounted credential before the
	// candidate push is released.
	waitProtectedActionRunning(t, fixture, evidence.executionID, "implement")
	implementContainer := waitProtectedGoldenContainer(t, fixture, evidence.executionID, "implement")
	evidence.executionToken = waitProtectedGoldenCredential(t, fixture, evidence.executionID, "implement")
	if got := protectedGoldenExec(t, fixture, implementContainer, "printenv", "KODAMA_TASK"); got != evidence.taskID {
		t.Fatalf("implement KODAMA_TASK = %q; want the task %q", got, evidence.taskID)
	}
	if snapshot := protectedGoldenRead(t, fixture, implementContainer, protectedGoldenTaskFile); !json.Valid([]byte(snapshot)) {
		t.Fatalf("implement /kodama/task.json is not valid JSON: %q", snapshot)
	}
	requireProtectedGoldenCredentialUsable(t, fixture, evidence.executionID, evidence.executionToken)
	releaseProtectedGoldenGate(t, fixture, implementContainer, evidence.gates.implement)

	// Test: require the retained checkout at exactly C, then run the real test
	// script and submit both declared outputs.
	waitProtectedActionRunning(t, fixture, evidence.executionID, "test")
	testContainer := waitProtectedGoldenContainer(t, fixture, evidence.executionID, "test")
	evidence.testToken = waitProtectedGoldenCredential(t, fixture, evidence.executionID, "test")
	requireProtectedGoldenMarker(t, fixture, testContainer, evidence.executionID)
	evidence.candidate = protectedGoldenRead(t, fixture, testContainer, protectedGoldenImplementCandidatePath)
	if evidence.candidate == "" || evidence.candidate == base {
		t.Fatalf("materialized implement candidate = %q; want a commit different from the base %q",
			evidence.candidate, base)
	}
	requireProtectedGoldenHead(t, fixture, testContainer, evidence.candidate)
	releaseProtectedGoldenGate(t, fixture, testContainer, evidence.gates.test)

	// Review: the request runs with no workload, the task stays open, main stays
	// at the reviewed base and the successor is unstarted.
	waitProtectedActionRunning(t, fixture, evidence.executionID, protectedRefReviewAction)
	requireProtectedGoldenNoWorkload(t, fixture, evidence.executionID)
	waiting := inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, evidence.executionID)
	evidence.pendingVersion = waiting.Version
	evidence.implement = actionByName(t, waiting, "implement")
	evidence.test = actionByName(t, waiting, "test")
	evidence.review = actionByName(t, waiting, protectedRefReviewAction)
	if evidence.review.State != "running" || evidence.review.Decision != "" ||
		evidence.review.Actor != "" || evidence.review.AnsweredAt != nil {
		t.Fatalf("review action = %+v; want an unanswered running request", evidence.review)
	}
	if successor := actionByName(t, waiting, protectedRefIntegrateAction); successor.State == "running" || successor.State == "succeeded" {
		t.Fatalf("integrate action state = %q while the request waits; want unstarted", successor.State)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != base {
		t.Fatalf("protected main = %q while the request waits; want the base %q", main, base)
	}
	item := waitProtectedGoldenRequest(t, fixture, evidence.reviewerToken, evidence.executionID)
	requireProtectedGoldenRequest(t, item, protectedGoldenEvidence{
		fixture:     fixture,
		executionID: evidence.executionID,
		base:        base,
		candidate:   evidence.candidate,
	}, evidence.review)
	return evidence
}

// resolveProtectedRefRequest answers one named request action. It retries only
// an explicit version_conflict from newly read state, exactly like the
// workload output submitter; every other refusal fails the test.
func resolveProtectedRefRequest(t *testing.T, fixture *gitHarness, token, executionID, actionName, decision, key string, outputs map[string]string) api.ExecutionRequestResolveResult {
	t.Helper()
	for range 30 {
		current := inspectProtectedGoldenExecution(t, fixture, token, executionID)
		if current.State != "running" {
			t.Fatalf("execution %s state = %q before resolving %q; want running",
				executionID, current.State, actionName)
		}
		action := actionByName(t, current, actionName)
		if action.State != "running" {
			t.Fatalf("action %q state = %q before resolve; want running", actionName, action.State)
		}
		result, publicError := fixture.REST(token, "execution.request.resolve", api.ExecutionRequestResolveRequest{
			ExecutionID:           executionID,
			ActionID:              action.ActionID,
			ExpectedActionVersion: action.Version,
			Decision:              decision,
			Outputs:               outputs,
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
			t.Fatalf("resolve versions = %+v; want both advanced past execution=%d action=%d",
				resolved, current.Version, action.Version)
		}
		return resolved
	}
	t.Fatalf("execution.request.resolve %q did not converge on a current version", actionName)
	return api.ExecutionRequestResolveResult{}
}

// cancelProtectedRefExecution cancels one execution at its current version and
// returns the committed cancellation result.
func cancelProtectedRefExecution(t *testing.T, fixture *gitHarness, executionID string) api.ExecutionCancelResult {
	t.Helper()
	for range 30 {
		result, publicError := fixture.REST(fixture.engineerToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			t.Fatalf("inspect %s before cancel: %s (%s)", executionID, publicError.Code, publicError.Message)
		}
		var current api.ExecutionInspectResult
		fixture.Decode(result, &current)
		if current.State != "running" {
			t.Fatalf("execution %s state = %q before cancel; want running", executionID, current.State)
		}
		cancelled, publicError := fixture.REST(fixture.engineerToken, "execution.cancel", api.ExecutionCancelRequest{
			ExecutionID: executionID,
		}, &current.Version, "cancel-"+executionID)
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("execution.cancel refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var committed api.ExecutionCancelResult
		fixture.Decode(cancelled, &committed)
		if committed.ExecutionVersion <= current.Version {
			t.Fatalf("execution.cancel version = %d; want an advance past %d", committed.ExecutionVersion, current.Version)
		}
		return committed
	}
	t.Fatalf("execution.cancel did not converge on a current version")
	return api.ExecutionCancelResult{}
}

// prepareProtectedRefIntegrate waits for the integrate checkpoint of an
// already-approved execution and proves the retained workspace, both settled
// outputs and the unchanged reviewed base. It does not release the gate, so the
// caller can change a committed rule or move the target first.
func prepareProtectedRefIntegrate(t *testing.T, evidence *protectedRefEvidence) string {
	t.Helper()
	fixture := evidence.fixture
	waitProtectedActionRunning(t, fixture, evidence.executionID, protectedRefIntegrateAction)
	container := waitProtectedGoldenContainer(t, fixture, evidence.executionID, protectedRefIntegrateAction)
	evidence.integrateToken = waitProtectedGoldenCredential(t, fixture, evidence.executionID, protectedRefIntegrateAction)
	requireProtectedGoldenMarker(t, fixture, container, evidence.executionID)
	requireProtectedGoldenHead(t, fixture, container, evidence.candidate)
	if got := protectedGoldenRead(t, fixture, container, protectedGoldenTestCandidatePath); got != evidence.candidate {
		t.Fatalf("materialized test candidate = %q; want %q", got, evidence.candidate)
	}
	if got := protectedGoldenRead(t, fixture, container, protectedGoldenTestResultPath); got != "pass:"+evidence.candidate {
		t.Fatalf("materialized test_result = %q; want pass:%s", got, evidence.candidate)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != evidence.base {
		t.Fatalf("protected main = %q before the integrate push; want the reviewed base %q", main, evidence.base)
	}
	requireProtectedGoldenCredentialUsable(t, fixture, evidence.executionID, evidence.integrateToken)
	return container
}

// finishProtectedRef answers the waiting review with a pass (the control), then
// releases the standard integrate checkpoint and collects the positive flow.
func finishProtectedRef(t *testing.T, evidence *protectedRefEvidence) protectedEdgeEvidence {
	t.Helper()
	resolveProtectedRefRequest(t, evidence.fixture, evidence.reviewerToken, evidence.executionID,
		protectedRefReviewAction, "pass", "resolve-"+evidence.spec.pipelineName, nil)
	container := prepareProtectedRefIntegrate(t, evidence)
	releaseProtectedGoldenGate(t, evidence.fixture, container, evidence.gates.integrate)
	return collectProtectedRefFlow(t, evidence)
}

// waitProtectedRefSucceeded waits for a succeeded execution with completed
// cleanup and fills the action and retained-log observations.
func waitProtectedRefSucceeded(t *testing.T, evidence *protectedRefEvidence) api.ExecutionInspectResult {
	t.Helper()
	fixture := evidence.fixture
	finished := waitProtectedGoldenTerminal(t, fixture, evidence.executionID)
	if finished.State != "succeeded" {
		t.Fatalf("execution state = %q; want succeeded\n%s",
			finished.State, fixture.executionLogs(t, finished))
	}
	evidence.finished = finished
	evidence.implement = actionByName(t, finished, "implement")
	evidence.test = actionByName(t, finished, "test")
	evidence.review = actionByName(t, finished, protectedRefReviewAction)
	evidence.integrate = actionByName(t, finished, protectedRefIntegrateAction)
	evidence.logs = fixture.executionLogs(t, finished)
	return finished
}

// collectProtectedRefFlow gathers the accepted protected journal, its real
// verification row, the stored requested approval, the report event and the
// done task, and returns the positive-flow evidence.
func collectProtectedRefFlow(t *testing.T, evidence *protectedRefEvidence) protectedEdgeEvidence {
	t.Helper()
	fixture := evidence.fixture
	waitProtectedRefSucceeded(t, evidence)

	workJournal := waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, evidence.workRef, evidence.candidate)
	protectedJournal := waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, gitMainRef, evidence.candidate)
	mainJournalCount := protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef)
	verification := fixture.verificationByID(t, protectedJournal.VerificationID)
	approval, err := fixture.git.RequestedApproval(t.Context(), verification.ID)
	if err != nil {
		t.Fatalf("read the protected verification's stored requested approval: %v", err)
	}
	executionActor := protectedGoldenExecutionActor(t, fixture, evidence.executionID)
	reportEvent := fixture.waitReportEvent(t, verification.ID)
	task := waitProtectedGoldenTask(t, fixture, evidence.engineerToken, evidence.taskID,
		"task execution="+evidence.executionID+" succeeded", func(read api.TaskReadResult) bool {
			return read.State == "done"
		})

	return protectedEdgeEvidence{
		fixture:           fixture,
		pipelineID:        evidence.pipelineID,
		taskID:            evidence.taskID,
		executionID:       evidence.executionID,
		base:              evidence.base,
		candidate:         evidence.candidate,
		workRef:           evidence.workRef,
		engineerPrincipal: evidence.engineerPrincipal,
		reviewerToken:     evidence.reviewerToken,
		reviewerPrincipal: evidence.reviewerPrincipal,
		executionToken:    evidence.executionToken,
		testToken:         evidence.testToken,
		integrateToken:    evidence.integrateToken,
		implement:         evidence.implement,
		test:              evidence.test,
		review:            evidence.review,
		integrate:         evidence.integrate,
		workJournal:       workJournal,
		protectedJournal:  protectedJournal,
		mainJournalCount:  mainJournalCount,
		verification:      verification,
		approval:          approval,
		executionActor:    executionActor,
		reportEvent:       reportEvent,
		task:              task,
		finished:          evidence.finished,
		logs:              evidence.logs,
	}
}

// commitProtectedRefPolicy installs one complete policy through a real clone,
// commit and served admin push, then requires the accepted blob to be exactly
// the supplied text. It never writes a policy file the product reads.
func commitProtectedRefPolicy(t *testing.T, fixture *gitHarness, policy string) string {
	t.Helper()
	directory := directoryFor(t, "protected-admin-policy")
	stderr, code := fixture.git.Clone(t, fixture.AdminToken, factory.GitAdminRepository, directory)
	if code != 0 {
		t.Fatalf("clone the admin repository for a policy change: exit %d: %s", code, stderr)
	}
	conf := filepath.Join(directory, filepath.FromSlash(native.CommittedPolicyFile))
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		t.Fatalf("create the policy directory: %v", err)
	}
	if err := os.WriteFile(conf, []byte(policy), 0o644); err != nil {
		t.Fatalf("write the proposed policy: %v", err)
	}
	commit := fixture.git.Commit(t, directory, "protected committed policy")
	stderr, code = fixture.push(directory, fixture.AdminToken, "HEAD:"+native.CommittedPolicyRef)
	if code != 0 {
		t.Fatalf("push the proposed policy: exit %d: %s", code, stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitAdminRepository, native.CommittedPolicyRef); remote != commit {
		t.Fatalf("admin policy ref = %q; want the accepted commit %q", remote, commit)
	}
	if accepted := fixture.git.AdminPolicy(t); accepted != policy {
		t.Fatalf("accepted policy differs from the proposed policy:\n%s", accepted)
	}
	return commit
}

// replaceProtectedRefPush replaces the one authored integrate push with a
// checkpoint script. Every earlier exact-candidate, output and ancestry check
// in the standard list is preserved.
func replaceProtectedRefPush(t *testing.T, standard []string, replacement string) []string {
	t.Helper()
	index := -1
	for position, command := range standard {
		if !strings.HasPrefix(command, "git_kodama push") {
			continue
		}
		if index >= 0 {
			t.Fatalf("the standard integrate commands carry %d pushes; want exactly one", 2)
		}
		index = position
	}
	if index < 0 {
		t.Fatalf("the standard integrate commands carry no authored push to replace")
	}
	replaced := append([]string(nil), standard[:index]...)
	replaced = append(replaced, replacement)
	return append(replaced, standard[index+1:]...)
}

// protectedRefPolicyIntegrate installs the two-phase policy checkpoint: the
// approved push must be refused by the changed committed rule, then the same
// push succeeds after the test restores the accepted policy.
func protectedRefPolicyIntegrate(t *testing.T, standard []string, base string) []string {
	t.Helper()
	lease := "--force-with-lease=" + gitMainRef + ":" + base
	script := "if git_kodama push --quiet " + lease +
		" origin \"$kodama_integrate_candidate\":" + gitMainRef + " 2>" + protectedRefPolicyErrorPath + "; then\n" +
		"  printf 'policy-change-rule-ignored\\n' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"printf 'policy-change-refused\\n' > " + protectedRefPolicyRefusedPath + "\n" +
		"while [ ! -e " + protectedRefPolicyRestoreGate + " ]; do sleep 1; done\n" +
		"git_kodama push --quiet " + lease +
		" origin \"$kodama_integrate_candidate\":" + gitMainRef
	return replaceProtectedRefPush(t, standard, script)
}

// protectedRefMovedIntegrate installs the moved-target checkpoint: the original
// lease precondition fails client-side, then a forced push reaches the server
// and is refused by the hook because its subject names the moved base.
func protectedRefMovedIntegrate(t *testing.T, standard []string, base string) []string {
	t.Helper()
	lease := "--force-with-lease=" + gitMainRef + ":" + base
	script := "if git_kodama push --quiet " + lease +
		" origin \"$kodama_integrate_candidate\":" + gitMainRef + " 2>" + protectedRefMovedLeaseError + "; then\n" +
		"  printf 'moved-target-lease-ignored\\n' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"printf 'lease-refused\\n' > " + protectedRefMovedLeaseRefused + "\n" +
		"if git_kodama push --quiet --force origin \"$kodama_integrate_candidate\":" + gitMainRef +
		" 2>" + protectedRefMovedHookError + "; then\n" +
		"  printf 'moved-target-hook-allowed\\n' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"printf 'hook-refused\\n' > " + protectedRefMovedHookRefused + "\n" +
		"printf 'moved-target-checkpoint-complete\\n' > " + protectedRefMovedCheckpoint + "\n" +
		"while [ ! -e " + protectedRefMovedFinishGate + " ]; do sleep 1; done\n" +
		"exit 0"
	return replaceProtectedRefPush(t, standard, script)
}

// protectedRefApprovalExecution is one live execution holding a real human
// request pass for an exact effect subject.
type protectedRefApprovalExecution struct {
	executionID string
	credential  string
}

// startProtectedRefApprovalExecution starts one task-free approval execution: a
// role/reviewer request for the exact canonical subject, a real human pass, and
// a hold container that keeps the minted post-pass credential live.
func startProtectedRefApprovalExecution(t *testing.T, fixture *gitHarness, name string, subject map[string]any, reviewerToken string) protectedRefApprovalExecution {
	t.Helper()
	applied := fixture.applyPipeline(name, marshalProtectedGoldenPipeline(t, protectedGoldenPipelineDoc{
		Actions: []pipelineActionDoc{
			{
				Name:    protectedRefReviewAction,
				Request: "role/reviewer",
				Timeout: protectedRefRequestTimeout,
				Subject: subject,
			},
			{
				Name:    protectedRefHoldAction,
				Image:   fixture.Image.Reference,
				Timeout: protectedGoldenActionTimeout,
				Commands: []string{
					`test ! -e /kodama/task.json`,
					`while [ ! -e /workspace/release ]; do sleep 1; done`,
				},
			},
		},
	}))
	started := fixture.startPipeline(applied.PipelineID, "start-"+name)
	t.Cleanup(func() { fixture.stopExecution(t, started.ExecutionID) })
	waitProtectedActionRunning(t, fixture, started.ExecutionID, protectedRefReviewAction)
	resolveProtectedRefRequest(t, fixture, reviewerToken, started.ExecutionID,
		protectedRefReviewAction, "pass", "resolve-"+name, nil)
	waitProtectedActionRunning(t, fixture, started.ExecutionID, protectedRefHoldAction)
	credential := waitProtectedGoldenCredential(t, fixture, started.ExecutionID, protectedRefHoldAction)
	return protectedRefApprovalExecution{
		executionID: started.ExecutionID,
		credential:  credential,
	}
}

// protectedRefEffectSubject renders the literal canonical five-field subject
// the served Git hook derives for one exact ref update. Literals are deliberate:
// the approval evidence must be byte-equal to the protected subject.
func protectedRefEffectSubject(fixture *gitHarness, expectedBase, candidate string) map[string]any {
	return map[string]any{
		"operation":     approvalOperation,
		"resource":      fixture.git.ProjectResourceID,
		"ref":           gitMainRef,
		"expected_base": expectedBase,
		"candidate":     candidate,
	}
}

// protectedRefCommitFromBase clones the project at the current main and writes
// one deterministic commit on top of it. The clone is taken at the current main,
// so the new commit is based on it; the served hook independently proves the
// ancestry from the pushed update. The bare repository cannot see the local
// commit until a push, so it is not read here.
func protectedRefCommitFromBase(t *testing.T, fixture *gitHarness, name string) (string, string) {
	t.Helper()
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, name)
	commit := fixture.commit(t, directory, name)
	return directory, commit
}

// protectedRefVerificationRows returns every verification row the exact
// execution recorded for one effect ref/candidate, selected by the stored
// typed subject and never by "some row".
func protectedRefVerificationRows(t *testing.T, fixture *gitHarness, executionID, ref, candidate string) []factory.VerificationRow {
	t.Helper()
	rows, err := fixture.git.Verifications(t.Context())
	if err != nil {
		t.Fatalf("read verification rows: %v", err)
	}
	var found []factory.VerificationRow
	for _, row := range rows {
		if row.ExecutionID != executionID || row.Operation != approvalOperation {
			continue
		}
		fields := protectedRefSubjectFields(t, row.Subject)
		if fields["ref"] == ref && fields["candidate"] == candidate {
			found = append(found, row)
		}
	}
	return found
}

// protectedRefVerificationRowsBySubject returns every verification row of one
// exact execution for the full canonical effect subject (ref, expected base and
// candidate). It is the selector for a moved-base attempt whose candidate value
// alone is ambiguous.
func protectedRefVerificationRowsBySubject(t *testing.T, fixture *gitHarness, executionID, ref, expectedBase, candidate string) []factory.VerificationRow {
	t.Helper()
	rows, err := fixture.git.Verifications(t.Context())
	if err != nil {
		t.Fatalf("read verification rows: %v", err)
	}
	var found []factory.VerificationRow
	for _, row := range rows {
		if row.ExecutionID != executionID || row.Operation != approvalOperation {
			continue
		}
		fields := protectedRefSubjectFields(t, row.Subject)
		if fields["ref"] == ref && fields["expected_base"] == expectedBase && fields["candidate"] == candidate {
			found = append(found, row)
		}
	}
	return found
}

// waitProtectedRefVerification waits for one exact verification row of an
// attempt selected by actor, ref and candidate. It is used for a human actor,
// whose refusal has no execution identity.
func waitProtectedRefVerification(t *testing.T, fixture *gitHarness, actor, ref, candidate string) factory.VerificationRow {
	t.Helper()
	var found factory.VerificationRow
	fixture.git.WaitFor(t,
		"verification actor="+actor+" ref="+ref+" candidate="+candidate,
		protectedGoldenTimeout, func() bool {
			rows, err := fixture.git.Verifications(t.Context())
			if err != nil {
				return false
			}
			for _, row := range rows {
				if row.ActorID != actor || row.Operation != approvalOperation {
					continue
				}
				fields := protectedRefSubjectFields(t, row.Subject)
				if fields["ref"] == ref && fields["candidate"] == candidate {
					found = row
					return true
				}
			}
			return false
		})
	return found
}

// protectedRefSubjectFields decodes one stored typed subject into name/value
// pairs.
func protectedRefSubjectFields(t *testing.T, subject string) map[string]string {
	t.Helper()
	var parsed api.Subject
	if err := json.Unmarshal([]byte(subject), &parsed); err != nil {
		t.Fatalf("decode stored verification subject %s: %v", subject, err)
	}
	fields := make(map[string]string, len(parsed.Fields))
	for _, field := range parsed.Fields {
		fields[field.Name] = field.Value
	}
	return fields
}

// protectedRefJournalFor returns the durable journal entries of one exact
// repository/ref/candidate in any state.
func protectedRefJournalFor(t *testing.T, fixture *gitHarness, ref, candidate string) []git.JournalEntry {
	t.Helper()
	var found []git.JournalEntry
	for _, entry := range fixture.git.JournalEntries(t) {
		if entry.Repository == factory.GitProjectRepository && entry.Ref == ref && entry.Candidate == candidate {
			found = append(found, entry)
		}
	}
	return found
}

// waitProtectedRefJournalState waits for one journal entry of an exact attempt
// in the named state.
func waitProtectedRefJournalState(t *testing.T, fixture *gitHarness, ref, candidate string, state git.JournalState) git.JournalEntry {
	t.Helper()
	var found git.JournalEntry
	fixture.git.WaitFor(t,
		"journal ref="+ref+" candidate="+candidate+" state="+string(state),
		protectedGoldenTimeout, func() bool {
			for _, entry := range protectedRefJournalFor(t, fixture, ref, candidate) {
				if entry.State == state {
					found = entry
					return true
				}
			}
			return false
		})
	return found
}

// requireProtectedRefNoJournal requires that an attempt left no journal entry
// at all, in any state.
func requireProtectedRefNoJournal(t *testing.T, fixture *gitHarness, ref, candidate string) {
	t.Helper()
	if entries := protectedRefJournalFor(t, fixture, ref, candidate); len(entries) != 0 {
		t.Fatalf("attempt ref=%s candidate=%s left %d journal entries; want none: %+v",
			ref, candidate, len(entries), entries)
	}
}

// requireProtectedRefNoEffect requires that a terminal or refused decision
// produced no protected effect: main unchanged, no journal entry for the
// candidate in any state, no verification row of this execution for that exact
// effect, and therefore no linked verified event.
func requireProtectedRefNoEffect(t *testing.T, fixture *gitHarness, executionID, expectedMain, candidate string) {
	t.Helper()
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != expectedMain {
		t.Fatalf("protected main = %q; want the unchanged %q", main, expectedMain)
	}
	requireProtectedRefNoJournal(t, fixture, gitMainRef, candidate)
	for _, row := range protectedRefVerificationRows(t, fixture, executionID, gitMainRef, candidate) {
		t.Fatalf("no protected effect was expected, but verification %+v exists for ref=%s candidate=%s",
			row, gitMainRef, candidate)
	}
}

// requireProtectedRefPending requires the review request to remain unanswered
// with its successor unstarted and the protected ref unchanged.
func requireProtectedRefPending(t *testing.T, fixture *gitHarness, evidence protectedRefEvidence, label string) {
	t.Helper()
	execution := inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, evidence.executionID)
	if execution.State != "running" {
		t.Fatalf("%s: execution state = %q; want running", label, execution.State)
	}
	review := actionByName(t, execution, protectedRefReviewAction)
	if review.State != "running" || review.Decision != "" || review.Actor != "" || review.AnsweredAt != nil {
		t.Fatalf("%s: review action was settled or changed: %+v", label, review)
	}
	if successor := actionByName(t, execution, protectedRefIntegrateAction); successor.State == "running" || successor.State == "succeeded" {
		t.Fatalf("%s: integrate action state = %q; want unstarted", label, successor.State)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != evidence.base {
		t.Fatalf("%s: protected main = %q; want the unchanged base %q", label, main, evidence.base)
	}
}

// requireProtectedRefReceipts requires the exact resolution receipt count.
func requireProtectedRefReceipts(t *testing.T, fixture *gitHarness, executionID, actionID string, want int64) {
	t.Helper()
	receipts, err := fixture.ReceiptCount(t.Context(), executionID+"/"+actionID)
	if err != nil {
		t.Fatalf("count resolution receipts: %v", err)
	}
	if receipts != want {
		t.Fatalf("resolution receipts = %d; want %d", receipts, want)
	}
}

// protectedRefUpdateVerificationCount reads the durable git.ref.update
// verification row count. It distinguishes an attempted ref effect from the
// served repository reads a push protocol also performs.
func protectedRefUpdateVerificationCount(t *testing.T, fixture *gitHarness) int {
	t.Helper()
	rows, err := fixture.git.Verifications(t.Context())
	if err != nil {
		t.Fatalf("read verification rows: %v", err)
	}
	count := 0
	for _, row := range rows {
		if row.Operation == approvalOperation {
			count++
		}
	}
	return count
}

// protectedRefHumanWorkControl pushes one fresh commit to a granted work ref
// with the OIDC engineer. It is the positive control that the served Git edge,
// the current membership and the repository are still usable after a refusal.
func protectedRefHumanWorkControl(t *testing.T, fixture *gitHarness, name string) string {
	t.Helper()
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "protected-control-"+name)
	commit := fixture.commit(t, directory, "protected control "+name)
	ref := protectedRefHumanControlPrefix + name
	if stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+ref); code != 0 {
		t.Fatalf("control work push %s: exit %d: %s", ref, code, stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, ref); remote != commit {
		t.Fatalf("control work ref = %q; want %q", remote, commit)
	}
	return commit
}
