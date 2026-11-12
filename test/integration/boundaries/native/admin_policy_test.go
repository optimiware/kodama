//go:build integration

package native_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

// policyDenyEngineer removes the engineer write rule for shop/web main while
// keeping execution writes. It retains one unrelated engineer write ref so the
// smart-HTTP repository-level write admission passes and the pre-receive hook
// makes the exact per-ref decision; a policy with no engineer write rule at all
// would be refused at advertisement with a 403 and the hook would never run.
const policyDenyEngineer = `
repo admin
    RW = @admin

repo shop/web
    R = @all
    RW refs/heads/rewind = @engineer
    RW refs/heads/ = @execution
`

// policyAllowEngineer grants the engineer role writes to shop/web. It is the
// permissive policy another tenant must never be able to supply.
const policyAllowEngineer = `
repo admin
    RW = @admin

repo shop/web
    R = @all
    RW refs/heads/ = @engineer
`

func newAdminHarness(t *testing.T, options nativefix.Options) *nativefix.Harness {
	t.Helper()
	// The fixture provisions the admin repository and installs the committed
	// admin policy once; no policy file exists for the app to read.
	return newHarness(t, options)
}

// seedWebWithHooks creates shop/web and its initial main commit directly, then
// installs the real product hooks. Setup never depends on the policy under
// test, so the first service operation is the observable assertion.
func seedWebWithHooks(t *testing.T, harness *nativefix.Harness) string {
	t.Helper()
	commit := harness.SeedRepository(t, nativefix.Repository)
	harness.InstallProductHooks(t, nativefix.Repository)
	return commit
}

func cloneAdmin(t *testing.T, harness *nativefix.Harness, name string) string {
	t.Helper()
	directory := filepath.Join(harness.Root, name)
	if stderr, code := harness.Clone(t, harness.AdminToken, nativefix.AdminRepository, directory); code != 0 {
		t.Fatalf("admin clone: %s", stderr)
	}
	return directory
}

func writeAdminPolicy(t *testing.T, directory, policy string) {
	t.Helper()
	path := filepath.Join(directory, "conf", "git.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create admin conf directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(policy), 0o600); err != nil {
		t.Fatalf("write admin policy: %v", err)
	}
}

func commitAdminPolicy(t *testing.T, harness *nativefix.Harness, directory, message string) string {
	t.Helper()
	if _, stderr, code := harness.GitIn(t, directory, "add", "--all"); code != 0 {
		t.Fatalf("git add admin policy: %s", stderr)
	}
	if _, stderr, code := harness.GitIn(t, directory,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit", "-m", message); code != 0 {
		t.Fatalf("git commit admin policy: %s", stderr)
	}
	stdout, stderr, code := harness.GitIn(t, directory, "rev-parse", "HEAD")
	if code != 0 {
		t.Fatalf("git rev-parse admin HEAD: %s", stderr)
	}
	return strings.TrimSpace(stdout)
}

func requirePush(t *testing.T, harness *nativefix.Harness, directory, token string) {
	t.Helper()
	stderr, code := harness.Push(t, directory, token, "origin", "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
}

func pushAdmin(t *testing.T, harness *nativefix.Harness, directory, refspec string) (string, int) {
	t.Helper()
	return harness.Push(t, directory, harness.AdminToken, "origin", refspec)
}

func requireAdminPush(t *testing.T, harness *nativefix.Harness, directory string) {
	t.Helper()
	stderr, code := pushAdmin(t, harness, directory, "HEAD:refs/heads/main")
	requireSuccess(t, stderr, code)
}

// TestAcceptedAdminPushChangesNextAuthorization pins the invariant end to end:
// the runtime reads only the committed conf/git.conf, so an accepted admin
// push changes the next authorization decision and the old rule stops applying.
func TestAcceptedAdminPushChangesNextAuthorization(t *testing.T) {
	harness := newAdminHarness(t, nativefix.Options{})
	seedWebWithHooks(t, harness)

	// The fixture policy grants the engineer a write of shop/web. Prove the old
	// rule applies before the admin changes it.
	human := cloneFor(t, harness, harness.HumanToken, "human-admin-accepted")
	second := harness.Commit(t, human, "second")
	requirePush(t, harness, human, harness.HumanToken)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != second {
		t.Fatalf("remote main = %q; want %q", observed, second)
	}

	// The admin replaces the committed policy: the engineer write rule is gone.
	admin := cloneAdmin(t, harness, "admin-accepted")
	writeAdminPolicy(t, admin, policyDenyEngineer)
	accepted := commitAdminPolicy(t, harness, admin, "deny engineer writes")
	requireAdminPush(t, harness, admin)
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != accepted {
		t.Fatalf("admin main = %q; want the accepted policy %q", observed, accepted)
	}

	// The next human attempt must be decided by the accepted admin commit, not
	// a cached rule or a working-tree file.
	third := harness.Commit(t, human, "third")
	stderr, code := harness.Push(t, human, harness.HumanToken, "origin", "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != second {
		t.Fatalf("refused push moved main to %q; want %q", observed, second)
	}

	// Execution authority is still granted by the accepted policy, proving the
	// refusal above is the changed rule and not a fail-closed app.
	execution := cloneFor(t, harness, nativefix.ExecutionToken, "execution-admin-accepted")
	fourth := harness.Commit(t, execution, "fourth")
	requirePush(t, harness, execution, nativefix.ExecutionToken)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != fourth {
		t.Fatalf("remote main = %q; want %q", observed, fourth)
	}
	if third == fourth {
		t.Fatal("fixture commits are identical")
	}
}

func TestAdminPolicyUsesAcceptedRefOnly(t *testing.T) {
	harness := newAdminHarness(t, nativefix.Options{})
	seedWebWithHooks(t, harness)

	human := cloneFor(t, harness, harness.HumanToken, "human-admin-ref")
	harness.Commit(t, human, "second")
	requirePush(t, harness, human, harness.HumanToken)

	// A work-branch candidate carries the new rules but is not accepted.
	admin := cloneAdmin(t, harness, "admin-ref")
	if _, stderr, code := harness.GitIn(t, admin, "checkout", "-b", "work/candidate"); code != 0 {
		t.Fatalf("checkout candidate: %s", stderr)
	}
	writeAdminPolicy(t, admin, policyDenyEngineer)
	candidate := commitAdminPolicy(t, harness, admin, "candidate deny engineer")
	if stderr, code := pushAdmin(t, harness, admin, "HEAD:refs/heads/work/candidate"); code != 0 {
		t.Fatalf("push admin candidate: %s", stderr)
	}
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/work/candidate"); observed != candidate {
		t.Fatalf("candidate ref = %q; want %q", observed, candidate)
	}

	// The unaccepted candidate must not become policy.
	third := harness.Commit(t, human, "third")
	requirePush(t, harness, human, harness.HumanToken)

	// Accepting the candidate on the trusted ref changes the next attempt.
	if _, stderr, code := harness.GitIn(t, admin, "checkout", "main"); code != 0 {
		t.Fatalf("checkout admin main: %s", stderr)
	}
	if _, stderr, code := harness.GitIn(t, admin, "merge", "--ff-only", "work/candidate"); code != 0 {
		t.Fatalf("merge candidate: %s", stderr)
	}
	requireAdminPush(t, harness, admin)

	if stderr, code := harness.Fetch(t, human, harness.HumanToken); code != 0 {
		t.Fatalf("fetch accepted main: %s", stderr)
	}
	if _, stderr, code := harness.GitIn(t, human, "reset", "--hard", "origin/main"); code != 0 {
		t.Fatalf("reset human main: %s", stderr)
	}
	fourth := harness.Commit(t, human, "fourth")
	stderr, code := harness.Push(t, human, harness.HumanToken, "origin", "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != third {
		t.Fatalf("refused push moved main to %q; want %q", observed, third)
	}
	if fourth == third {
		t.Fatal("fixture commits are identical")
	}
}

func TestAdminPolicyRestartUsesCommittedRules(t *testing.T) {
	harness := newAdminHarness(t, nativefix.Options{})
	seedWebWithHooks(t, harness)

	human := cloneFor(t, harness, harness.HumanToken, "human-admin-restart")
	second := harness.Commit(t, human, "second")
	requirePush(t, harness, human, harness.HumanToken)

	admin := cloneAdmin(t, harness, "admin-restart")
	writeAdminPolicy(t, admin, policyDenyEngineer)
	commitAdminPolicy(t, harness, admin, "deny engineer writes")
	requireAdminPush(t, harness, admin)

	// Restart over the same durable storage. The app must reconstruct the
	// accepted rules from the admin commit, never from an in-memory cache.
	if err := harness.Restart(t); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	human = cloneFor(t, harness, harness.HumanToken, "human-admin-restart-after")
	third := harness.Commit(t, human, "third")
	stderr, code := harness.Push(t, human, harness.HumanToken, "origin", "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != second {
		t.Fatalf("refused push after restart moved main to %q; want %q", observed, second)
	}

	// The restarted app still serves an authority the accepted policy grants.
	execution := cloneFor(t, harness, nativefix.ExecutionToken, "execution-admin-restart")
	fourth := harness.Commit(t, execution, "fourth")
	requirePush(t, harness, execution, nativefix.ExecutionToken)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != fourth {
		t.Fatalf("remote main = %q; want %q", observed, fourth)
	}
	if third == fourth {
		t.Fatal("fixture commits are identical")
	}
}

func TestInvalidAdminPolicyRefusedBeforeReceive(t *testing.T) {
	harness := newAdminHarness(t, nativefix.Options{})
	seedWebWithHooks(t, harness)
	admin := cloneAdmin(t, harness, "admin-invalid")
	accepted := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main")
	if accepted == "" {
		t.Fatal("the fixture admin repository has no committed policy")
	}

	// A malformed proposed policy must be refused before receive-pack moves
	// the admin ref.
	writeAdminPolicy(t, admin, "repo shop/web\nRW = @wizard\n")
	commitAdminPolicy(t, harness, admin, "malformed policy")
	stderr, code := pushAdmin(t, harness, admin, "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != accepted {
		t.Fatalf("malformed policy moved admin main to %q; want %q", observed, accepted)
	}

	// A missing policy is equally invalid: it must never mean "no rules".
	if _, stderr, code := harness.GitIn(t, admin, "reset", "--hard", "origin/main"); code != 0 {
		t.Fatalf("reset admin after refusal: %s", stderr)
	}
	if err := os.Remove(filepath.Join(admin, "conf", "git.conf")); err != nil {
		t.Fatalf("remove admin policy: %v", err)
	}
	commitAdminPolicy(t, harness, admin, "missing policy")
	stderr, code = pushAdmin(t, harness, admin, "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != accepted {
		t.Fatalf("missing policy moved admin main to %q; want %q", observed, accepted)
	}

	// The established policy remains in force; a refusal never widens access.
	human := cloneFor(t, harness, harness.HumanToken, "human-admin-invalid")
	second := harness.Commit(t, human, "second")
	requirePush(t, harness, human, harness.HumanToken)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != second {
		t.Fatalf("remote main = %q; want %q", observed, second)
	}
}

func TestAdminPolicyTenantIsolation(t *testing.T) {
	// This tenant's accepted admin policy removes the engineer write rule.
	harness := newAdminHarness(t, nativefix.Options{AdminPolicy: policyDenyEngineer})
	seedWebWithHooks(t, harness)

	// Another tenant's admin repository grants the engineer write access. It
	// must never become this tenant's policy.
	harness.SeedAdminPolicy(t, nativefix.OtherTenant, policyAllowEngineer)

	execution := cloneFor(t, harness, nativefix.ExecutionToken, "execution-admin-isolation")
	second := harness.Commit(t, execution, "second")
	requirePush(t, harness, execution, nativefix.ExecutionToken)

	human := cloneFor(t, harness, harness.HumanToken, "human-admin-isolation")
	third := harness.Commit(t, human, "third")
	stderr, code := harness.Push(t, human, harness.HumanToken, "origin", "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != second {
		t.Fatalf("refused push moved main to %q; want %q", observed, second)
	}
	if third == second {
		t.Fatal("fixture commits are identical")
	}
}

// adminRefPath returns the loose trusted-ref path in the fixture tenant's real
// admin repository. Corruption helpers write it directly, exactly as a broken
// ref looks on disk.
func adminRefPath(t *testing.T, harness *nativefix.Harness) string {
	t.Helper()
	return filepath.Join(harness.RepositoryPath(nativefix.AdminRepository), "refs", "heads", "main")
}

// requireEngineerPushRefused pushes one prepared human (engineer) commit and
// requires it to be refused without moving main. The clone and commit are made
// before the trusted admin repository is corrupted, so the only failure under
// test is the push itself.
func requireEngineerPushRefused(t *testing.T, harness *nativefix.Harness, directory, base, message string) {
	t.Helper()
	harness.Commit(t, directory, message)
	stderr, code := harness.Push(t, directory, harness.HumanToken, "origin", "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != base {
		t.Fatalf("refused push moved main to %q; want %q", observed, base)
	}
}

func TestAdminPolicyBrokenTrustedRefFailsClosed(t *testing.T) {
	for _, broken := range []struct {
		name    string
		content []byte
	}{
		{name: "empty", content: nil},
		{name: "garbage", content: []byte("not-an-object-id\n")},
	} {
		t.Run(broken.name, func(t *testing.T) {
			// The committed policy denies engineer writes. A broken trusted
			// ref has no policy to read and must fail closed.
			harness := newAdminHarness(t, nativefix.Options{AdminPolicy: policyDenyEngineer})
			base := seedWebWithHooks(t, harness)
			human := cloneFor(t, harness, harness.HumanToken, "human-broken-ref-"+broken.name)

			ref := adminRefPath(t, harness)
			if _, err := os.Stat(ref); err != nil {
				t.Fatalf("trusted ref file: %v", err)
			}
			if err := os.WriteFile(ref, broken.content, 0o600); err != nil {
				t.Fatalf("corrupt trusted ref: %v", err)
			}
			requireEngineerPushRefused(t, harness, human, base, "broken-ref-"+broken.name)
		})
	}
}

func TestAdminPolicyIgnoresReplaceObjects(t *testing.T) {
	// The accepted commit carries the permissive policy. A replacement object
	// would swap in the denying policy for the same object id; policy reads
	// must ignore refs/replace so only the accepted commit's own blob counts.
	harness := newAdminHarness(t, nativefix.Options{})
	seedWebWithHooks(t, harness)

	accepted := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main")
	if accepted == "" {
		t.Fatal("the fixture admin repository has no committed policy")
	}
	admin := cloneAdmin(t, harness, "admin-replace")
	writeAdminPolicy(t, admin, policyDenyEngineer)
	replacement := commitAdminPolicy(t, harness, admin, "replacement policy")
	if stderr, code := pushAdmin(t, harness, admin, "HEAD:refs/heads/replacement"); code != 0 {
		t.Fatalf("push replacement commit: %s", stderr)
	}
	adminPath := harness.RepositoryPath(nativefix.AdminRepository)
	if _, stderr, code := harness.Git(t, "--git-dir="+adminPath,
		"update-ref", "refs/replace/"+accepted, replacement); code != 0 {
		t.Fatalf("install replace object: %s", stderr)
	}

	human := cloneFor(t, harness, harness.HumanToken, "human-replace")
	commit := harness.Commit(t, human, "replace-allowed")
	requirePush(t, harness, human, harness.HumanToken)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != commit {
		t.Fatalf("remote main = %q; want %q", observed, commit)
	}
}

func TestAdminPolicyDiscoveryDoesNotWalkUp(t *testing.T) {
	// An existing but non-repository admin directory must fail closed, even when
	// an ancestor work tree carries a permissive policy. Git discovery must not
	// climb out of the trusted admin path.
	harness := newAdminHarness(t, nativefix.Options{AdminPolicy: policyDenyEngineer})
	base := seedWebWithHooks(t, harness)
	human := cloneFor(t, harness, harness.HumanToken, "human-walk-up")

	adminPath := harness.RepositoryPath(nativefix.AdminRepository)
	if err := os.RemoveAll(adminPath); err != nil {
		t.Fatalf("remove admin repository: %v", err)
	}
	if err := os.MkdirAll(adminPath, 0o755); err != nil {
		t.Fatalf("create non-repository admin directory: %v", err)
	}
	tenantPath := filepath.Dir(adminPath)
	if _, stderr, code := harness.GitIn(t, tenantPath, "init", "--quiet", "-b", "main"); code != 0 {
		t.Fatalf("init ancestor work tree: %s", stderr)
	}
	policyPath := filepath.Join(tenantPath, "conf", "git.conf")
	if err := os.MkdirAll(filepath.Dir(policyPath), 0o755); err != nil {
		t.Fatalf("create ancestor conf directory: %v", err)
	}
	if err := os.WriteFile(policyPath, []byte(policyAllowEngineer), 0o600); err != nil {
		t.Fatalf("write ancestor policy: %v", err)
	}
	if _, stderr, code := harness.GitIn(t, tenantPath, "add", "--all"); code != 0 {
		t.Fatalf("git add ancestor policy: %s", stderr)
	}
	if _, stderr, code := harness.GitIn(t, tenantPath,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit", "-m", "ancestor policy"); code != 0 {
		t.Fatalf("commit ancestor policy: %s", stderr)
	}

	requireEngineerPushRefused(t, harness, human, base, "walk-up")
}

func TestAdminPolicyValidatesEveryMainLineInABatch(t *testing.T) {
	harness := newAdminHarness(t, nativefix.Options{})
	admin := cloneAdmin(t, harness, "admin-multiref")
	accepted := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main")

	// Seed a second existing ref. receive-pack reports updates of existing refs
	// before created refs, so the subsequent batch lists refs/heads/aaa first
	// and refs/heads/main second.
	if stderr, code := pushAdmin(t, harness, admin, "HEAD:refs/heads/aaa"); code != 0 {
		t.Fatalf("seed admin aaa ref: %s", stderr)
	}
	writeAdminPolicy(t, admin, "repo shop/web\nRW = @wizard\n")
	candidate := commitAdminPolicy(t, harness, admin, "malformed batch policy")

	// Validating only the first batch line (aaa, whose policy is unchanged)
	// would accept this malformed main policy.
	stderr, code := harness.Push(t, admin, harness.AdminToken, "origin", "HEAD:refs/heads/aaa", "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	requireRejectionMessage(t, stderr)
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != accepted {
		t.Fatalf("refused batch moved admin main to %q; want %q", observed, accepted)
	}
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/aaa"); observed != accepted {
		t.Fatalf("refused batch moved admin aaa to %q; want %q (candidate %q)", observed, accepted, candidate)
	}
}

// requireGitSeesNoTrustedRef proves the fault under test is invisible to raw
// git exactly like genuine absence: for-each-ref exits zero with no output and
// no diagnostic, and rev-parse --verify --quiet exits one with no output. That
// is the state that must never be mistaken for an unprovisioned tenant.
func requireGitSeesNoTrustedRef(t *testing.T, harness *nativefix.Harness, adminPath string) {
	t.Helper()
	stdout, stderr, code := harness.Git(t, "--git-dir="+adminPath,
		"for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/main")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("git for-each-ref = (%q, %q, %d); want silent absence", stdout, stderr, code)
	}
	stdout, stderr, code = harness.Git(t, "--git-dir="+adminPath,
		"rev-parse", "--verify", "--quiet", "refs/heads/main")
	if code != 1 || stdout != "" || stderr != "" {
		t.Fatalf("git rev-parse --verify = (%q, %q, %d); want silent absence", stdout, stderr, code)
	}
}

// chmodForFault removes every permission bit and restores them before the
// harness removes its temporary tree.
func chmodForFault(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod %q: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
}

// TestAdminPolicyUnreadableRefStoreFailsClosed corrupts the established admin
// repository so its trusted ref cannot be read. The engineer push must be
// refused: an unreadable committed policy is a dependency failure, never a
// fallback.
func TestAdminPolicyUnreadableRefStoreFailsClosed(t *testing.T) {
	faults := []struct {
		name   string
		silent bool
		// chmod marks a fault injected by removing permission bits. Root reads
		// through them, so those cases are skipped as root; the others still
		// run.
		chmod  bool
		mutate func(t *testing.T, harness *nativefix.Harness, adminPath string)
	}{
		{
			name:   "refs/heads unreadable",
			silent: true,
			chmod:  true,
			mutate: func(t *testing.T, harness *nativefix.Harness, adminPath string) {
				chmodForFault(t, filepath.Join(adminPath, "refs", "heads"))
			},
		},
		{
			name:  "refs unreadable",
			chmod: true,
			mutate: func(t *testing.T, harness *nativefix.Harness, adminPath string) {
				chmodForFault(t, filepath.Join(adminPath, "refs"))
			},
		},
		{
			name:  "packed-refs unreadable",
			chmod: true,
			mutate: func(t *testing.T, harness *nativefix.Harness, adminPath string) {
				if _, stderr, code := harness.Git(t, "--git-dir="+adminPath, "pack-refs", "--all"); code != 0 {
					t.Fatalf("pack admin refs: %s", stderr)
				}
				chmodForFault(t, filepath.Join(adminPath, "packed-refs"))
			},
		},
		{
			name:   "refs/heads replaced by a file",
			silent: true,
			mutate: func(t *testing.T, harness *nativefix.Harness, adminPath string) {
				heads := filepath.Join(adminPath, "refs", "heads")
				if err := os.Rename(heads, heads+".hidden"); err != nil {
					t.Fatalf("hide refs/heads directory: %v", err)
				}
				if err := os.WriteFile(heads, []byte("not a directory\n"), 0o600); err != nil {
					t.Fatalf("replace refs/heads with a file: %v", err)
				}
			},
		},
	}
	for _, fault := range faults {
		t.Run(fault.name, func(t *testing.T) {
			if fault.chmod && os.Geteuid() == 0 {
				t.Skip("permission fault injection is not observable as root")
			}
			harness := newAdminHarness(t, nativefix.Options{AdminPolicy: policyDenyEngineer})
			base := seedWebWithHooks(t, harness)
			human := cloneFor(t, harness, harness.HumanToken, "human-ref-store-fault")
			adminPath := harness.RepositoryPath(nativefix.AdminRepository)
			fault.mutate(t, harness, adminPath)
			if fault.silent {
				requireGitSeesNoTrustedRef(t, harness, adminPath)
			}
			requireEngineerPushRefused(t, harness, human, base, "ref-store-fault")
		})
	}
}

// TestAdminPolicyReftableConfigFailsClosed switches the established admin
// repository to the reftable backend without migrating its loose refs. Raw git
// then sees no refs at all although refs/heads/main is still on disk; the push
// must fail closed instead of reading a different policy.
func TestAdminPolicyReftableConfigFailsClosed(t *testing.T) {
	harness := newAdminHarness(t, nativefix.Options{AdminPolicy: policyDenyEngineer})
	base := seedWebWithHooks(t, harness)
	human := cloneFor(t, harness, harness.HumanToken, "human-reftable")
	adminPath := harness.RepositoryPath(nativefix.AdminRepository)
	accepted := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main")
	if accepted == "" {
		t.Fatal("the fixture admin repository has no committed policy")
	}
	config := "[core]\n\trepositoryformatversion = 1\n\tbare = true\n[extensions]\n\trefStorage = reftable\n"
	if err := os.WriteFile(filepath.Join(adminPath, "config"), []byte(config), 0o600); err != nil {
		t.Fatalf("switch admin repository to reftable: %v", err)
	}
	// The loose trusted ref is still on disk; git simply stops seeing it.
	if _, err := os.Stat(adminRefPath(t, harness)); err != nil {
		t.Fatalf("loose trusted ref: %v", err)
	}
	requireGitSeesNoTrustedRef(t, harness, adminPath)
	requireEngineerPushRefused(t, harness, human, base, "reftable-config")
}

// TestAdminPolicyMissingAdminRepositoryFailsClosed pins that a tenant whose
// admin repository is gone has no policy at all. There is no bootstrap file to
// fall back to, so the push must be refused and the ref left unchanged.
func TestAdminPolicyMissingAdminRepositoryFailsClosed(t *testing.T) {
	harness := newAdminHarness(t, nativefix.Options{})
	base := seedWebWithHooks(t, harness)
	human := cloneFor(t, harness, harness.HumanToken, "human-absent-admin")
	commit := harness.Commit(t, human, "missing-admin-refused")
	adminPath := harness.RepositoryPath(nativefix.AdminRepository)
	if err := os.RemoveAll(adminPath); err != nil {
		t.Fatalf("remove admin repository: %v", err)
	}
	stderr, code := harness.Push(t, human, harness.HumanToken, "origin", "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != base {
		t.Fatalf("refused push moved main to %q; want %q (candidate %q)", observed, base, commit)
	}
}

// TestAdminPolicyMissingTrustedRefFailsClosed pins the second missing
// dependency: an admin repository without its trusted ref has no committed
// policy. The push must fail closed rather than read another file or rule.
func TestAdminPolicyMissingTrustedRefFailsClosed(t *testing.T) {
	harness := newAdminHarness(t, nativefix.Options{})
	base := seedWebWithHooks(t, harness)
	human := cloneFor(t, harness, harness.HumanToken, "human-unborn-admin")
	commit := harness.Commit(t, human, "missing-ref-refused")
	adminPath := harness.RepositoryPath(nativefix.AdminRepository)
	if _, stderr, code := harness.Git(t, "--git-dir="+adminPath, "update-ref", "-d", "refs/heads/main"); code != 0 {
		t.Fatalf("delete trusted ref: %s", stderr)
	}
	if observed := harness.RemoteRef(t, nativefix.AdminRepository, "refs/heads/main"); observed != "" {
		t.Fatalf("trusted ref = %q; want absent", observed)
	}
	stderr, code := harness.Push(t, human, harness.HumanToken, "origin", "HEAD:refs/heads/main")
	requireFailure(t, stderr, code)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != base {
		t.Fatalf("refused push moved main to %q; want %q (candidate %q)", observed, base, commit)
	}
}
