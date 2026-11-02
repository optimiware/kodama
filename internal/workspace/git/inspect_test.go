package git_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/workspace"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// The server-side check of an applied push reads the host with the read
// credential: it relates the pushed commit to the accepted one and scans every
// object the push introduced for the values the attempt must not publish.
func TestInspectRelatesAndScansPushedCommits(t *testing.T) {
	f := newFixture(t, nil)
	branch, base := f.repo.BaseRef, f.repo.Initial
	const secret = "super-secret-token-value"
	clean := f.repo.HumanPush(t, branch, "clean", gitfx.File{Path: "a.txt", Content: "fine"})
	leak := f.repo.HumanPush(t, branch, "leak", gitfx.File{Path: "env.txt", Content: "TOKEN=" + secret})
	fixed := f.repo.HumanPush(t, branch, "remove it again", gitfx.File{Path: "env.txt", Content: "gone"})

	inspect := func(ref, commit, since string, needles ...string) (workspace.Relation, error) {
		t.Helper()
		var ns [][]byte
		for _, n := range needles {
			ns = append(ns, []byte(n))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return f.reader.Inspect(ctx, workspace.InspectRequest{Repo: f.ref, Ref: ref, Commit: commitOf(commit), Since: commitOf(since), Needles: ns})
	}

	t.Run("a clean fast-forward descends", func(t *testing.T) {
		if rel, err := inspect(branch, clean, base, secret); err != nil || rel != workspace.RelationDescends {
			t.Fatalf("%q %v", rel, err)
		}
		if rel, err := inspect(branch, fixed, leak, secret); err != nil || rel != workspace.RelationDescends {
			t.Fatalf("a push after the leaking commit was accepted: %q %v", rel, err)
		}
	})

	t.Run("a value in any introduced commit is a finding although the tip is clean", func(t *testing.T) {
		if _, err := inspect(branch, fixed, base, secret); !errors.Is(err, workspace.ErrSecretInSource) {
			t.Fatalf("got %v", err)
		}
		if _, err := inspect(branch, leak, clean, secret); !errors.Is(err, workspace.ErrSecretInSource) {
			t.Fatalf("got %v", err)
		}
		// The branch moved on after the push: the pushed commit is still found in its history.
		if _, err := inspect(branch, leak, base, secret); !errors.Is(err, workspace.ErrSecretInSource) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("values too short to scan for are ignored", func(t *testing.T) {
		if rel, err := inspect(branch, leak, base, "TOKEN"); err != nil || rel != workspace.RelationDescends {
			t.Fatalf("%q %v", rel, err)
		}
	})

	t.Run("an earlier push is contained and never moves the accepted commit back", func(t *testing.T) {
		for _, c := range []string{clean, leak, fixed} {
			if rel, err := inspect(branch, c, fixed, secret); err != nil || rel != workspace.RelationContained {
				t.Fatalf("%s: %q %v", c, rel, err)
			}
		}
	})

	t.Run("an unrelated or vanished accepted commit is a divergence", func(t *testing.T) {
		f.repo.SetRef(t, "refs/heads/side", base)
		side := f.repo.HumanPush(t, "refs/heads/side", "side work", gitfx.File{Path: "s.txt", Content: "s"})
		if rel, err := inspect(branch, fixed, side); err != nil || rel != workspace.RelationDiverged {
			t.Fatalf("%q %v", rel, err)
		}
		if rel, err := inspect(branch, fixed, strings.Repeat("e", 40)); err != nil || rel != workspace.RelationDiverged {
			t.Fatalf("an accepted commit that is not on the host: %q %v", rel, err)
		}
	})

	t.Run("evidence that cannot be read is unavailable, not a finding or a verdict", func(t *testing.T) {
		if _, err := inspect(branch, strings.Repeat("e", 40), base, secret); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("a pushed commit that is not on the host: %v", err)
		}
		if _, err := inspect("refs/heads/kodama/work/missing", clean, base, secret); !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("a branch that does not exist: %v", err)
		}
		if _, err := inspect("refs/tags/v1", clean, base); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("a tag: %v", err)
		}
		if _, err := inspect(branch, clean, strings.Repeat("e", 64)); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("mixed object formats: %v", err)
		}
	})

	t.Run("only the read credential is used and no scratch remains", func(t *testing.T) {
		if _, err := inspect(branch, clean, base, secret); err != nil {
			t.Fatal(err)
		}
		// A read-only credential suffices: the adapter holds no publisher credential.
		weak := f.withTokens(f.srv.ReadToken, "no-such-write-token")
		ctx := context.Background()
		if rel, err := weak.reader.Inspect(ctx, workspace.InspectRequest{Repo: weak.ref, Ref: branch, Commit: commitOf(clean), Since: commitOf(base)}); err != nil || rel != workspace.RelationDescends {
			t.Fatalf("with the read token only: %q %v", rel, err)
		}
		// A credential that cannot read is unavailable evidence.
		bad := f.withTokens("wrong-token", f.srv.WriteToken)
		if _, err := bad.reader.Inspect(ctx, workspace.InspectRequest{Repo: bad.ref, Ref: branch, Commit: commitOf(clean), Since: commitOf(base)}); err == nil {
			t.Fatal("a wrong credential read the host")
		}
		left, err := readDir(f.scratch)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range left {
			if strings.HasPrefix(name, "op-") {
				t.Fatalf("scratch left behind: %v", left)
			}
		}
	})
}
