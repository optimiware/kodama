// Package gitopstest is the test support of package gitops: real bare Git
// repositories in temporary directories behind a worker stand-in, and in-memory
// stand-ins of every other port. It is test-only.
package gitopstest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitops"
	"github.com/optimiware/kodama/internal/gitops/localgit"
)

// Volume maps tenants' repositories to bare repositories under one temporary
// directory and implements localgit.Locator.
type Volume struct {
	T    testing.TB
	Root string
}

var _ localgit.Locator = (*Volume)(nil)

// NewVolume creates an empty volume.
func NewVolume(t testing.TB) *Volume { return &Volume{T: t, Root: t.TempDir()} }

// RepositoryPath implements localgit.Locator.
func (v *Volume) RepositoryPath(tenant domain.TenantID, id string) (string, error) {
	return filepath.Join(v.Root, string(tenant), id+".git"), nil
}

// RepositoryDir is the directory of the repository on the volume.
func (v *Volume) RepositoryDir(tenant domain.TenantID, id string) string { return v.path(tenant, id) }

func (v *Volume) path(tenant domain.TenantID, id string) string {
	p, _ := v.RepositoryPath(tenant, id)
	return p
}

// Git runs git in a directory and returns trimmed stdout; it fails the test on
// error.
func Git(t testing.TB, dir string, env []string, args ...string) string {
	t.Helper()
	out, err := GitErr(dir, env, args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

// GitErr is Git returning the error.
func GitErr(dir string, env []string, args ...string) (string, error) {
	var out, stderr bytes.Buffer
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(localgit.Env(), env...)
	c.Stdout, c.Stderr = &out, &stderr
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Init creates a bare repository for the tenant and ID (idempotent).
func (v *Volume) Init(tenant domain.TenantID, id string) string {
	v.T.Helper()
	dir := v.path(tenant, id)
	if _, err := os.Stat(dir); err == nil {
		return dir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		v.T.Fatal(err)
	}
	Git(v.T, dir, nil, "init", "--bare", "--quiet", "--initial-branch=main")
	return dir
}

// Commit makes an empty-tree commit with the message and committer time on top
// of the parent (none when ""), and returns its ID. The commit is not on any
// ref until SetRef.
func (v *Volume) Commit(tenant domain.TenantID, id, parent, message string, at time.Time) string {
	v.T.Helper()
	dir := v.Init(tenant, id)
	stamp := at.UTC().Format(time.RFC3339)
	env := []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test",
		"GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp}
	tree := Git(v.T, dir, nil, "hash-object", "-t", "tree", "-w", "--stdin")
	args := []string{"commit-tree", tree, "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	return Git(v.T, dir, env, args...)
}

// SetRef points a ref at a commit.
func (v *Volume) SetRef(tenant domain.TenantID, id, ref, commit string) {
	v.T.Helper()
	Git(v.T, v.path(tenant, id), nil, "update-ref", ref, commit)
}

// DelRef deletes a ref.
func (v *Volume) DelRef(tenant domain.TenantID, id, ref string) {
	v.T.Helper()
	Git(v.T, v.path(tenant, id), nil, "update-ref", "-d", ref)
}

// Ref reads a ref ("" when absent).
func (v *Volume) Ref(tenant domain.TenantID, id, ref string) string {
	out, err := GitErr(v.path(tenant, id), nil, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return out
}

// Refs lists every ref of the repository as ref -> commit.
func (v *Volume) Refs(tenant domain.TenantID, id string) map[string]string {
	v.T.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(Git(v.T, v.path(tenant, id), nil, "for-each-ref", "--format=%(objectname) %(refname)"), "\n") {
		if c, r, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			out[r] = c
		}
	}
	return out
}

// HasObject reports whether the commit exists.
func (v *Volume) HasObject(tenant domain.TenantID, id, commit string) bool {
	_, err := GitErr(v.path(tenant, id), nil, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// GC runs an aggressive garbage collection that prunes everything unreachable
// now: what git maintenance may do at its worst.
func (v *Volume) GC(tenant domain.TenantID, id string) {
	v.T.Helper()
	dir := v.path(tenant, id)
	Git(v.T, dir, nil, "reflog", "expire", "--expire=now", "--all")
	Git(v.T, dir, nil, "gc", "--prune=now", "--quiet")
}

// Worker is a gitops.Worker over a Volume with real Git: refs, retained refs and
// branch deletion are real ref operations with their expected-value checks, and
// maintenance is an aggressive gc. The journal head is scripted.
type Worker struct {
	V *Volume

	mu    sync.Mutex
	heads map[string]int64
	// Calls records "<op> <repo> <operationId>" in order.
	Calls []string
	// Fail, when set for an operation name ("maintain", "deleteBranch",
	// "journal", ...), makes it return the error.
	Fail map[string]error
	// Hook runs when an operation starts.
	Hook   func(op, repo string)
	fenced map[string]bool
}

var _ gitops.Worker = (*Worker)(nil)

// NewWorker returns a worker over the volume.
func NewWorker(v *Volume) *Worker {
	return &Worker{V: v, heads: map[string]int64{}, Fail: map[string]error{}}
}

// SetHead scripts the journal head of a repository.
func (w *Worker) SetHead(tenant domain.TenantID, id string, head int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.heads[string(tenant)+"/"+id] = head
}

// Count is how many recorded calls start with the prefix.
func (w *Worker) Count(prefix string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.Calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (w *Worker) enter(op, repo, id string) error {
	w.mu.Lock()
	w.Calls = append(w.Calls, strings.TrimSpace(op+" "+repo+" "+id))
	hook, err := w.Hook, w.Fail[op]
	w.mu.Unlock()
	if hook != nil {
		hook(op, repo)
	}
	return err
}

func contract(code domain.ErrorCode, msg string) error {
	return &gitv1.ContractError{Code: code, Message: msg}
}

// Initialize implements gitops.Worker.
func (w *Worker) Initialize(_ context.Context, tenant domain.TenantID, req gitv1.InitializeRepositoryRequest) (gitv1.InitializeRepositoryResponse, error) {
	if err := w.enter("initialize", req.RepositoryID, ""); err != nil {
		return gitv1.InitializeRepositoryResponse{}, err
	}
	_, existed := os.Stat(w.V.path(tenant, req.RepositoryID))
	w.V.Init(tenant, req.RepositoryID)
	return gitv1.InitializeRepositoryResponse{RepositoryID: req.RepositoryID, DefaultBranch: req.DefaultBranch, Created: existed != nil}, nil
}

// ReadRefs implements gitops.Worker.
func (w *Worker) ReadRefs(_ context.Context, tenant domain.TenantID, req gitv1.ReadRefsRequest) (gitv1.RefsResponse, error) {
	if err := w.enter("readRefs", req.RepositoryID, ""); err != nil {
		return gitv1.RefsResponse{}, err
	}
	refs := w.V.Refs(tenant, req.RepositoryID)
	var names []string
	for r := range refs {
		if strings.HasPrefix(r, req.Prefix) && (req.Cursor == nil || r > *req.Cursor) {
			names = append(names, r)
		}
	}
	sort.Strings(names)
	out := gitv1.RefsResponse{Refs: []gitv1.RefTip{}, Empty: len(refs) == 0}
	for i, r := range names {
		if int64(i) >= req.Limit {
			c := out.Refs[len(out.Refs)-1].Ref
			out.NextCursor = &c
			break
		}
		out.Refs = append(out.Refs, gitv1.RefTip{Ref: r, Commit: refs[r]})
	}
	return out, nil
}

// ReadRefValues implements gitops.Worker.
func (w *Worker) ReadRefValues(_ context.Context, tenant domain.TenantID, req gitv1.ReadRefValuesRequest) (gitv1.ReadRefValuesResponse, error) {
	if err := w.enter("readRefValues", req.RepositoryID, ""); err != nil {
		return gitv1.ReadRefValuesResponse{}, err
	}
	out := gitv1.ReadRefValuesResponse{Quiescent: true, JournalHead: w.head(tenant, req.RepositoryID), WorkerInstance: "worker-1"}
	for _, r := range req.Refs {
		v := gitv1.RefValue{Ref: r}
		if c := w.V.Ref(tenant, req.RepositoryID, r); c != "" {
			v.Commit = &c
		}
		out.Values = append(out.Values, v)
	}
	return out, nil
}

// Head is the scripted journal head of a repository.
func (w *Worker) Head(tenant domain.TenantID, id string) int64 { return w.head(tenant, id) }

func (w *Worker) head(tenant domain.TenantID, id string) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.heads[string(tenant)+"/"+id]
}

// ReadJournal implements gitops.Worker: no entries, the scripted head.
func (w *Worker) ReadJournal(_ context.Context, tenant domain.TenantID, req gitv1.ReadJournalRequest) (gitv1.ReadJournalResponse, error) {
	if err := w.enter("journal", req.RepositoryID, ""); err != nil {
		return gitv1.ReadJournalResponse{}, err
	}
	return gitv1.ReadJournalResponse{Entries: []gitv1.JournalEntry{}, HeadSequence: w.head(tenant, req.RepositoryID), WorkerInstance: "worker-1"}, nil
}

// SetRetainedRef implements gitops.Worker with the contract's idempotency.
func (w *Worker) SetRetainedRef(_ context.Context, tenant domain.TenantID, req gitv1.SetRetainedRefRequest) (gitv1.SetRetainedRefResponse, error) {
	if err := w.enter("setRetainedRef", req.RepositoryID, req.OperationID); err != nil {
		return gitv1.SetRetainedRefResponse{}, err
	}
	switch cur := w.V.Ref(tenant, req.RepositoryID, req.Ref); cur {
	case req.Commit:
		return gitv1.SetRetainedRefResponse{Ref: req.Ref, Commit: req.Commit}, nil
	case "":
		w.V.SetRef(tenant, req.RepositoryID, req.Ref, req.Commit)
		return gitv1.SetRetainedRefResponse{Ref: req.Ref, Commit: req.Commit, Created: true}, nil
	}
	return gitv1.SetRetainedRefResponse{}, contract(gitv1.CodeRetainedRefExists, "the retained ref exists at another commit")
}

func (w *Worker) deleteRef(tenant domain.TenantID, repo, ref, expected string) (bool, error) {
	switch cur := w.V.Ref(tenant, repo, ref); cur {
	case "":
		return false, nil
	case expected:
		Git(w.V.T, w.V.path(tenant, repo), nil, "update-ref", "-d", ref, expected)
		return true, nil
	}
	return false, contract(gitv1.CodeRefChanged, "the ref no longer has the expected value")
}

// DeleteRetainedRef implements gitops.Worker.
func (w *Worker) DeleteRetainedRef(_ context.Context, tenant domain.TenantID, req gitv1.DeleteRetainedRefRequest) (gitv1.DeleteRetainedRefResponse, error) {
	if err := w.enter("deleteRetainedRef", req.RepositoryID, req.OperationID); err != nil {
		return gitv1.DeleteRetainedRefResponse{}, err
	}
	deleted, err := w.deleteRef(tenant, req.RepositoryID, req.Ref, req.ExpectedCommit)
	return gitv1.DeleteRetainedRefResponse{Ref: req.Ref, Deleted: deleted}, err
}

// DeleteBranch implements gitops.Worker.
func (w *Worker) DeleteBranch(_ context.Context, tenant domain.TenantID, req gitv1.DeleteBranchRequest) (gitv1.DeleteBranchResponse, error) {
	if err := w.enter("deleteBranch", req.RepositoryID, req.OperationID); err != nil {
		return gitv1.DeleteBranchResponse{}, err
	}
	deleted, err := w.deleteRef(tenant, req.RepositoryID, req.Ref, req.ExpectedCommit)
	return gitv1.DeleteBranchResponse{Ref: req.Ref, Deleted: deleted}, err
}

// Maintain implements gitops.Worker with an aggressive gc.
func (w *Worker) Maintain(_ context.Context, tenant domain.TenantID, req gitv1.MaintainRequest) (gitv1.MaintainResponse, error) {
	if err := w.enter("maintain", req.RepositoryID, req.OperationID); err != nil {
		return gitv1.MaintainResponse{}, err
	}
	w.V.GC(tenant, req.RepositoryID)
	return gitv1.MaintainResponse{OperationID: req.OperationID, CompletedAt: gitv1.Timestamp(time.Now().UTC().Format("2006-01-02T15:04:05Z"))}, nil
}

// SetRestoreFence implements gitops.Worker: the fence is per repository.
func (w *Worker) SetRestoreFence(_ context.Context, tenant domain.TenantID, repositoryID string, fenced bool) error {
	if err := w.enter("setRestoreFence", repositoryID, fmt.Sprint(fenced)); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fenced == nil {
		w.fenced = map[string]bool{}
	}
	w.fenced[string(tenant)+"/"+repositoryID] = fenced
	return nil
}

// Fenced reports whether the repository is fenced for restore.
func (w *Worker) Fenced(tenant domain.TenantID, repositoryID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fenced[string(tenant)+"/"+repositoryID]
}

// ReplayBundle is the stand-in of the worker's T-160 replay with its
// semantics: it verifies the bundle and its digest, creates the refs the
// repository lacks and nothing else (no fast-forward, no rewind, no deletion),
// reports every ref whose value differs with its relation to the bundle's, is
// idempotent on the operation ID and refuses unless the repository is fenced for
// restore. The real worker does this inside its mutation gate with its hooks;
// this stand-in uses plain git on the test volume.
func (w *Worker) ReplayBundle(_ context.Context, tenant domain.TenantID, req gitops.ReplayRequest, r io.Reader) (gitops.ReplayResult, error) {
	var res gitops.ReplayResult
	repositoryID := req.RepositoryID
	if err := w.enter("replayBundle", repositoryID, req.OperationID); err != nil {
		return res, err
	}
	if !w.Fenced(tenant, repositoryID) {
		return res, contract("restore_fence_required", "the repository is not fenced for restore")
	}
	dir := w.V.path(tenant, repositoryID)
	if _, err := os.Stat(dir); err != nil {
		return res, contract(domain.CodeNotFound, "no such repository")
	}
	tmp, err := os.CreateTemp("", "kodama-replay-*")
	if err != nil {
		return res, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		tmp.Close()
		return res, err
	}
	tmp.Close()
	if req.SHA256 != "" && hex.EncodeToString(h.Sum(nil)) != req.SHA256 {
		return res, fmt.Errorf("%w: the digest does not match", gitops.ErrBackupCorrupt)
	}
	if _, err := GitErr(dir, nil, "bundle", "verify", tmp.Name()); err != nil {
		return res, fmt.Errorf("%w: %v", gitops.ErrBackupCorrupt, err)
	}
	listing, err := GitErr(dir, nil, "bundle", "list-heads", tmp.Name())
	if err != nil {
		return res, err
	}
	for _, line := range strings.Split(listing, "\n") {
		want, ref, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || ref == "HEAD" || gitv1.ValidateRefName(ref) != nil {
			continue
		}
		switch have := w.V.Ref(tenant, repositoryID, ref); have {
		case want:
			res.Unchanged++
		case "":
			if _, err := GitErr(dir, nil, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", tmp.Name(), ref+":"+ref); err != nil {
				return res, fmt.Errorf("%w: %v", gitops.ErrBackupCorrupt, err)
			}
			res.Created++
		default:
			// The bundle's objects may not be in the repository: fetch them without
			// a ref so the relation can be computed (a worker does this in a quarantine).
			rel := gitops.Diverged
			if _, err := GitErr(dir, nil, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", tmp.Name(), want); err == nil {
				switch {
				case isAncestor(dir, want, have):
					rel = gitops.RepositoryAhead
				case isAncestor(dir, have, want):
					rel = gitops.BundleAhead
				}
			}
			res.Conflicts = append(res.Conflicts, gitops.RefConflict{Ref: ref, Have: have, Want: want, Relation: rel})
		}
	}
	return res, nil
}

func isAncestor(dir, a, d string) bool {
	_, err := GitErr(dir, nil, "merge-base", "--is-ancestor", a, d)
	return err == nil
}
