package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/installation"
	shellapp "github.com/optimiware/kodema/internal/shell/app"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/tenancy"
	"github.com/optimiware/kodema/internal/userland/git"
	gitapp "github.com/optimiware/kodema/internal/userland/git/app"
	"github.com/optimiware/kodema/internal/userland/git/app/actors"
)

type noopGitVerifier struct{}

func (noopGitVerifier) Verify(context.Context, api.VerifyRequest) (api.VerifyResponse, error) {
	return api.VerifyResponse{}, fault.New(fault.Unauthenticated, "unused")
}

func (noopGitVerifier) Report(context.Context, api.ReportRequest) (api.ReportResponse, error) {
	return api.ReportResponse{}, fault.New(fault.Unauthenticated, "unused")
}

type noopGitReadVerifier struct{}

func (noopGitReadVerifier) VerifyReadActor(context.Context, string, string) (git.Identity, error) {
	return git.Identity{}, fault.New(fault.Unauthenticated, "unused")
}

// TestRepositoryAndProjectOnboardingEndToEndRESTAndMCP wires a real Shell
// composition (REST + MCP HTTP edges) to a real Userland Git companion HTTP
// server and verifies project creation/listing (without tenant_id) and
// end-to-end repository creation/listing across REST and MCP.
func TestRepositoryAndProjectOnboardingEndToEndRESTAndMCP(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	const (
		tenantID          = "tenant-e2e"
		projectID         = "project-e2e"
		adminToken        = "dev-tenant-admin-token"
		projectAdminToken = "dev-project-admin-token"
		engToken          = "dev-project-eng-token"
		provSecret        = "e2e-git-provision-secret-token"
	)

	storageRoot := filepath.Join(tmp, "git-repos")
	journalDir := filepath.Join(tmp, "git-journal")
	tokenFile := filepath.Join(tmp, "git-provision-token")
	if err := os.WriteFile(tokenFile, []byte(provSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write provision token: %v", err)
	}
	shellTokenFile := filepath.Join(tmp, "shell-git-provision-token")
	if err := os.WriteFile(shellTokenFile, []byte(provSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write shell provision token: %v", err)
	}
	hookBinary := filepath.Join(tmp, "kodema-hook")
	if err := os.WriteFile(hookBinary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write hook binary: %v", err)
	}

	if _, err := gitapp.ProvisionTenant(ctx, gitapp.ProvisionConfig{
		TenantID:         id.TenantID(tenantID),
		StorageRoot:      storageRoot,
		JournalDirectory: journalDir,
		HookBinary:       hookBinary,
	}); err != nil {
		t.Fatalf("gitapp.ProvisionTenant: %v", err)
	}
	registry, err := actors.NewRegistry(actors.Config{TenantID: id.TenantID(tenantID)})
	if err != nil {
		t.Fatalf("actors.NewRegistry: %v", err)
	}
	defer func() { _ = registry.Close() }()

	gitServer, err := gitapp.New(gitapp.Config{
		TenantID:           id.TenantID(tenantID),
		StorageRoot:        storageRoot,
		JournalDirectory:   journalDir,
		HookBinary:         hookBinary,
		ProvisionTokenFile: tokenFile,
		ResourceIDs:        map[string]string{"admin": "git-admin"},
		Verify:             noopGitVerifier{},
		Actors:             noopGitReadVerifier{},
		ActorHandles:       registry,
		HookActors:         registry,
	})
	if err != nil {
		t.Fatalf("gitapp.New: %v", err)
	}
	defer func() { _ = gitServer.Close() }()
	gitHTTP := httptest.NewServer(gitServer.Handler())
	defer gitHTTP.Close()

	expiry := time.Now().Add(time.Hour)
	app, err := shellapp.New(ctx, shellapp.Config{
		Profile:        shellapp.ProfileDevelopment,
		InstallationID: "inst-e2e",
		DataDir:        filepath.Join(tmp, "shell-data"),
		Installation: installation.Config{
			Environment: installation.EnvironmentDevelopment,
			State:       installation.StateMemory,
			Messaging:   installation.MessagingMemory,
			Runtime:     installation.RuntimeProcess,
			Keys:        installation.KeyEphemeral,
			PublicURL:   "https://127.0.0.1:8443",
		},
		Dev: &shellapp.DevelopmentConfig{
			Bootstrap: &shellapp.Bootstrap{
				TenantID:       tenantID,
				TenantName:     "Tenant E2E",
				ProjectID:      projectID,
				ProjectName:    "Project E2E",
				AdminPrincipal: "admin-1",
				AdminRoles:     []string{"admin", "engineer", "reviewer", "operator", "viewer"},
			},
			Tokens: []shellapp.DevToken{
				{
					Credential: adminToken,
					Principal: identity.Principal{
						ID:      "admin-1",
						Kind:    identity.KindHuman,
						Scope:   tenancy.Scope{TenantID: tenantID},
						Roles:   []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer},
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: expiry,
				},
				{
					Credential: projectAdminToken,
					Principal: identity.Principal{
						ID:      "admin-1",
						Kind:    identity.KindHuman,
						Scope:   tenancy.Scope{TenantID: tenantID, ProjectID: projectID},
						Roles:   []tenancy.Role{tenancy.RoleAdmin, tenancy.RoleEngineer, tenancy.RoleReviewer, tenancy.RoleOperator, tenancy.RoleViewer},
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: expiry,
				},
				{
					Credential: engToken,
					Principal: identity.Principal{
						ID:      "eng-1",
						Kind:    identity.KindHuman,
						Scope:   tenancy.Scope{TenantID: tenantID, ProjectID: projectID},
						Roles:   []tenancy.Role{tenancy.RoleEngineer},
						Enabled: true,
						Version: 1,
					},
					ExpiresAt: expiry,
				},
			},
		},
		GitCompanion: &shellapp.GitCompanionConfig{
			AdminURL:       gitHTTP.URL,
			PublicGitURL:   "https://127.0.0.1:8443/git",
			ServiceID:      "git",
			TrustedOrigins: []string{"https://127.0.0.1:8443"},
			TokenFile:      shellTokenFile,
		},
	})
	if err != nil {
		t.Fatalf("shellapp.New: %v", err)
	}
	defer func() { _ = app.Close(ctx) }()

	shellHTTP := httptest.NewServer(app.Handler())
	defer shellHTTP.Close()

	restDo := func(method, path, token, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, shellHTTP.URL+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", api.MediaType)
		resp, err := shellHTTP.Client().Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.Bytes()
	}

	mcpCall := func(token, toolName string, args map[string]any) map[string]any {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      toolName,
				"arguments": args,
			},
		})
		status, raw := restDo(http.MethodPost, "/mcp", token, string(payload))
		if status != http.StatusOK {
			t.Fatalf("MCP %s status = %d body=%s", toolName, status, raw)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode MCP response: %v body=%s", err, raw)
		}
		return out
	}

	// Create the enforcing "git" service account in project-e2e and grant eng-1
	// project membership so current-authority checks pass for project reads.
	status, raw := restDo(http.MethodPost, "/api/v1/projects/"+projectID+"/services", projectAdminToken,
		`{"input":{"service_id":"git","roles":["viewer"]},"idempotency_key":"svc-git-1"}`)
	if status != http.StatusOK {
		t.Fatalf("service.create git status=%d body=%s", status, raw)
	}
	status, raw = restDo(http.MethodPut, "/api/v1/projects/"+projectID+"/memberships/eng-1", adminToken,
		`{"input":{"roles":["engineer"]},"idempotency_key":"grant-eng-1"}`)
	if status != http.StatusOK {
		t.Fatalf("membership.grant eng-1 status=%d body=%s", status, raw)
	}

	// 1. REST POST /api/v1/projects (omitting tenant_id!) creates project-2 with
	// creator membership automatically initialized.
	status, raw = restDo(http.MethodPost, "/api/v1/projects", adminToken,
		`{"input":{"project_id":"project-two","name":"Project Two"},"idempotency_key":"proj-two-1"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /api/v1/projects status=%d body=%s", status, raw)
	}
	// REST GET /api/v1/projects (omitting tenant_id!) lists both projects.
	status, raw = restDo(http.MethodGet, "/api/v1/projects", adminToken, "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/projects status=%d body=%s", status, raw)
	}
	if !strings.Contains(string(raw), `"project-two"`) || !strings.Contains(string(raw), `"`+projectID+`"`) {
		t.Fatalf("GET /api/v1/projects body = %s; want both projects", raw)
	}

	// 2. MCP project_list without tenant_id works identically.
	mcpProjList := mcpCall(adminToken, "project_list", map[string]any{})
	mcpProjBytes, _ := json.Marshal(mcpProjList)
	if !strings.Contains(string(mcpProjBytes), "project-two") {
		t.Fatalf("MCP project_list = %s; missing project-two", mcpProjBytes)
	}

	// 3. MCP repository_create provisions bare repo + conf/git.conf + binding in one call!
	mcpRepoCreate := mcpCall(adminToken, "repository_create", map[string]any{
		"project_id":      projectID,
		"name":            "payments/api",
		"default_branch":  "main",
		"idempotency_key": "mcp-repo-1",
	})
	mcpRepoBytes, _ := json.Marshal(mcpRepoCreate)
	if !strings.Contains(string(mcpRepoBytes), "git-payments-api") ||
		!strings.Contains(string(mcpRepoBytes), "https://127.0.0.1:8443/git/payments/api.git") {
		t.Fatalf("MCP repository_create = %s", mcpRepoBytes)
	}
	// Verify the Git companion actually provisioned the bare repo and option resource!
	resolvedResource, err := gitServer.Service().ResolveResourceID(ctx, "payments/api")
	if err != nil || resolvedResource != "git-payments-api" {
		t.Fatalf("gitServer ResolveResourceID(payments/api) = %q, %v; want git-payments-api", resolvedResource, err)
	}

	// 4. REST repository.list returns the onboarded repository.
	status, raw = restDo(http.MethodGet, "/api/v1/projects/"+projectID+"/repositories", engToken, "")
	if status != http.StatusOK {
		t.Fatalf("GET repositories status=%d body=%s", status, raw)
	}
	var listEnv struct {
		Operation string                   `json:"operation"`
		Body      api.RepositoryListResult `json:"body"`
	}
	if err := json.Unmarshal(raw, &listEnv); err != nil {
		t.Fatalf("decode repository.list: %v", err)
	}
	if len(listEnv.Body.Repositories) != 1 || listEnv.Body.Repositories[0].Name != "payments/api" ||
		listEnv.Body.Repositories[0].CloneURL != "https://127.0.0.1:8443/git/payments/api.git" {
		t.Fatalf("repository.list = %+v", listEnv.Body.Repositories)
	}

	// 5. MCP repository_list returns the same repository.
	mcpRepoList := mcpCall(engToken, "repository_list", map[string]any{"project_id": projectID})
	mcpRepoListBytes, _ := json.Marshal(mcpRepoList)
	if !strings.Contains(string(mcpRepoListBytes), "payments/api") {
		t.Fatalf("MCP repository_list = %s", mcpRepoListBytes)
	}

	// 6. Both REST and MCP reject ".git" repository names ("admin.git" and
	// "shop/web.git") BEFORE committing any binding.
	for _, badName := range []string{"admin.git", "shop/web.git"} {
		status, raw = restDo(http.MethodPost, "/api/v1/projects/"+projectID+"/repositories", adminToken,
			`{"input":{"name":"`+badName+`"},"idempotency_key":"rest-bad-`+badName+`"}`)
		if status != http.StatusBadRequest {
			t.Fatalf("REST repository.create %q status=%d body=%s; want 400", badName, status, raw)
		}
		mcpBad := mcpCall(adminToken, "repository_create", map[string]any{
			"project_id":      projectID,
			"name":            badName,
			"idempotency_key": "mcp-bad-" + badName,
		})
		mcpBadBytes, _ := json.Marshal(mcpBad)
		if !strings.Contains(string(mcpBadBytes), `"isError":true`) && !strings.Contains(string(mcpBadBytes), `"error"`) {
			t.Fatalf("MCP repository_create %q = %s; want error", badName, mcpBadBytes)
		}
	}
	// Verify no orphaned bindings were committed.
	status, raw = restDo(http.MethodGet, "/api/v1/projects/"+projectID+"/repositories", engToken, "")
	if status != http.StatusOK {
		t.Fatalf("GET repositories after .git rejections status=%d body=%s", status, raw)
	}
	listEnv = struct {
		Operation string                   `json:"operation"`
		Body      api.RepositoryListResult `json:"body"`
	}{}
	if err := json.Unmarshal(raw, &listEnv); err != nil {
		t.Fatalf("decode repository.list: %v", err)
	}
	if len(listEnv.Body.Repositories) != 1 || listEnv.Body.Repositories[0].Name != "payments/api" {
		t.Fatalf("repository.list after .git rejections = %+v; want only payments/api", listEnv.Body.Repositories)
	}

	// 7. A server-to-git provision token mismatch surfaces as HTTP 503
	// dependency_unavailable (NEVER 401 unauthenticated to end users).
	if err := os.WriteFile(shellTokenFile, []byte("mismatched-provision-secret\n"), 0o600); err != nil {
		t.Fatalf("rotate shell token file to mismatched secret: %v", err)
	}
	status, raw = restDo(http.MethodPost, "/api/v1/projects/"+projectID+"/repositories", adminToken,
		`{"input":{"name":"token/mismatch"},"idempotency_key":"rest-tok-mismatch"}`)
	if status != http.StatusServiceUnavailable || !strings.Contains(string(raw), `"dependency_unavailable"`) {
		t.Fatalf("REST repository.create with mismatched git provision token status=%d body=%s; want 503 dependency_unavailable", status, raw)
	}
}
