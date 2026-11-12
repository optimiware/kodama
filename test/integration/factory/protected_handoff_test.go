//go:build integration

package factorytest

// Conversation handoff and a review wait that survives a controller restart on
// the protected first-factory path.
//
// The two tests compose the shared protected-path utilities and the authored
// Git commands against a fresh real fixture each:
//
//   - TestProtectedFactoryConversationHandoff drives a task-associated
//     execution whose live first action asks for an unplanned decision. The
//     ask cancels that execution, clears its credential and records one
//     question with a durable Git reference. The assigned reviewer answers
//     from its own inbox; the reply queues exactly one fresh execution with a
//     new workspace and the updated /kodama/task.json. Its first action loads
//     the referenced Git ref and fails when it is absent. The fresh execution
//     then produces its own candidate and must obtain its own human review
//     before the protected ref moves; the cancelled execution's answer is not
//     approval evidence. A live unrelated execution also tries to answer the
//     fresh human request and is refused.
//   - TestProtectedFactoryWaitSurvivesRestart reaches the real role/reviewer
//     wait, restarts only the controller over the same durable state and then
//     resolves and integrates the same execution/workspace. Identity, version,
//     deadline, subject, credential revocation, ref base and pending inbox
//     discovery must be unchanged across the restart, with no duplicate
//     execution and no approval inherited from the stopped controller.
//
// Every fact below is read back from the served composition: the real
// PostgreSQL rows, the real task/request APIs, the real workloads and the real
// Git smart-HTTP edge with its managed hooks. Nothing injects an approval, a
// verification row or a ref.

import (
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// protectedHandoffLiveAction is the first action. Without an answer it
	// writes disposable scratch state and stays live until the ask cancels it;
	// with the answer it verifies the durable Git reference named by the
	// conversation and exits, so the rest of the pipeline starts only after
	// restoration succeeded.
	protectedHandoffLiveAction = "live"

	// protectedHandoffScratch and protectedHandoffRestored are the disposable
	// scratch state and the restoration proof inside the workspace.
	protectedHandoffScratch  = "/workspace/protected-scratch"
	protectedHandoffRestored = "/workspace/protected-restored"

	// protectedHandoffQuestion is the one outstanding conversation question.
	protectedHandoffQuestion = "Which durable Git ref should the next attempt restore?"

	// protectedHandoffAnswerRef is the durable Git reference the successful
	// handoff records on the question and answers. It names the served protected
	// branch, not a disposable path or process-local identity.
	protectedHandoffAnswerRef = "refs/heads/main"
	// protectedHandoffMissingRef is a well-formed Git ref that the served
	// repository never publishes, so the restoring action must fail.
	protectedHandoffMissingRef = "refs/heads/protected-missing-context"

	// protectedHandoffWorkRef is the granted work branch the fresh execution's
	// implement action pushes its candidate to.
	protectedHandoffWorkRef = "refs/heads/work/protected-handoff"

	// protectedHandoffScenario names one fixture-unique protected pipeline.
	protectedHandoffScenario = "protected-handoff"
)

// protectedHandoffEvidence is every durable observation the conversation
// handoff assertions need. It never carries a synthesized fact.
type protectedHandoffEvidence struct {
	fixture *gitHarness

	pipelineID  string
	taskID      string
	executionID string
	base        string
	workRef     string
	gates       protectedRefGates

	reviewerToken     string
	reviewerPrincipal string

	live           api.ExecutionAction
	executionToken string
	workspaceID    string
	scratch        string
}

// TestProtectedFactoryConversationHandoff proves the unplanned-decision path is
// a separate conversation handoff, not a protected approval: the cancelled
// execution is replaced by one fresh execution with a new workspace, restored
// durable context and its own human review before the protected ref moves.
func TestProtectedFactoryConversationHandoff(t *testing.T) {
	t.Run("replaces-execution", func(t *testing.T) {
		testProtectedHandoffReplacesExecution(t)
	})
	t.Run("missing-context", func(t *testing.T) {
		testProtectedHandoffMissingContext(t)
	})
}

// TestProtectedFactoryWaitSurvivesRestart proves a protected review wait is
// durable: only the controller is replaced, and the same execution, action,
// workspace, deadline, subject and revoked credential still drive the one human
// pass and the exact protected integration afterwards.
func TestProtectedFactoryWaitSurvivesRestart(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedRefScenario(t, fixture, protectedRefScenarioSpec{
		pipelineName: "protected-restart",
		taskTitle:    "protected wait restart",
		workRef:      "refs/heads/work/protected-restart",
	})
	ctx := t.Context()
	executionID := evidence.executionID

	workspaceBefore, err := fixture.ExecutionWorkspaceID(ctx, executionID)
	if err != nil || workspaceBefore == "" {
		t.Fatalf("read the pre-restart execution workspace: %v (%q)", err, workspaceBefore)
	}
	clearedBefore, err := fixture.ExecutionCredentialCleared(ctx, executionID)
	if err != nil || !clearedBefore {
		t.Fatalf("pre-restart execution credential cleared = %v (%v); want both columns absent", clearedBefore, err)
	}
	taskBefore := readProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID)
	if taskBefore.State != "open" || taskBefore.CurrentExecution != executionID || taskBefore.Question != nil {
		t.Fatalf("pre-restart task = %+v; want open with the waiting execution", taskBefore)
	}
	requestBefore := waitProtectedGoldenRequest(t, fixture, evidence.reviewerToken, executionID)
	mainBefore := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if mainBefore != evidence.base {
		t.Fatalf("pre-restart protected main = %q; want the reviewed base %q", mainBefore, evidence.base)
	}
	journalBefore := protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef)
	verificationsBefore := protectedEdgeVerificationCount(t, fixture)
	executionsBefore, err := fixture.ExecutionCount(ctx, evidence.pipelineID)
	if err != nil || executionsBefore != 1 {
		t.Fatalf("pre-restart executions = %d (%v); want exactly 1", executionsBefore, err)
	}

	// Only the controller process is replaced. PostgreSQL, NATS, the Git
	// service, the public edge and every durable row survive.
	fixture.RestartController()
	if fixture.Git == nil {
		t.Fatalf("the controller restart removed the Git fixture")
	}

	after := inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, executionID)
	if after.ExecutionID != executionID || after.State != "running" || after.Version != evidence.pendingVersion {
		t.Fatalf("post-restart execution = id %q state %q version %d; want id %q running version %d",
			after.ExecutionID, after.State, after.Version, executionID, evidence.pendingVersion)
	}
	reviewAfter := actionByName(t, after, protectedRefReviewAction)
	if reviewAfter.ActionID != evidence.review.ActionID ||
		reviewAfter.State != "running" ||
		reviewAfter.Version != evidence.review.Version ||
		reviewAfter.Decision != "" || reviewAfter.Actor != "" || reviewAfter.AnsweredAt != nil {
		t.Fatalf("post-restart review action = %+v; want the same unanswered running action %+v",
			reviewAfter, evidence.review)
	}
	if reviewAfter.Deadline == nil || evidence.review.Deadline == nil ||
		!reviewAfter.Deadline.Equal(*evidence.review.Deadline) {
		t.Fatalf("post-restart review deadline = %v; want the persisted %v",
			reviewAfter.Deadline, evidence.review.Deadline)
	}
	if successor := actionByName(t, after, protectedRefIntegrateAction); successor.State == "running" || successor.State == "succeeded" {
		t.Fatalf("post-restart integrate action state = %q; want still unstarted", successor.State)
	}

	taskAfter := readProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID)
	if taskAfter.State != taskBefore.State || taskAfter.CurrentExecution != taskBefore.CurrentExecution ||
		taskAfter.LifecycleVersion != taskBefore.LifecycleVersion ||
		len(taskAfter.ExecutionHistory) != 1 || taskAfter.ExecutionHistory[0] != executionID {
		t.Fatalf("post-restart task = %+v; want the persisted %+v", taskAfter, taskBefore)
	}

	// The pending request is still discoverable through the reviewer's own
	// inbox with the same identity, version and canonical subject.
	requestAfter := waitProtectedGoldenRequest(t, fixture, evidence.reviewerToken, executionID)
	if requestAfter.ExecutionID != requestBefore.ExecutionID ||
		requestAfter.ActionID != requestBefore.ActionID ||
		requestAfter.Assignment != requestBefore.Assignment ||
		requestAfter.Version != requestBefore.Version {
		t.Fatalf("post-restart inbox request = %+v; want the persisted %+v", requestAfter, requestBefore)
	}
	requireProtectedGoldenSubject(t, "post-restart inbox request", requestAfter.Subject, protectedGoldenEvidence{
		fixture:     fixture,
		executionID: executionID,
		base:        evidence.base,
		candidate:   evidence.candidate,
	})

	// Request entry revocation is durable, not controller memory, and the
	// pre-wait credential stays refused by both served edges.
	clearedAfter, err := fixture.ExecutionCredentialCleared(ctx, executionID)
	if err != nil || !clearedAfter {
		t.Fatalf("post-restart execution credential cleared = %v (%v); want both columns absent", clearedAfter, err)
	}
	requireProtectedEdgeShellRefusal(t, fixture, evidence.testToken, executionID,
		"after the controller restart")
	requireProtectedEdgeGitRefusal(t, fixture, evidence.testToken, "after-the-restart")

	workspaceAfterRestart, err := fixture.ExecutionWorkspaceID(ctx, executionID)
	if err != nil || workspaceAfterRestart != workspaceBefore {
		t.Fatalf("post-restart workspace = %q (%v); want the retained %q", workspaceAfterRestart, err, workspaceBefore)
	}
	if mainAfter := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); mainAfter != evidence.base {
		t.Fatalf("post-restart protected main = %q; want the unchanged base %q", mainAfter, evidence.base)
	}
	if journalAfter := protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef); journalAfter != journalBefore {
		t.Fatalf("post-restart reported protected journal entries = %d; want the unchanged %d",
			journalAfter, journalBefore)
	}
	if verificationsAfter := protectedEdgeVerificationCount(t, fixture); verificationsAfter != verificationsBefore {
		t.Fatalf("post-restart verification rows = %d; want the unchanged %d", verificationsAfter, verificationsBefore)
	}
	if executionsAfter, err := fixture.ExecutionCount(ctx, evidence.pipelineID); err != nil || executionsAfter != executionsBefore {
		t.Fatalf("post-restart executions = %d (%v); want exactly the one before", executionsAfter, err)
	}

	// The same reviewer answers the same request and the same execution
	// integrates into the same retained workspace with a freshly minted token.
	flow := finishProtectedRef(t, &evidence)
	assertProtectedEdgeFlow(t, flow)
	if flow.integrateToken == "" || flow.integrateToken == flow.testToken {
		t.Fatalf("post-restart integrate credential is not a fresh value")
	}
	workspaceFinal, err := fixture.ExecutionWorkspaceID(ctx, executionID)
	if err != nil || workspaceFinal != workspaceBefore {
		t.Fatalf("final workspace = %q (%v); want the pre-restart workspace %q", workspaceFinal, err, workspaceBefore)
	}
	if flow.task.State != "done" || len(flow.task.ExecutionHistory) != 1 || flow.task.ExecutionHistory[0] != executionID {
		t.Fatalf("post-restart task = %+v; want done with exactly [%s]", flow.task, executionID)
	}
}

// testProtectedHandoffReplacesExecution proves ask cancels the live execution,
// the reply queues one fresh execution whose first action restores durable
// context and whose own review authorizes the protected integration.
func testProtectedHandoffReplacesExecution(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedHandoffScenario(t, fixture, protectedHandoffScenario,
		"protected conversation handoff", protectedHandoffAnswerRef)
	ctx := t.Context()
	oldExecutionID := evidence.executionID

	// The reviewer is assigned this question, but the ask itself is a
	// conversation operation by the initiating engineer.
	asked, waiting := askProtectedHandoff(t, fixture, evidence.taskID,
		[]string{protectedHandoffAnswerRef}, "ask-handoff")
	if asked.QuestionID == "" || asked.QuestionVersion == 0 {
		t.Fatalf("task.ask returned %+v; want a question identity and version", asked)
	}
	if waiting.State != "waiting" || waiting.Question == nil ||
		waiting.Question.QuestionID != asked.QuestionID ||
		waiting.Question.Version != asked.QuestionVersion {
		t.Fatalf("waiting task = %+v; want the asked question %q version %d",
			waiting, asked.QuestionID, asked.QuestionVersion)
	}
	if len(waiting.Question.References) != 1 || waiting.Question.References[0] != protectedHandoffAnswerRef {
		t.Fatalf("waiting question references = %v; want [%s]", waiting.Question.References, protectedHandoffAnswerRef)
	}
	if waiting.Question.Assignment != "role/reviewer" {
		t.Fatalf("waiting question assignment = %q; want role/reviewer", waiting.Question.Assignment)
	}

	// Ask cancels the nonterminal execution and clears its credential
	// atomically; it creates no request action and no approval evidence.
	cancelled := waitProtectedGoldenTerminal(t, fixture, oldExecutionID)
	if cancelled.State != "cancelled" {
		t.Fatalf("cancelled execution state = %q (failure %+v); want cancelled",
			cancelled.State, cancelled.Failure)
	}
	if action := actionByName(t, cancelled, protectedHandoffLiveAction); action.State != "cancelled" {
		t.Fatalf("cancelled live action state = %q; want cancelled", action.State)
	}
	cleared, err := fixture.ExecutionCredentialCleared(ctx, oldExecutionID)
	if err != nil || !cleared {
		t.Fatalf("cancelled execution credential cleared = %v (%v); want both columns absent", cleared, err)
	}
	requireProtectedEdgeShellRefusal(t, fixture, evidence.executionToken, oldExecutionID, "after the ask")
	requireProtectedEdgeGitRefusal(t, fixture, evidence.executionToken, "after-the-ask")
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != evidence.base {
		t.Fatalf("protected main after the ask = %q; want the unchanged base %q", main, evidence.base)
	}
	if count := protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef); count != 0 {
		t.Fatalf("the ask produced %d protected journal entries; want none", count)
	}

	// The question is visible only through the assigned reviewer's inbox, and
	// its references are the durable context pointer the fresh attempt needs.
	inbox := protectedHandoffWaitingTasks(t, fixture, evidence.reviewerToken)
	item, ok := protectedHandoffFindWaitingTask(inbox, evidence.taskID)
	if !ok {
		t.Fatalf("reviewer inbox has no waiting_task for %q: %+v", evidence.taskID, inbox.Items)
	}
	if item.QuestionID != waiting.Question.QuestionID ||
		item.Version != waiting.Question.Version ||
		item.LifecycleVersion != waiting.LifecycleVersion {
		t.Fatalf("reviewer waiting_task = %+v; want question %q version %d lifecycle %d",
			item, waiting.Question.QuestionID, waiting.Question.Version, waiting.LifecycleVersion)
	}
	if len(item.References) != 1 || item.References[0] != protectedHandoffAnswerRef {
		t.Fatalf("reviewer waiting_task references = %v; want [%s]", item.References, protectedHandoffAnswerRef)
	}

	// The assigned reviewer answers from its own inbox with the durable Git
	// reference the question named. The reply admits one fresh execution.
	replied := replyProtectedHandoff(t, fixture, evidence.reviewerToken, evidence.taskID,
		item, protectedHandoffAnswerRef, "reply-handoff")
	freshExecutionID := replied.ExecutionID
	if freshExecutionID == "" || freshExecutionID == oldExecutionID {
		t.Fatalf("reply execution = %q; want a fresh execution distinct from %q", freshExecutionID, oldExecutionID)
	}
	afterReply := readProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID)
	if afterReply.State != "open" || afterReply.CurrentExecution != freshExecutionID || afterReply.Question != nil {
		t.Fatalf("task after reply = %+v; want open with current %q and no question", afterReply, freshExecutionID)
	}

	freshWorkspace, err := fixture.ExecutionWorkspaceID(ctx, freshExecutionID)
	if err != nil || freshWorkspace == "" {
		t.Fatalf("read the fresh execution workspace: %v (%q)", err, freshWorkspace)
	}
	if freshWorkspace == evidence.workspaceID {
		t.Fatalf("fresh execution reused the cancelled workspace %q", freshWorkspace)
	}

	// The fresh execution's first action restores the durable reference from
	// the updated task snapshot; the cancelled attempt's scratch is absent.
	waitProtectedActionRunning(t, fixture, freshExecutionID, protectedHandoffLiveAction)
	freshExecutionToken := waitProtectedGoldenCredential(t, fixture, freshExecutionID, protectedHandoffLiveAction)
	if freshExecutionToken == evidence.executionToken {
		t.Fatalf("the fresh execution reused the cancelled execution credential")
	}
	waitProtectedHandoffActionSucceeded(t, fixture, freshExecutionID, protectedHandoffLiveAction)
	requireProtectedEdgeShellRefusal(t, fixture, evidence.executionToken, oldExecutionID,
		"while the fresh execution runs")
	requireProtectedEdgeGitRefusal(t, fixture, evidence.executionToken, "while-the-fresh-execution-runs")

	// Implement: the fresh execution authors and pushes its own candidate. The
	// restoring action's proof is inside the same retained workspace, and the
	// gated container is a stable reader for both files.
	waitProtectedActionRunning(t, fixture, freshExecutionID, "implement")
	implementContainer := waitProtectedGoldenContainer(t, fixture, freshExecutionID, "implement")
	implementToken := waitProtectedGoldenCredential(t, fixture, freshExecutionID, "implement")
	snapshot := protectedGoldenRead(t, fixture, implementContainer, protectedGoldenTaskFile)
	if !strings.Contains(snapshot, `"body":"`+protectedHandoffAnswerRef+`"`) {
		t.Fatalf("fresh /kodama/task.json does not carry the reply answer: %s", snapshot)
	}
	restored := protectedGoldenRead(t, fixture, implementContainer, protectedHandoffRestored)
	if !strings.Contains(restored, protectedHandoffAnswerRef) {
		t.Fatalf("restored context = %q; want the served ref %q", restored, protectedHandoffAnswerRef)
	}
	releaseProtectedGoldenGate(t, fixture, implementContainer, evidence.gates.implement)
	waitProtectedActionRunning(t, fixture, freshExecutionID, "test")
	testContainer := waitProtectedGoldenContainer(t, fixture, freshExecutionID, "test")
	testToken := waitProtectedGoldenCredential(t, fixture, freshExecutionID, "test")
	requireProtectedGoldenMarker(t, fixture, testContainer, freshExecutionID)
	candidate := protectedGoldenRead(t, fixture, testContainer, protectedGoldenImplementCandidatePath)
	if candidate == "" || candidate == evidence.base {
		t.Fatalf("fresh implement candidate = %q; want a commit different from the base %q", candidate, evidence.base)
	}
	requireProtectedGoldenHead(t, fixture, testContainer, candidate)
	releaseProtectedGoldenGate(t, fixture, testContainer, evidence.gates.test)

	// The fresh request still waits on main B with the canonical subject.
	waitProtectedActionRunning(t, fixture, freshExecutionID, protectedRefReviewAction)
	requireProtectedGoldenNoWorkload(t, fixture, freshExecutionID)
	waitingFresh := inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, freshExecutionID)
	freshReview := actionByName(t, waitingFresh, protectedRefReviewAction)
	if freshReview.State != "running" || freshReview.Decision != "" || freshReview.Actor != "" || freshReview.AnsweredAt != nil {
		t.Fatalf("fresh review action = %+v; want the unanswered running request", freshReview)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != evidence.base {
		t.Fatalf("protected main before the fresh review = %q; want the base %q", main, evidence.base)
	}
	requireProtectedGoldenSubject(t, "fresh review action", freshReview.Subject, protectedGoldenEvidence{
		fixture:     fixture,
		executionID: freshExecutionID,
		base:        evidence.base,
		candidate:   candidate,
	})

	// A live execution that does not own this request cannot answer a human
	// role assignment. This is the real foreign/unrelated execution case, not
	// the protected execution's own principal.
	attemptProtectedHandoffUnrelatedResolve(t, fixture, freshExecutionID, freshReview, waitingFresh.Version)

	// The fresh reviewer passes the exact request; only then does the same
	// fresh execution integrate its candidate.
	refEvidence := protectedRefEvidence{
		fixture:           fixture,
		spec:              protectedRefScenarioSpec{pipelineName: protectedHandoffScenario, workRef: protectedHandoffWorkRef, taskTitle: "protected conversation handoff"},
		gates:             evidence.gates,
		pipelineID:        evidence.pipelineID,
		taskID:            evidence.taskID,
		executionID:       freshExecutionID,
		base:              evidence.base,
		candidate:         candidate,
		workRef:           protectedHandoffWorkRef,
		engineerToken:     fixture.engineerToken,
		engineerPrincipal: fixture.engineerPrincipal,
		reviewerToken:     evidence.reviewerToken,
		reviewerPrincipal: evidence.reviewerPrincipal,
	}
	resolveProtectedRefRequest(t, fixture, evidence.reviewerToken, freshExecutionID,
		protectedRefReviewAction, "pass", "resolve-handoff", nil)
	integrateContainer := prepareProtectedRefIntegrate(t, &refEvidence)
	releaseProtectedGoldenGate(t, fixture, integrateContainer, refEvidence.gates.integrate)
	flow := collectProtectedRefFlow(t, &refEvidence)
	if flow.finished.State != "succeeded" || flow.finished.Cleanup.State != "complete" {
		t.Fatalf("fresh execution state=%q cleanup=%q; want succeeded and complete",
			flow.finished.State, flow.finished.Cleanup.State)
	}
	if flow.finished.TaskID != evidence.taskID || flow.finished.ExecutionID != freshExecutionID {
		t.Fatalf("fresh execution identity = %+v; want task %q execution %q",
			flow.finished, evidence.taskID, freshExecutionID)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != candidate {
		t.Fatalf("protected main = %q; want the fresh candidate %q", main, candidate)
	}
	if parents := fixture.git.CommitParents(t, factory.GitProjectRepository, candidate); len(parents) != 1 || parents[0] != evidence.base {
		t.Fatalf("fresh candidate %s parents = %v; want exactly the base %s", candidate, parents, evidence.base)
	}
	if count := protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef); count != 1 {
		t.Fatalf("protected journal entries = %d; want exactly the fresh integration", count)
	}
	if flow.verification.ExecutionID != freshExecutionID || flow.verification.ActionID != refEvidence.integrate.ActionID {
		t.Fatalf("protected verification execution/action = %q/%q; want %q/%q",
			flow.verification.ExecutionID, flow.verification.ActionID, freshExecutionID, refEvidence.integrate.ActionID)
	}
	if flow.review.Actor != evidence.reviewerPrincipal || flow.review.Decision != "pass" {
		t.Fatalf("protected review = %+v; want the assigned reviewer's pass", flow.review)
	}

	// The cancelled execution stays cancelled, never acquires a decision and
	// contributes no protected effect.
	cancelledAfter := inspectProtectedGoldenExecution(t, fixture, fixture.engineerToken, oldExecutionID)
	if cancelledAfter.State != "cancelled" {
		t.Fatalf("cancelled execution state after integration = %q; want cancelled", cancelledAfter.State)
	}
	oldReview := actionByName(t, cancelledAfter, protectedRefReviewAction)
	if oldReview.State == "running" || oldReview.State == "succeeded" || oldReview.Decision != "" {
		t.Fatalf("cancelled execution review = %+v; want an unactivated, unanswered action", oldReview)
	}
	if oldIntegrate := actionByName(t, cancelledAfter, protectedRefIntegrateAction); oldIntegrate.State == "succeeded" {
		t.Fatalf("cancelled execution integrated: %+v", oldIntegrate)
	}
	oldLogs := fixture.executionLogs(t, cancelledAfter)
	if !strings.Contains(oldLogs, "handoff-ready") {
		t.Fatalf("cancelled execution logs do not show the live handoff checkpoint:\n%s", oldLogs)
	}
	if !strings.Contains(flow.logs, "context-restored") {
		t.Fatalf("fresh execution logs do not show the restored durable context:\n%s", flow.logs)
	}

	// The task keeps both executions in history, carries the reviewer's answer
	// as a conversation message and settles done only on the fresh success.
	if flow.task.State != "done" {
		t.Fatalf("task state = %q; want done", flow.task.State)
	}
	if len(flow.task.ExecutionHistory) != 2 ||
		flow.task.ExecutionHistory[0] != oldExecutionID ||
		flow.task.ExecutionHistory[1] != freshExecutionID {
		t.Fatalf("task history = %v; want [%s %s]", flow.task.ExecutionHistory, oldExecutionID, freshExecutionID)
	}
	foundAnswer := false
	for _, message := range flow.task.Messages {
		if message.Body == protectedHandoffAnswerRef && message.Author == evidence.reviewerPrincipal {
			foundAnswer = true
		}
	}
	if !foundAnswer {
		t.Fatalf("task conversation does not retain the reviewer's answer: %+v", flow.task.Messages)
	}

	protectedEdgeScanCredentials(t, fixture, freshExecutionID, flow.logs,
		evidence.executionToken, implementToken, testToken, flow.integrateToken)
}

// testProtectedHandoffMissingContext proves a reply cannot restore a reference
// the durable store never had: the fresh execution fails its restoring action,
// starts no later action and never moves the protected ref.
func testProtectedHandoffMissingContext(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedHandoffScenario(t, fixture, "protected-handoff-missing",
		"protected missing handoff context", protectedHandoffMissingRef)
	oldExecutionID := evidence.executionID

	asked, waiting := askProtectedHandoff(t, fixture, evidence.taskID,
		[]string{protectedHandoffMissingRef}, "ask-handoff-missing")
	if waiting.State != "waiting" || waiting.Question == nil || waiting.Question.QuestionID != asked.QuestionID {
		t.Fatalf("waiting task = %+v; want the asked question %q", waiting, asked.QuestionID)
	}
	cancelled := waitProtectedGoldenTerminal(t, fixture, oldExecutionID)
	if cancelled.State != "cancelled" {
		t.Fatalf("cancelled execution state = %q; want cancelled", cancelled.State)
	}

	inbox := protectedHandoffWaitingTasks(t, fixture, evidence.reviewerToken)
	item, ok := protectedHandoffFindWaitingTask(inbox, evidence.taskID)
	if !ok {
		t.Fatalf("reviewer inbox has no waiting_task for %q: %+v", evidence.taskID, inbox.Items)
	}
	replied := replyProtectedHandoff(t, fixture, evidence.reviewerToken, evidence.taskID,
		item, protectedHandoffMissingRef, "reply-handoff-missing")
	freshExecutionID := replied.ExecutionID
	if freshExecutionID == "" || freshExecutionID == oldExecutionID {
		t.Fatalf("reply execution = %q; want a fresh execution", freshExecutionID)
	}

	finished := waitProtectedGoldenTerminal(t, fixture, freshExecutionID)
	if finished.State != "failed" {
		t.Fatalf("fresh execution state = %q; want failed on the missing durable context", finished.State)
	}
	live := actionByName(t, finished, protectedHandoffLiveAction)
	if live.State != "failed" {
		t.Fatalf("restoring action state = %q; want failed", live.State)
	}
	for _, name := range []string{"implement", "test", protectedRefReviewAction, protectedRefIntegrateAction} {
		action := actionByName(t, finished, name)
		if action.State == "running" || action.State == "succeeded" {
			t.Fatalf("action %q state = %q after the restoring action failed; want it never started", name, action.State)
		}
	}
	logs := fixture.executionLogs(t, finished)
	if !strings.Contains(logs, "missing durable context") {
		t.Fatalf("failed restoring action does not report the missing durable context:\n%s", logs)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != evidence.base {
		t.Fatalf("protected main = %q after the missing-context failure; want the base %q", main, evidence.base)
	}
	if count := protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef); count != 0 {
		t.Fatalf("the missing-context attempt produced %d protected journal entries; want none", count)
	}

	task := waitProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID,
		"task current execution="+freshExecutionID+" failed", func(read api.TaskReadResult) bool {
			return read.State == "open" && read.CurrentExecution == freshExecutionID
		})
	if task.Question != nil {
		t.Fatalf("failed restoring task still carries a question: %+v", task.Question)
	}
	if len(task.ExecutionHistory) != 2 ||
		task.ExecutionHistory[0] != oldExecutionID ||
		task.ExecutionHistory[1] != freshExecutionID {
		t.Fatalf("task history = %v; want [%s %s]", task.ExecutionHistory, oldExecutionID, freshExecutionID)
	}
	if after := inspectProtectedGoldenExecution(t, fixture, fixture.engineerToken, oldExecutionID); after.State != "cancelled" {
		t.Fatalf("cancelled execution state after the failed restore = %q; want cancelled", after.State)
	}
}

// runProtectedHandoffScenario starts one task execution whose first action is
// the live handoff checkpoint. The rest of the pipeline is the protected path:
// implement -> test -> review -> integrate, gated one action at a time.
func runProtectedHandoffScenario(t *testing.T, fixture *gitHarness, pipelineName, taskTitle, answerRef string) protectedHandoffEvidence {
	t.Helper()
	if fixture == nil || fixture.git == nil {
		t.Fatalf("the protected handoff scenario needs the Git fixture")
	}
	image := fixture.git.EnsureWorkloadImage(t)
	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded project main ref is missing")
	}

	evidence := protectedHandoffEvidence{
		fixture:     fixture,
		base:        base,
		workRef:     protectedHandoffWorkRef,
		executionID: "",
	}
	evidence.reviewerToken, evidence.reviewerPrincipal = grantProtectedReviewer(t, fixture, protectedEdgeReviewerSubject)

	spec := factory.WorkloadCandidateSpec{
		WorkRef:      protectedHandoffWorkRef,
		ExpectedBase: base,
		RequireTask:  true,
	}
	implement, err := fixture.git.WorkloadCandidateCommands(t, spec)
	if err != nil {
		t.Fatalf("author the handoff implement commands: %v", err)
	}
	tested, err := fixture.git.WorkloadTestCandidateCommands(t, spec)
	if err != nil {
		t.Fatalf("author the handoff test commands: %v", err)
	}
	integrated, err := fixture.git.WorkloadIntegrateCandidateCommands(t, spec)
	if err != nil {
		t.Fatalf("author the handoff integrate commands: %v", err)
	}
	gates := protectedRefGatePaths(pipelineName)
	evidence.gates = gates

	inputs := map[string]string{
		"repository":    fixture.git.ProjectResourceID,
		"target_ref":    gitMainRef,
		"expected_base": base,
	}
	document := protectedGoldenPipelineDoc{
		Inputs: []string{"repository", "target_ref", "expected_base"},
		Actions: []pipelineActionDoc{
			{
				Name:     protectedHandoffLiveAction,
				Image:    image.RunnableReference,
				Timeout:  protectedGoldenActionTimeout,
				Commands: protectedHandoffLiveCommands(t, fixture, answerRef),
			},
			{
				Name:     "implement",
				Image:    image.RunnableReference,
				Timeout:  protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(gates.implement, implement),
				Outputs:  []string{"candidate"},
			},
			{
				Name:     "test",
				Image:    image.RunnableReference,
				Timeout:  protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(gates.test, tested),
				Outputs:  []string{"candidate", "test_result"},
			},
			{
				Name:    protectedRefReviewAction,
				Request: "role/reviewer",
				Timeout: protectedGoldenRequestTimeout,
				Subject: protectedGoldenReviewSubject(),
			},
			{
				Name:     protectedRefIntegrateAction,
				Image:    image.RunnableReference,
				Timeout:  protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(gates.integrate, integrated),
			},
		},
	}
	applied := fixture.applyPipeline(pipelineName, marshalProtectedGoldenPipeline(t, document))
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("pipeline.apply returned %+v; want an admitted pipeline and version", applied)
	}
	evidence.pipelineID = applied.PipelineID

	created := api.TaskCreateResult{}
	fixture.Decode(fixture.MustREST(fixture.engineerToken, "task.create", api.TaskCreateRequest{
		ProjectID:  factory.ProjectID,
		Title:      taskTitle,
		PipelineID: applied.PipelineID,
		Inputs:     inputs,
	}, nil, "create-"+pipelineName), &created)
	evidence.taskID = created.TaskID
	if created.TaskID == "" || created.LifecycleVersion == 0 {
		t.Fatalf("task.create returned %+v; want a committed task", created)
	}
	createdRead := readProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID)
	started := api.TaskStartResult{}
	fixture.Decode(fixture.MustREST(fixture.engineerToken, "task.start", api.TaskStartRequest{
		TaskID: evidence.taskID,
	}, &createdRead.LifecycleVersion, "start-"+pipelineName), &started)
	evidence.executionID = started.ExecutionID
	if started.ExecutionID == "" {
		t.Fatalf("task.start returned %+v; want a new execution", started)
	}

	waitProtectedActionRunning(t, fixture, evidence.executionID, protectedHandoffLiveAction)
	container := waitProtectedGoldenContainer(t, fixture, evidence.executionID, protectedHandoffLiveAction)
	evidence.executionToken = waitProtectedGoldenCredential(t, fixture, evidence.executionID, protectedHandoffLiveAction)
	workspace, err := fixture.ExecutionWorkspaceID(t.Context(), evidence.executionID)
	if err != nil || workspace == "" {
		t.Fatalf("read the handoff execution workspace: %v (%q)", err, workspace)
	}
	evidence.workspaceID = workspace
	if got := protectedGoldenExec(t, fixture, container, "printenv", "KODAMA_TASK"); got != evidence.taskID {
		t.Fatalf("handoff KODAMA_TASK = %q; want the task %q", got, evidence.taskID)
	}
	snapshot := protectedGoldenRead(t, fixture, container, protectedGoldenTaskFile)
	if !strings.Contains(snapshot, "[]") {
		t.Fatalf("initial /kodama/task.json = %q; want the empty conversation array", snapshot)
	}
	if strings.Contains(snapshot, `"body":"`) {
		t.Fatalf("initial /kodama/task.json already carries an answer: %s", snapshot)
	}
	evidence.scratch = protectedEdgeWaitContainerFile(t, fixture, evidence.executionID, container,
		protectedHandoffScratch, "live handoff checkpoint execution="+evidence.executionID)
	evidence.live = actionByName(t, inspectProtectedGoldenExecution(t, fixture, fixture.engineerToken, evidence.executionID),
		protectedHandoffLiveAction)
	return evidence
}

// protectedHandoffLiveCommands authors the first action. It publishes no output
// and performs no effect until a reply names the durable Git ref; then it
// proves the exact ref exists on the served repository and records that proof
// inside the fresh workspace.
func protectedHandoffLiveCommands(t *testing.T, fixture *gitHarness, answerRef string) []string {
	t.Helper()
	if fixture.TLS == nil {
		t.Fatalf("the handoff live action needs the private-CA TLS fixture")
	}
	ca := string(fixture.TLS.CAPEM)
	if !strings.HasSuffix(ca, "\n") {
		ca += "\n"
	}
	url := fixture.git.WorkloadRepositoryURL(factory.GitProjectRepository)
	return []string{
		"cat > /workspace/protected-ca.pem <<'KODAMA_HANDOFF_CA'\n" + ca + "KODAMA_HANDOFF_CA",
		"export GIT_SSL_CAINFO=/workspace/protected-ca.pem",
		"export GIT_CONFIG_NOSYSTEM=1",
		"export GIT_CONFIG_GLOBAL=/dev/null",
		"export GIT_ATTR_NOSYSTEM=1",
		"export GIT_TERMINAL_PROMPT=0",
		"git_handoff() {\n" +
			"  KODAMA_HANDOFF_AUTH=\"Authorization: Basic $(printf 'kodama:%s' \"$(cat /kodama/secrets/KODAMA_TOKEN)\" | base64 -w0)\" \\\n" +
			"  git -c http.followRedirects=false --config-env=http.extraHeader=KODAMA_HANDOFF_AUTH \"$@\"\n" +
			"}",
		"if grep -Fq " + shellQuoteForSubject(`"body":"`+answerRef+`"`) + " /kodama/task.json 2>/dev/null; then\n" +
			"  test ! -e " + protectedHandoffScratch + " || { printf 'fresh attempt inherited cancelled scratch\\n' >&2; exit 1; }\n" +
			"  matches=$(git_handoff ls-remote --heads " + shellQuoteForSubject(url) + ")\n" +
			"  if printf '%s\\n' \"$matches\" | grep -F " + shellQuoteForSubject(answerRef) + " > " + protectedHandoffRestored + "; then\n" +
			"    printf 'context-restored\\n'\n" +
			"    exit 0\n" +
			"  fi\n" +
			"  printf 'missing durable context %s\\n' " + shellQuoteForSubject(answerRef) + " >&2\n" +
			"  exit 1\n" +
			"fi\n" +
			"printf 'scratch\\n' > " + protectedHandoffScratch + "\n" +
			"printf 'handoff-ready\\n'\n" +
			"sleep 120",
	}
}

// askProtectedHandoff asks from the latest task lifecycle version. Only the
// documented version conflict is retried; nothing was committed by a refusal.
func askProtectedHandoff(t *testing.T, fixture *gitHarness, taskID string, references []string, key string) (api.TaskAskResult, api.TaskReadResult) {
	t.Helper()
	for range 30 {
		current := readProtectedGoldenTask(t, fixture, fixture.engineerToken, taskID)
		result, publicError := fixture.REST(fixture.engineerToken, "task.ask", api.TaskAskRequest{
			TaskID:     taskID,
			Question:   protectedHandoffQuestion,
			Assignment: "role/reviewer",
			References: references,
		}, &current.LifecycleVersion, key)
		if publicError.Code == api.CodeVersionConflict {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("task.ask refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var asked api.TaskAskResult
		fixture.Decode(result, &asked)
		return asked, readProtectedGoldenTask(t, fixture, fixture.engineerToken, taskID)
	}
	t.Fatalf("task.ask did not converge on a current lifecycle version")
	return api.TaskAskResult{}, api.TaskReadResult{}
}

// replyProtectedHandoff answers the exact inbox question as the assigned
// reviewer and returns the admitted fresh execution.
func replyProtectedHandoff(t *testing.T, fixture *gitHarness, token, taskID string, item api.InboxItem, answer, key string) api.TaskReplyResult {
	t.Helper()
	result := fixture.MustREST(token, "task.reply", api.TaskReplyRequest{
		TaskID:                  taskID,
		QuestionID:              item.QuestionID,
		ExpectedQuestionVersion: item.Version,
		Answer:                  answer,
	}, &item.LifecycleVersion, key)
	var replied api.TaskReplyResult
	fixture.Decode(result, &replied)
	return replied
}

// attemptProtectedHandoffUnrelatedResolve starts one independent live execution
// and presents its current credential to the protected human request. The
// refusal is the exact public authentication error, the request stays pending
// and no resolution receipt is committed.
func attemptProtectedHandoffUnrelatedResolve(t *testing.T, fixture *gitHarness, executionID string, review api.ExecutionAction, executionVersion uint64) {
	t.Helper()
	unrelatedID, unrelatedToken := fixture.startExecution(t, "protected-unrelated")
	defer fixture.stopExecution(t, unrelatedID)
	_, publicError := fixture.REST(unrelatedToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           executionID,
		ActionID:              review.ActionID,
		ExpectedActionVersion: review.Version,
		Decision:              "pass",
	}, &executionVersion, "resolve-unrelated-"+unrelatedID)
	if publicError.Code != api.CodeForbidden {
		t.Fatalf("live unrelated execution resolve code = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeForbidden)
	}
	after := inspectProtectedGoldenExecution(t, fixture, fixture.ProjectToken, executionID)
	requireProtectedEdgePendingRequest(t, after, executionID, "after the unrelated live execution resolve")
	receipts, err := fixture.ReceiptCount(t.Context(), executionID+"/"+review.ActionID)
	if err != nil {
		t.Fatalf("count resolution receipts after the unrelated refusal: %v", err)
	}
	if receipts != 0 {
		t.Fatalf("the unrelated live execution committed %d resolution receipts; want 0", receipts)
	}
}

// waitProtectedHandoffActionSucceeded waits until one named action has
// succeeded. A failed execution or action fails the test with its retained
// logs, so a wrong restore branch is attributed immediately instead of looking
// like a missing workspace file.
func waitProtectedHandoffActionSucceeded(t *testing.T, fixture *gitHarness, executionID, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		execution, ok := fixture.inspectExecution(t, executionID)
		if !ok {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		switch execution.State {
		case "failed", "cancelled":
			t.Fatalf("execution reached %q before action %q succeeded\n%s",
				execution.State, name, fixture.executionLogs(t, execution))
		}
		action := actionByName(t, execution, name)
		switch action.State {
		case "succeeded":
			return
		case "failed", "cancelled":
			t.Fatalf("action %q reached %q\n%s", name, action.State, fixture.executionLogs(t, execution))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("action %q did not succeed within 2m", name)
}

// protectedHandoffWaitingTasks reads only the waiting_task inbox kind for one
// principal.
func protectedHandoffWaitingTasks(t *testing.T, fixture *gitHarness, token string) api.TaskInboxResult {
	t.Helper()
	result := fixture.MustREST(token, "task.inbox", api.TaskInboxRequest{
		ProjectID: factory.ProjectID,
		Kinds:     []string{"waiting_task"},
	}, nil, "")
	var inbox api.TaskInboxResult
	fixture.Decode(result, &inbox)
	return inbox
}

// protectedHandoffFindWaitingTask returns the waiting_task item for one exact
// task.
func protectedHandoffFindWaitingTask(inbox api.TaskInboxResult, taskID string) (api.InboxItem, bool) {
	for _, item := range inbox.Items {
		if item.Kind == "waiting_task" && item.TaskID == taskID {
			return item, true
		}
	}
	return api.InboxItem{}, false
}
