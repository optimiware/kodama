//go:build integration

package factorytest

// Authority controls and wait revocation on the protected first-factory path.
//
// The four tests below are the named negative/control assertions on top of the
// protected golden path. Each starts its own real fixture and drives the same
// checkpoint-oriented companion scenario (protected_edge_helpers_test.go):
// implement -> test -> [pre-review checkpoint] -> review -> integrate.
//
// The refusals are the served facts, never a substituted verifier:
//
//   - TestProtectedFactoryPushBeforeReviewIsRefused: with the test container
//     still live and before the review action activates, the exact candidate
//     pushed to refs/heads/main is refused by the managed hook for missing
//     approval; the ref does not move, no accepted/reported journal entry or
//     verified event is produced, the durable refused git.ref.update row names
//     this execution/action/resource and requests role/reviewer, and the same
//     live credential still pushes the granted work ref. After the review pass
//     the same candidate integrates successfully.
//   - TestProtectedFactoryResolveRequiresCurrentHumanRole: an engineer without
//     reviewer, an admin without reviewer, a reviewer-role service and the
//     execution principal cannot answer the human role/reviewer request. The
//     unanswered action, the pending successor and the protected ref are
//     preserved; the reviewer still resolves afterwards.
//   - TestProtectedFactoryReviewerRevokedDuringWait: the reviewer discovers the
//     request, its membership is revoked at the observed version, the same OIDC
//     token is refused on the very next resolve, and a public regrant restores
//     the authority so the reviewer resolves successfully.
//   - TestProtectedFactoryWaitRevokesAndRefreshesCredential: request entry
//     leaves no live workload and clears both credential columns; the captured
//     pre-wait token is refused by Shell and Git; after the pass a different
//     mounted token runs the same execution/workspace, the old token is still
//     refused, and the new token integrates the exact candidate.

import "testing"

// TestProtectedFactoryPushBeforeReviewIsRefused proves a live execution with no
// review approval cannot move the protected ref, and the refusal is missing
// approval rather than a broken credential: the same live credential still
// pushes the granted work ref, and the same candidate lands after the review.
func TestProtectedFactoryPushBeforeReviewIsRefused(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedEdgeScenario(t, fixture, protectedEdgeOptions{holdBeforeReview: true})
	assertProtectedEdgePreReviewRefusal(t, evidence)
	assertProtectedEdgeFlow(t, evidence)
}

// TestProtectedFactoryResolveRequiresCurrentHumanRole proves resolution is
// answered only by the currently assigned human role, not by any other human
// role, a reviewer-role service or the execution principal.
func TestProtectedFactoryResolveRequiresCurrentHumanRole(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedEdgeScenario(t, fixture, protectedEdgeOptions{refuseResolveMatrix: true})
	assertProtectedEdgeResolveRefusals(t, evidence)
	assertProtectedEdgeFlow(t, evidence)
}

// TestProtectedFactoryReviewerRevokedDuringWait proves the reviewer membership
// is re-read on the next request: discovery succeeds, revocation refuses the
// same token, and a public regrant restores the exact authority.
func TestProtectedFactoryReviewerRevokedDuringWait(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedEdgeScenario(t, fixture, protectedEdgeOptions{revokeReviewer: true})
	assertProtectedEdgeRevocation(t, evidence)
	assertProtectedEdgeFlow(t, evidence)
}

// TestProtectedFactoryWaitRevokesAndRefreshesCredential proves request entry
// revokes the live execution credential atomically and the post-pass container
// receives a different token in the same execution and workspace.
func TestProtectedFactoryWaitRevokesAndRefreshesCredential(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedEdgeScenario(t, fixture, protectedEdgeOptions{credentialWait: true})
	assertProtectedEdgeCredentialWait(t, evidence)
	assertProtectedEdgeFlow(t, evidence)
}
