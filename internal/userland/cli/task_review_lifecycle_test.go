package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/userland/cli"
)

type reviewLifecycleTransport struct {
	respond func(api.Invocation) (api.Result, error)
}

func (tr *reviewLifecycleTransport) Invoke(_ context.Context, inv api.Invocation) (api.Result, error) {
	return tr.respond(inv)
}

func TestCLITaskCompleteParentAndCompletesTaskFlags(t *testing.T) {
	t.Run("task complete auto-fetches version and sends reason", func(t *testing.T) {
		var seenOps []string
		var completeReq api.TaskCompleteRequest
		var completeVersion uint64

		transport := &reviewLifecycleTransport{
			respond: func(inv api.Invocation) (api.Result, error) {
				seenOps = append(seenOps, inv.Operation)
				switch inv.Operation {
				case "task.read":
					body, _ := json.Marshal(api.TaskReadResult{
						TaskID:           "task-1",
						ParentID:         "task-parent",
						Title:            "Child",
						State:            "open",
						ContentVersion:   1,
						LifecycleVersion: 4,
						Initiator:        "user-1",
					})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "task.complete":
					if inv.Request.ExpectedVersion != nil {
						completeVersion = *inv.Request.ExpectedVersion
					}
					_ = json.Unmarshal(inv.Request.Input, &completeReq)
					body, _ := json.Marshal(api.TaskCompleteResult{LifecycleVersion: 5})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				default:
					t.Fatalf("unexpected operation %q", inv.Operation)
					return api.Result{}, nil
				}
			},
		}

		var out, errOut bytes.Buffer
		code := cli.Run(context.Background(), cli.Config{ServerURL: "https://kodema.example"},
			[]string{"task", "complete", "task-1", "--reason", "all stages verified"},
			cli.Ports{Out: &out, Err: &errOut, Client: transport})
		if code != cli.ExitSuccess {
			t.Fatalf("task complete exit = %d; stderr=%q", code, errOut.String())
		}
		if len(seenOps) != 2 || seenOps[0] != "task.read" || seenOps[1] != "task.complete" {
			t.Fatalf("operations = %v; want [task.read task.complete]", seenOps)
		}
		if completeVersion != 4 || completeReq.TaskID != "task-1" || completeReq.Reason != "all stages verified" {
			t.Fatalf("task.complete sent ver=%d req=%+v", completeVersion, completeReq)
		}
		if !strings.Contains(out.String(), "completed task task-1 (lifecycle_version=5)") {
			t.Fatalf("stdout = %q", out.String())
		}
	})

	t.Run("task complete respects explicit --version without extra read", func(t *testing.T) {
		var calls int
		var gotVer uint64
		transport := &reviewLifecycleTransport{
			respond: func(inv api.Invocation) (api.Result, error) {
				calls++
				if inv.Operation != "task.complete" {
					t.Fatalf("unexpected op %q", inv.Operation)
				}
				if inv.Request.ExpectedVersion != nil {
					gotVer = *inv.Request.ExpectedVersion
				}
				body, _ := json.Marshal(api.TaskCompleteResult{LifecycleVersion: 8})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			},
		}
		var out, errOut bytes.Buffer
		code := cli.Run(context.Background(), cli.Config{ServerURL: "https://kodema.example"},
			[]string{"task", "complete", "task-1", "--version", "7"},
			cli.Ports{Out: &out, Err: &errOut, Client: transport})
		if code != cli.ExitSuccess || calls != 1 || gotVer != 7 {
			t.Fatalf("code=%d calls=%d ver=%d stderr=%q", code, calls, gotVer, errOut.String())
		}
	})

	t.Run("task create/run/list/show parent and completes-task flags", func(t *testing.T) {
		var createdReq api.TaskCreateRequest
		var runReq api.TaskCreateAndStartRequest
		var startReq api.TaskStartRequest
		var restartReq api.TaskRestartRequest
		var listReq api.TaskListRequest

		transport := &reviewLifecycleTransport{
			respond: func(inv api.Invocation) (api.Result, error) {
				switch inv.Operation {
				case "task.create":
					_ = json.Unmarshal(inv.Request.Input, &createdReq)
					body, _ := json.Marshal(api.TaskCreateResult{TaskID: "task-c1", ContentVersion: 1, LifecycleVersion: 1})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "task.create_and_start":
					_ = json.Unmarshal(inv.Request.Input, &runReq)
					body, _ := json.Marshal(api.TaskCreateAndStartResult{TaskID: "task-c2", ExecutionID: "exec-2", LifecycleVersion: 2})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "task.start":
					_ = json.Unmarshal(inv.Request.Input, &startReq)
					body, _ := json.Marshal(api.TaskStartResult{ExecutionID: "exec-3", LifecycleVersion: 3})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "task.restart":
					_ = json.Unmarshal(inv.Request.Input, &restartReq)
					body, _ := json.Marshal(api.TaskRestartResult{ExecutionID: "exec-4", LifecycleVersion: 4})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "task.list":
					_ = json.Unmarshal(inv.Request.Input, &listReq)
					body, _ := json.Marshal(api.TaskListResult{Tasks: []api.TaskSummaryView{
						{TaskID: "task-c1", ParentID: "task-p1", Title: "Child 1", State: "open", ContentVersion: 1, LifecycleVersion: 1},
					}})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "task.read":
					body, _ := json.Marshal(api.TaskReadResult{
						TaskID: "task-c1", ParentID: "task-p1", Title: "Child 1", State: "open",
						ContentVersion: 1, LifecycleVersion: 2, Initiator: "user-1",
					})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				default:
					t.Fatalf("unexpected op %q", inv.Operation)
					return api.Result{}, nil
				}
			},
		}

		cfg := cli.Config{ServerURL: "https://kodema.example", Project: "proj-1"}
		var out, errOut bytes.Buffer
		if code := cli.Run(context.Background(), cfg, []string{"task", "create", "--title", "Child 1", "--parent", "task-p1"}, cli.Ports{Out: &out, Err: &errOut, Client: transport}); code != cli.ExitSuccess {
			t.Fatalf("task create failed: %s", errOut.String())
		}
		if createdReq.ParentID != "task-p1" {
			t.Fatalf("task.create ParentID = %q; want task-p1", createdReq.ParentID)
		}

		if code := cli.Run(context.Background(), cfg, []string{"task", "run", "--title", "Child 2", "--parent", "task-p1", "--pipeline", "pipe-1", "--no-complete"}, cli.Ports{Out: &out, Err: &errOut, Client: transport}); code != cli.ExitSuccess {
			t.Fatalf("task run failed: %s", errOut.String())
		}
		if runReq.ParentID != "task-p1" || runReq.CompletesTask == nil || *runReq.CompletesTask {
			t.Fatalf("task.create_and_start req = %+v; want parent=task-p1 completes_task=false", runReq)
		}

		if code := cli.Run(context.Background(), cfg, []string{"task", "start", "task-c1", "--expected-version", "2", "--completes-task=false"}, cli.Ports{Out: &out, Err: &errOut, Client: transport}); code != cli.ExitSuccess {
			t.Fatalf("task start failed: %s", errOut.String())
		}
		if startReq.CompletesTask == nil || *startReq.CompletesTask {
			t.Fatalf("task.start CompletesTask = %v; want false", startReq.CompletesTask)
		}

		if code := cli.Run(context.Background(), cfg, []string{"task", "restart", "task-c1", "--expected-version", "3", "--no-complete"}, cli.Ports{Out: &out, Err: &errOut, Client: transport}); code != cli.ExitSuccess {
			t.Fatalf("task restart failed: %s", errOut.String())
		}
		if restartReq.CompletesTask == nil || *restartReq.CompletesTask {
			t.Fatalf("task.restart CompletesTask = %v; want false", restartReq.CompletesTask)
		}

		if code := cli.Run(context.Background(), cfg, []string{"task", "list", "--parent", "task-p1"}, cli.Ports{Out: &out, Err: &errOut, Client: transport}); code != cli.ExitSuccess {
			t.Fatalf("task list failed: %s", errOut.String())
		}
		if listReq.ParentID != "task-p1" {
			t.Fatalf("task.list ParentID = %q; want task-p1", listReq.ParentID)
		}

		out.Reset()
		if code := cli.Run(context.Background(), cfg, []string{"task", "show", "task-c1"}, cli.Ports{Out: &out, Err: &errOut, Client: transport}); code != cli.ExitSuccess {
			t.Fatalf("task show failed: %s", errOut.String())
		}
		if !strings.Contains(out.String(), "parent_id\ttask-p1") {
			t.Fatalf("task show output missing parent_id:\n%s", out.String())
		}
	})
}

func TestCLIReviseAndRejectDecisionRevise(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"revise command", []string{"revise", "exec-1", "--reason", "Add tests for nil case"}},
		{"reject --decision revise", []string{"reject", "exec-1", "--decision", "revise", "--reason", "Add tests for nil case"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resolvedReq api.ExecutionRequestResolveRequest
			transport := &reviewLifecycleTransport{
				respond: func(inv api.Invocation) (api.Result, error) {
					switch inv.Operation {
					case "execution.inspect":
						body, _ := json.Marshal(api.ExecutionInspectResult{
							ExecutionID: "exec-1",
							State:       "running",
							Version:     3,
							Actions: []api.ExecutionAction{
								{ActionID: "act-review", Index: 0, Name: "review", Kind: "request", State: "running", Version: 2, Assignment: "role/reviewer"},
							},
							Cleanup: api.ExecutionCleanup{State: "none"},
						})
						return api.Result{Operation: inv.Operation, Body: body}, nil
					case "execution.request.resolve":
						_ = json.Unmarshal(inv.Request.Input, &resolvedReq)
						body, _ := json.Marshal(api.ExecutionRequestResolveResult{ExecutionVersion: 4, ActionVersion: 3})
						return api.Result{Operation: inv.Operation, Body: body}, nil
					default:
						t.Fatalf("unexpected op %q", inv.Operation)
						return api.Result{}, nil
					}
				},
			}
			var out, errOut bytes.Buffer
			code := cli.Run(context.Background(), cli.Config{ServerURL: "https://kodema.example"}, tc.args, cli.Ports{Out: &out, Err: &errOut, Client: transport})
			if code != cli.ExitSuccess {
				t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
			}
			if resolvedReq.Decision != "revise" || resolvedReq.Note != "Add tests for nil case" || resolvedReq.ActionID != "act-review" {
				t.Fatalf("resolve req = %+v; want decision=revise note='Add tests for nil case'", resolvedReq)
			}
			if !strings.Contains(out.String(), "decision=revise") {
				t.Fatalf("stdout = %q; want decision=revise", out.String())
			}
		})
	}
}
