package services_test

// Contract tests: email-to-task intake.
//
// One externally identified RFC822 message creates and starts exactly one
// diagnostic task through the public `task.create_and_start` operation. The
// tests below exercise the concrete entry point:
//
//	services.IntakeEmail(ctx, services.EmailIntakeConfig, io.Reader) (services.EmailIntakeResult, error)
//
// Trusted configuration (tenant, project, pipeline) selects scope. Nothing in
// the message selects scope, pipeline or a Kodama role. The bounded Message-ID
// is the only message identity; the stable idempotency key is derived from the
// trusted configuration plus that identity and never from the message content,
// so a redelivery converges through the public receipt while changed content
// under the same identity is refused.
//
// Size policy (smallest safe behaviour): the whole raw message, the header
// field count, the Message-ID value and the body are bounded, and any
// attachment or multipart message is refused rather than stored.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/services"
)

const (
	testEmailPipeline      = "pipeline-email"
	testMessageID          = "<p26-intake@example.test>"
	testCanonicalMessageID = "p26-intake@example.test"
)

func emailIntakeConfig(handler api.Handler) services.EmailIntakeConfig {
	return services.EmailIntakeConfig{
		TenantID:   testTenant,
		ProjectID:  testProject,
		PipelineID: testEmailPipeline,
		Handler:    handler,
	}
}

// emailMessage renders one bounded RFC822 message. Exactly one Message-ID and
// one Subject are present; extra headers are appended before the blank line.
func emailMessage(messageID, subject, body string, extraHeaders ...string) string {
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

// emailInvocation is one recorded public operation call with its decoded
// request, so tests inspect the exact served DTO and not a private fake shape.
type emailInvocation struct {
	invocation api.Invocation
	request    api.TaskCreateAndStartRequest
}

type emailReceipt struct {
	input  []byte
	result api.Result
}

// emailFakeHandler is a deterministic public operation boundary with the
// receipt semantics the served edge provides: equal key and equal content is a
// replay of the committed result, while equal key with changed content is a
// public changed-idempotency-content refusal.
type emailFakeHandler struct {
	mu          sync.Mutex
	invocations []emailInvocation
	receipts    map[string]emailReceipt
	committed   []api.TaskCreateAndStartResult
	loseNext    bool
	// failWith makes the public call fail after the invocation is recorded, so
	// a test can drive the handler-failure pass-through with fixed error text.
	failWith error
}

func newEmailFakeHandler() *emailFakeHandler {
	return &emailFakeHandler{receipts: make(map[string]emailReceipt)}
}

func (handler *emailFakeHandler) Invoke(_ context.Context, invocation api.Invocation) (api.Result, error) {
	if err := invocation.Validate(); err != nil {
		return api.Result{}, err
	}
	codec, ok := servedCodecs[invocation.Operation]
	if !ok {
		return api.Result{}, fmt.Errorf("email intake invoked an unserved operation %q", invocation.Operation)
	}
	decoded, err := codec.DecodeRequest(invocation.Request.Input)
	if err != nil {
		return api.Result{}, err
	}
	request, ok := decoded.(api.TaskCreateAndStartRequest)
	if !ok {
		return api.Result{}, fmt.Errorf("email intake request decoded as %T", decoded)
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.invocations = append(handler.invocations, emailInvocation{invocation: invocation, request: request})
	if handler.failWith != nil {
		return api.Result{}, handler.failWith
	}
	if receipt, found := handler.receipts[invocation.Request.IdempotencyKey]; found {
		if !bytes.Equal(receipt.input, invocation.Request.Input) {
			return api.Result{}, &api.Error{
				Code:    api.CodeChangedIdempotencyContent,
				Message: "caller key was reused with changed content",
			}
		}
		result := receipt.result
		result.Replayed = true
		return result, nil
	}

	committed := api.TaskCreateAndStartResult{
		TaskID:           fmt.Sprintf("task-email-%d", len(handler.committed)+1),
		ExecutionID:      fmt.Sprintf("execution-email-%d", len(handler.committed)+1),
		LifecycleVersion: 1,
	}
	body, err := codec.EncodeResult(committed)
	if err != nil {
		return api.Result{}, err
	}
	result := api.Result{Operation: invocation.Operation, Body: body}
	handler.receipts[invocation.Request.IdempotencyKey] = emailReceipt{
		input:  append([]byte(nil), invocation.Request.Input...),
		result: result,
	}
	handler.committed = append(handler.committed, committed)
	if handler.loseNext {
		handler.loseNext = false
		// The public receipt committed; only the response was lost.
		return api.Result{}, errors.New("simulated lost task.create_and_start response")
	}
	return result, nil
}

func (handler *emailFakeHandler) recorded() []emailInvocation {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return append([]emailInvocation(nil), handler.invocations...)
}

func (handler *emailFakeHandler) commits() []api.TaskCreateAndStartResult {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return append([]api.TaskCreateAndStartResult(nil), handler.committed...)
}

func (handler *emailFakeHandler) invocationCount() int {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return len(handler.invocations)
}

// TestEmailCreatesAndStartsDiagnosticTask proves one message produces exactly
// one create-and-start call carrying the trusted scope, the configured
// pipeline, a bounded title/description and the message identity metadata.
func TestEmailCreatesAndStartsDiagnosticTask(t *testing.T) {
	handler := newEmailFakeHandler()
	body := "Check the office printer queue and report the failing job.\r\n"
	result, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(emailMessage(
		testMessageID, "Printer queue diagnostic", body,
	)))
	if err != nil {
		t.Fatalf("IntakeEmail: %v", err)
	}
	if result.MessageID != testCanonicalMessageID {
		t.Fatalf("result message identity = %q; want the canonical %q", result.MessageID, testCanonicalMessageID)
	}
	if result.TaskID == "" || result.ExecutionID == "" || result.LifecycleVersion == 0 {
		t.Fatalf("result = %+v; want the created task, execution and lifecycle version", result)
	}

	recorded := handler.recorded()
	if len(recorded) != 1 {
		t.Fatalf("public invocations = %d; want exactly one create-and-start", len(recorded))
	}
	call := recorded[0]
	if call.invocation.Operation != "task.create_and_start" {
		t.Fatalf("operation = %q; want task.create_and_start", call.invocation.Operation)
	}
	if err := call.invocation.Request.Validate(); err != nil {
		t.Fatalf("public envelope is invalid: %v", err)
	}
	if call.invocation.Request.IdempotencyKey == "" {
		t.Fatal("create-and-start carried no idempotency key")
	}
	request := call.request
	if request.ProjectID != string(testProject) {
		t.Fatalf("project = %q; want the trusted configuration project %q", request.ProjectID, testProject)
	}
	if request.PipelineID != testEmailPipeline {
		t.Fatalf("pipeline = %q; want the trusted configuration pipeline %q", request.PipelineID, testEmailPipeline)
	}
	if request.Title == "" {
		t.Fatal("the created task has no title")
	}
	if !strings.Contains(request.Description, "office printer queue") {
		t.Fatalf("description = %q; want the message body", request.Description)
	}
	if request.Metadata["email.message_id"] != testCanonicalMessageID {
		t.Fatalf("metadata message id = %q; want the canonical %q", request.Metadata["email.message_id"], testCanonicalMessageID)
	}
	// The message identity, never the content, keys the receipt. The exact
	// derived key is an implementation detail, but it must be stable and legal.
	firstKey := call.invocation.Request.IdempotencyKey
	if len(firstKey) > api.MaxIdempotencyKeyBytes {
		t.Fatalf("idempotency key is %d bytes; at most %d are allowed", len(firstKey), api.MaxIdempotencyKeyBytes)
	}

	// Trusted configuration is required: an incomplete configuration is refused
	// before any public call.
	incomplete := []struct {
		name   string
		config services.EmailIntakeConfig
	}{
		{"missing tenant", services.EmailIntakeConfig{ProjectID: testProject, PipelineID: testEmailPipeline, Handler: handler}},
		{"missing project", services.EmailIntakeConfig{TenantID: testTenant, PipelineID: testEmailPipeline, Handler: handler}},
		{"missing pipeline", services.EmailIntakeConfig{TenantID: testTenant, ProjectID: testProject, Handler: handler}},
		{"missing handler", services.EmailIntakeConfig{TenantID: testTenant, ProjectID: testProject, PipelineID: testEmailPipeline}},
	}
	for _, testCase := range incomplete {
		if _, err := services.IntakeEmail(t.Context(), testCase.config, strings.NewReader(emailMessage(testMessageID, "subject", "body"))); err == nil {
			t.Fatalf("%s: IntakeEmail succeeded; want a configuration refusal", testCase.name)
		}
	}
	if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), nil); err == nil {
		t.Fatal("IntakeEmail(nil source) succeeded; want a refusal")
	}
	if got := handler.invocationCount(); got != 1 {
		t.Fatalf("public invocations = %d after configuration refusals; want the one successful call", got)
	}

	// The subject contributes to the content fingerprint, so the same identity
	// with a different subject is a different request under the same key.
	if handler.commits()[0].TaskID != result.TaskID {
		t.Fatalf("committed task %q != returned task %q", handler.commits()[0].TaskID, result.TaskID)
	}
}

// TestEmailRedeliveryAfterResponseLossUsesSameTask proves a message delivered
// again after a lost response (or a restart) reuses the same key and content and
// converges to the one committed task and execution.
func TestEmailRedeliveryAfterResponseLossUsesSameTask(t *testing.T) {
	handler := newEmailFakeHandler()
	handler.loseNext = true
	message := emailMessage(testMessageID, "Restart convergence", "first delivery\r\n")

	if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(message)); err == nil {
		t.Fatal("IntakeEmail reported success although the response was lost")
	}
	commits := handler.commits()
	if len(commits) != 1 {
		t.Fatalf("committed tasks = %d; want the one committed by the lost response", len(commits))
	}
	committed := commits[0]

	result, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(message))
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if result.TaskID != committed.TaskID || result.ExecutionID != committed.ExecutionID {
		t.Fatalf("redelivery result = %s/%s; want the committed %s/%s",
			result.TaskID, result.ExecutionID, committed.TaskID, committed.ExecutionID)
	}
	if got := len(handler.commits()); got != 1 {
		t.Fatalf("committed tasks = %d; want no second task after redelivery", got)
	}

	recorded := handler.recorded()
	if len(recorded) != 2 {
		t.Fatalf("public invocations = %d; want the lost call and the redelivery", len(recorded))
	}
	if recorded[0].invocation.Request.IdempotencyKey != recorded[1].invocation.Request.IdempotencyKey {
		t.Fatalf("redelivery used a new key %q; want the original %q",
			recorded[1].invocation.Request.IdempotencyKey, recorded[0].invocation.Request.IdempotencyKey)
	}
	if !bytes.Equal(recorded[0].invocation.Request.Input, recorded[1].invocation.Request.Input) {
		t.Fatalf("redelivery changed the request content:\nfirst:  %s\nsecond: %s",
			recorded[0].invocation.Request.Input, recorded[1].invocation.Request.Input)
	}
}

// TestEmailChangedContentSameMessageIDRefused proves changed content under one
// message identity is refused without minting a new key or a new task.
func TestEmailChangedContentSameMessageIDRefused(t *testing.T) {
	handler := newEmailFakeHandler()
	first, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
		emailMessage(testMessageID, "Original subject", "original diagnostic body\r\n"),
	))
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	tampered := []struct {
		name    string
		message string
	}{
		{"changed body", emailMessage(testMessageID, "Original subject", "tampered diagnostic body\r\n")},
		{"changed subject", emailMessage(testMessageID, "Tampered subject", "original diagnostic body\r\n")},
	}
	for _, testCase := range tampered {
		t.Run(testCase.name, func(t *testing.T) {
			changed, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(testCase.message))
			if !errors.Is(err, services.ErrEmailContentChanged) {
				t.Fatalf("changed content error = %v; want ErrEmailContentChanged", err)
			}
			if changed.TaskID != "" || changed.ExecutionID != "" {
				t.Fatalf("changed content returned task %s/%s; want no task", changed.TaskID, changed.ExecutionID)
			}
		})
	}

	recorded := handler.recorded()
	if len(recorded) != 3 {
		t.Fatalf("public invocations = %d; want the original and both refused retries", len(recorded))
	}
	for index, call := range recorded[1:] {
		if call.invocation.Request.IdempotencyKey != recorded[0].invocation.Request.IdempotencyKey {
			t.Fatalf("changed content %d minted a new key %q; want the original %q",
				index, call.invocation.Request.IdempotencyKey, recorded[0].invocation.Request.IdempotencyKey)
		}
		if bytes.Equal(recorded[0].invocation.Request.Input, call.invocation.Request.Input) {
			t.Fatalf("changed content %d produced the identical public request", index)
		}
	}
	commits := handler.commits()
	if len(commits) != 1 || commits[0].TaskID != first.TaskID {
		t.Fatalf("committed tasks = %+v; want only the original %s", commits, first.TaskID)
	}
}

// TestEmailHeadersCannotSelectScopeOrPipeline proves message headers grant no
// scope, pipeline or Kodama role: only trusted configuration selects them.
func TestEmailHeadersCannotSelectScopeOrPipeline(t *testing.T) {
	hostile := []string{
		"X-Kodama-Tenant: tenant-evil",
		"X-Kodama-Project: project-evil",
		"X-Kodama-Pipeline: pipeline-evil",
		"Pipeline-ID: pipeline-evil",
		"X-Pipeline: pipeline-evil",
		"X-Kodama-Role: admin",
		"X-Kodama-Assignment: role/admin",
		"X-Kodama-Service: evil-service",
		"Reply-To: attacker@evil.test",
	}
	handler := newEmailFakeHandler()
	_, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
		emailMessage("<p26-hostile@example.test>", "Hostile headers", "diagnose\r\n", hostile...),
	))
	if err != nil {
		t.Fatalf("IntakeEmail with hostile headers: %v", err)
	}
	recorded := handler.recorded()
	if len(recorded) != 1 {
		t.Fatalf("public invocations = %d; want one", len(recorded))
	}
	request := recorded[0].request
	if request.ProjectID != string(testProject) {
		t.Fatalf("project = %q; a message header selected the scope; want %q", request.ProjectID, testProject)
	}
	if request.PipelineID != testEmailPipeline {
		t.Fatalf("pipeline = %q; a message header selected the pipeline; want %q", request.PipelineID, testEmailPipeline)
	}
	if request.Title != "Hostile headers" {
		t.Fatalf("title = %q; want the message subject", request.Title)
	}
	baselineKey := recorded[0].invocation.Request.IdempotencyKey
	for key, value := range request.Metadata {
		lower := strings.ToLower(key + "=" + value)
		for _, forbidden := range []string{"tenant", "project", "pipeline", "role/", "assignment", "service/"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("metadata %q=%q carries header-selected authority %q", key, value, forbidden)
			}
		}
	}
	// The same message under a different trusted configuration uses a different
	// receipt key; the message headers contributed no scope of their own. The
	// project and the tenant each participate, not just their combination.
	type scopeVariant struct {
		name   string
		config services.EmailIntakeConfig
	}
	variants := []scopeVariant{
		{
			name: "other project",
			config: services.EmailIntakeConfig{
				TenantID:   testTenant,
				ProjectID:  id.ProjectID("project-b"),
				PipelineID: testEmailPipeline,
				Handler:    nil,
			},
		},
		{
			name: "other tenant",
			config: services.EmailIntakeConfig{
				TenantID:   id.TenantID("tenant-b"),
				ProjectID:  testProject,
				PipelineID: testEmailPipeline,
				Handler:    nil,
			},
		},
	}
	for _, variant := range variants {
		handler := newEmailFakeHandler()
		variant.config.Handler = handler
		if _, err := services.IntakeEmail(t.Context(), variant.config, strings.NewReader(
			emailMessage("<p26-hostile@example.test>", "Hostile headers", "diagnose\r\n", hostile...),
		)); err != nil {
			t.Fatalf("IntakeEmail under the %s configuration: %v", variant.name, err)
		}
		recorded := handler.recorded()
		if len(recorded) != 1 {
			t.Fatalf("%s configuration invocations = %d; want one", variant.name, len(recorded))
		}
		if recorded[0].invocation.Request.IdempotencyKey == "" {
			t.Fatalf("%s configuration produced no key", variant.name)
		}
		if recorded[0].invocation.Request.IdempotencyKey == baselineKey {
			t.Fatalf("the receipt key does not include the %s scope", variant.name)
		}
		if recorded[0].request.ProjectID != string(variant.config.ProjectID) {
			t.Fatalf("%s request project = %q; want its own trusted %q",
				variant.name, recorded[0].request.ProjectID, variant.config.ProjectID)
		}
		if recorded[0].request.PipelineID != variant.config.PipelineID {
			t.Fatalf("%s request pipeline = %q; want its own trusted %q",
				variant.name, recorded[0].request.PipelineID, variant.config.PipelineID)
		}
	}
}

// TestEmailOversizeAndMalformedMessagesRefused proves every bound and malformed
// input is refused before any public call, and attachments are never stored.
func TestEmailOversizeAndMalformedMessagesRefused(t *testing.T) {
	manyHeaders := func(count int) string {
		headers := []string{
			"Message-ID: <p26-headers@example.test>",
			"Subject: header bound",
			"From: reporter@example.test",
			"To: intake@kodama.test",
			"Content-Type: text/plain; charset=utf-8",
		}
		for index := range count {
			headers = append(headers, fmt.Sprintf("X-Pad-%03d: value", index))
		}
		return strings.Join(headers, "\r\n") + "\r\n\r\nbody\r\n"
	}

	cases := []struct {
		name    string
		message string
		want    error
	}{
		{
			name:    "missing message identity",
			message: "Subject: no identity\r\n\r\nbody\r\n",
			want:    services.ErrEmailMalformed,
		},
		{
			name: "duplicate message identity",
			message: "Message-ID: <one@example.test>\r\nMessage-ID: <two@example.test>\r\n" +
				"Subject: ambiguous\r\n\r\nbody\r\n",
			want: services.ErrEmailMalformed,
		},
		{
			name:    "message identity with whitespace",
			message: "Message-ID: <one@example.test> <two@example.test>\r\nSubject: ambiguous\r\n\r\nbody\r\n",
			want:    services.ErrEmailMalformed,
		},
		{
			name: "oversize message identity",
			message: emailMessage("<"+strings.Repeat("a", services.MaxEmailMessageIDBytes)+"@example.test>",
				"identity bound", "body\r\n"),
			want: services.ErrEmailOversize,
		},
		{
			name:    "missing header separator",
			message: "Message-ID: <p26-noblank@example.test>\r\nSubject: no separator\r\n",
			want:    services.ErrEmailMalformed,
		},
		{
			name: "malformed header line",
			message: "Message-ID: <p26-badline@example.test>\r\nSubject: malformed\r\n" +
				"not-a-header-line\r\n\r\nbody\r\n",
			want: services.ErrEmailMalformed,
		},
		{
			name:    "too many headers",
			message: manyHeaders(services.MaxEmailHeaderCount),
			want:    services.ErrEmailOversize,
		},
		{
			name: "oversize body",
			message: emailMessage("<p26-body@example.test>", "body bound",
				strings.Repeat("b", services.MaxEmailBodyBytes+1)),
			want: services.ErrEmailOversize,
		},
		{
			name: "oversize raw message",
			message: emailMessage("<p26-raw@example.test>", "raw bound", "body\r\n",
				"X-Padding: "+strings.Repeat("p", services.MaxEmailMessageBytes)),
			want: services.ErrEmailOversize,
		},
		{
			name: "multipart attachment",
			message: "Message-ID: <p26-attachment@example.test>\r\n" +
				"Subject: attachment\r\n" +
				"Content-Type: multipart/mixed; boundary=\"b\"\r\n" +
				"\r\n" +
				"--b\r\n" +
				"Content-Type: text/plain\r\n\r\nhello\r\n" +
				"--b\r\n" +
				"Content-Type: application/octet-stream\r\n" +
				"Content-Disposition: attachment; filename=\"evidence.bin\"\r\n\r\nbytes\r\n" +
				"--b--\r\n",
			want: services.ErrEmailAttachment,
		},
		{
			name: "single part declared attachment",
			message: "Message-ID: <p26-disposition@example.test>\r\n" +
				"Subject: attachment\r\n" +
				"Content-Type: text/plain\r\n" +
				"Content-Disposition: attachment; filename=\"evidence.txt\"\r\n" +
				"\r\nbody\r\n",
			want: services.ErrEmailAttachment,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := newEmailFakeHandler()
			result, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(testCase.message))
			if err == nil {
				t.Fatalf("IntakeEmail accepted %s and returned %+v", testCase.name, result)
			}
			if !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v; want %v", err, testCase.want)
			}
			if got := handler.invocationCount(); got != 0 {
				t.Fatalf("public invocations = %d; want none for a refused message", got)
			}
		})
	}

	// The bounds are inclusive: a message at each bound is accepted.
	t.Run("exact bounds accepted", func(t *testing.T) {
		handler := newEmailFakeHandler()
		message := emailMessage(
			"<"+strings.Repeat("m", services.MaxEmailMessageIDBytes-2)+">",
			"boundary subject",
			strings.Repeat("b", services.MaxEmailBodyBytes),
		)
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(message)); err != nil {
			t.Fatalf("message at the identity and body bounds: %v", err)
		}
		if got := handler.invocationCount(); got != 1 {
			t.Fatalf("public invocations = %d; want one for an accepted message", got)
		}

		if services.MaxEmailHeaderCount < 5 {
			t.Fatalf("MaxEmailHeaderCount = %d; want room for the five base headers", services.MaxEmailHeaderCount)
		}
		handler = newEmailFakeHandler()
		exactHeaders := manyHeaders(services.MaxEmailHeaderCount - 5)
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(exactHeaders)); err != nil {
			t.Fatalf("message at the header-count bound: %v", err)
		}
		if got := handler.invocationCount(); got != 1 {
			t.Fatalf("public invocations = %d; want one for the exact header count", got)
		}
	})

	t.Run("source read failure", func(t *testing.T) {
		handler := newEmailFakeHandler()
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), errorReader{}); err == nil {
			t.Fatal("IntakeEmail accepted a failing source")
		}
		if got := handler.invocationCount(); got != 0 {
			t.Fatalf("public invocations = %d; want none after a source failure", got)
		}
	})
}

// errorReader fails every read, so a truncated transport cannot be mistaken for
// an empty message.
type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("simulated source failure")
}

// --- Bounds, key ambiguity, context and no-echo ---

// The media type is lowercased by mime.ParseMediaType, so the sentinel is
// lowercase to remain visible on every echo path.
const emailEchoSentinel = "p26-secret-payload"

// rawEmailMessageOfSize renders one well-formed header-only message whose exact
// encoded length is size bytes. The padding header carries no body, so the whole
// message bound is exercised without also crossing the body bound.
func rawEmailMessageOfSize(t *testing.T, size int) string {
	t.Helper()
	const prefix = "Message-ID: <p26-raw-limit@example.test>\r\n" +
		"Subject: raw limit\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"X-Padding: "
	const suffix = "\r\n\r\n"
	if size < len(prefix)+len(suffix) {
		t.Fatalf("requested raw message size %d is below the fixed header size %d", size, len(prefix)+len(suffix))
	}
	return prefix + strings.Repeat("p", size-len(prefix)-len(suffix)) + suffix
}

// emailHeadersMessage renders one message with exactly extraFields additional
// header fields beyond the five base headers.
func emailHeadersMessage(extraFields int) string {
	headers := []string{
		"Message-ID: <p26-header-limit@example.test>",
		"Subject: header bound",
		"From: reporter@example.test",
		"To: intake@kodama.test",
		"Content-Type: text/plain; charset=utf-8",
	}
	for index := range extraFields {
		headers = append(headers, fmt.Sprintf("X-Pad-%03d: value", index))
	}
	return strings.Join(headers, "\r\n") + "\r\n\r\nbody\r\n"
}

// emailTitleFor intake one message with the given subject and returns the task
// title the public request carried.
func emailTitleFor(t *testing.T, subject string) string {
	t.Helper()
	handler := newEmailFakeHandler()
	if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
		emailMessage("<p26-title@example.test>", subject, "body\r\n"),
	)); err != nil {
		t.Fatalf("IntakeEmail with a %d-byte subject: %v", len(subject), err)
	}
	recorded := handler.recorded()
	if len(recorded) != 1 {
		t.Fatalf("public invocations = %d; want one", len(recorded))
	}
	return recorded[0].request.Title
}

// TestEmailIntakeBoundsAreInclusive pins both sides of every intake bound.
func TestEmailIntakeBoundsAreInclusive(t *testing.T) {
	t.Run("whole raw message", func(t *testing.T) {
		atLimit := rawEmailMessageOfSize(t, services.MaxEmailMessageBytes)
		if len(atLimit) != services.MaxEmailMessageBytes {
			t.Fatalf("fixture is %d bytes; want %d", len(atLimit), services.MaxEmailMessageBytes)
		}
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(newEmailFakeHandler()), strings.NewReader(atLimit)); err != nil {
			t.Fatalf("message of exactly %d bytes: %v", len(atLimit), err)
		}

		overLimit := rawEmailMessageOfSize(t, services.MaxEmailMessageBytes+1)
		handler := newEmailFakeHandler()
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(overLimit)); !errors.Is(err, services.ErrEmailOversize) {
			t.Fatalf("message of %d bytes error = %v; want ErrEmailOversize", len(overLimit), err)
		}
		if got := handler.invocationCount(); got != 0 {
			t.Fatalf("public invocations = %d; want none for an oversize message", got)
		}
	})

	t.Run("header field count", func(t *testing.T) {
		for _, testCase := range []struct {
			name        string
			extraFields int
			wantRefused bool
		}{
			{"at the bound accepted", services.MaxEmailHeaderCount - 5, false},
			{"bound plus one refused", services.MaxEmailHeaderCount - 4, true},
		} {
			handler := newEmailFakeHandler()
			_, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
				emailHeadersMessage(testCase.extraFields),
			))
			if testCase.wantRefused {
				if !errors.Is(err, services.ErrEmailOversize) {
					t.Fatalf("%s: error = %v; want ErrEmailOversize", testCase.name, err)
				}
				if got := handler.invocationCount(); got != 0 {
					t.Fatalf("%s: public invocations = %d; want none", testCase.name, got)
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s: %v", testCase.name, err)
			}
		}
	})

	t.Run("message identity", func(t *testing.T) {
		atLimit := "<" + strings.Repeat("a", services.MaxEmailMessageIDBytes-2) + ">"
		if len(atLimit) != services.MaxEmailMessageIDBytes {
			t.Fatalf("fixture identity is %d bytes; want %d", len(atLimit), services.MaxEmailMessageIDBytes)
		}
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(newEmailFakeHandler()), strings.NewReader(
			emailMessage(atLimit, "identity bound", "body\r\n"),
		)); err != nil {
			t.Fatalf("message identity of exactly %d bytes: %v", len(atLimit), err)
		}

		overLimit := "<" + strings.Repeat("a", services.MaxEmailMessageIDBytes-1) + ">"
		handler := newEmailFakeHandler()
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
			emailMessage(overLimit, "identity bound", "body\r\n"),
		)); !errors.Is(err, services.ErrEmailOversize) {
			t.Fatalf("message identity of %d bytes error = %v; want ErrEmailOversize", len(overLimit), err)
		}
		if got := handler.invocationCount(); got != 0 {
			t.Fatalf("public invocations = %d; want none for an oversize identity", got)
		}

		// A valid multibyte identity at the bound is accepted; an identity of
		// the same length with one invalid byte is refused as malformed rather
		// than inflating under JSON encoding.
		multibyteAtLimit := "<" + strings.Repeat("é", (services.MaxEmailMessageIDBytes-2)/2) + ">"
		if len(multibyteAtLimit) != services.MaxEmailMessageIDBytes {
			t.Fatalf("multibyte identity is %d bytes; want %d", len(multibyteAtLimit), services.MaxEmailMessageIDBytes)
		}
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(newEmailFakeHandler()), strings.NewReader(
			emailMessage(multibyteAtLimit, "identity bound", "body\r\n"),
		)); err != nil {
			t.Fatalf("valid multibyte identity at the bound: %v", err)
		}

		invalidAtLimit := "<" + strings.Repeat("a", services.MaxEmailMessageIDBytes-3) + "\xff>"
		if len(invalidAtLimit) != services.MaxEmailMessageIDBytes {
			t.Fatalf("invalid identity is %d bytes; want %d", len(invalidAtLimit), services.MaxEmailMessageIDBytes)
		}
		handler = newEmailFakeHandler()
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
			emailMessage(invalidAtLimit, "identity bound", "body\r\n"),
		)); !errors.Is(err, services.ErrEmailMalformed) {
			t.Fatalf("invalid UTF-8 identity at the bound error = %v; want ErrEmailMalformed", err)
		}
		if got := handler.invocationCount(); got != 0 {
			t.Fatalf("public invocations = %d; want none for an invalid UTF-8 identity", got)
		}
	})

	t.Run("body", func(t *testing.T) {
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(newEmailFakeHandler()), strings.NewReader(
			emailMessage("<p26-body-limit@example.test>", "body bound", strings.Repeat("b", services.MaxEmailBodyBytes)),
		)); err != nil {
			t.Fatalf("body of exactly %d bytes: %v", services.MaxEmailBodyBytes, err)
		}

		handler := newEmailFakeHandler()
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
			emailMessage("<p26-body-limit@example.test>", "body bound", strings.Repeat("b", services.MaxEmailBodyBytes+1)),
		)); !errors.Is(err, services.ErrEmailOversize) {
			t.Fatalf("body of %d bytes error = %v; want ErrEmailOversize", services.MaxEmailBodyBytes+1, err)
		}
		if got := handler.invocationCount(); got != 0 {
			t.Fatalf("public invocations = %d; want none for an oversize body", got)
		}
	})

	t.Run("title", func(t *testing.T) {
		exact := strings.Repeat("t", services.MaxEmailSubjectBytes)
		if title := emailTitleFor(t, exact); title != exact {
			t.Fatalf("title at the bound = %q (len %d); want the subject unchanged", title, len(title))
		}

		over := strings.Repeat("t", services.MaxEmailSubjectBytes+1)
		title := emailTitleFor(t, over)
		if title != over[:services.MaxEmailSubjectBytes] {
			t.Fatalf("title for a %d-byte subject = %q (len %d); want the %d-byte prefix", len(over), title, len(title), services.MaxEmailSubjectBytes)
		}

		multibyteAtLimit := strings.Repeat("é", services.MaxEmailSubjectBytes/2)
		if len(multibyteAtLimit) != services.MaxEmailSubjectBytes {
			t.Fatalf("fixture is %d bytes; want %d", len(multibyteAtLimit), services.MaxEmailSubjectBytes)
		}
		if title := emailTitleFor(t, multibyteAtLimit); title != multibyteAtLimit {
			t.Fatalf("multibyte title at the bound = %q; want the subject unchanged", title)
		}

		// One ASCII byte then three-byte runes: the 200-byte cut lands inside
		// the last rune, which must be dropped rather than split.
		splitRune := "a" + strings.Repeat("é", 150)
		title = emailTitleFor(t, splitRune)
		if !utf8.ValidString(title) {
			t.Fatalf("title cut inside a rune is not valid UTF-8: %q", title)
		}
		if len(title) > services.MaxEmailSubjectBytes {
			t.Fatalf("title is %d bytes; want at most %d", len(title), services.MaxEmailSubjectBytes)
		}
		want := "a" + strings.Repeat("é", 99)
		if title != want {
			t.Fatalf("title cut inside a rune = %q (len %d); want %q (len %d)", title, len(title), want, len(want))
		}
	})
}

// endlessEmailReader never reports EOF. Once it has produced blockAfter bytes it
// stops making progress instead of growing without bound, so an unbounded read
// fails the test deadline rather than exhausting memory.
type endlessEmailReader struct {
	read       atomic.Int64
	blockAfter int64
}

func (reader *endlessEmailReader) Read(buffer []byte) (int, error) {
	read := reader.read.Load()
	if read >= reader.blockAfter {
		select {}
	}
	remaining := reader.blockAfter - read
	count := min(int64(len(buffer)), remaining)
	for index := range count {
		buffer[index] = 'x'
	}
	reader.read.Add(count)
	return int(count), nil
}

// gatedEmailReader blocks every read until released, after signalling that the
// read started.
type gatedEmailReader struct {
	started chan struct{}
	release chan struct{}
	reads   atomic.Int32
}

func newGatedEmailReader() *gatedEmailReader {
	return &gatedEmailReader{started: make(chan struct{}, 1), release: make(chan struct{})}
}

func (reader *gatedEmailReader) Read([]byte) (int, error) {
	reader.reads.Add(1)
	select {
	case reader.started <- struct{}{}:
	default:
	}
	<-reader.release
	return 0, io.EOF
}

// TestEmailIntakeEndlessSourceIsBounded proves an endless source is read only
// up to the whole-message bound (plus the one overflow byte) and refused.
func TestEmailIntakeEndlessSourceIsBounded(t *testing.T) {
	reader := &endlessEmailReader{blockAfter: 1 << 20}
	handler := newEmailFakeHandler()
	done := make(chan error, 1)
	go func() {
		_, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), reader)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, services.ErrEmailOversize) {
			t.Fatalf("endless source error = %v; want ErrEmailOversize", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("IntakeEmail did not bound an endless source")
	}
	if got, max := reader.read.Load(), int64(services.MaxEmailMessageBytes+1); got > max {
		t.Fatalf("read %d bytes from an endless source; at most %d are allowed", got, max)
	}
	if got := handler.invocationCount(); got != 0 {
		t.Fatalf("public invocations = %d; want none for an oversize source", got)
	}
}

// TestEmailIntakeHonoursContextWhileReading proves a cancellation is honoured
// before the read and while a read blocks, with no public call afterwards.
func TestEmailIntakeHonoursContextWhileReading(t *testing.T) {
	t.Run("cancelled before reading", func(t *testing.T) {
		handler := newEmailFakeHandler()
		reader := newGatedEmailReader()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		done := make(chan error, 1)
		go func() {
			_, err := services.IntakeEmail(ctx, emailIntakeConfig(handler), reader)
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v; want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("IntakeEmail did not return promptly for a cancelled context")
		}
		if got := reader.reads.Load(); got != 0 {
			t.Fatalf("source reads = %d; want none after cancellation", got)
		}
		if got := handler.invocationCount(); got != 0 {
			t.Fatalf("public invocations = %d; want none after cancellation", got)
		}
		close(reader.release)
	})

	t.Run("cancelled while reading", func(t *testing.T) {
		handler := newEmailFakeHandler()
		reader := newGatedEmailReader()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := services.IntakeEmail(ctx, emailIntakeConfig(handler), reader)
			done <- err
		}()
		select {
		case <-reader.started:
		case <-time.After(5 * time.Second):
			t.Fatal("IntakeEmail never began reading the source")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v; want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("IntakeEmail did not return after the context was cancelled during the read")
		}
		if got := handler.invocationCount(); got != 0 {
			t.Fatalf("public invocations = %d; want none after cancellation", got)
		}
		close(reader.release)
	})
}

// TestEmailIdempotencyKeyRejectsAmbiguousScopeSplits proves the key is built
// from separated, validated identifiers rather than a bare concatenation.
func TestEmailIdempotencyKeyRejectsAmbiguousScopeSplits(t *testing.T) {
	const messageID = "<p26-ambiguous@example.test>"
	configs := []services.EmailIntakeConfig{
		{TenantID: id.TenantID("ab"), ProjectID: id.ProjectID("c-1"), PipelineID: testEmailPipeline},
		{TenantID: id.TenantID("a"), ProjectID: id.ProjectID("bc-1"), PipelineID: testEmailPipeline},
	}
	keys := make([]string, 0, len(configs))
	for index := range configs {
		handler := newEmailFakeHandler()
		configs[index].Handler = handler
		if _, err := services.IntakeEmail(t.Context(), configs[index], strings.NewReader(
			emailMessage(messageID, "ambiguous scope", "body\r\n"),
		)); err != nil {
			t.Fatalf("IntakeEmail for tenant %q project %q: %v", configs[index].TenantID, configs[index].ProjectID, err)
		}
		recorded := handler.recorded()
		if len(recorded) != 1 {
			t.Fatalf("public invocations for tenant %q project %q = %d; want one",
				configs[index].TenantID, configs[index].ProjectID, len(recorded))
		}
		keys = append(keys, recorded[0].invocation.Request.IdempotencyKey)
	}
	if keys[0] == keys[1] {
		t.Fatalf("tenant 'ab' + project 'c-1' and tenant 'a' + project 'bc-1' produced the same receipt key %q", keys[0])
	}

	// The key is stable for the identical trusted scope.
	handler := newEmailFakeHandler()
	repeatConfig := services.EmailIntakeConfig{
		TenantID: configs[0].TenantID, ProjectID: configs[0].ProjectID,
		PipelineID: testEmailPipeline, Handler: handler,
	}
	if _, err := services.IntakeEmail(t.Context(), repeatConfig, strings.NewReader(
		emailMessage(messageID, "ambiguous scope", "body\r\n"),
	)); err != nil {
		t.Fatalf("repeat IntakeEmail: %v", err)
	}
	if recorded := handler.recorded(); len(recorded) != 1 || recorded[0].invocation.Request.IdempotencyKey != keys[0] {
		t.Fatalf("the identical scope produced a different key")
	}
}

// TestEmailReceiptIgnoresTransportHeaders proves only the Message-ID, subject
// and body participate: changing From/To/Reply-To/Received/X- headers keeps the
// identical receipt key and public request.
func TestEmailReceiptIgnoresTransportHeaders(t *testing.T) {
	const messageID = "<p26-invariant@example.test>"
	message := func(from, to, replyTo, received, trace string) string {
		return "Message-ID: " + messageID + "\r\n" +
			"Subject: Invariant subject\r\n" +
			"From: " + from + "\r\n" +
			"To: " + to + "\r\n" +
			"Reply-To: " + replyTo + "\r\n" +
			"Received: " + received + "\r\n" +
			"X-Trace: " + trace + "\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" +
			"invariant body\r\n"
	}
	first := newEmailFakeHandler()
	second := newEmailFakeHandler()
	firstMessage := message("first@example.test", "intake@kodama.test", "first-reply@example.test",
		"from relay-a.example.test by intake.example.test", "trace-a")
	secondMessage := message("second@example.test", "another@kodama.test", "second-reply@example.test",
		"from relay-b.example.test by intake.example.test", "trace-b")
	if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(first), strings.NewReader(firstMessage)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(second), strings.NewReader(secondMessage)); err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	firstCalls := first.recorded()
	secondCalls := second.recorded()
	if len(firstCalls) != 1 || len(secondCalls) != 1 {
		t.Fatalf("public invocations = %d/%d; want one each", len(firstCalls), len(secondCalls))
	}
	if firstCalls[0].invocation.Request.IdempotencyKey != secondCalls[0].invocation.Request.IdempotencyKey {
		t.Fatalf("transport headers changed the receipt key: %q != %q",
			firstCalls[0].invocation.Request.IdempotencyKey, secondCalls[0].invocation.Request.IdempotencyKey)
	}
	if !bytes.Equal(firstCalls[0].invocation.Request.Input, secondCalls[0].invocation.Request.Input) {
		t.Fatalf("transport headers changed the public request:\nfirst:  %s\nsecond: %s",
			firstCalls[0].invocation.Request.Input, secondCalls[0].invocation.Request.Input)
	}
}

// TestEmailRefusalsNeverEchoMessageContent proves every refusal path reports
// fixed text: neither the returned error nor any wrapped error may contain a
// sentinel placed in the offending message content.
func TestEmailRefusalsNeverEchoMessageContent(t *testing.T) {
	// The header-count bound is refused after the header map exists, so put the
	// sentinel in both header names and the subject to catch an echo of either.
	headerEchoFields := []string{
		"Message-ID: <p26-echo-header-count@example.test>",
		"Subject: " + emailEchoSentinel + " subject",
		"From: reporter@example.test",
		"To: intake@kodama.test",
		"Content-Type: text/plain; charset=utf-8",
	}
	for index := range services.MaxEmailHeaderCount {
		headerEchoFields = append(headerEchoFields, fmt.Sprintf("X-%s-%03d: value", emailEchoSentinel, index))
	}
	headerEchoMessage := strings.Join(headerEchoFields, "\r\n") + "\r\n\r\nbody\r\n"

	cases := []struct {
		name    string
		message string
		want    error
	}{
		{
			name: "header line without a colon",
			message: "Message-ID: <p26-echo-header@example.test>\r\n" +
				"Subject: echo\r\n" +
				emailEchoSentinel + "-no-colon here\r\n" +
				"\r\nbody\r\n",
			want: services.ErrEmailMalformed,
		},
		{
			name: "body run-on parsed as a header",
			message: "Message-ID: <p26-echo-runon@example.test>\r\n" +
				"Subject: echo\r\n" +
				emailEchoSentinel + "-body run-on\r\n",
			want: services.ErrEmailMalformed,
		},
		{
			name: "NUL in subject",
			message: "Message-ID: <p26-echo-nul@example.test>\r\n" +
				"Subject: a" + emailEchoSentinel + "\x00b\r\n" +
				"\r\nbody\r\n",
			want: services.ErrEmailMalformed,
		},
		{
			name: "bad content type",
			message: "Message-ID: <p26-echo-type@example.test>\r\n" +
				"Subject: echo\r\n" +
				"Content-Type: " + emailEchoSentinel + "/plain\r\n" +
				"\r\nbody\r\n",
			want: services.ErrEmailMalformed,
		},
		{
			name: "unparseable content type",
			message: "Message-ID: <p26-echo-type-parse@example.test>\r\n" +
				"Subject: echo\r\n" +
				"Content-Type: text/plain; charset=\"" + emailEchoSentinel + "\r\n" +
				"\r\nbody\r\n",
			want: services.ErrEmailMalformed,
		},
		{
			name: "Oversize message with a sentinel subject",
			message: emailMessage("<p26-echo-oversize@example.test>", emailEchoSentinel+" subject", "body\r\n",
				"X-Padding: "+strings.Repeat("p", services.MaxEmailMessageBytes)),
			want: services.ErrEmailOversize,
		},
		{
			name: "oversize Message-ID carrying the sentinel",
			message: emailMessage("<"+emailEchoSentinel+strings.Repeat("a", services.MaxEmailMessageIDBytes)+"@example.test>",
				"echo", "body\r\n"),
			want: services.ErrEmailOversize,
		},
		{
			name:    "header count oversize carrying the sentinel",
			message: headerEchoMessage,
			want:    services.ErrEmailOversize,
		},
		{
			name: "oversize body with a sentinel subject and sentinel body",
			message: emailMessage("<p26-echo-body-oversize@example.test>", emailEchoSentinel+" subject",
				strings.Repeat("b", services.MaxEmailBodyBytes)+emailEchoSentinel),
			want: services.ErrEmailOversize,
		},
		{
			name: "multipart message",
			message: "Message-ID: <p26-echo-multipart@example.test>\r\n" +
				"Subject: " + emailEchoSentinel + "\r\n" +
				"Content-Type: multipart/mixed; boundary=\"" + emailEchoSentinel + "\"\r\n" +
				"\r\n--" + emailEchoSentinel + "--\r\n",
			want: services.ErrEmailAttachment,
		},
		{
			name: "declared attachment",
			message: "Message-ID: <p26-echo-attachment@example.test>\r\n" +
				"Subject: echo\r\n" +
				"Content-Type: text/plain\r\n" +
				"Content-Disposition: attachment; filename=\"" + emailEchoSentinel + ".bin\"\r\n" +
				"\r\nbody\r\n",
			want: services.ErrEmailAttachment,
		},
		{
			name: "duplicate message identity",
			message: "Message-ID: <" + emailEchoSentinel + "@example.test>\r\n" +
				"Message-ID: <other@example.test>\r\n" +
				"Subject: echo\r\n\r\nbody\r\n",
			want: services.ErrEmailMalformed,
		},
		{
			name:    "missing message identity",
			message: "Subject: " + emailEchoSentinel + "\r\n\r\nbody\r\n",
			want:    services.ErrEmailMalformed,
		},
		{
			name: "invalid message identity characters",
			message: "Message-ID: <" + emailEchoSentinel + " here>\r\n" +
				"Subject: echo\r\n\r\nbody\r\n",
			want: services.ErrEmailMalformed,
		},
		{
			name: "invalid UTF-8 in message identity",
			message: "Message-ID: <" + emailEchoSentinel + "\xff@example.test>\r\n" +
				"Subject: echo\r\n\r\nbody\r\n",
			want: services.ErrEmailMalformed,
		},
		{
			name: "invalid UTF-8 in subject",
			message: "Message-ID: <p26-echo-utf8@example.test>\r\n" +
				"Subject: " + emailEchoSentinel + "\xff\r\n" +
				"\r\nbody\r\n",
			want: services.ErrEmailMalformed,
		},
		{
			name:    "invalid UTF-8 in body",
			message: emailMessage("<p26-echo-utf8-body@example.test>", "echo", emailEchoSentinel+"\xff\r\n"),
			want:    services.ErrEmailMalformed,
		},
		{
			name:    "NUL in body",
			message: emailMessage("<p26-echo-nul-body@example.test>", "echo", "before\x00"+emailEchoSentinel+"\r\n"),
			want:    services.ErrEmailMalformed,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := newEmailFakeHandler()
			_, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(testCase.message))
			if err == nil {
				t.Fatalf("IntakeEmail accepted %s", testCase.name)
			}
			if !errors.Is(err, testCase.want) {
				t.Fatalf("%s error = %v; want %v", testCase.name, err, testCase.want)
			}
			for current := err; current != nil; current = errors.Unwrap(current) {
				if strings.Contains(current.Error(), emailEchoSentinel) {
					t.Fatalf("%s refusal echoes message content: %q", testCase.name, current.Error())
				}
			}
			if got := handler.invocationCount(); got != 0 {
				t.Fatalf("%s: public invocations = %d; want none", testCase.name, got)
			}
		})
	}
}

// TestEmailPublicCallFailureNeverEchoesMessageContent proves the handler-failure
// pass-through reports only the handler's own fixed error (or the fixed changed
// content refusal), never message-derived text, even when the subject, body and
// Message-ID all carry the sentinel.
func TestEmailPublicCallFailureNeverEchoesMessageContent(t *testing.T) {
	const fixedHandlerFailure = "p26 fixed handler refusal"
	message := emailMessage("<"+emailEchoSentinel+"@example.test>",
		emailEchoSentinel+" subject", emailEchoSentinel+" body\r\n")
	cases := []struct {
		name    string
		failure error
		want    error
	}{
		{
			name:    "non-conflict handler failure",
			failure: errors.New(fixedHandlerFailure),
		},
		{
			name: "conflict handler failure",
			failure: &api.Error{
				Code:    api.CodeChangedIdempotencyContent,
				Message: fixedHandlerFailure,
			},
			want: services.ErrEmailContentChanged,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := newEmailFakeHandler()
			handler.failWith = testCase.failure
			_, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(message))
			if err == nil {
				t.Fatalf("IntakeEmail succeeded although the public call %s", testCase.name)
			}
			if testCase.want != nil && !errors.Is(err, testCase.want) {
				t.Fatalf("%s error = %v; want %v", testCase.name, err, testCase.want)
			}
			if testCase.want == nil && !strings.Contains(err.Error(), fixedHandlerFailure) {
				t.Fatalf("%s error = %q; want the handler's own fixed error %q",
					testCase.name, err.Error(), fixedHandlerFailure)
			}
			for current := err; current != nil; current = errors.Unwrap(current) {
				if strings.Contains(current.Error(), emailEchoSentinel) {
					t.Fatalf("%s public-call refusal echoes message content: %q", testCase.name, current.Error())
				}
			}
			if got := handler.invocationCount(); got != 1 {
				t.Fatalf("%s: public invocations = %d; want the one failed call", testCase.name, got)
			}
		})
	}
}

// cancelOnEOFReader cancels the intake context exactly when it reaches the end
// of the bounded message, so a cancellation that arrives during the local read
// and parse must still be honoured before the public boundary.
type cancelOnEOFReader struct {
	message []byte
	offset  int
	cancel  func()
}

func (reader *cancelOnEOFReader) Read(buffer []byte) (int, error) {
	if reader.offset >= len(reader.message) {
		reader.cancel()
		return 0, io.EOF
	}
	count := copy(buffer, reader.message[reader.offset:])
	reader.offset += count
	return count, nil
}

// eofCancelContext reports cancellation only through Err; Done stays nil so the
// read select always completes with the message and the post-parse Err check is
// the single barrier before the public call. The earlier subtests cover a real
// context whose Done closes before and during the read; this context makes the
// read-completes-then-cancels ordering deterministic in plain and race builds,
// so removing the post-parse check is caught in both.
type eofCancelContext struct {
	mu       sync.Mutex
	canceled bool
}

func (ctx *eofCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *eofCancelContext) Done() <-chan struct{}       { return nil }
func (ctx *eofCancelContext) Value(any) any               { return nil }

func (ctx *eofCancelContext) Err() error {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	if ctx.canceled {
		return context.Canceled
	}
	return nil
}

func (ctx *eofCancelContext) cancel() {
	ctx.mu.Lock()
	ctx.canceled = true
	ctx.mu.Unlock()
}

// TestEmailIntakeCancellationAfterReadNeverCallsPublic proves a cancellation
// that lands while the message is consumed is still checked after the parse. The
// reader cancels on EOF, after the read select has completed; the loop keeps the
// scenario cheap while every iteration must return the cancellation and reach
// no public call.
func TestEmailIntakeCancellationAfterReadNeverCallsPublic(t *testing.T) {
	const iterations = 2000
	handler := newEmailFakeHandler()
	message := emailMessage("<p26-cancel-eof@example.test>", "cancel after read", "body\r\n")
	for iteration := range iterations {
		ctx := &eofCancelContext{}
		reader := &cancelOnEOFReader{message: []byte(message), cancel: ctx.cancel}
		_, err := services.IntakeEmail(ctx, emailIntakeConfig(handler), reader)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d error = %v; want context.Canceled", iteration, err)
		}
		if got := handler.invocationCount(); got != 0 {
			t.Fatalf("iteration %d: public invocations = %d; want none after cancellation", iteration, got)
		}
	}
}

// sentinelErrorReader fails every read with an error carrying the sentinel, so
// an echo of the raw source error is visible.
type sentinelErrorReader struct{}

func (sentinelErrorReader) Read([]byte) (int, error) {
	return 0, errors.New(emailEchoSentinel + " source failure")
}

// TestEmailSourceReadFailureNeverEchoesTheSourceError proves a failing source is
// reported with the fixed read-failure text and never with the source's own
// error text.
func TestEmailSourceReadFailureNeverEchoesTheSourceError(t *testing.T) {
	handler := newEmailFakeHandler()
	_, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), sentinelErrorReader{})
	if err == nil {
		t.Fatal("IntakeEmail accepted a failing source")
	}
	if got, want := err.Error(), "email intake: message source read failed"; got != want {
		t.Fatalf("source read error = %q; want the fixed %q", got, want)
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if strings.Contains(current.Error(), emailEchoSentinel) {
			t.Fatalf("source read refusal echoes the source error: %q", current.Error())
		}
	}
	if got := handler.invocationCount(); got != 0 {
		t.Fatalf("public invocations = %d; want none after a source failure", got)
	}
}

// TestEmailRefusesNonTextPayloads pins the invalid UTF-8 and NUL refusals that
// keep the JSON-encoded request within its bounds.
func TestEmailRefusesNonTextPayloads(t *testing.T) {
	refused := []struct {
		name    string
		message string
	}{
		{name: "NUL in subject", message: emailMessage("<p26-text-nul@example.test>", "a\x00b", "body\r\n")},
		{name: "NUL in body", message: emailMessage("<p26-text-nul@example.test>", "subject", "a\x00b\r\n")},
		{name: "NUL in another header", message: emailMessage("<p26-text-nul@example.test>", "subject", "body\r\n", "X-Note: a\x00b")},
		{name: "NUL at the body bound", message: emailMessage("<p26-text-nul@example.test>", "subject",
			strings.Repeat("b", services.MaxEmailBodyBytes-1)+"\x00")},
		{name: "invalid UTF-8 in subject", message: emailMessage("<p26-text-utf8@example.test>", "a\xffb", "body\r\n")},
		{name: "invalid UTF-8 in body", message: emailMessage("<p26-text-utf8@example.test>", "subject", "a\xffb\r\n")},
		{name: "invalid UTF-8 in message identity", message: emailMessage("<p26-text-utf8-id\xff@example.test>", "subject", "body\r\n")},
		{name: "invalid UTF-8 at the body bound", message: emailMessage("<p26-text-utf8@example.test>", "subject",
			strings.Repeat("b", services.MaxEmailBodyBytes-1)+"\xff")},
	}
	for _, testCase := range refused {
		t.Run(testCase.name, func(t *testing.T) {
			handler := newEmailFakeHandler()
			_, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(testCase.message))
			if !errors.Is(err, services.ErrEmailMalformed) {
				t.Fatalf("%s error = %v; want ErrEmailMalformed", testCase.name, err)
			}
			if got := handler.invocationCount(); got != 0 {
				t.Fatalf("%s: public invocations = %d; want none", testCase.name, got)
			}
		})
	}

	// A NUL in the leading mbox envelope line is refused by the whole-raw check,
	// before the envelope is ignored, with the fixed text and no public call.
	t.Run("NUL in the envelope", func(t *testing.T) {
		handler := newEmailFakeHandler()
		_, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
			"From x\x00y\r\n"+emailMessage("<p26-text-nul-envelope@example.test>", "subject", "body\r\n"),
		))
		if !errors.Is(err, services.ErrEmailMalformed) {
			t.Fatalf("NUL in the envelope error = %v; want ErrEmailMalformed", err)
		}
		if got, want := err.Error(), "email intake: malformed message: message contains a NUL byte"; got != want {
			t.Fatalf("NUL in the envelope error text = %q; want the fixed %q", got, want)
		}
		if got := handler.invocationCount(); got != 0 {
			t.Fatalf("NUL in the envelope: public invocations = %d; want none", got)
		}
	})

	// Valid multibyte text at the body bound is accepted and keeps its encoded
	// size, so the bound is not inflated by JSON replacement.
	multibyteBody := strings.Repeat("é", services.MaxEmailBodyBytes/2)
	if len(multibyteBody) != services.MaxEmailBodyBytes {
		t.Fatalf("fixture is %d bytes; want %d", len(multibyteBody), services.MaxEmailBodyBytes)
	}
	handler := newEmailFakeHandler()
	if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
		emailMessage("<p26-text-utf8-ok@example.test>", "subject", multibyteBody),
	)); err != nil {
		t.Fatalf("valid multibyte body at the bound: %v", err)
	}
	recorded := handler.recorded()
	if len(recorded) != 1 || recorded[0].request.Description != multibyteBody {
		t.Fatalf("description did not preserve the valid multibyte body")
	}
}

// --- Canonical Message-ID, line endings, mbox envelope ---

// TestEmailMessageIDCanonicalization proves one Message-ID has exactly one
// canonical identity: surrounding transport whitespace is discarded and exactly
// one surrounding bracket pair is removed, while case is preserved and no
// repeated stripping or character-set trimming happens. The canonical value is
// used consistently by the receipt key, the submitted metadata, the returned
// result and the fallback title.
func TestEmailMessageIDCanonicalization(t *testing.T) {
	const canonical = "A@B"
	// Independently computed: sha256("email-intake\0v1\0tenant-1\0project-1\0A@B").
	const literalKey = "email-intake:v1:ff19a0fe3c5fa6fb9138583a0e24592ba24750e8f56d690a8e807605a963bfa6"

	handler := newEmailFakeHandler()
	config := services.EmailIntakeConfig{
		TenantID:   id.TenantID("tenant-1"),
		ProjectID:  id.ProjectID("project-1"),
		PipelineID: testEmailPipeline,
		Handler:    handler,
	}
	first, err := services.IntakeEmail(t.Context(), config, strings.NewReader(
		emailMessage(" \t<A@B>\t ", "s", "one\ntwo\n"),
	))
	if err != nil {
		t.Fatalf("bracketed and padded Message-ID: %v", err)
	}
	if first.MessageID != canonical {
		t.Fatalf("result message identity = %q; want the canonical %q", first.MessageID, canonical)
	}
	calls := handler.recorded()
	if len(calls) != 1 {
		t.Fatalf("public invocations = %d; want one", len(calls))
	}
	firstCall := calls[0]
	if got := firstCall.request.Metadata["email.message_id"]; got != canonical {
		t.Fatalf("metadata message id = %q; want the canonical %q", got, canonical)
	}
	if got := firstCall.invocation.Request.IdempotencyKey; got != literalKey {
		t.Fatalf("receipt key = %q; want the literal canonical key %q", got, literalKey)
	}
	if got := firstCall.request.Title; got != "s" {
		t.Fatalf("title = %q; want the subject", got)
	}

	// The plain, unbracketed spelling of the same identity is the same public
	// request: same key, byte-identical input and the same committed result.
	plain, err := services.IntakeEmail(t.Context(), config, strings.NewReader(
		emailMessage("A@B", "s", "one\ntwo\n"),
	))
	if err != nil {
		t.Fatalf("plain Message-ID: %v", err)
	}
	if plain.TaskID != first.TaskID || plain.ExecutionID != first.ExecutionID {
		t.Fatalf("plain identity converged to %s/%s; want the committed %s/%s",
			plain.TaskID, plain.ExecutionID, first.TaskID, first.ExecutionID)
	}
	if got := len(handler.commits()); got != 1 {
		t.Fatalf("committed tasks = %d; want no second task for the canonical replay", got)
	}
	calls = handler.recorded()
	if len(calls) != 2 {
		t.Fatalf("public invocations = %d; want the replay too", len(calls))
	}
	if calls[1].invocation.Request.IdempotencyKey != firstCall.invocation.Request.IdempotencyKey {
		t.Fatalf("plain identity used key %q; want %q",
			calls[1].invocation.Request.IdempotencyKey, firstCall.invocation.Request.IdempotencyKey)
	}
	if !bytes.Equal(calls[1].invocation.Request.Input, firstCall.invocation.Request.Input) {
		t.Fatalf("plain identity changed the request bytes:\nfirst:  %s\nsecond: %s",
			firstCall.invocation.Request.Input, calls[1].invocation.Request.Input)
	}

	// Case is preserved and significant: a@b is a different identity.
	lower, err := services.IntakeEmail(t.Context(), config, strings.NewReader(
		emailMessage("a@b", "s", "one\ntwo\n"),
	))
	if err != nil {
		t.Fatalf("lowercase Message-ID: %v", err)
	}
	if lower.MessageID != "a@b" {
		t.Fatalf("lowercase result identity = %q; want %q", lower.MessageID, "a@b")
	}
	calls = handler.recorded()
	lowerCall := calls[len(calls)-1]
	if lowerCall.invocation.Request.IdempotencyKey == firstCall.invocation.Request.IdempotencyKey {
		t.Fatal("lowercase identity reused the canonical receipt key")
	}
	if bytes.Equal(lowerCall.invocation.Request.Input, firstCall.invocation.Request.Input) {
		t.Fatal("lowercase identity produced the identical public request")
	}

	// Exactly one surrounding bracket pair is stripped, never repeated.
	nested, err := services.IntakeEmail(t.Context(), config, strings.NewReader(
		emailMessage("<<A@B>>", "s", "one\ntwo\n"),
	))
	if err != nil {
		t.Fatalf("doubly bracketed Message-ID: %v", err)
	}
	if nested.MessageID != "<A@B>" {
		t.Fatalf("doubly bracketed identity = %q; want one pair stripped to %q", nested.MessageID, "<A@B>")
	}
	calls = handler.recorded()
	nestedCall := calls[len(calls)-1]
	if got := nestedCall.request.Metadata["email.message_id"]; got != "<A@B>" {
		t.Fatalf("doubly bracketed metadata = %q; want %q", got, "<A@B>")
	}
	if nestedCall.invocation.Request.IdempotencyKey == firstCall.invocation.Request.IdempotencyKey {
		t.Fatal("the doubly bracketed identity collapsed to the canonical key")
	}

	// Both brackets of the one conventional pair must be present: an unpaired
	// bracket is part of the identity, not framing. Stripping each bracket
	// independently would collapse these distinct identities onto the canonical
	// one, colliding two senders onto one receipt.
	unpaired := []struct {
		name    string
		spelled string
		want    string
	}{
		{"leading bracket only", "<A@B", "<A@B"},
		{"trailing bracket only", "A@B>", "A@B>"},
	}
	unpairedKeys := make([]string, 0, len(unpaired))
	for _, testCase := range unpaired {
		result, err := services.IntakeEmail(t.Context(), config, strings.NewReader(
			emailMessage(testCase.spelled, "s", "one\ntwo\n"),
		))
		if err != nil {
			t.Fatalf("%s Message-ID: %v", testCase.name, err)
		}
		if result.MessageID != testCase.want {
			t.Fatalf("%s identity = %q; want it kept as spelled %q", testCase.name, result.MessageID, testCase.want)
		}
		calls = handler.recorded()
		call := calls[len(calls)-1]
		if got := call.request.Metadata["email.message_id"]; got != testCase.want {
			t.Fatalf("%s metadata = %q; want it kept as spelled %q", testCase.name, got, testCase.want)
		}
		if call.invocation.Request.IdempotencyKey == firstCall.invocation.Request.IdempotencyKey {
			t.Fatalf("%s collapsed onto the canonical receipt key", testCase.name)
		}
		unpairedKeys = append(unpairedKeys, call.invocation.Request.IdempotencyKey)
	}
	if unpairedKeys[0] == unpairedKeys[1] {
		t.Fatalf("the unpaired bracket spellings share receipt key %q; want distinct identities", unpairedKeys[0])
	}

	// Transport whitespace is discarded before framing, so a no-break space in
	// front of the bracket pair is not part of the identity. (textproto trims
	// ASCII space and tab itself, so only a non-ASCII space pins
	// strings.TrimSpace here.)
	nbsp, err := services.IntakeEmail(t.Context(), config, strings.NewReader(
		emailMessage("\u00a0<A@B>", "s", "one\ntwo\n"),
	))
	if err != nil {
		t.Fatalf("no-break-space-padded Message-ID: %v", err)
	}
	if nbsp.MessageID != canonical {
		t.Fatalf("no-break-space identity = %q; want the canonical %q", nbsp.MessageID, canonical)
	}
	calls = handler.recorded()
	nbspCall := calls[len(calls)-1]
	if got := nbspCall.request.Metadata["email.message_id"]; got != canonical {
		t.Fatalf("no-break-space metadata = %q; want the canonical %q", got, canonical)
	}
	if nbspCall.invocation.Request.IdempotencyKey != firstCall.invocation.Request.IdempotencyKey {
		t.Fatalf("no-break-space identity used key %q; want the canonical %q",
			nbspCall.invocation.Request.IdempotencyKey, firstCall.invocation.Request.IdempotencyKey)
	}
	if !bytes.Equal(nbspCall.invocation.Request.Input, firstCall.invocation.Request.Input) {
		t.Fatalf("no-break-space identity changed the request bytes:\nfirst: %s\nnbsp:  %s",
			firstCall.invocation.Request.Input, nbspCall.invocation.Request.Input)
	}

	// An empty bracket pair has no identity token and is refused locally.
	before := handler.invocationCount()
	if _, err := services.IntakeEmail(t.Context(), config, strings.NewReader(
		emailMessage("<>", "s", "one\ntwo\n"),
	)); !errors.Is(err, services.ErrEmailMalformed) {
		t.Fatalf("empty bracket pair error = %v; want ErrEmailMalformed", err)
	}
	if got := handler.invocationCount(); got != before {
		t.Fatalf("public invocations = %d after the empty identity; want %d", got, before)
	}
}

// TestEmailFallbackTitleUsesTheCanonicalIdentity proves a subject-less message
// titles the task from the canonical identity without its transport brackets,
// and that both wire spellings still submit the byte-identical public request.
func TestEmailFallbackTitleUsesTheCanonicalIdentity(t *testing.T) {
	const canonical = "A@B"
	subjectless := func(messageID string) string {
		return "Message-ID: " + messageID + "\r\n" +
			"From: reporter@example.test\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\nfallback title body\r\n"
	}
	handler := newEmailFakeHandler()
	config := services.EmailIntakeConfig{
		TenantID:   id.TenantID("tenant-1"),
		ProjectID:  id.ProjectID("project-1"),
		PipelineID: testEmailPipeline,
		Handler:    handler,
	}
	for index, spelled := range []string{"<" + canonical + ">", canonical} {
		if _, err := services.IntakeEmail(t.Context(), config, strings.NewReader(subjectless(spelled))); err != nil {
			t.Fatalf("subject-less spelling %d (%q): %v", index, spelled, err)
		}
	}
	calls := handler.recorded()
	if len(calls) != 2 {
		t.Fatalf("public invocations = %d; want the bracketed and plain spellings", len(calls))
	}
	for index, call := range calls {
		if got, want := call.request.Title, "Email "+canonical; got != want {
			t.Fatalf("spelling %d title = %q; want the canonical fallback %q", index, got, want)
		}
	}
	if calls[0].invocation.Request.IdempotencyKey != calls[1].invocation.Request.IdempotencyKey {
		t.Fatalf("the two spellings used keys %q and %q; want one canonical key",
			calls[0].invocation.Request.IdempotencyKey, calls[1].invocation.Request.IdempotencyKey)
	}
	if !bytes.Equal(calls[0].invocation.Request.Input, calls[1].invocation.Request.Input) {
		t.Fatalf("the two spellings submitted different requests:\nfirst: %s\nplain: %s",
			calls[0].invocation.Request.Input, calls[1].invocation.Request.Input)
	}
	if got := len(handler.commits()); got != 1 {
		t.Fatalf("committed tasks = %d; want the one canonical task", got)
	}
}

// TestEmailCRLFRedeliveryUsesSameReceipt proves CRLF and LF spellings of one
// message converge on the identical receipt request, while changed content and
// bare-CR content stay distinct. The digest and the submitted description both
// come from the normalized body, so normalizing only one of them is visible.
func TestEmailCRLFRedeliveryUsesSameReceipt(t *testing.T) {
	const messageID = "<p26-crlf@example.test>"
	const subject = "s"
	// Independently computed for subject "s" and the normalized body "one\ntwo\n".
	const wantDigest = "fad4c1bbc7bfa2177dc95fb74262f014a1b1b3f279204a9095c3951b1dfdcf56"
	// Independently computed for subject "s" and the normalized body "one\rtwo\n".
	const bareCRDigest = "bc1774ac465598cdaa1eb34eeec56fc703ec4ea3dd2db7a450b6ec596498984b"

	handler := newEmailFakeHandler()
	handler.loseNext = true
	if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
		emailMessage(messageID, subject, "one\r\ntwo\r\n"),
	)); err == nil {
		t.Fatal("IntakeEmail reported success although the response was lost")
	}
	commits := handler.commits()
	if len(commits) != 1 {
		t.Fatalf("committed tasks = %d; want the one committed by the lost response", len(commits))
	}
	committed := commits[0]
	first := handler.recorded()[0]
	if got := first.request.Description; got != "one\ntwo" {
		t.Fatalf("description = %q; want the normalized body %q", got, "one\ntwo")
	}
	if got := first.request.Metadata["email.content_sha256"]; got != wantDigest {
		t.Fatalf("content digest = %q; want the literal normalized digest %q", got, wantDigest)
	}

	retry, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
		emailMessage(messageID, subject, "one\ntwo\n"),
	))
	if err != nil {
		t.Fatalf("LF redelivery: %v", err)
	}
	if retry.TaskID != committed.TaskID || retry.ExecutionID != committed.ExecutionID {
		t.Fatalf("LF redelivery converged to %s/%s; want the committed %s/%s",
			retry.TaskID, retry.ExecutionID, committed.TaskID, committed.ExecutionID)
	}
	if got := len(handler.commits()); got != 1 {
		t.Fatalf("committed tasks = %d; want no second task after the LF redelivery", got)
	}
	second := handler.recorded()[1]
	if second.invocation.Request.IdempotencyKey != first.invocation.Request.IdempotencyKey {
		t.Fatalf("LF redelivery used key %q; want %q",
			second.invocation.Request.IdempotencyKey, first.invocation.Request.IdempotencyKey)
	}
	if !bytes.Equal(second.invocation.Request.Input, first.invocation.Request.Input) {
		t.Fatalf("LF redelivery changed the request bytes:\nCRLF: %s\nLF:   %s",
			first.invocation.Request.Input, second.invocation.Request.Input)
	}

	// Changed subject and changed body remain distinct content under one
	// identity.
	for _, changed := range []struct {
		name    string
		subject string
		body    string
	}{
		{"changed subject", "t", "one\ntwo\n"},
		{"changed body", subject, "one\ntwo!\n"},
	} {
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
			emailMessage(messageID, changed.subject, changed.body),
		)); !errors.Is(err, services.ErrEmailContentChanged) {
			t.Fatalf("%s error = %v; want ErrEmailContentChanged", changed.name, err)
		}
	}

	// A bare CR is content, not a line ending: it is preserved, and the digest
	// covers the normalized body rather than a CR-stripped or trimmed one.
	if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
		emailMessage(messageID, subject, "one\rtwo\r\n"),
	)); !errors.Is(err, services.ErrEmailContentChanged) {
		t.Fatalf("bare-CR content error = %v; want ErrEmailContentChanged", err)
	}
	recorded := handler.recorded()
	bareCall := recorded[len(recorded)-1]
	if got := bareCall.request.Description; got != "one\rtwo" {
		t.Fatalf("bare-CR description = %q; want %q", got, "one\rtwo")
	}
	if got := bareCall.request.Metadata["email.content_sha256"]; got != bareCRDigest {
		t.Fatalf("bare-CR digest = %q; want the literal %q", got, bareCRDigest)
	}
}

// TestEmailMboxEnvelopeIsIgnoredExactlyOnce proves zero or one exact byte-zero
// `From ` envelope line (LF or CRLF terminated) is ignored, a second one is
// refused, and body lines that begin `From ` or `>From ` survive untouched.
func TestEmailMboxEnvelopeIsIgnoredExactlyOnce(t *testing.T) {
	const messageID = "<p26-mbox@example.test>"
	base := emailMessage(messageID, "mbox", "From the queue\n>From the archive\n")
	const envelopeCRLF = "From reporter@example.test\r\n"
	const envelopeLF = "From reporter@example.test\n"

	handler := newEmailFakeHandler()
	variants := []string{base, envelopeLF + base, envelopeCRLF + base}
	var first services.EmailIntakeResult
	for index, message := range variants {
		result, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(message))
		if err != nil {
			t.Fatalf("variant %d: %v", index, err)
		}
		if index == 0 {
			first = result
			continue
		}
		if result.TaskID != first.TaskID || result.ExecutionID != first.ExecutionID {
			t.Fatalf("variant %d converged to %s/%s; want %s/%s",
				index, result.TaskID, result.ExecutionID, first.TaskID, first.ExecutionID)
		}
	}
	if got := len(handler.commits()); got != 1 {
		t.Fatalf("committed tasks = %d; want one for the equivalent envelopes", got)
	}
	recorded := handler.recorded()
	if len(recorded) != 3 {
		t.Fatalf("public invocations = %d; want three", len(recorded))
	}
	for index, call := range recorded[1:] {
		if call.invocation.Request.IdempotencyKey != recorded[0].invocation.Request.IdempotencyKey {
			t.Fatalf("variant %d used key %q; want %q",
				index+1, call.invocation.Request.IdempotencyKey, recorded[0].invocation.Request.IdempotencyKey)
		}
		if !bytes.Equal(call.invocation.Request.Input, recorded[0].invocation.Request.Input) {
			t.Fatalf("variant %d changed the request bytes:\nfirst: %s\nthis:  %s",
				index+1, recorded[0].invocation.Request.Input, call.invocation.Request.Input)
		}
	}
	// Body lines that look like envelope lines are content and are preserved.
	if got := recorded[0].request.Description; got != "From the queue\n>From the archive" {
		t.Fatalf("description = %q; want the body's From lines preserved", got)
	}

	// A second leading envelope line is ambiguous and refused with no public
	// call at all.
	before := handler.invocationCount()
	if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
		envelopeLF+envelopeCRLF+base,
	)); !errors.Is(err, services.ErrEmailMalformed) {
		t.Fatalf("second envelope error = %v; want ErrEmailMalformed", err)
	}
	if got := handler.invocationCount(); got != before {
		t.Fatalf("public invocations = %d after the refused second envelope; want %d", got, before)
	}

	// Only the exact byte-zero `From ` spelling is an envelope. A case-insensitive
	// match, a bare `From` prefix and a `From ` line after the first header are
	// all malformed headers and must be refused before any public call.
	nearMisses := []struct {
		name    string
		message string
	}{
		{"lowercase from is not an envelope", "from x\r\n" + base},
		{"uppercase FROM is not an envelope", "FROM x\r\n" + base},
		{"From prefix without the space is not an envelope", "Fromage x\r\n" + base},
		{"From after the first header is not an envelope", "Message-ID: <A@B>\r\nFrom x\r\nSubject: s\r\n\r\nb"},
	}
	for _, nearMiss := range nearMisses {
		t.Run(nearMiss.name, func(t *testing.T) {
			handler := newEmailFakeHandler()
			result, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(nearMiss.message))
			if !errors.Is(err, services.ErrEmailMalformed) {
				t.Fatalf("%s error = %v; want ErrEmailMalformed", nearMiss.name, err)
			}
			if result != (services.EmailIntakeResult{}) {
				t.Fatalf("%s returned %+v; want no result", nearMiss.name, result)
			}
			if got := handler.invocationCount(); got != 0 {
				t.Fatalf("%s: public invocations = %d; want none", nearMiss.name, got)
			}
		})
	}
}

// TestEmailNormalizationPreservesRawBounds proves every bound is applied to the
// raw bytes before brackets or line endings are normalized.
func TestEmailNormalizationPreservesRawBounds(t *testing.T) {
	t.Run("CRLF-heavy body over the body bound", func(t *testing.T) {
		rawBody := strings.Repeat("a\r\n", services.MaxEmailBodyBytes/2)
		normalized := strings.ReplaceAll(rawBody, "\r\n", "\n")
		if len(rawBody) <= services.MaxEmailBodyBytes {
			t.Fatalf("fixture raw body is %d bytes; want over %d", len(rawBody), services.MaxEmailBodyBytes)
		}
		if len(normalized) > services.MaxEmailBodyBytes {
			t.Fatalf("fixture normalized body is %d bytes; want it inside the bound", len(normalized))
		}
		handler := newEmailFakeHandler()
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
			emailMessage("<p26-crlf-bound@example.test>", "bound", rawBody),
		)); !errors.Is(err, services.ErrEmailOversize) {
			t.Fatalf("CRLF-heavy body error = %v; want ErrEmailOversize on the raw bytes", err)
		}
		if got := handler.invocationCount(); got != 0 {
			t.Fatalf("public invocations = %d; want none", got)
		}
	})

	t.Run("envelope counts toward the raw message bound", func(t *testing.T) {
		atLimit := rawEmailMessageOfSize(t, services.MaxEmailMessageBytes)
		handler := newEmailFakeHandler()
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
			"From envelope@example.test\n"+atLimit,
		)); !errors.Is(err, services.ErrEmailOversize) {
			t.Fatalf("envelope over the raw bound error = %v; want ErrEmailOversize", err)
		}
		if got := handler.invocationCount(); got != 0 {
			t.Fatalf("public invocations = %d; want none", got)
		}
	})

	t.Run("bracketed identity over the identity bound", func(t *testing.T) {
		overLimit := "<" + strings.Repeat("a", services.MaxEmailMessageIDBytes-1) + ">"
		if len(overLimit) != services.MaxEmailMessageIDBytes+1 {
			t.Fatalf("fixture identity is %d bytes; want %d", len(overLimit), services.MaxEmailMessageIDBytes+1)
		}
		handler := newEmailFakeHandler()
		if _, err := services.IntakeEmail(t.Context(), emailIntakeConfig(handler), strings.NewReader(
			emailMessage(overLimit, "bound", "body\r\n"),
		)); !errors.Is(err, services.ErrEmailOversize) {
			t.Fatalf("bracketed identity of %d bytes error = %v; want ErrEmailOversize", len(overLimit), err)
		}
		if got := handler.invocationCount(); got != 0 {
			t.Fatalf("public invocations = %d; want none", got)
		}
	})
}
