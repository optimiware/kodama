package git_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	git "github.com/optimiware/kodama/internal/userland/git"
)

func TestHookPreReceiveRejectsMissingOrMalformedSourceEvent(t *testing.T) {
	cases := map[string]func(map[string]string){
		"missing":   func(environment map[string]string) { delete(environment, git.EnvSourceEvent) },
		"malformed": func(environment map[string]string) { environment[git.EnvSourceEvent] = "not a journal id" },
		"oversized": func(environment map[string]string) { environment[git.EnvSourceEvent] = strings.Repeat("a", 129) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newHookFixture(t)
			mutate(f.env)
			stdin := zeroObjectID + " " + hookA + " refs/heads/work/a\n"
			result, err := f.hook.RunPreReceive(context.Background(), strings.NewReader(stdin))
			if err == nil {
				t.Fatal("batch without a push identity was accepted")
			}
			if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
				t.Fatalf("result = %+v; want reject/1", result)
			}
			result, err = f.hook.RunUpdate(context.Background(), testRef, hookBase, hookCandidate)
			if err == nil {
				t.Fatal("update without a push identity was accepted")
			}
			if result.Decision != git.DecisionDeny || result.ExitCode != 1 {
				t.Fatalf("result = %+v; want reject/1", result)
			}
			if calls := len(f.verifier.verifyRequests()); calls != 0 {
				t.Fatalf("verification calls = %d; want 0", calls)
			}
			if f.executor.updateRefCount() != 0 {
				t.Fatal("a missing push identity reached the executor")
			}
		})
	}
}

func TestHookIdenticalPushWithFreshSourceEventVerifiesAfresh(t *testing.T) {
	f := newHookFixture(t)
	f.verifier.script(testCredential, executionScript("exe-1"))
	f.executor.setRef("shop/web", testRef, hookBase)
	f.executor.setAncestor("shop/web", hookBase, hookCandidate, true)

	first, err := f.hook.RunUpdate(context.Background(), testRef, hookBase, hookCandidate)
	if err != nil || !first.Accepted() {
		t.Fatalf("first update = %+v, %v; want accept", first, err)
	}
	if calls := len(f.verifier.verifyRequests()); calls != 1 {
		t.Fatalf("first verification calls = %d; want 1", calls)
	}

	// Reset the ref and revoke the credential. A later identical push is a new
	// push with a fresh per-request identity, so it must verify afresh instead
	// of settling the earlier reported entry.
	f.executor.setRef("shop/web", testRef, hookBase)
	f.verifier.fail(errors.New("credential revoked"))
	f.env[git.EnvSourceEvent] = "push-2"

	second, err := f.hook.RunUpdate(context.Background(), testRef, hookBase, hookCandidate)
	if err == nil {
		t.Fatal("second identical push was accepted from the stale reported entry")
	}
	if second.Decision != git.DecisionDeny || second.ExitCode != 1 {
		t.Fatalf("second update = %+v; want reject/1", second)
	}
	if calls := len(f.verifier.verifyRequests()); calls != 2 {
		t.Fatalf("verification calls = %d; want the second push verified afresh", calls)
	}
	if got := f.executor.commit("shop/web", testRef); got != hookBase {
		t.Fatalf("ref = %q; want the reset base", got)
	}
}
