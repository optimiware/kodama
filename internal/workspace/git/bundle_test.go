package git_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// Test_bundle_validation_and_bounds covers WS-05, WS-06, WS-09 and WS-10 at the
// Git adapter: a bundle is accepted only when its size, digest, header, objects
// and ancestry check out, no injected secret enters source, and every transfer,
// scratch and time bound fails safely. Real Git over HTTPS.
func Test_bundle_validation_and_bounds(t *testing.T) {
	t.Run("accepts an entrypoint bundle and publishes with expected-old-ref CAS", func(t *testing.T) {
		f := newFixture(t, nil)
		commit, bundle, _ := f.repo.Bundle(t, f.repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "one"})
		p, err := f.prepare(f.request(bundle, commit, f.repo.Initial, f.repo.Initial))
		if err != nil {
			t.Fatal(err)
		}
		defer p.Release()
		if p.ObservedTip() != commitOf(f.repo.Initial) {
			t.Fatalf("observed tip %s", p.ObservedTip())
		}
		if ff, err := p.DescendsFrom(context.Background(), commitOf(f.repo.Initial)); err != nil || !ff {
			t.Fatalf("descends = %v, %v", ff, err)
		}
		if err := f.pub.Publish(context.Background(), f.ref, workspace.PublishRequest{Branch: f.repo.BaseRef, Expected: commitOf(f.repo.Initial), Commit: commitOf(commit), Source: p}); err != nil {
			t.Fatal(err)
		}
		if got := f.repo.Tip(t, f.repo.BaseRef); got != commit {
			t.Fatalf("tip = %s, want %s", got, commit)
		}
		// Read credentials never reached receive-pack; write credentials never
		// fetched a bundle: the roles are separated on the wire.
		if n := f.srv.CountRequests(func(r gitfx.Request) bool { return r.Service == "git-receive-pack" && r.Role != gitfx.RoleWrite }); n != 0 {
			t.Fatalf("%d receive-pack requests without the write token", n)
		}
	})

	t.Run("digest mismatch and truncation are rejected", func(t *testing.T) {
		f := newFixture(t, nil)
		commit, bundle, _ := f.repo.Bundle(t, f.repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "one"})

		flipped := append([]byte(nil), bundle...)
		flipped[len(flipped)-3] ^= 0xff
		req := f.request(bundle, commit, f.repo.Initial, f.repo.Initial)
		req.Body = bytes.NewReader(flipped)
		_, err := f.prepare(req)
		if got := bundleReason(t, err); got != workspace.ReasonDigestMismatch {
			t.Fatalf("flipped byte: %s", got)
		}

		req = f.request(bundle, commit, f.repo.Initial, f.repo.Initial)
		req.Body = bytes.NewReader(bundle[:len(bundle)/2])
		_, err = f.prepare(req)
		if got := bundleReason(t, err); got != workspace.ReasonTruncated {
			t.Fatalf("truncated against its declared size: %s", got)
		}

		// A truncated artifact whose size and digest were recorded consistently
		// is still caught by Git's own verification.
		cut := bundle[:len(bundle)-12]
		_, err = f.prepare(f.request(cut, commit, f.repo.Initial, f.repo.Initial))
		if got := bundleReason(t, err); got != workspace.ReasonTruncated && got != workspace.ReasonCorruptObjects {
			t.Fatalf("self-consistent truncation: %s", got)
		}
	})

	t.Run("prerequisites are limited to the pinned input and expected tip", func(t *testing.T) {
		f := newFixture(t, nil)
		other := f.repo.HumanPush(t, f.repo.BaseRef, "elsewhere", gitfx.File{Path: "h.txt", Content: "h"})
		commit, bundle, _ := f.repo.Bundle(t, other, "change", gitfx.File{Path: "a.txt", Content: "one"})
		_, err := f.prepare(f.request(bundle, commit, f.repo.Initial, f.repo.Initial))
		if got := bundleReason(t, err); got != workspace.ReasonBadPrerequisite {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("a commit that does not descend from the pinned input is rejected", func(t *testing.T) {
		f := newFixture(t, nil)
		tip := f.repo.HumanPush(t, f.repo.BaseRef, "human", gitfx.File{Path: "h.txt", Content: "h"})
		// Built on the initial commit, but the writer was pinned to the human tip.
		commit, bundle, _ := f.repo.Bundle(t, f.repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "one"})
		_, err := f.prepare(f.request(bundle, commit, tip, f.repo.Initial))
		if got := bundleReason(t, err); got != workspace.ReasonNotDescendant {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("the proposed commit must be the bundle's ref", func(t *testing.T) {
		f := newFixture(t, nil)
		_, bundle, _ := f.repo.Bundle(t, f.repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "one"})
		_, err := f.prepare(f.request(bundle, strings.Repeat("a", 40), f.repo.Initial, f.repo.Initial))
		if got := bundleReason(t, err); got != workspace.ReasonCommitMismatch {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("an injected secret in a new object is refused, anywhere in the introduced history", func(t *testing.T) {
		f := newFixture(t, nil)
		const secret = "s3cr3t-value-0123456789"
		// In a blob.
		commit, bundle, _ := f.repo.Bundle(t, f.repo.Initial, "innocent", gitfx.File{Path: "cfg.txt", Content: "token=" + secret})
		req := f.request(bundle, commit, f.repo.Initial, f.repo.Initial)
		req.Needles = [][]byte{[]byte(secret)}
		if _, err := f.prepare(req); !errors.Is(err, workspace.ErrSecretInSource) {
			t.Fatalf("blob: %v", err)
		}
		// In a commit message.
		commit, bundle, _ = f.repo.Bundle(t, f.repo.Initial, "uses "+secret, gitfx.File{Path: "a.txt", Content: "fine"})
		req = f.request(bundle, commit, f.repo.Initial, f.repo.Initial)
		req.Needles = [][]byte{[]byte(secret)}
		if _, err := f.prepare(req); !errors.Is(err, workspace.ErrSecretInSource) {
			t.Fatalf("commit message: %v", err)
		}
		// A clean bundle with the same needles passes; short needles are ignored.
		commit, bundle, _ = f.repo.Bundle(t, f.repo.Initial, "clean", gitfx.File{Path: "a.txt", Content: "fine"})
		req = f.request(bundle, commit, f.repo.Initial, f.repo.Initial)
		req.Needles = [][]byte{[]byte(secret), []byte("fine")}
		p, err := f.prepare(req)
		if err != nil {
			t.Fatalf("clean bundle: %v", err)
		}
		p.Release()
	})

	t.Run("credentials never appear in scratch or errors", func(t *testing.T) {
		f := newFixture(t, nil)
		commit, bundle, _ := f.repo.Bundle(t, f.repo.Initial, "change", gitfx.File{Path: "a.txt", Content: "one"})
		p, err := f.prepare(f.request(bundle, commit, f.repo.Initial, f.repo.Initial))
		if err != nil {
			t.Fatal(err)
		}
		for _, tok := range []string{f.srv.ReadToken, f.srv.WriteToken} {
			if hit := walkContains(t, f.scratch, tok); hit != "" {
				t.Fatalf("credential persisted in %s", hit)
			}
		}
		p.Release()

		// A rejected credential fails without echoing it.
		bad := f.withTokens("wrong-token-9999", "wrong-write-9999")
		_, err = bad.prepare(bad.request(bundle, commit, bad.repo.Initial, bad.repo.Initial))
		if err == nil {
			t.Fatal("prepare succeeded with a rejected credential")
		}
		if strings.Contains(err.Error(), "wrong-token-9999") {
			t.Fatalf("credential leaked into the error: %v", err)
		}
	})

	t.Run("transfer limit", func(t *testing.T) {
		f := newFixture(t, func(o *gitad.Options) { o.MaxBundleBytes = 2048; o.MaxScratchBytes = 1 << 20 })
		big := make([]byte, 64<<10)
		if _, err := rand.Read(big); err != nil {
			t.Fatal(err)
		}
		commit, bundle, _ := f.repo.Bundle(t, f.repo.Initial, "big", gitfx.File{Path: "big.bin", Content: string(big)})
		// Declared size is refused before a byte is read.
		req := f.request(bundle, commit, f.repo.Initial, f.repo.Initial)
		if _, err := f.prepare(req); !errors.Is(err, workspace.ErrLimitExceeded) {
			t.Fatalf("declared oversize: %v", err)
		}
		// An understated size cannot be used to sneak bytes past the limit.
		req = f.request(bundle, commit, f.repo.Initial, f.repo.Initial)
		req.Size = 0
		if _, err := f.prepare(req); !errors.Is(err, workspace.ErrLimitExceeded) {
			t.Fatalf("undeclared oversize: %v", err)
		}
	})

	t.Run("scratch limit is enforced while Git expands objects", func(t *testing.T) {
		f := newFixture(t, func(o *gitad.Options) { o.MaxBundleBytes = 3 << 20; o.MaxScratchBytes = 3<<20 + 512<<10 })
		big := make([]byte, 2<<20)
		if _, err := rand.Read(big); err != nil {
			t.Fatal(err)
		}
		commit, bundle, _ := f.repo.Bundle(t, f.repo.Initial, "big", gitfx.File{Path: "big.bin", Content: string(big)})
		_, err := f.prepare(f.request(bundle, commit, f.repo.Initial, f.repo.Initial))
		if !errors.Is(err, workspace.ErrLimitExceeded) {
			t.Fatalf("scratch overflow: %v", err)
		}
		if entries := scratchLeft(t, f.scratch); entries != 0 {
			t.Fatalf("%d scratch directories left behind", entries)
		}
	})

	t.Run("command deadline", func(t *testing.T) {
		f := newFixture(t, func(o *gitad.Options) { o.CommandTimeout = 400 * time.Millisecond })
		f.srv.DelayInfoRefs(5 * time.Second)
		start := time.Now()
		_, _, err := f.reader.ResolveRef(context.Background(), f.ref, f.repo.BaseRef)
		if !errors.Is(err, workspace.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
		if time.Since(start) > 4*time.Second {
			t.Fatalf("deadline not enforced: %s", time.Since(start))
		}
	})

	t.Run("options require explicit positive bounds", func(t *testing.T) {
		srv := gitfx.NewServer(t)
		for _, mutate := range []func(*gitad.Options){
			func(o *gitad.Options) { o.CommandTimeout = 0 },
			func(o *gitad.Options) { o.MaxBundleBytes = 0 },
			func(o *gitad.Options) { o.MaxScratchBytes = 0 },
			func(o *gitad.Options) { o.ScratchRoot = "" },
			func(o *gitad.Options) { o.MaxScratchBytes = o.MaxBundleBytes - 1 },
		} {
			o := defaultOptions(t, srv)
			mutate(&o)
			if _, err := gitad.NewReader(o, &tokens{}); !errors.Is(err, workspace.ErrInvalidConfiguration) {
				t.Fatalf("accepted invalid options: %v", err)
			}
		}
		if _, err := gitad.NewReader(defaultOptions(t, srv), nil); !errors.Is(err, workspace.ErrNotConfigured) {
			t.Fatalf("accepted missing credentials: %v", err)
		}
	})

	t.Run("only plain https repository URLs are used", func(t *testing.T) {
		f := newFixture(t, nil)
		for _, u := range []string{"http://127.0.0.1/x.git", "https://user:pw@127.0.0.1/x.git", "file:///etc", "https://127.0.0.1/x.git?a=b", "ssh://h/x"} {
			ref := f.ref
			ref.URL = u
			if _, _, err := f.reader.ResolveRef(context.Background(), ref, f.repo.BaseRef); !errors.Is(err, workspace.ErrInvalidValue) {
				t.Fatalf("%s accepted: %v", u, err)
			}
		}
	})
}

func scratchLeft(t *testing.T, root string) int {
	t.Helper()
	n := 0
	entries, err := readDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e, "op-") {
			n++
		}
	}
	return n
}
