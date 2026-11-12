package main

// Contract tests for the concrete `kodama services` email-intake entry.
//
// The one-shot intake reuses the `services` subcommand rather than
// adding a top-level command: `--kind email` plus `--message` selects one
// bounded RFC822 message, with `--tenant`, `--project` and `--pipeline` as the
// trusted configuration. Without `--message` the command keeps its
// long-running adapter behaviour.
//
// The message source is read before any public call, so an unreadable source is
// a local refusal, never an ambiguous mutation.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

const cliEmailMessage = "Message-ID: <cli-intake@example.test>\r\n" +
	"Subject: CLI intake\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\nRun the diagnostic.\r\n"

func writeCLIEmailMessage(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "message.eml")
	if err := os.WriteFile(path, []byte(cliEmailMessage), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestServicesEmailIntakeRequiresPipeline pins the email-intake flag shape.
func TestServicesEmailIntakeRequiresPipeline(t *testing.T) {
	message := writeCLIEmailMessage(t)
	var stderr lockedBuffer
	code := runServicesEntry(context.Background(), []string{
		"--kind", "email",
		"--tenant", "tenant-1",
		"--project", "project-1",
		"--message", message,
	}, &lockedBuffer{}, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "email intake requires --pipeline") {
		t.Fatalf("stderr = %q; want the email intake pipeline requirement", stderr.String())
	}
}

// TestServicesEmailIntakeRefusesForeignKind proves --message is only an email
// intake selector and never silently reinterprets another adapter kind.
func TestServicesEmailIntakeRefusesForeignKind(t *testing.T) {
	message := writeCLIEmailMessage(t)
	var stderr lockedBuffer
	code := runServicesEntry(context.Background(), []string{
		"--kind", "responder",
		"--service", "responder-svc",
		"--tenant", "tenant-1",
		"--project", "project-1",
		"--pipeline", "pipeline-1",
		"--message", message,
	}, &lockedBuffer{}, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "email intake requires --kind email") {
		t.Fatalf("stderr = %q; want the email intake kind refusal", stderr.String())
	}
}

// TestServicesEmailIntakeReadsTheMessageBeforeAnyPublicCall proves a missing
// source is refused locally, before the public transport is used.
func TestServicesEmailIntakeReadsTheMessageBeforeAnyPublicCall(t *testing.T) {
	var stderr lockedBuffer
	code := runServicesEntry(context.Background(), []string{
		"--kind", "email",
		"--tenant", "tenant-1",
		"--project", "project-1",
		"--pipeline", "pipeline-1",
		"--message", filepath.Join(t.TempDir(), "absent.eml"),
	}, &lockedBuffer{}, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
	if !strings.Contains(stderr.String(), "email message") {
		t.Fatalf("stderr = %q; want the email message source refusal", stderr.String())
	}
}

// TestServicesEmailIntakeNeedsNoScanningServiceIdentity proves the one-shot
// intake needs only the mounted credential: it does not require the scanning
// adapter's --service flag, and it reaches the public transport construction.
func TestServicesEmailIntakeNeedsNoScanningServiceIdentity(t *testing.T) {
	message := writeCLIEmailMessage(t)
	var stderr lockedBuffer
	code := runServicesEntry(context.Background(), []string{
		"--kind", "email",
		"--tenant", "tenant-1",
		"--project", "project-1",
		"--pipeline", "pipeline-1",
		"--message", message,
	}, &lockedBuffer{}, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d; want 2", code)
	}
	if !strings.Contains(stderr.String(), "public base URL is required") {
		t.Fatalf("stderr = %q; want the public transport requirement", stderr.String())
	}
}

// cliEmailSentinel marks bytes the CLI must never print back: refusals report
// fixed text, never message content.
const cliEmailSentinel = "CLI-MSG-SECRET"

// TestServicesEmailIntakeStopsWhenCancelled proves the one-shot intake honours a
// cancelled context while stdin is blocking instead of outliving SIGTERM.
func TestServicesEmailIntakeStopsWhenCancelled(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	originalStdin := os.Stdin
	os.Stdin = readEnd
	t.Cleanup(func() {
		os.Stdin = originalStdin
		_ = readEnd.Close()
		_ = writeEnd.Close()
	})
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte("unused"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- runServicesEntry(ctx, []string{
			"--kind", "email",
			"--tenant", "tenant-1",
			"--project", "project-1",
			"--pipeline", "pipeline-1",
			"--message", "-",
			"--base-url", "http://127.0.0.1:1",
			"--credential-file", credential,
		}, &lockedBuffer{}, &stderr)
	}()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("exit code = %d; want 1", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the one-shot intake did not stop after the context was cancelled")
	}
	if !strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("stderr = %q; want the cancellation refusal", stderr.String())
	}
}

// TestServicesEmailIntakeNeverPrintsMessageContent proves the CLI refusal for a
// malformed, oversize or publicly failed message reports fixed text, never the
// offending message content.
func TestServicesEmailIntakeNeverPrintsMessageContent(t *testing.T) {
	cases := []struct {
		name         string
		message      string
		wantFixed    string
		wantExitCode int
	}{
		{
			name: "malformed header line",
			message: "Message-ID: <cli-echo@example.test>\r\n" +
				"Subject: echo\r\n" +
				cliEmailSentinel + "-no-colon here\r\n" +
				"\r\nbody\r\n",
			wantFixed:    "kodama: email intake: malformed message",
			wantExitCode: 1,
		},
		{
			name: "oversize message identity carrying the sentinel",
			message: "Message-ID: <" + cliEmailSentinel + strings.Repeat("a", 256) + "@example.test>\r\n" +
				"Subject: echo\r\n" +
				"\r\nbody\r\n",
			wantFixed:    "kodama: email intake: message exceeds a bound",
			wantExitCode: 1,
		},
		{
			name: "second leading envelope line",
			message: "From first@example.test\r\n" +
				"From second@example.test\r\n" +
				"Message-ID: <" + cliEmailSentinel + "@example.test>\r\n" +
				"Subject: " + cliEmailSentinel + "\r\n" +
				"Content-Type: text/plain; charset=utf-8\r\n" +
				"\r\n" + cliEmailSentinel + "\r\n",
			wantFixed:    "kodama: email intake: malformed message",
			wantExitCode: 1,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "message.eml")
			if err := os.WriteFile(path, []byte(testCase.message), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			credential := filepath.Join(t.TempDir(), "credential")
			if err := os.WriteFile(credential, []byte("unused"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			var stdout, stderr lockedBuffer
			code := runServicesEntry(context.Background(), []string{
				"--kind", "email",
				"--tenant", "tenant-1",
				"--project", "project-1",
				"--pipeline", "pipeline-1",
				"--message", path,
				"--base-url", "http://127.0.0.1:1",
				"--credential-file", credential,
			}, &stdout, &stderr)
			if code != testCase.wantExitCode {
				t.Fatalf("exit code = %d; want %d (stderr %q)", code, testCase.wantExitCode, stderr.String())
			}
			if got := stdout.String(); got != "" {
				t.Fatalf("refusal wrote success output to stdout: %q", got)
			}
			if strings.Contains(stdout.String(), cliEmailSentinel) || strings.Contains(stderr.String(), cliEmailSentinel) {
				t.Fatalf("refusal echoes message content: stdout %q stderr %q", stdout.String(), stderr.String())
			}
			if testCase.wantFixed != "" && !strings.Contains(stderr.String(), testCase.wantFixed) {
				t.Fatalf("stderr = %q; want the fixed refusal %q", stderr.String(), testCase.wantFixed)
			}
		})
	}

	t.Run("public call failure", func(t *testing.T) {
		edge := newCLIEmailEdge()
		edge.fail = true
		message := "Message-ID: <" + cliEmailSentinel + "@example.test>\r\n" +
			"Subject: " + cliEmailSentinel + "\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" + cliEmailSentinel + "\r\n"
		var stdout, stderr lockedBuffer
		if code := runCLIEmailIntake(t, edge, message, &stdout, &stderr); code != 1 {
			t.Fatalf("exit code = %d; want 1 (stderr %q)", code, stderr.String())
		}
		if got := stdout.String(); got != "" {
			t.Fatalf("failed call wrote success output to stdout: %q", got)
		}
		for _, output := range []string{stdout.String(), stderr.String()} {
			for _, sentinel := range []string{cliEmailSentinel, cliCredentialSentinel} {
				if strings.Contains(output, sentinel) {
					t.Fatalf("public-call failure echoes %q: %q", sentinel, output)
				}
			}
		}
	})

	t.Run("successful result", func(t *testing.T) {
		edge := newCLIEmailEdge()
		message := "From envelope-" + cliEmailSentinel + "@example.test\r\n" +
			"Message-ID: <" + cliEmailSentinel + "@example.test>\r\n" +
			"Subject: " + cliEmailSentinel + "\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" + cliEmailSentinel + "\r\n"
		var stdout, stderr lockedBuffer
		if code := runCLIEmailIntake(t, edge, message, &stdout, &stderr); code != 0 {
			t.Fatalf("exit code = %d; want 0 (stderr %q)", code, stderr.String())
		}
		if got, want := stdout.String(), "task_id=task-cli-1 execution_id=execution-cli-1\n"; got != want {
			t.Fatalf("stdout = %q; want exactly %q", got, want)
		}
		if got := stderr.String(); got != "" {
			t.Fatalf("stderr = %q; want empty", got)
		}
		for _, output := range []string{stdout.String(), stderr.String()} {
			for _, sentinel := range []string{cliEmailSentinel, cliCredentialSentinel} {
				if strings.Contains(output, sentinel) {
					t.Fatalf("success output echoes %q: %q", sentinel, output)
				}
			}
		}
	})
}

// --- CLI success output and its exact leakage boundary ---

// cliEmailReceipt is one committed public receipt: the exact submitted input
// bytes and the served result they replay.
type cliEmailReceipt struct {
	input  []byte
	result api.Result
}

// cliEmailEdge is an in-process public REST edge over the real operation codecs
// and the public route table. It commits one receipt per idempotency key and
// replays it for byte-identical input, like the served edge.
type cliEmailEdge struct {
	mu       sync.Mutex
	receipts map[string]cliEmailReceipt
	calls    int
	commits  int
	fail     bool
}

func newCLIEmailEdge() *cliEmailEdge {
	return &cliEmailEdge{receipts: make(map[string]cliEmailReceipt)}
}

func (edge *cliEmailEdge) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	route, params, ok := api.Match(api.Method(request.Method), request.URL.Path)
	if !ok {
		cliWritePublicError(writer, api.CodeNotFound, "no operation is bound to this method and path")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, api.MaxRequestBytes+1))
	if err != nil {
		cliWritePublicError(writer, api.CodeInvalidInput, "request body is unreadable")
		return
	}
	envelope, err := api.DecodeRequest(body)
	if err != nil {
		cliWritePublicError(writer, api.CodeInvalidInput, "request envelope is invalid")
		return
	}
	if _, err := api.DecodeRESTRequest(route.Operation, envelope.Input, params); err != nil {
		cliWritePublicError(writer, api.CodeInvalidInput, "request input is invalid")
		return
	}

	edge.mu.Lock()
	edge.calls++
	if edge.fail {
		edge.mu.Unlock()
		cliWritePublicError(writer, api.CodeInternalFailure, "the edge failed")
		return
	}
	if receipt, found := edge.receipts[envelope.IdempotencyKey]; found {
		if !bytes.Equal(receipt.input, envelope.Input) {
			edge.mu.Unlock()
			cliWritePublicError(writer, api.CodeChangedIdempotencyContent,
				"caller key was reused with changed content")
			return
		}
		result := receipt.result
		result.Replayed = true
		edge.mu.Unlock()
		cliWriteResult(writer, result)
		return
	}

	edge.commits++
	codec, ok := cliOperationCodec(route.Operation)
	if !ok {
		edge.mu.Unlock()
		cliWritePublicError(writer, api.CodeInternalFailure, "no codec")
		return
	}
	resultBody, err := codec.EncodeResult(api.TaskCreateAndStartResult{
		TaskID:           fmt.Sprintf("task-cli-%d", edge.commits),
		ExecutionID:      fmt.Sprintf("execution-cli-%d", edge.commits),
		LifecycleVersion: 1,
	})
	if err != nil {
		edge.mu.Unlock()
		cliWritePublicError(writer, api.CodeInternalFailure, "result is not encodable")
		return
	}
	result := api.Result{Operation: route.Operation, Body: resultBody}
	edge.receipts[envelope.IdempotencyKey] = cliEmailReceipt{
		input:  append([]byte(nil), envelope.Input...),
		result: result,
	}
	edge.mu.Unlock()
	cliWriteResult(writer, result)
}

func (edge *cliEmailEdge) callCount() int {
	edge.mu.Lock()
	defer edge.mu.Unlock()
	return edge.calls
}

func cliOperationCodec(operation string) (api.OperationCodec, bool) {
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == operation {
			return codec, true
		}
	}
	return api.OperationCodec{}, false
}

func cliWriteResult(writer http.ResponseWriter, result api.Result) {
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(api.HTTPStatusSuccess)
	_ = json.NewEncoder(writer).Encode(result)
}

func cliWritePublicError(writer http.ResponseWriter, code, message string) {
	status, ok := api.HTTPStatus(code)
	if !ok {
		status = http.StatusInternalServerError
	}
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(api.Error{Code: code, Message: message})
}

// cliCredentialSentinel is the mounted credential content. The CLI must never
// print it.
const cliCredentialSentinel = "p26-cli-credential-secret"

// runCLIEmailIntake runs the one-shot intake against one in-process public edge
// with the injected stdout and stderr writers.
func runCLIEmailIntake(t *testing.T, edge *cliEmailEdge, message string, stdout, stderr io.Writer) int {
	t.Helper()
	server := httptest.NewServer(edge)
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "message.eml")
	if err := os.WriteFile(path, []byte(message), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte(cliCredentialSentinel), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return runServicesEntry(context.Background(), []string{
		"--kind", "email",
		"--tenant", "tenant-1",
		"--project", "project-1",
		"--pipeline", "pipeline-1",
		"--message", path,
		"--base-url", server.URL,
		"--credential-file", credential,
	}, stdout, stderr)
}

// failingCLIWriter fails every write so the CLI must report a fixed output-write
// refusal instead of pretending the identifiers were delivered.
type failingCLIWriter struct{}

func (failingCLIWriter) Write([]byte) (int, error) {
	return 0, errors.New("simulated stdout failure")
}

// TestServicesEmailIntakePrintsTaskAndExecutionIDs proves a successful intake
// prints exactly one line carrying only the committed task and execution IDs to
// the injected stdout, for both a new result and a receipt replay, with empty
// stderr.
func TestServicesEmailIntakePrintsTaskAndExecutionIDs(t *testing.T) {
	const message = "Message-ID: <cli-print@example.test>\r\n" +
		"Subject: CLI print\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\nPrint the identifiers.\r\n"
	edge := newCLIEmailEdge()

	var firstStdout, firstStderr lockedBuffer
	if code := runCLIEmailIntake(t, edge, message, &firstStdout, &firstStderr); code != 0 {
		t.Fatalf("exit code = %d; want 0 (stderr %q)", code, firstStderr.String())
	}
	if got, want := firstStdout.String(), "task_id=task-cli-1 execution_id=execution-cli-1\n"; got != want {
		t.Fatalf("stdout = %q; want exactly %q", got, want)
	}
	if got := firstStderr.String(); got != "" {
		t.Fatalf("stderr = %q; want empty", got)
	}

	var replayStdout, replayStderr lockedBuffer
	if code := runCLIEmailIntake(t, edge, message, &replayStdout, &replayStderr); code != 0 {
		t.Fatalf("replay exit code = %d; want 0 (stderr %q)", code, replayStderr.String())
	}
	if got, want := replayStdout.String(), "task_id=task-cli-1 execution_id=execution-cli-1\n"; got != want {
		t.Fatalf("replay stdout = %q; want exactly %q", got, want)
	}
	if got := replayStderr.String(); got != "" {
		t.Fatalf("replay stderr = %q; want empty", got)
	}
	if got := edge.callCount(); got != 2 {
		t.Fatalf("public calls = %d; want the new call and the replay", got)
	}

	// An output-write failure is a fixed refusal with a nonzero exit, never a
	// silent success.
	var stderr lockedBuffer
	if code := runCLIEmailIntake(t, edge, message, failingCLIWriter{}, &stderr); code != 1 {
		t.Fatalf("output-write failure exit code = %d; want 1", code)
	}
	if got, want := stderr.String(), "kodama: email intake: writing the result failed\n"; got != want {
		t.Fatalf("stderr = %q; want exactly the fixed %q", got, want)
	}
	if strings.Contains(stderr.String(), cliCredentialSentinel) {
		t.Fatalf("stderr echoes the credential: %q", stderr.String())
	}
}
