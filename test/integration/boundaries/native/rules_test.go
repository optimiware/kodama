//go:build integration

package native_test

import (
	"testing"

	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

func TestOrderedRuleEnforcementAndDeleteRefusal(t *testing.T) {
	harness := newHarness(t, nativefix.Options{})
	_, first := seedMain(t, harness)
	human := cloneFor(t, harness, harness.HumanToken, "human-rules")

	// A human engineer may fast-forward an ordinary branch.
	second := harness.Commit(t, human, "second")
	if stderr, code := harness.Push(t, human, harness.HumanToken, "origin", "HEAD:refs/heads/main"); code != 0 {
		t.Fatalf("fast-forward main: %s", stderr)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != second {
		t.Fatalf("remote main = %q; want %q", observed, second)
	}

	// RW does not cover a rewind: force-updating main is refused.
	if _, stderr, code := harness.GitIn(t, human, "reset", "--hard", first); code != 0 {
		t.Fatalf("reset: %s", stderr)
	}
	if stderr, code := harness.Push(t, human, harness.HumanToken, "--force", "origin", "HEAD:refs/heads/main"); code == 0 {
		t.Fatal("a non-fast-forward main update was accepted")
	} else {
		requireRejectionMessage(t, stderr)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/main"); observed != second {
		t.Fatalf("rejected force push moved main to %q; want %q", observed, second)
	}

	// RW+ allows create, fast-forward and rewind on its ref.
	if _, stderr, code := harness.GitIn(t, human, "checkout", "-b", "rewind", first); code != 0 {
		t.Fatalf("checkout rewind: %s", stderr)
	}
	if stderr, code := harness.Push(t, human, harness.HumanToken, "origin", "HEAD:refs/heads/rewind"); code != 0 {
		t.Fatalf("create rewind: %s", stderr)
	}
	third := harness.Commit(t, human, "third")
	if stderr, code := harness.Push(t, human, harness.HumanToken, "origin", "HEAD:refs/heads/rewind"); code != 0 {
		t.Fatalf("fast-forward rewind: %s", stderr)
	}
	if _, stderr, code := harness.GitIn(t, human, "reset", "--hard", first); code != 0 {
		t.Fatalf("reset rewind: %s", stderr)
	}
	if stderr, code := harness.Push(t, human, harness.HumanToken, "--force", "origin", "HEAD:refs/heads/rewind"); code != 0 {
		t.Fatalf("RW+ rewind: %s", stderr)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/rewind"); observed != first {
		t.Fatalf("rewind ref = %q; want %q", observed, first)
	}
	if third == first {
		t.Fatal("fixture commits are identical")
	}

	// First-match-wins: an earlier deny cannot be overridden by a later allow.
	if stderr, code := harness.Push(t, human, harness.HumanToken, "origin", "HEAD:refs/heads/protected"); code == 0 {
		t.Fatal("the protected ref accepted a direct human push")
	} else {
		requireRejectionMessage(t, stderr)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/protected"); observed != "" {
		t.Fatalf("protected ref = %q; want absent", observed)
	}

	// A guarded ref refuses a human direct push even when the ordered rules
	// would allow the permission; approval evidence is execution-only.
	if stderr, code := harness.Push(t, human, harness.HumanToken, "origin", "HEAD:refs/heads/guarded/change"); code == 0 {
		t.Fatal("the approval-guarded ref accepted a direct human push")
	} else {
		requireRejectionMessage(t, stderr)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/guarded/change"); observed != "" {
		t.Fatalf("guarded ref = %q; want absent", observed)
	}

	// Ref deletes are refused with the documented message.
	if stderr, code := harness.Push(t, human, harness.HumanToken, "origin", "--delete", "rewind"); code == 0 {
		t.Fatal("a ref delete was accepted")
	} else {
		requireRejectionMessage(t, stderr)
	}
	if observed := harness.RemoteRef(t, nativefix.Repository, "refs/heads/rewind"); observed != first {
		t.Fatalf("rejected delete changed rewind to %q; want %q", observed, first)
	}
}
