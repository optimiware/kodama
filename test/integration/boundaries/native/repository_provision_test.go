//go:build integration

package native_test

import (
	"path/filepath"
	"strings"
	"testing"

	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

// TestRepositoryProvisionCLIDefaultRulesGovernAPush proves the operator
// provisioning flow end to end. The real binary provisions the tenant and then
// one repository through the product code path; the printed --resource mapping
// is exactly the mapping the composed service already serves; and, started over
// the provisioned state, the service accepts the push the committed default
// rules allow and refuses the push they do not.
func TestRepositoryProvisionCLIDefaultRulesGovernAPush(t *testing.T) {
	// The fixture's resource map already contains the shop/web mapping
	// (nativefix.ResourceID), which is the mapping the command must print;
	// SkipProvision keeps the admin repository for the real CLI commands below.
	harness := newHarness(t, nativefix.Options{SkipProvision: true})
	environment := harness.Environment()
	tenant := string(harness.Tenant)

	stdout, stderr, code := provisionCLI(t, environment,
		"git", "provision-tenant",
		"--tenant", tenant,
		"--storage-root", harness.StorageRoot,
		"--journal-dir", harness.JournalDir,
	)
	if code != 0 {
		t.Fatalf("kodama git provision-tenant exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, filepath.Join(harness.StorageRoot, tenant, nativefix.AdminRepository)) {
		t.Fatalf("provision-tenant stdout = %q; want the admin repository path", stdout)
	}

	stdout, stderr, code = provisionCLI(t, environment,
		"git", "provision-repository",
		"--tenant", tenant,
		"--storage-root", harness.StorageRoot,
		"--repository", nativefix.Repository,
		"--resource", nativefix.ResourceID,
	)
	if code != 0 {
		t.Fatalf("kodama git provision-repository exited %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("provision-repository stdout = %q; want a mapping line and a restart reminder", stdout)
	}
	wantMapping := "--resource " + nativefix.Repository + "=" + nativefix.ResourceID
	if lines[0] != wantMapping {
		t.Fatalf("mapping line = %q; want %q", lines[0], wantMapping)
	}
	if !strings.Contains(lines[1], "restart kodama git serve") {
		t.Fatalf("restart reminder = %q; want a git serve restart reminder", lines[1])
	}

	// Start a fresh service instance over the provisioned repository and the
	// printed resource mapping. The managed hooks are the ones the command
	// installed before the repository became reachable: nothing here installs
	// them again.
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart over the provisioned repository: %v", err)
	}

	// A work-branch push is allowed by the default RW+ rule for engineers and
	// executions.
	directory := filepath.Join(harness.Root, "provisioned-work")
	if cloneStderr, cloneCode := harness.Clone(t, nativefix.ExecutionToken, nativefix.Repository, directory); cloneCode != 0 {
		t.Fatalf("clone provisioned repository: %s", cloneStderr)
	}
	allowed := harness.Commit(t, directory, "allowed work branch")
	pushStderr, pushCode := harness.Push(t, directory, nativefix.ExecutionToken, "origin", "HEAD:refs/heads/work/feature")
	requireSuccess(t, pushStderr, pushCode)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/work/feature"); observed != allowed {
		t.Fatalf("allowed work ref = %q; want %q", observed, allowed)
	}

	// The default rules give the engineer role no rule for the default branch,
	// so the push is refused and the ref stays absent.
	refused := harness.Commit(t, directory, "refused default branch")
	pushStderr, pushCode = harness.Push(t, directory, harness.HumanToken, "origin", "HEAD:refs/heads/main")
	requireFailure(t, pushStderr, pushCode)
	requireRejectionMessage(t, pushStderr)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != "" {
		t.Fatalf("refused push moved main to %q; want it absent (candidate %q)", observed, refused)
	}
	if refused == allowed {
		t.Fatal("fixture commits are identical")
	}
}
