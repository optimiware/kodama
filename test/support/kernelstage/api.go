package kernelstage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

type Project struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

type Pipeline struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Source     string `json:"source"`
	SourceKey  string `json:"sourceCommit"`
	Repository string `json:"repositoryId"`
}

type Step struct {
	StepKey  string `json:"stepKey"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
	ExitCode int    `json:"exitCode"`
	LogURI   string `json:"logUri"`
}

type Run struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
	Steps    []Step `json:"steps"`
	Pipeline string `json:"pipelineName"`
	Backend  string `json:"backend"`
}

type Repository struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"defaultBranch"`
	Status        string `json:"status"`
	Empty         bool   `json:"empty"`
}

// CreateProject creates one project in a tenant.
func (i *Installation) CreateProject(ctx context.Context, tenantKey, persona, key string) Project {
	i.T.Helper()
	var p Project
	status, body := i.JSON(ctx, http.MethodPost, "/api/v1/tenants/"+tenantKey+"/projects", i.Token(persona), map[string]any{"key": key}, &p)
	if status != http.StatusCreated || p.ID == "" {
		i.T.Fatalf("create project: HTTP %d: %s", status, body)
	}
	return p
}

// RegisterPipeline posts a .kodama document under the project.
func (i *Installation) RegisterPipeline(ctx context.Context, tenantKey, projectKey, persona string, document string) Pipeline {
	i.T.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.S.URL+"/api/v1/tenants/"+tenantKey+"/projects/"+projectKey+"/pipelines", strings.NewReader(document))
	if err != nil {
		i.T.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+i.Token(persona))
	req.Header.Set("Content-Type", "application/x-yaml")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		i.T.Fatal(err)
	}
	defer resp.Body.Close()
	var p Pipeline
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		i.T.Fatalf("register pipeline: HTTP %d: %v", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusCreated {
		i.T.Fatalf("register pipeline: HTTP %d", resp.StatusCode)
	}
	return p
}

// ListPipelines returns the project's indexed pipelines.
func (i *Installation) ListPipelines(ctx context.Context, tenantKey, projectKey, persona string) []Pipeline {
	i.T.Helper()
	var out struct {
		Items []Pipeline `json:"items"`
	}
	status, body := i.JSON(ctx, http.MethodGet, "/api/v1/tenants/"+tenantKey+"/projects/"+projectKey+"/pipelines", i.Token(persona), nil, &out)
	if status != http.StatusOK {
		i.T.Fatalf("list pipelines: HTTP %d: %s", status, body)
	}
	return out.Items
}

// AdmitRun starts a pipeline by name.
func (i *Installation) AdmitRun(ctx context.Context, tenantKey, projectKey, persona, pipeline string) Run {
	i.T.Helper()
	var r Run
	status, body := i.JSON(ctx, http.MethodPost, "/api/v1/tenants/"+tenantKey+"/projects/"+projectKey+"/runs", i.Token(persona), map[string]any{"pipeline": pipeline}, &r)
	if status != http.StatusCreated {
		i.T.Fatalf("admit run: HTTP %d: %s", status, body)
	}
	return r
}

// GetRun reads one run.
func (i *Installation) GetRun(ctx context.Context, tenantKey, projectKey, runID string) Run {
	i.T.Helper()
	var r Run
	status, body := i.JSON(ctx, http.MethodGet, "/api/v1/tenants/"+tenantKey+"/projects/"+projectKey+"/runs/"+runID, i.Token("viewer"), nil, &r)
	if status != http.StatusOK {
		i.T.Fatalf("get run: HTTP %d: %s", status, body)
	}
	return r
}

// ListRuns returns recent runs.
func (i *Installation) ListRuns(ctx context.Context, tenantKey, projectKey, persona string) []Run {
	i.T.Helper()
	var out struct {
		Items []Run `json:"items"`
	}
	status, body := i.JSON(ctx, http.MethodGet, "/api/v1/tenants/"+tenantKey+"/projects/"+projectKey+"/runs", i.Token(persona), nil, &out)
	if status != http.StatusOK {
		i.T.Fatalf("list runs: HTTP %d: %s", status, body)
	}
	return out.Items
}

// WaitRun polls until the run is terminal.
func (i *Installation) WaitRun(ctx context.Context, tenantKey, projectKey, runID string) Run {
	i.T.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		r := i.GetRun(ctx, tenantKey, projectKey, runID)
		switch r.Status {
		case "succeeded", "failed", "cancelled":
			return r
		}
		select {
		case <-ctx.Done():
			i.T.Fatalf("wait run: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	i.T.Fatalf("run %s did not become terminal", runID)
	return Run{}
}

// Logs reads a step's retained log.
func (i *Installation) Logs(ctx context.Context, tenantKey, projectKey, runID, step string) string {
	i.T.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, i.S.URL+"/api/v1/tenants/"+tenantKey+"/projects/"+projectKey+"/runs/"+runID+"/logs?step="+step, nil)
	req.Header.Set("Authorization", "Bearer "+i.Token("viewer"))
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		i.T.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		i.T.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		i.T.Fatalf("logs: HTTP %d", resp.StatusCode)
	}
	return string(raw)
}

// Cancel requests cancellation.
func (i *Installation) Cancel(ctx context.Context, tenantKey, projectKey, runID string) {
	i.T.Helper()
	status, body := i.JSON(ctx, http.MethodPost, "/api/v1/tenants/"+tenantKey+"/projects/"+projectKey+"/runs/"+runID+"/cancel", i.Token("engineer"), nil, nil)
	if status != http.StatusAccepted {
		i.T.Fatalf("cancel: HTTP %d: %s", status, body)
	}
}

// PutSecret stores one project secret through the kernel API.
func (i *Installation) PutSecret(ctx context.Context, tenantKey, projectKey, name, value string) {
	i.T.Helper()
	status, body := i.JSON(ctx, http.MethodPut, "/api/v1/tenants/"+tenantKey+"/projects/"+projectKey+"/secrets/"+name, i.Token("engineer"), map[string]any{"value": value}, nil)
	if status != http.StatusCreated {
		i.T.Fatalf("put secret: HTTP %d: %s", status, body)
	}
}

// CreateRepository creates an empty project repository.
func (i *Installation) CreateRepository(ctx context.Context, tenantKey, projectKey, persona, name string) Repository {
	i.T.Helper()
	var r Repository
	status, body := i.JSON(ctx, http.MethodPost, "/api/v1/tenants/"+tenantKey+"/projects/"+projectKey+"/repositories", i.Token(persona), map[string]any{"name": name}, &r)
	if status != http.StatusCreated && status != http.StatusOK {
		i.T.Fatalf("create repository: HTTP %d: %s", status, body)
	}
	return r
}

// Credential exchanges the persona's token for a Git password.
func (i *Installation) Credential(ctx context.Context, tenantKey, projectID, repositoryID, persona string, seed bool) string {
	i.T.Helper()
	var out struct {
		Password string `json:"password"`
	}
	status, body := i.JSON(ctx, http.MethodPost, "/api/v1/tenants/"+tenantKey+"/git-credentials", i.Token(persona),
		map[string]any{"projectId": projectID, "repositoryId": repositoryID, "seed": seed}, &out)
	if status != http.StatusCreated || out.Password == "" {
		i.T.Fatalf("git credential: HTTP %d: %s", status, body)
	}
	return out.Password
}

// WaitRunFor waits for a run of a pipeline name.
func (i *Installation) WaitRunFor(ctx context.Context, tenantKey, projectKey, pipeline string, within time.Duration) Run {
	i.T.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, r := range i.ListRuns(ctx, tenantKey, projectKey, "engineer") {
			if r.Pipeline == pipeline {
				return r
			}
		}
		select {
		case <-ctx.Done():
			i.T.Fatalf("wait run for %s: %v", pipeline, ctx.Err())
		case <-time.After(time.Second):
		}
	}
	i.T.Fatalf("pipeline %s admitted no run within %s", pipeline, within)
	return Run{}
}

// WaitPipeline waits until a pipeline indexed from Git has the source commit.
func (i *Installation) WaitPipeline(ctx context.Context, tenantKey, projectKey, name string, within time.Duration) Pipeline {
	i.T.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, p := range i.ListPipelines(ctx, tenantKey, projectKey, "engineer") {
			if p.Name == name && p.SourceKey != "" {
				return p
			}
		}
		select {
		case <-ctx.Done():
			i.T.Fatalf("wait pipeline %s: %v", name, ctx.Err())
		case <-time.After(time.Second):
		}
	}
	i.T.Fatalf("pipeline %s was not indexed within %s", name, within)
	return Pipeline{}
}

// TenantKey derives the seed key that produces the session's tenant ID.
func TenantKey(tenantID string) string {
	return strings.TrimPrefix(tenantID, "t_")
}

// GetRepository reads one repository.
func (i *Installation) GetRepository(ctx context.Context, tenantKey, projectKey, persona, id string) Repository {
	i.T.Helper()
	var r Repository
	status, body := i.JSON(ctx, http.MethodGet, "/api/v1/tenants/"+tenantKey+"/projects/"+projectKey+"/repositories/"+id, i.Token(persona), nil, &r)
	if status != http.StatusOK {
		i.T.Fatalf("get repository: HTTP %d: %s", status, body)
	}
	return r
}

// WaitRepository waits for the native worker to finish provisioning.
func (i *Installation) WaitRepository(ctx context.Context, tenantKey, projectKey, persona, id string, within time.Duration) Repository {
	i.T.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		r := i.GetRepository(ctx, tenantKey, projectKey, persona, id)
		if r.Status == "ready" {
			return r
		}
		select {
		case <-ctx.Done():
			i.T.Fatalf("wait repository: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	i.T.Fatalf("repository %s did not become ready within %s", id, within)
	return Repository{}
}
