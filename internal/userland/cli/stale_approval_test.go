package cli_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/userland/cli"
)

func TestInboxStaleRenderingTableAndJSON(t *testing.T) {
	const (
		shaBase = "1111111111111111111111111111111111111111"
		shaHead = "abcdef1234567890abcdef1234567890abcdef12"
		shaCand = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)

	makeInboxResult := func() api.TaskInboxResult {
		return api.TaskInboxResult{
			Items: []api.InboxItem{
				{
					Kind:        "request",
					TaskID:      "task-1",
					ExecutionID: "exec-stale",
					ActionID:    "act-stale",
					Assignment:  "role/reviewer",
					Subject: &api.Subject{Fields: []api.SubjectField{
						{Name: "candidate", Kind: "string", Value: shaCand},
						{Name: "expected_base", Kind: "string", Value: shaBase},
						{Name: "operation", Kind: "string", Value: "git.ref.update"},
						{Name: "ref", Kind: "string", Value: "refs/heads/main"},
						{Name: "resource", Kind: "string", Value: "repo-1"},
					}},
					Stale:            true,
					CurrentRefCommit: shaHead,
					StaleReason:      "ref refs/heads/main on repo-1 moved from expected_base " + shaBase + " to " + shaHead,
					Version:          2,
					At:               testNow(),
				},
				{
					Kind:        "request",
					TaskID:      "task-2",
					ExecutionID: "exec-deleted",
					ActionID:    "act-deleted",
					Assignment:  "role/reviewer",
					Subject: &api.Subject{Fields: []api.SubjectField{
						{Name: "expected_base", Kind: "string", Value: shaBase},
						{Name: "operation", Kind: "string", Value: "git.ref.update"},
						{Name: "ref", Kind: "string", Value: "refs/heads/release"},
						{Name: "resource", Kind: "string", Value: "repo-1"},
					}},
					Stale:       true,
					StaleReason: "ref refs/heads/release on repo-1 was deleted",
					Version:     2,
					At:          testNow(),
				},
				{
					Kind:        "request",
					TaskID:      "task-3",
					ExecutionID: "exec-fresh",
					ActionID:    "act-fresh",
					Assignment:  "role/reviewer",
					Subject: &api.Subject{Fields: []api.SubjectField{
						{Name: "candidate", Kind: "string", Value: shaCand},
						{Name: "expected_base", Kind: "string", Value: shaHead},
						{Name: "operation", Kind: "string", Value: "git.ref.update"},
						{Name: "ref", Kind: "string", Value: "refs/heads/main"},
						{Name: "resource", Kind: "string", Value: "repo-1"},
					}},
					Version: 2,
					At:      testNow(),
				},
			},
		}
	}

	t.Run("table output renders STALE badge with short sha", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "task.inbox":
				body, _ := json.Marshal(makeInboxResult())
				return api.Result{Operation: inv.Operation, Body: body}, nil
			case "execution.inspect":
				return api.Result{}, &api.Error{Code: api.CodeForbidden, Message: "denied"}
			default:
				t.Fatalf("unexpected operation %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{"inbox", "--project", "proj-1"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		rendered := out.String()
		wantBadge := "STALE (moved -> " + shaHead[:12] + ")"
		if !strings.Contains(rendered, wantBadge) {
			t.Fatalf("inbox table missing %q:\n%s", wantBadge, rendered)
		}
		if !strings.Contains(rendered, "STALE (deleted)") {
			t.Fatalf("inbox table missing STALE (deleted):\n%s", rendered)
		}
		for _, line := range strings.Split(rendered, "\n") {
			if strings.Contains(line, "exec-fresh") && strings.Contains(line, "STALE") {
				t.Fatalf("fresh inbox row unexpectedly rendered STALE: %q", line)
			}
		}
	})

	t.Run("json output preserves full stale fields", func(t *testing.T) {
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			body, _ := json.Marshal(makeInboxResult())
			return api.Result{Operation: inv.Operation, Body: body}, nil
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		code := cli.Run(context.Background(), cfg, []string{"inbox", "--project", "proj-1", "--output", "json"}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		var parsed api.TaskInboxResult
		if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
			t.Fatalf("unmarshal json inbox: %v\n%s", err, out.String())
		}
		if len(parsed.Items) != 3 {
			t.Fatalf("items = %d; want 3", len(parsed.Items))
		}
		if !parsed.Items[0].Stale || parsed.Items[0].CurrentRefCommit != shaHead || !strings.Contains(parsed.Items[0].StaleReason, shaBase) {
			t.Fatalf("json stale item = %+v", parsed.Items[0])
		}
		if parsed.Items[2].Stale || parsed.Items[2].CurrentRefCommit != "" || parsed.Items[2].StaleReason != "" {
			t.Fatalf("json fresh item = %+v", parsed.Items[2])
		}
	})
}

func TestApproveAllowStaleAndCandidateGuard(t *testing.T) {
	const (
		shaCand  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		shaWrong = "9999999999999999999999999999999999999999"
	)

	inspectWithCandidate := func(candidate string) []byte {
		fields := []api.SubjectField{
			{Name: "operation", Kind: "string", Value: "git.ref.update"},
			{Name: "resource", Kind: "string", Value: "repo-1"},
			{Name: "ref", Kind: "string", Value: "refs/heads/main"},
		}
		if candidate != "" {
			fields = append(fields, api.SubjectField{Name: "candidate", Kind: "string", Value: candidate})
		}
		body, _ := json.Marshal(map[string]any{
			"execution_id": "exec-7",
			"state":        "running",
			"version":      4,
			"actions": []map[string]any{
				{
					"action_id": "act-gate",
					"name":      "gate",
					"kind":      "request",
					"state":     "running",
					"version":   3,
					"subject":   api.Subject{Fields: fields},
				},
			},
		})
		return body
	}

	t.Run("approve --allow-stale --candidate matching succeeds and sets allow_stale=true", func(t *testing.T) {
		var sent api.ExecutionRequestResolveRequest
		transport := &scriptedTransport{respond: func(_ context.Context, idx int, inv api.Invocation) (api.Result, error) {
			switch idx {
			case 0:
				if inv.Operation != "execution.inspect" {
					t.Fatalf("call 0 = %s; want execution.inspect", inv.Operation)
				}
				return api.Result{Operation: inv.Operation, Body: inspectWithCandidate(shaCand)}, nil
			case 1:
				if inv.Operation != "execution.request.resolve" {
					t.Fatalf("call 1 = %s; want execution.request.resolve", inv.Operation)
				}
				if err := json.Unmarshal(inv.Request.Input, &sent); err != nil {
					t.Fatalf("unmarshal resolve: %v", err)
				}
				body, _ := json.Marshal(api.ExecutionRequestResolveResult{ExecutionVersion: 5, ActionVersion: 4})
				return api.Result{Operation: inv.Operation, Body: body}, nil
			default:
				t.Fatalf("unexpected call %d", idx)
				return api.Result{}, nil
			}
		}}
		ports, out, errOut := testPorts(transport)
		cfg := testConfig()
		cfg.Output = cli.OutputTable
		code := cli.Run(context.Background(), cfg, []string{
			"approve", "exec-7", "--allow-stale", "--candidate", shaCand,
		}, ports)
		if code != cli.ExitSuccess {
			t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
		}
		if !sent.AllowStale || sent.Decision != "pass" || sent.ActionID != "act-gate" {
			t.Fatalf("resolve req = %+v; want AllowStale=true Decision=pass ActionID=act-gate", sent)
		}
		if !strings.Contains(out.String(), "approved execution exec-7 action act-gate") {
			t.Fatalf("stdout = %q", out.String())
		}
	})

	t.Run("approve --candidate still inspects and guards even with explicit --action and --expected-action-version", func(t *testing.T) {
		resolveCalls := 0
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				return api.Result{Operation: inv.Operation, Body: inspectWithCandidate(shaCand)}, nil
			case "execution.request.resolve":
				resolveCalls++
				return api.Result{}, nil
			default:
				t.Fatalf("unexpected op %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		ports, _, errOut := testPorts(transport)
		code := cli.Run(context.Background(), testConfig(), []string{
			"approve", "exec-7", "--action", "act-gate", "--expected-action-version", "3", "--candidate", shaWrong,
		}, ports)
		if code == cli.ExitSuccess {
			t.Fatalf("mismatched --candidate unexpectedly succeeded")
		}
		if resolveCalls != 0 {
			t.Fatalf("execution.request.resolve was called %d times on candidate mismatch; want 0", resolveCalls)
		}
		for _, want := range []string{"candidate mismatch", shaCand, shaWrong} {
			if !strings.Contains(errOut.String(), want) {
				t.Errorf("stderr %q missing %q", errOut.String(), want)
			}
		}
	})

	t.Run("approve --candidate refuses when request subject has no candidate field", func(t *testing.T) {
		resolveCalls := 0
		transport := &scriptedTransport{respond: func(_ context.Context, _ int, inv api.Invocation) (api.Result, error) {
			switch inv.Operation {
			case "execution.inspect":
				return api.Result{Operation: inv.Operation, Body: inspectWithCandidate("")}, nil
			case "execution.request.resolve":
				resolveCalls++
				return api.Result{}, nil
			default:
				t.Fatalf("unexpected op %s", inv.Operation)
				return api.Result{}, nil
			}
		}}
		ports, _, errOut := testPorts(transport)
		code := cli.Run(context.Background(), testConfig(), []string{
			"approve", "exec-7", "--candidate", shaCand,
		}, ports)
		if code == cli.ExitSuccess {
			t.Fatalf("absent subject.candidate unexpectedly succeeded")
		}
		if resolveCalls != 0 {
			t.Fatalf("execution.request.resolve was called %d times; want 0", resolveCalls)
		}
		if !strings.Contains(errOut.String(), "no subject.candidate field") {
			t.Fatalf("stderr = %q; want missing subject.candidate message", errOut.String())
		}
	})

	t.Run("approve --candidate empty value and reject --allow-stale return usage error", func(t *testing.T) {
		ports, _, errOut := testPorts(&scriptedTransport{})
		if code := cli.Run(context.Background(), testConfig(), []string{"approve", "exec-7", "--candidate", "   "}, ports); code != cli.ExitUsage {
			t.Fatalf("empty --candidate exit = %d; want %d (stderr=%q)", code, cli.ExitUsage, errOut.String())
		}
		ports2, _, errOut2 := testPorts(&scriptedTransport{})
		if code := cli.Run(context.Background(), testConfig(), []string{"reject", "exec-7", "--reason", "stale", "--allow-stale"}, ports2); code != cli.ExitUsage {
			t.Fatalf("reject --allow-stale exit = %d; want %d (stderr=%q)", code, cli.ExitUsage, errOut2.String())
		}
	})

	t.Run("help approve documents --candidate and --allow-stale", func(t *testing.T) {
		ports, out, errOut := testPorts(&scriptedTransport{})
		if code := cli.Run(context.Background(), cli.Config{}, []string{"help", "approve"}, ports); code != cli.ExitSuccess {
			t.Fatalf("help approve exit = %d; stderr=%q", code, errOut.String())
		}
		for _, want := range []string{"--candidate", "--allow-stale"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("help approve missing %q:\n%s", want, out.String())
			}
		}
	})
}
