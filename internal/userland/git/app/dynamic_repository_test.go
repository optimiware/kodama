package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/git"
	"github.com/optimiware/kodama/internal/userland/git/app/actors"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
)

type dynamicVerifyClient struct {
	mu       sync.Mutex
	requests []api.VerifyRequest
	roles    map[string][]string
}

func (client *dynamicVerifyClient) Verify(_ context.Context, request api.VerifyRequest) (api.VerifyResponse, error) {
	client.mu.Lock()
	client.requests = append(client.requests, request)
	roles, ok := client.roles[request.ActorCredential]
	client.mu.Unlock()
	if !ok {
		return api.VerifyResponse{}, git.UnregisteredRepositoryError(request.ResourceID)
	}
	return api.VerifyResponse{
		Principal:      api.VerifiedPrincipal{ID: "human-1", Kind: "human"},
		Roles:          append([]string(nil), roles...),
		VerificationID: "ver-dyn-1",
	}, nil
}

func (client *dynamicVerifyClient) Report(_ context.Context, request api.ReportRequest) (api.ReportResponse, error) {
	return api.ReportResponse{
		VerificationID: request.VerificationID,
		EventID:        request.SourceEventID,
	}, nil
}

func TestProvisionRepositoryWritesOptionResourceWhenProvided(t *testing.T) {
	layout := newProvisionLayout(t)
	mustProvisionTenant(t, layout.config())
	config := repositoryProvisionConfig(layout, "shop/api")
	config.ResourceID = "res-shop-api"

	result := mustProvisionRepository(t, config)
	if !result.Created {
		t.Fatal("Created = false; want true")
	}
	policyText := repositoryProvisionPolicy(t, config.Environment, layout.repositoryPath())
	if !strings.Contains(policyText, "repo shop/api\n    option resource = res-shop-api\n") {
		t.Fatalf("committed policy missing option resource:\n%s", policyText)
	}
	parsed, err := git.ParseConfig([]byte(policyText))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	policy, ok := parsed.Policy("shop/api")
	if !ok || policy.ResourceID != "res-shop-api" {
		t.Fatalf("Policy(shop/api).ResourceID = %q (ok=%v); want res-shop-api", policy.ResourceID, ok)
	}

	injected := repositoryProvisionConfig(layout, "shop/web")
	injected.ResourceID = "res-shop-web\n    RW+ refs/heads/release/ = @all"
	if _, err := ProvisionRepository(context.Background(), injected); err == nil {
		t.Fatal("ProvisionRepository accepted a resource id that carries policy text")
	}
}

// TestServerOnDemandDynamicRepositoryInitializationAndRaceSafety proves that a
// running `git serve` instance without `--resource` flags initializes bare
// repositories and managed hooks on demand from `admin/conf/git.conf`'s
// `option resource = <id>` safely under concurrent requests, never creates
// storage for unauthenticated or unauthorized requests, and refuses static pin
// spoofing with 403 Forbidden.
func TestServerOnDemandDynamicRepositoryInitializationAndRaceSafety(t *testing.T) {
	layout := newProvisionLayout(t)
	tenantCfg := layout.config()
	mustProvisionTenant(t, tenantCfg)

	socketBase, err := os.MkdirTemp("", "k-dyn-")
	if err != nil {
		t.Fatalf("MkdirTemp socket: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketBase) })

	verifier := &dynamicVerifyClient{
		roles: map[string][]string{
			"engineer-token": {"engineer"},
			"viewer-token":   {"viewer"},
		},
	}
	readVerifier, err := git.NewReadVerifier(verifier, func(repository string) (string, error) {
		return "", git.UnregisteredRepositoryError(repository)
	})
	if err != nil {
		t.Fatalf("NewReadVerifier: %v", err)
	}
	registry, err := actors.NewRegistry(actors.Config{TenantID: provisionTenantID})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	server, err := New(Config{
		TenantID:            provisionTenantID,
		StorageRoot:         layout.storageRoot,
		JournalDirectory:    layout.journalDirectory,
		ResourceIDs:         map[string]string{"admin": "res-admin", "pinned/app": "res-pinned"},
		HookSocketDirectory: socketBase,
		Verify:              verifier,
		Actors:              readVerifier,
		ActorHandles:        registry,
		HookActors:          registry,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	// Commit a dynamic repository and a spoofed static repository into admin/conf/git.conf
	// AFTER the server is already constructed — zero restart!
	policy := adminInitialPolicy + `
repo my-app
    option resource = res-my-app
    R = @engineer
    RW+ refs/heads/work/ = @engineer

repo team/service
    option resource = res-team-service
    R = @all
    RW refs/heads/main = @engineer

repo pinned/app
    option resource = res-spoofed
    R = @all
`
	writeRepositoryProvisionPolicy(t, tenantCfg.Environment, layout.repositoryPath(), policy)

	myAppPath := repositoryProvisionPath(layout, "my-app")

	// 1. Unauthenticated and policy-denied requests must NOT initialize storage on disk.
	unauth := httptest.NewRequest(http.MethodGet, "/my-app.git/info/refs?service=git-upload-pack", nil)
	unauthRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthRec, unauth)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d; want 401", unauthRec.Code)
	}
	if _, err := os.Stat(myAppPath); !os.IsNotExist(err) {
		t.Fatalf("unauthenticated request created %q on disk", myAppPath)
	}

	denied := httptest.NewRequest(http.MethodGet, "/my-app.git/info/refs?service=git-upload-pack", nil)
	denied.SetBasicAuth("kodama", "viewer-token")
	deniedRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(deniedRec, denied)
	if deniedRec.Code != http.StatusNotFound {
		t.Fatalf("policy-denied status = %d; want 404", deniedRec.Code)
	}
	if _, err := os.Stat(myAppPath); !os.IsNotExist(err) {
		t.Fatalf("policy-denied request created %q on disk", myAppPath)
	}

	// 2. Static --resource pin disagreed with by policy option resource is refused with 403.
	mismatchReq := httptest.NewRequest(http.MethodGet, "/pinned/app.git/info/refs?service=git-upload-pack", nil)
	mismatchReq.SetBasicAuth("kodama", "engineer-token")
	mismatchRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(mismatchRec, mismatchReq)
	if mismatchRec.Code != http.StatusForbidden {
		t.Fatalf("mismatched static pin status = %d; want 403", mismatchRec.Code)
	}

	// 3. Concurrent first authenticated requests to brand-new dynamic repositories
	// initialize bare storage and hooks cleanly with zero race.
	const workers = 8
	var wg sync.WaitGroup
	codes := make([]int, workers)
	for i := range workers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			target := "/my-app.git/info/refs?service=git-receive-pack"
			if idx%2 == 1 {
				target = "/team/service.git/info/refs?service=git-upload-pack"
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.SetBasicAuth("kodama", "engineer-token")
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, req)
			codes[idx] = rec.Code
		}(i)
	}
	wg.Wait()
	for idx, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("worker %d status = %d; want 200", idx, code)
		}
	}

	for _, repo := range []string{"my-app", "team/service"} {
		repoPath := filepath.Join(layout.tenantDirectory(), filepath.FromSlash(repo))
		if bare := strings.TrimSpace(requireProvisionGit(t, tenantCfg.Environment,
			"--git-dir="+repoPath, "rev-parse", "--is-bare-repository")); bare != "true" {
			t.Fatalf("%s bare = %q; want true", repo, bare)
		}
		if head := strings.TrimSpace(requireProvisionGit(t, tenantCfg.Environment,
			"--git-dir="+repoPath, "symbolic-ref", "HEAD")); head != native.CommittedPolicyRef {
			t.Fatalf("%s HEAD = %q; want %q", repo, head, native.CommittedPolicyRef)
		}
		requireManagedRepositoryHooks(t, repoPath)
	}
}
