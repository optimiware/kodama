//go:build e2e

package smoke_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// TestSmokeClassifyTerminalRefusals proves every fetch path used by the
// release-smoke helpers (execution poll, task state and retained-log read)
// classifies a 401 as an expired persona token and a 403 as a forbidden role,
// and aborts immediately instead of retrying to the poll timeout. The fake
// transport is a TLS httptest server carrying the public error body.
func TestSmokeClassifyTerminalRefusals(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		want   string
	}{
		{name: "expired persona token", status: http.StatusUnauthorized, code: api.CodeUnauthenticated, want: "expired"},
		{name: "forbidden role", status: http.StatusForbidden, code: api.CodeForbidden, want: "forbidden"},
	}
	operations := []struct {
		operation string
		input     any
	}{
		{"execution.inspect", api.ExecutionInspectRequest{ExecutionID: "execution-1"}},
		{"task.read", api.TaskReadRequest{TaskID: "task-1"}},
		{"execution.logs.read", api.ExecutionLogsReadRequest{ExecutionID: "execution-1", ActionID: "action-1", Stream: "stdout"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			for _, operation := range operations {
				client, calls, closeServer := newFakeAPI(t, testCase.status, testCase.code)
				token := harness.Secret("test-token")
				_, err := client.Get(t.Context(), token, operation.operation, operation.input, "")
				if err == nil {
					t.Fatalf("%s was accepted by the fake refusing transport", operation.operation)
				}
				classified, terminal := smoke.Classify(operation.operation, err)
				if !terminal {
					t.Fatalf("%s refusal was not terminal: %v", operation.operation, classified)
				}
				if !strings.Contains(classified.Error(), testCase.want) {
					t.Fatalf("%s refusal %q does not name %q", operation.operation, classified.Error(), testCase.want)
				}
				if testCase.status == http.StatusUnauthorized && !strings.Contains(classified.Error(), "899") {
					t.Fatalf("expired-token refusal %q does not name the ~899s OAuthSonas lifetime", classified.Error())
				}

				atomic.StoreInt32(calls, 0)
				started := time.Now()
				_, _, waitErr := smoke.Wait[api.Result](t.Context(), 10*time.Millisecond, 5*time.Second, operation.operation,
					func(ctx context.Context) (api.Result, error) {
						return client.Get(ctx, token, operation.operation, operation.input, "")
					},
					func(api.Result) bool { return false },
				)
				if waitErr == nil {
					t.Fatalf("%s wait accepted a terminal refusal", operation.operation)
				}
				if got := atomic.LoadInt32(calls); got != 1 {
					t.Fatalf("%s wait retried a terminal refusal %d times", operation.operation, got)
				}
				if elapsed := time.Since(started); elapsed > 2*time.Second {
					t.Fatalf("%s wait took %s; a terminal refusal must abort immediately", operation.operation, elapsed)
				}
				closeServer()
			}
		})
	}
}

// newFakeAPI builds the public API client against a TLS server that answers one
// public refusal code.
func newFakeAPI(t *testing.T, status int, code string) (*harness.API, *int32, func()) {
	t.Helper()
	var calls int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		atomic.AddInt32(&calls, 1)
		writer.Header().Set("Content-Type", api.MediaType)
		writer.WriteHeader(status)
		_ = json.NewEncoder(writer).Encode(api.Error{Code: code, Message: "test refusal"})
	}))
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, encoded, 0o600); err != nil {
		server.Close()
		t.Fatalf("write the fake CA: %v", err)
	}
	client, err := harness.NewAPI(harness.Config{APIURL: server.URL, CAPEM: caPath})
	if err != nil {
		server.Close()
		t.Fatalf("build the public API client: %v", err)
	}
	return client, &calls, server.Close
}

// TestSmokeExecutionIDs proves cleanup enumerates the full task history, the
// current execution and any caller-supplied id exactly once.
func TestSmokeExecutionIDs(t *testing.T) {
	task := api.TaskReadResult{
		ExecutionHistory: []string{"execution-1", "execution-2"},
		CurrentExecution: "execution-2",
	}
	got := smoke.ExecutionIDs(task, "execution-1", "execution-3", "")
	want := []string{"execution-1", "execution-2", "execution-3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("execution ids = %v; want %v", got, want)
	}
	if ids := smoke.ExecutionIDs(api.TaskReadResult{}); len(ids) != 0 {
		t.Fatalf("empty task enumerated %v", ids)
	}
}

// TestSmokeCheckSubjectEqual proves the immutable subject comparison is
// field-set equality: a reordered subject still matches, a changed field does
// not, and a missing subject does not.
func TestSmokeCheckSubjectEqual(t *testing.T) {
	want := &api.Subject{Fields: []api.SubjectField{
		{Name: "question", Kind: "string", Value: "w49-question"},
		{Name: "candidate", Kind: "string", Value: "abc1234"},
	}}
	reordered := &api.Subject{Fields: []api.SubjectField{
		{Name: "candidate", Kind: "string", Value: "abc1234"},
		{Name: "question", Kind: "string", Value: "w49-question"},
	}}
	if err := smoke.CheckSubjectEqual(want, reordered); err != nil {
		t.Fatalf("reordered subject refused: %v", err)
	}
	changed := &api.Subject{Fields: []api.SubjectField{
		{Name: "question", Kind: "string", Value: "another"},
		{Name: "candidate", Kind: "string", Value: "abc1234"},
	}}
	if err := smoke.CheckSubjectEqual(want, changed); err == nil {
		t.Fatal("a changed subject field was accepted")
	}
	if err := smoke.CheckSubjectEqual(want, nil); err == nil {
		t.Fatal("a missing final subject was accepted")
	}
}
