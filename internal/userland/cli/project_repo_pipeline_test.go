package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/userland/cli"
)

type prpFakeTransport struct {
	invoke func(context.Context, api.Invocation) (api.Result, error)
}

func (f prpFakeTransport) Invoke(ctx context.Context, inv api.Invocation) (api.Result, error) {
	return f.invoke(ctx, inv)
}

func TestProjectCreateAndListCLI(t *testing.T) {
	t.Run("project create table and json", func(t *testing.T) {
		var seen []api.Invocation
		transport := prpFakeTransport{invoke: func(_ context.Context, inv api.Invocation) (api.Result, error) {
			seen = append(seen, inv)
			return api.Result{
				Operation: "project.create",
				Body:      json.RawMessage(`{"project_id":"payments","version":1}`),
			}, nil
		}}

		var out, errOut bytes.Buffer
		code := cli.Run(context.Background(), cli.Config{ServerURL: "https://api.example.com", Output: cli.OutputTable},
			[]string{"project", "create", "payments", "--name", "Payments", "--copy-members-from", "project-local"},
			cli.Ports{Client: transport, Out: &out, Err: &errOut})
		if code != cli.ExitSuccess {
			t.Fatalf("exit=%d stderr=%s", code, errOut.String())
		}
		if len(seen) != 1 || seen[0].Operation != "project.create" {
			t.Fatalf("invocations = %+v", seen)
		}
		var req api.ProjectCreateRequest
		if err := json.Unmarshal(seen[0].Request.Input, &req); err != nil {
			t.Fatalf("unmarshal input: %v", err)
		}
		if req.ProjectID != "payments" || req.Name != "Payments" || req.CopyMembersFrom != "project-local" || req.TenantID != "" {
			t.Fatalf("project.create req = %+v", req)
		}
		if !strings.Contains(out.String(), "project_id\tpayments") || !strings.Contains(out.String(), "copied_members_from\tproject-local") {
			t.Fatalf("table stdout = %q", out.String())
		}

		// Default name = id and --output json.
		out.Reset()
		errOut.Reset()
		code = cli.Run(context.Background(), cli.Config{ServerURL: "https://api.example.com", Output: cli.OutputTable},
			[]string{"project", "create", "payments", "--output", "json"},
			cli.Ports{Client: transport, Out: &out, Err: &errOut})
		if code != cli.ExitSuccess {
			t.Fatalf("json exit=%d stderr=%s", code, errOut.String())
		}
		var req2 api.ProjectCreateRequest
		_ = json.Unmarshal(seen[1].Request.Input, &req2)
		if req2.Name != "payments" {
			t.Fatalf("default name = %q; want payments", req2.Name)
		}
		if req2.CreatorRoles != nil {
			t.Fatalf("omitted --creator-role produced %v; want nil", req2.CreatorRoles)
		}
		if !strings.Contains(out.String(), `"project_id":"payments"`) {
			t.Fatalf("json stdout = %q", out.String())
		}

		// Explicit empty --creator-roles "" opts out with "creator_roles":[].
		out.Reset()
		errOut.Reset()
		code = cli.Run(context.Background(), cli.Config{ServerURL: "https://api.example.com", Output: cli.OutputTable},
			[]string{"project", "create", "payments", "--creator-roles", ""},
			cli.Ports{Client: transport, Out: &out, Err: &errOut})
		if code != cli.ExitSuccess {
			t.Fatalf("empty creator-roles exit=%d stderr=%s", code, errOut.String())
		}
		if !strings.Contains(string(seen[2].Request.Input), `"creator_roles":[]`) {
			t.Fatalf("empty --creator-roles wire JSON = %s; want \"creator_roles\":[]", seen[2].Request.Input)
		}
		var req3 api.ProjectCreateRequest
		_ = json.Unmarshal(seen[2].Request.Input, &req3)
		if req3.CreatorRoles == nil || len(*req3.CreatorRoles) != 0 {
			t.Fatalf("empty --creator-roles decoded = %v; want non-nil empty slice", req3.CreatorRoles)
		}

		// Explicit --creator-role admin,engineer sends exact role list.
		out.Reset()
		errOut.Reset()
		code = cli.Run(context.Background(), cli.Config{ServerURL: "https://api.example.com", Output: cli.OutputTable},
			[]string{"project", "create", "payments", "--creator-role", "admin,engineer"},
			cli.Ports{Client: transport, Out: &out, Err: &errOut})
		if code != cli.ExitSuccess {
			t.Fatalf("creator-role exit=%d stderr=%s", code, errOut.String())
		}
		if !strings.Contains(string(seen[3].Request.Input), `"creator_roles":["admin","engineer"]`) {
			t.Fatalf("--creator-role wire JSON = %s; want [admin engineer]", seen[3].Request.Input)
		}
	})

	t.Run("project list table and json paginated", func(t *testing.T) {
		page := 0
		transport := prpFakeTransport{invoke: func(_ context.Context, inv api.Invocation) (api.Result, error) {
			if inv.Operation != "project.list" {
				t.Fatalf("unexpected operation %s", inv.Operation)
			}
			page++
			if page == 1 {
				return api.Result{
					Operation: "project.list",
					Body:      json.RawMessage(`{"projects":[{"project_id":"alpha","name":"Alpha","version":1}],"next_cursor":"c1"}`),
				}, nil
			}
			return api.Result{
				Operation: "project.list",
				Body:      json.RawMessage(`{"projects":[{"project_id":"beta","name":"Beta","version":2}]}`),
			}, nil
		}}

		var out, errOut bytes.Buffer
		code := cli.Run(context.Background(), cli.Config{ServerURL: "https://api.example.com", Output: cli.OutputTable},
			[]string{"project", "list"},
			cli.Ports{Client: transport, Out: &out, Err: &errOut})
		if code != cli.ExitSuccess {
			t.Fatalf("exit=%d stderr=%s", code, errOut.String())
		}
		if !strings.Contains(out.String(), "PROJECT") || !strings.Contains(out.String(), "alpha") || !strings.Contains(out.String(), "beta") {
			t.Fatalf("table stdout = %q", out.String())
		}

		page = 0
		out.Reset()
		code = cli.Run(context.Background(), cli.Config{ServerURL: "https://api.example.com", Output: cli.OutputJSON},
			[]string{"project", "list"},
			cli.Ports{Client: transport, Out: &out, Err: &errOut})
		if code != cli.ExitSuccess {
			t.Fatalf("json exit=%d stderr=%s", code, errOut.String())
		}
		if !strings.Contains(out.String(), `"alpha"`) || !strings.Contains(out.String(), `"beta"`) {
			t.Fatalf("json stdout = %q", out.String())
		}
	})
}

func TestRepoCreateAndListCLI(t *testing.T) {
	var seen []api.Invocation
	transport := prpFakeTransport{invoke: func(_ context.Context, inv api.Invocation) (api.Result, error) {
		seen = append(seen, inv)
		switch inv.Operation {
		case "repository.create":
			return api.Result{
				Operation: "repository.create",
				Body: json.RawMessage(`{
					"name":"shop/web",
					"resource_id":"git-shop-web",
					"binding_id":"git-shop-web",
					"enforcing_service":"git",
					"default_branch":"trunk",
					"clone_url":"https://127.0.0.1:8443/git/shop/web.git",
					"trusted_origins":["https://127.0.0.1:8443"],
					"version":1,
					"created":true
				}`),
			}, nil
		case "repository.list":
			return api.Result{
				Operation: "repository.list",
				Body: json.RawMessage(`{
					"repositories":[{
						"name":"shop/web",
						"resource_id":"git-shop-web",
						"binding_id":"git-shop-web",
						"enforcing_service":"git",
						"default_branch":"trunk",
						"clone_url":"https://127.0.0.1:8443/git/shop/web.git",
						"trusted_origins":["https://127.0.0.1:8443"],
						"version":1
					}]
				}`),
			}, nil
		default:
			t.Fatalf("unexpected op %s", inv.Operation)
			return api.Result{}, nil
		}
	}}

	var out, errOut bytes.Buffer
	code := cli.Run(context.Background(), cli.Config{ServerURL: "https://api.example.com", Project: "project-local", Output: cli.OutputTable},
		[]string{"repo", "create", "shop/web.git", "--default-branch", "trunk", "--resource-id", "git-shop-web", "--service-id", "git", "--trusted-origin", "https://127.0.0.1:8443"},
		cli.Ports{Client: transport, Out: &out, Err: &errOut})
	if code != cli.ExitSuccess {
		t.Fatalf("repo create exit=%d stderr=%s", code, errOut.String())
	}
	var req api.RepositoryCreateRequest
	_ = json.Unmarshal(seen[0].Request.Input, &req)
	if req.Name != "shop/web" || req.DefaultBranch != "trunk" || req.ResourceID != "git-shop-web" || req.EnforcingService != "git" || len(req.TrustedOrigins) != 1 {
		t.Fatalf("repository.create req = %+v", req)
	}
	if !strings.Contains(out.String(), "clone_url\thttps://127.0.0.1:8443/git/shop/web.git") || !strings.Contains(out.String(), "created\ttrue") {
		t.Fatalf("repo create stdout = %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	code = cli.Run(context.Background(), cli.Config{ServerURL: "https://api.example.com", Project: "project-local", Output: cli.OutputTable},
		[]string{"repo", "list"},
		cli.Ports{Client: transport, Out: &out, Err: &errOut})
	if code != cli.ExitSuccess {
		t.Fatalf("repo list exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "REPOSITORY") || !strings.Contains(out.String(), "shop/web") || !strings.Contains(out.String(), "git-shop-web") {
		t.Fatalf("repo list stdout = %q", out.String())
	}

	out.Reset()
	errOut.Reset()
	code = cli.Run(context.Background(), cli.Config{ServerURL: "https://api.example.com", Project: "project-local", Output: cli.OutputJSON},
		[]string{"repo", "list"},
		cli.Ports{Client: transport, Out: &out, Err: &errOut})
	if code != cli.ExitSuccess {
		t.Fatalf("repo list json exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"git-shop-web"`) {
		t.Fatalf("repo list json stdout = %q", out.String())
	}
}

func TestPipelineShowDeleteAndApplyHelpCLI(t *testing.T) {
	yamlDoc := "actions:\n  - name: test\n    image: alpine:3.20\n    commands: [echo hi]\n"
	encodedRead, _ := json.Marshal(api.PipelineReadResult{
		PipelineID: "pipe-42",
		Name:       "build.v1",
		Version:    3,
		YAML:       []byte(yamlDoc),
	})

	var deletedVersion *uint64
	var deletedID string
	transport := prpFakeTransport{invoke: func(_ context.Context, inv api.Invocation) (api.Result, error) {
		switch inv.Operation {
		case "pipeline.list":
			return api.Result{
				Operation: "pipeline.list",
				Body: json.RawMessage(`{"pipelines":[
					{"pipeline_id":"pipe-42","name":"build.v1","version":3},
					{"pipeline_id":"pipe-99","name":"deploy","version":2}
				]}`),
			}, nil
		case "pipeline.read":
			var req api.PipelineReadRequest
			_ = json.Unmarshal(inv.Request.Input, &req)
			if req.PipelineID == "pipe-42" {
				return api.Result{Operation: "pipeline.read", Body: encodedRead}, nil
			}
			if req.PipelineID == "pipe-99" {
				body, _ := json.Marshal(api.PipelineReadResult{
					PipelineID: "pipe-99",
					Name:       "deploy",
					Version:    2,
					YAML:       []byte(yamlDoc),
				})
				return api.Result{Operation: "pipeline.read", Body: body}, nil
			}
			return api.Result{}, fault.New(fault.NotFound, "pipeline not found")
		case "pipeline.delete":
			var req api.PipelineDeleteRequest
			_ = json.Unmarshal(inv.Request.Input, &req)
			deletedID = req.PipelineID
			deletedVersion = inv.Request.ExpectedVersion
			return api.Result{Operation: "pipeline.delete", Body: json.RawMessage(`{"version":4}`)}, nil
		default:
			t.Fatalf("unexpected op %s", inv.Operation)
			return api.Result{}, nil
		}
	}}

	cfg := cli.Config{ServerURL: "https://api.example.com", Project: "project-local", Output: cli.OutputTable}

	// 1. pipeline show by ID.
	var out, errOut bytes.Buffer
	if code := cli.Run(context.Background(), cfg, []string{"pipeline", "show", "pipe-42"},
		cli.Ports{Client: transport, Out: &out, Err: &errOut}); code != cli.ExitSuccess {
		t.Fatalf("pipeline show by id exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "pipeline_id\tpipe-42") || !strings.Contains(out.String(), "echo hi") {
		t.Fatalf("pipeline show by id stdout = %q", out.String())
	}

	// 2. pipeline show by simple name (resolves via pipeline.list after not_found).
	out.Reset()
	errOut.Reset()
	if code := cli.Run(context.Background(), cfg, []string{"pipeline", "show", "deploy"},
		cli.Ports{Client: transport, Out: &out, Err: &errOut}); code != cli.ExitSuccess {
		t.Fatalf("pipeline show by name exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "pipeline_id\tpipe-99") || !strings.Contains(out.String(), "name\tdeploy") {
		t.Fatalf("pipeline show by name stdout = %q", out.String())
	}

	// 3. pipeline show by dotted name with --yaml.
	out.Reset()
	errOut.Reset()
	if code := cli.Run(context.Background(), cfg, []string{"pipeline", "show", "build.v1", "--yaml"},
		cli.Ports{Client: transport, Out: &out, Err: &errOut}); code != cli.ExitSuccess {
		t.Fatalf("pipeline show --yaml exit=%d stderr=%s", code, errOut.String())
	}
	if out.String() != yamlDoc {
		t.Fatalf("pipeline show --yaml = %q; want %q", out.String(), yamlDoc)
	}

	// 4. pipeline show --output json.
	out.Reset()
	errOut.Reset()
	if code := cli.Run(context.Background(), cfg, []string{"pipeline", "show", "pipe-42", "--output", "json"},
		cli.Ports{Client: transport, Out: &out, Err: &errOut}); code != cli.ExitSuccess {
		t.Fatalf("pipeline show json exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"pipeline_id":"pipe-42"`) {
		t.Fatalf("pipeline show json = %q", out.String())
	}

	// 5. pipeline delete by name (auto-fetches version via pipeline.list).
	out.Reset()
	errOut.Reset()
	if code := cli.Run(context.Background(), cfg, []string{"pipeline", "delete", "build.v1"},
		cli.Ports{Client: transport, Out: &out, Err: &errOut}); code != cli.ExitSuccess {
		t.Fatalf("pipeline delete by name exit=%d stderr=%s", code, errOut.String())
	}
	if deletedID != "pipe-42" || deletedVersion == nil || *deletedVersion != 3 {
		t.Fatalf("delete sent id=%q version=%v; want pipe-42 v3", deletedID, deletedVersion)
	}
	if !strings.Contains(out.String(), "deleted pipeline build.v1 (pipeline_id=pipe-42, version=4)") {
		t.Fatalf("pipeline delete stdout = %q", out.String())
	}

	// 6. pipeline delete by ID with --output json.
	out.Reset()
	errOut.Reset()
	if code := cli.Run(context.Background(), cfg, []string{"pipeline", "delete", "pipe-99", "--output", "json"},
		cli.Ports{Client: transport, Out: &out, Err: &errOut}); code != cli.ExitSuccess {
		t.Fatalf("pipeline delete by id exit=%d stderr=%s", code, errOut.String())
	}
	if deletedID != "pipe-99" || deletedVersion == nil || *deletedVersion != 2 {
		t.Fatalf("delete sent id=%q version=%v; want pipe-99 v2", deletedID, deletedVersion)
	}
	if !strings.Contains(out.String(), `"pipeline_id":"pipe-99"`) {
		t.Fatalf("pipeline delete json = %q", out.String())
	}

	// 7. Help text clearly documents filename-derived pipeline names & new commands.
	for _, helpArgs := range [][]string{
		{"pipeline", "--help"},
		{"pipeline", "apply", "--help"},
		{"pipeline", "show", "--help"},
		{"pipeline", "delete", "--help"},
		{"project", "--help"},
		{"project", "create", "--help"},
		{"project", "list", "--help"},
		{"repo", "--help"},
		{"repo", "create", "--help"},
		{"repo", "list", "--help"},
	} {
		out.Reset()
		errOut.Reset()
		if code := cli.Run(context.Background(), cli.Config{}, helpArgs, cli.Ports{Out: &out, Err: &errOut}); code != cli.ExitSuccess {
			t.Fatalf("help %v exit=%d stderr=%s", helpArgs, code, errOut.String())
		}
		if helpArgs[0] == "pipeline" && (len(helpArgs) == 2 || helpArgs[1] == "apply") {
			if !strings.Contains(strings.ToLower(out.String()), "filename") {
				t.Fatalf("help %v should document filename-derived pipeline name: %q", helpArgs, out.String())
			}
		}
	}
}
