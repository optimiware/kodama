package opsapp_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	opsapp "github.com/optimiware/kodama/internal/shell/app/ops"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/execution"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/transport/ops"
)

// inlineStartYAML is one exact inline document whose bytes every assertion
// compares verbatim.
var inlineStartYAML = []byte("actions:\n  - name: build\n    image: " + pinnedImage + "\n    commands:\n      - echo inline\n")

// inlineInputsYAML is one exact inline document that declares the branch input.
var inlineInputsYAML = []byte("inputs:\n  - branch\nactions:\n  - name: build\n    image: " + pinnedImage + "\n    commands:\n      - echo inline\n")

// recordingAdmission delegates to the real admission service and records every
// request the handler built, so the selection mapping is observable without
// re-implementing admission.
type recordingAdmission struct {
	inner    opsapp.PipelineAdmission
	requests []pipeline.AdmitRequest
}

func (recorder *recordingAdmission) Admit(ctx context.Context, request pipeline.AdmitRequest) (pipeline.AdmissionResult, error) {
	recorder.requests = append(recorder.requests, request)
	return recorder.inner.Admit(ctx, request)
}

// recordingExecutions delegates to the real execution service and records every
// start command, so the receipt content is observable at the effect
// boundary.
type recordingExecutions struct {
	inner  opsapp.ExecutionOperations
	starts []execution.StartCommand
}

func (recorder *recordingExecutions) Start(ctx context.Context, command execution.StartCommand) (execution.StartResult, error) {
	recorder.starts = append(recorder.starts, command)
	return recorder.inner.Start(ctx, command)
}

func (recorder *recordingExecutions) Inspect(ctx context.Context, command execution.InspectCommand) (execution.View, error) {
	return recorder.inner.Inspect(ctx, command)
}

func (recorder *recordingExecutions) List(ctx context.Context, command execution.ListCommand) (execution.Page, error) {
	return recorder.inner.List(ctx, command)
}

func (recorder *recordingExecutions) Cancel(ctx context.Context, command execution.CancelCommand) (execution.TransitionResult, error) {
	return recorder.inner.Cancel(ctx, command)
}

func (recorder *recordingExecutions) StageOutputs(ctx context.Context, command execution.OutputsCommand) (execution.TransitionResult, error) {
	return recorder.inner.StageOutputs(ctx, command)
}

// installAdmissionRecording routes the real admission through a recording seam
// and rebuilds the router over it.
func (h *harness) installAdmissionRecording(t *testing.T) *recordingAdmission {
	t.Helper()
	recorder := &recordingAdmission{inner: h.admission}
	h.admissionSeam = recorder
	h.buildRouter(t)
	return recorder
}

// installExecutionRecording routes the real execution service through a
// recording seam and rebuilds the router over it.
func (h *harness) installExecutionRecording(t *testing.T) *recordingExecutions {
	t.Helper()
	recorder := &recordingExecutions{inner: h.executions}
	h.executionSeam = recorder
	h.buildRouter(t)
	return recorder
}

// invokeInlineREST starts inline YAML on the task-free alternate REST binding,
// which supplies only the project.
func (h *harness) invokeInlineREST(t *testing.T, credential string, document []byte, inputs map[string]string, key string, expected *uint64) (api.Result, error) {
	t.Helper()
	input, err := json.Marshal(api.PipelineStartRequest{YAML: document, Inputs: inputs})
	if err != nil {
		t.Fatalf("marshal inline start input: %v", err)
	}
	return h.router.Invoke(context.Background(), ops.Invocation{
		Operation:  authorize.OperationName("pipeline.start"),
		Credential: credential,
		Request: api.Request{
			Input:           input,
			ExpectedVersion: expected,
			IdempotencyKey:  key,
		},
		Params: map[string]string{"project": testProject},
	})
}

// invokeInlineMCP starts inline YAML through the targetless MCP edge.
func (h *harness) invokeInlineMCP(t *testing.T, credential string, document []byte, inputs map[string]string, key string, expected *uint64) (api.Result, error) {
	t.Helper()
	input, err := json.Marshal(api.PipelineStartRequest{
		ProjectID: testProject,
		YAML:      document,
		Inputs:    inputs,
	})
	if err != nil {
		t.Fatalf("marshal inline MCP start input: %v", err)
	}
	return h.router.Invoke(context.Background(), ops.Invocation{
		Operation:  authorize.OperationName("pipeline.start"),
		Credential: credential,
		Request: api.Request{
			Input:           input,
			ExpectedVersion: expected,
			IdempotencyKey:  key,
		},
	})
}

// inlineStart decodes one successful inline start result.
func inlineStart(t *testing.T, result api.Result) api.PipelineStartResult {
	t.Helper()
	var started api.PipelineStartResult
	decodeResult(t, result, &started)
	if started.ExecutionID == "" || started.Version == 0 {
		t.Fatalf("inline start result = %+v", started)
	}
	return started
}

// executionRowCount counts the committed execution rows of the fixture project
// through the store seam. The list result cannot carry an inline execution
// because its public contract requires a stored pipeline identity.
func (h *harness) executionRowCount(t *testing.T) int {
	t.Helper()
	page, err := h.store.Query(context.Background(), tenancy.Scope{TenantID: testTenant, ProjectID: testProject}, store.ScanQuery{
		Table: "executions",
		Limit: 100,
	})
	if err != nil {
		t.Fatalf("query executions: %v", err)
	}
	return len(page.Rows)
}

// TestPipelineStartInlineSelectionBindings proves the project-only alternate
// REST binding and the targetless MCP tool both admit inline YAML. The route
// decoder must not synthesize a stored pipeline id for an inline document.
func TestPipelineStartInlineSelectionBindings(t *testing.T) {
	t.Run("alternate REST binding", func(t *testing.T) {
		h := newHarness(t)
		result, err := h.invokeInlineREST(t, testToken, inlineStartYAML, nil, "inline-rest", nil)
		if err != nil {
			t.Fatalf("inline REST pipeline.start: %v", err)
		}
		started := inlineStart(t, result)
		if started.Version != 1 {
			t.Fatalf("inline REST start version = %d; want the execution version 1", started.Version)
		}
	})

	t.Run("MCP tool", func(t *testing.T) {
		h := newHarness(t)
		result, err := h.invokeInlineMCP(t, testToken, inlineStartYAML, nil, "inline-mcp", nil)
		if err != nil {
			t.Fatalf("inline MCP pipeline.start: %v", err)
		}
		started := inlineStart(t, result)
		if started.Version != 1 {
			t.Fatalf("inline MCP start version = %d; want the execution version 1", started.Version)
		}
	})

	t.Run("conflicting body project refused", func(t *testing.T) {
		h := newHarness(t)
		input, err := json.Marshal(api.PipelineStartRequest{ProjectID: "project-b", YAML: inlineStartYAML})
		if err != nil {
			t.Fatalf("marshal inline start input: %v", err)
		}
		_, err = h.router.Invoke(context.Background(), ops.Invocation{
			Operation:  authorize.OperationName("pipeline.start"),
			Credential: testToken,
			Request:    api.Request{Input: input, IdempotencyKey: "inline-conflict"},
			Params:     map[string]string{"project": testProject},
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("conflicting inline start = %v; want invalid_input", err)
		}
	})

	t.Run("canonical route with inline YAML refused", func(t *testing.T) {
		h := newHarness(t)
		input, err := json.Marshal(api.PipelineStartRequest{YAML: inlineStartYAML})
		if err != nil {
			t.Fatalf("marshal inline start input: %v", err)
		}
		_, err = h.router.Invoke(context.Background(), ops.Invocation{
			Operation:  authorize.OperationName("pipeline.start"),
			Credential: testToken,
			Request:    api.Request{Input: input, IdempotencyKey: "inline-canonical"},
			Params:     map[string]string{"project": testProject, "pipeline": "pipeline-1"},
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
			t.Fatalf("canonical inline start = %v; want invalid_input", err)
		}
	})
}

// TestPipelineStartSelectionVersionRules proves the exclusive selection and the
// stored expected-version rule are decided before admission or identity
// allocation: a refused request leaves no execution and allocates no identity.
func TestPipelineStartSelectionVersionRules(t *testing.T) {
	h := newHarness(t)
	pipelineID := applyPipelineWithKey(t, h, "build.one", "apply-one")
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
	before := h.executionRowCount(t)

	cases := []struct {
		name string
		run  func() error
	}{
		{
			name: "both selections",
			run: func() error {
				_, err := h.invokeKey(t, testToken, "pipeline.start", jsonBody(t, api.PipelineStartRequest{
					PipelineID: pipelineID,
					YAML:       inlineStartYAML,
				}), "both-selections", map[string]string{"project": testProject})
				return err
			},
		},
		{
			name: "neither selection",
			run: func() error {
				_, err := h.invokeKey(t, testToken, "pipeline.start", `{"inputs":{}}`, "neither-selection", map[string]string{"project": testProject})
				return err
			},
		},
		{
			name: "inline with expected pipeline version",
			run: func() error {
				version := uint64(1)
				_, err := h.invokeInlineREST(t, testToken, inlineStartYAML, nil, "inline-expected-version", &version)
				return err
			},
		},
		{
			name: "stored without a positive expected version",
			run: func() error {
				_, err := h.invokeKey(t, testToken, "pipeline.start", `{"inputs":{}}`, "stored-without-version", map[string]string{
					"project":  testProject,
					"pipeline": pipelineID,
				})
				return err
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			idsBefore := h.ids.next
			err := testCase.run()
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("%s = %v; want invalid_input", testCase.name, err)
			}
			if h.ids.next != idsBefore {
				t.Fatalf("%s allocated identities %d -> %d; want a refusal before allocation", testCase.name, idsBefore, h.ids.next)
			}
		})
	}
	after := h.executionRowCount(t)
	if after != before {
		t.Fatalf("refused selections created executions: before=%d after=%d", before, after)
	}
	if _, ok := h.store.Snapshot(scope, "pipelines", pipelineID); !ok {
		t.Fatal("the stored fixture pipeline is missing")
	}
}

// TestPipelineStartMapsInlineAdmission proves the inline selection reaches the
// immutable admission boundary with the exact document bytes, the declared
// inputs, the request scope, the configured action timeout and no task context.
func TestPipelineStartMapsInlineAdmission(t *testing.T) {
	h := newHarness(t)
	admission := h.installAdmissionRecording(t)
	executions := h.installExecutionRecording(t)

	result, err := h.invokeInlineREST(t, testToken, inlineInputsYAML, map[string]string{"branch": "main"}, "mapped-inline", nil)
	if err != nil {
		t.Fatalf("inline pipeline.start: %v", err)
	}
	started := inlineStart(t, result)

	if len(admission.requests) != 1 {
		t.Fatalf("admission requests = %d; want 1", len(admission.requests))
	}
	request := admission.requests[0]
	if request.Definition.ID != "" || request.Definition.Version != 0 {
		t.Fatalf("inline admission definition = %+v; want empty id and version", request.Definition)
	}
	if request.Definition.Name != "" || request.Definition.Push != nil {
		t.Fatalf("inline admission definition = %+v; want no stored name or trigger", request.Definition)
	}
	if !bytes.Equal(request.Definition.YAML, inlineInputsYAML) {
		t.Fatalf("inline admission yaml = %q; want the exact bytes %q", request.Definition.YAML, inlineInputsYAML)
	}
	if len(request.Inputs) != 1 || request.Inputs[id.InputKey("branch")] != "main" {
		t.Fatalf("inline admission inputs = %v; want branch=main", request.Inputs)
	}
	if request.Scope != (tenancy.Scope{TenantID: testTenant, ProjectID: testProject}) {
		t.Fatalf("inline admission scope = %+v", request.Scope)
	}
	if request.DefaultTimeout != h.deps.ActionTimeout {
		t.Fatalf("inline admission default timeout = %s; want %s", request.DefaultTimeout, h.deps.ActionTimeout)
	}
	if request.Task.TaskID != "" || len(request.Task.Conversation) != 0 ||
		len(request.Task.References) != 0 || len(request.Task.Metadata) != 0 {
		t.Fatalf("inline admission carries a task context: %+v", request.Task)
	}

	if len(executions.starts) != 1 {
		t.Fatalf("execution starts = %d; want 1", len(executions.starts))
	}
	start := executions.starts[0]
	if start.Initiator != testEngineer {
		t.Fatalf("start initiator = %q; want %q", start.Initiator, testEngineer)
	}
	if start.ExecutionPrincipal == "" || start.ExecutionPrincipal == start.Initiator {
		t.Fatalf("start execution principal = %q; want a fresh capability identity", start.ExecutionPrincipal)
	}
	if start.Admission.PipelineID != "" || start.Admission.PipelineVersion != 0 {
		t.Fatalf("inline start admission = %+v; want no stored pipeline", start.Admission)
	}
	if started.Version != 1 {
		t.Fatalf("inline start version = %d; want 1", started.Version)
	}
}

// TestPipelineStartUsesIdempotencyReceiptContent proves the receipt content is
// the pipeline.start idempotency content, not the generic request wrapper. An
// inline document contributes its SHA-256 and inputs only; a stored selection
// contributes its id and expected version.
func TestPipelineStartUsesIdempotencyReceiptContent(t *testing.T) {
	t.Run("inline", func(t *testing.T) {
		h := newHarness(t)
		executions := h.installExecutionRecording(t)

		if _, err := h.invokeInlineREST(t, testToken, inlineInputsYAML, map[string]string{"branch": "main"}, "inline-content", nil); err != nil {
			t.Fatalf("inline pipeline.start: %v", err)
		}
		if len(executions.starts) != 1 {
			t.Fatalf("execution starts = %d; want 1", len(executions.starts))
		}
		want, err := api.PipelineStartIdempotencyContent(api.PipelineStartRequest{
			YAML:   inlineInputsYAML,
			Inputs: map[string]string{"branch": "main"},
		}, nil)
		if err != nil {
			t.Fatalf("inline idempotency content: %v", err)
		}
		assertReceiptContent(t, executions.starts[0].Content, want)
	})

	t.Run("stored", func(t *testing.T) {
		h := newHarness(t)
		pipelineID := applyPipelineWithKey(t, h, "build.one", "apply-one")
		executions := h.installExecutionRecording(t)

		// The stored selection is started at its current version. The
		// idempotency content still carries the envelope expectation rather
		// than the generic request wrapper.
		version := uint64(1)
		if _, err := invokeKeyVersion(t, h, testToken, "pipeline.start", `{"inputs":{}}`, "stored-content", version, map[string]string{
			"project":  testProject,
			"pipeline": pipelineID,
		}); err != nil {
			t.Fatalf("stored pipeline.start: %v", err)
		}
		// A fresh start whose envelope expectation is stale must be refused by
		// the version guard, never execute the current definition.
		_, err := invokeKeyVersion(t, h, testToken, "pipeline.start", `{"inputs":{}}`, "stored-content-stale", version+1, map[string]string{
			"project":  testProject,
			"pipeline": pipelineID,
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
			t.Fatalf("stale stored pipeline.start = %v; want version_conflict", err)
		}
		want, err := api.PipelineStartIdempotencyContent(api.PipelineStartRequest{
			ProjectID:  testProject,
			PipelineID: pipelineID,
		}, &version)
		if err != nil {
			t.Fatalf("stored idempotency content: %v", err)
		}
		assertReceiptContent(t, executions.starts[0].Content, want)
	})
}

// assertReceiptContent compares one recorded receipt content with the expected
// bytes and refuses the generic wrapper shape.
func assertReceiptContent(t *testing.T, got receipt.Content, want []byte) {
	t.Helper()
	if got.MediaType != api.MediaType {
		t.Fatalf("receipt content media type = %q; want %q", got.MediaType, api.MediaType)
	}
	if !bytes.Equal(got.Bytes, want) {
		t.Fatalf("receipt content = %s; want the idempotency content %s", got.Bytes, want)
	}
	if bytes.Contains(got.Bytes, []byte(`"request"`)) {
		t.Fatalf("receipt content uses the generic request wrapper: %s", got.Bytes)
	}
}

// TestPipelineStartStoredExpectedVersionContent proves a stored retry that
// reuses one idempotency key with a different positive expected pipeline
// version is refused as changed idempotency content and admits no second
// execution on either edge. The original version still replays.
func TestPipelineStartStoredExpectedVersionContent(t *testing.T) {
	edges := []struct {
		name string
		key  string
		run  func(t *testing.T, h *harness, pipelineID string, version uint64, key string) (api.Result, error)
	}{
		{
			name: "REST",
			key:  "stored-expected-version-rest",
			run: func(t *testing.T, h *harness, pipelineID string, version uint64, key string) (api.Result, error) {
				return invokeKeyVersion(t, h, testToken, "pipeline.start", `{"inputs":{}}`, key, version, map[string]string{
					"project":  testProject,
					"pipeline": pipelineID,
				})
			},
		},
		{
			name: "MCP",
			key:  "stored-expected-version-mcp",
			run: func(t *testing.T, h *harness, pipelineID string, version uint64, key string) (api.Result, error) {
				return invokeKeyVersion(t, h, testToken, "pipeline.start", jsonBody(t, map[string]any{
					"project_id":  testProject,
					"pipeline_id": pipelineID,
					"inputs":      map[string]string{},
				}), key, version, nil)
			},
		},
	}
	for _, edge := range edges {
		t.Run(edge.name, func(t *testing.T) {
			h := newHarness(t)
			pipelineID := applyPipelineWithKey(t, h, "build.one", "apply-one")
			executions := h.installExecutionRecording(t)

			first, err := edge.run(t, h, pipelineID, 1, edge.key)
			if err != nil {
				t.Fatalf("%s first stored start: %v", edge.name, err)
			}
			started := inlineStart(t, first)
			if len(executions.starts) != 1 {
				t.Fatalf("%s execution starts = %d; want 1", edge.name, len(executions.starts))
			}
			rowsAfterFirst := h.executionRowCount(t)

			_, err = edge.run(t, h, pipelineID, 2, edge.key)
			if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
				t.Fatalf("%s changed-version retry = %v; want changed_idempotency_content", edge.name, err)
			}
			if rows := h.executionRowCount(t); rows != rowsAfterFirst {
				t.Fatalf("%s changed-version retry admitted another execution: before=%d after=%d", edge.name, rowsAfterFirst, rows)
			}

			replayed, err := edge.run(t, h, pipelineID, 1, edge.key)
			if err != nil {
				t.Fatalf("%s original-version replay: %v", edge.name, err)
			}
			replayedStart := inlineStart(t, replayed)
			if replayedStart.ExecutionID != started.ExecutionID {
				t.Fatalf("%s original-version replay execution = %q; want %q", edge.name, replayedStart.ExecutionID, started.ExecutionID)
			}
		})
	}
}

// TestPipelineStartSelectionReceiptParity proves REST and MCP retries of one
// logical inline start converge on one receipt while changed bytes or inputs
// refuse, and an omitted input map equals an empty one.
func TestPipelineStartSelectionReceiptParity(t *testing.T) {
	h := newHarness(t)

	first, err := h.invokeInlineREST(t, testToken, inlineInputsYAML, map[string]string{"branch": "main"}, "parity-inline", nil)
	if err != nil {
		t.Fatalf("REST inline start: %v", err)
	}
	restStarted := inlineStart(t, first)

	replayed, err := h.invokeInlineMCP(t, testToken, inlineInputsYAML, map[string]string{"branch": "main"}, "parity-inline", nil)
	if err != nil {
		t.Fatalf("MCP inline replay: %v", err)
	}
	mcpStarted := inlineStart(t, replayed)
	if mcpStarted.ExecutionID != restStarted.ExecutionID {
		t.Fatalf("MCP replay execution = %q; want %q", mcpStarted.ExecutionID, restStarted.ExecutionID)
	}

	variants := []struct {
		name     string
		document []byte
		inputs   map[string]string
	}{
		{name: "byte-distinct yaml", document: append(append([]byte(nil), inlineInputsYAML...), '\n'), inputs: map[string]string{"branch": "main"}},
		{name: "changed inputs", document: inlineInputsYAML, inputs: map[string]string{"branch": "other"}},
	}
	for index, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			key := "parity-variant-" + string(rune('a'+index))
			if _, err := h.invokeInlineREST(t, testToken, inlineInputsYAML, map[string]string{"branch": "main"}, key, nil); err != nil {
				t.Fatalf("seed inline start: %v", err)
			}
			_, err := h.invokeInlineMCP(t, testToken, variant.document, variant.inputs, key, nil)
			if class, ok := fault.ClassOf(err); !ok || class != fault.ChangedIdempotencyContent {
				t.Fatalf("%s retry = %v; want changed_idempotency_content", variant.name, err)
			}
		})
	}

	omitted, err := h.invokeInlineREST(t, testToken, inlineStartYAML, nil, "parity-inputs", nil)
	if err != nil {
		t.Fatalf("REST inline start with omitted inputs: %v", err)
	}
	omittedStarted := inlineStart(t, omitted)
	emptied, err := h.invokeInlineMCP(t, testToken, inlineStartYAML, map[string]string{}, "parity-inputs", nil)
	if err != nil {
		t.Fatalf("MCP inline replay with empty inputs: %v", err)
	}
	emptiedStarted := inlineStart(t, emptied)
	if emptiedStarted.ExecutionID != omittedStarted.ExecutionID {
		t.Fatalf("empty-input replay execution = %q; want %q", emptiedStarted.ExecutionID, omittedStarted.ExecutionID)
	}
}

// TestPipelineStartReturnsExecutionVersion proves the result carries the
// committed execution version, including a replay after the execution has
// progressed, and never the stored pipeline version or the live execution
// version.
func TestPipelineStartReturnsExecutionVersion(t *testing.T) {
	t.Run("inline", func(t *testing.T) {
		h := newHarness(t)
		result, err := h.invokeInlineREST(t, testToken, inlineStartYAML, nil, "inline-version", nil)
		if err != nil {
			t.Fatalf("inline pipeline.start: %v", err)
		}
		started := inlineStart(t, result)
		if started.Version != 1 {
			t.Fatalf("inline start version = %d; want 1", started.Version)
		}
	})

	t.Run("replay after progression", func(t *testing.T) {
		h := newHarness(t)
		result, err := h.invokeInlineREST(t, testToken, inlineStartYAML, nil, "inline-progress", nil)
		if err != nil {
			t.Fatalf("inline pipeline.start: %v", err)
		}
		started := inlineStart(t, result)

		cancelled, err := h.invokeVersion(t, testToken, "execution.cancel", `{}`, started.Version, map[string]string{"execution": started.ExecutionID})
		if err != nil {
			t.Fatalf("execution.cancel: %v", err)
		}
		var cancellation api.ExecutionCancelResult
		decodeResult(t, cancelled, &cancellation)
		if cancellation.ExecutionVersion <= started.Version {
			t.Fatalf("cancel version = %d; want > %d", cancellation.ExecutionVersion, started.Version)
		}

		replayed, err := h.invokeInlineREST(t, testToken, inlineStartYAML, nil, "inline-progress", nil)
		if err != nil {
			t.Fatalf("inline replay after progression: %v", err)
		}
		replayedStart := inlineStart(t, replayed)
		if replayedStart.ExecutionID != started.ExecutionID {
			t.Fatalf("replay execution = %q; want %q", replayedStart.ExecutionID, started.ExecutionID)
		}
		if replayedStart.Version != started.Version {
			t.Fatalf("replay version = %d; want the committed start version %d", replayedStart.Version, started.Version)
		}
	})

	t.Run("stored", func(t *testing.T) {
		h := newHarness(t)
		pipelineID := applyPipelineWithKey(t, h, "build.one", "apply-one")
		version := uint64(1)
		started, err := invokeKeyVersion(t, h, testToken, "pipeline.start", `{"inputs":{}}`, "stored-version", version, map[string]string{
			"project":  testProject,
			"pipeline": pipelineID,
		})
		if err != nil {
			t.Fatalf("stored pipeline.start: %v", err)
		}
		var result api.PipelineStartResult
		decodeResult(t, started, &result)
		if result.Version != 1 {
			t.Fatalf("stored start version = %d; want the execution version 1, not the pipeline version", result.Version)
		}
	})
}

// TestPipelineStartSelectionAuthority proves pipeline.start uses the same
// authority matrix on every edge and selection: engineer humans and engineer
// services are allowed, while reviewer, operator, viewer, admin and execution
// principals are refused before admission and before identity allocation.
//
// Every refused human holds a real membership row with exactly the role under
// test. A bare principal would be refused by the membership authority read
// inside execution start even if the registry row wrongly granted the role, so
// the rows keep the grant decision observable: zero admissions and an unchanged
// identity counter prove the router refused the request, not a later commit.
func TestPipelineStartSelectionAuthority(t *testing.T) {
	h := newHarness(t)
	if _, err := h.invoke(t, testToken, "service.create", `{"service_id":"service-1","roles":["engineer"]}`, map[string]string{"project": testProject}); err != nil {
		t.Fatalf("service.create: %v", err)
	}
	scope := tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
	h.principals.principals["token-service"] = identity.Principal{
		ID:      testService,
		Kind:    identity.KindService,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	}

	refused := []struct {
		name       string
		credential string
		principal  identity.Principal
	}{
		{
			name:       "reviewer",
			credential: "token-reviewer",
			principal: identity.Principal{
				ID: "reviewer-1", Kind: identity.KindHuman, Scope: scope,
				Roles: []tenancy.Role{tenancy.RoleReviewer}, Enabled: true, Version: 1,
			},
		},
		{
			name:       "operator",
			credential: "token-operator",
			principal: identity.Principal{
				ID: "operator-1", Kind: identity.KindHuman, Scope: scope,
				Roles: []tenancy.Role{tenancy.RoleOperator}, Enabled: true, Version: 1,
			},
		},
		{
			name:       "viewer",
			credential: "token-viewer",
			principal: identity.Principal{
				ID: "viewer-1", Kind: identity.KindHuman, Scope: scope,
				Roles: []tenancy.Role{tenancy.RoleViewer}, Enabled: true, Version: 1,
			},
		},
		{
			name:       "admin",
			credential: "token-admin-only",
			principal: identity.Principal{
				ID: "admin-only-1", Kind: identity.KindHuman, Scope: scope,
				Roles: []tenancy.Role{tenancy.RoleAdmin}, Enabled: true, Version: 1,
			},
		},
	}
	for _, refusal := range refused {
		h.principals.principals[refusal.credential] = refusal.principal
		h.seedMembership(t, scope, "membership-refused-"+refusal.name, refusal.principal.ID, 1, refusal.principal.Roles...)
	}
	executionPrincipal := identity.Principal{
		ID: "execution-actor", Kind: identity.KindExecution, Scope: scope,
		Enabled: true, Version: 1, ExecutionID: "execution-actor",
	}
	h.principals.principals["token-execution"] = executionPrincipal
	h.facts.install("execution-actor", authorize.AdmittedExecution{
		Principal:   executionPrincipal,
		ExecutionID: "execution-actor",
		ActionID:    "action-1",
		Permission:  authorize.ExecNone,
	})

	admission := h.installAdmissionRecording(t)
	pipelineID := applyPipelineWithKey(t, h, "build.one", "apply-one")

	allowed := map[string]string{
		"engineer": testToken,
		"service":  "token-service",
	}
	for name, credential := range allowed {
		t.Run("allowed "+name, func(t *testing.T) {
			result, err := h.invokeInlineREST(t, credential, inlineStartYAML, nil, "authority-"+name, nil)
			if err != nil {
				t.Fatalf("%s inline start: %v", name, err)
			}
			inlineStart(t, result)
		})
	}

	refusedCredentials := make(map[string]string, len(refused)+1)
	for _, refusal := range refused {
		refusedCredentials[refusal.name] = refusal.credential
	}
	refusedCredentials["execution"] = "token-execution"

	storedVersion := uint64(1)
	edges := []struct {
		name string
		key  string
		run  func(t *testing.T, credential, key string) error
	}{
		{
			name: "inline REST",
			key:  "inline-rest",
			run: func(t *testing.T, credential, key string) error {
				_, err := h.invokeInlineREST(t, credential, inlineStartYAML, nil, key, nil)
				return err
			},
		},
		{
			name: "inline MCP",
			key:  "inline-mcp",
			run: func(t *testing.T, credential, key string) error {
				_, err := h.invokeInlineMCP(t, credential, inlineStartYAML, nil, key, nil)
				return err
			},
		},
		{
			name: "stored REST",
			key:  "stored-rest",
			run: func(t *testing.T, credential, key string) error {
				_, err := invokeKeyVersion(t, h, credential, "pipeline.start", `{"inputs":{}}`, key, storedVersion, map[string]string{
					"project":  testProject,
					"pipeline": pipelineID,
				})
				return err
			},
		},
		{
			name: "stored MCP",
			key:  "stored-mcp",
			run: func(t *testing.T, credential, key string) error {
				_, err := invokeKeyVersion(t, h, credential, "pipeline.start", jsonBody(t, map[string]any{
					"project_id":  testProject,
					"pipeline_id": pipelineID,
					"inputs":      map[string]string{},
				}), key, storedVersion, nil)
				return err
			},
		},
	}
	before := h.executionRowCount(t)
	for name, credential := range refusedCredentials {
		for _, edge := range edges {
			t.Run("refused "+name+" "+edge.name, func(t *testing.T) {
				admissionsBefore := len(admission.requests)
				idsBefore := h.ids.next
				err := edge.run(t, credential, "refused-"+name+"-"+edge.key)
				if class, ok := fault.ClassOf(err); !ok || class != fault.Forbidden {
					t.Fatalf("%s %s = %v; want forbidden", name, edge.name, err)
				}
				if got := len(admission.requests); got != admissionsBefore {
					t.Fatalf("%s %s reached admission %d -> %d; want a refusal before admission", name, edge.name, admissionsBefore, got)
				}
				if h.ids.next != idsBefore {
					t.Fatalf("%s %s allocated identities %d -> %d; want a refusal before allocation", name, edge.name, idsBefore, h.ids.next)
				}
			})
		}
	}
	after := h.executionRowCount(t)
	if after != before {
		t.Fatalf("refused principals created executions: before=%d after=%d", before, after)
	}
}

// TestPipelineStartReplaySurvivesPipelineEdit proves pipeline.start is
// receipt-first on the real handler: an equal retry after the stored definition
// advanced returns the original execution without a second admission, while a
// fresh start at the stale envelope version is a version conflict.
func TestPipelineStartReplaySurvivesPipelineEdit(t *testing.T) {
	h := newHarness(t)
	pipelineID := applyPipelineWithKey(t, h, "build.replay", "apply-replay")
	admission := h.installAdmissionRecording(t)
	params := map[string]string{"project": testProject, "pipeline": pipelineID}

	first, err := invokeKeyVersion(t, h, testToken, "pipeline.start", `{"inputs":{}}`, "replay-start", 1, params)
	if err != nil {
		t.Fatalf("first pipeline.start: %v", err)
	}
	var started api.PipelineStartResult
	decodeResult(t, first, &started)
	if len(admission.requests) != 1 {
		t.Fatalf("admissions after first start = %d; want 1", len(admission.requests))
	}

	edited := []byte("inputs:\n  - edited-required\nactions:\n  - name: build\n    image: " + pinnedImage + "\n    commands:\n      - echo edited\n")
	_, err = invokeKeyVersion(t, h, testToken, "pipeline.update", jsonBody(t, map[string]any{
		"name": "build.replay",
		"yaml": base64.StdEncoding.EncodeToString(edited),
	}), "replay-update", 1, params)
	if err != nil {
		t.Fatalf("pipeline.update: %v", err)
	}

	replayed, err := invokeKeyVersion(t, h, testToken, "pipeline.start", `{"inputs":{}}`, "replay-start", 1, params)
	if err != nil {
		t.Fatalf("replayed pipeline.start after edit: %v", err)
	}
	var replayedResult api.PipelineStartResult
	decodeResult(t, replayed, &replayedResult)
	if replayedResult.ExecutionID != started.ExecutionID || replayedResult.Version != started.Version {
		t.Fatalf("replayed start = %+v; want the original %+v", replayedResult, started)
	}
	if len(admission.requests) != 1 {
		t.Fatalf("admissions after replay = %d; want the original admission only", len(admission.requests))
	}

	_, err = invokeKeyVersion(t, h, testToken, "pipeline.start", jsonBody(t, map[string]any{
		"inputs": map[string]string{"edited-required": "value"},
	}), "stale-start", 1, params)
	if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
		t.Fatalf("fresh start at the stale envelope version = %v; want version_conflict", err)
	}
}
