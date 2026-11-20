package cli_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
	"github.com/optimiware/kodema/internal/userland/cli"
)

// streamingScriptedTransport extends scriptedTransport with an injected
// client.EventStreamer for testing `events --follow`, `watch --project`, and
// `inbox --wait`.
type streamingScriptedTransport struct {
	*scriptedTransport
	streamFn func(ctx context.Context, req client.EventStreamRequest) (client.EventStream, error)
}

var _ client.EventStreamer = (*streamingScriptedTransport)(nil)

func (t *streamingScriptedTransport) StreamEvents(ctx context.Context, req client.EventStreamRequest) (client.EventStream, error) {
	if t.streamFn == nil {
		return nil, io.EOF
	}
	return t.streamFn(ctx, req)
}

type chanEventStream struct {
	ch     chan api.StreamEvent
	closed chan struct{}
	once   sync.Once
}

func newChanEventStream(events ...api.StreamEvent) *chanEventStream {
	ch := make(chan api.StreamEvent, len(events)+4)
	for _, e := range events {
		ch <- e
	}
	return &chanEventStream{ch: ch, closed: make(chan struct{})}
}

func (s *chanEventStream) Next(ctx context.Context) (api.StreamEvent, error) {
	select {
	case <-ctx.Done():
		return api.StreamEvent{}, ctx.Err()
	case <-s.closed:
		return api.StreamEvent{}, io.EOF
	case evt, ok := <-s.ch:
		if !ok {
			return api.StreamEvent{}, io.EOF
		}
		return evt, nil
	}
}

func (s *chanEventStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func TestRunEventsOnceDecodesBase64JSONTableAndJSON(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	nextCur, _ := api.EncodeEventCursor(42)
	sinceCur, _ := api.EncodeEventCursor(10)

	var seenReq api.EventReadRequest
	transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
		if inv.Operation != "event.read" {
			t.Fatalf("unexpected operation %s", inv.Operation)
		}
		if err := json.Unmarshal(inv.Request.Input, &seenReq); err != nil {
			t.Fatalf("unmarshal input: %v", err)
		}
		res := api.EventReadResult{
			Events: []api.Event{
				{
					ID:        "evt-1",
					Type:      "task.created",
					Source:    api.EventSource{Kind: "shell", PrincipalID: "human-1"},
					At:        now,
					Scope:     api.EventScope{TenantID: "t1", ProjectID: "proj-1"},
					SubjectID: "task-1",
					Data:      []byte(`{"title":"Ship SSE","priority":1}`),
				},
				{
					ID:         "evt-2",
					Type:       "resource.verified",
					ReportType: "git.ref.updated",
					Source:     api.EventSource{Kind: "service", PrincipalID: "git"},
					At:         now.Add(time.Second),
					Scope:      api.EventScope{TenantID: "t1", ProjectID: "proj-1"},
					SubjectID:  "git-app",
					Data:       []byte(`{"ref":"refs/heads/main"}`),
				},
			},
			Next:    nextCur,
			HasMore: false,
		}
		body, _ := json.Marshal(res)
		return api.Result{Operation: "event.read", Body: body}, nil
	}}

	t.Run("table", func(t *testing.T) {
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{
			"events", "--project", "proj-1",
			"--type", "task.created", "--type", "resource.verified",
			"--since", sinceCur, "--limit", "25",
		}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if seenReq.ProjectID != "proj-1" || seenReq.Page == nil || seenReq.Page.Cursor != sinceCur || seenReq.Page.Limit != 25 {
			t.Fatalf("event.read req = %+v", seenReq)
		}
		text := out.String()
		if !strings.Contains(text, "task.created") || !strings.Contains(text, "Ship SSE") {
			t.Fatalf("table output missing decoded task.created data:\n%s", text)
		}
		if !strings.Contains(text, "resource.verified (git.ref.updated)") || !strings.Contains(text, "refs/heads/main") {
			t.Fatalf("table output missing report_type/data:\n%s", text)
		}
	})

	t.Run("json_decodes_base64_data", func(t *testing.T) {
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		code := cli.Run(context.Background(), cfg, []string{
			"events", "--project", "proj-1", "--output", "json",
		}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		var parsed struct {
			Events []api.StreamEvent `json:"events"`
			Next   string            `json:"next"`
		}
		if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
			t.Fatalf("invalid json: %v (%s)", err, out.String())
		}
		if len(parsed.Events) != 2 || parsed.Next != nextCur {
			t.Fatalf("parsed = %+v", parsed)
		}
		var dataObj map[string]any
		if err := json.Unmarshal(parsed.Events[0].Data, &dataObj); err != nil {
			t.Fatalf("event[0].Data not decoded JSON object: %s (%v)", string(parsed.Events[0].Data), err)
		}
		if dataObj["title"] != "Ship SSE" {
			t.Fatalf("event[0].Data = %v; want title=Ship SSE", dataObj)
		}
	})
}

func TestRunEventsFollowStreamsNDJSONAndDeduplicates(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	cur1, _ := api.EncodeEventCursor(1)
	cur2, _ := api.EncodeEventCursor(2)

	var gotStreamReq client.EventStreamRequest
	tr := &streamingScriptedTransport{
		scriptedTransport: &scriptedTransport{},
		streamFn: func(_ context.Context, req client.EventStreamRequest) (client.EventStream, error) {
			gotStreamReq = req
			s := newChanEventStream(
				api.StreamEvent{ID: "e1", Cursor: cur1, Sequence: 1, Type: "task.created", At: now, SubjectID: "task-1", Data: json.RawMessage(`{"title":"First"}`)},
				// Duplicate of e1 must be dropped cleanly.
				api.StreamEvent{ID: "e1", Cursor: cur1, Sequence: 1, Type: "task.created", At: now, SubjectID: "task-1", Data: json.RawMessage(`{"title":"First"}`)},
				api.StreamEvent{ID: "e2", Cursor: cur2, Sequence: 2, Type: "execution.queued", At: now.Add(time.Second), SubjectID: "exec-1", Data: json.RawMessage(`{"state":"queued"}`)},
			)
			close(s.ch)
			return s, nil
		},
	}

	ports, out, errOut := testPorts(tr)
	cfg := testConfig()
	code := cli.Run(context.Background(), cfg, []string{
		"events", "--project", "proj-1", "--type", "task.created,execution.queued", "--follow", "--output", "json",
	}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
	}
	if gotStreamReq.ProjectID != "proj-1" || len(gotStreamReq.Types) != 2 {
		t.Fatalf("stream request = %+v", gotStreamReq)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("NDJSON lines = %d (%q); want 2 unique lines", len(lines), out.String())
	}
	var e1, e2 api.StreamEvent
	if err := json.Unmarshal([]byte(lines[0]), &e1); err != nil || e1.ID != "e1" {
		t.Fatalf("line 0 = %q (%v)", lines[0], err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &e2); err != nil || e2.ID != "e2" {
		t.Fatalf("line 1 = %q (%v)", lines[1], err)
	}
}

func TestRunWatchProjectLiveAndUntilRequest(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	t.Run("no_args_no_project_still_usage_error", func(t *testing.T) {
		ports, _, errOut := testPorts(&scriptedTransport{})
		cfg := testConfig()
		cfg.Project = "proj-ambient" // ambient config.Project alone must NOT bypass usage error when --project was not explicitly passed
		code := cli.Run(context.Background(), cfg, []string{"watch"}, ports)
		if code != cli.ExitUsage {
			t.Fatalf("exit = %d; want %d (stderr=%q)", code, cli.ExitUsage, errOut.String())
		}
	})

	t.Run("until_request_immediate_when_pending_present", func(t *testing.T) {
		streamOpened := false
		tr := &streamingScriptedTransport{
			scriptedTransport: &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
				switch inv.Operation {
				case "task.list":
					body, _ := json.Marshal(api.TaskListResult{Tasks: []api.TaskSummaryView{{TaskID: "task-1", State: "open", Title: "Deploy"}}})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "execution.list":
					body, _ := json.Marshal(api.ExecutionListResult{Executions: []api.ExecutionSummary{{ExecutionID: "exec-1", State: "running", TaskID: "task-1"}}})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "task.inbox":
					body, _ := json.Marshal(api.TaskInboxResult{Items: []api.InboxItem{{
						Kind: "request", ExecutionID: "exec-1", ActionID: "act-req", Version: 1, Assignment: "role/reviewer",
					}}})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				default:
					t.Fatalf("unexpected operation %s", inv.Operation)
					return api.Result{}, nil
				}
			}},
			streamFn: func(_ context.Context, _ client.EventStreamRequest) (client.EventStream, error) {
				streamOpened = true
				return newChanEventStream(), nil
			},
		}
		ports, out, errOut := testPorts(tr)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"watch", "--project", "proj-1", "--until", "request"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if streamOpened {
			t.Fatal("stream should not open when pending request already exists in initial snapshot")
		}
		if !strings.Contains(out.String(), "attention needed:") || !strings.Contains(out.String(), "act-req") {
			t.Fatalf("stdout missing attention item:\n%s", out.String())
		}
	})

	t.Run("until_request_waits_for_delayed_arrival_and_emits_ndjson", func(t *testing.T) {
		var (
			mu          sync.Mutex
			requestLive bool
			inboxCalls  int
			t2Done      = make(chan struct{})
			t2Once      sync.Once
		)
		stream := newChanEventStream()
		tr := &streamingScriptedTransport{
			scriptedTransport: &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
				switch inv.Operation {
				case "task.list":
					body, _ := json.Marshal(api.TaskListResult{Tasks: []api.TaskSummaryView{}})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "execution.list":
					body, _ := json.Marshal(api.ExecutionListResult{Executions: []api.ExecutionSummary{}})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "task.inbox":
					mu.Lock()
					inboxCalls++
					call := inboxCalls
					ready := requestLive
					mu.Unlock()
					if call == 2 {
						t2Once.Do(func() { close(t2Done) })
					}
					var items []api.InboxItem
					if ready {
						items = []api.InboxItem{{
							Kind: "waiting_task", TaskID: "task-9", Version: 2, Question: "Proceed with cutover?", Assignment: "role/engineer",
						}}
					}
					body, _ := json.Marshal(api.TaskInboxResult{Items: items})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				default:
					t.Fatalf("unexpected operation %s", inv.Operation)
					return api.Result{}, nil
				}
			}},
			streamFn: func(_ context.Context, req client.EventStreamRequest) (client.EventStream, error) {
				if req.ProjectID != "proj-1" {
					t.Fatalf("stream project = %q; want proj-1", req.ProjectID)
				}
				go func() {
					<-t2Done
					// First send an unrelated event, then make inbox ready and emit task.question.
					stream.ch <- api.StreamEvent{ID: "e1", Sequence: 1, Type: "execution.started", At: now, SubjectID: "exec-9"}
					mu.Lock()
					requestLive = true
					mu.Unlock()
					stream.ch <- api.StreamEvent{ID: "e2", Sequence: 2, Type: "task.question", At: now.Add(time.Second), SubjectID: "task-9", Data: json.RawMessage(`{"question":"Proceed with cutover?"}`)}
				}()
				return stream, nil
			},
		}

		ports, out, errOut := testPorts(tr)
		cfg := testConfig()
		code := cli.Run(context.Background(), cfg, []string{"watch", "--project", "proj-1", "--until", "request", "--output", "json"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 4 {
			t.Fatalf("NDJSON lines = %d (%q); want 4 (snapshot, event 1, event 2, attention)", len(lines), out.String())
		}
		var kinds []string
		for _, line := range lines {
			var obj map[string]any
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				t.Fatalf("invalid NDJSON line %q: %v", line, err)
			}
			kinds = append(kinds, obj["kind"].(string))
		}
		if strings.Join(kinds, ",") != "snapshot,event,event,attention" {
			t.Fatalf("NDJSON kinds = %v; want [snapshot event event attention]", kinds)
		}
	})
}

func TestRunInboxWaitImmediateDelayedStaleAndCancel(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	t.Run("immediate_return_preserves_stale_badge_and_pure_json", func(t *testing.T) {
		streamOpened := false
		tr := &streamingScriptedTransport{
			scriptedTransport: &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
				switch inv.Operation {
				case "task.inbox":
					body, _ := json.Marshal(api.TaskInboxResult{Items: []api.InboxItem{{
						Kind:             "request",
						ExecutionID:      "exec-1",
						ActionID:         "act-1",
						Version:          1,
						Assignment:       "role/reviewer",
						Stale:            true,
						CurrentRefCommit: "deadbeef1234567890",
					}}})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "execution.inspect":
					body, _ := json.Marshal(map[string]any{"execution_id": "exec-1", "task_id": "task-1"})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				default:
					t.Fatalf("unexpected op %s", inv.Operation)
					return api.Result{}, nil
				}
			}},
			streamFn: func(_ context.Context, _ client.EventStreamRequest) (client.EventStream, error) {
				streamOpened = true
				return newChanEventStream(), nil
			},
		}

		// Table preserves STALE badge.
		ports, out, errOut := testPorts(tr)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		if code := cli.Run(context.Background(), cfg, []string{"inbox", "--project", "proj-1", "--wait"}, ports); code != cli.ExitSuccess {
			t.Fatalf("table exit = %d; stderr=%q", code, errOut.String())
		}
		if streamOpened {
			t.Fatal("stream should not open on immediate hit")
		}
		if !strings.Contains(out.String(), "STALE (moved -> deadbeef1234)") {
			t.Fatalf("table missing STALE badge:\n%s", out.String())
		}

		// JSON is pure single JSON document.
		portsJSON, outJSON, errJSON := testPorts(tr)
		if code := cli.Run(context.Background(), cfg, []string{"inbox", "--project", "proj-1", "--wait", "--output", "json"}, portsJSON); code != cli.ExitSuccess {
			t.Fatalf("json exit = %d; stderr=%q", code, errJSON.String())
		}
		var parsed api.TaskInboxResult
		if err := json.Unmarshal(outJSON.Bytes(), &parsed); err != nil || len(parsed.Items) != 1 || !parsed.Items[0].Stale {
			t.Fatalf("json inbox = %q (%v)", outJSON.String(), err)
		}
	})

	t.Run("waits_until_actionable_event_arrives", func(t *testing.T) {
		var (
			mu         sync.Mutex
			ready      bool
			inboxCalls int
			t2Done     = make(chan struct{})
			t2Once     sync.Once
		)
		stream := newChanEventStream()
		tr := &streamingScriptedTransport{
			scriptedTransport: &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
				if inv.Operation != "task.inbox" {
					t.Fatalf("unexpected op %s", inv.Operation)
				}
				mu.Lock()
				inboxCalls++
				call := inboxCalls
				r := ready
				mu.Unlock()
				if call == 2 {
					t2Once.Do(func() { close(t2Done) })
				}
				var items []api.InboxItem
				if r {
					items = []api.InboxItem{{Kind: "failed_task", TaskID: "task-fail", Version: 3}}
				}
				body, _ := json.Marshal(api.TaskInboxResult{Items: items})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			}},
			streamFn: func(_ context.Context, _ client.EventStreamRequest) (client.EventStream, error) {
				go func() {
					<-t2Done
					stream.ch <- api.StreamEvent{ID: "e-ignore", Sequence: 1, Type: "observation.custom", At: now, SubjectID: "x"}
					mu.Lock()
					ready = true
					mu.Unlock()
					stream.ch <- api.StreamEvent{ID: "e-fail", Sequence: 2, Type: "execution.failed", At: now.Add(time.Second), SubjectID: "exec-fail"}
				}()
				return stream, nil
			},
		}
		ports, out, errOut := testPorts(tr)
		cfg := testConfig()
		if code := cli.Run(context.Background(), cfg, []string{"inbox", "--project", "proj-1", "--wait", "--output", "json"}, ports); code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		var parsed api.TaskInboxResult
		if err := json.Unmarshal(out.Bytes(), &parsed); err != nil || len(parsed.Items) != 1 || parsed.Items[0].TaskID != "task-fail" {
			t.Fatalf("json output = %q (%v)", out.String(), err)
		}
	})

	t.Run("cancellation_exits_cleanly", func(t *testing.T) {
		stream := newChanEventStream()
		ctx, cancel := context.WithCancel(context.Background())
		tr := &streamingScriptedTransport{
			scriptedTransport: &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
				body, _ := json.Marshal(api.TaskInboxResult{Items: []api.InboxItem{}})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			}},
			streamFn: func(_ context.Context, _ client.EventStreamRequest) (client.EventStream, error) {
				go func() {
					time.Sleep(15 * time.Millisecond)
					cancel()
				}()
				return stream, nil
			},
		}
		ports, out, errOut := testPorts(tr)
		cfg := testConfig()
		code := cli.Run(ctx, cfg, []string{"inbox", "--project", "proj-1", "--wait", "--output", "json"}, ports)
		if code != cli.ExitFailure {
			t.Fatalf("exit = %d; want %d", code, cli.ExitFailure)
		}
		if out.Len() != 0 {
			t.Fatalf("stdout should be empty on cancel; got %q", out.String())
		}
		if !strings.Contains(errOut.String(), "canceled") {
			t.Fatalf("stderr = %q; want canceled diagnostic", errOut.String())
		}
	})
}

func TestRunLogsFollowMultiActionAndRequestPause(t *testing.T) {
	t.Run("follows_multiple_container_actions_with_headers_and_drains_completely", func(t *testing.T) {
		inspectStep := 0
		act1Reads := 0
		act2Reads := 0
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				inspectStep++
				var body []byte
				switch inspectStep {
				case 1:
					body, _ = json.Marshal(map[string]any{
						"execution_id": "exec-multi", "state": "running", "current_action": 0,
						"actions": []map[string]any{
							{"action_id": "act-1", "index": 0, "name": "build", "kind": "container", "state": "running"},
							{"action_id": "act-2", "index": 1, "name": "test", "kind": "container", "state": "pending"},
						},
					})
				case 2:
					body, _ = json.Marshal(map[string]any{
						"execution_id": "exec-multi", "state": "running", "current_action": 1,
						"actions": []map[string]any{
							{"action_id": "act-1", "index": 0, "name": "build", "kind": "container", "state": "succeeded"},
							{"action_id": "act-2", "index": 1, "name": "test", "kind": "container", "state": "running"},
						},
					})
				default:
					body, _ = json.Marshal(map[string]any{
						"execution_id": "exec-multi", "state": "succeeded", "current_action": -1,
						"actions": []map[string]any{
							{"action_id": "act-1", "index": 0, "name": "build", "kind": "container", "state": "succeeded"},
							{"action_id": "act-2", "index": 1, "name": "test", "kind": "container", "state": "succeeded"},
						},
					})
				}
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "execution.logs.read":
				var req api.ExecutionLogsReadRequest
				_ = json.Unmarshal(inv.Request.Input, &req)
				switch req.ActionID {
				case "act-1":
					act1Reads++
					var chunks []api.LogChunk
					if act1Reads == 1 {
						chunks = []api.LogChunk{{ActionID: "act-1", Stream: "stdout", Offset: 0, Sequence: 1, Data: []byte("building...\n")}}
					} else if act1Reads == 2 {
						chunks = []api.LogChunk{{ActionID: "act-1", Stream: "stdout", Offset: 12, Sequence: 2, Data: []byte("build done\n")}}
					}
					body, _ := json.Marshal(api.ExecutionLogsReadResult{Chunks: chunks})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "act-2":
					act2Reads++
					var chunks []api.LogChunk
					if act2Reads == 1 {
						chunks = []api.LogChunk{{ActionID: "act-2", Stream: "stdout", Offset: 0, Sequence: 1, Data: []byte("testing...\n")}}
					} else if act2Reads == 2 {
						chunks = []api.LogChunk{{ActionID: "act-2", Stream: "stdout", Offset: 11, Sequence: 2, Data: []byte("all tests passed\n")}}
					}
					body, _ := json.Marshal(api.ExecutionLogsReadResult{Chunks: chunks})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				default:
					t.Fatalf("unexpected action %s", req.ActionID)
					return api.Result{}, nil
				}
			default:
				t.Fatalf("unexpected op %s", inv.Operation)
				return api.Result{}, nil
			}
		}}

		ports, out, errOut := testPorts(transport)
		ports.Sleep = func(time.Duration) {}
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"logs", "--follow", "exec-multi"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		want := "==> action build (act-1) <==\nbuilding...\nbuild done\n==> action test (act-2) <==\ntesting...\nall tests passed\n"
		if out.String() != want {
			t.Fatalf("stdout =\n%q\nwant:\n%q", out.String(), want)
		}
	})

	t.Run("pauses_cleanly_at_waiting_request_action_and_keeps_json_pure_ndjson", func(t *testing.T) {
		inspectStep := 0
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				inspectStep++
				var body []byte
				if inspectStep == 1 {
					body, _ = json.Marshal(map[string]any{
						"execution_id": "exec-req", "state": "running", "current_action": 0,
						"actions": []map[string]any{
							{"action_id": "act-1", "index": 0, "name": "prepare", "kind": "container", "state": "running"},
							{"action_id": "act-2", "index": 1, "name": "approve", "kind": "request", "state": "pending", "assignment": "role/reviewer"},
							{"action_id": "act-3", "index": 2, "name": "deploy", "kind": "container", "state": "pending"},
						},
					})
				} else {
					body, _ = json.Marshal(map[string]any{
						"execution_id": "exec-req", "state": "running", "current_action": 1,
						"actions": []map[string]any{
							{"action_id": "act-1", "index": 0, "name": "prepare", "kind": "container", "state": "succeeded"},
							{"action_id": "act-2", "index": 1, "name": "approve", "kind": "request", "state": "running", "assignment": "role/reviewer"},
							{"action_id": "act-3", "index": 2, "name": "deploy", "kind": "container", "state": "pending"},
						},
					})
				}
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "execution.logs.read":
				var req api.ExecutionLogsReadRequest
				_ = json.Unmarshal(inv.Request.Input, &req)
				if req.ActionID != "act-1" {
					t.Fatalf("must not read logs for non-container/pending action %s", req.ActionID)
				}
				var chunks []api.LogChunk
				if inspectStep == 1 {
					chunks = []api.LogChunk{{ActionID: "act-1", Stream: "stdout", Offset: 0, Sequence: 1, Data: []byte("prep line 1\n")}}
				} else {
					chunks = []api.LogChunk{{ActionID: "act-1", Stream: "stdout", Offset: 12, Sequence: 2, Data: []byte("prep line 2\n")}}
				}
				body, _ := json.Marshal(api.ExecutionLogsReadResult{Chunks: chunks})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected op %s", inv.Operation)
				return api.Result{}, nil
			}
		}}

		ports, out, errOut := testPorts(transport)
		ports.Sleep = func(time.Duration) {}
		cfg := testConfig()
		code := cli.Run(context.Background(), cfg, []string{"logs", "--follow", "exec-req", "--output", "json"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if !strings.Contains(errOut.String(), "paused") || !strings.Contains(errOut.String(), "approve") {
			t.Fatalf("stderr = %q; want concise request-pause notice", errOut.String())
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 2 {
			t.Fatalf("NDJSON lines = %d (%q); want 2", len(lines), out.String())
		}
		for i, line := range lines {
			var obj map[string]any
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				t.Fatalf("line %d is not valid JSON: %v (%q)", i, err, line)
			}
			if obj["action_id"] != "act-1" || obj["action_name"] != "prepare" {
				t.Fatalf("line %d missing action identity: %v", i, obj)
			}
		}
	})

	t.Run("skips_already_passed_request_action_to_follow_next_container", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				body, _ := json.Marshal(map[string]any{
					"execution_id": "exec-after-req", "state": "succeeded", "current_action": -1,
					"actions": []map[string]any{
						{"action_id": "act-1", "index": 0, "name": "build", "kind": "container", "state": "succeeded"},
						{"action_id": "act-2", "index": 1, "name": "approve", "kind": "request", "state": "succeeded", "decision": "pass"},
						{"action_id": "act-3", "index": 2, "name": "deploy", "kind": "container", "state": "succeeded"},
					},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "execution.logs.read":
				var req api.ExecutionLogsReadRequest
				_ = json.Unmarshal(inv.Request.Input, &req)
				switch req.ActionID {
				case "act-1":
					body, _ := json.Marshal(api.ExecutionLogsReadResult{
						Chunks: []api.LogChunk{{ActionID: "act-1", Stream: "stdout", Offset: 0, Sequence: 1, Data: []byte("built\n")}},
					})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "act-3":
					body, _ := json.Marshal(api.ExecutionLogsReadResult{
						Chunks: []api.LogChunk{{ActionID: "act-3", Stream: "stdout", Offset: 0, Sequence: 1, Data: []byte("deployed\n")}},
					})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				default:
					t.Fatalf("must skip request action %s", req.ActionID)
					return api.Result{}, nil
				}
			default:
				t.Fatalf("unexpected op %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"logs", "--follow", "exec-after-req"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		want := "==> action build (act-1) <==\nbuilt\n==> action deploy (act-3) <==\ndeployed\n"
		if out.String() != want {
			t.Fatalf("stdout =\n%q\nwant:\n%q", out.String(), want)
		}
	})

	t.Run("explicit_action_single_follow_unchanged", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				body, _ := json.Marshal(map[string]any{
					"execution_id": "exec-exp", "state": "running", "current_action": 1,
					"actions": []map[string]any{
						{"action_id": "act-1", "index": 0, "name": "build", "kind": "container", "state": "succeeded"},
						{"action_id": "act-2", "index": 1, "name": "test", "kind": "container", "state": "running"},
					},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "execution.logs.read":
				var req api.ExecutionLogsReadRequest
				_ = json.Unmarshal(inv.Request.Input, &req)
				if req.ActionID != "act-1" {
					t.Fatalf("explicit --action build read %s; want act-1 only", req.ActionID)
				}
				body, _ := json.Marshal(api.ExecutionLogsReadResult{
					Chunks: []api.LogChunk{{ActionID: "act-1", Stream: "stdout", Offset: 0, Sequence: 1, Data: []byte("only build\n")}},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected op %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"logs", "--follow", "--action", "build", "exec-exp"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if out.String() != "only build\n" {
			t.Fatalf("stdout = %q; want %q without transition header", out.String(), "only build\n")
		}
	})
}
