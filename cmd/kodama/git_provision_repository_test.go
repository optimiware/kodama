package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
)

// TestGitProvisionRepositoryDispatchesIndependently pins that
// `git provision-repository` is its own subcommand and starts no other runner.
func TestGitProvisionRepositoryDispatchesIndependently(t *testing.T) {
	recorder := &dispatchRecorder{}
	deps := deps{
		server:                 recorder.runner("server"),
		gitServe:               recorder.runner("gitServe"),
		gitHook:                recorder.runner("gitHook"),
		gitProvision:           recorder.runner("gitProvision"),
		gitProvisionRepository: recorder.runner("gitProvisionRepository"),
		agent:                  recorder.runner("agent"),
		services:               recorder.runner("services"),
		migrate:                recorder.runner("migrate"),
		bootstrap:              recorder.runner("bootstrap"),
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"git", "provision-repository",
		"--tenant", "tenant-x",
		"--storage-root", "/srv/git",
		"--repository", "shop/web",
		"--resource", "res-1",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}
	if len(recorder.calls) != 1 || recorder.calls[0] != "gitProvisionRepository" {
		t.Fatalf("calls = %v; want [gitProvisionRepository]", recorder.calls)
	}
}

// TestGitProvisionRepositoryRequiresFlags pins the required operator inputs and
// that a refused invocation never calls the provisioner.
func TestGitProvisionRepositoryRequiresFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no-flags", nil},
		{"missing-tenant", []string{"--storage-root", "/srv/git", "--repository", "shop/web", "--resource", "res-1"}},
		{"missing-storage", []string{"--tenant", "tenant-x", "--repository", "shop/web", "--resource", "res-1"}},
		{"missing-repository", []string{"--tenant", "tenant-x", "--storage-root", "/srv/git", "--resource", "res-1"}},
		{"missing-resource", []string{"--tenant", "tenant-x", "--storage-root", "/srv/git", "--repository", "shop/web"}},
		{"invalid-resource-space", []string{"--tenant", "tenant-x", "--storage-root", "/srv/git", "--repository", "shop/web", "--resource", "res 1"}},
		{"invalid-resource-equals", []string{"--tenant", "tenant-x", "--storage-root", "/srv/git", "--repository", "shop/web", "--resource", "res=1"}},
		{"invalid-resource-newline", []string{"--tenant", "tenant-x", "--storage-root", "/srv/git", "--repository", "shop/web", "--resource", "res\n1"}},
		{"invalid-resource-dot", []string{"--tenant", "tenant-x", "--storage-root", "/srv/git", "--repository", "shop/web", "--resource", "."}},
		{"invalid-resource-slash", []string{"--tenant", "tenant-x", "--storage-root", "/srv/git", "--repository", "shop/web", "--resource", "res/1"}},
		{"unexpected-argument", []string{
			"--tenant", "tenant-x",
			"--storage-root", "/srv/git",
			"--repository", "shop/web",
			"--resource", "res-1",
			"extra",
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			called := false
			original := provisionRepository
			provisionRepository = func(context.Context, gitapp.RepositoryProvisionConfig) (gitapp.RepositoryProvisionResult, error) {
				called = true
				return gitapp.RepositoryProvisionResult{}, nil
			}
			defer func() { provisionRepository = original }()

			var stdout, stderr bytes.Buffer
			code := runGitProvisionRepositoryEntry(context.Background(), testCase.args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("exit code = %d; want 2; stderr=%s", code, stderr.String())
			}
			if called {
				t.Fatal("a refused invocation called the provisioner")
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q; want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), "usage: kodama") {
				t.Fatalf("stderr = %q; want usage", stderr.String())
			}
		})
	}
}

// TestGitProvisionRepositoryInvokesProvisioner pins the parsed configuration
// handed to the app boundary and the exact stdout contract: the --resource
// mapping followed by the restart reminder.
func TestGitProvisionRepositoryInvokesProvisioner(t *testing.T) {
	var received gitapp.RepositoryProvisionConfig
	original := provisionRepository
	provisionRepository = func(_ context.Context, config gitapp.RepositoryProvisionConfig) (gitapp.RepositoryProvisionResult, error) {
		received = config
		return gitapp.RepositoryProvisionResult{
			RepositoryPath: "/srv/git/tenant-x/shop/web",
			Repository:     "shop/web",
			Created:        true,
		}, nil
	}
	defer func() { provisionRepository = original }()

	var stdout, stderr bytes.Buffer
	code := runGitProvisionRepositoryEntry(context.Background(), []string{
		"--tenant", "tenant-x",
		"--storage-root", "/srv/git",
		"--repository", "shop/web",
		"--resource", "res-1",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q; want empty", stderr.String())
	}
	if string(received.TenantID) != "tenant-x" {
		t.Fatalf("tenant = %q; want tenant-x", received.TenantID)
	}
	if received.StorageRoot != "/srv/git" {
		t.Fatalf("storage root = %q; want /srv/git", received.StorageRoot)
	}
	if received.Repository != "shop/web" {
		t.Fatalf("repository = %q; want shop/web", received.Repository)
	}
	want := "--resource shop/web=res-1\n" +
		"restart kodama git serve with the new --resource mapping\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q; want %q", stdout.String(), want)
	}
}

// TestGitProvisionRepositoryIsIdempotent pins CLI idempotence: a valid existing
// repository reports exit 0 with Created=false and prints the same mapping.
func TestGitProvisionRepositoryIsIdempotent(t *testing.T) {
	original := provisionRepository
	calls := 0
	provisionRepository = func(_ context.Context, _ gitapp.RepositoryProvisionConfig) (gitapp.RepositoryProvisionResult, error) {
		calls++
		return gitapp.RepositoryProvisionResult{
			RepositoryPath: "/srv/git/tenant-x/shop/web",
			Created:        calls == 1,
		}, nil
	}
	defer func() { provisionRepository = original }()

	args := []string{
		"--tenant", "tenant-x",
		"--storage-root", "/srv/git",
		"--repository", "shop/web",
		"--resource", "res-1",
	}
	var firstStdout, firstStderr bytes.Buffer
	if code := runGitProvisionRepositoryEntry(context.Background(), args, &firstStdout, &firstStderr); code != 0 {
		t.Fatalf("first exit code = %d; stderr=%s", code, firstStderr.String())
	}
	var secondStdout, secondStderr bytes.Buffer
	if code := runGitProvisionRepositoryEntry(context.Background(), args, &secondStdout, &secondStderr); code != 0 {
		t.Fatalf("second (existing repository) exit code = %d; stderr=%s", code, secondStderr.String())
	}
	if secondStderr.Len() != 0 {
		t.Fatalf("second stderr = %q; want empty", secondStderr.String())
	}
	if secondStdout.String() != firstStdout.String() {
		t.Fatalf("second stdout = %q; want the first mapping output %q",
			secondStdout.String(), firstStdout.String())
	}
	if calls != 2 {
		t.Fatalf("provisioner calls = %d; want 2", calls)
	}
}

// TestGitProvisionRepositoryReportsFailure pins that a provisioning refusal is
// a nonzero exit with the cause on stderr and no mapping on stdout.
func TestGitProvisionRepositoryReportsFailure(t *testing.T) {
	original := provisionRepository
	provisionRepository = func(context.Context, gitapp.RepositoryProvisionConfig) (gitapp.RepositoryProvisionResult, error) {
		return gitapp.RepositoryProvisionResult{}, errors.New("destination refused")
	}
	defer func() { provisionRepository = original }()

	var stdout, stderr bytes.Buffer
	code := runGitProvisionRepositoryEntry(context.Background(), []string{
		"--tenant", "tenant-x",
		"--storage-root", "/srv/git",
		"--repository", "shop/web",
		"--resource", "res-1",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q; want empty on failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "destination refused") {
		t.Fatalf("stderr = %q; want the refusal cause", stderr.String())
	}
}
