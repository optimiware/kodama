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

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/userland/git"
	gitapp "github.com/optimiware/kodema/internal/userland/git/app"
	"github.com/optimiware/kodema/internal/userland/git/app/actors"
)

type provisionTestReadVerifier struct {
	identities map[string]git.Identity
}

func (v provisionTestReadVerifier) VerifyReadActor(_ context.Context, repository, credential string) (git.Identity, error) {
	if repository != "admin" {
		return git.Identity{}, fault.New(fault.NotFound, "unexpected repository")
	}
	identity, ok := v.identities[credential]
	if !ok {
		return git.Identity{}, fault.New(fault.Unauthenticated, "invalid credential")
	}
	return identity, nil
}

type noopVerifyClient struct{}

func (noopVerifyClient) Verify(context.Context, api.VerifyRequest) (api.VerifyResponse, error) {
	return api.VerifyResponse{}, fault.New(fault.Unauthenticated, "unused")
}

func (noopVerifyClient) Report(context.Context, api.ReportRequest) (api.ReportResponse, error) {
	return api.ReportResponse{}, fault.New(fault.Unauthenticated, "unused")
}

// TestGitServeHTTPProvisioning exercises POST /_kodema/repositories against a
// real gitapp.Server with real bare storage and admin/conf/git.conf commits:
// auth enforcement (401/403), provision token auth, human @admin auth, custom
// default branch, idempotency, resource mismatch conflict (409), and validation.
func TestGitServeHTTPProvisioning(t *testing.T) {
	ctx := context.Background()
	tenantID := id.TenantID("tenant-a")
	root := t.TempDir()
	storageRoot := filepath.Join(root, "repos")
	journalDir := filepath.Join(root, "journal")
	tokenFile := filepath.Join(root, "provision-token")
	const provisionSecret = "super-secret-provision-token-123"
	if err := os.WriteFile(tokenFile, []byte(provisionSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write provision token: %v", err)
	}
	hookBinary := filepath.Join(root, "kodema-hook")
	if err := os.WriteFile(hookBinary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write hook binary: %v", err)
	}

	if _, err := gitapp.ProvisionTenant(ctx, gitapp.ProvisionConfig{
		TenantID:         tenantID,
		StorageRoot:      storageRoot,
		JournalDirectory: journalDir,
		HookBinary:       hookBinary,
	}); err != nil {
		t.Fatalf("ProvisionTenant: %v", err)
	}

	registry, err := actors.NewRegistry(actors.Config{TenantID: tenantID})
	if err != nil {
		t.Fatalf("actors.NewRegistry: %v", err)
	}
	defer func() { _ = registry.Close() }()

	server, err := gitapp.New(gitapp.Config{
		TenantID:           tenantID,
		StorageRoot:        storageRoot,
		JournalDirectory:   journalDir,
		HookBinary:         hookBinary,
		ProvisionTokenFile: tokenFile,
		ResourceIDs:        map[string]string{"admin": "git-admin"},
		Verify:             noopVerifyClient{},
		Actors: provisionTestReadVerifier{identities: map[string]git.Identity{
			"human-admin":    {Roles: []string{"admin"}},
			"human-engineer": {Roles: []string{"engineer"}},
			"svc-ci":         {ServiceID: "svc-ci"},
		}},
		ActorHandles: registry,
		HookActors:   registry,
	})
	if err != nil {
		t.Fatalf("gitapp.New: %v", err)
	}
	defer func() { _ = server.Close() }()

	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	post := func(token, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, httpServer.URL+git.ProvisionHTTPPath, bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", api.MediaType)
		resp, err := httpServer.Client().Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		return resp
	}

	// 1. Unauthenticated -> 401.
	resp := post("", `{"repository":"app","resource_id":"git-app"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d; want 401", resp.StatusCode)
	}

	// 2. Invalid token -> 401.
	resp = post("wrong-token", `{"repository":"app","resource_id":"git-app"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d; want 401", resp.StatusCode)
	}

	// 3. Non-admin human and service account -> 403.
	for _, token := range []string{"human-engineer", "svc-ci"} {
		resp = post(token, `{"repository":"app","resource_id":"git-app"}`)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s status = %d; want 403", token, resp.StatusCode)
		}
	}

	// 4. Non-POST method -> 405.
	getReq, _ := http.NewRequest(http.MethodGet, httpServer.URL+git.ProvisionHTTPPath, nil)
	getReq.Header.Set("Authorization", "Bearer "+provisionSecret)
	resp, err = httpServer.Client().Do(getReq)
	if err != nil {
		t.Fatalf("GET request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d; want 405", resp.StatusCode)
	}

	// 5. Provision with shared provision token -> 200 created=true.
	resp = post(provisionSecret, `{"repository":"shop/web","resource_id":"git-shop-web","default_branch":"main"}`)
	var created git.ProvisionHTTPResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode created response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !created.Created || created.Repository != "shop/web" || created.ResourceID != "git-shop-web" || created.DefaultBranch != "main" {
		t.Fatalf("created = status %d %+v", resp.StatusCode, created)
	}

	// Verify bare repository, managed hooks, and dynamic ResolveResourceID.
	repoDir := filepath.Join(storageRoot, string(tenantID), "shop", "web")
	if _, err := os.Stat(filepath.Join(repoDir, "HEAD")); err != nil {
		t.Fatalf("provisioned bare repo HEAD missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "hooks", "pre-receive")); err != nil {
		t.Fatalf("provisioned pre-receive hook missing: %v", err)
	}
	resolvedID, err := server.Service().ResolveResourceID(ctx, "shop/web")
	if err != nil || resolvedID != "git-shop-web" {
		t.Fatalf("ResolveResourceID(shop/web) = %q, %v; want git-shop-web", resolvedID, err)
	}

	// 6. Idempotent repeat -> 200 created=false.
	resp = post(provisionSecret, `{"repository":"shop/web","resource_id":"git-shop-web"}`)
	var repeated git.ProvisionHTTPResponse
	if err := json.NewDecoder(resp.Body).Decode(&repeated); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode repeated response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || repeated.Created || repeated.ResourceID != "git-shop-web" {
		t.Fatalf("repeated = status %d %+v; want created=false", resp.StatusCode, repeated)
	}

	// 7. Conflicting resource_id for existing repo -> 409 Conflict.
	resp = post(provisionSecret, `{"repository":"shop/web","resource_id":"git-other"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("conflicting resource_id status = %d; want 409", resp.StatusCode)
	}

	// 7b. Duplicate resource_id across a DIFFERENT repo -> 409 Conflict AND no
	// orphan bare directory created on disk.
	resp = post(provisionSecret, `{"repository":"other/repo","resource_id":"git-shop-web"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate resource_id across different repo status = %d; want 409", resp.StatusCode)
	}
	orphanDir := filepath.Join(storageRoot, string(tenantID), "other", "repo")
	if _, err := os.Lstat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("duplicate resource_id created orphan repository path %q (err=%v); want non-existent", orphanDir, err)
	}

	// 8. Provision via verified human @admin credential with custom default branch.
	resp = post("human-admin", `{"repository":"infra.git","resource_id":"git-infra","default_branch":"trunk"}`)
	var adminCreated git.ProvisionHTTPResponse
	if err := json.NewDecoder(resp.Body).Decode(&adminCreated); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode adminCreated response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !adminCreated.Created || adminCreated.Repository != "infra" || adminCreated.DefaultBranch != "trunk" {
		t.Fatalf("adminCreated = status %d %+v", resp.StatusCode, adminCreated)
	}
	headBytes, err := os.ReadFile(filepath.Join(storageRoot, string(tenantID), "infra", "HEAD"))
	if err != nil || !strings.Contains(string(headBytes), "refs/heads/trunk") {
		t.Fatalf("infra HEAD = %q, %v; want refs/heads/trunk", string(headBytes), err)
	}

	// 9. Reserved admin repo name -> 400.
	resp = post(provisionSecret, `{"repository":"admin","resource_id":"git-admin"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reserved admin repo status = %d; want 400", resp.StatusCode)
	}
}
