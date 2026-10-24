package git_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
	gitad "github.com/optimiware/kodama/internal/workspace/git"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

const (
	checkoutRef  = "secret://checkout"
	publisherRef = "secret://publisher"
)

var scope = domain.Scope{TenantID: "t013_a", ProjectID: "p013_a"}

// tokens resolves references to the fixture's tokens. The two references
// resolve to different credentials, as production wiring does.
type tokens struct {
	byRef map[string]string
	calls []string
}

func (c *tokens) Resolve(_ context.Context, s domain.Scope, ref string) (gitad.Credential, error) {
	if s != scope {
		return gitad.Credential{}, errors.New("foreign scope")
	}
	c.calls = append(c.calls, ref)
	tok, ok := c.byRef[ref]
	if !ok {
		return gitad.Credential{}, errors.New("unknown reference")
	}
	return gitad.Credential{Username: gitfx.Username, Password: tok}, nil
}

type fixture struct {
	t       *testing.T
	srv     *gitfx.Server
	repo    *gitfx.Repo
	ref     workspace.RepoRef
	reader  *gitad.Reader
	pub     *gitad.Publisher
	scratch string
	opts    gitad.Options
}

func defaultOptions(t *testing.T, srv *gitfx.Server) gitad.Options {
	return gitad.Options{
		CAFile: srv.CAFile, CommandTimeout: 20 * time.Second,
		MaxBundleBytes: 4 << 20, MaxScratchBytes: 32 << 20,
		ScratchRoot: filepath.Join(t.TempDir(), "scratch"),
	}
}

func newFixture(t *testing.T, mutate func(*gitad.Options)) *fixture {
	t.Helper()
	srv := gitfx.NewServer(t)
	opts := defaultOptions(t, srv)
	if mutate != nil {
		mutate(&opts)
	}
	return newFixtureOn(t, srv, opts)
}

func newFixtureOn(t *testing.T, srv *gitfx.Server, opts gitad.Options) *fixture {
	t.Helper()
	repo := srv.NewRepo(t, "repo")
	creds := &tokens{byRef: map[string]string{checkoutRef: srv.ReadToken, publisherRef: srv.WriteToken}}
	reader, err := gitad.NewReader(opts, creds)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := gitad.NewPublisher(opts, creds)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reader.Close)
	t.Cleanup(pub.Close)
	return &fixture{
		t: t, srv: srv, repo: repo, reader: reader, pub: pub, opts: opts, scratch: opts.ScratchRoot,
		ref: workspace.RepoRef{Scope: scope, URL: repo.URL, CheckoutSecretRef: checkoutRef, PublisherSecretRef: publisherRef},
	}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func commitOf(s string) workspace.Commit { return workspace.Commit(s) }

// request builds a PrepareRequest for a bundle created on base.
func (f *fixture) request(bundle []byte, commit, input, expected string) workspace.PrepareRequest {
	return workspace.PrepareRequest{
		Repo: f.ref, Branch: f.repo.BaseRef, Body: bytes.NewReader(bundle), Digest: digestOf(bundle), Size: int64(len(bundle)),
		Input: commitOf(input), ExpectedTip: commitOf(expected), Commit: commitOf(commit),
	}
}

func (f *fixture) prepare(req workspace.PrepareRequest) (workspace.Prepared, error) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return f.reader.Prepare(ctx, req)
}

func bundleReason(t *testing.T, err error) string {
	t.Helper()
	var be *workspace.BundleError
	if !errors.As(err, &be) {
		t.Fatalf("want *BundleError, got %v", err)
	}
	return be.Reason
}

// walkContains reports the first file under root whose content contains needle.
func walkContains(t *testing.T, root, needle string) string {
	t.Helper()
	var hit string
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() && hit == "" {
			if b, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(b), needle) {
				hit = p
			}
		}
		return nil
	})
	return hit
}

func readDir(root string) ([]string, error) {
	es, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out, nil
}

// withTokens builds a second adapter pair against the same server and
// repository but with different credentials.
func (f *fixture) withTokens(read, write string) *fixture {
	f.t.Helper()
	creds := &tokens{byRef: map[string]string{checkoutRef: read, publisherRef: write}}
	reader, err := gitad.NewReader(f.opts, creds)
	if err != nil {
		f.t.Fatal(err)
	}
	pub, err := gitad.NewPublisher(f.opts, creds)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(reader.Close)
	f.t.Cleanup(pub.Close)
	c := *f
	c.reader, c.pub = reader, pub
	return &c
}
