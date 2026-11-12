package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/userland/cli"
)

// wireRequest is one concrete request observed by the wire round tripper.
type wireRequest struct {
	method string
	path   string
	body   []byte
}

// wireRoundTripper records the concrete request and answers with one successful
// result for the operation under test.
type wireRoundTripper struct {
	operation string
	body      string

	mu       sync.Mutex
	requests []wireRequest
}

func (roundTripper *wireRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	data, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	roundTripper.mu.Lock()
	roundTripper.requests = append(roundTripper.requests, wireRequest{
		method: request.Method,
		path:   request.URL.Path,
		body:   data,
	})
	roundTripper.mu.Unlock()
	payload, err := json.Marshal(api.Result{
		Operation: roundTripper.operation,
		Body:      json.RawMessage(roundTripper.body),
	})
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: api.HTTPStatusSuccess,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{api.MediaType}},
		Body:       io.NopCloser(bytes.NewReader(payload)),
	}, nil
}

func (roundTripper *wireRoundTripper) recorded() []wireRequest {
	roundTripper.mu.Lock()
	defer roundTripper.mu.Unlock()
	return append([]wireRequest(nil), roundTripper.requests...)
}

// routeParamsFromPath recovers the route parameter values from the concrete
// path the client expanded from the invocation input.
func routeParamsFromPath(t *testing.T, operation, concrete string) map[string]string {
	t.Helper()
	route, ok := api.LookupRoute(operation)
	if !ok {
		t.Fatalf("operation %q has no declared route", operation)
	}
	template := strings.Split(strings.TrimPrefix(route.Path, "/"), "/")
	actual := strings.Split(strings.TrimPrefix(concrete, "/"), "/")
	if len(template) != len(actual) {
		t.Fatalf("path %q does not match template %q", concrete, route.Path)
	}
	params := make(map[string]string)
	for index, segment := range template {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			name := strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")
			params[name] = actual[index]
			continue
		}
		if segment != actual[index] {
			t.Fatalf("path %q does not match template %q", concrete, route.Path)
		}
	}
	return params
}

// codecForOperation returns the codec row for one operation.
func codecForOperation(t *testing.T, operation string) api.OperationCodec {
	t.Helper()
	for _, codec := range api.OperationCodecs() {
		if codec.Operation == operation {
			return codec
		}
	}
	t.Fatalf("operation %q has no declared codec", operation)
	return api.OperationCodec{}
}

// TestCLIInvocationInputsDecodeAtTheRESTEdge runs every CLI command that calls
// a registry operation through the real client transport and then decodes the
// produced input with the REST edge. A CLI field type, name or encoding that
// disagrees with the declared DTO fails here instead of against a live edge.
func TestCLIInvocationInputsDecodeAtTheRESTEdge(t *testing.T) {
	directory := "/repo/.kodama"
	cases := []struct {
		name         string
		operation    string
		response     string
		args         []string
		setup        func(*fakeFS)
		check        func(*testing.T, any)
		wantRequests int
	}{
		{
			name:      "call task.read",
			operation: "task.read",
			response:  `{}`,
			args:      []string{"call", "task.read", `--input={"task_id":"task-1"}`},
			check: func(t *testing.T, decoded any) {
				request, ok := decoded.(api.TaskReadRequest)
				if !ok {
					t.Fatalf("decoded %T; want api.TaskReadRequest", decoded)
				}
				if request.TaskID != "task-1" {
					t.Fatalf("task_id = %q; want task-1", request.TaskID)
				}
			},
			wantRequests: 1,
		},
		{
			name:      "call pipeline.apply",
			operation: "pipeline.apply",
			response:  `{}`,
			args: []string{
				"call", "pipeline.apply",
				`--input={"project_id":"shop","name":"build","yaml":"bmFtZTogYnVpbGQK"}`,
			},
			check: func(t *testing.T, decoded any) {
				request, ok := decoded.(api.PipelineApplyRequest)
				if !ok {
					t.Fatalf("decoded %T; want api.PipelineApplyRequest", decoded)
				}
				if request.ProjectID != "shop" || request.Name != "build" {
					t.Fatalf("decoded project/name = %q/%q; want shop/build", request.ProjectID, request.Name)
				}
				if string(request.YAML) != "name: build\n" {
					t.Fatalf("decoded yaml = %q; want exact bytes", request.YAML)
				}
			},
			wantRequests: 1,
		},
		{
			name:      "inspect",
			operation: "execution.inspect",
			response:  `{"status":"succeeded"}`,
			args:      []string{"inspect", "exec-1"},
			check: func(t *testing.T, decoded any) {
				request, ok := decoded.(api.ExecutionInspectRequest)
				if !ok {
					t.Fatalf("decoded %T; want api.ExecutionInspectRequest", decoded)
				}
				if request.ExecutionID != "exec-1" {
					t.Fatalf("execution_id = %q; want exec-1", request.ExecutionID)
				}
			},
			wantRequests: 1,
		},
		{
			name:      "watch",
			operation: "execution.inspect",
			response:  `{"status":"succeeded"}`,
			args:      []string{"watch", "exec-1"},
			check: func(t *testing.T, decoded any) {
				request, ok := decoded.(api.ExecutionInspectRequest)
				if !ok {
					t.Fatalf("decoded %T; want api.ExecutionInspectRequest", decoded)
				}
				if request.ExecutionID != "exec-1" {
					t.Fatalf("execution_id = %q; want exec-1", request.ExecutionID)
				}
			},
			wantRequests: 1,
		},
		{
			name:      "pipeline apply",
			operation: "pipeline.apply",
			response:  `{}`,
			args:      []string{"pipeline", "apply", directory},
			setup: func(filesystem *fakeFS) {
				filesystem.entries[directory] = []cli.DirEntry{{Name: "build.yaml"}}
				filesystem.files[directory+"/build.yaml"] = []byte("name: build\n")
			},
			check: func(t *testing.T, decoded any) {
				request, ok := decoded.(api.PipelineApplyRequest)
				if !ok {
					t.Fatalf("decoded %T; want api.PipelineApplyRequest", decoded)
				}
				if request.ProjectID != "shop" || request.Name != "build" {
					t.Fatalf("decoded project/name = %q/%q; want shop/build", request.ProjectID, request.Name)
				}
				if string(request.YAML) != "name: build\n" {
					t.Fatalf("decoded yaml = %q; want exact bytes", request.YAML)
				}
			},
			wantRequests: 1,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			roundTripper := &wireRoundTripper{operation: testCase.operation, body: testCase.response}
			transport := &client.HTTPTransport{
				BaseURL:      testConfig().ServerURL,
				RoundTripper: roundTripper,
				Source:       &scriptedSource{credentials: []client.Credential{humanCredential()}},
				Retry:        client.RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
				Now:          testNow,
				Sleep:        func(time.Duration) {},
			}
			ports, _, errOut := testPorts(transport)
			if testCase.setup != nil {
				filesystem := newFakeFS()
				testCase.setup(filesystem)
				ports.Files = filesystem
			}
			if code := cli.Run(context.Background(), testConfig(), testCase.args, ports); code != cli.ExitSuccess {
				t.Fatalf("exit code = %d; want %d (stderr: %s)", code, cli.ExitSuccess, errOut.String())
			}
			requests := roundTripper.recorded()
			if len(requests) != testCase.wantRequests {
				t.Fatalf("requests = %d; want %d", len(requests), testCase.wantRequests)
			}
			codec := codecForOperation(t, testCase.operation)
			for index, request := range requests {
				var envelope api.Request
				if err := json.Unmarshal(request.body, &envelope); err != nil {
					t.Fatalf("request %d body is not an api.Request: %v", index, err)
				}
				params := routeParamsFromPath(t, testCase.operation, request.path)
				decoded, err := api.DecodeRESTRequest(testCase.operation, envelope.Input, params)
				if err != nil {
					t.Fatalf("request %d input %s does not decode at the REST edge: %v", index, envelope.Input, err)
				}
				testCase.check(t, decoded)
				if _, err := codec.DecodeRequest(envelope.Input); err != nil {
					t.Fatalf("request %d input %s does not decode with the declared codec: %v", index, envelope.Input, err)
				}
			}
		})
	}
}
