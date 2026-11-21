package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/userland/cli"
)

func TestKVPutGetCLIFileStdinOutJSONAndWorkloadProjectAutoResolve(t *testing.T) {
	stored := map[string][]byte{}
	storedType := map[string]string{}
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

	transport := &scriptedTransport{
		respond: func(_ context.Context, _ int, invocation api.Invocation) (api.Result, error) {
			switch invocation.Operation {
			case "principal.whoami":
				body, _ := json.Marshal(api.PrincipalWhoamiResult{
					PrincipalID: "exec-p-1",
					Kind:        "workload",
					TenantID:    "tenant-1",
					ProjectID:   "proj-workload",
					Roles:       []string{},
				})
				return api.Result{Operation: "principal.whoami", Body: body}, nil
			case "kv.put":
				var req api.KVPutRequest
				if err := json.Unmarshal(invocation.Request.Input, &req); err != nil {
					t.Fatalf("unmarshal kv.put: %v", err)
				}
				data, _, err := req.DecodeBytes()
				if err != nil {
					t.Fatalf("DecodeBytes: %v", err)
				}
				sum := sha256.Sum256(data)
				key := api.KVKeyPrefix + hex.EncodeToString(sum[:])
				_, existed := stored[req.ProjectID+"/"+key]
				stored[req.ProjectID+"/"+key] = data
				ct := req.ContentType
				if ct == "" {
					ct = "application/octet-stream"
				}
				storedType[req.ProjectID+"/"+key] = ct
				body, _ := json.Marshal(api.KVPutResult{
					Key:         key,
					ProjectID:   req.ProjectID,
					SizeBytes:   int64(len(data)),
					ContentType: ct,
					CreatedAt:   now,
					Existed:     existed,
				})
				return api.Result{Operation: "kv.put", Body: body}, nil
			case "kv.get":
				var req api.KVGetRequest
				if err := json.Unmarshal(invocation.Request.Input, &req); err != nil {
					t.Fatalf("unmarshal kv.get: %v", err)
				}
				data := stored[req.ProjectID+"/"+req.Key]
				text := string(data)
				body, _ := json.Marshal(api.KVGetResult{
					Key:         req.Key,
					ProjectID:   req.ProjectID,
					SizeBytes:   int64(len(data)),
					ContentType: storedType[req.ProjectID+"/"+req.Key],
					CreatedBy:   "exec-p-1",
					CreatedAt:   now,
					DataBase64:  base64.StdEncoding.EncodeToString(data),
					Text:        &text,
				})
				return api.Result{Operation: "kv.get", Body: body}, nil
			default:
				t.Fatalf("unexpected operation %q", invocation.Operation)
				return api.Result{}, nil
			}
		},
	}

	cfg := testConfig()
	cfg.Output = cli.OutputTable

	// 1. Put from file with explicit --project and --content-type.
	ports, out, errOut := testPorts(transport)
	fs := ports.Files.(*fakeFS)
	fs.files["plan.md"] = []byte("# Plan\n\nDetails\n")
	code := cli.RunWithDeps(context.Background(), cfg, []string{"kv", "put", "plan.md", "--project", "proj-1", "--content-type", "text/markdown"}, ports, cli.CommandDeps{})
	if code != cli.ExitSuccess {
		t.Fatalf("kv put file exit=%d stderr=%q", code, errOut.String())
	}
	key := strings.TrimSpace(out.String())
	if !api.ValidKVKey(key) {
		t.Fatalf("kv put printed invalid key %q", key)
	}

	// 2. Get to stdout writes exact raw bytes (no added newline).
	ports2, out2, errOut2 := testPorts(transport)
	code = cli.RunWithDeps(context.Background(), cfg, []string{"kv", "get", key, "--project", "proj-1"}, ports2, cli.CommandDeps{})
	if code != cli.ExitSuccess {
		t.Fatalf("kv get stdout exit=%d stderr=%q", code, errOut2.String())
	}
	if out2.String() != "# Plan\n\nDetails\n" {
		t.Fatalf("kv get stdout = %q; want exact file bytes", out2.String())
	}

	// 3. Get to --out <file> writes exact bytes and keeps stdout empty.
	ports3, out3, errOut3 := testPorts(transport)
	code = cli.RunWithDeps(context.Background(), cfg, []string{"kv", "get", key, "--project", "proj-1", "--out", "downloaded.md"}, ports3, cli.CommandDeps{})
	if code != cli.ExitSuccess {
		t.Fatalf("kv get --out exit=%d stderr=%q", code, errOut3.String())
	}
	if out3.Len() != 0 {
		t.Fatalf("kv get --out wrote unexpected stdout: %q", out3.String())
	}
	if got := string(ports3.Files.(*fakeFS).files["downloaded.md"]); got != "# Plan\n\nDetails\n" {
		t.Fatalf("downloaded.md = %q; want exact bytes", got)
	}

	// 4. Get with --output json prints structured JSON.
	ports4, out4, errOut4 := testPorts(transport)
	code = cli.RunWithDeps(context.Background(), cfg, []string{"kv", "get", key, "--project", "proj-1", "--output", "json"}, ports4, cli.CommandDeps{})
	if code != cli.ExitSuccess {
		t.Fatalf("kv get --output json exit=%d stderr=%q", code, errOut4.String())
	}
	var gotJSON api.KVGetResult
	if err := json.Unmarshal(out4.Bytes(), &gotJSON); err != nil {
		t.Fatalf("decode kv get json: %v (%q)", err, out4.String())
	}
	if gotJSON.Key != key || gotJSON.ProjectID != "proj-1" {
		t.Fatalf("kv get json = %+v", gotJSON)
	}

	// 5. Workload container (KODEMA_EXECUTION set, --project omitted) puts stdin and gets back via auto-resolved project.
	ports5, out5, errOut5 := testPorts(transport)
	ports5.In = bytes.NewBufferString("from workload stdin")
	workloadCfg := cfg
	workloadCfg.Project = ""
	workloadDeps := cli.CommandDeps{
		Env: func(name string) string {
			if name == "KODEMA_EXECUTION" {
				return "exec-1"
			}
			return ""
		},
	}
	code = cli.RunWithDeps(context.Background(), workloadCfg, []string{"kv", "put", "--output", "json"}, ports5, workloadDeps)
	if code != cli.ExitSuccess {
		t.Fatalf("workload kv put stdin exit=%d stderr=%q", code, errOut5.String())
	}
	var putJSON api.KVPutResult
	if err := json.Unmarshal(out5.Bytes(), &putJSON); err != nil {
		t.Fatalf("decode workload kv put json: %v", err)
	}
	if putJSON.ProjectID != "proj-workload" {
		t.Fatalf("workload auto-resolved project = %q; want proj-workload", putJSON.ProjectID)
	}

	// 6. Malformed key refused with ExitUsage.
	ports6, _, errOut6 := testPorts(transport)
	code = cli.RunWithDeps(context.Background(), cfg, []string{"kv", "get", "../bad", "--project", "proj-1"}, ports6, cli.CommandDeps{})
	if code != cli.ExitUsage || !strings.Contains(errOut6.String(), "invalid kv key") {
		t.Fatalf("malformed key exit=%d stderr=%q; want usage error", code, errOut6.String())
	}

	// 7. Help topics for kv, kv put, kv get work cleanly.
	for _, args := range [][]string{{"help", "kv"}, {"help", "kv", "put"}, {"help", "kv", "get"}, {"kv", "--help"}} {
		var hOut, hErr bytes.Buffer
		if code := cli.Run(context.Background(), cli.Config{}, args, cli.Ports{Out: &hOut, Err: &hErr}); code != cli.ExitSuccess {
			t.Errorf("args %v exit=%d stderr=%q", args, code, hErr.String())
		}
	}
}
