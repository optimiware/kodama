//go:build integration

package factorytest

// The protected factory golden path.
//
// One task-associated execution drives the whole scenario through public
// operations only:
//
//   - an OIDC engineer applies a stored pipeline that declares the repository,
//     target-ref and expected-base inputs, creates a task with the registered
//     project resource identity, the protected ref and the seeded base, reads
//     the open task and starts it at the observed lifecycle version;
//   - a Shell-launched container verifies KODAMA_TASK and /kodama/task.json,
//     clones through the workload relay, commits one candidate C on a granted
//     work ref and submits the declared `candidate` output;
//   - a second container reuses the retained checkout and marker, proves HEAD
//     is C, runs the committed tests and submits `candidate=C` plus
//     `test_result=pass:C`;
//   - one OIDC human holding exactly engineer+reviewer discovers the request
//     through task.inbox, inspects the execution and resolves a pass;
//   - a third container reads both settled outputs, proves B is C's ancestor
//     and pushes exactly C to refs/heads/main with an explicit
//     `--force-with-lease=refs/heads/main:B` precondition and no new commit;
//   - the test waits for `succeeded` and completed cleanup, reads the task,
//     execution and retained logs, replays the exact accepted resource.report
//     with the Git service credential and the original source-event
//     idempotency key, and requires unchanged identities and one linked event.
//
// The scenario and its evidence assertions live in
// protected_golden_helpers_test.go so the single no-substitute pin runs the
// same proof against its own fixture.

import "testing"

// TestProtectedFactoryGoldenPath proves the positive protected golden path: a
// real task, real containers, a real human approval and a real protected ref
// update, with durable Shell/Git/PostgreSQL evidence for every step.
func TestProtectedFactoryGoldenPath(t *testing.T) {
	fixture := startGitHarness(t)
	evidence := runProtectedGoldenScenario(t, fixture)
	assertProtectedGoldenEvidence(t, evidence)
}
