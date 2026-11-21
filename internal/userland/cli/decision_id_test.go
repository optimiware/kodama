package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/userland/cli"
)

func TestCLIApproveAndInspectSurfaceDecisionID(t *testing.T) {
	t.Run("approve prints decision_id in table and json output", func(t *testing.T) {
		transport := &reviewLifecycleTransport{
			respond: func(inv api.Invocation) (api.Result, error) {
				switch inv.Operation {
				case "execution.inspect":
					body, _ := json.Marshal(api.ExecutionInspectResult{
						ExecutionID: "exec-1",
						State:       "running",
						Version:     2,
						Actions: []api.ExecutionAction{{
							ActionID:   "act-review",
							Index:      0,
							Name:       "review",
							Kind:       "request",
							State:      "running",
							Version:    2,
							Assignment: "role/reviewer",
						}},
						Cleanup: api.ExecutionCleanup{State: "none"},
					})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				case "execution.request.resolve":
					body, _ := json.Marshal(api.ExecutionRequestResolveResult{
						ExecutionVersion: 3,
						ActionVersion:    3,
						DecisionID:       "dec_0123456789abcdef0123456789abcdef",
					})
					return api.Result{Operation: inv.Operation, Body: body}, nil
				default:
					t.Fatalf("unexpected operation %q", inv.Operation)
					return api.Result{}, nil
				}
			},
		}

		var out, errOut bytes.Buffer
		code := cli.Run(context.Background(), cli.Config{ServerURL: "https://kodema.example"},
			[]string{"approve", "exec-1", "--comment", "lgtm"},
			cli.Ports{Out: &out, Err: &errOut, Client: transport})
		if code != cli.ExitSuccess {
			t.Fatalf("approve exit=%d stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "decision_id=dec_0123456789abcdef0123456789abcdef") {
			t.Fatalf("approve table output missing decision_id:\n%s", out.String())
		}

		out.Reset()
		errOut.Reset()
		code = cli.Run(context.Background(), cli.Config{ServerURL: "https://kodema.example"},
			[]string{"approve", "exec-1", "--output", "json"},
			cli.Ports{Out: &out, Err: &errOut, Client: transport})
		if code != cli.ExitSuccess {
			t.Fatalf("approve --output json exit=%d stderr=%q", code, errOut.String())
		}
		var parsed api.ExecutionRequestResolveResult
		if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
			t.Fatalf("unmarshal json output: %v\n%s", err, out.String())
		}
		if parsed.DecisionID != "dec_0123456789abcdef0123456789abcdef" {
			t.Fatalf("json decision_id = %q", parsed.DecisionID)
		}
	})

	t.Run("inspect displays decision_id in table and json output", func(t *testing.T) {
		answered := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
		transport := &reviewLifecycleTransport{
			respond: func(inv api.Invocation) (api.Result, error) {
				body, _ := json.Marshal(api.ExecutionInspectResult{
					ExecutionID: "exec-1",
					TaskID:      "task-1",
					State:       "succeeded",
					Version:     3,
					Actions: []api.ExecutionAction{{
						ActionID:   "act-review",
						Index:      0,
						Name:       "review",
						Kind:       "request",
						State:      "succeeded",
						Version:    3,
						Assignment: "role/reviewer",
						Decision:   "pass",
						DecisionID: "dec_fedcba9876543210fedcba9876543210",
						Actor:      "reviewer-1",
						AnsweredAt: &answered,
					}},
					Cleanup: api.ExecutionCleanup{State: "complete"},
				})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			},
		}

		var out, errOut bytes.Buffer
		code := cli.Run(context.Background(), cli.Config{ServerURL: "https://kodema.example"},
			[]string{"inspect", "exec-1"},
			cli.Ports{Out: &out, Err: &errOut, Client: transport})
		if code != cli.ExitSuccess {
			t.Fatalf("inspect exit=%d stderr=%q", code, errOut.String())
		}
		if !strings.Contains(out.String(), "decision_id=dec_fedcba9876543210fedcba9876543210") {
			t.Fatalf("inspect table output missing decision_id:\n%s", out.String())
		}

		out.Reset()
		errOut.Reset()
		code = cli.Run(context.Background(), cli.Config{ServerURL: "https://kodema.example"},
			[]string{"inspect", "exec-1", "--output", "json"},
			cli.Ports{Out: &out, Err: &errOut, Client: transport})
		if code != cli.ExitSuccess {
			t.Fatalf("inspect --output json exit=%d stderr=%q", code, errOut.String())
		}
		var parsed api.ExecutionInspectResult
		if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
			t.Fatalf("unmarshal inspect json: %v", err)
		}
		if len(parsed.Actions) != 1 || parsed.Actions[0].DecisionID != "dec_fedcba9876543210fedcba9876543210" {
			t.Fatalf("inspect json action decision_id = %+v", parsed.Actions)
		}
	})
}
