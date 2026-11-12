package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
)

// TestGitProvisionTenantDispatchesIndependently pins that
// `git provision-tenant` is its own subcommand and starts no other runner.
func TestGitProvisionTenantDispatchesIndependently(t *testing.T) {
	recorder := &dispatchRecorder{}
	deps := deps{
		server:       recorder.runner("server"),
		gitServe:     recorder.runner("gitServe"),
		gitHook:      recorder.runner("gitHook"),
		gitProvision: recorder.runner("gitProvision"),
		agent:        recorder.runner("agent"),
		services:     recorder.runner("services"),
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"git", "provision-tenant",
		"--tenant", "tenant-x",
		"--storage-root", "/srv/git",
		"--journal-dir", "/var/lib/kodama/journal",
	}, &stdout, &stderr, deps)
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
	}
	if len(recorder.calls) != 1 || recorder.calls[0] != "gitProvision" {
		t.Fatalf("calls = %v; want [gitProvision]", recorder.calls)
	}
}

// TestGitProvisionTenantRequiresFlags pins the required operator inputs and
// that a refused invocation never calls the provisioner.
func TestGitProvisionTenantRequiresFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no-flags", nil},
		{"missing-tenant", []string{"--storage-root", "/srv/git", "--journal-dir", "/var/lib/kodama/journal"}},
		{"missing-storage", []string{"--tenant", "tenant-x", "--journal-dir", "/var/lib/kodama/journal"}},
		{"missing-journal", []string{"--tenant", "tenant-x", "--storage-root", "/srv/git"}},
		{"unexpected-argument", []string{"--tenant", "tenant-x", "--storage-root", "/srv/git", "--journal-dir", "/var/lib/kodama/journal", "extra"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			called := false
			original := provisionTenant
			provisionTenant = func(context.Context, gitapp.ProvisionConfig) (gitapp.ProvisionResult, error) {
				called = true
				return gitapp.ProvisionResult{}, nil
			}
			defer func() { provisionTenant = original }()

			var stdout, stderr bytes.Buffer
			code := runGitProvisionEntry(context.Background(), testCase.args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("exit code = %d; want 2; stderr=%s", code, stderr.String())
			}
			if called {
				t.Fatal("a refused invocation called the provisioner")
			}
			if !strings.Contains(stderr.String(), "usage: kodama") {
				t.Fatalf("stderr = %q; want usage", stderr.String())
			}
		})
	}
}

// TestGitProvisionTenantInvokesProvisioner pins the parsed configuration handed
// to the app boundary.
func TestGitProvisionTenantInvokesProvisioner(t *testing.T) {
	var received gitapp.ProvisionConfig
	original := provisionTenant
	provisionTenant = func(_ context.Context, config gitapp.ProvisionConfig) (gitapp.ProvisionResult, error) {
		received = config
		return gitapp.ProvisionResult{RepositoryPath: "/srv/git/tenant-x/admin", Created: true}, nil
	}
	defer func() { provisionTenant = original }()

	var stdout, stderr bytes.Buffer
	code := runGitProvisionEntry(context.Background(), []string{
		"--tenant", "tenant-x",
		"--storage-root", "/srv/git",
		"--journal-dir", "/var/lib/kodama/journal",
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
	if received.JournalDirectory != "/var/lib/kodama/journal" {
		t.Fatalf("journal directory = %q; want /var/lib/kodama/journal", received.JournalDirectory)
	}
	if !strings.Contains(stdout.String(), "/srv/git/tenant-x/admin") {
		t.Fatalf("stdout = %q; want the provisioned repository path", stdout.String())
	}
}

// TestGitProvisionTenantIsIdempotent pins CLI idempotence: a valid existing
// repository reports exit 0 with Created=false and prints exactly the same
// admin path as the creating run.
func TestGitProvisionTenantIsIdempotent(t *testing.T) {
	original := provisionTenant
	calls := 0
	provisionTenant = func(_ context.Context, _ gitapp.ProvisionConfig) (gitapp.ProvisionResult, error) {
		calls++
		return gitapp.ProvisionResult{
			RepositoryPath: "/srv/git/tenant-x/admin",
			Created:        calls == 1,
		}, nil
	}
	defer func() { provisionTenant = original }()

	args := []string{
		"--tenant", "tenant-x",
		"--storage-root", "/srv/git",
		"--journal-dir", "/var/lib/kodama/journal",
	}
	var firstStdout, firstStderr bytes.Buffer
	if code := runGitProvisionEntry(context.Background(), args, &firstStdout, &firstStderr); code != 0 {
		t.Fatalf("first exit code = %d; stderr=%s", code, firstStderr.String())
	}
	var secondStdout, secondStderr bytes.Buffer
	if code := runGitProvisionEntry(context.Background(), args, &secondStdout, &secondStderr); code != 0 {
		t.Fatalf("second (existing repository) exit code = %d; stderr=%s", code, secondStderr.String())
	}
	if secondStderr.Len() != 0 {
		t.Fatalf("second stderr = %q; want empty", secondStderr.String())
	}
	if secondStdout.String() != firstStdout.String() {
		t.Fatalf("second stdout = %q; want the first admin path %q",
			secondStdout.String(), firstStdout.String())
	}
	if !strings.Contains(secondStdout.String(), "/srv/git/tenant-x/admin") {
		t.Fatalf("second stdout = %q; want the admin repository path", secondStdout.String())
	}
	if calls != 2 {
		t.Fatalf("provisioner calls = %d; want 2", calls)
	}
}

// TestGitProvisionTenantReportsFailure pins that a provisioning refusal is a
// nonzero exit with the cause on stderr.
func TestGitProvisionTenantReportsFailure(t *testing.T) {
	original := provisionTenant
	provisionTenant = func(context.Context, gitapp.ProvisionConfig) (gitapp.ProvisionResult, error) {
		return gitapp.ProvisionResult{}, errors.New("destination refused")
	}
	defer func() { provisionTenant = original }()

	var stdout, stderr bytes.Buffer
	code := runGitProvisionEntry(context.Background(), []string{
		"--tenant", "tenant-x",
		"--storage-root", "/srv/git",
		"--journal-dir", "/var/lib/kodama/journal",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d; want 1", code)
	}
	if !strings.Contains(stderr.String(), "destination refused") {
		t.Fatalf("stderr = %q; want the refusal cause", stderr.String())
	}
}
