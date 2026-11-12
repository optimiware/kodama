//go:build integration

package factorytest

// Real-harness smoke test for the protected-workload candidate and output
// commands.
//
// A Shell-launched container commits a real candidate on a granted work ref
// through the workload relay, using only its mounted execution credential. It
// then submits the declared `candidate` output through the HTTPS output
// submitter: the submitter resolves the KODAMA_ACTION slug to the real action
// ID through execution.inspect and sends the current expected_version.
//
// The test observes the durable facts: the reported work-branch journal entry,
// the real ref move with the seeded base as the candidate's parent, the promoted
// action output, and the next action's materialized `/kodama/outputs/implement/
// candidate` file. The protected `main` ref stays at the seeded base.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// candidateOutputWorkRef is the granted work ref this smoke test moves.
	candidateOutputWorkRef = "refs/heads/work/protected-candidate-outputs"
	// candidateOutputReleasePath releases the consume action after the test
	// observed the materialized output.
	candidateOutputReleasePath = "/workspace/consume-release"
	// candidateOutputTimeout bounds every wait for one durable fact.
	candidateOutputTimeout = 5 * time.Minute
)

// TestFactoryCandidateWorkloadPublishesExactOutputs proves the candidate/output
// half of the protected golden path against the real factory composition.
func TestFactoryCandidateWorkloadPublishesExactOutputs(t *testing.T) {
	fixture := startGitHarness(t)
	image := fixture.git.EnsureWorkloadImage(t)

	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded project main ref is missing")
	}

	commands, err := fixture.git.WorkloadCandidateCommands(t, factory.WorkloadCandidateSpec{
		WorkRef:      candidateOutputWorkRef,
		ExpectedBase: base,
	})
	if err != nil {
		t.Fatalf("author candidate workload commands: %v", err)
	}
	// The consume action keeps the execution alive while the test reads the
	// materialized output and proves the next action sees the earlier output.
	commands = append(commands, `printf 'candidate-output-submitted\n'`)
	consume := []string{
		`test -s /kodama/outputs/implement/candidate`,
		`printf '%s' "$(cat /kodama/outputs/implement/candidate)" > /workspace/consume-candidate`,
		`while [ ! -e ` + candidateOutputReleasePath + ` ]; do sleep 1; done`,
	}

	applied := fixture.applyPipeline("protected-candidate-outputs", marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{
			{
				Name:     "implement",
				Image:    image.RunnableReference,
				Timeout:  "20m",
				Commands: commands,
				Outputs:  []string{"candidate"},
			},
			{
				Name:     "consume",
				Image:    image.RunnableReference,
				Timeout:  "20m",
				Commands: consume,
			},
		},
	}))
	started := fixture.startPipeline(applied.PipelineID, "start-protected-candidate-outputs")
	executionID := started.ExecutionID

	// Wait for the reported journal entry of the granted work ref. The managed
	// hooks journal only after the served Shell verified the execution actor.
	var entry git.JournalEntry
	fixture.git.WaitFor(t,
		"reported journal entry ref="+candidateOutputWorkRef+" execution="+executionID,
		candidateOutputTimeout, func() bool {
			for _, candidate := range fixture.git.JournalEntries(t) {
				if candidate.Ref == candidateOutputWorkRef && candidate.State == git.JournalReported {
					entry = candidate
					return true
				}
			}
			return false
		})
	candidate := entry.Candidate
	if candidate == "" || candidate == base {
		t.Fatalf("work journal candidate = %q; want a commit different from the base %q", candidate, base)
	}
	if entry.Repository != factory.GitProjectRepository || entry.ResourceID != fixture.git.ProjectResourceID {
		t.Fatalf("work journal entry = %+v; want the registered project repository and resource", entry)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, candidateOutputWorkRef); remote != candidate {
		t.Fatalf("work ref = %q; want the reported candidate %q", remote, candidate)
	}
	if parents := fixture.git.CommitParents(t, factory.GitProjectRepository, candidate); len(parents) != 1 || parents[0] != base {
		t.Fatalf("candidate %s parents = %v; want exactly the seeded base %s", candidate, parents, base)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != base {
		t.Fatalf("protected main = %q; want the unchanged base %q", main, base)
	}

	// The submitter must have staged the declared output on this exact action;
	// the next action runs only after that output was promoted and materialized.
	waitProtectedActionRunning(t, fixture, executionID, "consume")

	var materialized string
	fixture.git.WaitFor(t,
		"materialized output /kodama/outputs/implement/candidate execution="+executionID,
		candidateOutputTimeout, func() bool {
			paths, err := fixture.OutputMaterializations("implement")
			if err != nil || len(paths) == 0 {
				return false
			}
			path := filepath.Join(paths[0], "candidate")
			data, err := os.ReadFile(path)
			if err != nil {
				return false
			}
			materialized = strings.TrimSpace(string(data))
			return materialized == candidate
		})
	if materialized != candidate {
		t.Fatalf("materialized implement candidate = %q; want %q", materialized, candidate)
	}

	// The next action independently read the same materialized bytes from its
	// own workload. Read its retained workspace file through podman exec while
	// the action is still held.
	var container string
	fixture.Eventually(t, time.Minute, func() bool {
		found, err := fixture.RunningExecutionContainer(t.Context(), executionID)
		if err != nil || found == "" {
			return false
		}
		container = found
		return true
	})
	var consumed string
	fixture.git.WaitFor(t,
		"consume action observed the materialized candidate",
		time.Minute, func() bool {
			value, err := fixture.ReadContainerFile(t.Context(), container, "/workspace/consume-candidate")
			if err != nil {
				return false
			}
			consumed = strings.TrimSpace(value)
			return consumed == candidate
		})

	// Release the held consume action and wait for a successful terminal
	// execution with completed cleanup so the retained logs are readable.
	if err := fixture.TouchContainerFile(t.Context(), container, candidateOutputReleasePath); err != nil {
		t.Fatalf("release the consume action: %v", err)
	}
	finished := fixture.waitExecutionTerminal(t, executionID)
	if finished.State != "succeeded" {
		fixture.dumpExecutionLogs(t, finished)
		t.Fatalf("candidate/output execution state = %q; want succeeded", finished.State)
	}

	// The promoted action output names the same commit the work ref carries.
	implement := actionByName(t, finished, "implement")
	if implement.Outputs["candidate"] != candidate {
		t.Fatalf("implement action outputs = %v; want candidate=%s", implement.Outputs, candidate)
	}
	logs := fixture.executionLogs(t, finished)
	if !strings.Contains(logs, "candidate-output-submitted") {
		t.Fatalf("candidate workload logs are missing the positive control:\n%s", logs)
	}

	t.Logf("candidate/output execution=%s action=%s journal=%s candidate=%s base=%s",
		executionID, implement.ActionID, entry.SourceEventID, candidate, base)
}

// waitProtectedActionRunning waits for one named action to run in a running
// execution. A terminal execution fails the test immediately with its retained
// logs instead of waiting out the whole budget.
func waitProtectedActionRunning(t *testing.T, fixture *gitHarness, executionID, name string) api.ExecutionInspectResult {
	t.Helper()
	deadline := time.Now().Add(candidateOutputTimeout)
	for time.Now().Before(deadline) {
		current, ok := fixture.inspectExecution(t, executionID)
		if ok {
			switch current.State {
			case "failed", "cancelled":
				fixture.dumpExecutionLogs(t, current)
				t.Fatalf("execution %s reached %q before action %q ran", executionID, current.State, name)
			}
			if current.State == "running" {
				for _, action := range current.Actions {
					if action.Name == name && action.State == "running" {
						return current
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	summary := ""
	if current, ok := fixture.inspectExecution(t, executionID); ok {
		summary = commitSummary(current)
	}
	t.Fatalf("action %q did not run within %s (%s)\n%s",
		name, candidateOutputTimeout, summary, fixture.git.DiagnosticSummary())
	return api.ExecutionInspectResult{}
}
