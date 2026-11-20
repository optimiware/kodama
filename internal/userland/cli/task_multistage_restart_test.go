package cli_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/userland/cli"
)

func TestCLITaskStartWithPipelineNameInputsAndOutputRef(t *testing.T) {
	t.Run("task start resolves pipeline name and input-from-output from latest succeeded execution", func(t *testing.T) {
		taskReadBody, _ := json.Marshal(api.TaskReadResult{
			TaskID:           "task-10",
			Title:            "Ship feature",
			State:            "done",
			ContentVersion:   1,
			LifecycleVersion: 3,
			SelectedPipeline: "pipe-impl",
			Initiator:        "oidc-1",
			ExecutionHistory: []string{"exec-1", "exec-2"},
			CurrentExecution: "exec-2",
		})
		exec2Body, _ := json.Marshal(api.ExecutionInspectResult{
			ExecutionID: "exec-2",
			State:       "failed",
			Version:     2,
		})
		exec1Body, _ := json.Marshal(api.ExecutionInspectResult{
			ExecutionID: "exec-1",
			State:       "succeeded",
			Version:     3,
			Actions: []api.ExecutionAction{
				{
					ActionID: "act-push",
					Index:    0,
					Name:     "push",
					Kind:     "container",
					State:    "succeeded",
					Version:  2,
					Outputs:  map[string]string{"branch": "work/task-10", "commit": "abc123"},
				},
			},
		})
		pipeListBody, _ := json.Marshal(api.PipelineListResult{
			Pipelines: []api.PipelineSummaryView{
				{PipelineID: "pipe-integrate", Name: "integrate-stage", Version: 1},
			},
		})

		startCalls := 0
		var finalStartReq api.TaskStartRequest
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "task.read":
				return api.Result{Operation: inv.Operation, Body: taskReadBody}, nil
			case "execution.inspect":
				var req api.ExecutionInspectRequest
				_ = json.Unmarshal(inv.Request.Input, &req)
				if req.ExecutionID == "exec-2" {
					return api.Result{Operation: inv.Operation, Body: exec2Body}, nil
				}
				return api.Result{Operation: inv.Operation, Body: exec1Body}, nil
			case "pipeline.list":
				return api.Result{Operation: inv.Operation, Body: pipeListBody}, nil
			case "task.start":
				startCalls++
				var req api.TaskStartRequest
				_ = json.Unmarshal(inv.Request.Input, &req)
				if req.PipelineID == "integrate-stage" {
					return api.Result{}, &api.Error{Code: api.CodeNotFound, Message: "pipeline not found"}
				}
				finalStartReq = req
				body, _ := json.Marshal(api.TaskStartResult{ExecutionID: "exec-3", LifecycleVersion: 4})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected operation %s", inv.Operation)
				return api.Result{}, nil
			}
		}}

		cfg := testConfig()
		cfg.Project = "shop"
		cfg.Output = cli.OutputTable
		ports, out, errOut := testPorts(transport)

		code := cli.Run(context.Background(), cfg, []string{
			"task", "start", "task-10",
			"--pipeline", "integrate-stage",
			"--input", "repository=shop/web",
			"--input-from-output", "branch=push.branch",
		}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if startCalls != 2 {
			t.Fatalf("task.start calls = %d; want 2 (id attempt + resolved name retry)", startCalls)
		}
		if finalStartReq.TaskID != "task-10" || finalStartReq.PipelineID != "pipe-integrate" {
			t.Fatalf("final task.start req = %+v", finalStartReq)
		}
		if finalStartReq.Inputs["repository"] != "shop/web" || finalStartReq.Inputs["branch"] != "work/task-10" {
			t.Fatalf("final task.start inputs = %+v", finalStartReq.Inputs)
		}
		if !strings.Contains(out.String(), "execution_id\texec-3") {
			t.Fatalf("stdout missing execution_id:\n%s", out.String())
		}
	})

	t.Run("task start resolves dotted pipeline name upfront", func(t *testing.T) {
		taskReadBody, _ := json.Marshal(api.TaskReadResult{
			TaskID: "task-11", Title: "T", State: "open", ContentVersion: 1, LifecycleVersion: 2, Initiator: "oidc-1",
		})
		pipeListBody, _ := json.Marshal(api.PipelineListResult{
			Pipelines: []api.PipelineSummaryView{{PipelineID: "pipe-v2", Name: "integrate.v2", Version: 1}},
		})
		var gotReq api.TaskStartRequest
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "task.read":
				return api.Result{Operation: inv.Operation, Body: taskReadBody}, nil
			case "pipeline.list":
				return api.Result{Operation: inv.Operation, Body: pipeListBody}, nil
			case "task.start":
				_ = json.Unmarshal(inv.Request.Input, &gotReq)
				body, _ := json.Marshal(api.TaskStartResult{ExecutionID: "exec-9", LifecycleVersion: 3})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected operation %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		cfg := testConfig()
		cfg.Project = "shop"
		ports, _, errOut := testPorts(transport)
		if code := cli.Run(context.Background(), cfg, []string{"task", "start", "task-11", "--pipeline", "integrate.v2"}, ports); code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if gotReq.PipelineID != "pipe-v2" {
			t.Fatalf("pipeline_id = %q; want pipe-v2", gotReq.PipelineID)
		}
	})

	t.Run("task start rejects invalid or colliding input-from-output", func(t *testing.T) {
		cfg := testConfig()
		ports, _, errOut := testPorts(&scriptedTransport{})
		if code := cli.Run(context.Background(), cfg, []string{"task", "start", "task-10", "--input-from-output", "badformat"}, ports); code != cli.ExitUsage {
			t.Fatalf("bad format exit = %d; want usage (%q)", code, errOut.String())
		}
	})
}

func TestCLITaskRestartFlagsOutputAndHelp(t *testing.T) {
	t.Run("task restart passes reason pipeline and inputs and renders superseded execution", func(t *testing.T) {
		taskReadBody, _ := json.Marshal(api.TaskReadResult{
			TaskID:           "task-20",
			Title:            "Integrate",
			State:            "open",
			ContentVersion:   1,
			LifecycleVersion: 6,
			SelectedPipeline: "pipe-integrate",
			Initiator:        "oidc-1",
			CurrentExecution: "exec-old",
		})
		var gotReq api.TaskRestartRequest
		var gotVer uint64
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "task.read":
				return api.Result{Operation: inv.Operation, Body: taskReadBody}, nil
			case "task.restart":
				if inv.Request.ExpectedVersion == nil || inv.Request.IdempotencyKey == "" {
					t.Fatalf("task.restart envelope missing version or key: %+v", inv.Request)
				}
				gotVer = *inv.Request.ExpectedVersion
				_ = json.Unmarshal(inv.Request.Input, &gotReq)
				body, _ := json.Marshal(api.TaskRestartResult{
					ExecutionID:         "exec-new",
					SupersededExecution: "exec-old",
					LifecycleVersion:    7,
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected op %s", inv.Operation)
				return api.Result{}, nil
			}
		}}

		cfg := testConfig()
		cfg.Output = cli.OutputTable
		ports, out, errOut := testPorts(transport)
		code := cli.Run(context.Background(), cfg, []string{
			"task", "restart", "task-20",
			"--reason", "rebase onto latest main",
			"--pipeline", "pipe-integrate",
			"--input", "branch=work/rebased",
		}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if gotVer != 6 {
			t.Fatalf("expected_version = %d; want 6", gotVer)
		}
		if gotReq.TaskID != "task-20" || gotReq.Reason != "rebase onto latest main" || gotReq.PipelineID != "pipe-integrate" || gotReq.Inputs["branch"] != "work/rebased" {
			t.Fatalf("task.restart req = %+v", gotReq)
		}
		rendered := out.String()
		for _, want := range []string{"task_id\ttask-20", "execution_id\texec-new", "superseded_execution\texec-old", "lifecycle_version\t7"} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("task restart output missing %q:\n%s", want, rendered)
			}
		}
	})

	t.Run("task help documents start flags and restart subcommand", func(t *testing.T) {
		cfg := testConfig()
		for _, tc := range []struct {
			argv []string
			want string
		}{
			{[]string{"task", "--help"}, "restart"},
			{[]string{"task", "start", "--help"}, "--pipeline"},
			{[]string{"task", "restart", "--help"}, "--reason"},
		} {
			ports, out, errOut := testPorts(&scriptedTransport{})
			if code := cli.Run(context.Background(), cfg, tc.argv, ports); code != cli.ExitSuccess {
				t.Fatalf("%v exit = %d; stderr=%q", tc.argv, code, errOut.String())
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("%v help missing %q:\n%s", tc.argv, tc.want, out.String())
			}
		}
	})
}
