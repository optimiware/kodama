package kube_kernel_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/test/support/kernelstage"
)

func setup(t *testing.T) (*kernelstage.Installation, context.Context, string, string) {
	t.Helper()
	i := kernelstage.New(t)
	ctx := i.Context(15 * time.Minute)
	if !i.Installed(ctx) {
		if err := i.Install(ctx); err != nil {
			t.Fatalf("install the kernel chart: %v", err)
		}
	}
	i.WaitReady(ctx)
	key := kernelstage.TenantKey(i.S.TenantA().TenantID)
	for _, p := range []struct {
		persona string
		roles   []string
	}{
		{"admin", []string{"admin"}},
		{"engineer", []string{"engineer"}},
		{"viewer", []string{"viewer"}},
		{"analyst", []string{"admin"}},
	} {
		i.Seed(ctx, key, p.persona, p.roles...)
	}
	keyB := kernelstage.TenantKey(i.S.TenantB().TenantID)
	i.Seed(ctx, keyB, "foreign", "admin")
	return i, ctx, key, keyB
}

func pipelineDocument(image string, steps string) string {
	return "name: build\nsteps:\n" + steps
}

func TestKernelCleanInstall(t *testing.T) {
	i := kernelstage.New(t)
	ctx := i.Context(15 * time.Minute)
	if i.Installed(ctx) {
		t.Fatal("clean-install acceptance needs a newly prepared qualification session")
	}
	if err := i.Install(ctx); err != nil {
		t.Fatalf("install: %v", err)
	}
	i.WaitReady(ctx)

	deploy, err := i.K.Run(ctx, "-n", i.S.Namespace, "get", "deploy", i.Deployment(), "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"server"`, `"--assembly"`, `"kernel"`, `"KODAMA_ENV"`, `"production"`, `"KODAMA_EXECUTOR"`, `"kubernetes"`} {
		if !strings.Contains(deploy, want) {
			t.Fatalf("deployment has no %s", want)
		}
	}
	if n := i.OwnerCount(`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='tenants'`); n != 1 {
		t.Fatalf("migration did not install the kernel schema (tenants table %d)", n)
	}
	var runtimeOwner bool
	i.MustScan(&runtimeOwner, `SELECT pg_get_userbyid(c.relowner) = 'kodama_runtime' FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname='tenants'`)
	if runtimeOwner {
		t.Fatal("the runtime role owns the kernel schema")
	}

	for _, path := range []string{"/healthz", "/readyz"} {
		code, err := i.Probe(ctx, path)
		if err != nil || code != http.StatusOK {
			t.Fatalf("%s: HTTP %d (%v)", path, code, err)
		}
	}

	i.RelayDown(ctx)
	deadline := time.Now().Add(90 * time.Second)
	for {
		code, err := i.Probe(ctx, "/readyz")
		if err != nil || code != http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("readiness stayed ready while the database path was down")
		}
		time.Sleep(time.Second)
	}
	i.RelayUp(ctx)
	i.WaitReady(i.Context(4 * time.Minute))
}

func TestKernelPipelineAndTenantIsolation(t *testing.T) {
	i, ctx, key, keyB := setup(t)
	project := i.CreateProject(ctx, key, "admin", "inventory")
	pipeline := i.RegisterPipeline(ctx, key, project.Key, "engineer", pipelineDocument(i.ServerImage(),
		`  - name: write
    image: `+i.ServerImage()+`
    commands:
      - echo shared > /workspace/shared.txt
  - name: read
    image: `+i.ServerImage()+`
    commands:
      - cat /workspace/shared.txt
`))
	if pipeline.Name != "build" {
		t.Fatalf("pipeline = %+v", pipeline)
	}
	run := i.AdmitRun(ctx, key, project.Key, "engineer", "build")
	final := i.WaitRun(ctx, key, project.Key, run.ID)
	if final.Status != "succeeded" {
		t.Fatalf("run %s (%s): %+v", final.Status, final.Reason, final.Steps)
	}
	if len(final.Steps) != 2 || final.Steps[1].Status != "succeeded" {
		t.Fatalf("steps = %+v", final.Steps)
	}
	if log := i.Logs(ctx, key, project.Key, run.ID, "read"); !strings.Contains(log, "shared") {
		t.Fatalf("second step did not read the shared workspace: %q", log)
	}

	var artifacts struct {
		Items []map[string]any `json:"items"`
	}
	status, body := i.JSON(ctx, http.MethodPost, "/api/v1/tenants/"+key+"/projects/"+project.Key+"/artifacts", "engineer",
		map[string]any{"runId": run.ID, "name": "report", "kind": "file", "uri": "https://example.invalid/report"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("artifact metadata: HTTP %d: %s", status, body)
	}
	status, body = i.JSON(ctx, http.MethodGet, "/api/v1/tenants/"+key+"/projects/"+project.Key+"/artifacts", "viewer", nil, &artifacts)
	if status != http.StatusOK || len(artifacts.Items) != 1 {
		t.Fatalf("artifacts: HTTP %d, %d items", status, len(artifacts.Items))
	}

	// A member of another tenant learns nothing about this run.
	status, _ = i.JSON(ctx, http.MethodGet, "/api/v1/tenants/"+key+"/projects/"+project.Key+"/runs/"+run.ID, "foreign", nil, nil)
	if status != http.StatusNotFound && status != http.StatusForbidden {
		t.Fatalf("a foreign tenant read the run: HTTP %d", status)
	}
	_ = keyB
}

func TestKernelGitPushAndRunCredential(t *testing.T) {
	i, ctx, key, _ := setup(t)
	project := i.CreateProject(ctx, key, "admin", "gitops")
	repo := i.CreateRepository(ctx, key, project.Key, "engineer", "source")
	if repo.ID == "" {
		t.Fatalf("repository = %+v", repo)
	}
	repo = i.WaitRepository(ctx, key, project.Key, "engineer", repo.ID, 2*time.Minute)
	seedPassword := i.Credential(ctx, key, project.ID, repo.ID, "engineer", true)
	work := t.TempDir()
	g := kernelstage.NewGitRepo(t, work, i.S.URL, key, project.Key, repo.Name, seedPassword)
	g.Init(ctx)
	g.Write(".kodama/build.yaml", "name: build\ntriggers:\n  git:\n    branches: [main]\nsteps:\n  - name: source\n    image: "+i.ServerImage()+`
    commands:
      - test -f README.md
      - git checkout -b "kodama/runs/$KODAMA_RUN_ID/work"
      - echo run > run.txt
      - git add run.txt
      - git -c user.name=kodama -c user.email=kodama@stage.invalid commit -q -m run
      - git push -q origin HEAD
`)
	g.Write("README.md", "hello\n")
	commit := g.Commit(ctx, "seed")
	g.Push(ctx, "main:refs/heads/main")
	p := i.WaitPipeline(ctx, key, project.Key, "build", 3*time.Minute)
	if p.SourceKey != commit {
		t.Fatalf("pipeline was indexed at %s, want the pushed commit %s", p.SourceKey, commit)
	}
	run := i.WaitRunFor(ctx, key, project.Key, "build", 2*time.Minute)
	final := i.WaitRun(ctx, key, project.Key, run.ID)
	if final.Status != "succeeded" {
		t.Fatalf("git-triggered run %s (%s): %+v", final.Status, final.Reason, final.Steps)
	}
	password := i.Credential(ctx, key, project.ID, repo.ID, "engineer", false)
	reader := kernelstage.NewGitRepo(t, t.TempDir(), i.S.URL, key, project.Key, repo.Name, password)
	branch := "refs/heads/kodama/runs/" + run.ID + "/work"
	if ref := reader.Head(ctx, branch); ref == "" {
		t.Fatal("the run's scoped Git credential did not publish its run branch")
	}
	if tip := reader.Head(ctx, "refs/heads/main"); tip != commit {
		t.Fatalf("a run push moved the default branch to %s", tip)
	}
}

func TestKernelSecretsUseTheConfiguredKMS(t *testing.T) {
	i, ctx, key, _ := setup(t)
	project := i.CreateProject(ctx, key, "admin", "secrets")
	i.PutSecret(ctx, key, project.Key, "TOKEN", "kernel-value")
	pipeline := i.RegisterPipeline(ctx, key, project.Key, "engineer", pipelineDocument(i.ServerImage(),
		`  - name: secret
    image: `+i.ServerImage()+`
    secrets: [TOKEN]
    commands:
      - test "$(cat /kodama/secrets/TOKEN)" = kernel-value
`))
	run := i.AdmitRun(ctx, key, project.Key, "engineer", pipeline.Name)
	if final := i.WaitRun(ctx, key, project.Key, run.ID); final.Status != "succeeded" {
		t.Fatalf("secret run %s (%s): %+v", final.Status, final.Reason, final.Steps)
	}
	var provider string
	i.MustScan(&provider, `SELECT envelope->'wrapped'->>'provider' FROM secrets WHERE tenant_id=$1 AND project_id=$2 AND name='TOKEN'`, i.S.TenantA().TenantID, project.ID)
	if provider != "scaleway" && provider != "aws" && provider != "gcp" {
		t.Fatalf("secret envelope provider = %q, want the configured cloud KMS", provider)
	}
	i.Restart(ctx)
	run = i.AdmitRun(ctx, key, project.Key, "engineer", pipeline.Name)
	if final := i.WaitRun(ctx, key, project.Key, run.ID); final.Status != "succeeded" {
		t.Fatalf("secret run after restart %s (%s): %+v", final.Status, final.Reason, final.Steps)
	}
}

func TestKernelCancelAndRestartAdoption(t *testing.T) {
	i, ctx, key, _ := setup(t)
	project := i.CreateProject(ctx, key, "admin", "recovery")
	i.RegisterPipeline(ctx, key, project.Key, "engineer", "name: slow\nsteps:\n  - name: sleep\n    image: "+i.ServerImage()+`
    commands:
      - sleep 90
`)
	run := i.AdmitRun(ctx, key, project.Key, "engineer", "slow")
	deadline := time.Now().Add(2 * time.Minute)
	for {
		cur := i.GetRun(ctx, key, project.Key, run.ID)
		if cur.Status == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run stayed %s", cur.Status)
		}
		time.Sleep(time.Second)
	}
	i.Cancel(ctx, key, project.Key, run.ID)
	final := i.WaitRun(ctx, key, project.Key, run.ID)
	if final.Status != "cancelled" {
		t.Fatalf("cancelled run ended %s (%s)", final.Status, final.Reason)
	}

	i.RegisterPipeline(ctx, key, project.Key, "engineer", "name: adopt\nsteps:\n  - name: short\n    image: "+i.ServerImage()+`
    commands:
      - sleep 15
      - echo adopted
`)
	adopt := i.AdmitRun(ctx, key, project.Key, "engineer", "adopt")
	deadline = time.Now().Add(2 * time.Minute)
	for i.GetRun(ctx, key, project.Key, adopt.ID).Status != "running" {
		if time.Now().After(deadline) {
			t.Fatal("adoption run did not start")
		}
		time.Sleep(time.Second)
	}
	i.Restart(ctx)
	if final := i.WaitRun(ctx, key, project.Key, adopt.ID); final.Status != "succeeded" {
		t.Fatalf("adopted run ended %s (%s): %+v", final.Status, final.Reason, final.Steps)
	}
}
