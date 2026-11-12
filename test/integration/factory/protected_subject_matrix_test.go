//go:build integration

package factorytest

// Exact evidence and assignment matrix.
//
// Four named negatives/controls on the protected first-factory path. Each
// starts its own real fixture and drives the shared subject scenario in
// protected_subject_helpers_test.go: implement -> test -> review -> integrate.
//
//   - TestProtectedFactoryRejectsMismatchedApprovalSubject: five fresh real
//     requests whose immutable subject differs from the actual protected effect
//     in exactly one canonical field (operation, resource, ref, candidate,
//     expected base). Each request receives a genuine authorized human pass;
//     the resolve API accepts no replacement subject, so the refusal belongs to
//     the subsequent managed-hook verification and Git integration. The exact
//     candidate push is refused, the protected ref does not move, no accepted
//     or reported journal entry or verified event appears, and the durable
//     refused git.ref.update row names the actual five-field effect and
//     requests role/reviewer.
//   - TestProtectedFactoryRejectsNonReviewerApprovalAssignment: a role/operator
//     human pass and an explicitly service-assigned reviewer-role service pass
//     are both genuine for their own assignments, and neither satisfies the
//     human role/reviewer approval the protected ref requires.
//   - TestProtectedFactoryIgnoresApprovalDisplayContext: additional immutable
//     display context on an otherwise exact request subject does not prevent
//     the exact approval, so the same candidate integrates.
//   - TestProtectedFactoryCopiedApprovalStringsGrantNothing: a live task-free
//     execution carrying decision/output-shaped strings (pass, role/reviewer,
//     candidate, test_result) but no matching request grant cannot move the
//     protected ref; the same credential still pushes a granted work ref.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestProtectedFactoryRejectsMismatchedApprovalSubject proves every canonical
// approval-subject field is required for the actual protected effect. The pass
// is real and authorized: the mismatch is discovered only when the managed hook
// verifies the exact push.
func TestProtectedFactoryRejectsMismatchedApprovalSubject(t *testing.T) {
	cases := []struct {
		name string
		// field is the one canonical subject field the declared request differs
		// in.
		field string
		// mutate changes exactly that field of the canonical request template.
		mutate func(fixture *gitHarness, base string, subject map[string]any)
		// probeReplacementSubject additionally pins that resolve carries no
		// replacement subject.
		probeReplacementSubject bool
	}{
		{
			name:  "operation",
			field: "operation",
			mutate: func(fixture *gitHarness, base string, subject map[string]any) {
				// A real, admitted resource operation, not a malformed string:
				// admission can never mask the approval mismatch.
				subject["operation"] = "git.read"
			},
			probeReplacementSubject: true,
		},
		{
			name:  "resource",
			field: "resource",
			mutate: func(fixture *gitHarness, base string, subject map[string]any) {
				// The other registered resource identity of the same served Git
				// binding family, so the value is a real resource.
				if fixture.git.AdminResourceID == "" {
					t.Fatalf("the fixture has no second registered resource identity")
				}
				subject["resource"] = fixture.git.AdminResourceID
			},
		},
		{
			name:  "ref",
			field: "ref",
			mutate: func(fixture *gitHarness, base string, subject map[string]any) {
				// The granted work branch this same execution really moves.
				subject["ref"] = protectedSubjectWorkRef
			},
		},
		{
			name:  "candidate",
			field: "candidate",
			mutate: func(fixture *gitHarness, base string, subject map[string]any) {
				// The real reviewed base commit, not an invented object id.
				subject["candidate"] = base
			},
		},
		{
			name:  "expected-base",
			field: "expected_base",
			mutate: func(fixture *gitHarness, base string, subject map[string]any) {
				// The real candidate the implement action settles.
				subject["expected_base"] = map[string]any{"from": "implement.outputs.candidate"}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := startGitHarness(t)
			base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
			if base == "" {
				t.Fatalf("the seeded project main ref is missing")
			}
			subject := protectedGoldenReviewSubject()
			testCase.mutate(fixture, base, subject)

			evidence := runProtectedSubjectScenario(t, fixture, protectedSubjectOptions{
				reviewSubject:           subject,
				assignment:              "role/reviewer",
				respondent:              protectedSubjectAnsweredByReviewer,
				probeReplacementSubject: testCase.probeReplacementSubject,
			})
			assertProtectedSubjectRefusal(t, evidence, testCase.field)
			if testCase.probeReplacementSubject && evidence.replacementSubjectErr.Code != api.CodeInvalidInput {
				t.Fatalf("replacement-subject refusal code = %q; want %q",
					evidence.replacementSubjectErr.Code, api.CodeInvalidInput)
			}
		})
	}
}

// TestProtectedFactoryRejectsNonReviewerApprovalAssignment proves the protected
// ref requires the exact human role/reviewer approval. A role/operator human
// pass and a service-assigned reviewer-role pass are both genuine decisions for
// their own assignments, and neither is execution approval.
func TestProtectedFactoryRejectsNonReviewerApprovalAssignment(t *testing.T) {
	cases := []struct {
		name       string
		assignment string
		respondent protectedSubjectRespondent
	}{
		{
			name:       "role-operator",
			assignment: "role/operator",
			respondent: protectedSubjectAnsweredByOperator,
		},
		{
			name:       "service-reviewer",
			assignment: "service/" + protectedSubjectServiceID,
			respondent: protectedSubjectAnsweredByService,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := startGitHarness(t)
			evidence := runProtectedSubjectScenario(t, fixture, protectedSubjectOptions{
				reviewSubject: protectedGoldenReviewSubject(),
				assignment:    testCase.assignment,
				respondent:    testCase.respondent,
			})
			assertProtectedSubjectRefusal(t, evidence, "")
			if evidence.review.Assignment != testCase.assignment {
				t.Fatalf("genuine pass assignment = %q; want %q",
					evidence.review.Assignment, testCase.assignment)
			}
		})
	}
}

// TestProtectedFactoryIgnoresApprovalDisplayContext proves additional immutable
// display context does not prevent an otherwise exact approval: the exact
// candidate reaches main through the real managed hook.
func TestProtectedFactoryIgnoresApprovalDisplayContext(t *testing.T) {
	fixture := startGitHarness(t)
	subject := protectedGoldenReviewSubject()
	subject[protectedSubjectDisplayField] = protectedSubjectDisplayValue

	evidence := runProtectedSubjectScenario(t, fixture, protectedSubjectOptions{
		reviewSubject: subject,
		assignment:    "role/reviewer",
		respondent:    protectedSubjectAnsweredByReviewer,
		expectSuccess: true,
	})
	assertProtectedSubjectIntegrated(t, evidence)
}

// TestProtectedFactoryCopiedApprovalStringsGrantNothing proves decision/output
// strings are data, never approval evidence: a live execution that stages the
// exact decision and output shapes, but has no matching request grant, cannot
// move the protected ref.
func TestProtectedFactoryCopiedApprovalStringsGrantNothing(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedSubjectCopiedScenario(t, fixture)
	assertProtectedSubjectCopiedRefusal(t, evidence)
}

// assertProtectedSubjectRefusal asserts the common refusal oracle for one
// mismatched-subject or mismatched-assignment scenario.
//
// wantSubjectField is the one canonical field the declared request subject
// differs in; an empty value means the subject was exact and only the
// assignment was wrong.
func assertProtectedSubjectRefusal(t *testing.T, evidence protectedSubjectEvidence, wantSubjectField string) {
	t.Helper()
	fixture := evidence.fixture
	if fixture == nil || fixture.git == nil {
		t.Fatalf("subject refusal evidence has no fixture")
	}
	if evidence.finished.State != "failed" {
		t.Fatalf("execution state = %q after the refused protected push; want failed", evidence.finished.State)
	}
	if evidence.pushError == "" || !containsAll(evidence.pushError, "kodama git: update rejected") {
		t.Fatalf("protected push did not reach the managed hook: %q", evidence.pushError)
	}
	if !evidence.controlPushed {
		t.Fatalf("the live execution did not push the granted control work ref after the refusal")
	}
	if evidence.candidateAfterControl != evidence.candidate {
		t.Fatalf("control work ref = %q; want the candidate %q",
			evidence.candidateAfterControl, evidence.candidate)
	}
	if evidence.mainAfter != evidence.base || evidence.mainBefore != evidence.base {
		t.Fatalf("protected main before/after = %q/%q; want the unchanged base %q",
			evidence.mainBefore, evidence.mainAfter, evidence.base)
	}
	if evidence.mainJournalAfter != evidence.mainJournalBefore {
		t.Fatalf("reported protected journal entries after the refusal = %d; want the unchanged %d",
			evidence.mainJournalAfter, evidence.mainJournalBefore)
	}
	if evidence.hasAcceptedJournalFor {
		t.Fatalf("candidate %s was accepted or reported on %s despite the refusal",
			evidence.candidate, gitMainRef)
	}

	// The pass is genuine and was recorded for the declared immutable subject.
	review := evidence.review
	if review.State != "succeeded" || review.Decision != "pass" || review.AnsweredAt == nil {
		t.Fatalf("request action = %+v; want a succeeded pass with an answered-at fact", review)
	}
	if review.Actor != evidence.respondent.actor || review.Actor == "" {
		t.Fatalf("request action actor = %q; want %q", review.Actor, evidence.respondent.actor)
	}
	if review.Assignment != evidence.respondent.assignment {
		t.Fatalf("request action assignment = %q; want %q",
			review.Assignment, evidence.respondent.assignment)
	}
	recorded := protectedSubjectFields(t, "review action", review.Subject)
	actual := protectedSubjectActual(fixture, evidence.base, evidence.candidate)
	differing := make([]string, 0, 1)
	for name, want := range actual {
		got, ok := recorded[name]
		if !ok {
			t.Fatalf("recorded request subject is missing the canonical field %q: %v", name, recorded)
		}
		if got != want {
			differing = append(differing, name)
		}
	}
	if wantSubjectField == "" {
		if len(recorded) != len(actual) || len(differing) != 0 {
			t.Fatalf("request subject = %v; want exactly the actual canonical subject %v", recorded, actual)
		}
	} else {
		if len(recorded) != len(actual) {
			t.Fatalf("mismatched request subject has %d fields %v; want exactly the canonical five", len(recorded), recorded)
		}
		if len(differing) != 1 || differing[0] != wantSubjectField {
			t.Fatalf("request subject differs from the actual effect in %v; want exactly %q", differing, wantSubjectField)
		}
	}
	if evidence.review.ActionID == evidence.integrate.ActionID || evidence.review.ActionID == "" {
		t.Fatalf("the pass and the refused integration share the action identity %q", evidence.review.ActionID)
	}

	// The exact durable refused row names the actual effect, this execution and
	// the integration action, and requests the human role/reviewer approval.
	row := evidence.refused
	if row.ID == "" || row.Result != "refused" || row.Approved {
		t.Fatalf("refused verification row = %+v; want one refused, unapproved row", row)
	}
	if row.Operation != approvalOperation {
		t.Fatalf("refused verification operation = %q; want %q", row.Operation, approvalOperation)
	}
	if row.ExecutionID != evidence.executionID || row.ActionID != evidence.integrate.ActionID {
		t.Fatalf("refused verification execution/action = %q/%q; want %q/%q",
			row.ExecutionID, row.ActionID, evidence.executionID, evidence.integrate.ActionID)
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
		t.Fatalf("refused verification requested approval = %+v; want exactly role/reviewer",
			evidence.refusedApproval)
	}
	if _, ok, err := fixture.git.ReportEvent(t.Context(), row.ID); err != nil {
		t.Fatalf("read the refused verification's report event: %v", err)
	} else if ok {
		t.Fatalf("the refused verification %s produced a verified event; want none", row.ID)
	}

	// The allowed execution push to the granted work ref happened before the
	// refusal and really moved that ref to C: the refusal was not a broken
	// credential or a missing write rule.
	if evidence.workJournal.State != git.JournalReported ||
		evidence.workJournal.Candidate != evidence.candidate {
		t.Fatalf("work journal = %+v; want a reported entry for the candidate", evidence.workJournal)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, protectedSubjectWorkRef); remote != evidence.candidate {
		t.Fatalf("work ref = %q; want the reported candidate %q", remote, evidence.candidate)
	}
}

// assertProtectedSubjectIntegrated asserts the positive display-context run: the
// exact candidate integrated through the real managed hook, and the recorded
// request subject is the canonical five fields plus the declared display field.
func assertProtectedSubjectIntegrated(t *testing.T, evidence protectedSubjectEvidence) {
	t.Helper()
	fixture := evidence.fixture
	if fixture == nil || fixture.git == nil {
		t.Fatalf("subject integration evidence has no fixture")
	}
	if evidence.base == "" || evidence.candidate == "" || evidence.candidate == evidence.base {
		t.Fatalf("integration evidence base=%q candidate=%q; want distinct commits",
			evidence.base, evidence.candidate)
	}
	finished := evidence.finished
	if finished.State != "succeeded" || finished.Cleanup.State != "complete" {
		t.Fatalf("execution state=%q cleanup=%q; want succeeded and complete",
			finished.State, finished.Cleanup.State)
	}
	if len(finished.Actions) != 4 {
		t.Fatalf("execution admits %d actions; want 4", len(finished.Actions))
	}
	for _, action := range []api.ExecutionAction{evidence.review, evidence.integrate} {
		if action.State != "succeeded" {
			t.Fatalf("action %q state = %q; want succeeded", action.Name, action.State)
		}
	}
	if got := evidence.candidate; fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef) != got {
		t.Fatalf("protected main = %q; want the candidate %q",
			fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef), got)
	}
	if parents := fixture.git.CommitParents(t, factory.GitProjectRepository, evidence.candidate); len(parents) != 1 || parents[0] != evidence.base {
		t.Fatalf("candidate %s parents = %v; want exactly the reviewed base %s",
			evidence.candidate, parents, evidence.base)
	}
	if history := fixture.git.History(t, factory.GitProjectRepository); len(history) != 2 ||
		history[0] != evidence.candidate || history[1] != evidence.base {
		t.Fatalf("protected history = %v; want exactly [%s %s]", history, evidence.candidate, evidence.base)
	}

	if evidence.mainJournalCount != 1 {
		t.Fatalf("reported protected journal entries = %d; want exactly one", evidence.mainJournalCount)
	}
	journal := evidence.protectedJournal
	if journal.Ref != gitMainRef || journal.State != git.JournalReported ||
		journal.ExpectedBase != evidence.base || journal.Candidate != evidence.candidate {
		t.Fatalf("protected journal = %+v; want a reported %s -> %s entry",
			journal, evidence.base, evidence.candidate)
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

	// The genuine pass carries the canonical five fields plus the display field;
	// the display field is immutable context, never a matching key.
	review := evidence.review
	if review.Assignment != evidence.respondent.assignment || review.Decision != "pass" ||
		review.Actor != evidence.respondent.actor || review.AnsweredAt == nil {
		t.Fatalf("review action = %+v; want the genuine pass", review)
	}
	recorded := protectedSubjectFields(t, "review action", review.Subject)
	actual := protectedSubjectActual(fixture, evidence.base, evidence.candidate)
	if len(recorded) != len(actual)+1 {
		t.Fatalf("recorded request subject fields = %v; want the canonical five plus %q",
			recorded, protectedSubjectDisplayField)
	}
	for name, want := range actual {
		if got, ok := recorded[name]; !ok || got != want {
			t.Fatalf("recorded request subject field %q = %q (present %t); want %q",
				name, got, ok, want)
		}
	}
	if got := recorded[protectedSubjectDisplayField]; got != protectedSubjectDisplayValue {
		t.Fatalf("recorded display field = %q; want %q", got, protectedSubjectDisplayValue)
	}

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
	for _, marker := range []string{
		"candidate-output-submitted",
		"test-outputs-submitted",
		"protected-integration-pushed",
		"repository-config-clean",
	} {
		if !strings.Contains(evidence.logs, marker) {
			t.Fatalf("retained workload logs are missing %q:\n%s", marker, evidence.logs)
		}
	}
	protectedEdgeScanCredentials(t, fixture, evidence.executionID, evidence.logs,
		evidence.executionToken, evidence.testToken, evidence.integrateToken)
}

// containsAll reports whether value contains every wanted substring.
func containsAll(value string, wanted ...string) bool {
	for _, part := range wanted {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
