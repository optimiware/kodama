//go:build integration

package factorytest

// Container consumption: a Shell-launched workload image clones and
// pushes through the workload relay with the execution credential mounted by
// the product. The served Shell remains the only authority: the verification
// rows identify that execution and its current action, the journal entry
// reports the accepted update, and the linked resource.verified event carries
// the execution correlation. No credential may persist in workload logs, the
// workload's repository configuration, the admitted pipeline or host artifacts.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// gitWorkloadActionName is the one container action that performs the
	// workload clone and push.
	gitWorkloadActionName = "git"
	// gitWorkloadReleasePath releases the held workload after the test observed
	// the durable authority and ref facts. The execution stays running until
	// then, so its mounted credential is live for the scans.
	gitWorkloadReleasePath = "/workspace/git-release"
	// gitWorkloadPipelineTimeout keeps the real PostgreSQL statement clock far
	// from the action deadline while the workload is held open.
	gitWorkloadPipelineTimeout = "20m"
	// gitWorkloadTimeout bounds every wait for one durable fact of this flow.
	gitWorkloadTimeout = 5 * time.Minute
)

// TestFactoryWithGitServiceWorkloadClonePush proves container consumption of
// the factory Git service: the workload image, published by digest to the real
// local registry and run by the Shell, materializes the public CA inside
// /workspace, clones and pushes through the workload relay using only its
// mounted execution token, and the served Shell records the exact execution and
// running action.
func TestFactoryWithGitServiceWorkloadClonePush(t *testing.T) {
	fixture := startGitHarness(t)
	image := fixture.git.EnsureWorkloadImage(t)
	if image.RunnableReference == "" || !strings.Contains(image.RunnableReference, "@sha256:") {
		t.Fatalf("workload image reference %q is not digest-pinned", image.RunnableReference)
	}
	if fixture.git.WorkloadURL == "" {
		t.Fatalf("the Git fixture exposed no workload URL")
	}
	if !strings.HasPrefix(fixture.git.WorkloadURL, "https://host.containers.internal:") {
		t.Fatalf("workload Git URL = %q; want the host workload alias through the relay", fixture.git.WorkloadURL)
	}

	commands, err := fixture.git.WorkloadClonePushCommands(t, factory.GitWorkloadRef)
	if err != nil {
		t.Fatalf("author workload Git commands: %v", err)
	}
	commands = append(commands,
		`printf 'workload push complete\n'`,
		`while [ ! -e `+gitWorkloadReleasePath+` ]; do sleep 1; done`,
	)
	applied := fixture.applyPipeline("git-workload", marshalPipeline(t, pipelineDoc{
		Actions: []pipelineActionDoc{{
			Name:     gitWorkloadActionName,
			Image:    image.RunnableReference,
			Timeout:  gitWorkloadPipelineTimeout,
			Commands: commands,
		}},
	}))
	started := fixture.startPipeline(applied.PipelineID, "start-git-workload")
	executionID := started.ExecutionID

	// The action runs while the push executes. Its identity is the one the
	// Shell resolves from the mounted execution credential, so the rows below
	// must name exactly this execution and action.
	execution := fixture.waitActionRunning(t, executionID, gitWorkloadActionName)
	action := actionByName(t, execution, gitWorkloadActionName)

	// The workload authenticates with the product-mounted execution credential.
	// Read the same materialized file while the execution is running; it is
	// only ever used as a bearer value for the leak scans.
	var executionToken string
	fixture.git.WaitFor(t,
		"execution credential for execution="+executionID+" action="+gitWorkloadActionName,
		gitWorkloadTimeout, func() bool {
			value, err := fixture.ExecutionCredential(t.Context(), executionID, gitWorkloadActionName)
			if err != nil {
				return false
			}
			executionToken = strings.TrimSpace(value)
			return executionToken != ""
		})

	// The push succeeds only through the managed hooks, so the reported journal
	// entry is the accepted candidate the workload committed.
	var entry git.JournalEntry
	fixture.git.WaitFor(t,
		"reported journal entry ref="+factory.GitWorkloadRef,
		gitWorkloadTimeout, func() bool {
			for _, candidate := range fixture.git.JournalEntries(t) {
				if candidate.Ref == factory.GitWorkloadRef && candidate.State == git.JournalReported {
					entry = candidate
					return true
				}
			}
			return false
		})
	if entry.Repository != factory.GitProjectRepository || entry.ResourceID != fixture.git.ProjectResourceID {
		t.Fatalf("workload journal entry = %+v", entry)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, factory.GitWorkloadRef); remote != entry.Candidate {
		t.Fatalf("workload ref = %q; want the reported candidate %q", remote, entry.Candidate)
	}

	// The read verification belongs to this execution and its current action.
	read := fixture.waitExecutionVerification(t, executionID, action.ActionID, "git.read")
	if read.Result != "approved" || read.ResourceID != fixture.git.ProjectResourceID {
		t.Fatalf("workload git.read verification = %+v", read)
	}
	var readSubject api.Subject
	if err := json.Unmarshal([]byte(read.Subject), &readSubject); err != nil {
		t.Fatalf("decode workload git.read subject %s: %v", read.Subject, err)
	}
	if len(readSubject.Fields) != 0 {
		t.Fatalf("workload git.read verification carries subject %s; want none", read.Subject)
	}

	// The ref update is the reported journal entry's own verification.
	update := fixture.verificationByID(t, entry.VerificationID)
	if update.Operation != "git.ref.update" || update.Result != "approved" || update.Approved {
		t.Fatalf("workload git.ref.update verification = %+v", update)
	}
	if update.ExecutionID != executionID || update.ActionID != action.ActionID {
		t.Fatalf("workload git.ref.update identity = execution %q action %q; want execution %q action %q",
			update.ExecutionID, update.ActionID, executionID, action.ActionID)
	}
	if update.ResourceID != fixture.git.ProjectResourceID || update.BindingID != fixture.git.ProjectBindingID {
		t.Fatalf("workload git.ref.update resource/binding = %+v", update)
	}
	var updateSubject api.Subject
	if err := json.Unmarshal([]byte(update.Subject), &updateSubject); err != nil {
		t.Fatalf("decode workload git.ref.update subject %s: %v", update.Subject, err)
	}
	gotSubject := make(map[string]string, len(updateSubject.Fields))
	for _, field := range updateSubject.Fields {
		gotSubject[field.Name] = field.Value
	}
	wantSubject := map[string]string{
		"operation":     "git.ref.update",
		"resource":      fixture.git.ProjectResourceID,
		"ref":           factory.GitWorkloadRef,
		"expected_base": "",
		"candidate":     entry.Candidate,
	}
	if len(gotSubject) != len(wantSubject) {
		t.Fatalf("workload subject %s has %d fields; want %d", update.Subject, len(gotSubject), len(wantSubject))
	}
	for name, want := range wantSubject {
		if got, ok := gotSubject[name]; !ok || got != want {
			t.Fatalf("workload subject %s field %q = %q (present %t); want %q", update.Subject, name, got, ok, want)
		}
	}

	event := fixture.waitReportEvent(t, entry.VerificationID)
	if event.Type != "resource.verified" || event.ReportType != "git.ref.updated" {
		t.Fatalf("workload report event = %+v", event)
	}
	if event.CausationID != entry.VerificationID ||
		event.SubjectID != fixture.git.ProjectResourceID ||
		event.CorrelationID != executionID ||
		event.SourceKind != "service" {
		t.Fatalf("workload report event linkage = %+v", event)
	}

	// Release the held action and wait for the workload to finish. The action
	// itself fails if the protected main ref accepts the execution credential
	// or if the workspace scan finds the token or its Basic encoding, so a
	// non-succeeded execution is an authority or credential defect.
	var container string
	fixture.Eventually(t, time.Minute, func() bool {
		found, err := fixture.RunningExecutionContainer(t.Context(), executionID)
		if err != nil || found == "" {
			return false
		}
		container = found
		return true
	})
	if err := fixture.TouchContainerFile(t.Context(), container, gitWorkloadReleasePath); err != nil {
		t.Fatalf("release the held workload: %v", err)
	}
	finished := fixture.waitExecutionTerminal(t, executionID)
	if finished.State != "succeeded" {
		fixture.dumpExecutionLogs(t, finished)
		t.Fatalf("workload execution state = %q; want succeeded", finished.State)
	}

	// The workload refused the protected main ref with its execution credential
	// and authored its own whole-workspace scan, printing the positive control
	// only after both guards held. Collect every retained chunk, prove the
	// markers ran, and scan the same bytes for the raw token and its Basic
	// encoding.
	logs := fixture.executionLogs(t, finished)
	for _, marker := range []string{"protected-ref-refused", "repository-config-clean", "workload push complete"} {
		if !strings.Contains(logs, marker) {
			t.Fatalf("workload logs are missing %q:\n%s", marker, logs)
		}
	}
	for _, secret := range []string{executionToken, "Basic " + basicValue(executionToken)} {
		if strings.Contains(logs, secret) {
			t.Fatalf("the execution credential leaked into the workload's retained logs")
		}
	}

	// The admitted pipeline bytes never carry the credential.
	yamlBytes, _, err := fixture.ExecutionRow(t.Context(), executionID)
	if err != nil {
		t.Fatalf("read admitted workload pipeline: %v", err)
	}
	for _, secret := range []string{executionToken, "Basic " + basicValue(executionToken)} {
		if strings.Contains(string(yamlBytes), secret) {
			t.Fatalf("the execution credential persisted in the admitted pipeline YAML")
		}
	}

	// Host artifacts: repository storage, the receive journal and the
	// composition log. The mounted credential file itself is the one intended
	// copy and is not scanned.
	serviceToken, err := fixture.git.ServiceToken(t)
	if err != nil {
		t.Fatalf("read the mounted service credential: %v", err)
	}
	leaks := fixture.git.ScanCredentialLeaks(t,
		executionToken,
		"Basic "+basicValue(executionToken),
		serviceToken,
		"Basic "+basicValue(serviceToken))
	if len(leaks) != 0 {
		t.Fatalf("credentials leaked into host artifacts %v", leaks)
	}

	t.Logf("workload execution=%s action=%s candidate=%s",
		executionID, action.ActionID, entry.Candidate)
	t.Logf("workload verifications read=%s update=%s event=%s",
		read.ID, update.ID, event.ID)
	t.Logf("workload image=%s base=%s git=%s",
		image.RunnableReference, image.BaseReference, image.GitVersion)
}

// waitActionRunning waits for one named action to run and returns the current
// execution observation.
func (fixture *gitHarness) waitActionRunning(t *testing.T, executionID, name string) api.ExecutionInspectResult {
	t.Helper()
	var found api.ExecutionInspectResult
	fixture.git.WaitFor(t,
		"action running execution="+executionID+" action="+name,
		gitWorkloadTimeout, func() bool {
			execution, ok := fixture.inspectExecution(t, executionID)
			if !ok || execution.State != "running" {
				return false
			}
			for _, action := range execution.Actions {
				if action.Name == name && action.State == "running" {
					found = execution
					return true
				}
			}
			return false
		})
	return found
}

// waitExecutionVerification waits for one approved verification of the exact
// execution and its currently running action.
func (fixture *gitHarness) waitExecutionVerification(t *testing.T, executionID, actionID, operation string) factory.VerificationRow {
	t.Helper()
	var found factory.VerificationRow
	fixture.git.WaitFor(t,
		"verification operation="+operation+" execution="+executionID+" action="+actionID,
		gitWorkloadTimeout, func() bool {
			rows, err := fixture.git.Verifications(t.Context())
			if err != nil {
				return false
			}
			for _, row := range rows {
				if row.Operation == operation &&
					row.ExecutionID == executionID &&
					row.ActionID == actionID &&
					row.Result == "approved" {
					found = row
					return true
				}
			}
			return false
		})
	return found
}

// waitExecutionTerminal waits for a terminal execution observation whose
// retained-log collection and workload cleanup have completed. Logs are
// appended before the container is removed, so a terminal state alone is not
// enough evidence that the retained chunks are readable.
func (fixture *gitHarness) waitExecutionTerminal(t *testing.T, executionID string) api.ExecutionInspectResult {
	t.Helper()
	var found api.ExecutionInspectResult
	fixture.git.WaitFor(t,
		"terminal execution="+executionID+" cleanup=complete",
		gitWorkloadTimeout, func() bool {
			execution, ok := fixture.inspectExecution(t, executionID)
			if !ok {
				return false
			}
			switch execution.State {
			case "succeeded", "failed", "cancelled":
			default:
				return false
			}
			// A cleanup retry waits for the factory clock, which is frozen for
			// the duration of a test. Advance it only while a retry is pending,
			// exactly as the harness notes require.
			if execution.Cleanup.State == "pending" && execution.Cleanup.Attempts > 0 {
				fixture.Clock.Advance(time.Second)
				return false
			}
			if execution.Cleanup.State != "complete" {
				return false
			}
			found = execution
			return true
		})
	return found
}

// inspectExecution reads one execution through the project-scoped credential.
// A refused read reports false; the poll then continues.
func (fixture *gitHarness) inspectExecution(t *testing.T, executionID string) (api.ExecutionInspectResult, bool) {
	t.Helper()
	result, publicError := fixture.REST(fixture.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: executionID,
	}, nil, "")
	if publicError.Code != "" {
		return api.ExecutionInspectResult{}, false
	}
	var inspected api.ExecutionInspectResult
	fixture.Decode(result, &inspected)
	return inspected, true
}

// executionLogs concatenates every retained stdout/stderr chunk of one
// execution in action and stream order. It never fails the test: a missing
// chunk is returned as absent evidence.
func (fixture *gitHarness) executionLogs(t *testing.T, execution api.ExecutionInspectResult) string {
	t.Helper()
	var builder strings.Builder
	for _, action := range execution.Actions {
		for _, stream := range []string{"stdout", "stderr"} {
			result, publicError := fixture.REST(fixture.ProjectToken, "execution.logs.read", api.ExecutionLogsReadRequest{
				ExecutionID: execution.ExecutionID,
				ActionID:    action.ActionID,
				Stream:      stream,
			}, nil, "")
			if publicError.Code != "" {
				continue
			}
			var read api.ExecutionLogsReadResult
			fixture.Decode(result, &read)
			for _, chunk := range read.Chunks {
				builder.WriteString(string(chunk.Data))
			}
		}
	}
	return builder.String()
}

// dumpExecutionLogs prints the retained logs of a failed workload as
// diagnostics. The logs are workload data, so they are sanitized like the
// composition log.
func (fixture *gitHarness) dumpExecutionLogs(t *testing.T, execution api.ExecutionInspectResult) {
	t.Helper()
	logs := fixture.executionLogs(t, execution)
	if logs == "" {
		return
	}
	t.Logf("execution %s logs:\n%s", execution.ExecutionID, factory.SanitizeDiagnostic(logs))
}
