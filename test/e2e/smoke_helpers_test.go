//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	maps0 "maps"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// This file holds the public-API-only scenario plumbing shared by the three
// non-Git release scenarios. The Git scenarios reuse the same helpers for the
// repository seed, protected task and human push. Nothing here reads the
// database, the journal, the runtime, the filesystem workspace or a factory
// fixture.

// smokeToken returns one configured persona credential.
func (instance *live) smokeToken(t *testing.T, persona harness.Persona) harness.Secret {
	t.Helper()
	token, ok := instance.tokens[persona]
	if !ok || !token.IsSet() {
		t.Fatalf("%s persona credential is not configured", persona)
	}
	return token
}

// smokeCall invokes one public operation with a persona token. Every failure
// goes through the shared classifier: a 401 names the expired OAuthSonas token
// (installed tokens live about 899 seconds) and a 403 names the missing role.
func (instance *live) smokeCall(ctx context.Context, t *testing.T, token harness.Secret, method api.Method, operation string, input any, key string, expected *uint64) api.Result {
	t.Helper()
	result, err := instance.invoke(ctx, token, method, operation, input, key, expected)
	if err != nil {
		classified, _ := smoke.Classify(operation, err)
		t.Fatalf("%v", classified)
	}
	return result
}

// smokeInspectResult reads one execution view for a poll callback.
func (instance *live) smokeInspectResult(ctx context.Context, token harness.Secret, executionID string) (api.ExecutionInspectResult, error) {
	result, err := instance.invoke(ctx, token, api.MethodGet, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: executionID}, "", nil)
	if err != nil {
		classified, _ := smoke.Classify("execution.inspect", err)
		return api.ExecutionInspectResult{}, classified
	}
	var view api.ExecutionInspectResult
	if err := decodeResultBody(result, &view); err != nil {
		return api.ExecutionInspectResult{}, fmt.Errorf("decode execution.inspect: %w", err)
	}
	return view, nil
}

// decodeResultBody decodes a public result body without failing a test.
func decodeResultBody(result api.Result, value any) error {
	return json.Unmarshal(result.Body, value)
}

// smokeInspect reads one execution view as the given persona.
func (instance *live) smokeInspect(ctx context.Context, t *testing.T, token harness.Secret, executionID string) api.ExecutionInspectResult {
	t.Helper()
	result := instance.smokeCall(ctx, t, token, api.MethodGet, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: executionID}, "", nil)
	return decodeResult[api.ExecutionInspectResult](t, result)
}

// smokeStableInspect returns an execution view that did not change across two
// consecutive reads. Cleanup progress commits separately from the execution row,
// so a baseline captured at the first terminal observation is not quiescent.
func (instance *live) smokeStableInspect(ctx context.Context, t *testing.T, token harness.Secret, executionID string) api.ExecutionInspectResult {
	t.Helper()
	previous := instance.smokeInspect(ctx, t, token, executionID)
	for range 20 {
		time.Sleep(100 * time.Millisecond)
		current := instance.smokeInspect(ctx, t, token, executionID)
		if current.Version == previous.Version && current.Cleanup.State == previous.Cleanup.State {
			return current
		}
		previous = current
	}
	return previous
}

// smokeWaitCompleteResult polls until the execution is terminal and its
// cleanup record is complete. Every fetch failure is classified, so a 401
// aborts immediately naming the expired persona token.
func (instance *live) smokeWaitCompleteResult(ctx context.Context, token harness.Secret, executionID string, timeout time.Duration) (api.ExecutionInspectResult, error) {
	if timeout <= 0 {
		timeout = 6 * time.Minute
	}
	final, _, err := smoke.Wait(ctx, 250*time.Millisecond, timeout, "execution.inspect", func(ctx context.Context) (api.ExecutionInspectResult, error) {
		return instance.smokeInspectResult(ctx, token, executionID)
	}, func(result api.ExecutionInspectResult) bool {
		status, interpretErr := harness.Interpret(result)
		return interpretErr == nil && status.Terminal && status.CleanupComplete
	})
	return final, err
}

// smokeWaitComplete polls an execution to terminal cleanup and fails with the
// last observed view on timeout.
func (instance *live) smokeWaitComplete(ctx context.Context, t *testing.T, token harness.Secret, executionID string, timeout time.Duration) api.ExecutionInspectResult {
	t.Helper()
	final, err := instance.smokeWaitCompleteResult(ctx, token, executionID, timeout)
	if err != nil {
		last, _ := json.Marshal(final)
		t.Fatalf("execution %s did not reach terminal cleanup within %s: %v; last=%s", executionID, timeout, err, last)
	}
	if _, err := harness.Interpret(final); err != nil {
		t.Fatalf("execution %s served an invalid terminal view: %v", executionID, err)
	}
	return final
}

// smokeWaitActionRunning polls until the named action is the running action of
// a running execution.
func (instance *live) smokeWaitActionRunning(ctx context.Context, t *testing.T, token harness.Secret, executionID, actionName string, timeout time.Duration) api.ExecutionInspectResult {
	t.Helper()
	final, _, err := smoke.Wait(ctx, 200*time.Millisecond, timeout, "execution.inspect", func(ctx context.Context) (api.ExecutionInspectResult, error) {
		return instance.smokeInspectResult(ctx, token, executionID)
	}, func(result api.ExecutionInspectResult) bool {
		if result.State != "running" {
			return false
		}
		for _, action := range result.Actions {
			if action.Name == actionName {
				return action.State == "running"
			}
		}
		return false
	})
	if err != nil {
		t.Fatalf("execution %s never showed action %q running: %v", executionID, actionName, err)
	}
	return final
}

// smokeReadLogsResult reads and reassembles one action stream, returning a
// classified error instead of failing inside a poll.
func (instance *live) smokeReadLogsResult(ctx context.Context, token harness.Secret, executionID, actionID, stream string) ([]byte, error) {
	var chunks []api.LogChunk
	cursor := ""
	for range 64 {
		request := api.ExecutionLogsReadRequest{
			ExecutionID: executionID,
			ActionID:    actionID,
			Stream:      stream,
		}
		if cursor != "" {
			request.Page = &api.Page{Cursor: cursor}
		}
		result, err := instance.invoke(ctx, token, api.MethodGet, "execution.logs.read", request, "", nil)
		if err != nil {
			classified, _ := smoke.Classify("execution.logs.read", err)
			return nil, classified
		}
		var decoded api.ExecutionLogsReadResult
		if err := decodeResultBody(result, &decoded); err != nil {
			return nil, fmt.Errorf("decode execution.logs.read: %w", err)
		}
		if decoded.Truncated {
			return nil, fmt.Errorf("retained %s log of action %s is truncated", stream, actionID)
		}
		chunks = append(chunks, decoded.Chunks...)
		if decoded.NextCursor == "" {
			return harness.ReassembleLog(chunks, actionID, stream)
		}
		cursor = decoded.NextCursor
	}
	return nil, fmt.Errorf("retained %s log of action %s did not finish within 64 pages", stream, actionID)
}

// smokeWaitTaskState polls task.read until the task reaches want. A 401/403
// aborts immediately with the classified credential/authority error.
func (instance *live) smokeWaitTaskState(ctx context.Context, t *testing.T, token harness.Secret, taskID, want string, timeout time.Duration) api.TaskReadResult {
	t.Helper()
	task, _, err := smoke.Wait(ctx, 250*time.Millisecond, timeout, "task.read", func(ctx context.Context) (api.TaskReadResult, error) {
		return instance.smokeTaskReadResult(ctx, token, taskID)
	}, func(task api.TaskReadResult) bool {
		return task.State == want
	})
	if err != nil {
		encoded, _ := json.Marshal(task)
		t.Fatalf("task %s did not reach state %q within %s: %v; last=%s", taskID, want, timeout, err, encoded)
	}
	return task
}

// smokeReadLogs reads one retained action stream and scans it for credentials
// before returning it.
func (instance *live) smokeReadLogs(ctx context.Context, t *testing.T, token harness.Secret, executionID, actionID, stream string) []byte {
	t.Helper()
	log, err := instance.smokeReadLogsResult(ctx, token, executionID, actionID, stream)
	if err != nil {
		t.Fatalf("read the %s log of action %s: %v", stream, actionID, err)
	}
	if err := harness.ScanCredentials(string(log), append(instance.config.PersonaSecrets(), instance.token)...); err != nil {
		t.Fatalf("retained %s log of action %s: %v", stream, actionID, err)
	}
	return log
}

// smokeExecutionIDs reads every execution identity in the project, following
// pagination.
func (instance *live) smokeExecutionIDs(ctx context.Context, t *testing.T, token harness.Secret) []string {
	t.Helper()
	ids := []string{}
	cursor := ""
	for range 64 {
		request := api.ExecutionListRequest{ProjectID: instance.projectID}
		if cursor != "" {
			request.Page = &api.Page{Cursor: cursor, Limit: api.MaxPageLimit}
		}
		result := instance.smokeCall(ctx, t, token, api.MethodGet, "execution.list", request, "", nil)
		decoded := decodeResult[api.ExecutionListResult](t, result)
		for _, execution := range decoded.Executions {
			ids = append(ids, execution.ExecutionID)
		}
		if decoded.NextCursor == "" {
			return ids
		}
		cursor = decoded.NextCursor
	}
	t.Fatalf("execution.list did not finish within 64 pages")
	return nil
}

// smokeScopedExecutionIDs reads the execution identities attributed to one task
// or stored pipeline, following pagination. A task-free inline execution has
// neither attribution and returns scoped=false: there is no scoped list to
// read. The S1/AC04 settlement proof deliberately starts from a stored pipeline
// and fails if scoped is false, so the list-level check always runs live.
func (instance *live) smokeScopedExecutionIDs(ctx context.Context, t *testing.T, token harness.Secret, taskID, pipelineID string) (ids []string, scoped bool) {
	t.Helper()
	if taskID == "" && pipelineID == "" {
		return nil, false
	}
	ids = []string{}
	cursor := ""
	for range 64 {
		request := api.ExecutionListRequest{ProjectID: instance.projectID, TaskID: taskID, PipelineID: pipelineID}
		if cursor != "" {
			request.Page = &api.Page{Cursor: cursor, Limit: api.MaxPageLimit}
		}
		result := instance.smokeCall(ctx, t, token, api.MethodGet, "execution.list", request, "", nil)
		decoded := decodeResult[api.ExecutionListResult](t, result)
		for _, execution := range decoded.Executions {
			ids = append(ids, execution.ExecutionID)
		}
		if decoded.NextCursor == "" {
			return ids, true
		}
		cursor = decoded.NextCursor
	}
	t.Fatalf("scoped execution.list did not finish within 64 pages")
	return nil, false
}

// smokeEventReadResult reads every committed event of the named types,
// following pagination, and classifies every fetch failure.
func (instance *live) smokeEventReadResult(ctx context.Context, token harness.Secret, types ...string) ([]api.Event, error) {
	events := []api.Event{}
	cursor := ""
	for range 64 {
		request := api.EventReadRequest{ProjectID: instance.projectID, Types: types}
		if cursor != "" {
			request.Page = &api.Page{Cursor: cursor, Limit: api.MaxPageLimit}
		}
		result, err := instance.invoke(ctx, token, api.MethodGet, "event.read", request, "", nil)
		if err != nil {
			classified, _ := smoke.Classify("event.read", err)
			return nil, classified
		}
		var decoded api.EventReadResult
		if err := decodeResultBody(result, &decoded); err != nil {
			return nil, fmt.Errorf("decode event.read: %w", err)
		}
		events = append(events, decoded.Events...)
		if !decoded.HasMore {
			return events, nil
		}
		if decoded.Next == "" {
			return nil, fmt.Errorf("event.read reported more events without a cursor")
		}
		cursor = decoded.Next
	}
	return nil, fmt.Errorf("event.read did not finish within 64 pages")
}

// smokeEventReadAll reads every committed event of the named types.
func (instance *live) smokeEventReadAll(ctx context.Context, t *testing.T, token harness.Secret, types ...string) []api.Event {
	t.Helper()
	events, err := instance.smokeEventReadResult(ctx, token, types...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return events
}

// smokeWaitEvent polls for one committed custom observation with the exact
// subject and correlation identities.
func (instance *live) smokeWaitEvent(ctx context.Context, t *testing.T, token harness.Secret, subjectID, correlationID string, timeout time.Duration) api.Event {
	t.Helper()
	events, _, err := smoke.Wait(ctx, 250*time.Millisecond, timeout, "event.read", func(ctx context.Context) ([]api.Event, error) {
		return instance.smokeEventReadResult(ctx, token, "observation.custom")
	}, func(events []api.Event) bool {
		for _, event := range events {
			if event.SubjectID == subjectID && event.CorrelationID == correlationID {
				return true
			}
		}
		return false
	})
	if err != nil {
		t.Fatalf("custom event %q correlated to %s did not appear: %v", subjectID, correlationID, err)
	}
	for _, event := range events {
		if event.SubjectID == subjectID && event.CorrelationID == correlationID {
			return event
		}
	}
	t.Fatalf("custom event %q correlated to %s is missing from the observed page", subjectID, correlationID)
	return api.Event{}
}

// smokeStartInline starts one inline task-free document as the given persona.
func (instance *live) smokeStartInline(ctx context.Context, t *testing.T, token harness.Secret, document []byte, key string) api.PipelineStartResult {
	t.Helper()
	result := instance.smokeCall(ctx, t, token, api.MethodPost, "pipeline.start", api.PipelineStartRequest{
		ProjectID: instance.projectID,
		YAML:      document,
	}, key, nil)
	return decodeResult[api.PipelineStartResult](t, result)
}

// smokeApplyPipeline applies one stored pipeline as the given persona.
func (instance *live) smokeApplyPipeline(ctx context.Context, t *testing.T, token harness.Secret, name string, document []byte, key string) api.PipelineApplyResult {
	t.Helper()
	result := instance.smokeCall(ctx, t, token, api.MethodPut, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: instance.projectID,
		Name:      name,
		YAML:      document,
	}, key, nil)
	return decodeResult[api.PipelineApplyResult](t, result)
}

// smokeTaskReadResult reads one task as the given persona.
func (instance *live) smokeTaskReadResult(ctx context.Context, token harness.Secret, taskID string) (api.TaskReadResult, error) {
	result, err := instance.invoke(ctx, token, api.MethodGet, "task.read", api.TaskReadRequest{TaskID: taskID}, "", nil)
	if err != nil {
		classified, _ := smoke.Classify("task.read", err)
		return api.TaskReadResult{}, classified
	}
	var task api.TaskReadResult
	if err := decodeResultBody(result, &task); err != nil {
		return api.TaskReadResult{}, fmt.Errorf("decode task.read: %w", err)
	}
	return task, nil
}

// smokeTaskRead reads one task as the given persona.
func (instance *live) smokeTaskRead(ctx context.Context, t *testing.T, token harness.Secret, taskID string) api.TaskReadResult {
	t.Helper()
	result := instance.smokeCall(ctx, t, token, api.MethodGet, "task.read", api.TaskReadRequest{TaskID: taskID}, "", nil)
	return decodeResult[api.TaskReadResult](t, result)
}

// smokeTaskInbox reads the named inbox kinds for the given persona.
func (instance *live) smokeTaskInbox(ctx context.Context, t *testing.T, token harness.Secret, kinds ...string) api.TaskInboxResult {
	t.Helper()
	result := instance.smokeCall(ctx, t, token, api.MethodGet, "task.inbox", api.TaskInboxRequest{
		ProjectID: instance.projectID,
		Kinds:     kinds,
	}, "", nil)
	return decodeResult[api.TaskInboxResult](t, result)
}

// smokeTaskCreateAndStart creates and starts one task as the given persona.
func (instance *live) smokeTaskCreateAndStart(ctx context.Context, t *testing.T, token harness.Secret, request api.TaskCreateAndStartRequest, key string) api.TaskCreateAndStartResult {
	t.Helper()
	result := instance.smokeCall(ctx, t, token, api.MethodPost, "task.create_and_start", request, key, nil)
	return decodeResult[api.TaskCreateAndStartResult](t, result)
}

// smokeTaskAsk asks the outstanding task question.
func (instance *live) smokeTaskAsk(ctx context.Context, t *testing.T, token harness.Secret, request api.TaskAskRequest, expected *uint64, key string) api.TaskAskResult {
	t.Helper()
	result := instance.smokeCall(ctx, t, token, api.MethodPost, "task.ask", request, key, expected)
	return decodeResult[api.TaskAskResult](t, result)
}

// smokeTaskReply answers the outstanding task question.
func (instance *live) smokeTaskReply(ctx context.Context, t *testing.T, token harness.Secret, request api.TaskReplyRequest, expected *uint64, key string) api.TaskReplyResult {
	t.Helper()
	result := instance.smokeCall(ctx, t, token, api.MethodPost, "task.reply", request, key, expected)
	return decodeResult[api.TaskReplyResult](t, result)
}

// smokeTaskComment appends one durable conversation comment.
func (instance *live) smokeTaskComment(ctx context.Context, t *testing.T, token harness.Secret, request api.TaskCommentRequest, expected *uint64, key string) api.TaskCommentResult {
	t.Helper()
	result := instance.smokeCall(ctx, t, token, api.MethodPost, "task.comment", request, key, expected)
	return decodeResult[api.TaskCommentResult](t, result)
}

// smokeTaskStart starts the task's selected pipeline.
func (instance *live) smokeTaskStart(ctx context.Context, t *testing.T, token harness.Secret, request api.TaskStartRequest, expected *uint64, key string) api.TaskStartResult {
	t.Helper()
	result := instance.smokeCall(ctx, t, token, api.MethodPost, "task.start", request, key, expected)
	return decodeResult[api.TaskStartResult](t, result)
}

// smokeResolve resolves one running request action. The envelope expected
// version is deliberately omitted: the body carries expected_action_version.
func (instance *live) smokeResolve(ctx context.Context, t *testing.T, token harness.Secret, request api.ExecutionRequestResolveRequest, key string) api.ExecutionRequestResolveResult {
	t.Helper()
	result := instance.smokeCall(ctx, t, token, api.MethodPost, "execution.request.resolve", request, key, nil)
	return decodeResult[api.ExecutionRequestResolveResult](t, result)
}

// smokeRequireForbidden asserts an authenticated persona without the role is
// refused with the public forbidden code.
func (instance *live) smokeRequireForbidden(ctx context.Context, t *testing.T, token harness.Secret, method api.Method, operation string, input any) {
	t.Helper()
	_, err := instance.invoke(ctx, token, method, operation, input, "", nil)
	var refusal *harness.APIError
	if !errors.As(err, &refusal) || refusal.Code != api.CodeForbidden {
		t.Fatalf("%s = %v; want the public forbidden refusal", operation, err)
	}
}

// smokeFindRequestInbox finds the inbox item for one exact request action.
func smokeFindRequestInbox(inbox api.TaskInboxResult, executionID, actionID string) (api.InboxItem, bool) {
	for _, item := range inbox.Items {
		if item.Kind == "request" && item.ExecutionID == executionID && item.ActionID == actionID {
			return item, true
		}
	}
	return api.InboxItem{}, false
}

// smokeFindWaitingTask finds the waiting_task item for one task.
func smokeFindWaitingTask(inbox api.TaskInboxResult, taskID string) (api.InboxItem, bool) {
	for _, item := range inbox.Items {
		if item.Kind == "waiting_task" && item.TaskID == taskID {
			return item, true
		}
	}
	return api.InboxItem{}, false
}

// smokeFindFailedTask finds the failed_task item for one task.
func smokeFindFailedTask(inbox api.TaskInboxResult, taskID string) (api.InboxItem, bool) {
	for _, item := range inbox.Items {
		if item.Kind == "failed_task" && item.TaskID == taskID {
			return item, true
		}
	}
	return api.InboxItem{}, false
}

// smokeSubjectValue returns one typed subject string field.
func smokeSubjectValue(subject *api.Subject, name string) (string, bool) {
	if subject == nil {
		return "", false
	}
	for _, field := range subject.Fields {
		if field.Name == name {
			return field.Value, true
		}
	}
	return "", false
}

// smokeMergeEnv merges action environment maps without mutating the inputs.
func smokeMergeEnv(maps ...map[string]string) map[string]string {
	merged := map[string]string{}
	for _, values := range maps {
		maps0.Copy(merged, values)
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// smokePersonaPrincipal returns the descriptor's derived principal identity
// for one persona.
func smokePersonaPrincipal(t *testing.T, instance *live, persona harness.Persona) string {
	t.Helper()
	view, ok := instance.config.Installation.OAuth.Personas[string(persona)]
	if !ok || view.PrincipalID == "" {
		t.Fatalf("the installation descriptor has no derived %s principal id", persona)
	}
	return view.PrincipalID
}

// smokeDumpLogs renders a failed execution's retained streams after scanning
// them for credentials.
func smokeDumpLogs(t *testing.T, instance *live, token harness.Secret, view api.ExecutionInspectResult) {
	t.Helper()
	for _, action := range view.Actions {
		for _, stream := range []string{"stdout", "stderr"} {
			log, err := instance.smokeReadLogsResult(context.Background(), token, view.ExecutionID, action.ActionID, stream)
			if err != nil {
				continue
			}
			if err := harness.ScanCredentials(string(log), append(instance.config.PersonaSecrets(), instance.token)...); err != nil {
				t.Logf("action %s %s was withheld: %v", action.Name, stream, err)
				continue
			}
			t.Logf("action %s %s:\n%s", action.Name, stream, log)
		}
	}
}

// smokeCleanupExecution cancels a nonterminal execution and waits for terminal
// cleanup. A cleanup that cannot complete fails the parent test with the owned
// execution identity; smoke.sh teardown does not remove workloads.
func (instance *live) smokeCleanupExecution(t *testing.T, token harness.Secret, executionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	view, err := instance.smokeInspectResult(ctx, token, executionID)
	if err != nil {
		t.Errorf("cleanup: inspect execution %s: %v", executionID, err)
		return
	}
	if !harness.IsTerminalState(view.State) {
		if err := instance.smokeCancelCurrent(ctx, token, executionID); err != nil {
			t.Errorf("cleanup: cancel execution %s: %v", executionID, err)
		}
	}
	if _, err := instance.smokeWaitCompleteResult(ctx, token, executionID, 2*time.Minute); err != nil {
		t.Errorf("cleanup: execution %s did not reach terminal cleanup: %v", executionID, err)
	}
}

// smokeCancelCurrent cancels one execution at its current version, retrying a
// version conflict a small number of times.
func (instance *live) smokeCancelCurrent(ctx context.Context, token harness.Secret, executionID string) error {
	key := "w49-cleanup-" + executionID
	for attempt := range 5 {
		view, err := instance.smokeInspectResult(ctx, token, executionID)
		if err != nil {
			return err
		}
		if harness.IsTerminalState(view.State) {
			return nil
		}
		_, err = instance.invoke(ctx, token, api.MethodPost, "execution.cancel", api.ExecutionCancelRequest{ExecutionID: executionID}, key+"-"+strconv.Itoa(attempt), &view.Version)
		if err == nil {
			return nil
		}
		if publicErrorCode(err) != api.CodeVersionConflict {
			classified, _ := smoke.Classify("execution.cancel", err)
			return classified
		}
	}
	return fmt.Errorf("execution.cancel %s did not converge on a current version", executionID)
}

// smokeCancelTaskCurrent cancels one open or waiting task at its current
// lifecycle version, re-reading after a version conflict.
func (instance *live) smokeCancelTaskCurrent(ctx context.Context, taskID string, lifecycleVersion uint64) error {
	key := "w49-cleanup-task-" + taskID
	current := lifecycleVersion
	for attempt := range 5 {
		_, err := instance.invoke(ctx, instance.token, api.MethodPost, "task.cancel", api.TaskCancelRequest{TaskID: taskID}, key+"-"+strconv.Itoa(attempt), &current)
		if err == nil {
			return nil
		}
		if publicErrorCode(err) != api.CodeVersionConflict {
			classified, _ := smoke.Classify("task.cancel", err)
			return classified
		}
		refreshed, readErr := instance.smokeTaskReadResult(ctx, instance.token, taskID)
		if readErr != nil {
			return readErr
		}
		if refreshed.State != "open" && refreshed.State != "waiting" {
			return nil
		}
		current = refreshed.LifecycleVersion
	}
	return fmt.Errorf("task.cancel %s did not converge on a current lifecycle version", taskID)
}

// smokeCleanupTask settles a waiting/open task and then cancels and waits for
// EVERY execution the task owns, even when the task cancel fails or is refused.
// A failed scenario may have queued a fresh execution after a reply or retry;
// that execution is only asynchronously cancelled, so the cleanup reads the
// complete history plus the current execution and waits on each one.
func (instance *live) smokeCleanupTask(t *testing.T, taskID, executionID string) {
	t.Helper()
	owned := map[string]bool{}
	add := func(ids []string) {
		for _, id := range ids {
			if id != "" {
				owned[id] = true
			}
		}
	}
	add([]string{executionID})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	task, err := instance.smokeTaskReadResult(ctx, instance.token, taskID)
	cancel()
	if err != nil {
		t.Errorf("cleanup: read task %s for owned execution ids: %v", taskID, err)
	} else {
		add(smoke.ExecutionIDs(task))
		if task.State == "open" || task.State == "waiting" {
			cancelCtx, cancelCancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := instance.smokeCancelTaskCurrent(cancelCtx, taskID, task.LifecycleVersion); err != nil {
				t.Errorf("cleanup: cancel task %s: %v", taskID, err)
			}
			cancelCancel()
		}
	}

	// Re-read even after a failed task cancel so an execution queued by the
	// failing scenario is included in the wait set.
	refreshCtx, cancelRefresh := context.WithTimeout(context.Background(), 2*time.Minute)
	refreshed, err := instance.smokeTaskReadResult(refreshCtx, instance.token, taskID)
	cancelRefresh()
	if err != nil {
		t.Errorf("cleanup: re-read task %s for owned execution ids: %v", taskID, err)
	} else {
		add(smoke.ExecutionIDs(refreshed))
	}

	ids := make([]string, 0, len(owned))
	for id := range owned {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		instance.smokeCleanupExecution(t, instance.token, id)
	}
}
