package workspacetest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// GitEnv is a Git remote under contract: the read side and the write side of one
// repository with an initial commit on BaseRef, and the fixture operations a
// test needs to act as the remote's other users.
type GitEnv struct {
	Reader    workspace.RefReader
	Preparer  workspace.SourcePreparer
	Publisher workspace.RefPublisher
	Repo      workspace.RepoRef
	BaseRef   string
	Initial   workspace.Commit
	// Stage creates a commit on top of parent (the current tip of BaseRef) and
	// returns it with a source ready to publish: the verified bundle for the
	// HTTPS adapter, the stored commit for the local one.
	Stage func(t testing.TB, parent workspace.Commit, msg string) (workspace.Commit, workspace.Prepared)
	// HumanPush fast-forwards branch with a new commit made by someone else and
	// returns it.
	HumanPush func(t testing.TB, branch, msg string) workspace.Commit
	// Bundle creates what the entrypoint uploads for a commit on top of base.
	Bundle func(t testing.TB, base workspace.Commit, msg string, files ...File) (workspace.Commit, []byte)
	// Foreign returns a commit the remote does not hold.
	Foreign func(t testing.TB) workspace.Commit
	// Tip reads a ref directly from the remote ("" when absent).
	Tip func(t testing.TB, ref string) workspace.Commit
	// CorruptObject returns a bundle whose commit is correctly hashed but is not
	// a valid Git object: the digest, header and prerequisites are fine, so only
	// the preparer's object integrity checks can refuse it.
	CorruptObject func(t testing.TB) (workspace.Commit, []byte)
	// ScratchLeft counts the scratch the Preparer still holds.
	ScratchLeft func(t testing.TB) int
	// Bounds builds uploads that attack the resource bounds an adapter exposes as
	// configuration. It is optional: an adapter without those knobs (the HTTPS
	// one is bounded by its own options and tests) leaves it nil.
	Bounds *BoundsGit
}

// BoundsGit builds hostile uploads against the environment's own Preparer.
type BoundsGit struct {
	// Expanding returns a bundle far below the transfer limit whose objects
	// expand beyond the Preparer's expanded-size bound.
	Expanding func(t testing.TB) (workspace.Commit, []byte)
	// ScratchBound prepares, with a Preparer whose scratch bound is below what
	// the bundle needs, and returns the scratch left behind and the error.
	ScratchBound func(t testing.TB) (left int, err error)
}

// OpenGit builds a fresh remote.
type OpenGit func(testing.TB) GitEnv

// GitContract is the behavior every workspace.RefReader and RefPublisher adapter
// shares: exact ref resolution, ancestry answers for a branch, publication that
// is an expected-old-ref update and never a force (a moved branch or a diverged
// proposal is refused and nothing is lost), refs created only when absent and
// deleted only against their expectation. It asserts outcomes, not transport.
func GitContract(t *testing.T, open OpenGit) {
	t.Helper()
	ctx := context.Background()

	t.Run("the reader resolves refs and answers ancestry", func(t *testing.T) {
		env := open(t)
		got, found, err := env.Reader.ResolveRef(ctx, env.Repo, env.BaseRef)
		if err != nil || !found || got != env.Initial {
			t.Fatalf("resolve: %q %v %v", got, found, err)
		}
		if _, found, err := env.Reader.ResolveRef(ctx, env.Repo, "refs/heads/missing"); err != nil || found {
			t.Fatalf("a missing ref: found %v, %v", found, err)
		}
		if _, _, err := env.Reader.ResolveRef(ctx, env.Repo, "refs/tags/v1"); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("a ref outside the branch namespace: %v", err)
		}
		if _, _, err := env.Reader.ResolveRef(ctx, env.Repo, "refs/heads/../x"); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("a traversing ref: %v", err)
		}
		tip2 := env.HumanPush(t, env.BaseRef, "second")
		info, err := env.Reader.BranchContains(ctx, env.Repo, env.BaseRef, env.Initial)
		if err != nil || !info.Exists || !info.Contains || info.Tip != tip2 {
			t.Fatalf("history: %+v %v", info, err)
		}
		if info, err = env.Reader.BranchContains(ctx, env.Repo, env.BaseRef, tip2); err != nil || !info.Contains || info.Tip != tip2 {
			t.Fatalf("the tip itself: %+v %v", info, err)
		}
		if info, err = env.Reader.BranchContains(ctx, env.Repo, env.BaseRef, ""); err != nil || !info.Exists || info.Contains || info.Tip != tip2 {
			t.Fatalf("a zero commit only resolves the tip: %+v %v", info, err)
		}
		if info, err = env.Reader.BranchContains(ctx, env.Repo, env.BaseRef, env.Foreign(t)); err != nil || !info.Exists || info.Contains {
			t.Fatalf("a foreign commit: %+v %v", info, err)
		}
		if info, err = env.Reader.BranchContains(ctx, env.Repo, "refs/heads/missing", env.Initial); err != nil || info.Exists || info.Contains {
			t.Fatalf("a missing branch: %+v %v", info, err)
		}
		if _, err := env.Reader.BranchContains(ctx, env.Repo, env.BaseRef, "not-a-commit"); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("a malformed commit: %v", err)
		}
	})

	t.Run("publication is an expected-old-ref update, never a force", func(t *testing.T) {
		env := open(t)
		commit, src := env.Stage(t, env.Initial, "agent")
		defer src.Release()
		if src.ObservedTip() != env.Initial {
			t.Fatalf("observed tip %q, want the initial commit", src.ObservedTip())
		}
		if err := env.Publisher.Publish(ctx, env.Repo, workspace.PublishRequest{Branch: env.BaseRef, Expected: env.Initial, Commit: commit, Source: nil}); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("an unprepared source: %v", err)
		}
		other, otherSrc := env.Stage(t, env.Initial, "other proposal")
		defer otherSrc.Release()
		if err := env.Publisher.Publish(ctx, env.Repo, workspace.PublishRequest{Branch: env.BaseRef, Expected: env.Initial, Commit: commit, Source: otherSrc}); !errors.Is(err, workspace.ErrInvalidValue) {
			t.Fatalf("a source prepared for another commit: %v", err)
		}
		if env.Tip(t, env.BaseRef) != env.Initial {
			t.Fatal("a refused publication moved the branch")
		}
		if err := env.Publisher.Publish(ctx, env.Repo, workspace.PublishRequest{Branch: env.BaseRef, Expected: env.Initial, Commit: commit, Source: src}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if got := env.Tip(t, env.BaseRef); got != commit {
			t.Fatalf("tip %q, want %q", got, commit)
		}
		// The branch moved on: the same expectation can no longer win, and the
		// published commit stays.
		if err := env.Publisher.Publish(ctx, env.Repo, workspace.PublishRequest{Branch: env.BaseRef, Expected: env.Initial, Commit: other, Source: otherSrc}); err == nil {
			t.Fatal("a publication overwrote a branch that moved")
		}
		if got := env.Tip(t, env.BaseRef); got != commit {
			t.Fatalf("the published commit was lost: tip %q", got)
		}
	})

	t.Run("a human push that wins the race is never overwritten", func(t *testing.T) {
		env := open(t)
		commit, src := env.Stage(t, env.Initial, "agent")
		defer src.Release()
		human := env.HumanPush(t, env.BaseRef, "human")
		err := env.Publisher.Publish(ctx, env.Repo, workspace.PublishRequest{Branch: env.BaseRef, Expected: env.Initial, Commit: commit, Source: src})
		if err == nil {
			t.Fatal("publication overwrote a branch that moved")
		}
		if got := env.Tip(t, env.BaseRef); got != human {
			t.Fatalf("the human commit was lost: tip %q, want %q", got, human)
		}
		// Expecting the new tip does not help a proposal that does not descend
		// from it: the history is never merged or rewound.
		err = env.Publisher.Publish(ctx, env.Repo, workspace.PublishRequest{Branch: env.BaseRef, Expected: human, Commit: commit, Source: src})
		var conflict *workspace.ConflictError
		if !errors.Is(err, workspace.ErrPublicationConflict) || !errors.As(err, &conflict) || conflict.Reason != "not_fast_forward" {
			t.Fatalf("a diverged proposal: %v", err)
		}
		if got := env.Tip(t, env.BaseRef); got != human {
			t.Fatalf("tip %q after a diverged proposal", got)
		}
	})

	t.Run("refs are created only when absent and deleted only against their expectation", func(t *testing.T) {
		env := open(t)
		branch := "refs/heads/kodama/work/wi_1"
		later := env.HumanPush(t, env.BaseRef, "second")

		if err := env.Publisher.CreateRef(ctx, env.Repo, branch, env.Initial, env.BaseRef); err != nil {
			t.Fatalf("create: %v", err)
		}
		if got := env.Tip(t, branch); got != env.Initial {
			t.Fatalf("branch at %q", got)
		}
		if err := env.Publisher.CreateRef(ctx, env.Repo, branch, env.Initial, env.BaseRef); err != nil {
			t.Fatalf("an identical create must be adopted: %v", err)
		}
		if err := env.Publisher.CreateRef(ctx, env.Repo, branch, later, env.BaseRef); !errors.Is(err, workspace.ErrBranchExists) {
			t.Fatalf("overwriting an existing ref: %v", err)
		}
		if got := env.Tip(t, branch); got != env.Initial {
			t.Fatalf("the ref was overwritten: %q", got)
		}
		if err := env.Publisher.CreateRef(ctx, env.Repo, "refs/heads/kodama/retained/x", env.Foreign(t), env.BaseRef); err == nil {
			t.Fatal("created a ref to a commit the remote does not hold")
		}
		if err := env.Publisher.CreateRef(ctx, env.Repo, "refs/heads/kodama/retained/y", env.Initial, "refs/heads/missing"); err == nil {
			t.Fatal("created a ref from a branch that does not exist")
		}
		if got := env.Tip(t, "refs/heads/kodama/retained/x"); got != "" {
			t.Fatalf("a refused create left a ref: %q", got)
		}

		if err := env.Publisher.DeleteRef(ctx, env.Repo, branch, later); !errors.Is(err, workspace.ErrStale) {
			t.Fatalf("delete with a wrong expectation: %v", err)
		}
		if env.Tip(t, branch) == "" {
			t.Fatal("the ref was deleted despite a mismatched expectation")
		}
		if err := env.Publisher.DeleteRef(ctx, env.Repo, branch, env.Initial); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if got := env.Tip(t, branch); got != "" {
			t.Fatalf("the ref survived its deletion: %q", got)
		}
		if err := env.Publisher.DeleteRef(ctx, env.Repo, branch, env.Initial); err != nil {
			t.Fatalf("deleting an absent ref: %v", err)
		}
		// The branch the work was based on is untouched by all of it.
		if got := env.Tip(t, env.BaseRef); got != later {
			t.Fatalf("base tip %q, want %q", got, later)
		}
	})

	t.Run("a bundle is verified before it is staged", func(t *testing.T) {
		env := open(t)
		prep := func(mutate func(*workspace.PrepareRequest)) (workspace.Prepared, error) {
			req := workspace.PrepareRequest{Repo: env.Repo, Branch: env.BaseRef, Input: env.Initial, ExpectedTip: env.Initial}
			mutate(&req)
			return env.Preparer.Prepare(ctx, req)
		}
		reason := func(err error) string {
			var be *workspace.BundleError
			if !errors.As(err, &be) {
				return ""
			}
			return be.Reason
		}
		commit, bundle := env.Bundle(t, env.Initial, "agent", File{Path: "agent.txt", Content: "the agent's work"})
		withBundle := func(r *workspace.PrepareRequest, b []byte) {
			r.Commit, r.Body, r.Digest, r.Size = commit, bytes.NewReader(b), Digest(b), int64(len(b))
		}
		good := func(r *workspace.PrepareRequest) { withBundle(r, bundle) }
		p, err := prep(good)
		if err != nil {
			t.Fatalf("a valid bundle: %v", err)
		}
		if p.ObservedTip() != env.Initial {
			t.Fatalf("observed tip %q", p.ObservedTip())
		}
		if ok, err := p.DescendsFrom(ctx, env.Initial); err != nil || !ok {
			t.Fatalf("descends from the input: %v %v", ok, err)
		}
		if ok, err := p.DescendsFrom(ctx, env.Foreign(t)); err != nil || ok {
			t.Fatalf("descends from a foreign commit: %v %v", ok, err)
		}
		p.Release()

		if _, err := prep(func(r *workspace.PrepareRequest) { good(r); r.Digest = Digest([]byte("other")) }); reason(err) != workspace.ReasonDigestMismatch {
			t.Errorf("a wrong digest: %v", err)
		}
		if _, err := prep(func(r *workspace.PrepareRequest) { good(r); r.Size = int64(len(bundle)) + 10 }); reason(err) != workspace.ReasonTruncated {
			t.Errorf("fewer bytes than declared: %v", err)
		}
		if _, err := prep(func(r *workspace.PrepareRequest) { good(r); r.Commit = env.Initial }); reason(err) != workspace.ReasonCommitMismatch {
			t.Errorf("a bundle for another commit: %v", err)
		}
		if _, err := prep(func(r *workspace.PrepareRequest) { withBundle(r, []byte("this is not a git bundle at all\n")) }); reason(err) == "" {
			t.Errorf("a file that is not a bundle: %v", err)
		}
		if _, err := prep(func(r *workspace.PrepareRequest) { withBundle(r, bundle[:len(bundle)/2]) }); reason(err) == "" {
			t.Errorf("a cut bundle: %v", err)
		}
		if _, err := prep(func(r *workspace.PrepareRequest) { good(r); r.Needles = [][]byte{[]byte("the agent's work")} }); !errors.Is(err, workspace.ErrSecretInSource) {
			t.Errorf("an injected secret in the new objects: %v", err)
		}
		if p, err := prep(func(r *workspace.PrepareRequest) { good(r); r.Needles = [][]byte{[]byte("short")} }); err != nil {
			t.Errorf("a needle below the scan floor must be ignored: %v", err)
		} else {
			p.Release()
		}
		// A bundle that does not descend from the pinned input.
		human := env.HumanPush(t, env.BaseRef, "human")
		if _, err := prep(func(r *workspace.PrepareRequest) { good(r); r.Input = human }); reason(err) != workspace.ReasonBadPrerequisite && reason(err) != workspace.ReasonNotDescendant {
			t.Errorf("a bundle that does not descend from the input: %v", err)
		}
		// A refused bundle moved nothing.
		if got := env.Tip(t, env.BaseRef); got != human {
			t.Fatalf("tip %q", got)
		}
	})

	prepareRaw := func(env GitEnv, commit workspace.Commit, b []byte, needles ...[]byte) error {
		p, err := env.Preparer.Prepare(context.Background(), workspace.PrepareRequest{Repo: env.Repo, Branch: env.BaseRef, Input: env.Initial, ExpectedTip: env.Initial,
			Commit: commit, Body: bytes.NewReader(b), Digest: Digest(b), Size: int64(len(b)), Needles: needles})
		if err == nil {
			p.Release()
		}
		return err
	}

	t.Run("a correctly hashed but malformed object is refused and leaves nothing", func(t *testing.T) {
		env := open(t)
		commit, corrupt := env.CorruptObject(t)
		var be *workspace.BundleError
		if err := prepareRaw(env, commit, corrupt); !errors.As(err, &be) || be.Reason != workspace.ReasonCorruptObjects {
			t.Fatalf("a correctly hashed but invalid object: %v", err)
		}
		if err := prepareRaw(env, commit, corrupt, []byte("a secret to scan for")); !errors.As(err, &be) || be.Reason != workspace.ReasonCorruptObjects {
			t.Fatalf("a malformed object in a bundle scanned for secrets: %v", err)
		}
		if left := env.ScratchLeft(t); left != 0 {
			t.Fatalf("%d scratch directories left after the malformed object", left)
		}
		if got := env.Tip(t, env.BaseRef); got != env.Initial {
			t.Fatalf("tip %q", got)
		}
		// The adapter still accepts a good bundle afterwards.
		good, bundle := env.Bundle(t, env.Initial, "fine", File{Path: "fine.txt", Content: "fine"})
		if err := prepareRaw(env, good, bundle); err != nil {
			t.Fatalf("a valid bundle after the refusal: %v", err)
		}
	})

	t.Run("an expanding or oversized bundle is refused within the preparer's bounds and leaves nothing", func(t *testing.T) {
		env := open(t)
		h := env.Bounds
		if h == nil {
			t.Skip("this adapter's object and scratch bounds are covered by its own tests")
		}
		// A bundle that compresses far below the transfer limit but expands past
		// the bound is refused from the object sizes, with or without secrets to scan.
		commit, zeros := h.Expanding(t)
		if err := prepareRaw(env, commit, zeros); !errors.Is(err, workspace.ErrLimitExceeded) {
			t.Fatalf("an expanding bundle: %v", err)
		}
		if err := prepareRaw(env, commit, zeros, []byte("a secret that is nowhere in the objects")); !errors.Is(err, workspace.ErrLimitExceeded) {
			t.Fatalf("an expanding bundle scanned for secrets: %v", err)
		}
		if left := env.ScratchLeft(t); left != 0 {
			t.Fatalf("%d scratch directories left after the expanding bundle", left)
		}
		// Importing cannot outgrow the scratch bound either.
		left, err := h.ScratchBound(t)
		if !errors.Is(err, workspace.ErrLimitExceeded) {
			t.Fatalf("a bundle over the scratch bound: %v", err)
		}
		if left != 0 {
			t.Fatalf("%d scratch directories left after the scratch overflow", left)
		}
		if got := env.Tip(t, env.BaseRef); got != env.Initial {
			t.Fatalf("tip %q", got)
		}
	})

	t.Run("a reader cannot write", func(t *testing.T) {
		env := open(t)
		if _, ok := env.Reader.(workspace.RefPublisher); ok {
			t.Fatal("the read side also implements the write side")
		}
	})
}

// OpenLocalGit is the OpenGit of the temporary bare repositories: no server, no
// credentials, only the git executable.
func OpenLocalGit(t testing.TB) GitEnv {
	t.Helper()
	g := LocalGitFor(t)
	g.SetMaxExpandedBytes(1 << 20)
	repo := g.NewRepo(t, "repo")
	ref := repo.Ref(ScopeA)
	return GitEnv{
		CorruptObject: func(t testing.TB) (workspace.Commit, []byte) {
			t.Helper()
			c, b, err := repo.CorruptBundle(context.Background(), repo.Initial)
			if err != nil {
				t.Fatal(err)
			}
			return c, b
		},
		ScratchLeft: func(testing.TB) int { return g.ScratchEntries() },
		Bounds: &BoundsGit{
			Expanding: func(t testing.TB) (workspace.Commit, []byte) {
				t.Helper()
				c, b, err := repo.CompressibleBundle(context.Background(), repo.Initial, 8<<20)
				if err != nil {
					t.Fatal(err)
				}
				if len(b) > 256<<10 {
					t.Fatalf("the bundle does not compress: %d bytes", len(b))
				}
				return c, b
			},
			ScratchBound: func(t testing.TB) (int, error) {
				t.Helper()
				tight := LocalGitFor(t)
				r := tight.NewRepo(t, "tight")
				random := make([]byte, 1<<20)
				if _, err := rand.Read(random); err != nil {
					t.Fatal(err)
				}
				c, b := r.BundleT(t, r.Initial, "random", File{Path: "random.bin", Content: string(random)})
				// The bundle fits; the bundle plus the objects Git imports from it do not.
				tight.SetMaxScratchBytes(int64(len(b)) + 256<<10)
				p, err := tight.Preparer().Prepare(context.Background(), workspace.PrepareRequest{Repo: r.Ref(ScopeA), Branch: r.BaseRef, Input: r.Initial, ExpectedTip: r.Initial,
					Commit: c, Body: bytes.NewReader(b), Digest: Digest(b), Size: int64(len(b))})
				if err == nil {
					p.Release()
				}
				return tight.ScratchEntries(), err
			},
		},
		Reader: g.Reader(), Preparer: g.Preparer(), Publisher: g.Publisher(), Repo: ref, BaseRef: repo.BaseRef, Initial: repo.Initial,
		Bundle: func(t testing.TB, base workspace.Commit, msg string, files ...File) (workspace.Commit, []byte) {
			t.Helper()
			return repo.BundleT(t, base, msg, files...)
		},
		Stage: func(t testing.TB, parent workspace.Commit, msg string) (workspace.Commit, workspace.Prepared) {
			t.Helper()
			commit, data := repo.BundleT(t, parent, msg, File{Path: msg + ".txt", Content: msg})
			p, err := g.Preparer().Prepare(context.Background(), workspace.PrepareRequest{Repo: ref, Branch: repo.BaseRef, Body: bytes.NewReader(data),
				Digest: Digest(data), Size: int64(len(data)), Input: parent, ExpectedTip: parent, Commit: commit})
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			return commit, p
		},
		HumanPush: func(t testing.TB, branch, msg string) workspace.Commit {
			t.Helper()
			return repo.HumanPush(t, branch, msg, File{Path: msg + ".txt", Content: msg})
		},
		Foreign: repo.Foreign,
		Tip:     repo.Tip,
	}
}

// OpenHTTPSGit is the OpenGit of the HTTPS adapter against the fixture Git
// server with distinct read and write tokens. It skips without the fixture, and
// always in the unit layer.
func OpenHTTPSGit(t testing.TB) GitEnv {
	t.Helper()
	srv := gitfx.NewServer(t)
	repo := srv.NewRepo(t, "t088-repo")
	scratchRoot := filepath.Join(t.TempDir(), "scratch")
	opts := gitad.Options{CAFile: srv.CAFile, CommandTimeout: 30 * time.Second, MaxBundleBytes: 4 << 20, MaxScratchBytes: 32 << 20,
		ScratchRoot: scratchRoot}
	creds := &credentials{byRef: map[string]string{CheckoutRef: srv.ReadToken, PublisherRef: srv.WriteToken}}
	reader, err := gitad.NewReader(opts, creds)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := gitad.NewPublisher(opts, creds)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reader.Close)
	t.Cleanup(publisher.Close)
	ref := workspace.RepoRef{Scope: ScopeA, URL: repo.URL, CheckoutSecretRef: CheckoutRef, PublisherSecretRef: PublisherRef}
	return GitEnv{
		Reader: reader, Preparer: reader, Publisher: publisher, Repo: ref, BaseRef: repo.BaseRef, Initial: workspace.Commit(repo.Initial),
		Bundle: func(t testing.TB, base workspace.Commit, msg string, files ...File) (workspace.Commit, []byte) {
			t.Helper()
			commit, data, _ := repo.Bundle(t, string(base), msg, files...)
			return workspace.Commit(commit), data
		},
		Stage: func(t testing.TB, parent workspace.Commit, msg string) (workspace.Commit, workspace.Prepared) {
			t.Helper()
			commit, bundle, _ := repo.Bundle(t, string(parent), msg, gitfx.File{Path: msg + ".txt", Content: msg})
			sum := sha256.Sum256(bundle)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			p, err := reader.Prepare(ctx, workspace.PrepareRequest{Repo: ref, Branch: repo.BaseRef, Body: bytes.NewReader(bundle),
				Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(bundle)), Input: parent, ExpectedTip: parent, Commit: workspace.Commit(commit)})
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			return workspace.Commit(commit), p
		},
		HumanPush: func(t testing.TB, branch, msg string) workspace.Commit {
			t.Helper()
			return workspace.Commit(repo.HumanPush(t, branch, msg, gitfx.File{Path: msg + ".txt", Content: msg}))
		},
		Foreign: func(t testing.TB) workspace.Commit {
			t.Helper()
			c, _ := repo.Commit(t, repo.Initial, "foreign", gitfx.File{Path: "foreign.txt", Content: "foreign"})
			return workspace.Commit(c)
		},
		Tip: func(t testing.TB, r string) workspace.Commit { return workspace.Commit(repo.Tip(t, r)) },
		CorruptObject: func(t testing.TB) (workspace.Commit, []byte) {
			t.Helper()
			// A working clone that holds the base, to build the malformed commit in.
			_, work := repo.Commit(t, repo.Initial, "scratch", gitfx.File{Path: "scratch.txt", Content: "scratch"})
			c, b, err := CorruptBundleIn(context.Background(), work, workspace.Commit(repo.Initial))
			if err != nil {
				t.Fatal(err)
			}
			return c, b
		},
		// Each operation's private scratch is an "op-" directory under the root;
		// the engine's own home directory is not scratch.
		ScratchLeft: func(testing.TB) int {
			entries, _ := os.ReadDir(scratchRoot)
			n := 0
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "op-") {
					n++
				}
			}
			return n
		},
	}
}
