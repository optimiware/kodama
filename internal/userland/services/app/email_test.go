package app

// Contract tests: the one-shot email intake app entry.
//
// The entry is:
//
//	app.RunEmailIntake(ctx, app.EmailIntakeConfig) (services.EmailIntakeResult, error)
//
// It is composition only: it validates the trusted configuration, selects the
// injected public handler (or the public transport), and delegates the bounded
// message read and the single create-and-start to the services intake. It never
// selects scope from the message and starts no long-running process.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
)

// emailAppHandler is a minimal deterministic public operation boundary.
type emailAppHandler struct {
	mu          sync.Mutex
	invocations []api.Invocation
	requests    []api.TaskCreateAndStartRequest
	committed   api.Result
}

func (handler *emailAppHandler) Invoke(_ context.Context, invocation api.Invocation) (api.Result, error) {
	codec, ok := emailOperationCodec(invocation.Operation)
	if !ok {
		return api.Result{}, fmt.Errorf("unexpected operation %q", invocation.Operation)
	}
	decoded, err := codec.DecodeRequest(invocation.Request.Input)
	if err != nil {
		return api.Result{}, err
	}
	request, ok := decoded.(api.TaskCreateAndStartRequest)
	if !ok {
		return api.Result{}, fmt.Errorf("request decoded as %T", decoded)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.invocations = append(handler.invocations, invocation)
	handler.requests = append(handler.requests, request)
	if handler.committed.Operation != "" {
		return handler.committed, nil
	}
	body, err := codec.EncodeResult(api.TaskCreateAndStartResult{
		TaskID:           "task-app-1",
		ExecutionID:      "execution-app-1",
		LifecycleVersion: 1,
	})
	if err != nil {
		return api.Result{}, err
	}
	handler.committed = api.Result{Operation: invocation.Operation, Body: body}
	return handler.committed, nil
}

func (handler *emailAppHandler) recorded() ([]api.Invocation, []api.TaskCreateAndStartRequest) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return append([]api.Invocation(nil), handler.invocations...), append([]api.TaskCreateAndStartRequest(nil), handler.requests...)
}

func emailOperationCodec(operation string) (api.OperationCodec, bool) {
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == operation {
			return codec, true
		}
	}
	return api.OperationCodec{}, false
}

// emailAppTransport adapts the injected handler to the public transport seam so
// the test proves the entry accepts either boundary.
type emailAppTransport struct{ handler api.Handler }

func (transport emailAppTransport) Invoke(ctx context.Context, invocation api.Invocation) (api.Result, error) {
	return transport.handler.Invoke(ctx, invocation)
}

func appEmailMessage(messageID, subject, body string) string {
	return "Message-ID: " + messageID + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"From: reporter@example.test\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" + body
}

func appEmailConfig(handler api.Handler) EmailIntakeConfig {
	return EmailIntakeConfig{
		TenantID:   id.TenantID("tenant-app"),
		ProjectID:  id.ProjectID("project-app"),
		PipelineID: "pipeline-app",
		Message:    strings.NewReader(appEmailMessage("<app-intake@example.test>", "App entry", "diagnose\r\n")),
		Handler:    handler,
	}
}

// TestRunEmailIntakeCreatesAndStartsThroughThePublicHandler pins the concrete
// app entry and proves it creates and starts exactly one task with the trusted
// scope and configured pipeline.
func TestRunEmailIntakeCreatesAndStartsThroughThePublicHandler(t *testing.T) {
	handler := &emailAppHandler{}
	result, err := RunEmailIntake(t.Context(), appEmailConfig(handler))
	if err != nil {
		t.Fatalf("RunEmailIntake: %v", err)
	}
	if result.TaskID == "" || result.ExecutionID == "" || result.MessageID != "app-intake@example.test" {
		t.Fatalf("result = %+v; want the intake identities", result)
	}
	invocations, requests := handler.recorded()
	if len(invocations) != 1 {
		t.Fatalf("public invocations = %d; want exactly one", len(invocations))
	}
	if invocations[0].Operation != "task.create_and_start" {
		t.Fatalf("operation = %q; want task.create_and_start", invocations[0].Operation)
	}
	if requests[0].ProjectID != "project-app" || requests[0].PipelineID != "pipeline-app" {
		t.Fatalf("request = %+v; want the trusted project and pipeline", requests[0])
	}
	if result.TaskID != "task-app-1" || result.ExecutionID != "execution-app-1" {
		t.Fatalf("result = %+v; want the committed public result", result)
	}

	// The public transport is the alternative boundary; it crosses the same
	// operation and produces the same result.
	transport := &emailAppHandler{}
	viaTransport := EmailIntakeConfig{
		TenantID:   id.TenantID("tenant-app"),
		ProjectID:  id.ProjectID("project-app"),
		PipelineID: "pipeline-app",
		Message:    strings.NewReader(appEmailMessage("<app-intake@example.test>", "App entry", "diagnose\r\n")),
		Transport:  emailAppTransport{handler: transport},
	}
	if _, err := RunEmailIntake(t.Context(), viaTransport); err != nil {
		t.Fatalf("RunEmailIntake over the public transport: %v", err)
	}
	if invocations, _ := transport.recorded(); len(invocations) != 1 {
		t.Fatalf("transport invocations = %d; want exactly one", len(invocations))
	}
}

// TestRunEmailIntakeRefusesIncompleteConfiguration proves the entry refuses an
// incomplete configuration before any public call.
func TestRunEmailIntakeRefusesIncompleteConfiguration(t *testing.T) {
	handler := &emailAppHandler{}
	cases := []struct {
		name   string
		config EmailIntakeConfig
	}{
		{"missing message", EmailIntakeConfig{TenantID: "tenant-app", ProjectID: "project-app", PipelineID: "pipeline-app", Handler: handler}},
		{"missing tenant", EmailIntakeConfig{ProjectID: "project-app", PipelineID: "pipeline-app", Message: strings.NewReader("x"), Handler: handler}},
		{"missing project", EmailIntakeConfig{TenantID: "tenant-app", PipelineID: "pipeline-app", Message: strings.NewReader("x"), Handler: handler}},
		{"missing pipeline", EmailIntakeConfig{TenantID: "tenant-app", ProjectID: "project-app", Message: strings.NewReader("x"), Handler: handler}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := RunEmailIntake(t.Context(), testCase.config); err == nil {
				t.Fatalf("RunEmailIntake accepted %s", testCase.name)
			}
		})
	}

	noBoundary := appEmailConfig(nil)
	noBoundary.Handler = nil
	noBoundary.Transport = nil
	if _, err := RunEmailIntake(t.Context(), noBoundary); err == nil {
		t.Fatal("RunEmailIntake succeeded without a public handler or transport")
	}
	if invocations, _ := handler.recorded(); len(invocations) != 0 {
		t.Fatalf("public invocations = %d; want none after configuration refusals", len(invocations))
	}
}

// TestRunEmailIntakeSurfacesRefusedMessage proves the app entry propagates the
// services refusal instead of inventing a task.
func TestRunEmailIntakeSurfacesRefusedMessage(t *testing.T) {
	handler := &emailAppHandler{}
	config := appEmailConfig(handler)
	config.Message = strings.NewReader("Message-ID: <app-attachment@example.test>\r\n" +
		"Subject: Refused\r\n" +
		"Content-Type: text/plain\r\n" +
		"Content-Disposition: attachment; filename=\"evidence.bin\"\r\n" +
		"\r\nbody\r\n")
	result, err := RunEmailIntake(t.Context(), config)
	if err == nil {
		t.Fatalf("RunEmailIntake accepted an attachment and returned %+v", result)
	}
	if invocations, _ := handler.recorded(); len(invocations) != 0 {
		t.Fatalf("public invocations = %d; want none for a refused message", len(invocations))
	}
}
