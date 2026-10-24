package git

import (
	"context"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/internal/workspace"
)

// Reader reads remote state and verifies bundles with checkout (read-only)
// credentials. It cannot publish: it holds no publisher credential and has no
// push operation.
type Reader struct{ e *engine }

var (
	_ workspace.RefReader      = (*Reader)(nil)
	_ workspace.SourcePreparer = (*Reader)(nil)
)

// NewReader validates the options. creds must resolve checkout references only.
func NewReader(o Options, creds Credentials) (*Reader, error) {
	e, err := newEngine(o, creds)
	if err != nil {
		return nil, err
	}
	return &Reader{e: e}, nil
}

// Close removes the private home directory.
func (r *Reader) Close() { r.e.close() }

func (e *engine) credential(ctx context.Context, repo workspace.RepoRef, ref string) (*Credential, error) {
	c, err := e.creds.Resolve(ctx, repo.Scope, ref)
	if err != nil {
		return nil, fmt.Errorf("%w: credential: %w", workspace.ErrUnavailable, err)
	}
	if !c.valid() {
		return nil, fmt.Errorf("%w: credential is empty or malformed", workspace.ErrUnavailable)
	}
	return &c, nil
}

// lsRemote resolves one exact ref name.
func (e *engine) lsRemote(ctx context.Context, sc *scratch, repo workspace.RepoRef, cred *Credential, ref string) (workspace.Commit, bool, error) {
	out, err := e.run(ctx, cmdSpec{dir: sc.dir, proto: "https", cred: cred, url: repo.URL,
		args: []string{"ls-remote", "--refs", repo.URL, ref}})
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		oid, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && name == ref {
			c := workspace.Commit(oid)
			if err := c.Validate(); err != nil {
				return "", false, fmt.Errorf("%w: remote returned a malformed object name", workspace.ErrUnavailable)
			}
			return c, true, nil
		}
	}
	return "", false, nil
}

// ResolveRef implements workspace.RefReader.
func (r *Reader) ResolveRef(ctx context.Context, repo workspace.RepoRef, ref string) (workspace.Commit, bool, error) {
	if err := validateRepo(repo); err != nil {
		return "", false, err
	}
	if err := workspace.ValidateBranch(ref); err != nil {
		return "", false, err
	}
	cred, err := r.e.credential(ctx, repo, repo.CheckoutSecretRef)
	if err != nil {
		return "", false, err
	}
	sc, err := r.e.newScratch()
	if err != nil {
		return "", false, err
	}
	defer sc.cleanup()
	return r.e.lsRemote(ctx, sc, repo, cred, ref)
}

// BranchContains implements workspace.RefReader.
func (r *Reader) BranchContains(ctx context.Context, repo workspace.RepoRef, branch string, commit workspace.Commit) (workspace.BranchInfo, error) {
	var info workspace.BranchInfo
	if err := validateRepo(repo); err != nil {
		return info, err
	}
	if err := workspace.ValidateBranch(branch); err != nil {
		return info, err
	}
	if commit != "" {
		if err := commit.Validate(); err != nil {
			return info, err
		}
	}
	cred, err := r.e.credential(ctx, repo, repo.CheckoutSecretRef)
	if err != nil {
		return info, err
	}
	sc, err := r.e.newScratch()
	if err != nil {
		return info, err
	}
	defer sc.cleanup()
	return r.e.branchContains(ctx, sc, repo, cred, branch, commit)
}

// branchContains fetches the full branch history into scratch and answers the
// ancestry question locally.
func (e *engine) branchContains(ctx context.Context, sc *scratch, repo workspace.RepoRef, cred *Credential, branch string, commit workspace.Commit) (workspace.BranchInfo, error) {
	var info workspace.BranchInfo
	tip, ok, err := e.lsRemote(ctx, sc, repo, cred, branch)
	if err != nil || !ok {
		return info, err
	}
	info.Tip, info.Exists = tip, true
	if commit == "" {
		return info, nil
	}
	if commit == tip {
		info.Contains = true
		return info, nil
	}
	dir, err := e.initBare(ctx, sc, tip)
	if err != nil {
		return info, err
	}
	if err := e.fetchBranch(ctx, sc, dir, repo, cred, branch, 0); err != nil {
		return info, err
	}
	got, err := e.revParse(ctx, sc, dir, "refs/kodama/tip")
	if err != nil {
		return info, err
	}
	if got != tip {
		// The branch moved between ls-remote and fetch: report what we hold.
		info.Tip = got
	}
	has, err := e.hasCommit(ctx, sc, dir, commit)
	if err != nil || !has {
		return info, err
	}
	info.Contains, err = e.isAncestor(ctx, sc, dir, commit, info.Tip)
	return info, err
}

// fetchBranch fetches branch into refs/kodama/tip; depth 0 means full history.
func (e *engine) fetchBranch(ctx context.Context, sc *scratch, dir string, repo workspace.RepoRef, cred *Credential, branch string, depth int) error {
	args := []string{"fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head"}
	if depth > 0 {
		args = append(args, fmt.Sprintf("--depth=%d", depth))
	}
	args = append(args, repo.URL, "+"+branch+":refs/kodama/tip")
	_, err := e.run(ctx, cmdSpec{dir: dir, proto: "https", cred: cred, url: repo.URL, args: args, watch: sc.dir})
	return err
}

func (e *engine) revParse(ctx context.Context, sc *scratch, dir, rev string) (workspace.Commit, error) {
	out, err := e.run(ctx, cmdSpec{dir: dir, proto: "file", args: []string{"rev-parse", "--verify", "--quiet", rev + "^{commit}"}})
	if err != nil {
		return "", err
	}
	c := workspace.Commit(strings.TrimSpace(string(out)))
	return c, c.Validate()
}

func (e *engine) hasCommit(ctx context.Context, sc *scratch, dir string, c workspace.Commit) (bool, error) {
	_, err := e.run(ctx, cmdSpec{dir: dir, proto: "file", args: []string{"cat-file", "-e", string(c) + "^{commit}"}})
	if err == nil {
		return true, nil
	}
	if exitCode(err) > 0 {
		return false, nil
	}
	return false, err
}

func (e *engine) isAncestor(ctx context.Context, sc *scratch, dir string, ancestor, descendant workspace.Commit) (bool, error) {
	_, err := e.run(ctx, cmdSpec{dir: dir, proto: "file", args: []string{"merge-base", "--is-ancestor", string(ancestor), string(descendant)}})
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, err
}
