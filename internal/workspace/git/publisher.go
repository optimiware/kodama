package git

import (
	"context"
	"fmt"

	"github.com/optimiware/kodama/internal/workspace"
)

// Publisher is the only component holding write authority. It resolves
// publisher credential references and performs expected-old-ref updates; it
// never force-pushes unconditionally.
type Publisher struct{ e *engine }

var _ workspace.RefPublisher = (*Publisher)(nil)

// NewPublisher validates the options. creds must resolve publisher references.
func NewPublisher(o Options, creds Credentials) (*Publisher, error) {
	e, err := newEngine(o, creds)
	if err != nil {
		return nil, err
	}
	return &Publisher{e: e}, nil
}

// Close removes the private home directory.
func (p *Publisher) Close() { p.e.close() }

// Publish implements workspace.RefPublisher. It first checks fast-forward
// ancestry in the verified scratch, then pushes with an explicit
// expected-old-ref lease so the server itself rejects a moved branch. Any error
// leaves the outcome unknown until read back from the remote.
func (p *Publisher) Publish(ctx context.Context, repo workspace.RepoRef, req workspace.PublishRequest) error {
	if err := validateRepo(repo); err != nil {
		return err
	}
	if err := workspace.ValidateBranch(req.Branch); err != nil {
		return err
	}
	if err := req.Expected.Validate(); err != nil {
		return err
	}
	if err := req.Commit.Validate(); err != nil {
		return err
	}
	src, ok := req.Source.(*prepared)
	if !ok || src == nil || src.commit != req.Commit {
		return fmt.Errorf("%w: publication source was not prepared for this commit", workspace.ErrInvalidValue)
	}
	ff, err := src.DescendsFrom(ctx, req.Expected)
	if err != nil {
		return err
	}
	if !ff {
		return &workspace.ConflictError{Reason: "not_fast_forward"}
	}
	cred, err := p.e.credential(ctx, repo, repo.PublisherSecretRef)
	if err != nil {
		return err
	}
	_, err = p.e.run(ctx, cmdSpec{dir: src.dir, proto: "https", cred: cred, url: repo.URL, watch: src.sc.dir,
		args: []string{"push", "--quiet", "--no-verify", "--no-recurse-submodules",
			"--force-with-lease=" + req.Branch + ":" + string(req.Expected),
			repo.URL, string(req.Commit) + ":" + req.Branch}})
	return err
}

// CreateRef implements workspace.RefPublisher: create ref at commit only when
// absent, adopting an existing ref already at commit.
func (p *Publisher) CreateRef(ctx context.Context, repo workspace.RepoRef, ref string, commit workspace.Commit, from string) error {
	if err := validateRepo(repo); err != nil {
		return err
	}
	if err := workspace.ValidateBranch(ref); err != nil {
		return err
	}
	if err := workspace.ValidateBranch(from); err != nil {
		return err
	}
	if err := commit.Validate(); err != nil {
		return err
	}
	cred, err := p.e.credential(ctx, repo, repo.PublisherSecretRef)
	if err != nil {
		return err
	}
	sc, err := p.e.newScratch()
	if err != nil {
		return err
	}
	defer sc.cleanup()
	existing, found, err := p.e.lsRemote(ctx, sc, repo, cred, ref)
	if err != nil {
		return err
	}
	if found {
		if existing == commit {
			return nil
		}
		return workspace.ErrBranchExists
	}
	dir, err := p.e.initBare(ctx, sc, commit)
	if err != nil {
		return err
	}
	if err := p.e.fetchBranch(ctx, sc, dir, repo, cred, from, 0); err != nil {
		return err
	}
	has, err := p.e.hasCommit(ctx, sc, dir, commit)
	if err != nil {
		return err
	}
	if !has {
		return fmt.Errorf("%w: commit is not reachable from %s", workspace.ErrInvalidValue, from)
	}
	_, perr := p.e.run(ctx, cmdSpec{dir: dir, proto: "https", cred: cred, url: repo.URL, watch: sc.dir,
		args: []string{"push", "--quiet", "--no-verify", "--force-with-lease=" + ref + ":", repo.URL, string(commit) + ":" + ref}})
	if perr == nil {
		return nil
	}
	// A lost response or a racing creator: read the ref back.
	if now, found, rerr := p.e.lsRemote(ctx, sc, repo, cred, ref); rerr == nil && found {
		if now == commit {
			return nil
		}
		return workspace.ErrBranchExists
	}
	return perr
}

// DeleteRef implements workspace.RefPublisher: delete ref only while it still
// points at expected; an absent ref is already deleted.
func (p *Publisher) DeleteRef(ctx context.Context, repo workspace.RepoRef, ref string, expected workspace.Commit) error {
	if err := validateRepo(repo); err != nil {
		return err
	}
	if err := workspace.ValidateBranch(ref); err != nil {
		return err
	}
	if err := expected.Validate(); err != nil {
		return err
	}
	cred, err := p.e.credential(ctx, repo, repo.PublisherSecretRef)
	if err != nil {
		return err
	}
	sc, err := p.e.newScratch()
	if err != nil {
		return err
	}
	defer sc.cleanup()
	existing, found, err := p.e.lsRemote(ctx, sc, repo, cred, ref)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if existing != expected {
		return fmt.Errorf("%w: reference moved; refusing to delete", workspace.ErrStale)
	}
	dir, err := p.e.initBare(ctx, sc, expected)
	if err != nil {
		return err
	}
	_, perr := p.e.run(ctx, cmdSpec{dir: dir, proto: "https", cred: cred, url: repo.URL, watch: sc.dir,
		args: []string{"push", "--quiet", "--no-verify", "--force-with-lease=" + ref + ":" + string(expected), repo.URL, ":" + ref}})
	if perr == nil {
		return nil
	}
	if _, found, rerr := p.e.lsRemote(ctx, sc, repo, cred, ref); rerr == nil && !found {
		return nil
	}
	return perr
}
