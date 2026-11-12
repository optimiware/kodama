//go:build integration

package factorytest

// Stale review versions, moved targets, current committed rules and ref-rule
// refusals on the protected first-factory path.
//
// Five named tests extend the real protected flow:
//
//   - TestProtectedFactoryStaleReviewVersionIsRefused submits a stale action
//     version directly (and a stale execution version directly) while the real
//     review waits. Both are refused version_conflict without settling; the
//     action stays unanswered, its successor unstarted, main at B and no
//     resolution receipt exists; the reviewer's pass then integrates the same
//     candidate as the control.
//   - TestProtectedFactoryMovedTargetRequiresFreshReview moves main to a
//     separately authorized candidate after the original review passed. The
//     original B->C push fails client-side on its exact lease; the same
//     candidate forced past that lease reaches the server and is refused
//     because its effect subject now names the moved base and carries no
//     approval. main stays at the separately authorized candidate and no
//     journal entry exists for the stale attempt.
//   - TestProtectedFactoryCurrentRulesOverrideEarlierApproval changes committed
//     admin/conf/git.conf after the approval, preserving admin access. The next
//     attempt is refused by the new rule even though its approval is valid;
//     restoring the accepted policy through another served admin commit lets
//     the same candidate integrate.
//   - TestProtectedFactoryDirectHumanGuardedPushIsRefused temporarily grants
//     the engineer an ordinary write of the approval-guarded main ref through a
//     served admin commit. The direct human push still fails: the verification
//     fact is refused because approval evidence requires an execution actor.
//   - TestProtectedFactoryRWRefusesRewindAndDelete obtains real approvals for a
//     fast-forward and then for a rewind of the same protected ref. The
//     fast-forward is accepted; the non-fast-forward is refused by the RW rule
//     after a valid approval was recorded. A delete of an RW+-granted work ref
//     is refused fail-closed; no RW+ delete support is claimed.

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	protectedRefStalePipeline = "protected-stale-review"
	protectedRefMovedPipeline = "protected-moved-target"
	protectedRefRulesPipeline = "protected-current-rules"
	protectedRefHumanPipeline = "protected-human-guarded"
	protectedRefRWPolicyName  = "protected-rw-rules"

	protectedRefStaleWorkRef = "refs/heads/work/protected-stale-review"
	protectedRefMovedWorkRef = "refs/heads/work/protected-moved-target"
	protectedRefRulesWorkRef = "refs/heads/work/protected-current-rules"

	protectedRefStaleTitle = "protected stale review version"
	protectedRefMovedTitle = "protected moved target"
	protectedRefRulesTitle = "protected current rules"

	protectedRefMovedAuthorityPipeline = "protected-moved-authority"
	protectedRefHumanMainControlRef    = "refs/heads/work/protected-human-control"
	protectedRefRWDeleteRef            = "refs/heads/work/protected-rw-delete"
	protectedRefRWControlRef           = "refs/heads/work/protected-rw-control"
	protectedRefRWFirstGate            = "/workspace/protected-rw-first"
	protectedRefRWSecondGate           = "/workspace/protected-rw-second"

	// protectedRefRuleFirstMatchRef, protectedRefRuleExactRef and
	// protectedRefRuleLockedRef are the exact/prefix rule-order refs.
	protectedRefRuleFirstMatchRef = "refs/heads/work/protected-first-match"
	protectedRefRuleExactRef      = "refs/heads/exact"
	protectedRefRulePrefixRef     = "refs/heads/work/protected-prefix"
	protectedRefRuleLockedRef     = "refs/heads/work/protected-locked"
)

// TestProtectedFactoryStaleReviewVersionIsRefused proves a stale review
// expectation is refused without any settlement and the control pass still
// integrates the same candidate.
func TestProtectedFactoryStaleReviewVersionIsRefused(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedRefScenario(t, fixture, protectedRefScenarioSpec{
		pipelineName: protectedRefStalePipeline,
		taskTitle:    protectedRefStaleTitle,
		workRef:      protectedRefStaleWorkRef,
	})
	before := protectedEdgeVerificationCount(t, fixture)

	// A stale expected action version is a direct call, not a conflict-retrying
	// helper: a retry would swallow exactly the version gate this pins.
	current := inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, evidence.executionID)
	review := actionByName(t, current, protectedRefReviewAction)
	_, publicError := fixture.REST(evidence.reviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           evidence.executionID,
		ActionID:              review.ActionID,
		ExpectedActionVersion: review.Version + 7,
		Decision:              "pass",
	}, &current.Version, "stale-"+protectedRefStalePipeline+"-action")
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("stale action version code = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeVersionConflict)
	}
	requireProtectedRefPending(t, fixture, evidence, "after the stale action version")
	requireProtectedRefReceipts(t, fixture, evidence.executionID, review.ActionID, 0)

	// The shared envelope's execution version is a second direct gate.
	current = inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, evidence.executionID)
	review = actionByName(t, current, protectedRefReviewAction)
	staleExecution := current.Version + 7
	_, publicError = fixture.REST(evidence.reviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
		ExecutionID:           evidence.executionID,
		ActionID:              review.ActionID,
		ExpectedActionVersion: review.Version,
		Decision:              "pass",
	}, &staleExecution, "stale-"+protectedRefStalePipeline+"-execution")
	if publicError.Code != api.CodeVersionConflict {
		t.Fatalf("stale execution version code = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeVersionConflict)
	}
	requireProtectedRefPending(t, fixture, evidence, "after the stale execution version")
	requireProtectedRefReceipts(t, fixture, evidence.executionID, review.ActionID, 0)

	requireProtectedRefNoEffect(t, fixture, evidence.executionID, evidence.base, evidence.candidate)
	if after := protectedEdgeVerificationCount(t, fixture); after != before {
		t.Fatalf("a stale resolution committed %d new verification rows; want none", after-before)
	}

	// Control: the exact current pass integrates the same candidate.
	edge := finishProtectedRef(t, &evidence)
	assertProtectedEdgeFlow(t, edge)
}

// TestProtectedFactoryMovedTargetRequiresFreshReview proves a later separately
// authorized main update invalidates the original lease, and that bypassing the
// lease reaches the server where the old approval no longer matches the moved
// base.
func TestProtectedFactoryMovedTargetRequiresFreshReview(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedRefScenario(t, fixture, protectedRefScenarioSpec{
		pipelineName: protectedRefMovedPipeline,
		taskTitle:    protectedRefMovedTitle,
		workRef:      protectedRefMovedWorkRef,
		integrate:    protectedRefMovedIntegrate,
	})
	resolveProtectedRefRequest(t, fixture, evidence.reviewerToken, evidence.executionID,
		protectedRefReviewAction, "pass", "resolve-"+protectedRefMovedPipeline, nil)
	container := prepareProtectedRefIntegrate(t, &evidence)

	// Move main through a separately authorized served push: a fresh execution
	// obtains a real human pass for exactly (main, B, C2), then pushes C2 with
	// its live post-pass credential.
	directory, moved := protectedRefCommitFromBase(t, fixture, "protected-moved-candidate")
	held := startProtectedRefApprovalExecution(t, fixture, protectedRefMovedAuthorityPipeline,
		protectedRefEffectSubject(fixture, evidence.base, moved), evidence.reviewerToken)
	if stderr, code := fixture.push(directory, held.credential, "HEAD:"+gitMainRef); code != 0 {
		t.Fatalf("separately authorized main push: exit %d: %s", code, stderr)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != moved {
		t.Fatalf("protected main = %q after the separately authorized push; want %q", main, moved)
	}
	movedJournal := waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, gitMainRef, moved)
	movedVerification := fixture.verificationByID(t, movedJournal.VerificationID)
	if movedVerification.Result != "approved" || !movedVerification.Approved {
		t.Fatalf("separately authorized verification = %+v; want approved with the approval fact", movedVerification)
	}
	movedApproval, err := fixture.git.RequestedApproval(t.Context(), movedVerification.ID)
	if err != nil {
		t.Fatalf("read the separately authorized approval: %v", err)
	}
	if movedApproval.String() != "role/reviewer" {
		t.Fatalf("separately authorized requested approval = %q; want role/reviewer", movedApproval.String())
	}
	movedFields := protectedRefSubjectFields(t, movedVerification.Subject)
	if movedFields["ref"] != gitMainRef || movedFields["expected_base"] != evidence.base || movedFields["candidate"] != moved {
		t.Fatalf("separately authorized subject = %v; want main %s -> %s", movedFields, evidence.base, moved)
	}

	// Release the original integrate attempt. Its exact lease still names B.
	releaseProtectedGoldenGate(t, fixture, container, evidence.gates.integrate)
	protectedEdgeWaitContainerFile(t, fixture, evidence.executionID, container,
		protectedRefMovedCheckpoint, "moved-target checkpoint")

	leaseError := protectedGoldenRead(t, fixture, container, protectedRefMovedLeaseError)
	hookError := protectedGoldenRead(t, fixture, container, protectedRefMovedHookError)
	if protectedGoldenRead(t, fixture, container, protectedRefMovedLeaseRefused) == "" ||
		protectedGoldenRead(t, fixture, container, protectedRefMovedHookRefused) == "" {
		t.Fatalf("the moved-target checkpoint did not record both refusals")
	}
	lowerLease := strings.ToLower(leaseError)
	if !strings.Contains(lowerLease, "stale") && !strings.Contains(lowerLease, "rejected") {
		t.Fatalf("the original lease was not refused client-side: %s", leaseError)
	}
	if strings.Contains(leaseError, "kodama git") {
		t.Fatalf("the lease attempt reached the server hook; want a client-side stale-info refusal: %s", leaseError)
	}
	if !strings.Contains(hookError, "kodama git: update rejected") {
		t.Fatalf("the forced stale-base push did not reach the managed hook: %s", hookError)
	}

	// The lease attempt produced no server fact. The forced attempt produced one
	// refused verification whose subject names the moved base (C2) and the old
	// candidate (C), with the requested approval role/reviewer: the earlier
	// B->C approval no longer matches.
	if rows := protectedRefVerificationRowsBySubject(t, fixture, evidence.executionID,
		gitMainRef, evidence.base, evidence.candidate); len(rows) != 0 {
		t.Fatalf("the client-side lease attempt produced %d server verification rows; want none", len(rows))
	}
	rows := protectedRefVerificationRowsBySubject(t, fixture, evidence.executionID,
		gitMainRef, moved, evidence.candidate)
	if len(rows) != 1 {
		t.Fatalf("the stale-base server attempt produced %d verification rows; want exactly one", len(rows))
	}
	refused := rows[0]
	if refused.Result != "refused" || refused.Approved {
		t.Fatalf("stale-base verification = %+v; want refused without its old approval", refused)
	}
	requested, err := fixture.git.RequestedApproval(t.Context(), refused.ID)
	if err != nil {
		t.Fatalf("read the stale-base requested approval: %v", err)
	}
	if requested.String() != "role/reviewer" {
		t.Fatalf("stale-base requested approval = %q; want role/reviewer", requested.String())
	}
	atCheckpoint := inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, evidence.executionID)
	integrateAtCheckpoint := actionByName(t, atCheckpoint, protectedRefIntegrateAction)
	if refused.ActionID != integrateAtCheckpoint.ActionID {
		t.Fatalf("stale-base verification action = %q; want the integrate action %q",
			refused.ActionID, integrateAtCheckpoint.ActionID)
	}
	if _, ok, err := fixture.git.ReportEvent(t.Context(), refused.ID); err != nil {
		t.Fatalf("read an event for the stale-base refusal: %v", err)
	} else if ok {
		t.Fatalf("the stale-base refusal produced a verified event")
	}
	requireProtectedRefNoJournal(t, fixture, gitMainRef, evidence.candidate)
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != moved {
		t.Fatalf("protected main = %q; want the separately authorized %q", main, moved)
	}

	// Finish the checkpoint and prove the original execution never integrated
	// C while the separately authorized candidate remains the target.
	releaseProtectedGoldenGate(t, fixture, container, protectedRefMovedFinishGate)
	waitProtectedRefSucceeded(t, &evidence)
	if evidence.review.Decision != "pass" {
		t.Fatalf("the original review decision = %q; want the recorded pass", evidence.review.Decision)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != moved {
		t.Fatalf("protected main = %q after the moved-target checkpoint; want %q", main, moved)
	}
	if strings.Contains(evidence.logs, "protected-integration-pushed") {
		t.Fatalf("the moved-target checkpoint reported a protected integration:\n%s", evidence.logs)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, evidence.workRef); remote != evidence.candidate {
		t.Fatalf("work ref = %q; want the untouched candidate %q", remote, evidence.candidate)
	}
	if reported := protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef); reported != 1 {
		t.Fatalf("reported protected journal entries = %d; want only the separately authorized one", reported)
	}
	heldToken := held.credential
	protectedEdgeScanCredentials(t, fixture, evidence.executionID, evidence.logs,
		evidence.executionToken, evidence.testToken, evidence.integrateToken, heldToken)
}

// TestProtectedFactoryCurrentRulesOverrideEarlierApproval changes the committed
// policy after the approval, proves the next attempt uses the new rule, then
// restores the accepted policy and proves the same candidate still integrates.
func TestProtectedFactoryCurrentRulesOverrideEarlierApproval(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedRefScenario(t, fixture, protectedRefScenarioSpec{
		pipelineName: protectedRefRulesPipeline,
		taskTitle:    protectedRefRulesTitle,
		workRef:      protectedRefRulesWorkRef,
		integrate:    protectedRefPolicyIntegrate,
	})
	resolveProtectedRefRequest(t, fixture, evidence.reviewerToken, evidence.executionID,
		protectedRefReviewAction, "pass", "resolve-"+protectedRefRulesPipeline, nil)
	container := prepareProtectedRefIntegrate(t, &evidence)

	if accepted := fixture.git.AdminPolicy(t); accepted != factory.GitPolicy {
		t.Fatalf("the fixture policy changed before the scenario:\n%s", accepted)
	}
	changedCommit := commitProtectedRefPolicy(t, fixture, protectedRefAdminOnlyMainPolicy)
	if remote := fixture.git.RemoteRef(t, factory.GitAdminRepository, "refs/heads/main"); remote != changedCommit {
		t.Fatalf("admin main = %q; want the changed policy %q", remote, changedCommit)
	}

	// The next attempt is decided by the new committed rule, not by the earlier
	// approval or a cached policy.
	releaseProtectedGoldenGate(t, fixture, container, evidence.gates.integrate)
	protectedEdgeWaitContainerFile(t, fixture, evidence.executionID, container,
		protectedRefPolicyRefusedPath, "policy-change refusal")
	policyError := protectedGoldenRead(t, fixture, container, protectedRefPolicyErrorPath)
	if !strings.Contains(policyError, "kodama git: update rejected") {
		t.Fatalf("the approved push after the rule change did not reach the managed hook: %s", policyError)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != evidence.base {
		t.Fatalf("protected main = %q after the rule change; want the unchanged base %q", main, evidence.base)
	}
	refusedEntry := waitProtectedRefJournalState(t, fixture, gitMainRef, evidence.candidate, git.JournalRefused)
	if refusedEntry.VerificationID == "" {
		t.Fatalf("the rule refusal carries no verification identity: %+v", refusedEntry)
	}
	refusedVerification := fixture.verificationByID(t, refusedEntry.VerificationID)
	if refusedVerification.Result != "approved" || !refusedVerification.Approved {
		t.Fatalf("the rule refusal verification = %+v; want the approval fact (the approval matched; the rule denied)", refusedVerification)
	}
	refusedApproval, err := fixture.git.RequestedApproval(t.Context(), refusedVerification.ID)
	if err != nil {
		t.Fatalf("read the rule refusal's requested approval: %v", err)
	}
	if refusedApproval.String() != "role/reviewer" {
		t.Fatalf("rule refusal requested approval = %q; want role/reviewer", refusedApproval.String())
	}
	refusedFields := protectedRefSubjectFields(t, refusedVerification.Subject)
	if refusedFields["ref"] != gitMainRef || refusedFields["expected_base"] != evidence.base ||
		refusedFields["candidate"] != evidence.candidate {
		t.Fatalf("rule refusal subject = %v; want main %s -> %s", refusedFields, evidence.base, evidence.candidate)
	}
	if _, ok, err := fixture.git.ReportEvent(t.Context(), refusedVerification.ID); err != nil {
		t.Fatalf("read an event for the rule refusal: %v", err)
	} else if ok {
		t.Fatalf("the rule refusal produced a verified event")
	}
	for _, entry := range protectedRefJournalFor(t, fixture, gitMainRef, evidence.candidate) {
		if entry.State == git.JournalAccepted || entry.State == git.JournalReported {
			t.Fatalf("the rule-refused candidate has an %q journal entry: %+v", entry.State, entry)
		}
	}

	// Restore the accepted policy through another served admin commit, preserving
	// admin access, then release the same approved push.
	restoreCommit := commitProtectedRefPolicy(t, fixture, factory.GitPolicy)
	if remote := fixture.git.RemoteRef(t, factory.GitAdminRepository, "refs/heads/main"); remote != restoreCommit {
		t.Fatalf("admin main = %q; want the restored policy %q", remote, restoreCommit)
	}
	releaseProtectedGoldenGate(t, fixture, container, protectedRefPolicyRestoreGate)

	edge := collectProtectedRefFlow(t, &evidence)
	assertProtectedEdgeFlow(t, edge)
	if edge.protectedJournal.VerificationID == refusedVerification.ID {
		t.Fatalf("the restored push reused the refused verification %q; want a fresh verification", refusedVerification.ID)
	}
	if !strings.Contains(evidence.logs, "protected-integration-pushed") {
		t.Fatalf("retained integrate logs are missing the restored integration marker:\n%s", evidence.logs)
	}
}

// TestProtectedFactoryDirectHumanGuardedPushIsRefused grants a human ordinary
// write permission on the approval-guarded ref and proves the direct push still
// fails: approval evidence is execution-only.
func TestProtectedFactoryDirectHumanGuardedPushIsRefused(t *testing.T) {
	fixture := startGitHarness(t)
	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded project main ref is missing")
	}
	// Control before: the engineer's current membership can write the ordinary
	// granted work refs.
	protectedRefHumanWorkControl(t, fixture, "human-before")

	commitProtectedRefPolicy(t, fixture, protectedRefHumanMainPolicy)
	if accepted := fixture.git.AdminPolicy(t); accepted != protectedRefHumanMainPolicy {
		t.Fatalf("the human main-write policy was not accepted:\n%s", accepted)
	}

	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "protected-human-main")
	candidate := fixture.commit(t, directory, "protected direct human guarded push")

	stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+gitMainRef)
	stderr = requireRefusal(t, "direct human push to the approval-guarded main", stderr, code)
	if !strings.Contains(stderr, "kodama git: update rejected") {
		t.Fatalf("the direct human push did not reach the managed hook: %s", stderr)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != base {
		t.Fatalf("protected main = %q after the direct human push; want the unchanged base %q", main, base)
	}
	requireProtectedRefNoJournal(t, fixture, gitMainRef, candidate)

	// The refusal reached the recorder: the exact verification row refuses the
	// human actor because execution approval evidence requires an execution.
	refused := waitProtectedRefVerification(t, fixture, fixture.engineerPrincipal, gitMainRef, candidate)
	if refused.Result != "refused" || refused.Approved {
		t.Fatalf("direct human verification = %+v; want refused without approval", refused)
	}
	if refused.ExecutionID != "" || refused.ActionID != "" {
		t.Fatalf("direct human verification carries execution/action %q/%q; want none",
			refused.ExecutionID, refused.ActionID)
	}
	if refused.ServiceID != fixture.git.ServiceID ||
		refused.ResourceID != fixture.git.ProjectResourceID ||
		refused.BindingID != fixture.git.ProjectBindingID ||
		refused.Operation != approvalOperation {
		t.Fatalf("direct human verification identity = %+v; want the registered Git binding", refused)
	}
	requested, err := fixture.git.RequestedApproval(t.Context(), refused.ID)
	if err != nil {
		t.Fatalf("read the direct human requested approval: %v", err)
	}
	if requested.String() != "role/reviewer" {
		t.Fatalf("direct human requested approval = %q; want role/reviewer", requested.String())
	}
	fields := protectedRefSubjectFields(t, refused.Subject)
	if fields["ref"] != gitMainRef || fields["expected_base"] != base || fields["candidate"] != candidate {
		t.Fatalf("direct human subject = %v; want main %s -> %s", fields, base, candidate)
	}
	if _, ok, err := fixture.git.ReportEvent(t.Context(), refused.ID); err != nil {
		t.Fatalf("read an event for the direct human refusal: %v", err)
	} else if ok {
		t.Fatalf("the direct human refusal produced a verified event")
	}

	// Restore the accepted policy and prove the same repository still accepts the
	// engineer's ordinary write.
	commitProtectedRefPolicy(t, fixture, factory.GitPolicy)
	if accepted := fixture.git.AdminPolicy(t); accepted != factory.GitPolicy {
		t.Fatalf("the accepted policy was not restored:\n%s", accepted)
	}
	protectedRefHumanWorkControl(t, fixture, "human-after")
}

// TestProtectedFactoryRWRefusesRewindAndDelete proves the RW rule refuses a
// rewind even after a valid approval was recorded for that exact moved-base
// effect, and that a delete stays refused.
func TestProtectedFactoryRWRefusesRewindAndDelete(t *testing.T) {
	fixture := startGitHarness(t)
	reviewerToken, _ := grantProtectedReviewer(t, fixture, protectedEdgeReviewerSubject)
	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded project main ref is missing")
	}
	firstDirectory, first := protectedRefCommitFromBase(t, fixture, "protected-rw-first")
	rewindDirectory, rewind := protectedRefCommitFromBase(t, fixture, "protected-rw-rewind")
	if first == rewind {
		t.Fatalf("fixture candidate commits are identical")
	}

	applied := fixture.applyPipeline(protectedRefRWPolicyName, marshalProtectedGoldenPipeline(t, protectedGoldenPipelineDoc{
		Actions: []pipelineActionDoc{
			{
				Name:    protectedRefReviewAction,
				Request: "role/reviewer",
				Timeout: protectedRefRequestTimeout,
				Subject: protectedRefEffectSubject(fixture, base, first),
			},
			{
				Name:    protectedRefHoldAction,
				Image:   fixture.Image.Reference,
				Timeout: protectedGoldenActionTimeout,
				Commands: []string{
					`test ! -e /kodama/task.json`,
					`while [ ! -e ` + protectedRefRWFirstGate + ` ]; do sleep 1; done`,
				},
			},
			{
				Name:    protectedRefSecondReview,
				Request: "role/reviewer",
				Timeout: protectedRefRequestTimeout,
				Subject: protectedRefEffectSubject(fixture, first, rewind),
			},
			{
				Name:    protectedRefSecondHold,
				Image:   fixture.Image.Reference,
				Timeout: protectedGoldenActionTimeout,
				Commands: []string{
					`test ! -e /kodama/task.json`,
					`while [ ! -e ` + protectedRefRWSecondGate + ` ]; do sleep 1; done`,
				},
			},
		},
	}))
	started := fixture.startPipeline(applied.PipelineID, "start-"+protectedRefRWPolicyName)
	t.Cleanup(func() { fixture.stopExecution(t, started.ExecutionID) })

	// First approved effect: a real fast-forward of main is accepted. This is
	// the usable-credential control the rewind refusal below is measured from.
	waitProtectedActionRunning(t, fixture, started.ExecutionID, protectedRefReviewAction)
	resolveProtectedRefRequest(t, fixture, reviewerToken, started.ExecutionID,
		protectedRefReviewAction, "pass", "resolve-"+protectedRefRWPolicyName+"-first", nil)
	waitProtectedActionRunning(t, fixture, started.ExecutionID, protectedRefHoldAction)
	firstContainer := waitProtectedGoldenContainer(t, fixture, started.ExecutionID, protectedRefHoldAction)
	firstToken := waitProtectedGoldenCredential(t, fixture, started.ExecutionID, protectedRefHoldAction)
	if stderr, code := fixture.push(firstDirectory, firstToken, "HEAD:"+gitMainRef); code != 0 {
		t.Fatalf("approved fast-forward main push: exit %d: %s", code, stderr)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != first {
		t.Fatalf("protected main = %q after the fast-forward; want %q", main, first)
	}
	firstJournal := waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, gitMainRef, first)
	firstVerification := fixture.verificationByID(t, firstJournal.VerificationID)
	if firstVerification.Result != "approved" || !firstVerification.Approved {
		t.Fatalf("fast-forward verification = %+v; want approved with the approval fact", firstVerification)
	}
	if err := fixture.TouchContainerFile(t.Context(), firstContainer, protectedRefRWFirstGate); err != nil {
		t.Fatalf("finish the first hold action: %v", err)
	}

	// Second approval: the exact rewound-base effect (main C1 -> C2). It is a
	// genuine human pass in the same execution, so a refusal below cannot be
	// "missing approval".
	waitProtectedActionRunning(t, fixture, started.ExecutionID, protectedRefSecondReview)
	resolveProtectedRefRequest(t, fixture, reviewerToken, started.ExecutionID,
		protectedRefSecondReview, "pass", "resolve-"+protectedRefRWPolicyName+"-second", nil)
	waitProtectedActionRunning(t, fixture, started.ExecutionID, protectedRefSecondHold)
	secondContainer := waitProtectedGoldenContainer(t, fixture, started.ExecutionID, protectedRefSecondHold)
	secondToken := waitProtectedGoldenCredential(t, fixture, started.ExecutionID, protectedRefSecondHold)
	secondAction := actionByName(t, inspectProtectedGoldenExecution(t, fixture, reviewerToken, started.ExecutionID), protectedRefSecondHold)

	// An RW+ work ref is a real delete target for the fail-closed delete check.
	if stderr, code := fixture.push(firstDirectory, fixture.engineerToken, "HEAD:"+protectedRefRWDeleteRef); code != 0 {
		t.Fatalf("create the delete-target work ref: exit %d: %s", code, stderr)
	}

	// RW (no plus) refuses the rewind after the valid approval was recorded.
	rewindError, code := fixture.push(rewindDirectory, secondToken, "--force", "HEAD:"+gitMainRef)
	rewindError = requireRefusal(t, "approved non-fast-forward main push", rewindError, code)
	if !strings.Contains(rewindError, "kodama git: update rejected") {
		t.Fatalf("the approved rewind did not reach the managed hook: %s", rewindError)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != first {
		t.Fatalf("protected main = %q after the refused rewind; want %q", main, first)
	}
	rewindEntry := waitProtectedRefJournalState(t, fixture, gitMainRef, rewind, git.JournalRefused)
	if rewindEntry.VerificationID == "" {
		t.Fatalf("the rewind refusal carries no verification identity: %+v", rewindEntry)
	}
	rewindVerification := fixture.verificationByID(t, rewindEntry.VerificationID)
	if rewindVerification.Result != "approved" || !rewindVerification.Approved {
		t.Fatalf("rewind verification = %+v; want the valid approval fact (the RW rule denied the rewind)", rewindVerification)
	}
	if rewindVerification.ExecutionID != started.ExecutionID || rewindVerification.ActionID != secondAction.ActionID {
		t.Fatalf("rewind verification execution/action = %q/%q; want %q/%q",
			rewindVerification.ExecutionID, rewindVerification.ActionID, started.ExecutionID, secondAction.ActionID)
	}
	rewindApproval, err := fixture.git.RequestedApproval(t.Context(), rewindVerification.ID)
	if err != nil {
		t.Fatalf("read the rewind requested approval: %v", err)
	}
	if rewindApproval.String() != "role/reviewer" {
		t.Fatalf("rewind requested approval = %q; want role/reviewer", rewindApproval.String())
	}
	rewindFields := protectedRefSubjectFields(t, rewindVerification.Subject)
	if rewindFields["ref"] != gitMainRef || rewindFields["expected_base"] != first ||
		rewindFields["candidate"] != rewind {
		t.Fatalf("rewind subject = %v; want main %s -> %s", rewindFields, first, rewind)
	}
	if _, ok, err := fixture.git.ReportEvent(t.Context(), rewindVerification.ID); err != nil {
		t.Fatalf("read an event for the rewind refusal: %v", err)
	} else if ok {
		t.Fatalf("the rewind refusal produced a verified event")
	}
	for _, entry := range protectedRefJournalFor(t, fixture, gitMainRef, rewind) {
		if entry.State == git.JournalAccepted || entry.State == git.JournalReported {
			t.Fatalf("the refused rewind has an %q journal entry: %+v", entry.State, entry)
		}
	}

	// A delete is refused even where RW+ covers the ref. The push
	// protocol still performs its served repository reads, so the absence is
	// asserted for the ref-update operation, not for every verification row.
	journalBefore := len(fixture.git.JournalEntries(t))
	updateRowsBefore := protectedRefUpdateVerificationCount(t, fixture)
	deleteError, code := fixture.push(firstDirectory, secondToken, "--delete", protectedRefRWDeleteRef)
	deleteError = requireRefusal(t, "delete of an RW+ work ref", deleteError, code)
	if !strings.Contains(deleteError, "kodama git: update rejected") {
		t.Fatalf("the delete did not reach the managed hook: %s", deleteError)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, protectedRefRWDeleteRef); remote != first {
		t.Fatalf("delete target = %q; want the unchanged %q", remote, first)
	}
	if after := len(fixture.git.JournalEntries(t)); after != journalBefore {
		t.Fatalf("a refused delete left a journal entry: %d -> %d", journalBefore, after)
	}
	if after := protectedRefUpdateVerificationCount(t, fixture); after != updateRowsBefore {
		t.Fatalf("a refused delete committed %d git.ref.update verification rows; want none", after-updateRowsBefore)
	}
	for _, row := range protectedRefVerificationRowsBySubject(t, fixture, started.ExecutionID, protectedRefRWDeleteRef, "", "") {
		t.Fatalf("a refused delete left verification %+v naming the deleted ref", row)
	}

	// Positive control after both refusals: the same live execution credential
	// pushes a new granted work ref.
	if stderr, code := fixture.push(rewindDirectory, secondToken, "HEAD:"+protectedRefRWControlRef); code != 0 {
		t.Fatalf("post-refusal execution work control: exit %d: %s", code, stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, protectedRefRWControlRef); remote != rewind {
		t.Fatalf("post-refusal control ref = %q; want %q", remote, rewind)
	}

	if err := fixture.TouchContainerFile(t.Context(), secondContainer, protectedRefRWSecondGate); err != nil {
		t.Fatalf("finish the second hold action: %v", err)
	}
	finished := waitProtectedGoldenTerminal(t, fixture, started.ExecutionID)
	if finished.State != "succeeded" {
		t.Fatalf("rules execution state = %q; want succeeded\n%s",
			finished.State, fixture.executionLogs(t, finished))
	}
}

// TestProtectedFactoryRuleFirstMatchWins proves the ordered rule list decides
// on the first ref/principal match: the same deny and allow rules for the same
// ref and principal flip the push solely by being reordered.
func TestProtectedFactoryRuleFirstMatchWins(t *testing.T) {
	fixture := startGitHarness(t)
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "protected-first-match")

	// Positive control under the accepted factory policy: the engineer may
	// create the granted work ref before the rule change.
	control := fixture.commit(t, directory, "protected first-match control")
	if stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+protectedRefRuleFirstMatchRef); code != 0 {
		t.Fatalf("first-match control push: exit %d: %s", code, stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, protectedRefRuleFirstMatchRef); remote != control {
		t.Fatalf("first-match control ref = %q; want %q", remote, control)
	}
	waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, protectedRefRuleFirstMatchRef, control)

	// Deny first, allow second: first-match-wins refuses the push even though a
	// later rule would allow the same ref and principal. The deny covers every
	// ref the later allow could expose, so the repository-level receive
	// advertisement is already write-denied (403) before receive-pack starts;
	// the hook is not reached. The ref and journal prove no effect.
	denyFirst := protectedRefRulePolicy("    - refs/heads/work/ = @engineer\n" +
		"    RW+ refs/heads/work/ = @engineer\n")
	commitProtectedRefPolicy(t, fixture, denyFirst)
	denied := fixture.commit(t, directory, "protected first-match denied")
	text, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+protectedRefRuleFirstMatchRef)
	text = requireRefusal(t, "deny-first rule order", text, code)
	if !strings.Contains(text, "403") && !strings.Contains(strings.ToLower(text), "forbidden") {
		t.Fatalf("the deny-first attempt was not refused as write-denied: %s", text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, protectedRefRuleFirstMatchRef); remote != control {
		t.Fatalf("first-match ref = %q after the deny-first push; want the unchanged %q", remote, control)
	}
	requireProtectedRefNoJournal(t, fixture, protectedRefRuleFirstMatchRef, denied)

	// Allow first, deny second: the identical rule set now accepts the same
	// ref. The allow is not "more specific"; it is simply first.
	allowFirst := protectedRefRulePolicy("    RW+ refs/heads/work/ = @engineer\n" +
		"    - refs/heads/work/ = @engineer\n")
	commitProtectedRefPolicy(t, fixture, allowFirst)
	allowed := fixture.commit(t, directory, "protected first-match allowed")
	if stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+protectedRefRuleFirstMatchRef); code != 0 {
		t.Fatalf("allow-first push: exit %d: %s", code, stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, protectedRefRuleFirstMatchRef); remote != allowed {
		t.Fatalf("first-match ref = %q after the allow-first push; want %q", remote, allowed)
	}
	allowedJournal := waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository,
		protectedRefRuleFirstMatchRef, allowed)
	if allowedJournal.VerificationID == "" {
		t.Fatalf("the allow-first push has no verification identity")
	}
}

// TestProtectedFactoryRuleExactVersusPrefix proves exact rules match only their
// exact ref, prefix rules match only when the rule ends in "/", and an exact
// deny placed before a broader prefix allow wins under first-match order.
func TestProtectedFactoryRuleExactVersusPrefix(t *testing.T) {
	fixture := startGitHarness(t)
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "protected-exact-prefix")

	policy := protectedRefRulePolicy("    RW refs/heads/work/ = @engineer\n" +
		"    RW refs/heads/exact = @engineer\n")
	commitProtectedRefPolicy(t, fixture, policy)

	// Exact and segment-prefix grants work for exactly their own refs.
	for _, ref := range []string{protectedRefRuleExactRef, protectedRefRulePrefixRef} {
		commit := fixture.commit(t, directory, "protected exact-prefix allowed "+ref)
		if stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+ref); code != 0 {
			t.Fatalf("granted ref %s: exit %d: %s", ref, code, stderr)
		}
		if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, ref); remote != commit {
			t.Fatalf("granted ref %s = %q; want %q", ref, remote, commit)
		}
		waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, ref, commit)
	}

	// An exact rule is not a prefix rule, and a prefix rule ends only at a
	// segment boundary. None of these refs is granted. A hook-level rule denial
	// journals one refused intent but never an accepted/reported effect.
	for _, ref := range []string{
		"refs/heads/exact/child",
		"refs/heads/exactness",
		"refs/heads/workshop",
		"refs/heads/workX",
	} {
		commit := fixture.commit(t, directory, "protected exact-prefix refused "+ref)
		text, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+ref)
		text = requireRefusal(t, "ungranted ref "+ref, text, code)
		if !strings.Contains(text, "kodama git: update rejected") {
			t.Fatalf("ungranted ref %s did not reach the managed hook: %s", ref, text)
		}
		if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, ref); remote != "" {
			t.Fatalf("ungranted ref %s moved to %q", ref, remote)
		}
		requireNoAcceptedJournal(t, fixture, ref, commit)
		refused := waitProtectedRefJournalState(t, fixture, ref, commit, git.JournalRefused)
		if refused.VerificationID == "" {
			t.Fatalf("ungranted ref %s refusal carries no verification identity: %+v", ref, refused)
		}
		row := fixture.verificationByID(t, refused.VerificationID)
		if row.Result != "approved" || row.Approved {
			t.Fatalf("ungranted ref %s verification = %+v; want an approved ordinary update fact", ref, row)
		}
	}

	// An exact deny before a broader prefix allow refuses that exact ref; the
	// same two rules reordered accept it. Specificity does not outrank order.
	exactDenyFirst := protectedRefRulePolicy("    - " + protectedRefRuleLockedRef + " = @engineer\n" +
		"    RW+ refs/heads/work/ = @engineer\n")
	commitProtectedRefPolicy(t, fixture, exactDenyFirst)
	locked := fixture.commit(t, directory, "protected exact deny first")
	text, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+protectedRefRuleLockedRef)
	text = requireRefusal(t, "exact deny before prefix allow", text, code)
	if !strings.Contains(text, "kodama git: update rejected") {
		t.Fatalf("the exact-deny-first attempt did not reach the managed hook: %s", text)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, protectedRefRuleLockedRef); remote != "" {
		t.Fatalf("exact-denied ref moved to %q", remote)
	}
	requireNoAcceptedJournal(t, fixture, protectedRefRuleLockedRef, locked)
	lockedRefusal := waitProtectedRefJournalState(t, fixture, protectedRefRuleLockedRef, locked, git.JournalRefused)
	if lockedRefusal.VerificationID == "" {
		t.Fatalf("the exact-denied push carries no verification identity: %+v", lockedRefusal)
	}

	prefixAllowFirst := protectedRefRulePolicy("    RW+ refs/heads/work/ = @engineer\n" +
		"    - " + protectedRefRuleLockedRef + " = @engineer\n")
	commitProtectedRefPolicy(t, fixture, prefixAllowFirst)
	released := fixture.commit(t, directory, "protected prefix allow first")
	if stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+protectedRefRuleLockedRef); code != 0 {
		t.Fatalf("prefix-allow-first push: exit %d: %s", code, stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, protectedRefRuleLockedRef); remote != released {
		t.Fatalf("prefix-allow-first ref = %q; want %q", remote, released)
	}
	waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, protectedRefRuleLockedRef, released)
}

// protectedRefRulePolicy renders the admin repository rule and the project
// repository's read rule with the scenario's ordered project rules appended.
func protectedRefRulePolicy(projectRules string) string {
	return "repo admin\n" +
		"    RW = @admin\n" +
		"\n" +
		"repo " + factory.GitProjectRepository + "\n" +
		"    R = @all\n" +
		projectRules
}
