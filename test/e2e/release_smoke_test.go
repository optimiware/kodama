//go:build e2e

package e2e

import (
	_ "embed"
	"testing"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// The release-smoke scripts are ordinary shell programs run by the pinned
// tools image. They are test data, never product code.
var (
	//go:embed testdata/smoke/request-wait-prepare.sh
	requestWaitPrepareScript string
	//go:embed testdata/smoke/request-wait-continue.sh
	requestWaitContinueScript string
	//go:embed testdata/smoke/operator-retry-work.sh
	operatorRetryWorkScript string
	//go:embed testdata/smoke/conversation-handoff-live.sh
	conversationHandoffLiveScript string
	//go:embed testdata/smoke/git-prelude.sh
	smokeGitPrelude string
	//go:embed testdata/smoke/protected-prepare.sh
	protectedPrepareScript string
	//go:embed testdata/smoke/protected-implement.sh
	protectedImplementScript string
	//go:embed testdata/smoke/protected-test.sh
	protectedTestScript string
	//go:embed testdata/smoke/protected-integrate.sh
	protectedIntegrateScript string
	//go:embed testdata/smoke/protected-seed-integrate.sh
	protectedSeedIntegrateScript string
	//go:embed testdata/smoke/human-push-execution.sh
	humanPushExecutionScript string
	//go:embed testdata/smoke/human-push-action.sh
	humanPushActionScript string
	//go:embed testdata/smoke/git-read-control.sh
	gitReadControlScript string
	//go:embed testdata/smoke/delivery-commit.sh
	deliveryCommitScript string
	//go:embed testdata/smoke/delivery-test.sh
	deliveryTestScript string
)

// TestReleaseSmoke is the installation-neutral release smoke. Its five
// subtests use only the public API and the served Git service against an
// installed stack:
//
//   - RequestWait (S1): a planned role/operator request answered through the
//     operator inbox and execution.request.resolve; the same execution and
//     workspace continue, exactly one continuation marker appears, and an
//     identical resolve replay converges.
//   - OperatorRetry (S4): a deliberately failing first execution, an explicit
//     engineer task.comment repair marker, and an operator-only task.start
//     retry discovered through the failed_task inbox; the replay converges on
//     one fresh execution with a fresh workspace and action identities.
//   - ConversationHandoff (S3): a live task action, an engineer task.ask with
//     an ordinary durable reference the product accepts without Git, the
//     reviewer's inbox reply, and a distinct fresh execution that reads the
//     answer from /kodama/task.json and not the cancelled attempt's scratch.
//   - ProtectedTask (S2): one task, one execution and one workspace run the
//     external-harness fixture, commit and test a candidate on a granted work
//     ref, submit declared outputs, wait for the reviewer's inbox decision and
//     integrate the exact reviewed candidate to main; the genuine verified
//     git.ref.updated report names the Git service, the execution and the
//     verification.
//   - HumanPush (S5): an engineer-role starter service binds an on.push
//     pipeline; an execution-originated push is reported with the execution as
//     correlation and starts nothing, while the host engineer's human push is
//     reported with a verification causation and no correlation and starts
//     exactly one execution that proves the human event's inputs.
//
// Required installation mode turns every missing input into a failure; an
// ordinary unconfigured invocation skips with the missing inputs named.
func TestReleaseSmoke(t *testing.T) {
	preflight := startLive(t)
	if missing := smoke.MissingInputs(preflight.config); !missing.Empty() {
		if preflight.config.RequireInstallation() {
			t.Fatalf("required release-smoke input(s) are missing: %s", missing)
		}
		t.Skipf("unconfigured release-smoke input(s): %s", missing)
	}
	for _, persona := range harness.Personas {
		preflight.smokeToken(t, persona)
	}

	// The parent only groups the scenarios; each one takes its own slot.
	e2eGate.Parallel(t)

	t.Run("RequestWait", func(t *testing.T) {
		e2eParallel(t)
		instance := startLive(t)
		testReleaseSmokeRequestWait(t, instance, smoke.NewNames(instance.runID, smoke.RequestWait), "TestReleaseSmoke-RequestWait", true)
	})
	t.Run("OperatorRetry", func(t *testing.T) {
		e2eParallel(t)
		instance := startLive(t)
		testReleaseSmokeOperatorRetry(t, instance, smoke.NewNames(instance.runID, smoke.OperatorRetry), "TestReleaseSmoke-OperatorRetry", true)
	})
	t.Run("ConversationHandoff", func(t *testing.T) {
		e2eParallel(t)
		instance := startLive(t)
		testReleaseSmokeConversationHandoff(t, instance, smoke.NewNames(instance.runID, smoke.ConversationHandoff), "TestReleaseSmoke-ConversationHandoff", true)
	})
	t.Run("ProtectedTask", func(t *testing.T) {
		if !preflight.config.GitSmokeEnabled() {
			t.Skipf("Git scenarios are disabled: %s=0 (set %s=1 to run ProtectedTask)", harness.InputSmokeGit, harness.InputSmokeGit)
		}
		e2eParallel(t)
		instance, repository := requireProtectedRepository(t, "ProtectedTask", gitSlotProtectedTask)
		testReleaseSmokeProtectedTask(t, instance, repository, smoke.NewNames(instance.runID, smoke.ProtectedTask), "TestReleaseSmoke-ProtectedTask", true)
	})
	t.Run("HumanPush", func(t *testing.T) {
		if !preflight.config.GitSmokeEnabled() {
			t.Skipf("Git scenarios are disabled: %s=0 (set %s=1 to run HumanPush)", harness.InputSmokeGit, harness.InputSmokeGit)
		}
		e2eParallel(t)
		instance := startLive(t)
		testReleaseSmokeHumanPush(t, instance, smoke.NewNames(instance.runID, smoke.HumanPush), "TestReleaseSmoke-HumanPush", true)
	})
}
