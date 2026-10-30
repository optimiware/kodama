package gitservice

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ---- initialization ----

// initialize creates the bare repository at the ID-derived path with the
// server-owned configuration, idempotently: a repeat finds the repository,
// reapplies the configuration and the default branch's HEAD and reports
// Created false. A crash leaves at most an unreferenced .init-* directory
// (removed at start); the repository appears with one atomic rename.
func (s *Service) initialize(ctx context.Context, pathID string, req gitv1.InitializeRepositoryRequest) (gitv1.InitializeRepositoryResponse, error) {
	if req.RepositoryID != pathID {
		return gitv1.InitializeRepositoryResponse{}, contractError(domain.CodeInvalidRequest, "repositoryId does not match the path")
	}
	dir, err := s.repoDir(req.RepositoryID)
	if err != nil {
		return gitv1.InitializeRepositoryResponse{}, err
	}
	// Initialization and reconfiguration run under the repository's mutation
	// gate: they are refused while the repository is fenced, never overlap the
	// repair that removes lock files (which could be this command's HEAD.lock),
	// and are rechecked once they hold the initialization mutex, because a fence
	// may have been raised while the request queued.
	g := s.gates.of(req.RepositoryID)
	release, err := g.acquireSharedKind(ctx, sharedConfig)
	if err != nil {
		return gitv1.InitializeRepositoryResponse{}, err
	}
	defer release()
	s.initMu.Lock()
	defer s.initMu.Unlock()
	if err := g.checkFence(); err != nil {
		return gitv1.InitializeRepositoryResponse{}, err
	}
	created := false
	if !isRepository(dir) {
		tmp := filepath.Join(s.layout.repositories, ".init-"+randomID("i", 8))
		defer os.RemoveAll(tmp)
		res, err := s.execGitTracked(ctx, s.layout.repositories, req.RepositoryID, false, nil, "init", "--bare", "--quiet", "--template=", "--initial-branch="+req.DefaultBranch, tmp)
		if err != nil {
			return gitv1.InitializeRepositoryResponse{}, err
		}
		if err := res.must("init"); err != nil {
			return gitv1.InitializeRepositoryResponse{}, err
		}
		if err := s.applyRepoConfig(ctx, tmp, req.RepositoryID); err != nil {
			return gitv1.InitializeRepositoryResponse{}, err
		}
		if err := journalAt(tmp).create(); err != nil {
			return gitv1.InitializeRepositoryResponse{}, err
		}
		if err := syncDir(tmp); err != nil {
			return gitv1.InitializeRepositoryResponse{}, err
		}
		switch err := os.Rename(tmp, dir); {
		case err == nil:
			// From here on the repository exists: a failure no longer means
			// "nothing happened", so the caller must repeat the request.
			created = true
			if err := syncDir(s.layout.repositories); err != nil {
				return gitv1.InitializeRepositoryResponse{}, unknownOutcome(err)
			}
		case isRepository(dir): // another process won the rename: it exists
		default:
			return gitv1.InitializeRepositoryResponse{}, err
		}
	} else {
		// A repeat reapplies configuration and HEAD in place; a failure midway
		// may have applied part of it.
		if err := s.applyRepoConfig(ctx, dir, req.RepositoryID); err != nil {
			return gitv1.InitializeRepositoryResponse{}, unknownOutcome(err)
		}
		if err := s.setHead(ctx, dir, req.DefaultBranch); err != nil {
			return gitv1.InitializeRepositoryResponse{}, unknownOutcome(err)
		}
		if err := journalAt(dir).create(); err != nil {
			return gitv1.InitializeRepositoryResponse{}, unknownOutcome(err)
		}
	}
	empty, err := s.repositoryEmpty(ctx, dir)
	if err != nil {
		return gitv1.InitializeRepositoryResponse{}, unknownOutcome(err)
	}
	return gitv1.InitializeRepositoryResponse{RepositoryID: req.RepositoryID, DefaultBranch: req.DefaultBranch, Empty: empty, Created: created}, nil
}

// applyRepoConfig writes the provisioning settings into the repository's own
// config (they are also enforced at command scope, so this is the durable copy
// an operator sees on the volume).
func (s *Service) applyRepoConfig(ctx context.Context, dir, repositoryID string) error {
	file := filepath.Join(dir, "config")
	for _, p := range repoInitConfig(s.layout.hooks, s.cfg.Limits.MaxPushBytes) {
		res, err := s.execGitTracked(ctx, dir, repositoryID, false, nil, "config", "--file", file, p.key, p.value)
		if err != nil {
			return err
		}
		if err := res.must("config " + p.key); err != nil {
			return err
		}
	}
	return nil
}

// setHead points HEAD at the default branch (the symbolic ref a clone of an
// empty repository, and git's default branch advertisement, follow).
func (s *Service) setHead(ctx context.Context, dir, branch string) error {
	want := "refs/heads/" + branch
	res, err := s.runGit(ctx, dir, false, nil, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(res.Stdout)) == want {
		return nil
	}
	res, err = s.runGitTracked(ctx, dir, false, nil, "symbolic-ref", "HEAD", want)
	if err != nil {
		return err
	}
	return res.must("symbolic-ref")
}

func (s *Service) repositoryEmpty(ctx context.Context, dir string) (bool, error) {
	res, err := s.runGit(ctx, dir, false, nil, "for-each-ref", "--count=1", "--format=%(refname)")
	if err != nil {
		return false, err
	}
	if err := res.must("for-each-ref"); err != nil {
		return false, err
	}
	return strings.TrimSpace(string(res.Stdout)) == "", nil
}

// ---- reads ----

// readRef returns the exact current value of ref ("" when absent). It lists
// with for-each-ref and matches the name exactly, so a short name is never
// resolved through another namespace.
func (s *Service) readRef(ctx context.Context, dir, ref string) (string, error) {
	res, err := s.runGit(ctx, dir, false, nil, "for-each-ref", "--format=%(refname) %(objectname)", ref)
	if err != nil {
		return "", err
	}
	if err := res.must("for-each-ref"); err != nil {
		return "", err
	}
	return exactRefValue(res.Stdout, ref), nil
}

// forEachRef streams `refname objectname` lines under prefix in refname order
// and stops, terminating Git, when fn returns true.
func (s *Service) forEachRef(ctx context.Context, dir, prefix string, fn func(name, oid string) (stop bool)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.cfg.GitBinary, "--git-dir="+dir, "for-each-ref", "--sort=refname", "--format=%(refname) %(objectname)", prefix)
	cmd.Dir = dir
	cmd.Env = s.gitEnv(false)
	cmd.ExtraFiles = []*os.File{s.lock}
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	stopped := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		name, oid, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		if fn(name, oid) {
			stopped = true
			cancel()
			break
		}
	}
	scanErr := sc.Err()
	waitErr := cmd.Wait()
	switch {
	case stopped:
		return nil
	case scanErr != nil:
		return scanErr
	}
	return waitErr
}

func (s *Service) readRefs(ctx context.Context, pathID string, req gitv1.ReadRefsRequest) (gitv1.RefsResponse, error) {
	dir, err := s.rpcRepo(pathID, req.RepositoryID)
	if err != nil {
		return gitv1.RefsResponse{}, err
	}
	resp := gitv1.RefsResponse{Refs: []gitv1.RefTip{}}
	err = s.forEachRef(ctx, dir, req.Prefix, func(name, oid string) bool {
		if req.Cursor != nil && name <= *req.Cursor {
			return false
		}
		if gitv1.ValidateRefName(name) != nil || gitv1.ValidateObjectID(oid) != nil {
			return false // outside the contract's ref syntax: never served
		}
		if int64(len(resp.Refs)) >= req.Limit {
			last := resp.Refs[len(resp.Refs)-1].Ref
			resp.NextCursor = &last
			return true
		}
		resp.Refs = append(resp.Refs, gitv1.RefTip{Ref: name, Commit: oid})
		return false
	})
	if err != nil {
		return gitv1.RefsResponse{}, err
	}
	if resp.Empty, err = s.repositoryEmpty(ctx, dir); err != nil {
		return gitv1.RefsResponse{}, err
	}
	return resp, nil
}

// readRefValues reads exact values with the repository's mutation gate held
// (no receive or operation can start meanwhile) and reports whether none was in
// progress, so a quiescent answer is a consistent boundary for settlement.
func (s *Service) readRefValues(ctx context.Context, pathID string, req gitv1.ReadRefValuesRequest) (gitv1.ReadRefValuesResponse, error) {
	dir, err := s.rpcRepo(pathID, req.RepositoryID)
	if err != nil {
		return gitv1.ReadRefValuesResponse{}, err
	}
	resp := gitv1.ReadRefValuesResponse{Values: []gitv1.RefValue{}, WorkerInstance: s.instance}
	err = s.gates.of(req.RepositoryID).snapshot(func(quiescent bool) error {
		head, err := journalAt(dir).head()
		if err != nil {
			return err
		}
		for _, ref := range req.Refs {
			v, err := s.readRef(ctx, dir, ref)
			if err != nil {
				return err
			}
			rv := gitv1.RefValue{Ref: ref}
			if v != "" {
				c := v
				rv.Commit = &c
			}
			resp.Values = append(resp.Values, rv)
		}
		resp.Quiescent, resp.JournalHead = quiescent, head
		return nil
	})
	if err != nil {
		return gitv1.ReadRefValuesResponse{}, s.journalError(err)
	}
	return resp, nil
}

// journalError reports an unreadable journal as unavailable: the control plane
// then leaves the entries unknown (journal_unreadable) rather than guessing.
func (s *Service) journalError(err error) error {
	if errors.Is(err, errJournalCorrupt) {
		s.cfg.Logger.Error("ref journal is unreadable", "error", err)
		return contractError(domain.CodeUnavailable, "the ref journal is unreadable")
	}
	return err
}

// journalReadError answers the journal read. A journal the worker affirmatively
// found corrupt, undecodable or missing is the definite journal_unreadable (409):
// the control plane maps exactly this code to ErrJournalUnreadable and settles
// the entries unknown. Any other failure (an I/O error, a descriptor limit, a
// lock that could not be taken) says nothing about the journal and stays the
// transient 503 unavailable.
func (s *Service) journalReadError(err error) error {
	if errors.Is(err, errJournalCorrupt) {
		s.cfg.Logger.Error("ref journal is unreadable", "error", err)
		return contractError(gitv1.CodeJournalUnreadable, "the ref journal is unreadable")
	}
	s.cfg.Logger.Error("ref journal could not be read", "error", err)
	return contractError(domain.CodeUnavailable, "the ref journal could not be read")
}

func (s *Service) readJournal(_ context.Context, pathID string, req gitv1.ReadJournalRequest) (gitv1.ReadJournalResponse, error) {
	dir, err := s.rpcRepo(pathID, req.RepositoryID)
	if err != nil {
		return gitv1.ReadJournalResponse{}, err
	}
	match := func(gitv1.JournalEntry) bool { return true }
	switch {
	case req.ReceiveID != nil:
		id := *req.ReceiveID
		match = func(e gitv1.JournalEntry) bool { return e.ReceiveID != nil && *e.ReceiveID == id }
	case req.Ref != nil:
		ref := *req.Ref
		match = func(e gitv1.JournalEntry) bool { return e.Ref == ref }
	}
	res, err := journalAt(dir).readExisting(req.SinceSequence, int(req.Limit), match)
	if err != nil {
		return gitv1.ReadJournalResponse{}, s.journalReadError(err)
	}
	entries := res.Entries
	if entries == nil {
		entries = []gitv1.JournalEntry{}
	}
	return gitv1.ReadJournalResponse{Entries: entries, HeadSequence: res.Head, HasMore: res.HasMore, WorkerInstance: s.instance}, nil
}

func (s *Service) receiveLiveness(pathID, receiveID string) (gitv1.ReceiveLivenessResponse, error) {
	if !repositoryIDPattern.MatchString(receiveID) {
		return gitv1.ReceiveLivenessResponse{}, contractError(domain.CodeInvalidRequest, "receiveId is not a valid ID")
	}
	if _, err := s.existingRepo(pathID); err != nil {
		return gitv1.ReceiveLivenessResponse{}, err
	}
	return gitv1.ReceiveLivenessResponse{ReceiveID: receiveID, Running: s.receiveRunning(pathID, receiveID), WorkerInstance: s.instance}, nil
}

// rpcRepo resolves the repository of an RPC: the body must name the path's
// repository, which must exist.
func (s *Service) rpcRepo(pathID, bodyID string) (string, error) {
	if pathID != bodyID {
		return "", contractError(domain.CodeInvalidRequest, "repositoryId does not match the path")
	}
	return s.existingRepo(bodyID)
}

// ---- server-side ref changes ----

// The operations below change refs through `git update-ref` with the worker's
// hooks, so each is journaled exactly like a receive's transaction: the
// reference-transaction hook appends `prepared` (with the observed value) under
// the ref lock, then `committed` or `aborted`, attributed to the operation ID
// instead of a receive ID. A server operation needs no commit intent.

func (s *Service) operationEnv(dir, repositoryID, operationID string) []string {
	return []string{
		envHookKind + "=" + kindOperation, envOperationID + "=" + operationID, envRepoID + "=" + repositoryID,
		envRepoDir + "=" + dir, envGitBinary + "=" + s.cfg.GitBinary,
		"GIT_COMMITTER_NAME=Kodama Git worker", "GIT_COMMITTER_EMAIL=git-worker@kodama.invalid",
	}
}

// updateRef runs one ref update as an operation. old is the expected value
// ("" for create, which is the all-zero ID to Git).
func (s *Service) updateRef(ctx context.Context, dir, repositoryID, operationID, ref, newValue, old string) (gitResult, error) {
	if err := s.gates.of(repositoryID).checkFence(); err != nil {
		return gitResult{}, err
	}
	env := s.operationEnv(dir, repositoryID, operationID)
	if newValue == "" { // delete
		return s.runGitTracked(ctx, dir, true, env, "update-ref", "--no-deref", "-d", ref, old)
	}
	if old == "" {
		old = gitv1.ZeroObjectID(len(newValue))
	}
	return s.runGitTracked(ctx, dir, true, env, "update-ref", "--no-deref", ref, newValue, old)
}

// settleOperation closes an operation's journaled transaction left open by an
// interrupted earlier attempt: committed when the ref now has the intended
// value, aborted otherwise. It writes nothing when no prepared entry is pending.
func (s *Service) settleOperation(dir, operationID string, u gitv1.RefUpdate, applied bool) error {
	state := gitv1.JournalAborted
	if applied {
		state = gitv1.JournalCommitted
	}
	_, err := journalAt(dir).appendOutcome(attribution{operationID: operationID}, state, []gitv1.RefUpdate{u})
	return err
}

// createRef creates ref at commit under an operation. conflict is the code of a
// ref that exists at another commit; it is answered whatever operation created
// the ref earlier, so a response never names a commit the ref does not point to.
// It converges after an unknown outcome: the ref already at commit is success
// whether or not the earlier attempt finished its journal entry.
func (s *Service) createRef(ctx context.Context, repositoryID, operationID, ref, commit string, conflict domain.ErrorCode) (created bool, err error) {
	dir, err := s.existingRepo(repositoryID)
	if err != nil {
		return false, err
	}
	g := s.gates.of(repositoryID)
	release, err := g.acquireShared(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	g.opMu.Lock()
	defer g.opMu.Unlock()
	// A request that queued behind another one must not act on a repository
	// that was fenced meanwhile (the predecessor may not be verified stopped).
	if err := g.checkFence(); err != nil {
		return false, err
	}
	return s.createRefHeld(ctx, dir, repositoryID, operationID, ref, commit, conflict, false)
}

// createRefHeld is createRef for a caller that holds the gate (shared with the
// operation mutex, or exclusively for a restore replay) and has checked the
// fence. anyObject accepts any existing object as the target (a tag object of a
// replayed bundle); otherwise it must be a commit.
func (s *Service) createRefHeld(ctx context.Context, dir, repositoryID, operationID, ref, commit string, conflict domain.ErrorCode, anyObject bool) (created bool, err error) {
	g := s.gates.of(repositoryID)
	zero := gitv1.ZeroObjectID(len(commit))
	u := gitv1.RefUpdate{Ref: ref, Old: zero, New: commit}
	cur, err := s.readRef(ctx, dir, ref)
	if err != nil {
		return false, err
	}
	switch {
	case cur == commit:
		return false, unknownIf(s.settleOperation(dir, operationID, u, true), s.journalError)
	case cur != "":
		return false, contractError(conflict, "%s exists at another commit", ref)
	}
	if err := g.checkFence(); err != nil {
		return false, err
	}
	if err := s.settleOperation(dir, operationID, u, false); err != nil { // an earlier attempt died before committing
		return false, s.journalError(err)
	}
	target := commit + "^{commit}"
	if anyObject {
		target = commit
	}
	res, err := s.runGit(ctx, dir, false, nil, "cat-file", "-e", target)
	if err != nil {
		return false, err
	}
	if res.ExitCode != 0 {
		return false, contractError(domain.CodeInvalidRequest, "the commit does not exist in the repository")
	}
	res, err = s.updateRef(ctx, dir, repositoryID, operationID, ref, commit, "")
	if err != nil {
		// Git was killed or could not be waited for while it may have been
		// publishing the ref (or running the committed hook after it did).
		return false, unknownOutcome(err)
	}
	if res.ExitCode != 0 {
		// A concurrent writer won the ref lock, or a hook refused the journal
		// write. Observe again and converge.
		now, rerr := s.readRef(ctx, dir, ref)
		if rerr != nil {
			return false, unknownOutcome(rerr)
		}
		switch now {
		case commit:
			return false, nil
		case "":
			return false, fmt.Errorf("%w: update-ref: %s", errGitFailed, truncateText(res.Stderr, 512))
		}
		return false, contractError(conflict, "%s exists at another commit", ref)
	}
	return true, nil
}

// unknownIf wraps a failure of a step that follows an observed change.
func unknownIf(err error, classify func(error) error) error {
	if err == nil {
		return nil
	}
	return unknownOutcome(classify(err))
}

// deleteRef removes ref when it still has expected under an operation; an
// absent ref is success with deleted false.
func (s *Service) deleteRef(ctx context.Context, repositoryID, operationID, ref, expected string) (deleted bool, err error) {
	dir, err := s.existingRepo(repositoryID)
	if err != nil {
		return false, err
	}
	g := s.gates.of(repositoryID)
	release, err := g.acquireShared(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	g.opMu.Lock()
	defer g.opMu.Unlock()
	if err := g.checkFence(); err != nil {
		return false, err
	}
	u := gitv1.RefUpdate{Ref: ref, Old: expected, New: gitv1.ZeroObjectID(len(expected))}
	cur, err := s.readRef(ctx, dir, ref)
	if err != nil {
		return false, err
	}
	switch {
	case cur == "":
		return false, unknownIf(s.settleOperation(dir, operationID, u, true), s.journalError)
	case cur != expected:
		return false, contractError(gitv1.CodeRefChanged, "%s no longer has the expected value", ref)
	}
	res, err := s.updateRef(ctx, dir, repositoryID, operationID, ref, "", expected)
	if err != nil {
		return false, unknownOutcome(err)
	}
	if res.ExitCode != 0 {
		now, rerr := s.readRef(ctx, dir, ref)
		if rerr != nil {
			return false, unknownOutcome(rerr)
		}
		switch now {
		case "":
			return false, nil
		case expected:
			return false, fmt.Errorf("%w: update-ref -d: %s", errGitFailed, truncateText(res.Stderr, 512))
		}
		return false, contractError(gitv1.CodeRefChanged, "%s no longer has the expected value", ref)
	}
	return true, nil
}

func (s *Service) setRetainedRef(ctx context.Context, pathID string, req gitv1.SetRetainedRefRequest) (gitv1.SetRetainedRefResponse, error) {
	if _, err := s.rpcRepo(pathID, req.RepositoryID); err != nil {
		return gitv1.SetRetainedRefResponse{}, err
	}
	created, err := s.createRef(ctx, req.RepositoryID, req.OperationID, req.Ref, req.Commit, gitv1.CodeRetainedRefExists)
	if err != nil {
		return gitv1.SetRetainedRefResponse{}, err
	}
	return gitv1.SetRetainedRefResponse{Ref: req.Ref, Commit: req.Commit, Created: created}, nil
}

func (s *Service) deleteRetainedRef(ctx context.Context, pathID string, req gitv1.DeleteRetainedRefRequest) (gitv1.DeleteRetainedRefResponse, error) {
	if _, err := s.rpcRepo(pathID, req.RepositoryID); err != nil {
		return gitv1.DeleteRetainedRefResponse{}, err
	}
	deleted, err := s.deleteRef(ctx, req.RepositoryID, req.OperationID, req.Ref, req.ExpectedCommit)
	if err != nil {
		return gitv1.DeleteRetainedRefResponse{}, err
	}
	return gitv1.DeleteRetainedRefResponse{Ref: req.Ref, Deleted: deleted}, nil
}

func (s *Service) createBranch(ctx context.Context, pathID string, req gitv1.CreateBranchRequest) (gitv1.CreateBranchResponse, error) {
	if _, err := s.rpcRepo(pathID, req.RepositoryID); err != nil {
		return gitv1.CreateBranchResponse{}, err
	}
	created, err := s.createRef(ctx, req.RepositoryID, req.OperationID, req.Ref, req.Commit, gitv1.CodeBranchExists)
	if err != nil {
		return gitv1.CreateBranchResponse{}, err
	}
	return gitv1.CreateBranchResponse{Ref: req.Ref, Commit: req.Commit, Created: created}, nil
}

func (s *Service) deleteBranch(ctx context.Context, pathID string, req gitv1.DeleteBranchRequest) (gitv1.DeleteBranchResponse, error) {
	if _, err := s.rpcRepo(pathID, req.RepositoryID); err != nil {
		return gitv1.DeleteBranchResponse{}, err
	}
	deleted, err := s.deleteRef(ctx, req.RepositoryID, req.OperationID, req.Ref, req.ExpectedCommit)
	if err != nil {
		return gitv1.DeleteBranchResponse{}, err
	}
	return gitv1.DeleteBranchResponse{Ref: req.Ref, Deleted: deleted}, nil
}

// ---- maintenance ----

// maintain runs Git maintenance with the repository's gate held exclusively:
// it never overlaps a receive or a server-side ref operation, and none starts
// until it ends. Retained refs are ordinary refs to gc, so every pinned commit
// stays reachable (GR-13); unreachable objects are only pruned after
// gc.pruneExpire.
func (s *Service) maintain(ctx context.Context, pathID string, req gitv1.MaintainRequest) (gitv1.MaintainResponse, error) {
	dir, err := s.rpcRepo(pathID, req.RepositoryID)
	if err != nil {
		return gitv1.MaintainResponse{}, err
	}
	release, err := s.gates.of(req.RepositoryID).acquireExclusive(ctx)
	if err != nil {
		return gitv1.MaintainResponse{}, err
	}
	defer release()
	// From here on maintenance has been launched: a failure or a cancellation
	// may have repacked or pruned part of the repository, so the caller must not
	// assume nothing happened.
	if s.maintainHook != nil {
		if err := s.maintainHook(ctx, req.RepositoryID); err != nil {
			return gitv1.MaintainResponse{}, unknownOutcome(err)
		}
	} else {
		res, err := s.runGitTracked(ctx, dir, false, nil, "maintenance", "run", "--quiet", "--task=gc", "--task=commit-graph")
		if err != nil {
			return gitv1.MaintainResponse{}, unknownOutcome(err)
		}
		if err := res.must("maintenance"); err != nil {
			return gitv1.MaintainResponse{}, unknownOutcome(err)
		}
	}
	return gitv1.MaintainResponse{OperationID: req.OperationID, CompletedAt: gitv1.Timestamp(s.now().Format("2006-01-02T15:04:05.000000000Z"))}, nil
}

// removeStaleLocks deletes ref and packed-refs locks and incoming-object
// quarantines under a repository that no process is using.
func removeStaleLocks(dir string) []string {
	var removed []string
	_ = filepath.WalkDir(filepath.Join(dir, "refs"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".lock") {
			if os.Remove(path) == nil {
				removed = append(removed, path)
			}
		}
		return nil
	})
	for _, name := range []string{"packed-refs.lock", "HEAD.lock", "shallow.lock"} {
		if os.Remove(filepath.Join(dir, name)) == nil {
			removed = append(removed, name)
		}
	}
	incoming, _ := filepath.Glob(filepath.Join(dir, "objects", "incoming-*"))
	for _, p := range incoming {
		if os.RemoveAll(p) == nil {
			removed = append(removed, p)
		}
	}
	return removed
}
