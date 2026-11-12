//go:build integration

package lifecycle

// Email-intake real-stack suite: one bounded email creates and starts
// exactly one diagnostic task through the served public `task.create_and_start`
// operation, and its Message-ID identity drives convergence.
//
// This suite proves against one real factory composition:
//   - one message creates one task and one running execution, with the trusted
//     configured project/pipeline and no message-selected authority;
//   - a lost create-and-start response converges on redelivery to the same task
//     and execution without a second task;
//   - changed content under the same Message-ID is refused through the served
//     receipt while the original task remains the only task;
//   - hostile message headers select neither scope, pipeline nor role.
//
// The intake reads no factory database: it crosses only the public operation
// boundary with the issued service credential.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/services"
	servicesapp "github.com/optimiware/kodama/internal/userland/services/app"
	"github.com/optimiware/kodama/test/support/factory"
)

const emailIntakeServiceID = "email-intake-svc"

// servedEmailHandler adapts one real issued service credential to the public
// operation boundary the intake consumes. It can drop the first committed
// response to model a lost response after the public receipt committed.
type servedEmailHandler struct {
	harness *factory.Harness
	token   string

	mu        sync.Mutex
	calls     int
	loseFirst bool
}

func (handler *servedEmailHandler) Invoke(ctx context.Context, invocation api.Invocation) (api.Result, error) {
	result, publicError, err := handler.harness.Invoke(ctx, handler.token, invocation.Operation,
		json.RawMessage(invocation.Request.Input), invocation.Request.ExpectedVersion, invocation.Request.IdempotencyKey)
	if err != nil {
		return api.Result{}, err
	}
	if publicError.Code != "" {
		return api.Result{}, &api.Error{Code: publicError.Code, Message: publicError.Message}
	}

	handler.mu.Lock()
	handler.calls++
	lose := handler.loseFirst
	if lose {
		handler.loseFirst = false
	}
	handler.mu.Unlock()
	if lose {
		// The operation committed at the served edge; only the response was lost.
		return api.Result{}, errors.New("simulated lost task.create_and_start response")
	}
	return result, nil
}

func (handler *servedEmailHandler) callCount() int {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.calls
}

// emailIntakeMessage renders one bounded RFC822 message with optional extra
// headers inserted before the blank line.
func emailIntakeMessage(messageID, subject, body string, extraHeaders ...string) string {
	var message strings.Builder
	message.WriteString("Message-ID: " + messageID + "\r\n")
	message.WriteString("Subject: " + subject + "\r\n")
	message.WriteString("From: reporter@example.test\r\n")
	message.WriteString("To: intake@kodama.test\r\n")
	message.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	for _, header := range extraHeaders {
		message.WriteString(header + "\r\n")
	}
	message.WriteString("\r\n")
	message.WriteString(body)
	return message.String()
}

// TestEmailIntakeServedCreateAndStart proves the real-stack intake behaviour.
func TestEmailIntakeServedCreateAndStart(t *testing.T) {
	harness := factory.Start(t)
	ctx := t.Context()

	// One service account with the engineer role: it may create and start a
	// task in its project, and the mounted credential is the only authority.
	created := harness.MustREST(harness.ProjectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: factory.ProjectID,
		ServiceID: emailIntakeServiceID,
		Roles:     []string{"engineer"},
	}, nil, "create-"+emailIntakeServiceID)
	var createdService api.ServiceCreateResult
	harness.Decode(created, &createdService)
	issued := harness.MustREST(harness.ProjectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
		ServiceID:  emailIntakeServiceID,
		TTLSeconds: 3600,
	}, &createdService.Version, "issue-"+emailIntakeServiceID)
	var credential api.ServiceCredentialIssueResult
	harness.Decode(issued, &credential)
	if credential.Token == nil || *credential.Token == "" {
		t.Fatal("service.credential.issue returned no credential token")
	}
	serviceToken := *credential.Token

	// The diagnostic pipeline is a request action assigned to the operator
	// role, so the started execution stays running without a container.
	document := marshalPipeline(t, pipelineDoc{Actions: []pipelineActionDoc{{
		Name:    "diagnose",
		Request: "role/operator",
		Subject: map[string]any{"operation": "email.diagnostic"},
		Timeout: "1h",
	}}})
	applied := applyPipeline(t, harness, "email-diagnostic", document)

	config := servicesapp.EmailIntakeConfig{
		TenantID:   harness.TenantID,
		ProjectID:  harness.ProjectID,
		PipelineID: applied.PipelineID,
	}
	runIntake := func(handler api.Handler, message string) (services.EmailIntakeResult, error) {
		config.Handler = handler
		config.Message = strings.NewReader(message)
		return servicesapp.RunEmailIntake(ctx, config)
	}
	// The canonical Message-ID is the token without its conventional bracket
	// pair; the wire spelling below keeps the brackets the sender uses.
	const messageID = "email-intake@example.test"
	message := emailIntakeMessage("<"+messageID+">", "Printer queue diagnostic",
		"Check the office printer queue and report the failing job.\r\n")

	// The first create-and-start commits but its response is lost.
	handler := &servedEmailHandler{harness: harness, token: serviceToken, loseFirst: true}
	if _, err := runIntake(handler, message); err == nil {
		t.Fatal("RunEmailIntake reported success although the response was lost")
	}
	if calls := handler.callCount(); calls != 1 {
		t.Fatalf("public calls = %d; want the one committed create-and-start", calls)
	}

	// Redelivery after the lost response converges to the same task and
	// execution: the stable Message-ID key answers from the public receipt.
	handler.loseFirst = false
	first, err := runIntake(handler, message)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if first.TaskID == "" || first.ExecutionID == "" || first.LifecycleVersion == 0 {
		t.Fatalf("intake result = %+v; want the created identities", first)
	}
	if calls := handler.callCount(); calls != 2 {
		t.Fatalf("public calls = %d; want the lost call and the redelivery", calls)
	}

	// Exactly one task exists with the trusted pipeline and the message
	// identity metadata; the initiator is the intake service, not a human role.
	read := harness.MustREST(serviceToken, "task.read", api.TaskReadRequest{TaskID: first.TaskID}, nil, "")
	var task api.TaskReadResult
	harness.Decode(read, &task)
	if task.SelectedPipeline != applied.PipelineID {
		t.Fatalf("task pipeline = %q; want the configured %q", task.SelectedPipeline, applied.PipelineID)
	}
	if task.Metadata["email.message_id"] != messageID {
		t.Fatalf("task metadata = %+v; want the message identity %q", task.Metadata, messageID)
	}
	if task.Initiator != emailIntakeServiceID {
		t.Fatalf("task initiator = %q; want the intake service %q", task.Initiator, emailIntakeServiceID)
	}
	if len(task.ExecutionHistory) != 1 || task.ExecutionHistory[0] != first.ExecutionID {
		t.Fatalf("execution history = %+v; want exactly the redelivered execution %q", task.ExecutionHistory, first.ExecutionID)
	}
	if task.CurrentExecution != first.ExecutionID {
		t.Fatalf("current execution = %q; want %q", task.CurrentExecution, first.ExecutionID)
	}

	// Equivalent spellings of the same message converge through the served
	// receipt: bracket and transport whitespace around the Message-ID, CRLF
	// versus LF line endings, and an optional single mbox `From ` envelope
	// line. The complete submitted request must stay byte-identical, so a
	// normalization that changed only the unit-level digest or only the
	// description would create a second task here.
	equivalents := []string{
		"From intake@example.test\r\n" + message,
		"From intake@example.test\n" + strings.ReplaceAll(message, "\r\n", "\n"),
		strings.Replace(message,
			"Message-ID: <"+messageID+">", "Message-ID: \t<"+messageID+">\t ", 1),
	}
	for index, equivalent := range equivalents {
		replay, err := runIntake(handler, equivalent)
		if err != nil {
			t.Fatalf("equivalent delivery %d: %v", index, err)
		}
		if replay.TaskID != first.TaskID || replay.ExecutionID != first.ExecutionID {
			t.Fatalf("equivalent delivery %d converged to %s/%s; want %s/%s",
				index, replay.TaskID, replay.ExecutionID, first.TaskID, first.ExecutionID)
		}
	}
	if calls := handler.callCount(); calls != 2+len(equivalents) {
		t.Fatalf("public calls = %d; want the lost call, the redelivery and every equivalent", calls)
	}
	// The stored metadata keeps the one canonical identity and the task still
	// has exactly one execution history entry.
	againRead := harness.MustREST(serviceToken, "task.read", api.TaskReadRequest{TaskID: first.TaskID}, nil, "")
	var equivalenceTask api.TaskReadResult
	harness.Decode(againRead, &equivalenceTask)
	if equivalenceTask.Metadata["email.message_id"] != messageID {
		t.Fatalf("stored metadata = %+v; want the canonical identity %q", equivalenceTask.Metadata, messageID)
	}
	if len(equivalenceTask.ExecutionHistory) != 1 || equivalenceTask.ExecutionHistory[0] != first.ExecutionID {
		t.Fatalf("execution history = %+v; want exactly the one execution %q",
			equivalenceTask.ExecutionHistory, first.ExecutionID)
	}

	// The execution is running its one diagnostic request action.
	running := waitRequestRunning(t, harness, first.ExecutionID, "diagnose")
	if running.ExecutionID != first.ExecutionID {
		t.Fatalf("running execution = %q; want %q", running.ExecutionID, first.ExecutionID)
	}

	// Changed content under the same Message-ID is refused by the served
	// receipt, and no second task is created.
	changed, err := runIntake(handler, emailIntakeMessage("<"+messageID+">", "Printer queue diagnostic",
		"A different body under the same message identity.\r\n"))
	if !errors.Is(err, services.ErrEmailContentChanged) {
		t.Fatalf("changed content error = %v; want ErrEmailContentChanged", err)
	}
	if changed.TaskID != "" || changed.ExecutionID != "" {
		t.Fatalf("changed content returned task %s/%s; want no task", changed.TaskID, changed.ExecutionID)
	}
	// The original task is still the only task for this identity.
	again := harness.MustREST(serviceToken, "task.read", api.TaskReadRequest{TaskID: first.TaskID}, nil, "")
	var againTask api.TaskReadResult
	harness.Decode(again, &againTask)
	if againTask.TaskID != first.TaskID || len(againTask.ExecutionHistory) != 1 {
		t.Fatalf("original task changed after the refused retry: %+v", againTask)
	}

	// Hostile headers select neither scope, pipeline nor role.
	hostile := emailIntakeMessage("<email-hostile@example.test>", "Hostile headers",
		"diagnose the hostile message\r\n",
		"X-Kodama-Tenant: tenant-evil",
		"X-Kodama-Project: project-evil",
		"X-Kodama-Pipeline: pipeline-evil",
		"Pipeline-ID: pipeline-evil",
		"X-Kodama-Role: admin",
		"X-Kodama-Assignment: role/admin",
		"Reply-To: attacker@evil.test",
	)
	hostileResult, err := runIntake(handler, hostile)
	if err != nil {
		t.Fatalf("hostile-header message: %v", err)
	}
	hostileRead := harness.MustREST(serviceToken, "task.read", api.TaskReadRequest{TaskID: hostileResult.TaskID}, nil, "")
	var hostileTask api.TaskReadResult
	harness.Decode(hostileRead, &hostileTask)
	if hostileTask.SelectedPipeline != applied.PipelineID {
		t.Fatalf("hostile-header task pipeline = %q; want the configured %q", hostileTask.SelectedPipeline, applied.PipelineID)
	}
	if hostileTask.Title != "Hostile headers" {
		t.Fatalf("hostile-header task title = %q; want the message subject", hostileTask.Title)
	}
	if hostileTask.Initiator != emailIntakeServiceID {
		t.Fatalf("hostile-header initiator = %q; want %q", hostileTask.Initiator, emailIntakeServiceID)
	}
}
