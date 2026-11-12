//go:build integration

package factorytest

// Checkpoint-oriented companion to the protected golden path.
//
// This file owns the shared scenario for the four authority/wait assertions in
// protected_authority_test.go and the representative negative the single
// no-substitute pin runs. It composes the golden-path utilities and the
// authored command builders; it never injects a verification row, an approval
// or a ref, and every refusal is read back from the served composition:
//
//   - implement: task context, exact base, candidate C on the granted work ref,
//     declared candidate output;
//   - test: retained checkout at exactly C, real test script, declared outputs,
//     and (when requested) a post-test checkpoint that keeps the container live
//     while the execution attempts C->main before review activation;
//   - review: one role/reviewer request with the canonical five-field subject;
//   - integrate: both settled outputs, retained marker, exact C, base ancestor,
//     `--force-with-lease=refs/heads/main:<B>` and no new commit.
//
// The scenario can stop at the pre-review checkpoint (the pin's representative
// negative) or continue through the human pass and the protected integration.
// It never retries a refused operation: the only retry is the existing
// version-conflict convergence for a resolution envelope. A current credential
// is not refused right after start, so the golden-path control is a plain
// bounded wait for the current credential nonce followed by one clone.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	shelloidc "github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/userland/git"
	identityfixture "github.com/optimiware/kodama/test/support/boundaries/identity"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// protectedEdgeWorkRef is the granted work branch this scenario pushes C to.
	protectedEdgeWorkRef = "refs/heads/work/protected-edge"
	// protectedEdgeControlRef is the second granted work ref the live execution
	// pushes to prove the protected refusal was about approval, not authority
	// or the Git edge.
	protectedEdgeControlRef = "refs/heads/work/protected-edge-control"
	// protectedEdgePipelineName and protectedEdgeTaskTitle name this scenario's
	// one pipeline and task.
	protectedEdgePipelineName = "protected-edge-authority"
	protectedEdgeTaskTitle    = "protected edge authority"
	// protectedEdgeReviewerSubject is the unique OIDC subject of the one human
	// granted exactly engineer+reviewer for this scenario.
	protectedEdgeReviewerSubject = "protected-edge-reviewer"
	// protectedEdgeAdminSubject is the unique OIDC subject of the admin-only
	// human used to prove an admin without reviewer cannot answer the request.
	protectedEdgeAdminSubject = "protected-edge-admin"
	// protectedEdgeReviewerServiceID is the reviewer-role service used to prove
	// a service role never substitutes for the assigned human.
	protectedEdgeReviewerServiceID = "protected-edge-reviewer-service"

	// protectedEdgeImplementGate, TestGate and IntegrateGate are the per-action
	// release files. The test creates one only after observing the predecessor's
	// durable state.
	protectedEdgeImplementGate = "/workspace/protected-edge-implement-ready"
	protectedEdgeTestGate      = "/workspace/protected-edge-test-ready"
	protectedEdgeIntegrateGate = "/workspace/protected-edge-integrate-ready"

	// protectedEdgePreReview* are the live-container checkpoint files. The
	// container announces readiness only after the test script ran and the
	// declared outputs were submitted, then waits for the test to release the
	// single prohibited push attempt and later the action exit.
	protectedEdgePreReviewReady   = "/workspace/protected-edge-pre-review-ready"
	protectedEdgePreReviewRelease = "/workspace/protected-edge-pre-review-release"
	protectedEdgePreReviewDone    = "/workspace/protected-edge-pre-review-done"
	protectedEdgePreReviewFinish  = "/workspace/protected-edge-pre-review-finish"
	protectedEdgePreReviewError   = "/workspace/protected-edge-pre-review-push.err"
	protectedEdgePreReviewControl = "/workspace/protected-edge-work-control"

	// protectedEdgeActionTimeout keeps the real PostgreSQL statement clock far
	// from an action deadline while a gate holds a container open.
	protectedEdgeActionTimeout = "20m"
	// protectedEdgeRequestTimeout keeps the request well beyond the same clock.
	protectedEdgeRequestTimeout = "1h"
)

// protectedEdgeOptions selects the checkpoint a caller asserts. Exactly one
// checkpoint option is normally set; the pin sets holdBeforeReview together
// with stopAfterPreReviewRefusal.
type protectedEdgeOptions struct {
	// holdBeforeReview keeps the test container live after the test script ran
	// and lets the execution attempt the protected push before review
	// activation.
	holdBeforeReview bool
	// stopAfterPreReviewRefusal ends the scenario at that checkpoint. The pin
	// uses it for a representative negative without a second full integration.
	stopAfterPreReviewRefusal bool
	// refuseResolveMatrix attempts resolution as an engineer without reviewer, an
	// admin without reviewer, a reviewer-role service and the execution
	// principal before the legitimate reviewer answers.
	refuseResolveMatrix bool
	// revokeReviewer revokes the reviewer membership between discovery and
	// resolution, retries the same token, then restores the membership.
	revokeReviewer bool
	// credentialWait asserts the request-entry revocation and post-pass refresh.
	credentialWait bool
}

// protectedEdgeRefusal records one refused resolution attempt and the durable
// facts that show it settled nothing.
type protectedEdgeRefusal struct {
	Name             string
	Code             string
	ExecutionVersion uint64
	ActionVersion    uint64
}

// protectedEdgeRevocation records the reviewer-revocation checkpoint.
type protectedEdgeRevocation struct {
	MembershipVersion uint64
	RevokedVersion    uint64
	RefusalCode       string
	RestoredVersion   uint64
	Discovered        bool
}

// protectedEdgeEvidence is every durable observation the scenario and the
// assertions need. It never carries a synthesized fact.
type protectedEdgeEvidence struct {
	fixture *gitHarness
	options protectedEdgeOptions

	pipelineID  string
	taskID      string
	executionID string
	base        string
	candidate   string
	workRef     string

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

	workJournal      git.JournalEntry
	protectedJournal git.JournalEntry
	mainJournalCount int

	verification   factory.VerificationRow
	approval       factory.ApprovalAssignment
	executionActor string
	reportEvent    factory.EventRow
	task           api.TaskReadResult
	finished       api.ExecutionInspectResult
	logs           string

	// Pre-review refusal checkpoint.
	preReviewPushError string
	preReviewMain      string
	preReviewControl   bool
	preReviewExecution api.ExecutionInspectResult
	refusedUpdate      factory.VerificationRow
	refusedApproval    factory.ApprovalAssignment
	// preReviewMainJournalBefore and preReviewMainJournalAfter are the reported
	// protected-journal counts immediately before and after the refused attempt.
	// A shared fixture may already carry the golden path's one entry, so the
	// assertion is "unchanged", not "zero".
	preReviewMainJournalBefore int
	preReviewMainJournalAfter  int
	preReviewAcceptedJournal   bool

	// Resolution refusal matrix.
	refusals []protectedEdgeRefusal

	// Reviewer revocation checkpoint.
	revocation protectedEdgeRevocation

	// Request-entry credential checkpoint.
	clearedAtWait         bool
	oldTokenRefusedAtWait bool
	workspaceBefore       string
	workspaceAfter        string
}

// runProtectedEdgeScenario drives the checkpoint scenario through the selected
// options and returns the durable observations the caller asserts.
func runProtectedEdgeScenario(t *testing.T, fixture *gitHarness, options protectedEdgeOptions) protectedEdgeEvidence {
	t.Helper()
	if fixture == nil || fixture.git == nil {
		t.Fatalf("the protected edge scenario needs the Git fixture")
	}
	image := fixture.git.EnsureWorkloadImage(t)
	if image.RunnableReference == "" || !strings.Contains(image.RunnableReference, "@sha256:") {
		t.Fatalf("workload image reference %q is not digest-pinned", image.RunnableReference)
	}
	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded project main ref is missing")
	}

	evidence := protectedEdgeEvidence{
		fixture:           fixture,
		options:           options,
		base:              base,
		workRef:           protectedEdgeWorkRef,
		engineerPrincipal: fixture.engineerPrincipal,
	}
	evidence.reviewerToken, evidence.reviewerPrincipal = grantProtectedReviewer(t, fixture, protectedEdgeReviewerSubject)
	if evidence.reviewerPrincipal == evidence.engineerPrincipal {
		t.Fatalf("the reviewer must be a separate human from the engineer")
	}

	spec := factory.WorkloadCandidateSpec{
		WorkRef:      protectedEdgeWorkRef,
		ExpectedBase: base,
		RequireTask:  true,
	}
	implement, err := fixture.git.WorkloadCandidateCommands(t, spec)
	if err != nil {
		t.Fatalf("author the implement workload commands: %v", err)
	}
	tested, err := fixture.git.WorkloadTestCandidateCommands(t, spec)
	if err != nil {
		t.Fatalf("author the test workload commands: %v", err)
	}
	if options.holdBeforeReview {
		tested = append(tested, protectedEdgePreReviewCommands(base)...)
	}
	integrated, err := fixture.git.WorkloadIntegrateCandidateCommands(t, spec)
	if err != nil {
		t.Fatalf("author the integrate workload commands: %v", err)
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
				Timeout:  protectedEdgeActionTimeout,
				Commands: protectedGoldenGated(protectedEdgeImplementGate, implement),
				Outputs:  []string{"candidate"},
			},
			{
				Name:     "test",
				Image:    image.RunnableReference,
				Timeout:  protectedEdgeActionTimeout,
				Commands: protectedGoldenGated(protectedEdgeTestGate, tested),
				Outputs:  []string{"candidate", "test_result"},
			},
			{
				Name:    "review",
				Request: "role/reviewer",
				Timeout: protectedEdgeRequestTimeout,
				Subject: protectedGoldenReviewSubject(),
			},
			{
				Name:     "integrate",
				Image:    image.RunnableReference,
				Timeout:  protectedEdgeActionTimeout,
				Commands: protectedGoldenGated(protectedEdgeIntegrateGate, integrated),
			},
		},
	}
	applied := fixture.applyPipeline(protectedEdgePipelineName, marshalProtectedGoldenPipeline(t, document))
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("pipeline.apply returned %+v; want an admitted pipeline and version", applied)
	}
	evidence.pipelineID = applied.PipelineID

	// One open task with the declared inputs, read back, then started at the
	// observed lifecycle version.
	created := api.TaskCreateResult{}
	fixture.Decode(fixture.MustREST(fixture.engineerToken, "task.create", api.TaskCreateRequest{
		ProjectID:  factory.ProjectID,
		Title:      protectedEdgeTaskTitle,
		PipelineID: applied.PipelineID,
		Inputs:     inputs,
	}, nil, "create-protected-edge"), &created)
	evidence.taskID = created.TaskID
	if created.TaskID == "" || created.ContentVersion == 0 || created.LifecycleVersion == 0 {
		t.Fatalf("task.create returned %+v; want a committed task", created)
	}
	createdRead := readProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID)
	if createdRead.State != "open" || createdRead.CurrentExecution != "" || len(createdRead.ExecutionHistory) != 0 {
		t.Fatalf("created task = %+v; want open with no execution", createdRead)
	}
	if createdRead.SelectedPipeline != applied.PipelineID || !reflectEqualInputs(createdRead.Inputs, inputs) {
		t.Fatalf("created task pipeline/inputs = %q/%v; want %q/%v",
			createdRead.SelectedPipeline, createdRead.Inputs, applied.PipelineID, inputs)
	}

	started := api.TaskStartResult{}
	fixture.Decode(fixture.MustREST(fixture.engineerToken, "task.start", api.TaskStartRequest{
		TaskID: evidence.taskID,
	}, &createdRead.LifecycleVersion, "start-protected-edge"), &started)
	evidence.executionID = started.ExecutionID
	if started.ExecutionID == "" || started.LifecycleVersion <= createdRead.LifecycleVersion {
		t.Fatalf("task.start returned %+v; want a new execution and an advanced lifecycle version", started)
	}
	waitProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID,
		"task execution="+started.ExecutionID+" after task.start", func(read api.TaskReadResult) bool {
			return read.CurrentExecution == started.ExecutionID
		})

	// Implement: prove the task context and the mounted credential, then let the
	// candidate push happen.
	implementRunning := waitProtectedActionRunning(t, fixture, evidence.executionID, "implement")
	evidence.implement = actionByName(t, implementRunning, "implement")
	implementContainer := waitProtectedGoldenContainer(t, fixture, evidence.executionID, "implement")
	evidence.executionToken = waitProtectedGoldenCredential(t, fixture, evidence.executionID, "implement")
	if got := protectedGoldenExec(t, fixture, implementContainer, "printenv", "KODAMA_TASK"); got != evidence.taskID {
		t.Fatalf("implement KODAMA_TASK = %q; want the task %q", got, evidence.taskID)
	}
	if snapshot := protectedGoldenRead(t, fixture, implementContainer, protectedGoldenTaskFile); !json.Valid([]byte(snapshot)) {
		t.Fatalf("implement /kodama/task.json is not valid JSON: %q", snapshot)
	}
	requireProtectedGoldenCredentialUsable(t, fixture, evidence.executionID, evidence.executionToken)
	releaseProtectedGoldenGate(t, fixture, implementContainer, protectedEdgeImplementGate)

	// Test: read the retained implement output and checkout, then let the test
	// script run.
	testRunning := waitProtectedActionRunning(t, fixture, evidence.executionID, "test")
	evidence.test = actionByName(t, testRunning, "test")
	testContainer := waitProtectedGoldenContainer(t, fixture, evidence.executionID, "test")
	evidence.testToken = waitProtectedGoldenCredential(t, fixture, evidence.executionID, "test")
	requireProtectedGoldenMarker(t, fixture, testContainer, evidence.executionID)
	evidence.candidate = protectedGoldenRead(t, fixture, testContainer, protectedGoldenImplementCandidatePath)
	if evidence.candidate == "" || evidence.candidate == base {
		t.Fatalf("materialized implement candidate = %q; want a commit different from the base %q",
			evidence.candidate, base)
	}
	requireProtectedGoldenHead(t, fixture, testContainer, evidence.candidate)
	releaseProtectedGoldenGate(t, fixture, testContainer, protectedEdgeTestGate)

	if options.holdBeforeReview {
		protectedEdgeRunPreReviewCheckpoint(t, fixture, &evidence, testContainer)
		if options.stopAfterPreReviewRefusal {
			// The refused attempt is already durable; terminate the checkpoint
			// execution so the pin leaves no live wait behind.
			fixture.stopExecution(t, evidence.executionID)
			return evidence
		}
		releaseProtectedGoldenGate(t, fixture, testContainer, protectedEdgePreReviewFinish)
	}

	// Review: the request runs with no workload; the task stays open, the
	// protected ref stays at the base and no successor action is running.
	waitProtectedActionRunning(t, fixture, evidence.executionID, "review")
	requireProtectedGoldenNoWorkload(t, fixture, evidence.executionID)
	waiting := inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, evidence.executionID)
	evidence.review = actionByName(t, waiting, "review")
	if evidence.review.State != "running" {
		t.Fatalf("review action state = %q while the request waits; want running", evidence.review.State)
	}
	if successor := actionByName(t, waiting, "integrate"); successor.State == "running" || successor.State == "succeeded" {
		t.Fatalf("integrate action state = %q while the request waits; want unstarted", successor.State)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != evidence.base {
		t.Fatalf("protected main = %q while the request waits; want the base %q", main, evidence.base)
	}

	if options.credentialWait {
		protectedEdgeRunCredentialWait(t, fixture, &evidence)
	}
	if options.refuseResolveMatrix {
		evidence.refusals = refuseProtectedEdgeResolutions(t, fixture, evidence.executionID, evidence.executionToken)
		// Preserve the unanswered action and pending successor after every
		// refused respondent, not only for the last one.
		after := inspectProtectedGoldenExecution(t, fixture, fixture.ProjectToken, evidence.executionID)
		requireProtectedEdgePendingRequest(t, after, evidence.executionID, "after the resolution refusal matrix")
	}
	if options.revokeReviewer {
		evidence.revocation = revokeProtectedEdgeReviewer(t, fixture, &evidence)
	}

	// The legitimate reviewer resolves the exact request; this is the control
	// that the refusals above were not a broken request path.
	golden := protectedGoldenEvidence{
		fixture:       fixture,
		executionID:   evidence.executionID,
		reviewerToken: evidence.reviewerToken,
		base:          evidence.base,
		candidate:     evidence.candidate,
	}
	resolveProtectedGoldenRequest(t, fixture, golden, evidence.review)

	// Integrate: the retained workspace and both outputs, the unchanged base,
	// then the exact-candidate push with the post-pass credential.
	waitProtectedActionRunning(t, fixture, evidence.executionID, "integrate")
	integrateContainer := waitProtectedGoldenContainer(t, fixture, evidence.executionID, "integrate")
	evidence.integrateToken = waitProtectedGoldenCredential(t, fixture, evidence.executionID, "integrate")
	requireProtectedGoldenMarker(t, fixture, integrateContainer, evidence.executionID)
	requireProtectedGoldenHead(t, fixture, integrateContainer, evidence.candidate)
	if got := protectedGoldenRead(t, fixture, integrateContainer, protectedGoldenTestCandidatePath); got != evidence.candidate {
		t.Fatalf("materialized test candidate = %q; want %q", got, evidence.candidate)
	}
	if got := protectedGoldenRead(t, fixture, integrateContainer, protectedGoldenTestResultPath); got != "pass:"+evidence.candidate {
		t.Fatalf("materialized test_result = %q; want pass:%s", got, evidence.candidate)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != evidence.base {
		t.Fatalf("protected main = %q before the integrate push; want the base %q", main, evidence.base)
	}
	if options.credentialWait {
		if evidence.integrateToken == evidence.executionToken {
			t.Fatalf("the integrate execution credential equals the pre-wait credential; want a freshly minted value")
		}
		workspace, err := fixture.ExecutionWorkspaceID(t.Context(), evidence.executionID)
		if err != nil {
			t.Fatalf("read the post-pass execution workspace: %v", err)
		}
		evidence.workspaceAfter = workspace
		requireProtectedEdgeShellRefusal(t, fixture, evidence.executionToken, evidence.executionID,
			"after the request pass")
		requireProtectedEdgeGitRefusal(t, fixture, evidence.executionToken, "after-the-pass")
	}
	requireProtectedGoldenCredentialUsable(t, fixture, evidence.executionID, evidence.integrateToken)
	releaseProtectedGoldenGate(t, fixture, integrateContainer, protectedEdgeIntegrateGate)

	finished := waitProtectedGoldenTerminal(t, fixture, evidence.executionID)
	if finished.State != "succeeded" {
		t.Fatalf("execution state = %q; want succeeded\n%s", finished.State, fixture.executionLogs(t, finished))
	}
	evidence.finished = finished
	evidence.implement = actionByName(t, finished, "implement")
	evidence.test = actionByName(t, finished, "test")
	evidence.review = actionByName(t, finished, "review")
	evidence.integrate = actionByName(t, finished, "integrate")
	evidence.logs = fixture.executionLogs(t, finished)

	evidence.workJournal = waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, protectedEdgeWorkRef, evidence.candidate)
	evidence.protectedJournal = waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, gitMainRef, evidence.candidate)
	evidence.mainJournalCount = protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef)
	evidence.verification = fixture.verificationByID(t, evidence.protectedJournal.VerificationID)
	evidence.approval, err = fixture.git.RequestedApproval(t.Context(), evidence.verification.ID)
	if err != nil {
		t.Fatalf("read the protected verification's stored requested approval: %v", err)
	}
	evidence.executionActor = protectedGoldenExecutionActor(t, fixture, evidence.executionID)
	evidence.reportEvent = fixture.waitReportEvent(t, evidence.verification.ID)
	evidence.task = waitProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID,
		"task execution="+evidence.executionID+" succeeded", func(read api.TaskReadResult) bool {
			return read.State == "done"
		})
	return evidence
}

// protectedEdgePreReviewCommands appends the live pre-review checkpoint to the
// test action. The test script has already run and submitted its outputs, so
// the container is live before the request activates. The single prohibited
// push is attempted once, its managed-hook diagnostic is retained, and the same
// live credential then pushes the granted control ref as the positive control.
// The final wait keeps the action running until the test has read the durable
// refusal facts.
func protectedEdgePreReviewCommands(base string) []string {
	return []string{
		"printf 'pre-review-ready\\n' > " + protectedEdgePreReviewReady,
		"while [ ! -e " + protectedEdgePreReviewRelease + " ]; do sleep 1; done",
		"if git_kodama push --quiet --force-with-lease=" + gitMainRef + ":" + base +
			" origin \"$kodama_test_candidate\":" + gitMainRef + " 2>" + protectedEdgePreReviewError +
			"; then\n  printf 'unapproved-protected-push-succeeded\\n' >&2\n  exit 1\nfi",
		"printf 'unapproved-protected-push-refused\\n'",
		"if ! git_kodama push --quiet origin \"$kodama_test_candidate\":" + protectedEdgeControlRef +
			"; then\n  printf 'work-control-push-refused\\n' >&2\n  exit 1\nfi",
		"printf 'work-control-pushed\\n'",
		"printf 'work-control-pushed\\n' > " + protectedEdgePreReviewControl,
		"printf 'pre-review-done\\n' > " + protectedEdgePreReviewDone,
		"while [ ! -e " + protectedEdgePreReviewFinish + " ]; do sleep 1; done",
		"printf 'pre-review-finished\\n'",
	}
}

// protectedEdgeRunPreReviewCheckpoint releases the single prohibited push and
// reads its durable refusal while the test action is still live.
func protectedEdgeRunPreReviewCheckpoint(t *testing.T, fixture *gitHarness, evidence *protectedEdgeEvidence, container string) {
	t.Helper()
	protectedEdgeWaitContainerFile(t, fixture, evidence.executionID, container, protectedEdgePreReviewReady,
		"pre-review checkpoint ready execution="+evidence.executionID)
	evidence.preReviewMainJournalBefore = protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef)
	releaseProtectedGoldenGate(t, fixture, container, protectedEdgePreReviewRelease)
	protectedEdgeWaitContainerFile(t, fixture, evidence.executionID, container, protectedEdgePreReviewDone,
		"pre-review checkpoint push attempt complete execution="+evidence.executionID)

	evidence.preReviewPushError = protectedGoldenRead(t, fixture, container, protectedEdgePreReviewError)
	evidence.preReviewControl = protectedGoldenRead(t, fixture, container, protectedEdgePreReviewControl) != ""
	evidence.refusedUpdate = waitProtectedEdgeRefusedUpdate(t, fixture, evidence.executionID,
		evidence.test.ActionID, evidence.candidate)
	approval, err := fixture.git.RequestedApproval(t.Context(), evidence.refusedUpdate.ID)
	if err != nil {
		t.Fatalf("read the refused verification's stored requested approval: %v", err)
	}
	evidence.refusedApproval = approval
	evidence.preReviewMain = fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	evidence.preReviewMainJournalAfter = protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef)
	evidence.preReviewAcceptedJournal = protectedEdgeHasAcceptedJournal(t, fixture, gitMainRef, evidence.candidate)

	// The attempt happened before review activation, while the test container
	// was still the running action.
	evidence.preReviewExecution = inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, evidence.executionID)
	if evidence.preReviewExecution.State != "running" {
		t.Fatalf("execution state = %q during the pre-review attempt; want running", evidence.preReviewExecution.State)
	}
	if action := actionByName(t, evidence.preReviewExecution, "test"); action.State != "running" {
		t.Fatalf("test action state = %q during the pre-review attempt; want running", action.State)
	}
	if action := actionByName(t, evidence.preReviewExecution, "review"); action.State == "running" || action.State == "succeeded" {
		t.Fatalf("review action state = %q before the pre-review attempt completed; want unactivated", action.State)
	}
	if action := actionByName(t, evidence.preReviewExecution, "integrate"); action.State == "running" || action.State == "succeeded" {
		t.Fatalf("integrate action state = %q before approval; want unstarted", action.State)
	}
}

// waitProtectedEdgeRefusedUpdate waits for the exact durable refused
// git.ref.update row of this execution/action whose canonical subject names the
// protected ref and candidate. The filter is the attempt, never "some refused
// row".
func waitProtectedEdgeRefusedUpdate(t *testing.T, fixture *gitHarness, executionID, actionID, candidate string) factory.VerificationRow {
	t.Helper()
	var found factory.VerificationRow
	fixture.git.WaitFor(t,
		"refused protected update execution="+executionID+" action="+actionID+" candidate="+candidate,
		protectedGoldenTimeout, func() bool {
			rows, err := fixture.git.Verifications(t.Context())
			if err != nil {
				return false
			}
			for _, row := range rows {
				if row.ExecutionID != executionID || row.ActionID != actionID ||
					row.Operation != approvalOperation || row.Result != "refused" || row.Approved {
					continue
				}
				var subject api.Subject
				if err := json.Unmarshal([]byte(row.Subject), &subject); err != nil {
					continue
				}
				fields := map[string]string{}
				for _, field := range subject.Fields {
					fields[field.Name] = field.Value
				}
				if fields["ref"] != gitMainRef || fields["candidate"] != candidate {
					continue
				}
				found = row
				return true
			}
			return false
		})
	return found
}

// protectedEdgeWaitContainerFile waits for one non-empty file inside a live
// workload container and returns its trimmed content. A terminal execution
// fails the test immediately with its retained logs instead of waiting out the
// whole budget.
func protectedEdgeWaitContainerFile(t *testing.T, fixture *gitHarness, executionID, container, path, name string) string {
	t.Helper()
	deadline := time.Now().Add(protectedGoldenTimeout)
	var value string
	for time.Now().Before(deadline) {
		data, err := fixture.ReadContainerFile(t.Context(), container, path)
		if err == nil {
			value = strings.TrimSpace(data)
			if value != "" {
				return value
			}
		}
		if execution, ok := fixture.inspectExecution(t, executionID); ok {
			switch execution.State {
			case "failed", "cancelled":
				t.Fatalf("%s: execution reached %q before the checkpoint file\n%s",
					name, execution.State, fixture.executionLogs(t, execution))
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s: checkpoint file %s did not appear within %s", name, path, protectedGoldenTimeout)
	return value
}

// requireProtectedEdgePendingRequest asserts one inspected execution still has
// the unanswered review request and its pending successor.
func requireProtectedEdgePendingRequest(t *testing.T, execution api.ExecutionInspectResult, executionID, label string) {
	t.Helper()
	if execution.ExecutionID != executionID || execution.State != "running" {
		t.Fatalf("%s: execution = %q state %q; want %q running", label, execution.ExecutionID, execution.State, executionID)
	}
	review := actionByName(t, execution, "review")
	if review.State != "running" || review.Decision != "" || review.Actor != "" || review.AnsweredAt != nil {
		t.Fatalf("%s: review action was settled or changed: %+v", label, review)
	}
	successor := actionByName(t, execution, "integrate")
	if successor.State == "running" || successor.State == "succeeded" {
		t.Fatalf("%s: integrate action state = %q; want unstarted", label, successor.State)
	}
}

// refuseProtectedEdgeResolutions attempts the four prohibited respondents once
// each and requires the exact public refusal with no settlement or receipt.
func refuseProtectedEdgeResolutions(t *testing.T, fixture *gitHarness, executionID, executionCredential string) []protectedEdgeRefusal {
	t.Helper()
	adminToken := mintProtectedEdgeAdmin(t, fixture)
	serviceToken := createProtectedReviewerService(t, fixture, protectedEdgeReviewerServiceID)

	cases := []struct {
		name string
		// describe names the respondent in failure messages.
		describe string
		token    string
		wantCode string
	}{
		{
			name:     "engineer-without-reviewer",
			describe: "an engineer without the assigned reviewer role",
			token:    fixture.EngineerToken,
			wantCode: api.CodeForbidden,
		},
		{
			name:     "admin-without-reviewer",
			describe: "an admin without the assigned reviewer role",
			token:    adminToken,
			wantCode: api.CodeForbidden,
		},
		{
			name:     "reviewer-role-service",
			describe: "a service holding the reviewer role",
			token:    serviceToken,
			wantCode: api.CodeForbidden,
		},
		{
			// Request entry already cleared this credential, so the refusal is
			// authentication of a revoked token. A live, unrelated execution
			// attempting a human resolve is the stronger case, covered in
			// protected_handoff_test.go.
			name:     "revoked-pre-wait-credential",
			describe: "the credential captured before the request wait",
			token:    executionCredential,
			wantCode: api.CodeUnauthenticated,
		},
	}
	refusals := make([]protectedEdgeRefusal, 0, len(cases))
	for _, testCase := range cases {
		refusals = append(refusals, attemptProtectedEdgeResolveRefusal(t, fixture, executionID,
			testCase.name, testCase.describe, testCase.token, testCase.wantCode))
	}
	return refusals
}

// attemptProtectedEdgeResolveRefusal performs one prohibited resolve and
// requires the exact refusal, an unanswered action, a pending successor and no
// receipt. The only retry is the existing version-conflict convergence.
func attemptProtectedEdgeResolveRefusal(t *testing.T, fixture *gitHarness, executionID, name, describe, token, wantCode string) protectedEdgeRefusal {
	t.Helper()
	for range 30 {
		current, ok := fixture.inspectExecution(t, executionID)
		if !ok || current.State != "running" {
			t.Fatalf("%s: execution is not running before the refusal", describe)
		}
		review := actionByName(t, current, "review")
		if review.State != "running" {
			t.Fatalf("%s: review action state = %q; want running", describe, review.State)
		}
		_, publicError := fixture.REST(token, "execution.request.resolve", api.ExecutionRequestResolveRequest{
			ExecutionID:           executionID,
			ActionID:              review.ActionID,
			ExpectedActionVersion: review.Version,
			Decision:              "pass",
		}, &current.Version, "resolve-edge-"+name)
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if publicError.Code != wantCode {
			t.Fatalf("%s: resolve code = %q (%s); want %q",
				describe, publicError.Code, publicError.Message, wantCode)
		}
		after, ok := fixture.inspectExecution(t, executionID)
		if !ok {
			t.Fatalf("%s: read the execution after the refusal", describe)
		}
		requireProtectedEdgePendingRequest(t, after, executionID, describe)
		if successor := actionByName(t, after, "integrate"); successor.State == "running" || successor.State == "succeeded" {
			t.Fatalf("%s: refusal advanced the pending successor: %+v", describe, successor)
		}
		receipts, err := fixture.ReceiptCount(t.Context(), executionID+"/"+review.ActionID)
		if err != nil {
			t.Fatalf("%s: count resolution receipts: %v", describe, err)
		}
		if receipts != 0 {
			t.Fatalf("%s: refused resolution committed %d receipts; want 0", describe, receipts)
		}
		return protectedEdgeRefusal{
			Name:             name,
			Code:             publicError.Code,
			ExecutionVersion: current.Version,
			ActionVersion:    review.Version,
		}
	}
	t.Fatalf("%s: resolve did not converge on a current version", describe)
	return protectedEdgeRefusal{}
}

// mintProtectedEdgeAdmin creates one OIDC human whose only project role is
// admin, so it is admitted at the grant stage but does not hold the assigned
// reviewer role at the commit boundary.
func mintProtectedEdgeAdmin(t *testing.T, fixture *gitHarness) string {
	t.Helper()
	token := fixture.OIDC.Mint(t, identityfixture.TokenSpec{Subject: protectedEdgeAdminSubject})
	principal, err := shelloidc.PrincipalIDFor(fixture.OIDC.Issuer, protectedEdgeAdminSubject)
	if err != nil {
		t.Fatalf("derive the edge admin principal: %v", err)
	}
	fixture.MustREST(fixture.AdminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: string(principal),
		Roles:       []string{"admin"},
	}, nil, "grant-"+protectedEdgeAdminSubject)
	return token
}

// createProtectedReviewerService creates one enabled service holding only the
// reviewer role and returns its one-time credential. A service holding the
// role is still not a human approval: it cannot answer a human role assignment
// and its own assignment cannot move the protected ref.
func createProtectedReviewerService(t *testing.T, fixture *gitHarness, serviceID string) string {
	t.Helper()
	created := fixture.MustREST(fixture.ProjectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: factory.ProjectID,
		ServiceID: serviceID,
		Roles:     []string{"reviewer"},
	}, nil, "create-"+serviceID)
	var service api.ServiceCreateResult
	fixture.Decode(created, &service)
	if service.Version == 0 {
		t.Fatalf("service.create returned no committed version")
	}
	issued := fixture.MustREST(fixture.ProjectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
		ServiceID:  serviceID,
		TTLSeconds: int(time.Hour / time.Second),
	}, &service.Version, "issue-"+serviceID)
	var credential api.ServiceCredentialIssueResult
	fixture.Decode(issued, &credential)
	if credential.Token == nil || *credential.Token == "" {
		t.Fatalf("service.credential.issue returned no one-time token")
	}
	return *credential.Token
}

// revokeProtectedEdgeReviewer discovers the request as the reviewer, revokes the
// membership at the observed version, retries the same token, and restores the
// exact authority through public operations.
func revokeProtectedEdgeReviewer(t *testing.T, fixture *gitHarness, evidence *protectedEdgeEvidence) protectedEdgeRevocation {
	t.Helper()
	item := waitProtectedGoldenRequest(t, fixture, evidence.reviewerToken, evidence.executionID)
	golden := protectedGoldenEvidence{
		fixture:     fixture,
		executionID: evidence.executionID,
		base:        evidence.base,
		candidate:   evidence.candidate,
	}
	requireProtectedGoldenRequest(t, item, golden, evidence.review)

	version := fixture.membershipVersion(t, evidence.reviewerPrincipal)
	revoked := fixture.MustREST(fixture.AdminToken, "membership.revoke", api.MembershipRevokeRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: evidence.reviewerPrincipal,
	}, &version, "revoke-"+protectedEdgeReviewerSubject)
	var revokedResult api.MembershipRevokeResult
	fixture.Decode(revoked, &revokedResult)
	if revokedResult.Version != version+1 {
		t.Fatalf("membership.revoke version = %d; want %d", revokedResult.Version, version+1)
	}
	requireProtectedEdgeMembershipAbsent(t, fixture, evidence.reviewerPrincipal)

	refusal := attemptProtectedEdgeResolveRefusal(t, fixture, evidence.executionID,
		"revoked-reviewer", "the reviewer whose membership was just revoked",
		evidence.reviewerToken, api.CodeUnauthenticated)

	restored := fixture.MustREST(fixture.AdminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: evidence.reviewerPrincipal,
		Roles:       []string{"engineer", "reviewer"},
	}, nil, "restore-"+protectedEdgeReviewerSubject)
	var restoredResult api.MembershipGrantResult
	fixture.Decode(restored, &restoredResult)
	if restoredResult.Version == 0 {
		t.Fatalf("membership.grant returned no committed version after revocation")
	}
	requireProtectedGoldenReviewerMembership(t, fixture, evidence.reviewerPrincipal)

	return protectedEdgeRevocation{
		MembershipVersion: version,
		RevokedVersion:    revokedResult.Version,
		RefusalCode:       refusal.Code,
		RestoredVersion:   restoredResult.Version,
		Discovered:        true,
	}
}

// requireProtectedEdgeMembershipAbsent asserts the revoked principal no longer
// appears in the public committed membership list.
func requireProtectedEdgeMembershipAbsent(t *testing.T, fixture *gitHarness, principal string) {
	t.Helper()
	var listed api.MembershipListResult
	fixture.Decode(fixture.MustREST(fixture.AdminToken, "membership.list", api.MembershipListRequest{
		ProjectID: factory.ProjectID,
	}, nil, ""), &listed)
	for _, membership := range listed.Memberships {
		if membership.PrincipalID == principal {
			t.Fatalf("revoked membership for %q is still committed: %+v", principal, membership)
		}
	}
}

// protectedEdgeRunCredentialWait asserts the request-entry revocation: no live
// workload, both credential columns cleared, and the captured pre-wait token
// refused by Shell and Git. A valid Git control proves the edge is healthy.
func protectedEdgeRunCredentialWait(t *testing.T, fixture *gitHarness, evidence *protectedEdgeEvidence) {
	t.Helper()
	cleared, err := fixture.ExecutionCredentialCleared(t.Context(), evidence.executionID)
	if err != nil {
		t.Fatalf("read the execution credential-cleared fact: %v", err)
	}
	if !cleared {
		t.Fatalf("execution %s still stores token_nonce/token_expires_at at request entry; want both absent",
			evidence.executionID)
	}
	evidence.clearedAtWait = true
	workspace, err := fixture.ExecutionWorkspaceID(t.Context(), evidence.executionID)
	if err != nil {
		t.Fatalf("read the request-entry execution workspace: %v", err)
	}
	evidence.workspaceBefore = workspace

	requireProtectedEdgeShellRefusal(t, fixture, evidence.executionToken, evidence.executionID,
		"during the request wait")
	requireProtectedEdgeGitRefusal(t, fixture, evidence.executionToken, "during-the-wait")
	evidence.oldTokenRefusedAtWait = true
	// Positive control: the same served Git edge accepts a valid actor.
	fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "edge-git-control")
}

// requireProtectedEdgeShellRefusal presents one captured credential to the
// served Shell and requires the exact unauthenticated refusal.
func requireProtectedEdgeShellRefusal(t *testing.T, fixture *gitHarness, credential, executionID, label string) {
	t.Helper()
	if credential == "" {
		t.Fatalf("%s: the scenario captured no execution credential to present", label)
	}
	_, publicError := fixture.REST(credential, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: executionID,
	}, nil, "")
	if publicError.Code != api.CodeUnauthenticated {
		t.Fatalf("%s: captured execution credential code = %q (%s); want %q",
			label, publicError.Code, publicError.Message, api.CodeUnauthenticated)
	}
}

// requireProtectedEdgeGitRefusal presents one captured credential to the real
// Git edge and requires the exact 401 authentication refusal, not a transport
// failure, from both the served smart-HTTP endpoint and a real Git clone, and
// no new verification fact.
func requireProtectedEdgeGitRefusal(t *testing.T, fixture *gitHarness, credential, label string) {
	t.Helper()
	if credential == "" {
		t.Fatalf("%s: the scenario captured no execution credential to present to Git", label)
	}
	if fixture.TLS == nil {
		t.Fatalf("%s: the Git fixture has no TLS client", label)
	}
	// Exact public refusal: the served smart-HTTP endpoint answers 401 to the
	// captured credential. Building the request through the fixture TLS client
	// means a DNS, TLS or connection failure fails this probe instead of being
	// accepted as authorization evidence.
	endpoint := fixture.git.HostURL + "/" + factory.GitProjectRepository + ".git/info/refs?service=git-upload-pack"
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("%s: build the Git smart-HTTP probe: %v", label, err)
	}
	request.Header.Set("Authorization", "Basic "+basicValue(credential))
	response, err := fixture.TLS.Client().Do(request)
	if err != nil {
		t.Fatalf("%s: the Git smart-HTTP probe did not reach the served edge: %v", label, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("%s: Git smart-HTTP status = %d; want %d",
			label, response.StatusCode, http.StatusUnauthorized)
	}

	before := protectedEdgeVerificationCount(t, fixture)
	stderr, code := fixture.git.Clone(t, credential, factory.GitProjectRepository,
		directoryFor(t, "edge-revoked-"+label))
	if code == 0 {
		t.Fatalf("%s: Git accepted the captured execution credential; want an authentication refusal", label)
	}
	lower := strings.ToLower(stderr)
	if !strings.Contains(lower, "authentication failed") &&
		!strings.Contains(lower, "could not read username") &&
		!strings.Contains(lower, "terminal prompts disabled") &&
		!strings.Contains(lower, "401") {
		t.Fatalf("%s: Git refused for a non-authentication reason: %s", label, stderr)
	}
	for _, transport := range []string{"certificate", "ssl", "could not resolve", "connection refused", "timed out"} {
		if strings.Contains(lower, transport) {
			t.Fatalf("%s: Git failure is a transport failure, not an authorization refusal: %s", label, stderr)
		}
	}
	after := protectedEdgeVerificationCount(t, fixture)
	if after != before {
		t.Fatalf("%s: a revoked credential produced %d new verification rows; want none", label, after-before)
	}
}

// protectedEdgeVerificationCount reads the durable verification row count.
func protectedEdgeVerificationCount(t *testing.T, fixture *gitHarness) int {
	t.Helper()
	rows, err := fixture.git.Verifications(t.Context())
	if err != nil {
		t.Fatalf("read verification rows: %v", err)
	}
	return len(rows)
}

// protectedEdgeHasAcceptedJournal reports whether a ref/candidate has any
// accepted or reported journal entry, read at the checkpoint rather than after
// a later legitimate integration.
func protectedEdgeHasAcceptedJournal(t *testing.T, fixture *gitHarness, ref, candidate string) bool {
	t.Helper()
	for _, entry := range fixture.git.JournalEntries(t) {
		if entry.Ref != ref || entry.Candidate != candidate {
			continue
		}
		if entry.State == git.JournalAccepted || entry.State == git.JournalReported {
			return true
		}
	}
	return false
}

// assertProtectedEdgePreReviewRefusal asserts the durable pre-review refusal:
// managed-hook rejection, unchanged ref, no accepted/reported journal entry or
// verified event, the exact refused row and the positive control.
func assertProtectedEdgePreReviewRefusal(t *testing.T, evidence protectedEdgeEvidence) {
	t.Helper()
	fixture := evidence.fixture
	if fixture == nil || fixture.git == nil {
		t.Fatalf("pre-review refusal evidence has no fixture")
	}
	if !strings.Contains(evidence.preReviewPushError, "kodama git: update rejected") {
		t.Fatalf("pre-review protected push did not reach the managed hook: %q", evidence.preReviewPushError)
	}
	if !evidence.preReviewControl {
		t.Fatalf("the live execution did not push the granted control ref after the refusal")
	}
	if evidence.preReviewMain != evidence.base {
		t.Fatalf("protected main = %q after the refused attempt; want the unchanged base %q",
			evidence.preReviewMain, evidence.base)
	}
	if evidence.preReviewAcceptedJournal {
		t.Fatalf("ref candidate %s was accepted or reported despite the refusal", evidence.candidate)
	}
	if evidence.preReviewMainJournalAfter != evidence.preReviewMainJournalBefore {
		t.Fatalf("reported protected journal entries after the refused attempt = %d; want the unchanged %d",
			evidence.preReviewMainJournalAfter, evidence.preReviewMainJournalBefore)
	}

	row := evidence.refusedUpdate
	if row.ID == "" || row.Result != "refused" || row.Approved {
		t.Fatalf("refused verification row = %+v; want one refused, unapproved row", row)
	}
	if row.Operation != approvalOperation {
		t.Fatalf("refused verification operation = %q; want %q", row.Operation, approvalOperation)
	}
	if row.ExecutionID != evidence.executionID || row.ActionID != evidence.test.ActionID {
		t.Fatalf("refused verification execution/action = %q/%q; want %q/%q",
			row.ExecutionID, row.ActionID, evidence.executionID, evidence.test.ActionID)
	}
	if row.ServiceID != fixture.git.ServiceID ||
		row.ResourceID != fixture.git.ProjectResourceID ||
		row.BindingID != fixture.git.ProjectBindingID {
		t.Fatalf("refused verification service/resource/binding = %q/%q/%q; want the registered Git binding",
			row.ServiceID, row.ResourceID, row.BindingID)
	}
	var subject api.Subject
	if err := json.Unmarshal([]byte(row.Subject), &subject); err != nil {
		t.Fatalf("decode the refused verification subject %s: %v", row.Subject, err)
	}
	requireProtectedGoldenCanonicalSubject(t, "refused protected update", subject, protectedGoldenEvidence{
		fixture:   fixture,
		base:      evidence.base,
		candidate: evidence.candidate,
	})
	if got := evidence.refusedApproval.String(); got != "role/reviewer" {
		t.Fatalf("refused verification requested approval = %q; want role/reviewer", got)
	}
	if evidence.refusedApproval.Kind != "role" || evidence.refusedApproval.Role != "reviewer" ||
		evidence.refusedApproval.ServiceID != "" {
		t.Fatalf("refused verification requested approval = %+v; want exactly role/reviewer", evidence.refusedApproval)
	}
	if _, ok, err := fixture.git.ReportEvent(t.Context(), row.ID); err != nil {
		t.Fatalf("read the refused verification's report event: %v", err)
	} else if ok {
		t.Fatalf("the refused verification %s produced a verified event; want none", row.ID)
	}

	// The attempt was made before review activation while the test container was
	// still live.
	if evidence.preReviewExecution.State != "running" ||
		actionByName(t, evidence.preReviewExecution, "test").State != "running" {
		t.Fatalf("pre-review attempt was not made while the test container was live: %+v", evidence.preReviewExecution)
	}
	if actionByName(t, evidence.preReviewExecution, "review").State == "running" {
		t.Fatalf("review was already active during the pre-review refusal")
	}
}

// assertProtectedEdgeResolveRefusals asserts the complete refusal matrix and
// that the pending request survived it.
func assertProtectedEdgeResolveRefusals(t *testing.T, evidence protectedEdgeEvidence) {
	t.Helper()
	want := map[string]string{
		"engineer-without-reviewer":   api.CodeForbidden,
		"admin-without-reviewer":      api.CodeForbidden,
		"reviewer-role-service":       api.CodeForbidden,
		"revoked-pre-wait-credential": api.CodeUnauthenticated,
	}
	if len(evidence.refusals) != len(want) {
		t.Fatalf("resolution refusal matrix ran %d attempts; want %d", len(evidence.refusals), len(want))
	}
	for _, refusal := range evidence.refusals {
		code, ok := want[refusal.Name]
		if !ok {
			t.Fatalf("unexpected resolution refusal respondent %q", refusal.Name)
		}
		if refusal.Code != code {
			t.Fatalf("%s refusal code = %q; want %q", refusal.Name, refusal.Code, code)
		}
		if refusal.ExecutionVersion == 0 || refusal.ActionVersion == 0 {
			t.Fatalf("%s refusal recorded no current versions", refusal.Name)
		}
	}
}

// assertProtectedEdgeRevocation asserts discovery, the exact revoked-token
// refusal and the restored membership.
func assertProtectedEdgeRevocation(t *testing.T, evidence protectedEdgeEvidence) {
	t.Helper()
	revocation := evidence.revocation
	if !revocation.Discovered {
		t.Fatalf("the reviewer did not discover the request before revocation")
	}
	if revocation.MembershipVersion == 0 || revocation.RevokedVersion != revocation.MembershipVersion+1 {
		t.Fatalf("membership revocation versions = %d -> %d; want an observed version and its successor",
			revocation.MembershipVersion, revocation.RevokedVersion)
	}
	if revocation.RefusalCode != api.CodeUnauthenticated {
		t.Fatalf("revoked reviewer resolve code = %q; want %q", revocation.RefusalCode, api.CodeUnauthenticated)
	}
	if revocation.RestoredVersion == 0 {
		t.Fatalf("the reviewer membership was not restored through the public operation")
	}
}

// assertProtectedEdgeCredentialWait asserts the recorded request-entry and
// post-pass credential facts.
func assertProtectedEdgeCredentialWait(t *testing.T, evidence protectedEdgeEvidence) {
	t.Helper()
	if !evidence.clearedAtWait {
		t.Fatalf("request entry did not clear both execution credential columns")
	}
	if !evidence.oldTokenRefusedAtWait {
		t.Fatalf("the captured pre-wait token was not refused during the request wait")
	}
	if evidence.workspaceBefore == "" || evidence.workspaceAfter != evidence.workspaceBefore {
		t.Fatalf("execution workspace before/after = %q/%q; want the same retained workspace",
			evidence.workspaceBefore, evidence.workspaceAfter)
	}
	if evidence.executionToken == "" || evidence.integrateToken == "" ||
		evidence.integrateToken == evidence.executionToken {
		t.Fatalf("pre-wait/post-pass execution credentials were not distinct and present")
	}
}

// assertProtectedEdgeFlow asserts the positive control the scenario completed
// after its checkpoint: the human pass, the exact protected integration and
// every durable fact.
func assertProtectedEdgeFlow(t *testing.T, evidence protectedEdgeEvidence) {
	t.Helper()
	fixture := evidence.fixture
	if fixture == nil || fixture.git == nil {
		t.Fatalf("edge evidence has no fixture")
	}
	if evidence.base == "" || evidence.candidate == "" || evidence.candidate == evidence.base {
		t.Fatalf("edge evidence base=%q candidate=%q; want distinct commits", evidence.base, evidence.candidate)
	}
	finished := evidence.finished
	if finished.State != "succeeded" || finished.Cleanup.State != "complete" {
		t.Fatalf("execution state=%q cleanup=%q; want succeeded and complete",
			finished.State, finished.Cleanup.State)
	}
	if finished.TaskID != evidence.taskID || finished.PipelineID != evidence.pipelineID {
		t.Fatalf("execution task/pipeline = %q/%q; want %q/%q",
			finished.TaskID, finished.PipelineID, evidence.taskID, evidence.pipelineID)
	}
	if len(finished.Actions) != 4 {
		t.Fatalf("execution admits %d actions; want 4", len(finished.Actions))
	}
	for _, action := range []api.ExecutionAction{evidence.implement, evidence.test, evidence.review, evidence.integrate} {
		if action.State != "succeeded" {
			t.Fatalf("action %q state = %q; want succeeded", action.Name, action.State)
		}
	}
	if got := evidence.implement.Outputs["candidate"]; got != evidence.candidate {
		t.Fatalf("implement candidate output = %q; want %q", got, evidence.candidate)
	}
	if got := evidence.test.Outputs["candidate"]; got != evidence.candidate {
		t.Fatalf("test candidate output = %q; want %q", got, evidence.candidate)
	}
	if got := evidence.test.Outputs["test_result"]; got != "pass:"+evidence.candidate {
		t.Fatalf("test_result output = %q; want pass:%s", got, evidence.candidate)
	}

	main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if main != evidence.candidate {
		t.Fatalf("protected main = %q; want the candidate %q", main, evidence.candidate)
	}
	if parents := fixture.git.CommitParents(t, factory.GitProjectRepository, evidence.candidate); len(parents) != 1 || parents[0] != evidence.base {
		t.Fatalf("candidate %s parents = %v; want exactly the base %s", evidence.candidate, parents, evidence.base)
	}
	history := fixture.git.History(t, factory.GitProjectRepository)
	if len(history) != 2 || history[0] != evidence.candidate || history[1] != evidence.base {
		t.Fatalf("protected history = %v; want exactly [%s %s]", history, evidence.candidate, evidence.base)
	}

	if evidence.workJournal.Ref != evidence.workRef || evidence.workJournal.State != git.JournalReported ||
		evidence.workJournal.Candidate != evidence.candidate {
		t.Fatalf("work journal = %+v; want a reported entry for %q at %q",
			evidence.workJournal, evidence.workRef, evidence.candidate)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, evidence.workRef); remote != evidence.candidate {
		t.Fatalf("work ref = %q; want the reported candidate %q", remote, evidence.candidate)
	}

	journal := evidence.protectedJournal
	if journal.Ref != gitMainRef || journal.State != git.JournalReported ||
		journal.ExpectedBase != evidence.base || journal.Candidate != evidence.candidate {
		t.Fatalf("protected journal = %+v; want a reported %s -> %s entry", journal,
			evidence.base, evidence.candidate)
	}
	if evidence.mainJournalCount != 1 {
		t.Fatalf("reported protected journal entries = %d; want exactly one", evidence.mainJournalCount)
	}
	row := evidence.verification
	if row.ID != journal.VerificationID || row.Result != "approved" || !row.Approved {
		t.Fatalf("protected verification = %+v; want the journal's approved verification", row)
	}
	if row.Operation != approvalOperation || row.ServiceID != fixture.git.ServiceID ||
		row.ResourceID != fixture.git.ProjectResourceID || row.BindingID != fixture.git.ProjectBindingID {
		t.Fatalf("protected verification identity = %+v; want the served Git binding", row)
	}
	if row.ExecutionID != evidence.executionID || row.ActionID != evidence.integrate.ActionID {
		t.Fatalf("protected verification execution/action = %q/%q; want %q/%q",
			row.ExecutionID, row.ActionID, evidence.executionID, evidence.integrate.ActionID)
	}
	if row.ActorID != evidence.executionActor || row.ActorID == "" {
		t.Fatalf("protected verification actor = %q; want the execution actor %q", row.ActorID, evidence.executionActor)
	}
	var span api.Subject
	if err := json.Unmarshal([]byte(row.Subject), &span); err != nil {
		t.Fatalf("decode protected verification subject %s: %v", row.Subject, err)
	}
	requireProtectedGoldenCanonicalSubject(t, "protected verification", span, protectedGoldenEvidence{
		fixture:   fixture,
		base:      evidence.base,
		candidate: evidence.candidate,
	})
	if got := evidence.approval.String(); got != "role/reviewer" {
		t.Fatalf("stored requested approval = %q; want role/reviewer", got)
	}

	review := evidence.review
	if review.Assignment != "role/reviewer" || review.Decision != "pass" ||
		review.Actor != evidence.reviewerPrincipal || review.AnsweredAt == nil {
		t.Fatalf("review action = %+v; want the reviewer's pass", review)
	}
	requireProtectedGoldenSubject(t, "review action", review.Subject, protectedGoldenEvidence{
		fixture:   fixture,
		base:      evidence.base,
		candidate: evidence.candidate,
	})

	event := evidence.reportEvent
	if event.Type != "resource.verified" || event.ReportType != "git.ref.updated" ||
		event.CausationID != row.ID || event.SubjectID != fixture.git.ProjectResourceID ||
		event.CorrelationID != evidence.executionID ||
		event.SourceKind != "service" || event.SourcePrincipal != fixture.git.ServiceID {
		t.Fatalf("report event = %+v; want one service-sourced verified event for this execution", event)
	}

	if evidence.task.State != "done" {
		t.Fatalf("task state = %q after success; want done", evidence.task.State)
	}
	if len(evidence.task.ExecutionHistory) != 1 || evidence.task.ExecutionHistory[0] != evidence.executionID {
		t.Fatalf("task execution history = %v; want exactly [%s]",
			evidence.task.ExecutionHistory, evidence.executionID)
	}

	markers := []string{
		"candidate-output-submitted",
		"test-outputs-submitted",
		"protected-integration-pushed",
		"repository-config-clean",
	}
	if evidence.options.holdBeforeReview {
		markers = append(markers, "unapproved-protected-push-refused", "work-control-pushed", "pre-review-finished")
	}
	for _, marker := range markers {
		if !strings.Contains(evidence.logs, marker) {
			t.Fatalf("retained workload logs are missing %q:\n%s", marker, evidence.logs)
		}
	}
	protectedEdgeScanCredentials(t, fixture, evidence.executionID, evidence.logs,
		evidence.executionToken, evidence.testToken, evidence.integrateToken)
}

// protectedEdgeScanCredentials searches every credential this scenario mounted
// (pre-wait, reused and post-pass), plus the Git service credential, in raw
// form, as the bare Basic value and as the Basic header form, over the retained
// logs, the admitted pipeline and the host artifacts.
func protectedEdgeScanCredentials(t *testing.T, fixture *gitHarness, executionID, logs string, tokens ...string) {
	t.Helper()
	serviceToken, err := fixture.git.ServiceToken(t)
	if err != nil {
		t.Fatalf("read the mounted Git service credential: %v", err)
	}
	unique := make([]string, 0, len(tokens))
	seen := map[string]bool{}
	for _, token := range tokens {
		if token == "" {
			t.Fatalf("the scenario did not observe every mounted execution credential")
		}
		if seen[token] {
			continue
		}
		seen[token] = true
		unique = append(unique, token)
	}
	secrets := make([]string, 0, 3*(len(unique)+1))
	for _, token := range append(unique, serviceToken) {
		secrets = append(secrets, token, basicValue(token), "Basic "+basicValue(token))
	}
	for _, secret := range secrets {
		if strings.Contains(logs, secret) {
			t.Fatalf("a credential leaked into the retained workload logs")
		}
	}
	yamlBytes, _, err := fixture.ExecutionRow(t.Context(), executionID)
	if err != nil {
		t.Fatalf("read the admitted edge pipeline: %v", err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(yamlBytes), secret) {
			t.Fatalf("a credential persisted in the admitted pipeline YAML")
		}
	}
	if leaks := fixture.git.ScanCredentialLeaks(t, secrets...); len(leaks) != 0 {
		t.Fatalf("credentials leaked into host artifacts %v", leaks)
	}
}

// reflectEqualInputs compares two input maps by value. It is local so this
// scenario does not depend on a specific reflection helper elsewhere.
func reflectEqualInputs(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
