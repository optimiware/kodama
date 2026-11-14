package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/userland/git"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	actorapp "github.com/optimiware/kodama/internal/userland/git/app/actors"
)

func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "git" && os.Args[2] == "hook" {
		os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, defaultDeps()))
	}
	os.Exit(m.Run())
}

type e2eGitVerification struct {
	mu       sync.Mutex
	verifies []api.VerifyRequest
	reports  []api.ReportRequest
	roles    map[string][]string
	seq      int
}

func (v *e2eGitVerification) Verify(_ context.Context, req api.VerifyRequest) (api.VerifyResponse, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.verifies = append(v.verifies, req)
	roles, ok := v.roles[req.ActorCredential]
	if !ok {
		return api.VerifyResponse{}, git.UnregisteredRepositoryError(req.ResourceID)
	}
	v.seq++
	return api.VerifyResponse{
		Principal:      api.VerifiedPrincipal{ID: "human-1", Kind: "human"},
		Roles:          append([]string(nil), roles...),
		VerificationID: fmt.Sprintf("ver-%d", v.seq),
	}, nil
}

func (v *e2eGitVerification) Report(_ context.Context, req api.ReportRequest) (api.ReportResponse, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.reports = append(v.reports, req)
	return api.ReportResponse{
		VerificationID: req.VerificationID,
		EventID:        req.SourceEventID,
	}, nil
}

func (v *e2eGitVerification) reportSnapshot() []api.ReportRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]api.ReportRequest(nil), v.reports...)
}

func runTestGit(t *testing.T, home, dir, token string, args ...string) string {
	t.Helper()
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
	}
	fullArgs := []string{"-c", "http.followRedirects=false"}
	if token != "" {
		header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("kodama:"+token))
		env = append(env, "KODAMA_TEST_AUTH="+header)
		fullArgs = append(fullArgs, "--config-env=http.extraHeader=KODAMA_TEST_AUTH")
	}
	fullArgs = append(fullArgs, args...)
	cmd := exec.Command("git", fullArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s failed: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// TestZeroRestartDynamicGitRepositoryWorkflow proves the complete frictionless
// workflow: `git serve` starts knowing ONLY `admin`, an admin pushes a new
// `repo my-app` + `option resource = my-app` stanza to `admin/conf/git.conf`,
// and an engineer immediately runs `git push origin main` to `my-app` with ZERO
// `git serve` restart and ZERO `provision-repository` commands.
func TestZeroRestartDynamicGitRepositoryWorkflow(t *testing.T) {
	root := t.TempDir()
	storageRoot := filepath.Join(root, "repos")
	journalDir := filepath.Join(root, "journal")
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("MkdirAll home: %v", err)
	}
	socketBase, err := os.MkdirTemp("", "k-e2e-")
	if err != nil {
		t.Fatalf("MkdirTemp socket: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketBase) })

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	const tenant = id.TenantID("tenant-dyn")
	if _, err := gitapp.ProvisionTenant(context.Background(), gitapp.ProvisionConfig{
		TenantID:         tenant,
		StorageRoot:      storageRoot,
		JournalDirectory: journalDir,
		HookBinary:       exe,
	}); err != nil {
		t.Fatalf("ProvisionTenant: %v", err)
	}

	verifier := &e2eGitVerification{
		roles: map[string][]string{
			"admin-token":    {"admin"},
			"engineer-token": {"engineer"},
		},
	}
	readVerifier, err := git.NewReadVerifier(verifier, func(repository string) (string, error) {
		if repository == "admin" {
			return "res-admin", nil
		}
		return "", git.UnregisteredRepositoryError(repository)
	})
	if err != nil {
		t.Fatalf("NewReadVerifier: %v", err)
	}
	registry, err := actorapp.NewRegistry(actorapp.Config{TenantID: tenant})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })

	// Start server with ONLY admin in ResourceIDs!
	server, err := gitapp.New(gitapp.Config{
		TenantID:            tenant,
		StorageRoot:         storageRoot,
		JournalDirectory:    journalDir,
		ResourceIDs:         map[string]string{"admin": "res-admin"},
		HookBinary:          exe,
		HookSocketDirectory: socketBase,
		Verify:              verifier,
		Actors:              readVerifier,
		ActorHandles:        registry,
		HookActors:          registry,
	})
	if err != nil {
		t.Fatalf("gitapp.New: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := server.StartHooks(ctx); err != nil {
		t.Fatalf("StartHooks: %v", err)
	}
	if err := server.InstallAllHooks(); err != nil {
		t.Fatalf("InstallAllHooks: %v", err)
	}

	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	// Step 1: Clone admin, add `repo my-app` + `option resource = my-app`, push!
	adminWork := filepath.Join(root, "admin-work")
	runTestGit(t, home, "", "admin-token", "clone", "--quiet", httpServer.URL+"/admin.git", adminWork)
	newPolicy := "repo admin\n" +
		"    RW = @admin\n" +
		"\n" +
		"repo my-app\n" +
		"    option resource = my-app\n" +
		"    R = @all\n" +
		"    RW+ refs/heads/work/ = @engineer\n" +
		"    RW refs/heads/main = @engineer\n"
	if err := os.WriteFile(filepath.Join(adminWork, "conf", "git.conf"), []byte(newPolicy), 0o644); err != nil {
		t.Fatalf("write git.conf: %v", err)
	}
	runTestGit(t, home, adminWork, "", "add", "conf/git.conf")
	runTestGit(t, home, adminWork, "", "-c", "user.name=Admin", "-c", "user.email=admin@example.invalid", "commit", "--quiet", "-m", "add my-app")
	runTestGit(t, home, adminWork, "admin-token", "push", "--quiet", "origin", "HEAD:refs/heads/main")

	// Step 2: IMMEDIATELY push to my-app (zero restart, zero provision-repository)!
	appWork := filepath.Join(root, "my-app-work")
	if err := os.MkdirAll(appWork, 0o755); err != nil {
		t.Fatalf("MkdirAll appWork: %v", err)
	}
	runTestGit(t, home, appWork, "", "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(appWork, "hello.txt"), []byte("zero-restart\n"), 0o644); err != nil {
		t.Fatalf("write hello.txt: %v", err)
	}
	runTestGit(t, home, appWork, "", "add", "hello.txt")
	runTestGit(t, home, appWork, "", "-c", "user.name=Eng", "-c", "user.email=eng@example.invalid", "commit", "--quiet", "-m", "initial commit")
	pushedCommit := runTestGit(t, home, appWork, "", "rev-parse", "HEAD")
	runTestGit(t, home, appWork, "", "remote", "add", "origin", httpServer.URL+"/my-app.git")
	runTestGit(t, home, appWork, "engineer-token", "push", "--quiet", "origin", "main")
	runTestGit(t, home, appWork, "engineer-token", "push", "--quiet", "origin", "HEAD:refs/heads/work/task-42")

	// Step 3: Clone my-app into a fresh directory and verify contents + verified reports!
	clonedWork := filepath.Join(root, "my-app-cloned")
	runTestGit(t, home, "", "engineer-token", "clone", "--quiet", httpServer.URL+"/my-app.git", clonedWork)
	clonedHead := runTestGit(t, home, clonedWork, "", "rev-parse", "HEAD")
	if clonedHead != pushedCommit {
		t.Fatalf("cloned HEAD = %q; want %q", clonedHead, pushedCommit)
	}
	content, err := os.ReadFile(filepath.Join(clonedWork, "hello.txt"))
	if err != nil || string(content) != "zero-restart\n" {
		t.Fatalf("cloned hello.txt = %q, %v; want zero-restart", content, err)
	}

	reports := verifier.reportSnapshot()
	var sawMain, sawWork bool
	for _, rep := range reports {
		if rep.ResourceID != "my-app" {
			continue
		}
		for _, field := range rep.Subject.Fields {
			if field.Name == "ref" && field.Value == "refs/heads/main" {
				sawMain = true
			}
			if field.Name == "ref" && field.Value == "refs/heads/work/task-42" {
				sawWork = true
			}
		}
	}
	if !sawMain || !sawWork {
		t.Fatalf("verified reports for my-app = %+v; want both refs/heads/main and refs/heads/work/task-42", reports)
	}
}
