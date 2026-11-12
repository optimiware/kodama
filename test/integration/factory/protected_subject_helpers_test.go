//go:build integration

package factorytest

// Exact evidence and assignment matrix.
//
// This file owns the shared scenario for the four subject/assignment tests in
// protected_subject_matrix_test.go. It composes the golden-path utilities and
// the authored workload commands; it never injects a verification row, an
// approval, an output decision or a Git ref.
//
// Every mismatch case is driven through the real protected flow:
//
//   - implement: task context, exact base, one candidate C on the granted work
//     ref and the declared candidate output (a real, allowed execution push);
//   - test: retained checkout at exactly C, the real test script and the
//     declared outputs;
//   - review: one request action whose immutable subject differs from the
//     eventual protected effect in exactly the field under test (operation,
//     resource, ref, candidate or expected base), or whose assignment is not
//     the human role the protected ref requires;
//   - a genuine authorized pass: the assigned human (or the explicitly
//     service-assigned service) resolves the request with `pass`. The resolve
//     API accepts no replacement subject, so the pass is recorded for the
//     mismatched subject exactly as declared;
//   - integrate: the execution pushes the exact candidate C to refs/heads/main
//     with `--force-with-lease=refs/heads/main:<B>`. The managed hook computes
//     the actual five-field effect subject, finds the genuine pass, and refuses
//     because it is not for this effect. The action captures the managed-hook
//     diagnostic, proves the same live credential still pushes the granted
//     control work ref, and then fails so the execution cannot report success;
//   - the test reads back the unchanged protected ref, the untouched reported
//     journal/verified event, the exact durable refused git.ref.update row and
//     its requested role/reviewer approval.
//
// The positive counterpart is the display-context case: an otherwise exact
// request subject carrying additional fields is still an exact approval, so the
// real candidate integrates. The copied-string case is a live task-free
// execution that carries decision/output-shaped strings but no matching request
// grant; its protected push is refused and the same credential still pushes a
// granted work ref.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	shelloidc "github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/userland/git"
	identityfixture "github.com/optimiware/kodama/test/support/boundaries/identity"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// protectedSubjectWorkRef is the granted work branch every scenario's
	// implement action pushes; it is also the real ref the "ref" mismatch names.
	protectedSubjectWorkRef = "refs/heads/work/protected-subject"
	// protectedSubjectControlRef is the granted work ref the refused integrate
	// action pushes after the protected refusal. It proves the live credential
	// and the served edge still allow a write, so the refusal was the approval.
	protectedSubjectControlRef = "refs/heads/work/protected-subject-control"
	// protectedSubjectPipelineName and protectedSubjectTaskTitle name the
	// per-fixture pipeline and task.
	protectedSubjectPipelineName = "protected-subject-matrix"
	protectedSubjectTaskTitle    = "protected subject matrix"
	// protectedSubjectReviewerSubject is the unique OIDC human granted exactly
	// engineer+reviewer.
	protectedSubjectReviewerSubject = "protected-subject-reviewer"
	// protectedSubjectOperatorSubject is the unique OIDC human granted exactly
	// engineer+operator, so it can answer a role/operator request but not the
	// human role/reviewer approval the protected ref requires.
	protectedSubjectOperatorSubject = "protected-subject-operator"
	// protectedSubjectServiceID is the reviewer-role service used for the
	// explicitly service-assigned pass. It is enabled and holds the same role a
	// human reviewer would, yet a service assignment is not a human approval.
	protectedSubjectServiceID = "protected-subject-reviewer-service"

	// protectedSubjectImplementGate, TestGate and IntegrateGate are the
	// per-action release files. The test creates each only after observing the
	// predecessor's durable state.
	protectedSubjectImplementGate = "/workspace/protected-subject-implement-ready"
	protectedSubjectTestGate      = "/workspace/protected-subject-test-ready"
	protectedSubjectIntegrateGate = "/workspace/protected-subject-integrate-ready"

	// protectedSubjectRefusalError is the captured managed-hook diagnostic of
	// the single protected push attempt.
	protectedSubjectRefusalError = "/workspace/protected-subject-push.err"

	// protectedSubjectActionTimeout keeps the real PostgreSQL statement clock
	// far from an action deadline while a gate holds a container open.
	protectedSubjectActionTimeout = "20m"
	// protectedSubjectRequestTimeout keeps the request well beyond that clock.
	protectedSubjectRequestTimeout = "1h"

	// protectedSubjectDisplayField and protectedSubjectDisplayValue are the
	// additional immutable display context the positive case declares. They are
	// not supplied by the enforcing service, so they cannot change the match.
	protectedSubjectDisplayField = "display_context"
	protectedSubjectDisplayValue = "subject display context"

	// protectedSubjectCopiedPipeline and CopiedAction name the task-free
	// copied-string execution.
	protectedSubjectCopiedPipeline = "protected-subject-copied-strings"
	protectedSubjectCopiedAction   = "copy"
	// protectedSubjectCopiedRef is the granted work ref the copied-string
	// execution's control push creates.
	protectedSubjectCopiedRef         = "refs/heads/work/protected-subject-copied-control"
	protectedSubjectCopiedReady       = "/workspace/protected-subject-copied-ready"
	protectedSubjectCopiedRelease     = "/workspace/protected-subject-copied-release"
	protectedSubjectCopiedError       = "/workspace/protected-subject-copied-push.err"
	protectedSubjectCopiedMarkerReady = "copied-approval-strings-ready"
)

// protectedSubjectRespondent selects who answers the request action.
type protectedSubjectRespondent int

const (
	// protectedSubjectAnsweredByReviewer is the engineer+reviewer human.
	protectedSubjectAnsweredByReviewer protectedSubjectRespondent = iota
	// protectedSubjectAnsweredByOperator is the engineer+operator human.
	protectedSubjectAnsweredByOperator
	// protectedSubjectAnsweredByService is the enabled reviewer-role service.
	protectedSubjectAnsweredByService
)

// protectedSubjectOptions is one scenario. The subject is the exact pipeline
// request template; the caller builds it from the canonical five fields and
// changes exactly the field under test (or adds display context).
type protectedSubjectOptions struct {
	reviewSubject map[string]any
	assignment    string
	respondent    protectedSubjectRespondent
	// expectSuccess selects the positive path (the real candidate integrates).
	expectSuccess bool
	// probeReplacementSubject additionally attempts one raw resolve carrying a
	// replacement subject. The strict public schema must refuse it and the
	// pending request must be unchanged.
	probeReplacementSubject bool
}

// protectedSubjectRespondentIdentity is one answering principal and the
// assignment it must hold.
type protectedSubjectRespondentIdentity struct {
	kind       protectedSubjectRespondent
	token      string
	actor      string
	assignment string
}

// protectedSubjectEvidence is every durable observation the assertions need.
type protectedSubjectEvidence struct {
	fixture     *gitHarness
	options     protectedSubjectOptions
	respondent  protectedSubjectRespondentIdentity
	base        string
	candidate   string
	workRef     string
	controlRef  string
	pipelineID  string
	taskID      string
	executionID string

	executionToken string
	testToken      string
	integrateToken string

	workJournal      git.JournalEntry
	protectedJournal git.JournalEntry
	mainJournalCount int

	review    api.ExecutionAction
	integrate api.ExecutionAction
	finished  api.ExecutionInspectResult
	task      api.TaskReadResult
	logs      string

	verification factory.VerificationRow
	approval     factory.ApprovalAssignment
	reportEvent  factory.EventRow

	// Refusal checkpoint.
	mainBefore            string
	mainAfter             string
	mainJournalBefore     int
	mainJournalAfter      int
	hasAcceptedJournalFor bool
	pushError             string
	controlPushed         bool
	refused               factory.VerificationRow
	refusedApproval       factory.ApprovalAssignment
	replacementSubjectErr api.Error

	candidateAfterControl string
}

// runProtectedSubjectScenario drives one exact subject/assignment scenario and
// returns the durable facts its caller asserts.
func runProtectedSubjectScenario(t *testing.T, fixture *gitHarness, options protectedSubjectOptions) protectedSubjectEvidence {
	t.Helper()
	if fixture == nil || fixture.git == nil {
		t.Fatalf("the protected subject scenario needs the Git fixture")
	}
	image := fixture.git.EnsureWorkloadImage(t)
	if image.RunnableReference == "" || !strings.Contains(image.RunnableReference, "@sha256:") {
		t.Fatalf("workload image reference %q is not digest-pinned", image.RunnableReference)
	}
	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded project main ref is missing")
	}

	evidence := protectedSubjectEvidence{
		fixture:    fixture,
		options:    options,
		base:       base,
		workRef:    protectedSubjectWorkRef,
		controlRef: protectedSubjectControlRef,
	}
	evidence.respondent = protectedSubjectSetUpRespondent(t, fixture, options.respondent)
	if options.assignment != "" && evidence.respondent.assignment != options.assignment {
		t.Fatalf("respondent assignment = %q; want %q", evidence.respondent.assignment, options.assignment)
	}

	spec := factory.WorkloadCandidateSpec{
		WorkRef:      protectedSubjectWorkRef,
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
	var integrated []string
	if options.expectSuccess {
		integrated, err = fixture.git.WorkloadIntegrateCandidateCommands(t, spec)
		if err != nil {
			t.Fatalf("author the integrate workload commands: %v", err)
		}
	} else {
		integrated = protectedSubjectRefusalIntegrateCommands(fixture, spec, protectedSubjectControlRef)
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
				Timeout:  protectedSubjectActionTimeout,
				Commands: protectedGoldenGated(protectedSubjectImplementGate, implement),
				Outputs:  []string{"candidate"},
			},
			{
				Name:     "test",
				Image:    image.RunnableReference,
				Timeout:  protectedSubjectActionTimeout,
				Commands: protectedGoldenGated(protectedSubjectTestGate, tested),
				Outputs:  []string{"candidate", "test_result"},
			},
			{
				Name:    "review",
				Request: evidence.respondent.assignment,
				Timeout: protectedSubjectRequestTimeout,
				Subject: options.reviewSubject,
			},
			{
				Name:     "integrate",
				Image:    image.RunnableReference,
				Timeout:  protectedSubjectActionTimeout,
				Commands: protectedGoldenGated(protectedSubjectIntegrateGate, integrated),
			},
		},
	}
	applied := fixture.applyPipeline(protectedSubjectPipelineName, marshalProtectedGoldenPipeline(t, document))
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("pipeline.apply returned %+v; want an admitted pipeline and version", applied)
	}
	evidence.pipelineID = applied.PipelineID

	created := api.TaskCreateResult{}
	fixture.Decode(fixture.MustREST(fixture.engineerToken, "task.create", api.TaskCreateRequest{
		ProjectID:  factory.ProjectID,
		Title:      protectedSubjectTaskTitle,
		PipelineID: applied.PipelineID,
		Inputs:     inputs,
	}, nil, "create-protected-subject"), &created)
	evidence.taskID = created.TaskID
	if created.TaskID == "" || created.ContentVersion == 0 || created.LifecycleVersion == 0 {
		t.Fatalf("task.create returned %+v; want a committed task", created)
	}
	createdRead := readProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID)
	if createdRead.State != "open" || createdRead.CurrentExecution != "" {
		t.Fatalf("created task = %+v; want open with no execution", createdRead)
	}

	started := api.TaskStartResult{}
	fixture.Decode(fixture.MustREST(fixture.engineerToken, "task.start", api.TaskStartRequest{
		TaskID: evidence.taskID,
	}, &createdRead.LifecycleVersion, "start-protected-subject"), &started)
	evidence.executionID = started.ExecutionID
	if started.ExecutionID == "" || started.LifecycleVersion <= createdRead.LifecycleVersion {
		t.Fatalf("task.start returned %+v; want a new execution and an advanced lifecycle version", started)
	}
	waitProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID,
		"task execution="+started.ExecutionID+" after task.start", func(read api.TaskReadResult) bool {
			return read.CurrentExecution == started.ExecutionID
		})

	// Implement: prove the task context and the mounted credential, then let the
	// real candidate push happen.
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
	releaseProtectedGoldenGate(t, fixture, implementContainer, protectedSubjectImplementGate)

	// Test: read the retained implement output and checkout, then run the real
	// test script and submit its declared outputs.
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
	releaseProtectedGoldenGate(t, fixture, testContainer, protectedSubjectTestGate)

	// The allowed execution push to the granted work ref is the identity/edge
	// control: it really moved the work ref to C before the protected attempt.
	evidence.workJournal = waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository,
		protectedSubjectWorkRef, evidence.candidate)
	if evidence.workJournal.State != git.JournalReported {
		t.Fatalf("work journal = %+v; want a reported entry for %q", evidence.workJournal, protectedSubjectWorkRef)
	}

	// Review: the request runs with no workload and the protected ref is still
	// the base. The declared subject is the immutable subject under test.
	waitProtectedActionRunning(t, fixture, evidence.executionID, "review")
	requireProtectedGoldenNoWorkload(t, fixture, evidence.executionID)
	pending := inspectProtectedGoldenExecution(t, fixture, fixture.ProjectToken, evidence.executionID)
	pendingReview := actionByName(t, pending, "review")
	if pendingReview.State != "running" {
		t.Fatalf("review action state = %q while the request waits; want running", pendingReview.State)
	}
	requireProtectedSubjectRequest(t, fixture, evidence, pendingReview)

	if options.probeReplacementSubject {
		evidence.replacementSubjectErr = protectedSubjectProbeReplacement(t, fixture, evidence, pending)
	}

	// The genuine pass. The resolve API accepts no replacement subject, so the
	// recorded evidence is exactly the request's declared subject.
	protectedSubjectResolve(t, fixture, evidence, pendingReview)

	// Integrate: the retained workspace and both settled outputs, the unchanged
	// base, then the single protected push attempt.
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
	evidence.mainBefore = fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if evidence.mainBefore != base {
		t.Fatalf("protected main = %q before the integrate push; want the reviewed base %q", evidence.mainBefore, base)
	}
	evidence.mainJournalBefore = protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef)
	requireProtectedGoldenCredentialUsable(t, fixture, evidence.executionID, evidence.integrateToken)
	releaseProtectedGoldenGate(t, fixture, integrateContainer, protectedSubjectIntegrateGate)

	finished := waitProtectedGoldenTerminal(t, fixture, evidence.executionID)
	evidence.finished = finished
	evidence.logs = fixture.executionLogs(t, finished)
	evidence.review = actionByName(t, finished, "review")
	evidence.integrate = actionByName(t, finished, "integrate")

	evidence.mainAfter = fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	evidence.mainJournalAfter = protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef)

	if options.expectSuccess {
		if finished.State != "succeeded" {
			t.Fatalf("execution state = %q; want succeeded\n%s", finished.State, evidence.logs)
		}
		protectedSubjectCollectAccepted(t, fixture, &evidence)
		evidence.task = waitProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID,
			"task execution="+evidence.executionID+" succeeded", func(read api.TaskReadResult) bool {
				return read.State == "done"
			})
		return evidence
	}

	if finished.State != "failed" {
		t.Fatalf("execution state = %q after the refused protected push; want failed\n%s",
			finished.State, evidence.logs)
	}
	evidence.refused = waitProtectedEdgeRefusedUpdate(t, fixture, evidence.executionID,
		evidence.integrate.ActionID, evidence.candidate)
	approval, err := fixture.git.RequestedApproval(t.Context(), evidence.refused.ID)
	if err != nil {
		t.Fatalf("read the refused verification's stored requested approval: %v", err)
	}
	evidence.refusedApproval = approval
	evidence.pushError = protectedSubjectLogLineContaining(t, evidence.logs, "kodama git: update rejected")
	evidence.controlPushed = strings.Contains(evidence.logs, "subject-control-pushed")
	evidence.hasAcceptedJournalFor = protectedEdgeHasAcceptedJournal(t, fixture, gitMainRef, evidence.candidate)
	evidence.candidateAfterControl = fixture.git.RemoteRef(t, factory.GitProjectRepository, protectedSubjectControlRef)
	return evidence
}

// protectedSubjectSetUpRespondent creates the one answering principal through
// public operations and returns its credential and exact assignment.
func protectedSubjectSetUpRespondent(t *testing.T, fixture *gitHarness, kind protectedSubjectRespondent) protectedSubjectRespondentIdentity {
	t.Helper()
	switch kind {
	case protectedSubjectAnsweredByReviewer:
		token, principal := grantProtectedSubjectHuman(t, fixture, protectedSubjectReviewerSubject, "engineer", "reviewer")
		return protectedSubjectRespondentIdentity{
			kind: kind, token: token, actor: principal, assignment: "role/reviewer",
		}
	case protectedSubjectAnsweredByOperator:
		token, principal := grantProtectedSubjectHuman(t, fixture, protectedSubjectOperatorSubject, "engineer", "operator")
		return protectedSubjectRespondentIdentity{
			kind: kind, token: token, actor: principal, assignment: "role/operator",
		}
	case protectedSubjectAnsweredByService:
		token := createProtectedReviewerService(t, fixture, protectedSubjectServiceID)
		return protectedSubjectRespondentIdentity{
			kind: kind, token: token, actor: protectedSubjectServiceID,
			assignment: "service/" + protectedSubjectServiceID,
		}
	default:
		t.Fatalf("unknown protected subject respondent %d", kind)
		return protectedSubjectRespondentIdentity{}
	}
}

// grantProtectedSubjectHuman mints one OIDC human, grants exactly the supplied
// project roles through the public operation and reads the committed row back.
func grantProtectedSubjectHuman(t *testing.T, fixture *gitHarness, subject string, roles ...string) (string, string) {
	t.Helper()
	token := fixture.OIDC.Mint(t, identityfixture.TokenSpec{Subject: subject})
	principal, err := shelloidc.PrincipalIDFor(fixture.OIDC.Issuer, subject)
	if err != nil {
		t.Fatalf("derive the principal for %q: %v", subject, err)
	}
	fixture.MustREST(fixture.AdminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: string(principal),
		Roles:       roles,
	}, nil, "grant-"+subject)
	requireProtectedSubjectMembership(t, fixture, string(principal), roles)
	return token, string(principal)
}

// requireProtectedSubjectMembership reads the committed membership back and
// requires exactly the granted role set.
func requireProtectedSubjectMembership(t *testing.T, fixture *gitHarness, principal string, roles []string) {
	t.Helper()
	var listed api.MembershipListResult
	fixture.Decode(fixture.MustREST(fixture.AdminToken, "membership.list", api.MembershipListRequest{
		ProjectID: factory.ProjectID,
	}, nil, ""), &listed)
	for _, membership := range listed.Memberships {
		if membership.PrincipalID != principal {
			continue
		}
		got := map[string]bool{}
		for _, role := range membership.Roles {
			got[role] = true
		}
		if len(membership.Roles) != len(roles) {
			t.Fatalf("committed membership for %q = %v; want exactly %v", principal, membership.Roles, roles)
		}
		for _, role := range roles {
			if !got[role] {
				t.Fatalf("committed membership for %q = %v; want exactly %v", principal, membership.Roles, roles)
			}
		}
		return
	}
	t.Fatalf("membership.list returned no committed membership for %q", principal)
}

// requireProtectedSubjectRequest asserts the pending request action carries the
// exact declared assignment and immutable subject. It reads the resolved
// subject from the public execution projection; no durable fact is synthesized.
func requireProtectedSubjectRequest(t *testing.T, fixture *gitHarness, evidence protectedSubjectEvidence, action api.ExecutionAction) {
	t.Helper()
	if action.Assignment != evidence.respondent.assignment {
		t.Fatalf("request assignment = %q; want %q", action.Assignment, evidence.respondent.assignment)
	}
	if action.Decision != "" || action.AnsweredAt != nil {
		t.Fatalf("pending request already carries decision/answer: %+v", action)
	}
	recorded := protectedSubjectFields(t, "pending request", action.Subject)
	for name, want := range protectedSubjectRecordedExpectation(t, fixture, evidence) {
		got, ok := recorded[name]
		if !ok || got != want {
			t.Fatalf("pending request subject field %q = %q (present %t); want %q", name, got, ok, want)
		}
	}
}

// protectedSubjectRecordedExpectation resolves the test's declared subject
// template against the durable base/candidate. References that are not yet
// resolvable at this point are supplied from the observed run.
func protectedSubjectRecordedExpectation(t *testing.T, fixture *gitHarness, evidence protectedSubjectEvidence) map[string]string {
	t.Helper()
	expect := map[string]string{}
	for name, value := range evidence.options.reviewSubject {
		switch typed := value.(type) {
		case string:
			expect[name] = typed
		case map[string]any:
			from, _ := typed["from"].(string)
			switch from {
			case "inputs.repository":
				expect[name] = fixture.git.ProjectResourceID
			case "inputs.target_ref":
				expect[name] = gitMainRef
			case "inputs.expected_base":
				expect[name] = evidence.base
			case "implement.outputs.candidate":
				expect[name] = evidence.candidate
			default:
				t.Fatalf("request subject field %q has an unexpected reference %q", name, from)
			}
		default:
			t.Fatalf("request subject field %q has an unexpected value %T", name, value)
		}
	}
	return expect
}

// protectedSubjectProbeReplacement performs one raw resolve carrying a
// replacement subject. The strict public schema must refuse it as an unknown
// field, and the outstanding request must be exactly unchanged.
func protectedSubjectProbeReplacement(t *testing.T, fixture *gitHarness, evidence protectedSubjectEvidence, current api.ExecutionInspectResult) api.Error {
	t.Helper()
	review := actionByName(t, current, "review")
	raw := map[string]any{
		"execution_id":            evidence.executionID,
		"action_id":               review.ActionID,
		"expected_action_version": review.Version,
		"decision":                "pass",
		"subject": api.Subject{Fields: []api.SubjectField{
			{Name: "operation", Kind: "string", Value: "git.ref.update"},
			{Name: "resource", Kind: "string", Value: fixture.git.ProjectResourceID},
			{Name: "ref", Kind: "string", Value: gitMainRef},
			{Name: "expected_base", Kind: "string", Value: evidence.base},
			{Name: "candidate", Kind: "string", Value: evidence.candidate},
		}},
	}
	_, publicError := fixture.REST(evidence.respondent.token, "execution.request.resolve", raw,
		&current.Version, "replace-subject-"+evidence.executionID)
	if publicError.Code != api.CodeInvalidInput {
		t.Fatalf("replacement-subject resolve code = %q (%s); want %q",
			publicError.Code, publicError.Message, api.CodeInvalidInput)
	}
	if !strings.Contains(publicError.Message, "unknown field") ||
		!strings.Contains(publicError.Message, "subject") {
		t.Fatalf("replacement-subject resolve message = %q; want the strict-schema unknown-field refusal",
			publicError.Message)
	}
	after := inspectProtectedGoldenExecution(t, fixture, fixture.ProjectToken, evidence.executionID)
	if after.ExecutionID != evidence.executionID || after.State != "running" {
		t.Fatalf("execution after the replacement-subject refusal = %q state %q; want the running request",
			after.ExecutionID, after.State)
	}
	pending := actionByName(t, after, "review")
	if pending.State != "running" || pending.Decision != "" || pending.Actor != "" || pending.AnsweredAt != nil {
		t.Fatalf("replacement-subject refusal changed the pending request: %+v", pending)
	}
	receipts, err := fixture.ReceiptCount(t.Context(), evidence.executionID+"/"+review.ActionID)
	if err != nil {
		t.Fatalf("count resolution receipts after the replacement-subject refusal: %v", err)
	}
	if receipts != 0 {
		t.Fatalf("replacement-subject refusal committed %d receipts; want 0", receipts)
	}
	return publicError
}

// protectedSubjectResolve answers the outstanding request with the genuine
// `pass` from the selected respondent. The only retry is the existing version
// conflict convergence.
func protectedSubjectResolve(t *testing.T, fixture *gitHarness, evidence protectedSubjectEvidence, initial api.ExecutionAction) {
	t.Helper()
	for range 30 {
		current := inspectProtectedGoldenExecution(t, fixture, fixture.ProjectToken, evidence.executionID)
		if current.State != "running" {
			t.Fatalf("execution %s state = %q before resolve; want running", evidence.executionID, current.State)
		}
		action := actionByName(t, current, "review")
		result, publicError := fixture.REST(evidence.respondent.token, "execution.request.resolve", api.ExecutionRequestResolveRequest{
			ExecutionID:           evidence.executionID,
			ActionID:              action.ActionID,
			ExpectedActionVersion: action.Version,
			Decision:              "pass",
		}, &current.Version, "resolve-subject-"+evidence.executionID)
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("execution.request.resolve refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var resolved api.ExecutionRequestResolveResult
		fixture.Decode(result, &resolved)
		if resolved.ExecutionVersion <= current.Version || resolved.ActionVersion <= action.Version {
			t.Fatalf("resolve versions = %+v; want both advanced past execution=%d action=%d",
				resolved, current.Version, action.Version)
		}
		if action.ActionID != initial.ActionID {
			t.Fatalf("review action identity changed from %q to %q", initial.ActionID, action.ActionID)
		}
		return
	}
	t.Fatalf("execution.request.resolve did not converge on a current version")
}

// protectedSubjectRefusalIntegrateCommands renders the authored integrate action
// for a negative case: it performs the same retained-workspace, settled-output
// and ancestry checks as the real integrate action, attempts the exact
// protected push once, captures the managed-hook diagnostic, proves the same
// live credential still pushes a granted work ref, and then fails the action so
// the execution cannot report success.
func protectedSubjectRefusalIntegrateCommands(fixture *gitHarness, spec factory.WorkloadCandidateSpec, controlRef string) []string {
	commands := protectedSubjectGitPrelude(fixture)
	commands = append(commands,
		"test -s "+protectedGoldenImplementCandidatePath,
		"test -s "+protectedGoldenTestCandidatePath,
		"test -s "+protectedGoldenTestResultPath,
		"kodama_subject_candidate=$(cat "+protectedGoldenImplementCandidatePath+")",
		`test "$(cat `+protectedGoldenTestCandidatePath+`)" = "$kodama_subject_candidate"`,
		`test "$(cat `+protectedGoldenTestResultPath+`)" = "pass:$kodama_subject_candidate"`,
		"test \"$(cat "+protectedGoldenMarkerPath+")\" = \"$KODAMA_EXECUTION\"",
		"cd "+protectedGoldenRepositoryDirectory,
		`test "$(git rev-parse HEAD)" = "$kodama_subject_candidate"`,
		`test "$kodama_subject_candidate" != `+shellQuoteForSubject(spec.ExpectedBase),
		"if git merge-base --is-ancestor "+shellQuoteForSubject(spec.ExpectedBase)+` "$kodama_subject_candidate"; then :; else printf 'subject-candidate-not-descended-from-base\n' >&2; exit 1; fi`,
		"if git_kodama push --quiet "+shellQuoteForSubject("--force-with-lease="+gitMainRef+":"+spec.ExpectedBase)+
			" origin \"$kodama_subject_candidate\":"+gitMainRef+" 2>"+protectedSubjectRefusalError+"; then\n"+
			"  printf 'subject-protected-push-accepted\n' >&2\n  exit 1\nfi",
		"printf 'subject-protected-push-refused\\n'",
		"cat "+protectedSubjectRefusalError+" >&2",
		"if ! git_kodama push --quiet origin \"$kodama_subject_candidate\":"+controlRef+"; then\n"+
			"  printf 'subject-control-push-refused\n' >&2\n  exit 1\nfi",
		"printf 'subject-control-pushed\\n'",
		"exit 1",
	)
	return commands
}

// protectedSubjectGitPrelude materializes the public CA and defines the one Git
// authentication function. Every invocation reads the mounted token afresh and
// supplies the derived Basic header only through Git's environment config.
func protectedSubjectGitPrelude(fixture *gitHarness) []string {
	caMaterial := string(fixture.TLS.CAPEM)
	if !strings.HasSuffix(caMaterial, "\n") {
		caMaterial += "\n"
	}
	return []string{
		"cat > /workspace/protected-subject-ca.pem <<'KODAMA_SUBJECT_CA'\n" + caMaterial + "KODAMA_SUBJECT_CA",
		"export GIT_SSL_CAINFO=/workspace/protected-subject-ca.pem",
		"export GIT_CONFIG_NOSYSTEM=1",
		"export GIT_CONFIG_GLOBAL=/dev/null",
		"export GIT_ATTR_NOSYSTEM=1",
		"export GIT_TERMINAL_PROMPT=0",
		"export GIT_TRACE=0 GIT_TRACE_PACKET=0 GIT_TRACE_CURL=0 GIT_CURL_VERBOSE=0",
		"git_kodama() {\n" +
			"  KODAMA_SUBJECT_GIT_AUTHORIZATION=\"Authorization: Basic $(printf 'kodama:%s' \"$(cat /kodama/secrets/KODAMA_TOKEN)\" | base64 -w0)\" \\\n" +
			"  git -c http.followRedirects=false --config-env=http.extraHeader=KODAMA_SUBJECT_GIT_AUTHORIZATION \"$@\"\n" +
			"}",
	}
}

// shellQuoteForSubject renders one single-quoted POSIX shell word.
func shellQuoteForSubject(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// protectedSubjectCollectAccepted reads the positive run's protected journal,
// verification, stored approval and verified event.
func protectedSubjectCollectAccepted(t *testing.T, fixture *gitHarness, evidence *protectedSubjectEvidence) {
	t.Helper()
	evidence.protectedJournal = waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository,
		gitMainRef, evidence.candidate)
	evidence.mainJournalCount = protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef)
	evidence.verification = fixture.verificationByID(t, evidence.protectedJournal.VerificationID)
	approval, err := fixture.git.RequestedApproval(t.Context(), evidence.verification.ID)
	if err != nil {
		t.Fatalf("read the protected verification's stored requested approval: %v", err)
	}
	evidence.approval = approval
	evidence.reportEvent = fixture.waitReportEvent(t, evidence.verification.ID)
}

// protectedSubjectFields decodes one public subject into a validated field map.
func protectedSubjectFields(t *testing.T, label string, subject *api.Subject) map[string]string {
	t.Helper()
	if subject == nil {
		t.Fatalf("%s carries no resolved subject", label)
	}
	fields := map[string]string{}
	for _, field := range subject.Fields {
		if field.Kind != "string" {
			t.Fatalf("%s subject field %q kind = %q; want a typed string", label, field.Name, field.Kind)
		}
		if field.Name == "" {
			t.Fatalf("%s subject carries an unnamed field", label)
		}
		if _, duplicate := fields[field.Name]; duplicate {
			t.Fatalf("%s subject carries duplicate field %q", label, field.Name)
		}
		fields[field.Name] = field.Value
	}
	return fields
}

// protectedSubjectActual is the canonical effect subject the managed hook
// supplies for the exact protected push.
func protectedSubjectActual(fixture *gitHarness, base, candidate string) map[string]string {
	return map[string]string{
		"operation":     "git.ref.update",
		"resource":      fixture.git.ProjectResourceID,
		"ref":           gitMainRef,
		"expected_base": base,
		"candidate":     candidate,
	}
}

// protectedSubjectCopiedEvidence is the durable state of the copied-string
// execution.
type protectedSubjectCopiedEvidence struct {
	fixture     *gitHarness
	base        string
	candidate   string
	executionID string
	action      api.ExecutionAction
	finished    api.ExecutionInspectResult
	logs        string
	credential  string
	staged      map[string]string
	refused     factory.VerificationRow
	approval    factory.ApprovalAssignment
	controlRef  string
	// mainJournalBefore is the reported protected-journal count read before the
	// single protected push attempt.
	mainJournalBefore int
}

// protectedSubjectCopiedOutputs is the exact decision/output shape a real
// approved run would produce. They are staged as ordinary action outputs in an
// execution with no request action; they must never become approval evidence.
func protectedSubjectCopiedOutputs(candidate string) map[string]string {
	return map[string]string{
		"candidate":   candidate,
		"test_result": "pass:" + candidate,
		"decision":    "pass",
		"approval":    "role/reviewer",
		"subject":     "operation=git.ref.update;ref=refs/heads/main;candidate=" + candidate,
	}
}

// runProtectedSubjectCopiedScenario drives one live task-free execution that
// clones a real candidate C, stages the exact decision/output strings, then
// attempts the exact protected push. It has no request action, so no approval
// evidence exists for its execution.
func runProtectedSubjectCopiedScenario(t *testing.T, fixture *gitHarness) protectedSubjectCopiedEvidence {
	t.Helper()
	if fixture == nil || fixture.git == nil {
		t.Fatalf("the copied-string scenario needs the Git fixture")
	}
	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded project main ref is missing")
	}
	// A real candidate C on a granted work ref, pushed by the fixture engineer
	// through the served edge. This is the same object identity a real run
	// would settle.
	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "protected-subject-copied-candidate")
	candidate := fixture.commit(t, directory, "copied-strings candidate")
	stderr, code := fixture.push(directory, fixture.engineerToken, "HEAD:"+protectedSubjectWorkRef)
	if code != 0 {
		t.Fatalf("create the copied-string work candidate: exit %d: %s", code, stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, protectedSubjectWorkRef); remote != candidate {
		t.Fatalf("work ref = %q; want the created candidate %q", remote, candidate)
	}
	if parents := fixture.git.CommitParents(t, factory.GitProjectRepository, candidate); len(parents) != 1 || parents[0] != base {
		t.Fatalf("candidate %s parents = %v; want exactly the base %s", candidate, parents, base)
	}

	image := fixture.git.EnsureWorkloadImage(t)
	evidence := protectedSubjectCopiedEvidence{
		fixture:    fixture,
		base:       base,
		candidate:  candidate,
		controlRef: protectedSubjectCopiedRef,
	}
	commands := protectedSubjectGitPrelude(fixture)
	commands = append(commands,
		"rm -rf -- "+protectedGoldenRepositoryDirectory,
		"git_kodama clone --quiet "+shellQuoteForSubject(fixture.git.WorkloadRepositoryURL(factory.GitProjectRepository))+
			" "+protectedGoldenRepositoryDirectory,
		"cd "+protectedGoldenRepositoryDirectory,
		"git checkout --quiet --detach "+shellQuoteForSubject(candidate),
		`test "$(git rev-parse HEAD)" = `+shellQuoteForSubject(candidate),
		"printf '%s\\n' "+shellQuoteForSubject(protectedSubjectCopiedMarkerReady)+" > "+protectedSubjectCopiedReady,
		"while [ ! -e "+protectedSubjectCopiedRelease+" ]; do sleep 1; done",
		// The decision/output strings are present in the live execution
		// immediately before the protected attempt. They are data, not a grant.
		"printf 'copied-decision=pass\\n'",
		"printf 'copied-approval=role/reviewer\\n'",
		"printf 'copied-candidate=%s\\n' "+shellQuoteForSubject(candidate),
		"printf 'copied-test-result=pass:%s\\n' "+shellQuoteForSubject(candidate),
		"if git_kodama push --quiet "+shellQuoteForSubject("--force-with-lease="+gitMainRef+":"+base)+
			" origin "+shellQuoteForSubject(candidate+":"+gitMainRef)+" 2>"+protectedSubjectCopiedError+"; then\n"+
			"  printf 'copied-approval-push-accepted\n' >&2\n  exit 1\nfi",
		"printf 'copied-approval-push-refused\\n'",
		"cat "+protectedSubjectCopiedError+" >&2",
		"if ! git_kodama push --quiet origin "+shellQuoteForSubject(candidate+":"+protectedSubjectCopiedRef)+"; then\n"+
			"  printf 'copied-control-push-refused\n' >&2\n  exit 1\nfi",
		"printf 'copied-control-pushed\\n'",
		"exit 1",
	)
	applied := fixture.applyPipeline(protectedSubjectCopiedPipeline, marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:     protectedSubjectCopiedAction,
			Image:    image.RunnableReference,
			Timeout:  protectedSubjectActionTimeout,
			Commands: commands,
			Outputs:  []string{"candidate", "test_result", "decision", "approval", "subject"},
		}},
	}))
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("pipeline.apply returned %+v; want an admitted pipeline and version", applied)
	}
	started := fixture.startPipeline(applied.PipelineID, "start-"+protectedSubjectCopiedPipeline)
	evidence.executionID = started.ExecutionID
	t.Cleanup(func() { fixture.stopExecution(t, evidence.executionID) })

	waitProtectedActionRunning(t, fixture, evidence.executionID, protectedSubjectCopiedAction)
	container := waitProtectedGoldenContainer(t, fixture, evidence.executionID, protectedSubjectCopiedAction)
	evidence.credential = waitProtectedGoldenCredential(t, fixture, evidence.executionID, protectedSubjectCopiedAction)
	if ready := protectedEdgeWaitContainerFile(t, fixture, evidence.executionID, container,
		protectedSubjectCopiedReady, "copied-string checkpoint ready"); !strings.Contains(ready, protectedSubjectCopiedMarkerReady) {
		t.Fatalf("copied-string checkpoint = %q; want the ready marker", ready)
	}

	// Stage the exact decision/output strings through the public execution
	// outputs operation with the execution's own live credential. This is a
	// durable commit, not a request decision.
	evidence.staged = protectedSubjectCopiedOutputs(candidate)
	idempotencyKey := "copied-outputs-" + evidence.executionID
	staged := false
	for range 30 {
		current := inspectProtectedGoldenExecution(t, fixture, fixture.ProjectToken, evidence.executionID)
		if current.State != "running" {
			t.Fatalf("execution state = %q before staging the copied outputs; want running", current.State)
		}
		action := actionByName(t, current, protectedSubjectCopiedAction)
		result, publicError := fixture.REST(evidence.credential, "execution.action.outputs", api.ExecutionActionOutputsRequest{
			ExecutionID: evidence.executionID,
			ActionID:    action.ActionID,
			Outputs:     evidence.staged,
		}, &current.Version, idempotencyKey)
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("stage the copied decision/output strings: %s (%s)",
				publicError.Code, publicError.Message)
		}
		var committed api.ExecutionActionOutputsResult
		fixture.Decode(result, &committed)
		if committed.ExecutionVersion <= current.Version || committed.ActionVersion <= action.Version {
			t.Fatalf("staged outputs versions = %+v; want both advanced past execution=%d action=%d",
				committed, current.Version, action.Version)
		}
		evidence.action = action
		staged = true
		break
	}
	if !staged {
		t.Fatalf("copied decision/output staging did not converge on a current version")
	}
	receipts, err := fixture.ReceiptCount(t.Context(), idempotencyKey)
	if err != nil {
		t.Fatalf("count the copied-output staging receipts: %v", err)
	}
	if receipts != 1 {
		t.Fatalf("copied-output staging receipts = %d; want exactly one durable staging commit", receipts)
	}

	evidence.mainJournalBefore = protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef)
	releaseProtectedGoldenGate(t, fixture, container, protectedSubjectCopiedRelease)

	finished := waitProtectedGoldenTerminal(t, fixture, evidence.executionID)
	evidence.finished = finished
	evidence.logs = fixture.executionLogs(t, finished)
	if finished.State != "failed" {
		t.Fatalf("copied-string execution state = %q; want failed after the refused protected push\n%s",
			finished.State, evidence.logs)
	}
	evidence.action = actionByName(t, finished, protectedSubjectCopiedAction)
	evidence.refused = waitProtectedEdgeRefusedUpdate(t, fixture, evidence.executionID,
		evidence.action.ActionID, evidence.candidate)
	approval, err := fixture.git.RequestedApproval(t.Context(), evidence.refused.ID)
	if err != nil {
		t.Fatalf("read the copied-string refused verification's stored requested approval: %v", err)
	}
	evidence.approval = approval
	return evidence
}

// assertProtectedSubjectCopiedRefusal asserts the copied-string refusal: the
// exact protected effect was attempted from a live execution that carried
// decision/output-shaped strings, and it was refused for missing approval
// evidence, not for a broken credential or write rule.
func assertProtectedSubjectCopiedRefusal(t *testing.T, evidence protectedSubjectCopiedEvidence) {
	t.Helper()
	fixture := evidence.fixture
	if fixture == nil || fixture.git == nil {
		t.Fatalf("copied-string refusal evidence has no fixture")
	}
	if evidence.finished.State != "failed" {
		t.Fatalf("execution state = %q; want failed", evidence.finished.State)
	}
	if len(evidence.finished.Actions) != 1 {
		t.Fatalf("copied-string execution admits %d actions; want exactly the one container action",
			len(evidence.finished.Actions))
	}
	if evidence.finished.Actions[0].Name != protectedSubjectCopiedAction ||
		evidence.finished.Actions[0].Kind != "container" {
		t.Fatalf("copied-string action = %+v; want the one declared container action",
			evidence.finished.Actions[0])
	}
	if !containsAll(evidence.logs, "kodama git: update rejected") {
		t.Fatalf("copied-string protected push did not reach the managed hook:\n%s", evidence.logs)
	}
	if strings.Contains(evidence.logs, "copied-approval-push-accepted") {
		t.Fatalf("the copied decision/output strings moved the protected ref")
	}
	// The live execution really carried the copied strings before the attempt:
	// they were staged durably as action outputs and printed by the action.
	for key, value := range evidence.staged {
		marker := "copied-" + strings.ReplaceAll(key, "_", "-") + "=" + value
		if !strings.Contains(evidence.logs, marker) && key != "subject" {
			t.Fatalf("the live execution did not print the copied %s string %q:\n%s",
				key, marker, evidence.logs)
		}
	}
	if !strings.Contains(evidence.logs, "copied-control-pushed") {
		t.Fatalf("the copied-string execution did not push the granted control work ref")
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, evidence.controlRef); remote != evidence.candidate {
		t.Fatalf("control work ref = %q; want the candidate %q", remote, evidence.candidate)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != evidence.base {
		t.Fatalf("protected main = %q after the copied-string refusal; want the unchanged base %q",
			main, evidence.base)
	}
	if protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef) != evidence.mainJournalBefore {
		t.Fatalf("the copied-string refusal changed the reported protected journal count; want the unchanged %d",
			evidence.mainJournalBefore)
	}
	if protectedEdgeHasAcceptedJournal(t, fixture, gitMainRef, evidence.candidate) {
		t.Fatalf("candidate %s was accepted on %s despite the copied-string refusal",
			evidence.candidate, gitMainRef)
	}

	row := evidence.refused
	if row.ID == "" || row.Result != "refused" || row.Approved || row.Operation != approvalOperation {
		t.Fatalf("copied-string refused verification row = %+v; want one refused git.ref.update row", row)
	}
	if row.ExecutionID != evidence.executionID || row.ActionID != evidence.action.ActionID {
		t.Fatalf("copied-string refused verification execution/action = %q/%q; want %q/%q",
			row.ExecutionID, row.ActionID, evidence.executionID, evidence.action.ActionID)
	}
	if row.ServiceID != fixture.git.ServiceID || row.ResourceID != fixture.git.ProjectResourceID ||
		row.BindingID != fixture.git.ProjectBindingID {
		t.Fatalf("copied-string refused verification identity = %+v; want the registered Git binding", row)
	}
	var subject api.Subject
	if err := json.Unmarshal([]byte(row.Subject), &subject); err != nil {
		t.Fatalf("decode the copied-string refused subject %s: %v", row.Subject, err)
	}
	requireProtectedGoldenCanonicalSubject(t, "copied-string refused update", subject, protectedGoldenEvidence{
		fixture:   fixture,
		base:      evidence.base,
		candidate: evidence.candidate,
	})
	if got := evidence.approval.String(); got != "role/reviewer" {
		t.Fatalf("copied-string refused verification requested approval = %q; want role/reviewer", got)
	}
	if _, ok, err := fixture.git.ReportEvent(t.Context(), row.ID); err != nil {
		t.Fatalf("read the copied-string refused verification's report event: %v", err)
	} else if ok {
		t.Fatalf("the copied-string refused verification produced a verified event; want none")
	}
}

// protectedSubjectLogLineContaining returns the first retained-log line that
// contains the wanted managed-hook diagnostic, so a transport or setup failure
// cannot be mistaken for the approval refusal.
func protectedSubjectLogLineContaining(t *testing.T, logs, wanted string) string {
	t.Helper()
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, wanted) {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("retained logs are missing %q:\n%s", wanted, logs)
	return ""
}
