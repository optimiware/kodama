// Package localgit implements the read-only gitops.Objects with the Git binary
// against repositories on a mounted volume: ancestry and commit times for
// pruning and bundle creation for backups. It never changes a repository: refs
// are created, replayed or deleted only through the worker, whose hooks, journal
// and mutation gate a plain git process would bypass (bundle replay is the
// worker's ReplayBundle, T-160). It exists for tests and for a process that
// mounts the volume read-only; the control plane uses the worker's RPCs.
// Every process starts from an empty environment with no system or global
// configuration, and arguments are validated object IDs and ref names, never
// free text.
package localgit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
)

// Locator maps a tenant's repository to its directory; it derives the path only
// from the server-assigned IDs (the worker's <DataRoot>/repositories/<id>.git).
type Locator interface {
	RepositoryPath(tenant domain.TenantID, repositoryID string) (string, error)
}

// Objects implements gitops.Objects.
type Objects struct {
	Locator Locator
	// Git is the Git binary (default "git").
	Git string
	// JournalHead, when set, supplies the journal head a bundle is taken at (the
	// journal lives with the worker; this package does not read it).
	JournalHead func(tenant domain.TenantID, repositoryID string) int64
}

var _ gitops.Objects = Objects{}

// Env is the environment every Git process starts from.
func Env() []string {
	return []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "PATH=" + os.Getenv("PATH")}
}

// Run runs Git in the directory with the sanitized environment. A failure
// carries stderr.
func Run(ctx context.Context, git, dir string, stdin io.Reader, stdout io.Writer, args ...string) error {
	if git == "" {
		git = "git"
	}
	cmd := exec.CommandContext(ctx, git, append([]string{"-C", dir}, args...)...)
	cmd.Env = Env()
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		return &Error{Args: args, Err: err, Stderr: strings.TrimSpace(stderr.String())}
	}
	return nil
}

// Error is a failed Git command.
type Error struct {
	Args   []string
	Err    error
	Stderr string
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args[:min(len(e.Args), 2)], " "), e.Err, e.Stderr)
}

func (e *Error) Unwrap() error { return e.Err }

// ExitCode is the exit status of a failed command, or -1.
func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func (o Objects) dir(tenant domain.TenantID, repositoryID string) (string, error) {
	if o.Locator == nil {
		return "", gitops.ErrNotConfigured
	}
	if err := tenant.Validate(); err != nil {
		return "", err
	}
	if err := domain.RunID(repositoryID).Validate(); err != nil { // the opaque-ID syntax
		return "", err
	}
	return o.Locator.RepositoryPath(tenant, repositoryID)
}

func (o Objects) out(ctx context.Context, dir string, args ...string) (string, error) {
	var b bytes.Buffer
	if err := Run(ctx, o.Git, dir, nil, &b, args...); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}

func commitID(id string) error {
	if err := gitv1.ValidateObjectID(id); err != nil || gitv1.IsZeroObjectID(id) {
		return fmt.Errorf("localgit: %q is not a commit ID", id)
	}
	return nil
}

// IsAncestor implements gitops.Objects.
func (o Objects) IsAncestor(ctx context.Context, tenant domain.TenantID, repositoryID, ancestor, descendant string) (bool, error) {
	dir, err := o.dir(tenant, repositoryID)
	if err != nil {
		return false, err
	}
	for _, c := range []string{ancestor, descendant} {
		if err := commitID(c); err != nil {
			return false, err
		}
		if ok, err := o.HasCommit(ctx, tenant, repositoryID, c); err != nil || !ok {
			return false, errors.Join(err, fmt.Errorf("localgit: the repository lacks commit %s", c))
		}
	}
	err = Run(ctx, o.Git, dir, nil, io.Discard, "merge-base", "--is-ancestor", ancestor, descendant)
	switch {
	case err == nil:
		return true, nil
	case ExitCode(err) == 1:
		return false, nil
	}
	return false, err
}

// HasCommit implements gitops.Objects.
func (o Objects) HasCommit(ctx context.Context, tenant domain.TenantID, repositoryID, commit string) (bool, error) {
	dir, err := o.dir(tenant, repositoryID)
	if err != nil {
		return false, err
	}
	if err := commitID(commit); err != nil {
		return false, err
	}
	err = Run(ctx, o.Git, dir, nil, io.Discard, "cat-file", "-e", commit+"^{commit}")
	switch {
	case err == nil:
		return true, nil
	case ExitCode(err) == 1 || ExitCode(err) == 128:
		return false, nil
	}
	return false, err
}

// CommitTime implements gitops.Objects: the committer time.
func (o Objects) CommitTime(ctx context.Context, tenant domain.TenantID, repositoryID, commit string) (time.Time, error) {
	dir, err := o.dir(tenant, repositoryID)
	if err != nil {
		return time.Time{}, err
	}
	if err := commitID(commit); err != nil {
		return time.Time{}, err
	}
	s, err := o.out(ctx, dir, "show", "-s", "--format=%ct", commit)
	if err != nil {
		return time.Time{}, err
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("localgit: commit time %q: %w", s, err)
	}
	return time.Unix(n, 0).UTC(), nil
}

// parseHeads parses "<object> <ref>" lines; HEAD and refs the contract does not
// name are not refs of Kodama.
func parseHeads(s string) ([]gitv1.RefTip, error) {
	var out []gitv1.RefTip
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, ref, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("localgit: unexpected ref line %q", line)
		}
		if ref == "HEAD" || gitv1.ValidateObjectID(id) != nil || gitv1.ValidateRefName(ref) != nil {
			continue
		}
		out = append(out, gitv1.RefTip{Ref: ref, Commit: id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// CreateBundle implements gitops.Objects: the bundle is written to a temporary
// file first (its refs are read from the file, so they are exactly what it
// holds), then copied to w.
func (o Objects) CreateBundle(ctx context.Context, tenant domain.TenantID, repositoryID string, w io.Writer) (gitops.Bundle, error) {
	dir, err := o.dir(tenant, repositoryID)
	if err != nil {
		return gitops.Bundle{}, err
	}
	n, err := o.out(ctx, dir, "for-each-ref", "--count=1", "--format=%(refname)", "refs/")
	if err != nil {
		return gitops.Bundle{}, err
	}
	if n == "" {
		return gitops.Bundle{}, gitops.ErrEmptyRepository
	}
	tmp, err := os.CreateTemp("", "kodama-bundle-*")
	if err != nil {
		return gitops.Bundle{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	// git refuses to overwrite nothing: the temporary file exists, so remove it first.
	if err := os.Remove(tmp.Name()); err != nil {
		return gitops.Bundle{}, err
	}
	if err := Run(ctx, o.Git, dir, nil, io.Discard, "bundle", "create", tmp.Name(), "--all"); err != nil {
		return gitops.Bundle{}, err
	}
	f, err := os.Open(tmp.Name())
	if err != nil {
		return gitops.Bundle{}, err
	}
	defer f.Close()
	listing, err := o.out(ctx, dir, "bundle", "list-heads", tmp.Name())
	if err != nil {
		return gitops.Bundle{}, err
	}
	refs, err := parseHeads(listing)
	if err != nil {
		return gitops.Bundle{}, err
	}
	if _, err := io.Copy(w, f); err != nil {
		return gitops.Bundle{}, err
	}
	// Consistent: this process mounts the volume read-only or quiesced (tests, a
	// snapshot); the worker's CreateBundle derives it from its gate instead.
	b := gitops.Bundle{Refs: refs, Consistent: true}
	if o.JournalHead != nil {
		b.JournalHead = o.JournalHead(tenant, repositoryID)
	}
	return b, nil
}
