//go:build e2e

package smoke_test

import (
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// TestSmokeChecks proves every release-smoke helper refuses the wrong-actor,
// wrong-version, replay-created-second-execution, reused-workspace and
// missing-marker observations the plan names. It is hermetic: no installation,
// no network, no clock.
func TestSmokeChecks(t *testing.T) {
	t.Run("wrong actor", func(t *testing.T) {
		answered := time.Now().UTC()
		action := api.ExecutionAction{
			ActionID: "action-a", Index: 0, Name: "approve", Kind: "request",
			State: "succeeded", Version: 2, Decision: "pass", Actor: "oidc-operator",
			AnsweredAt: &answered,
		}
		if err := smoke.CheckDecisionActor(action, "pass", "oidc-operator"); err != nil {
			t.Fatalf("the exact decision actor was refused: %v", err)
		}
		if err := smoke.CheckDecisionActor(action, "pass", "oidc-engineer"); err == nil {
			t.Fatal("a request decision by another actor was accepted")
		}
		if err := smoke.CheckDecisionActor(action, "fail", "oidc-operator"); err == nil {
			t.Fatal("a request decision with another decision was accepted")
		}
	})

	t.Run("wrong version", func(t *testing.T) {
		if err := smoke.CheckMutationVersion(3, 3); err != nil {
			t.Fatalf("the observed version was refused: %v", err)
		}
		if err := smoke.CheckMutationVersion(2, 3); err == nil {
			t.Fatal("a mutation sent with a stale/guessed version was accepted")
		}
		if err := smoke.CheckMutationVersion(0, 3); err == nil {
			t.Fatal("a mutation with no expected version was accepted")
		}
	})

	t.Run("replay creating a second execution", func(t *testing.T) {
		first := api.TaskStartResult{ExecutionID: "execution-1", LifecycleVersion: 3}
		if err := smoke.CheckReplayConverged(first, first); err != nil {
			t.Fatalf("an identical replay was refused: %v", err)
		}
		second := api.TaskStartResult{ExecutionID: "execution-2", LifecycleVersion: 4}
		if err := smoke.CheckReplayConverged(first, second); err == nil {
			t.Fatal("a replay that returned a different execution was accepted")
		}
		if err := smoke.CheckExactHistory([]string{"execution-1"}, "execution-1"); err != nil {
			t.Fatalf("the exact history was refused: %v", err)
		}
		if err := smoke.CheckExactHistory([]string{"execution-1", "execution-2"}, "execution-1"); err == nil {
			t.Fatal("a replay-created second execution was accepted in the task history")
		}
		if err := smoke.CheckExecutionSet([]string{"execution-1", "execution-2"}, []string{"execution-2", "execution-1"}); err != nil {
			t.Fatalf("the same execution set in another order was refused: %v", err)
		}
		if err := smoke.CheckExecutionSet([]string{"execution-1"}, []string{"execution-1", "execution-2"}); err == nil {
			t.Fatal("a new execution was accepted as an unchanged set")
		}
	})

	t.Run("settlement scoping", func(t *testing.T) {
		before := api.ExecutionInspectResult{
			ExecutionID: "execution-1", State: "running", Version: 4,
			Actions: []api.ExecutionAction{
				{ActionID: "action-a", Index: 0, Name: "prepare"},
				{ActionID: "action-b", Index: 1, Name: "continue"},
			},
		}
		after := before
		after.State = "succeeded"
		after.Version = 7
		if err := smoke.CheckExecutionContinued(before, after); err != nil {
			t.Fatalf("a continued execution was refused: %v", err)
		}

		restarted := after
		restarted.ExecutionID = "execution-2"
		if err := smoke.CheckExecutionContinued(before, restarted); err == nil {
			t.Fatal("a settlement that returned another execution was accepted")
		}
		reset := after
		reset.Version = before.Version
		if err := smoke.CheckExecutionContinued(before, reset); err == nil {
			t.Fatal("a settlement that reset the execution version was accepted")
		}
		reattributed := after
		reattributed.TaskID = "task-1"
		if err := smoke.CheckExecutionContinued(before, reattributed); err == nil {
			t.Fatal("a settlement that reattributed the execution was accepted")
		}
		added := after
		added.Actions = append(append([]api.ExecutionAction(nil), after.Actions...),
			api.ExecutionAction{ActionID: "action-c", Index: 2, Name: "extra"})
		if err := smoke.CheckExecutionContinued(before, added); err == nil {
			t.Fatal("a settlement that admitted an extra action was accepted")
		}
		reordered := after
		reordered.Actions = []api.ExecutionAction{after.Actions[1], after.Actions[0]}
		if err := smoke.CheckExecutionContinued(before, reordered); err == nil {
			t.Fatal("a settlement that reordered the admitted actions was accepted")
		}

		if err := smoke.CheckScopedExecutionSet([]string{"execution-1"}, []string{"execution-1"}, "execution-1"); err != nil {
			t.Fatalf("the unchanged scoped execution set was refused: %v", err)
		}
		if err := smoke.CheckScopedExecutionSet([]string{"execution-1"}, []string{"execution-1", "execution-2"}, "execution-1"); err == nil {
			t.Fatal("a new execution in the task/pipeline scope was accepted")
		}
		if err := smoke.CheckScopedExecutionSet([]string{"execution-1"}, []string{"execution-1"}, "execution-2"); err == nil {
			t.Fatal("a scoped set without the execution under test was accepted")
		}
		if err := smoke.CheckScopedExecutionSet(nil, nil, ""); err == nil {
			t.Fatal("a scoped set with no execution identity was accepted")
		}
	})

	t.Run("reused workspace id", func(t *testing.T) {
		if err := smoke.CheckDistinctWorkspaces("workspace-a", "workspace-b"); err != nil {
			t.Fatalf("two distinct workspaces were refused: %v", err)
		}
		if err := smoke.CheckDistinctWorkspaces("workspace-a", "workspace-a"); err == nil {
			t.Fatal("a reused workspace identity was accepted")
		}
		if err := smoke.CheckDistinctWorkspaces("", "workspace-b"); err == nil {
			t.Fatal("an empty workspace identity was accepted")
		}
		if err := smoke.CheckDistinctWorkspaces("workspace a", "workspace-b"); err == nil {
			t.Fatal("an unbounded workspace identity was accepted")
		}
	})

	t.Run("missing marker", func(t *testing.T) {
		if err := smoke.CheckMarker([]byte("prefix w49-continuation suffix"), "w49-continuation", 1); err != nil {
			t.Fatalf("the single positive marker was refused: %v", err)
		}
		if err := smoke.CheckMarker([]byte("prefix only"), "w49-continuation", 1); err == nil {
			t.Fatal("a missing positive marker was accepted")
		}
		if err := smoke.CheckMarker([]byte("w49-continuation x w49-continuation"), "w49-continuation", 1); err == nil {
			t.Fatal("a duplicated continuation marker was accepted as exactly one")
		}
		if err := smoke.CheckMarker([]byte("anything"), "", 1); err == nil {
			t.Fatal("an empty marker was accepted")
		}
	})

	t.Run("fresh action ids", func(t *testing.T) {
		first := api.ExecutionInspectResult{Actions: []api.ExecutionAction{
			{ActionID: "action-a"}, {ActionID: "action-b"},
		}}
		fresh := api.ExecutionInspectResult{Actions: []api.ExecutionAction{{ActionID: "action-c"}}}
		if err := smoke.CheckFreshActionIDs(first, fresh); err != nil {
			t.Fatalf("fresh action ids were refused: %v", err)
		}
		reused := api.ExecutionInspectResult{Actions: []api.ExecutionAction{{ActionID: "action-a"}}}
		if err := smoke.CheckFreshActionIDs(first, reused); err == nil {
			t.Fatal("a reused action id was accepted")
		}
		empty := api.ExecutionInspectResult{Actions: []api.ExecutionAction{{ActionID: ""}}}
		if err := smoke.CheckFreshActionIDs(first, empty); err == nil {
			t.Fatal("an empty action id was accepted")
		}
		duplicate := api.ExecutionInspectResult{Actions: []api.ExecutionAction{
			{ActionID: "action-c"}, {ActionID: "action-c"},
		}}
		if err := smoke.CheckFreshActionIDs(first, duplicate); err == nil {
			t.Fatal("a duplicated action id inside the fresh execution was accepted")
		}
	})

	t.Run("durable answer message", func(t *testing.T) {
		messages := []api.TaskMessage{{Author: "oidc-reviewer", Body: "w49-answer"}}
		if err := smoke.CheckBodyMessage(messages, "w49-answer", "oidc-reviewer"); err != nil {
			t.Fatalf("the exact durable answer was refused: %v", err)
		}
		if err := smoke.CheckBodyMessage(messages, "another-answer", "oidc-reviewer"); err == nil {
			t.Fatal("a missing durable answer was accepted")
		}
		if err := smoke.CheckBodyMessage(messages, "w49-answer", "oidc-operator"); err == nil {
			t.Fatal("an answer by another actor was accepted")
		}
	})

	t.Run("scenario names", func(t *testing.T) {
		first := smoke.NewNames("0123456789abcdef", smoke.RequestWait)
		second := smoke.NewNames("0123456789abcdef", smoke.OperatorRetry)
		if first.Prefix() == second.Prefix() {
			t.Fatal("two scenarios share a name prefix")
		}
		if first.Key("resolve") == first.Key("start") {
			t.Fatal("two steps share an idempotency key")
		}
		if first.Marker("prepare") == first.Marker("continue") {
			t.Fatal("two markers are identical")
		}
		if !strings.HasPrefix(first.Reference(), "sha256:") || len(first.Reference()) != len("sha256:")+64 {
			t.Fatalf("durable reference %q is not an artifact digest", first.Reference())
		}
		if got := smoke.NewNames("0123456789abcdef", smoke.RequestWait); got.Prefix() != first.Prefix() {
			t.Fatal("the same run and scenario produced a different prefix")
		}
	})
}

// TestSmokeMissingInputs proves the scenario input set is the full installation
// preflight: the four personas, API/CA/project, a digest-pinned tools and
// external-harness image, the workload CA, the Git endpoint and a registered
// repository. A missing persona, pin, endpoint or repository is named, never
// silently skipped.
func TestSmokeMissingInputs(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	config := harness.Config{
		APIURL:               "https://localhost:18443",
		CAPEM:                "/tmp/kodama-ca.crt",
		ProjectID:            "tenant-local",
		Image:                "docker.io/library/buildpack-deps@" + digest,
		ExternalHarnessImage: "docker.io/library/alpine@" + digest,
		WorkloadCAFile:       "/tmp/kodama-ca.crt",
		PersonaTokenFiles: map[harness.Persona]string{
			harness.PersonaAdmin:    "/tmp/admin-token",
			harness.PersonaEngineer: "/tmp/engineer-token",
			harness.PersonaReviewer: "/tmp/reviewer-token",
			harness.PersonaOperator: "/tmp/operator-token",
		},
		Installation: harness.Installation{
			Git: harness.GitConfig{URL: "https://localhost:18445"},
			SmokeRepository: &harness.GitRepository{
				Name: "smoke/run", ResourceID: "w49-repo-run", BindingID: "w49-repo-run",
			},
		},
	}
	if missing := smoke.MissingInputs(config); !missing.Empty() {
		t.Fatalf("a complete scenario configuration was refused: %s", missing)
	}

	config.PersonaTokenFiles[harness.PersonaOperator] = ""
	missing := smoke.MissingInputs(config)
	if !strings.Contains(missing.String(), string(harness.InputOperatorTokenFile)) {
		t.Fatalf("a missing operator credential was not named: %s", missing)
	}

	config.PersonaTokenFiles[harness.PersonaOperator] = "/tmp/operator-token"
	config.Image = "docker.io/library/buildpack-deps:latest"
	missing = smoke.MissingInputs(config)
	if !strings.Contains(missing.String(), "image digest") {
		t.Fatalf("an unpinned tools image was not named: %s", missing)
	}

	config.Image = "docker.io/library/buildpack-deps@" + digest
	config.Installation.SmokeRepository = nil
	if missing := smoke.MissingInputs(config); !strings.Contains(missing.String(), "git.repositories") {
		t.Fatalf("a missing repository was not named: %s", missing)
	}
	config.Installation.SmokeRepository = &harness.GitRepository{Name: "smoke/run", ResourceID: "w49-repo-run", BindingID: "w49-repo-run"}
	config.ExternalHarnessImage = ""
	if missing := smoke.MissingInputs(config); !strings.Contains(missing.String(), "external harness image digest") {
		t.Fatalf("a missing external-harness pin was not named: %s", missing)
	}
}

// TestSmokeMissingInputsNonGit proves Q01_SMOKE_GIT=0 drops every Git
// requirement while keeping the API, CA, project, four persona credentials and
// both digest pins; the default Git mode keeps naming the Git inputs.
func TestSmokeMissingInputsNonGit(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	config := harness.Config{
		APIURL:               "https://localhost:18443",
		CAPEM:                "/tmp/kodama-ca.crt",
		ProjectID:            "tenant-a",
		Image:                "docker.io/library/buildpack-deps@" + digest,
		ExternalHarnessImage: "docker.io/library/alpine@" + digest,
		WorkloadCAFile:       "/tmp/kodama-ca.crt",
		SmokeGitDisabled:     true,
		PersonaTokenFiles: map[harness.Persona]string{
			harness.PersonaAdmin:    "/tmp/admin-token",
			harness.PersonaEngineer: "/tmp/engineer-token",
			harness.PersonaReviewer: "/tmp/reviewer-token",
			harness.PersonaOperator: "/tmp/operator-token",
		},
	}
	if missing := smoke.MissingInputs(config); !missing.Empty() {
		t.Fatalf("a complete non-Git configuration was refused: %s", missing)
	}

	config.PersonaTokenFiles[harness.PersonaReviewer] = ""
	missing := smoke.MissingInputs(config)
	if !strings.Contains(missing.String(), string(harness.InputReviewerTokenFile)) {
		t.Fatalf("a missing reviewer credential was not named in non-Git mode: %s", missing)
	}
	if strings.Contains(missing.String(), "git.") {
		t.Fatalf("non-Git mode named a Git input: %s", missing)
	}
	config.PersonaTokenFiles[harness.PersonaReviewer] = "/tmp/reviewer-token"

	config.Image = "docker.io/library/buildpack-deps:latest"
	if missing := smoke.MissingInputs(config); !strings.Contains(missing.String(), "image digest") {
		t.Fatalf("an unpinned tools image was not named in non-Git mode: %s", missing)
	}
	config.Image = "docker.io/library/buildpack-deps@" + digest

	config.ExternalHarnessImage = ""
	if missing := smoke.MissingInputs(config); !strings.Contains(missing.String(), "external harness image digest") {
		t.Fatalf("a missing external-harness pin was not named in non-Git mode: %s", missing)
	}
	config.ExternalHarnessImage = "docker.io/library/alpine@" + digest

	// The default Git mode still requires both Git inputs on the same
	// otherwise-complete configuration.
	config.SmokeGitDisabled = false
	missing = smoke.MissingInputs(config)
	if !strings.Contains(missing.String(), "git.url") || !strings.Contains(missing.String(), "git.repositories") {
		t.Fatalf("Git mode did not name the Git inputs: %s", missing)
	}
}

// TestSmokeExecutionSetDelta proves the delta names exactly the executions
// admitted meanwhile, each once, and refuses a list that lost an execution.
func TestSmokeExecutionSetDelta(t *testing.T) {
	added, err := smoke.ExecutionSetDelta(
		[]string{"execution-1", "execution-2"},
		[]string{"execution-4", "execution-2", "execution-3", "execution-1", "execution-4"})
	if err != nil || len(added) != 2 || added[0] != "execution-3" || added[1] != "execution-4" {
		t.Fatalf("ExecutionSetDelta = %v, %v; want [execution-3 execution-4]", added, err)
	}
	if added, err := smoke.ExecutionSetDelta([]string{"execution-1"}, []string{"execution-1"}); err != nil || len(added) != 0 {
		t.Fatalf("an unchanged set reported %v, %v", added, err)
	}
	if _, err := smoke.ExecutionSetDelta([]string{"execution-1", "execution-2"}, []string{"execution-2", "execution-3"}); err == nil {
		t.Fatal("a list that lost an execution was accepted")
	}
}

// TestSmokeForbiddenActionNames proves a concurrent scenario's execution
// passes while an execution carrying the scenario's own action is refused.
func TestSmokeForbiddenActionNames(t *testing.T) {
	sibling := api.ExecutionInspectResult{
		ExecutionID: "execution-3",
		Actions:     []api.ExecutionAction{{Name: "ac11-write"}, {Name: "ac11-read"}},
	}
	if err := smoke.CheckForbiddenActionNames(sibling, "ac03-once", "ac03-changed"); err != nil {
		t.Fatalf("a concurrent scenario's execution was refused: %v", err)
	}
	admitted := api.ExecutionInspectResult{
		ExecutionID: "execution-4",
		Actions:     []api.ExecutionAction{{Name: "prepare"}, {Name: "ac03-changed"}},
	}
	if err := smoke.CheckForbiddenActionNames(admitted, "ac03-once", "ac03-changed"); err == nil {
		t.Fatal("an execution carrying a forbidden action was accepted")
	}
}
