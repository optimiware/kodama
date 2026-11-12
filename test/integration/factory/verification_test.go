//go:build integration

package factorytest

// Served approval verification without Git.
//
// Coverage map (KODAMA.md, S05 §4):
//   - exact same-execution request evidence approves one live execution actor
//     for one exact operation/resource/ref/expected_base/candidate;
//   - a changed subject field never matches, another execution's evidence never
//     matches, and a pass for the same resource/operation with a different
//     effect subject never matches;
//   - humans and services cannot carry execution approval evidence even when
//     they hold reviewer or copy the resolved answer string;
//   - every new verification attempt rechecks live authority: after the
//     execution is cancelled through the public API, the prior approved result
//     does not authorize a repeat.
//
// The tests use only public operations for setup and effect. The persisted
// request decision is produced by execution.request.resolve; the actor
// credential is read from the live workload's mounted token file. No test-only
// store read supplies missing client authority or fabricates approval.
//
// Boundary: this is Shell-verification evidence only. It makes no claim about
// protected Git integration, Git enforcement, or reviewer-only discovery, and
// no claim about the Git read identity.

import (
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// TestServedApprovalRequiresExactExecutionAndSubject proves the positive exact
// match and the three subject/identity negatives the enforcement path needs.
func TestServedApprovalRequiresExactExecutionAndSubject(t *testing.T) {
	fixture := newApprovalFixture(t)

	// canonicalGitSubject is the exact five-field subject the Git service sends
	// for `git.ref.update`: operation, resource (the registered repository
	// resource identity), ref, expected_base and candidate.
	canonicalGitSubject := func(resourceID string) []api.SubjectField {
		return []api.SubjectField{
			{Name: "operation", Kind: "string", Value: approvalOperation},
			{Name: "resource", Kind: "string", Value: resourceID},
			{Name: "ref", Kind: "string", Value: approvalRef},
			{Name: "expected_base", Kind: "string", Value: approvalBase},
			{Name: "candidate", Kind: "string", Value: approvalCandidate},
		}
	}

	type liveExecution struct {
		id    string
		token string
	}
	var approved liveExecution
	var plain liveExecution
	var otherSubject liveExecution

	t.Run("exact same-execution evidence approves the live execution", func(t *testing.T) {
		id, token := fixture.startApprovedExecution(t, "exact", "start-exact", approvalCandidate)
		approved = liveExecution{id: id, token: token}

		verified, result, publicError := fixture.verifyRaw(t, token, fixture.resourceID,
			approvalSubject(approvalRef, approvalBase, approvalCandidate), "role/reviewer")
		if publicError.Code != "" {
			t.Fatalf("actor.verify refused the exact recorded approval: %s (%s); execution %s",
				publicError.Code, publicError.Message, id)
		}
		requireApproved(t, verified)
		if strings.Contains(string(result.Body), token) {
			t.Fatalf("actor.verify response body echoed the forwarded execution credential")
		}
	})

	t.Run("Git's canonical five-field subject approves after a successful pass", func(t *testing.T) {
		if approved.token == "" {
			t.Fatal("positive control did not produce a live execution credential")
		}
		// The enforcing Git service submits operation and resource (the
		// registered resource identity) alongside the effect fields. The
		// approval match must keep and compare all five.
		verified, publicError := fixture.verify(t, approved.token, fixture.resourceID,
			canonicalGitSubject(fixture.resourceID), "role/reviewer")
		if publicError.Code != "" {
			t.Fatalf("actor.verify refused Git's canonical five-field subject: %s (%s)",
				publicError.Code, publicError.Message)
		}
		requireApproved(t, verified)
	})

	t.Run("changed candidate, base or ref never matches the recorded approval", func(t *testing.T) {
		base := liveExecution{approved.id, approved.token}
		if base.token == "" {
			t.Fatal("positive control did not produce a live execution credential")
		}
		cases := []struct {
			name    string
			subject []api.SubjectField
		}{
			{
				name:    "candidate",
				subject: approvalSubject(approvalRef, approvalBase, approvalOtherCandidate),
			},
			{
				name:    "expected_base",
				subject: approvalSubject(approvalRef, approvalOtherCandidate, approvalCandidate),
			},
			{
				name:    "ref",
				subject: approvalSubject("refs/heads/other", approvalBase, approvalCandidate),
			},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				_, publicError := fixture.verify(t, base.token, fixture.resourceID, testCase.subject, "role/reviewer")
				if publicError.Code != api.CodeForbidden {
					t.Fatalf("changed %s: actor.verify code = %q (%s); want %q",
						testCase.name, publicError.Code, publicError.Message, api.CodeForbidden)
				}
			})
		}
	})

	t.Run("another execution without approval evidence never matches", func(t *testing.T) {
		id, token := fixture.startPlainExecution(t, "plain", "start-plain")
		plain = liveExecution{id: id, token: token}

		// The approved execution's exact subject is supplied with an execution
		// that has no request action at all. Evidence is scoped to the actor's
		// own execution, so the foreign fact must never approve this actor.
		_, publicError := fixture.verify(t, token, fixture.resourceID,
			approvalSubject(approvalRef, approvalBase, approvalCandidate), "role/reviewer")
		if publicError.Code != api.CodeForbidden {
			t.Fatalf("foreign execution evidence: actor.verify code = %q (%s); want %q",
				publicError.Code, publicError.Message, api.CodeForbidden)
		}
	})

	t.Run("a pass for the same resource and operation but another subject never matches", func(t *testing.T) {
		id, token := fixture.startApprovedExecution(t, "other-subject", "start-other-subject", approvalOtherCandidate)
		otherSubject = liveExecution{id: id, token: token}

		// This execution genuinely holds a pass decision for the same enforcing
		// service, resource and operation. Only its effect subject differs, so a
		// matcher that selects approval by resource/operation alone would accept.
		_, publicError := fixture.verify(t, token, fixture.resourceID,
			approvalSubject(approvalRef, approvalBase, approvalCandidate), "role/reviewer")
		if publicError.Code != api.CodeForbidden {
			t.Fatalf("resource-only approval selection: actor.verify code = %q (%s); want %q",
				publicError.Code, publicError.Message, api.CodeForbidden)
		}
	})

	t.Cleanup(func() {
		for _, execution := range []liveExecution{approved, plain, otherSubject} {
			if execution.id != "" {
				fixture.cancelExecution(t, execution.id, "cleanup-"+execution.id)
			}
		}
	})
}

// TestServedApprovalDoesNotTransferToHumanOrServiceActors proves the execution
// boundary of the approval fact. The positive execution control runs first and
// again last so every refusal is attributable to the actor, not to a broken
// service binding.
func TestServedApprovalDoesNotTransferToHumanOrServiceActors(t *testing.T) {
	fixture := newApprovalFixture(t)

	executionID, executionToken := fixture.startApprovedExecution(t, "actor-control", "start-actor-control", approvalCandidate)
	exact := approvalSubject(approvalRef, approvalBase, approvalCandidate)

	t.Run("positive control approves the live execution", func(t *testing.T) {
		verified, publicError := fixture.verify(t, executionToken, fixture.resourceID, exact, "role/reviewer")
		if publicError.Code != "" {
			t.Fatalf("positive control: actor.verify refused the exact approval: %s (%s)",
				publicError.Code, publicError.Message)
		}
		requireApproved(t, verified)
	})

	t.Run("a human holding the assigned reviewer role cannot carry execution approval", func(t *testing.T) {
		// The dual-role reader/reviewer is a real human currently holding
		// engineer+reviewer. It can inspect and resolve, but it is not an
		// eligible execution actor.
		_, publicError := fixture.verify(t, fixture.dualToken, fixture.resourceID, exact, "role/reviewer")
		if publicError.Code != api.CodeForbidden {
			t.Fatalf("human actor: actor.verify code = %q (%s); want %q",
				publicError.Code, publicError.Message, api.CodeForbidden)
		}
	})

	t.Run("a service holding the reviewer role cannot carry execution approval", func(t *testing.T) {
		_, publicError := fixture.verify(t, fixture.serviceToken, fixture.resourceID, exact, "role/reviewer")
		if publicError.Code != api.CodeForbidden {
			t.Fatalf("service actor: actor.verify code = %q (%s); want %q",
				publicError.Code, publicError.Message, api.CodeForbidden)
		}
	})

	t.Run("copied result strings confer nothing", func(t *testing.T) {
		// The resolved answer string is public data, not authority. Supplying it
		// as a subject field is still a subject mismatch for a human actor and a
		// non-execution actor may never carry approval in any case.
		copied := []api.SubjectField{
			{Name: "ref", Kind: "string", Value: approvalRef},
			{Name: "expected_base", Kind: "string", Value: approvalBase},
			{Name: "candidate", Kind: "string", Value: approvalOutputValue},
		}
		for _, actor := range []struct {
			name  string
			token string
		}{
			{name: "human", token: fixture.dualToken},
			{name: "service", token: fixture.serviceToken},
		} {
			t.Run(actor.name, func(t *testing.T) {
				_, publicError := fixture.verify(t, actor.token, fixture.resourceID, copied, "role/reviewer")
				if publicError.Code != api.CodeForbidden {
					t.Fatalf("copied answer for %s actor: code = %q (%s); want %q",
						actor.name, publicError.Code, publicError.Message, api.CodeForbidden)
				}
			})
		}
	})

	t.Run("positive control still approves after the refusals", func(t *testing.T) {
		verified, publicError := fixture.verify(t, executionToken, fixture.resourceID, exact, "role/reviewer")
		if publicError.Code != "" {
			t.Fatalf("post-refusal positive control: actor.verify refused: %s (%s)",
				publicError.Code, publicError.Message)
		}
		requireApproved(t, verified)
	})

	t.Cleanup(func() {
		if executionID != "" {
			fixture.cancelExecution(t, executionID, "cleanup-"+executionID)
		}
	})
}

// TestServedVerificationRechecksLiveExecutionAuthority proves a prior approved
// verification is not cached authorization: cancelling the execution clears its
// credential and the repeat attempt is refused. A fresh approved execution is
// verified afterwards so the refusal is live-authority specific, not a broken
// service or binding.
func TestServedVerificationRechecksLiveExecutionAuthority(t *testing.T) {
	fixture := newApprovalFixture(t)

	executionID, token := fixture.startApprovedExecution(t, "cancel", "start-cancel", approvalCandidate)
	exact := approvalSubject(approvalRef, approvalBase, approvalCandidate)

	var verificationID string
	t.Run("first verification is approved", func(t *testing.T) {
		verified, publicError := fixture.verify(t, token, fixture.resourceID, exact, "role/reviewer")
		if publicError.Code != "" {
			t.Fatalf("first actor.verify refused: %s (%s)", publicError.Code, publicError.Message)
		}
		requireApproved(t, verified)
		verificationID = verified.VerificationID
	})

	t.Run("cancel through the public API clears the live credential", func(t *testing.T) {
		if verificationID == "" {
			t.Fatal("positive control produced no verification fact")
		}
		fixture.cancelExecution(t, executionID, "cancel-"+executionID)
		// The credential fields are cleared atomically with the terminal
		// transition, so the former token is refused without a fixed sleep.
		fixture.harness.Eventually(t, 30*time.Second, func() bool {
			_, publicError := fixture.harness.REST(token, "execution.inspect", api.ExecutionInspectRequest{
				ExecutionID: executionID,
			}, nil, "")
			return publicError.Code == api.CodeUnauthenticated
		})
	})

	t.Run("repeat verification is refused despite the prior approved result", func(t *testing.T) {
		_, publicError := fixture.verify(t, token, fixture.resourceID, exact, "role/reviewer")
		if publicError.Code != api.CodeUnauthenticated {
			t.Fatalf("repeat after cancel: actor.verify code = %q (%s); want %q",
				publicError.Code, publicError.Message, api.CodeUnauthenticated)
		}
	})

	t.Run("a different live execution still verifies", func(t *testing.T) {
		freshID, freshToken := fixture.startApprovedExecution(t, "fresh", "start-fresh", approvalCandidate)
		t.Cleanup(func() {
			if freshID != "" {
				fixture.cancelExecution(t, freshID, "cleanup-"+freshID)
			}
		})
		verified, publicError := fixture.verify(t, freshToken, fixture.resourceID, exact, "role/reviewer")
		if publicError.Code != "" {
			t.Fatalf("fresh execution actor.verify refused: %s (%s)", publicError.Code, publicError.Message)
		}
		requireApproved(t, verified)
		if verified.VerificationID == verificationID {
			t.Fatalf("fresh verification reused the prior verification id %q", verificationID)
		}
	})
}
