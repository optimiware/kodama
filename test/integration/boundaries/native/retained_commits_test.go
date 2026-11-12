//go:build integration

package native_test

// Retained-commit keep refs over real Git. A real RW+ rewind leaves the
// discarded commit unreachable; a real `git gc` then prunes it unless the
// service holds a keep ref named by the retained set. The test uses the real
// smart-HTTP service, the real installed hooks and the real host Git binary;
// only the retained-commit set is supplied by the test, standing in for the
// Shell's retention store.

import (
	"context"
	"strings"
	"testing"

	git "github.com/optimiware/kodama/internal/userland/git"
	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

func TestRetainedCommitSurvivesGCAfterRewindAndIsReleased(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	directory, initial := seedMain(t, harness)
	repository := harness.RepositoryPath(nativefix.Repository)

	// Build one commit on a rewinding branch and push it, then commit a second
	// one and push again. `RW+ refs/heads/rewind = @engineer` permits the
	// rewind the engineer performs below.
	if _, stderr, code := harness.GitIn(t, directory, "checkout", "-b", "rewind"); code != 0 {
		t.Fatalf("checkout rewind: %s", stderr)
	}
	retained := harness.Commit(t, directory, "retained-candidate")
	stderr, code := harness.Push(t, directory, harness.HumanToken, "origin", "HEAD:refs/heads/rewind")
	requireSuccess(t, stderr, code)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/rewind"); observed != retained {
		t.Fatalf("rewind = %q; want %q", observed, retained)
	}
	discarded := harness.Commit(t, directory, "discarded-candidate")
	stderr, code = harness.Push(t, directory, harness.HumanToken, "origin", "HEAD:refs/heads/rewind")
	requireSuccess(t, stderr, code)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/rewind"); observed != discarded {
		t.Fatalf("rewind = %q; want %q", observed, discarded)
	}

	// Rewind the real branch with RW+. The discarded commit is now
	// unreachable from every branch; its journal entry still names it.
	if _, stderr, code := harness.GitIn(t, directory, "reset", "--hard", retained); code != 0 {
		t.Fatalf("reset: %s", stderr)
	}
	stderr, code = harness.Push(t, directory, harness.HumanToken, "origin", "--force", "HEAD:refs/heads/rewind")
	requireSuccess(t, stderr, code)
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/rewind"); observed != retained {
		t.Fatalf("rewound ref = %q; want %q", observed, retained)
	}

	// A second, never-pushed commit is the control: nothing names it, so gc
	// must prune it.
	tree, stderr, code := harness.Git(t, "-C", repository, "rev-parse", "refs/heads/main^{tree}")
	if code != 0 {
		t.Fatalf("resolve tree: %s", stderr)
	}
	unreferenced, stderr, code := harness.Git(t, "-C", repository,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit-tree", strings.TrimSpace(tree), "-m", "unreferenced")
	if code != 0 {
		t.Fatalf("commit-tree: %s", stderr)
	}
	unreferenced = strings.TrimSpace(unreferenced)

	service := harness.Server.Service()
	first, err := service.KeepRefs(context.Background(), nativefix.Repository, []string{discarded})
	if err != nil {
		t.Fatalf("KeepRefs: %v", err)
	}
	if len(first.Kept) != 1 || first.Kept[0] != discarded {
		t.Fatalf("kept = %v; want [%s]", first.Kept, discarded)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, git.KeepRefPrefix+discarded); observed != discarded {
		t.Fatalf("keep ref = %q; want %q", observed, discarded)
	}

	// The managed namespace is retention bookkeeping: the smart-HTTP ref
	// advertisement hides it in both directions while served branches remain
	// visible.
	advertised, code := harness.LSRemote(t, harness.HumanToken, nativefix.Repository)
	if code != 0 {
		t.Fatalf("ls-remote exited %d", code)
	}
	if strings.Contains(advertised, git.KeepRefNamespace) {
		t.Fatalf("smart HTTP advertised the managed namespace:\n%s", advertised)
	}
	if !strings.Contains(advertised, "refs/heads/rewind") {
		t.Fatalf("smart HTTP did not advertise the served branch:\n%s", advertised)
	}

	// The sweep is idempotent: the same set leaves the ref exactly as it is.
	second, err := service.KeepRefs(context.Background(), nativefix.Repository, []string{discarded})
	if err != nil {
		t.Fatalf("second KeepRefs: %v", err)
	}
	if len(second.Released) != 0 || len(second.Kept) != 1 {
		t.Fatalf("second sweep = %+v; want the one retained commit kept and nothing released", second)
	}

	// Invalid and absent names are safe: they are reported, never written.
	safe, err := service.KeepRefs(context.Background(), nativefix.Repository, []string{
		discarded, "not-a-commit", strings.Repeat("f", 40),
	})
	if err != nil {
		t.Fatalf("KeepRefs with invalid/missing names: %v", err)
	}
	if len(safe.Invalid) != 1 || len(safe.Missing) != 1 {
		t.Fatalf("invalid/missing = %v/%v; want one of each", safe.Invalid, safe.Missing)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, git.KeepRefPrefix+strings.Repeat("f", 40)); observed != "" {
		t.Fatalf("missing commit got keep ref %q", observed)
	}

	gcRepository(t, harness, repository)
	if code := catFile(t, harness, repository, discarded); code != 0 {
		t.Fatalf("retained commit %s was pruned despite its keep ref", discarded)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, git.KeepRefPrefix+discarded); observed != discarded {
		t.Fatalf("keep ref after gc = %q; want %q", observed, discarded)
	}
	if code := catFile(t, harness, repository, unreferenced); code == 0 {
		t.Fatalf("unreferenced commit %s survived gc; the control proves gc pruned", unreferenced)
	}

	// Releasing the retention removes the keep ref, and the next gc prunes the
	// commit that no retained execution names any more.
	released, err := service.KeepRefs(context.Background(), nativefix.Repository, nil)
	if err != nil {
		t.Fatalf("release KeepRefs: %v", err)
	}
	if len(released.Released) != 1 || released.Released[0] != discarded {
		t.Fatalf("released = %v; want [%s]", released.Released, discarded)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, git.KeepRefPrefix+discarded); observed != "" {
		t.Fatalf("keep ref after release = %q; want it removed", observed)
	}
	gcRepository(t, harness, repository)
	if code := catFile(t, harness, repository, discarded); code == 0 {
		t.Fatalf("released commit %s survived gc; want it pruned", discarded)
	}

	// The seed commit is still reachable through main.
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != initial {
		t.Fatalf("main = %q; want %q", observed, initial)
	}
}

// gcRepository expires every reflog and prunes immediately, so a commit kept
// only by an implicit reflog entry cannot mask a missing keep ref.
func gcRepository(t *testing.T, harness *nativefix.Harness, repository string) {
	t.Helper()
	if _, stderr, code := harness.Git(t, "-C", repository, "reflog", "expire", "--expire=now", "--all"); code != 0 {
		t.Fatalf("reflog expire: %s", stderr)
	}
	if _, stderr, code := harness.Git(t, "-C", repository, "gc", "--prune=now", "--quiet"); code != 0 {
		t.Fatalf("gc: %s", stderr)
	}
}

// catFile returns the native cat-file exit code: 0 when the commit exists, 1
// when it is absent.
func catFile(t *testing.T, harness *nativefix.Harness, repository, commit string) int {
	t.Helper()
	_, _, code := harness.Git(t, "-C", repository, "cat-file", "-e", commit+"^{commit}")
	return code
}
