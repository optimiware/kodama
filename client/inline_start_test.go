// Inline-YAML pipeline.start client behavior.
//
// An inline pipeline.start uses the additive alternate binding
// POST /api/v1/projects/{project}/executions, and
// api.MaxEnvelopeBytes(operation) is the request envelope bound (2 MiB for
// pipeline.apply, pipeline.update and pipeline.start; 1 MiB otherwise). The
// transport must select that binding for an inline selection, keep the
// canonical stored-pipeline route, refuse an exclusive-selection violation and
// an inline expected_version before sending, apply the per-operation envelope
// bound, and decode the admitted execution version from the result.
package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
)

// inlineDocument is one byte-exact inline pipeline document.
func inlineDocument() []byte {
	return []byte("pipeline: build\nsteps:\n  - run: make\n")
}

// inlineStartInvocation builds one inline pipeline.start invocation.
func inlineStartInvocation(t *testing.T, document []byte, inputs map[string]string) api.Invocation {
	t.Helper()
	input, err := json.Marshal(api.PipelineStartRequest{
		ProjectID: "shop",
		YAML:      document,
		Inputs:    inputs,
	})
	if err != nil {
		t.Fatalf("marshal inline pipeline.start input: %v", err)
	}
	return api.Invocation{
		Operation: "pipeline.start",
		Request: api.Request{
			Input:          input,
			IdempotencyKey: "inline-start-key-1",
		},
	}
}

// envelopeBytes returns the exact serialized api.Request body length.
func envelopeBytes(t *testing.T, invocation api.Invocation) int {
	t.Helper()
	body, err := json.Marshal(invocation.Request)
	if err != nil {
		t.Fatalf("marshal request envelope: %v", err)
	}
	return len(body)
}

// inlineStartAtEnvelope builds a fully valid inline pipeline.start invocation
// whose serialized envelope is exactly target bytes. The 1 MiB document is
// padded with a bounded input value, so the typed input stays valid and only
// the envelope size moves.
func inlineStartAtEnvelope(t *testing.T, target int) api.Invocation {
	t.Helper()
	document := make([]byte, api.MaxInlineYAMLBytes)
	for index := range document {
		document[index] = 'a'
	}
	document[0] = 'p'
	base := inlineStartInvocation(t, document, map[string]string{"pad": ""})
	padding := target - envelopeBytes(t, base)
	if padding < 0 {
		t.Fatalf("envelope target %d is below the %d-byte base", target, envelopeBytes(t, base))
	}
	invocation := inlineStartInvocation(t, document, map[string]string{"pad": strings.Repeat("a", padding)})
	if got := envelopeBytes(t, invocation); got != target {
		t.Fatalf("built envelope = %d bytes; want %d", got, target)
	}
	return invocation
}

// commentInvocationAtEnvelope builds one fully valid task.comment invocation
// whose serialized envelope is exactly target bytes.
func commentInvocationAtEnvelope(t *testing.T, target int) api.Invocation {
	t.Helper()
	build := func(comment string) api.Invocation {
		input, err := json.Marshal(api.TaskCommentRequest{TaskID: "task-1", Body: comment})
		if err != nil {
			t.Fatalf("marshal task.comment input: %v", err)
		}
		return api.Invocation{
			Operation: "task.comment",
			Request: api.Request{
				Input:          input,
				IdempotencyKey: "comment-key-1",
			},
		}
	}
	padding := target - envelopeBytes(t, build(""))
	if padding <= 0 {
		t.Fatalf("envelope target %d does not leave a non-empty comment body", target)
	}
	invocation := build(strings.Repeat("a", padding))
	if got := envelopeBytes(t, invocation); got != target {
		t.Fatalf("built envelope = %d bytes; want %d", got, target)
	}
	return invocation
}

// TestHTTPTransportInlineStartUsesAlternateBinding pins the binding
// selection: an inline selection with no pipeline id goes to
// POST /api/v1/projects/{project}/executions with the exact document bytes and
// no expected pipeline version, and the admitted execution version is decoded.
func TestHTTPTransportInlineStartUsesAlternateBinding(t *testing.T) {
	document := inlineDocument()
	invocation := inlineStartInvocation(t, document, map[string]string{"branch": "main"})

	roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
		return resultResponse("pipeline.start", `{"execution_id":"exec-1","version":3}`), nil
	}}
	transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
	result, err := transport.Invoke(context.Background(), invocation)
	if err != nil {
		t.Fatalf("inline pipeline.start: %v", err)
	}

	recorded := roundTripper.recorded()
	if len(recorded) != 1 {
		t.Fatalf("requests = %d; want 1", len(recorded))
	}
	if recorded[0].method != string(api.MethodPost) {
		t.Errorf("method = %q; want POST", recorded[0].method)
	}
	if got := recorded[0].url.Path; got != "/api/v1/projects/shop/executions" {
		t.Errorf("path = %q; want the alternate executions binding", got)
	}

	var sent api.Request
	if err := json.Unmarshal(recorded[0].body, &sent); err != nil {
		t.Fatalf("sent body is not an api.Request: %v", err)
	}
	if sent.ExpectedVersion != nil {
		t.Errorf("inline start sent expected version %v; want none", *sent.ExpectedVersion)
	}
	if sent.IdempotencyKey != "inline-start-key-1" {
		t.Errorf("idempotency key = %q", sent.IdempotencyKey)
	}
	var input api.PipelineStartRequest
	if err := json.Unmarshal(sent.Input, &input); err != nil {
		t.Fatalf("sent input is not a pipeline.start input: %v", err)
	}
	if input.ProjectID != "shop" {
		t.Errorf("project_id = %q; want shop", input.ProjectID)
	}
	if input.PipelineID != "" {
		t.Errorf("inline start sent pipeline_id %q", input.PipelineID)
	}
	if string(input.YAML) != string(document) {
		t.Errorf("inline yaml = %q; want the exact bytes %q", input.YAML, document)
	}
	if input.Inputs["branch"] != "main" {
		t.Errorf("inputs = %v; want branch=main", input.Inputs)
	}

	var admitted api.PipelineStartResult
	if err := json.Unmarshal(result.Body, &admitted); err != nil {
		t.Fatalf("result body is not a pipeline.start result: %v", err)
	}
	if admitted.ExecutionID != "exec-1" || admitted.Version != 3 {
		t.Fatalf("admitted execution = %+v; want exec-1 version 3", admitted)
	}
}

// TestHTTPTransportStoredStartKeepsCanonicalBinding pins that a stored pipeline
// selection keeps the canonical start route, the pipeline id in the path and
// the expected pipeline version from the shared envelope.
func TestHTTPTransportStoredStartKeepsCanonicalBinding(t *testing.T) {
	input, err := json.Marshal(api.PipelineStartRequest{
		ProjectID:  "shop",
		PipelineID: "pipe-1",
		Inputs:     map[string]string{"branch": "main"},
	})
	if err != nil {
		t.Fatalf("marshal stored pipeline.start input: %v", err)
	}
	roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
		return resultResponse("pipeline.start", `{"execution_id":"exec-2","version":4}`), nil
	}}
	transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
	result, err := transport.Invoke(context.Background(), api.Invocation{
		Operation: "pipeline.start",
		Request: api.Request{
			Input:           input,
			ExpectedVersion: new(uint64(7)),
			IdempotencyKey:  "stored-start-key-1",
		},
	})
	if err != nil {
		t.Fatalf("stored pipeline.start: %v", err)
	}
	recorded := roundTripper.recorded()
	if len(recorded) != 1 {
		t.Fatalf("requests = %d; want 1", len(recorded))
	}
	if got := recorded[0].url.Path; got != "/api/v1/projects/shop/pipelines/pipe-1/start" {
		t.Errorf("path = %q; want the canonical stored-pipeline start route", got)
	}
	var sent api.Request
	if err := json.Unmarshal(recorded[0].body, &sent); err != nil {
		t.Fatalf("sent body is not an api.Request: %v", err)
	}
	if sent.ExpectedVersion == nil || *sent.ExpectedVersion != 7 {
		t.Fatalf("expected version = %v; want 7", sent.ExpectedVersion)
	}
	var admitted api.PipelineStartResult
	if err := json.Unmarshal(result.Body, &admitted); err != nil {
		t.Fatalf("result body is not a pipeline.start result: %v", err)
	}
	if admitted.ExecutionID != "exec-2" || admitted.Version != 4 {
		t.Fatalf("admitted execution = %+v; want exec-2 version 4", admitted)
	}
}

// TestHTTPTransportYAMLEnvelopeUsesPerOperationBound pins the envelope rule:
// the three YAML-carrying operations accept a request above the 1 MiB
// default because a 1 MiB document expands to about 1.4 MiB of base64. Each
// also keeps its own declared binding.
func TestHTTPTransportYAMLEnvelopeUsesPerOperationBound(t *testing.T) {
	document := make([]byte, api.MaxInlineYAMLBytes)
	for index := range document {
		document[index] = 'a'
	}
	document[0] = 'p'

	applyInput, err := json.Marshal(api.PipelineApplyRequest{ProjectID: "shop", Name: "build.v2", YAML: document})
	if err != nil {
		t.Fatalf("marshal pipeline.apply input: %v", err)
	}
	updateInput, err := json.Marshal(api.PipelineUpdateRequest{
		ProjectID: "shop", PipelineID: "pipe-1", Name: "build.v2", YAML: document,
	})
	if err != nil {
		t.Fatalf("marshal pipeline.update input: %v", err)
	}

	cases := []struct {
		name       string
		invocation api.Invocation
		wantPath   string
		response   string
	}{
		{
			name: "pipeline.apply",
			invocation: api.Invocation{Operation: "pipeline.apply", Request: api.Request{
				Input: applyInput, IdempotencyKey: "apply-key-1",
			}},
			wantPath: "/api/v1/projects/shop/pipelines/build.v2",
			response: `{"pipeline_id":"pipe-1","version":8}`,
		},
		{
			name: "pipeline.update",
			invocation: api.Invocation{Operation: "pipeline.update", Request: api.Request{
				Input: updateInput, ExpectedVersion: new(uint64(7)), IdempotencyKey: "update-key-1",
			}},
			wantPath: "/api/v1/projects/shop/pipelines/pipe-1",
			response: `{"version":8}`,
		},
		{
			name:       "pipeline.start inline",
			invocation: inlineStartInvocation(t, document, nil),
			wantPath:   "/api/v1/projects/shop/executions",
			response:   `{"execution_id":"exec-1","version":1}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			size := envelopeBytes(t, testCase.invocation)
			if size <= api.MaxRequestBytes {
				t.Fatalf("test envelope = %d bytes; want above the 1 MiB default %d", size, api.MaxRequestBytes)
			}
			if size > api.MaxYAMLRequestBytes {
				t.Fatalf("test envelope = %d bytes; want within the 2 MiB YAML bound %d", size, api.MaxYAMLRequestBytes)
			}

			roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
				return resultResponse(testCase.invocation.Operation, testCase.response), nil
			}}
			transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
			if _, err := transport.Invoke(context.Background(), testCase.invocation); err != nil {
				t.Fatalf("%s at a %d-byte envelope: %v", testCase.name, size, err)
			}
			recorded := roundTripper.recorded()
			if len(recorded) != 1 {
				t.Fatalf("requests = %d; want 1", len(recorded))
			}
			if got := recorded[0].url.Path; got != testCase.wantPath {
				t.Errorf("path = %q; want %q", got, testCase.wantPath)
			}
		})
	}
}

// TestHTTPTransportEnvelopeBoundaries pins the exact accepted bound and the
// one-byte-over refusal before any request: 2 MiB for pipeline.start and the
// 1 MiB default for every other operation.
func TestHTTPTransportEnvelopeBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, target int) api.Invocation
		bound int
	}{
		{"pipeline.start", inlineStartAtEnvelope, api.MaxYAMLRequestBytes},
		{"task.comment", commentInvocationAtEnvelope, api.MaxRequestBytes},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			atBound := testCase.build(t, testCase.bound)
			roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
				return resultResponse(atBound.Operation, `{"execution_id":"exec-1","version":1}`), nil
			}}
			transport := newTransport(roundTripper, &fakeSource{credentials: []client.Credential{validCredential()}})
			if _, err := transport.Invoke(context.Background(), atBound); err != nil {
				t.Fatalf("%s at exactly %d bytes refused: %v", testCase.name, testCase.bound, err)
			}
			if got := len(roundTripper.recorded()); got != 1 {
				t.Fatalf("at-bound requests = %d; want 1", got)
			}

			overBound := testCase.build(t, testCase.bound+1)
			refused := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
				return resultResponse(overBound.Operation, `{}`), nil
			}}
			refusedTransport := newTransport(refused, &fakeSource{credentials: []client.Credential{validCredential()}})
			_, err := refusedTransport.Invoke(context.Background(), overBound)
			if err == nil {
				t.Fatalf("%s at %d bytes was accepted", testCase.name, testCase.bound+1)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("one-byte-over class = %v; want invalid_input", class)
			}
			if got := len(refused.recorded()); got != 0 {
				t.Fatalf("one-byte-over requests = %d; want 0", got)
			}
		})
	}
}

// TestHTTPTransportRefusesInlineStartSelectionViolations pins the client-side
// exclusive-selection rule before any request: both selections, neither, and an
// inline selection carrying an expected pipeline version all refuse with
// invalid_input.
func TestHTTPTransportRefusesInlineStartSelectionViolations(t *testing.T) {
	response := `{"execution_id":"exec-1","version":1}`
	cases := []struct {
		name     string
		request  api.Request
		response string
	}{
		{
			name: "both selections",
			request: api.Request{Input: json.RawMessage(
				`{"project_id":"shop","pipeline_id":"pipe-1","yaml":"cGlwZWxpbmU6IGJ1aWxkCg=="}`),
				ExpectedVersion: new(uint64(7)), IdempotencyKey: "key-1"},
			response: response,
		},
		{
			name:     "neither selection",
			request:  api.Request{Input: json.RawMessage(`{"project_id":"shop","inputs":{}}`), IdempotencyKey: "key-1"},
			response: response,
		},
		{
			name: "inline with expected version",
			request: api.Request{Input: json.RawMessage(
				`{"project_id":"shop","yaml":"cGlwZWxpbmU6IGJ1aWxkCg=="}`),
				ExpectedVersion: new(uint64(7)), IdempotencyKey: "key-1"},
			response: response,
		},
		{
			name:     "empty inline document and no pipeline",
			request:  api.Request{Input: json.RawMessage(`{"project_id":"shop","yaml":""}`), IdempotencyKey: "key-1"},
			response: response,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			roundTripper := &recordingRoundTripper{respond: func(_ *http.Request, _ int) (*http.Response, error) {
				return resultResponse("pipeline.start", testCase.response), nil
			}}
			source := &fakeSource{credentials: []client.Credential{validCredential()}}
			transport := newTransport(roundTripper, source)
			_, err := transport.Invoke(context.Background(), api.Invocation{
				Operation: "pipeline.start",
				Request:   testCase.request,
			})
			if err == nil {
				t.Fatal("Invoke accepted an invalid pipeline.start selection")
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("class = %v; want invalid_input", class)
			}
			if got := len(roundTripper.recorded()); got != 0 {
				t.Fatalf("requests = %d; want 0", got)
			}
			if got := source.callCount(); got != 0 {
				t.Fatalf("credential reads = %d; want 0", got)
			}
		})
	}
}
