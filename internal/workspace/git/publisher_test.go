package git_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/workspace"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

func TestPublishIsCompareAndSwapNeverForce(t *testing.T) {
	f := newFixture(t, nil)
	commit, bundle, _ := f.repo.Bundle(t, f.repo.Initial, "agent", gitfx.File{Path: "a.txt", Content: "agent"})
	p, err := f.prepare(f.request(bundle, commit, f.repo.Initial, f.repo.Initial))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()

	// A human fast-forwards the branch after the proposal was prepared.
	human := f.repo.HumanPush(t, f.repo.BaseRef, "human", gitfx.File{Path: "h.txt", Content: "human"})
	err = f.pub.Publish(context.Background(), f.ref, workspace.PublishRequest{
		Branch: f.repo.BaseRef, Expected: commitOf(f.repo.Initial), Commit: commitOf(commit), Source: p})
	if err == nil {
		t.Fatal("publication overwrote a branch that moved")
	}
	if got := f.repo.Tip(t, f.repo.BaseRef); got != human {
		t.Fatalf("the human commit was lost: tip %s, want %s", got, human)
	}
}

func TestPublisherWithReadCredentialCannotWrite(t *testing.T) {
	f := newFixture(t, nil)
	commit, bundle, _ := f.repo.Bundle(t, f.repo.Initial, "agent", gitfx.File{Path: "a.txt", Content: "agent"})
	p, err := f.prepare(f.request(bundle, commit, f.repo.Initial, f.repo.Initial))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	// A mis-wired publisher that was only given the read token.
	weak := f.withTokens(f.srv.ReadToken, f.srv.ReadToken)
	err = weak.pub.Publish(context.Background(), f.ref, workspace.PublishRequest{
		Branch: f.repo.BaseRef, Expected: commitOf(f.repo.Initial), Commit: commitOf(commit), Source: p})
	if err == nil {
		t.Fatal("the read token published")
	}
	if got := f.repo.Tip(t, f.repo.BaseRef); got != f.repo.Initial {
		t.Fatalf("branch moved to %s", got)
	}
}

func TestPublishRefusesUnpreparedSource(t *testing.T) {
	f := newFixture(t, nil)
	err := f.pub.Publish(context.Background(), f.ref, workspace.PublishRequest{
		Branch: f.repo.BaseRef, Expected: commitOf(f.repo.Initial), Commit: commitOf(f.repo.Initial), Source: nil})
	if !errors.Is(err, workspace.ErrInvalidValue) {
		t.Fatalf("got %v", err)
	}
}

func TestCreateAndDeleteRefSemantics(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	branch := "refs/heads/kodama/work/wi_1"
	later := f.repo.HumanPush(t, f.repo.BaseRef, "second", gitfx.File{Path: "b.txt", Content: "b"})

	if err := f.pub.CreateRef(ctx, f.ref, branch, commitOf(f.repo.Initial), f.repo.BaseRef); err != nil {
		t.Fatal(err)
	}
	if got := f.repo.Tip(t, branch); got != f.repo.Initial {
		t.Fatalf("branch at %s", got)
	}
	// Repeating after a lost acknowledgment adopts the ref created at the same commit.
	if err := f.pub.CreateRef(ctx, f.ref, branch, commitOf(f.repo.Initial), f.repo.BaseRef); err != nil {
		t.Fatalf("idempotent create: %v", err)
	}
	// An independently existing ref elsewhere is never overwritten.
	if err := f.pub.CreateRef(ctx, f.ref, branch, commitOf(later), f.repo.BaseRef); !errors.Is(err, workspace.ErrBranchExists) {
		t.Fatalf("overwrite attempt: %v", err)
	}
	if got := f.repo.Tip(t, branch); got != f.repo.Initial {
		t.Fatalf("branch overwritten: %s", got)
	}
	// A commit not reachable from the named branch cannot be pinned.
	orphan, _ := f.repo.Commit(t, f.repo.Initial, "orphan", gitfx.File{Path: "o", Content: "o"})
	if err := f.pub.CreateRef(ctx, f.ref, "refs/heads/kodama/retained/x", commitOf(orphan), f.repo.BaseRef); err == nil {
		t.Fatal("created a ref to a commit the remote does not hold")
	}

	// Deletion is also expected-old-ref and idempotent.
	if err := f.pub.DeleteRef(ctx, f.ref, branch, commitOf(later)); !errors.Is(err, workspace.ErrStale) {
		t.Fatalf("delete with a wrong expected commit: %v", err)
	}
	if f.repo.Tip(t, branch) == "" {
		t.Fatal("ref deleted despite a mismatched expectation")
	}
	if err := f.pub.DeleteRef(ctx, f.ref, branch, commitOf(f.repo.Initial)); err != nil {
		t.Fatal(err)
	}
	if err := f.pub.DeleteRef(ctx, f.ref, branch, commitOf(f.repo.Initial)); err != nil {
		t.Fatalf("deleting an absent ref: %v", err)
	}
}

func TestReaderBranchContains(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	tip2 := f.repo.HumanPush(t, f.repo.BaseRef, "second", gitfx.File{Path: "b.txt", Content: "b"})
	info, err := f.reader.BranchContains(ctx, f.ref, f.repo.BaseRef, commitOf(f.repo.Initial))
	if err != nil || !info.Exists || !info.Contains || info.Tip != commitOf(tip2) {
		t.Fatalf("history: %+v, %v", info, err)
	}
	orphan, _ := f.repo.Commit(t, f.repo.Initial, "orphan", gitfx.File{Path: "o", Content: "o"})
	info, err = f.reader.BranchContains(ctx, f.ref, f.repo.BaseRef, commitOf(orphan))
	if err != nil || info.Contains {
		t.Fatalf("foreign commit: %+v, %v", info, err)
	}
	info, err = f.reader.BranchContains(ctx, f.ref, "refs/heads/missing", commitOf(f.repo.Initial))
	if err != nil || info.Exists {
		t.Fatalf("missing branch: %+v, %v", info, err)
	}
}
