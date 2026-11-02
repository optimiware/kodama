package gitservice

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Object-level operations (T-160): ancestry, commit existence and time, and the
// bundles of backup and restore. The control plane calls them instead of
// mounting the tenant volume (GR-13, GR-15).
//
// Every Git process here starts from the sanitized environment with the hooks
// disabled and GIT_NO_REPLACE_OBJECTS=1: a client can push refs/replace/*, and a
// replacement object must never change the answer about a commit.

// objectEnv is added to the environment of every object read.
var objectEnv = []string{"GIT_NO_REPLACE_OBJECTS=1"}

// bundleNamespaces are the ref namespaces a bundle carries and a replay may
// create: heads, tags and the server's hidden refs.
var bundleNamespaces = []string{"refs/heads/", "refs/tags/", "refs/kodama/"}

func inBundleNamespace(ref string) bool {
	for _, p := range bundleNamespaces {
		if strings.HasPrefix(ref, p) {
			return true
		}
	}
	return false
}

// ---- reads ----

// objectType returns the type of the object ("" when the repository lacks it).
func (s *Service) objectType(ctx context.Context, dir, oid string) (string, error) {
	res, err := s.execGitIO(ctx, dir, "", false, objectEnv, strings.NewReader(oid+"\n"), nil, "--git-dir="+dir, "cat-file", "--batch-check")
	if err != nil {
		return "", err
	}
	if err := res.must("cat-file"); err != nil {
		return "", err
	}
	f := strings.Fields(string(res.Stdout))
	if len(f) < 2 || f[0] != oid {
		return "", fmt.Errorf("%w: cat-file answered %q", errGitFailed, truncateText(string(res.Stdout), 128))
	}
	if f[1] == "missing" {
		return "", nil
	}
	return f[1], nil
}

func (s *Service) requireCommit(ctx context.Context, dir, oid string) error {
	t, err := s.objectType(ctx, dir, oid)
	if err != nil {
		return err
	}
	if t != "commit" {
		return contractError(gitv1.CodeCommitNotFound, "the repository has no commit %s", oid)
	}
	return nil
}

// isAncestor runs merge-base --is-ancestor: exit 0 and 1 are the answers, any
// other status is a failure.
func (s *Service) isAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	res, err := s.runGit(ctx, dir, false, objectEnv, "merge-base", "--is-ancestor", ancestor, descendant)
	if err != nil {
		return false, err
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, res.must("merge-base")
}

func (s *Service) isAncestorRPC(ctx context.Context, pathID string, req gitv1.IsAncestorRequest) (gitv1.IsAncestorResponse, error) {
	dir, err := s.rpcRepo(pathID, req.RepositoryID)
	if err != nil {
		return gitv1.IsAncestorResponse{}, err
	}
	for _, c := range []string{req.Ancestor, req.Descendant} {
		if err := s.requireCommit(ctx, dir, c); err != nil {
			return gitv1.IsAncestorResponse{}, err
		}
	}
	ok, err := s.isAncestor(ctx, dir, req.Ancestor, req.Descendant)
	if err != nil {
		return gitv1.IsAncestorResponse{}, err
	}
	return gitv1.IsAncestorResponse{IsAncestor: ok}, nil
}

func (s *Service) hasCommitRPC(ctx context.Context, pathID string, req gitv1.HasCommitRequest) (gitv1.HasCommitResponse, error) {
	dir, err := s.rpcRepo(pathID, req.RepositoryID)
	if err != nil {
		return gitv1.HasCommitResponse{}, err
	}
	t, err := s.objectType(ctx, dir, req.Commit)
	if err != nil {
		return gitv1.HasCommitResponse{}, err
	}
	return gitv1.HasCommitResponse{Present: t == "commit"}, nil
}

// Committer times outside this range are clamped: a commit pushed with a
// timestamp far in the future stays "not old" and one far in the past stays
// "old", which is the safe direction for age-based pruning, and the answer is
// always a valid contract timestamp.
var (
	minCommitTime = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	maxCommitTime = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
)

func (s *Service) commitTimeRPC(ctx context.Context, pathID string, req gitv1.CommitTimeRequest) (gitv1.CommitTimeResponse, error) {
	dir, err := s.rpcRepo(pathID, req.RepositoryID)
	if err != nil {
		return gitv1.CommitTimeResponse{}, err
	}
	if err := s.requireCommit(ctx, dir, req.Commit); err != nil {
		return gitv1.CommitTimeResponse{}, err
	}
	res, err := s.runGit(ctx, dir, false, objectEnv, "show", "-s", "--format=%ct", "--end-of-options", req.Commit)
	if err != nil {
		return gitv1.CommitTimeResponse{}, err
	}
	if err := res.must("show"); err != nil {
		return gitv1.CommitTimeResponse{}, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(res.Stdout)), 10, 64)
	if err != nil {
		return gitv1.CommitTimeResponse{}, fmt.Errorf("%w: commit time %q", errGitFailed, truncateText(string(res.Stdout), 64))
	}
	t := time.Unix(n, 0).UTC()
	switch {
	case n < minCommitTime.Unix():
		t = minCommitTime
	case n > maxCommitTime.Unix():
		t = maxCommitTime
	}
	return gitv1.CommitTimeResponse{CommittedAt: gitv1.Timestamp(t.Format("2006-01-02T15:04:05Z"))}, nil
}

// ---- bundles: shared plumbing ----

// parseBundleHeads parses `git bundle list-heads` ("<object> <ref>" lines),
// ignoring HEAD, and returns the tips sorted by ref. A ref or object outside the
// contract's syntax, or outside the namespaces a bundle may carry, is an error.
func parseBundleHeads(out []byte) ([]gitv1.RefTip, error) {
	var tips []gitv1.RefTip
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, ref, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("unexpected bundle head line %q", truncateText(line, 128))
		}
		if ref == "HEAD" {
			continue
		}
		if gitv1.ValidateObjectID(id) != nil || gitv1.IsZeroObjectID(id) || gitv1.ValidateRefName(ref) != nil || !inBundleNamespace(ref) {
			return nil, fmt.Errorf("the bundle names %q, which a bundle may not carry", truncateText(ref, 128))
		}
		tips = append(tips, gitv1.RefTip{Ref: ref, Commit: id})
	}
	sort.Slice(tips, func(i, j int) bool { return tips[i].Ref < tips[j].Ref })
	for i := 1; i < len(tips); i++ {
		if tips[i].Ref == tips[i-1].Ref {
			return nil, fmt.Errorf("the bundle names %q twice", tips[i].Ref)
		}
	}
	return tips, nil
}

// acquireBundleSlot bounds the concurrent bundle requests; the caller answers
// 429 when it is false.
func (s *Service) acquireBundleSlot() (release func(), ok bool) {
	select {
	case s.bundleSlots <- struct{}{}:
		return func() { <-s.bundleSlots }, true
	default:
		return nil, false
	}
}

func writeBundleBusy(w http.ResponseWriter, limit int) {
	w.Header().Set("Retry-After", "5")
	writeContractError(w, contractError(domain.CodeRateLimited, "the Git worker is serving its maximum of %d concurrent bundle requests", limit), "")
}

// readRPCRequest reads, bounds and strictly decodes a JSON request body.
func readRPCRequest[Req any, P interface {
	*Req
	Validate() error
}](s *Service, w http.ResponseWriter, r *http.Request) (Req, bool) {
	var zero Req
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRPCBody))
	if err != nil {
		writeContractError(w, contractError(domain.CodeInvalidRequest, "the request body is unreadable or too large"), "")
		return zero, false
	}
	req, err := gitv1.DecodeRequest[Req, P](data)
	if err != nil {
		s.writeError(w, err, "")
		return zero, false
	}
	return req, true
}

// ---- CreateBundle ----

// bundleFile is a bundle on the worker's temporary storage; cleanup removes it.
type bundleFile struct {
	manifest gitv1.CreateBundleResponse
	path     string
	cleanup  func()
}

// bundleCreateHandler serves POST .../bundles/create: the answer is one JSON
// line (the manifest) followed by the bundle bytes (gitv1.BundleMediaType).
func (s *Service) bundleCreateHandler(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateControl(w, r) {
		return
	}
	req, ok := readRPCRequest[gitv1.CreateBundleRequest](s, w, r)
	if !ok {
		return
	}
	releaseSlot, ok := s.acquireBundleSlot()
	if !ok {
		writeBundleBusy(w, s.cfg.Limits.MaxConcurrentBundles)
		return
	}
	defer releaseSlot()
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Limits.BundleTimeout)
	defer cancel()
	b, err := s.createBundle(ctx, r.PathValue("id"), req)
	if err != nil {
		s.writeError(w, err, "")
		return
	}
	defer b.cleanup()
	line, err := gitv1.FrameLine(b.manifest)
	if err != nil {
		s.writeError(w, err, "")
		return
	}
	f, err := os.Open(b.path)
	if err != nil {
		s.writeError(w, err, "")
		return
	}
	defer f.Close()
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(s.cfg.Limits.BundleTimeout))
	w.Header().Set("Content-Type", gitv1.BundleMediaType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(int64(len(line))+b.manifest.Size, 10))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(line); err != nil {
		return
	}
	if _, err := io.Copy(w, f); err != nil {
		panic(http.ErrAbortHandler) // a short body must never look like a complete bundle
	}
}

// createBundle creates the bundle under the repository's shared gate and returns
// its manifest. The gate is released before the bytes are streamed.
//
// The refs and the journal head are captured while no receive or server
// operation can be admitted (the gate's mutex is held); Consistent is true only
// when none was running then, none was admitted until the bundle was complete,
// no killed Git process left the repository fenced, and the bundle holds exactly
// the captured refs. The manifest's refs are read from the bundle file itself.
func (s *Service) createBundle(ctx context.Context, pathID string, req gitv1.CreateBundleRequest) (*bundleFile, error) {
	dir, err := s.rpcRepo(pathID, req.RepositoryID)
	if err != nil {
		return nil, err
	}
	var want map[string]bool
	if req.Refs != nil {
		want = make(map[string]bool, len(req.Refs))
		for _, ref := range req.Refs {
			if !inBundleNamespace(ref) {
				return nil, contractError(domain.CodeInvalidRequest, "a bundle carries only refs/heads/, refs/tags/ and refs/kodama/ refs")
			}
			want[ref] = true
		}
	}
	g := s.gates.of(req.RepositoryID)
	gctx, cancelGate := context.WithTimeout(ctx, s.cfg.Limits.GateWait)
	release, err := g.acquireSharedKind(gctx, sharedObserve)
	cancelGate()
	if err != nil {
		if _, ok := asContract(err); ok {
			return nil, err
		}
		return nil, contractError(domain.CodeUnavailable, "the repository is under maintenance; retry shortly")
	}
	defer release()

	// Registering as an observer excluded maintenance and the replay; this
	// snapshot records whether any receive or operation was running and the
	// admission epoch. It performs no I/O under the gate's mutex. Everything read
	// below is one exact boundary only if nothing was running now and nothing is
	// admitted before the end (epochNow == epoch): any mutation that could change
	// a ref or the journal either was running (not quiescent) or bumps the epoch.
	var (
		quiescent bool
		epoch     uint64
	)
	_ = g.observe(func(q bool, ep uint64) error {
		quiescent, epoch = q, ep
		return nil
	})
	head, err := journalAt(dir).head()
	if err != nil {
		return nil, s.journalError(err)
	}
	captured := map[string]string{}
	tooMany := false
	badRef := ""
	for _, prefix := range bundleNamespaces {
		err := s.forEachRef(ctx, dir, prefix, func(name, oid string) bool {
			if want != nil && !want[name] {
				return false
			}
			if gitv1.ValidateRefName(name) != nil || gitv1.ValidateObjectID(oid) != nil || gitv1.IsZeroObjectID(oid) {
				// A backup must never silently lose a ref: refuse the bundle.
				badRef = name
				return true
			}
			captured[name] = oid
			if len(captured) > gitv1.MaxBundleRefs {
				tooMany = true
				return true
			}
			return false
		})
		if err != nil {
			return nil, err
		}
		if badRef != "" || tooMany {
			break
		}
	}
	if badRef != "" {
		return nil, contractError(domain.CodeInvalidRequest, "the repository holds a ref outside the contract's ref syntax, which a bundle cannot carry: %s", truncateText(badRef, 128))
	}
	if tooMany {
		return nil, contractError(gitv1.CodeBundleTooLarge, "the repository has more than %d refs", gitv1.MaxBundleRefs)
	}
	if want == nil && len(captured) == 0 {
		return nil, contractError(gitv1.CodeRepositoryEmpty, "the repository has no refs to bundle")
	}
	for ref := range want {
		if _, ok := captured[ref]; !ok {
			return nil, contractError(domain.CodeNotFound, "the ref %s was not found", ref)
		}
	}
	if hook := s.bundleHook.Load(); hook != nil {
		(*hook)(ctx, req.RepositoryID)
	}
	names := make([]string, 0, len(captured))
	for ref := range captured {
		names = append(names, ref)
	}
	sort.Strings(names)

	tmp, err := os.MkdirTemp(s.layout.tmp, "bundle-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	ok := false
	defer func() {
		if !ok {
			cleanup()
		}
	}()
	path := filepath.Join(tmp, "repository.bundle")
	if err := s.writeBundle(ctx, dir, path, names); err != nil {
		return nil, err
	}
	size, sum, err := fileDigest(path)
	if err != nil {
		return nil, err
	}
	res, err := s.runGit(ctx, dir, false, objectEnv, "bundle", "list-heads", path)
	if err != nil {
		return nil, err
	}
	if err := res.must("bundle list-heads"); err != nil {
		return nil, err
	}
	tips, err := parseBundleHeads(res.Stdout)
	if err != nil {
		return nil, err
	}
	consistent := quiescent && g.epochNow() == epoch && !g.isFenced() && len(tips) == len(captured)
	for _, t := range tips {
		if captured[t.Ref] != t.Commit {
			consistent = false
		}
	}
	release() // the bytes stream without the gate: a slow reader must not hold maintenance off
	manifest := gitv1.CreateBundleResponse{
		RepositoryID: req.RepositoryID, Refs: tips, JournalHead: head, Consistent: consistent,
		Size: size, SHA256: sum, WorkerInstance: s.instance,
	}
	if err := manifest.Validate(); err != nil {
		return nil, fmt.Errorf("the bundle manifest is invalid: %w", err)
	}
	ok = true
	return &bundleFile{manifest: manifest, path: path, cleanup: cleanup}, nil
}

// writeBundle runs `git bundle create` for the named refs, which Git writes
// through path+".lock". It cannot bound its output, so the size of both files is
// watched and Git is stopped (bundle_too_large) at the installation's limit.
func (s *Service) writeBundle(ctx context.Context, dir, path string, refs []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var tooLarge atomic.Bool
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(25 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				for _, p := range []string{path, path + ".lock"} {
					if st, err := os.Stat(p); err == nil && st.Size() > s.cfg.Limits.MaxBundleBytes {
						tooLarge.Store(true)
						cancel()
						return
					}
				}
			}
		}
	}()
	stdin := strings.NewReader(strings.Join(refs, "\n") + "\n")
	res, err := s.execGitIO(ctx, dir, "", false, objectEnv, stdin, nil, "--git-dir="+dir, "bundle", "create", path, "--stdin")
	close(done)
	<-finished
	if st, serr := os.Stat(path); serr == nil && st.Size() > s.cfg.Limits.MaxBundleBytes {
		tooLarge.Store(true)
	}
	if tooLarge.Load() {
		return contractError(gitv1.CodeBundleTooLarge, "the bundle exceeds %d bytes", s.cfg.Limits.MaxBundleBytes)
	}
	if err != nil {
		return err
	}
	return res.must("bundle create")
}

func fileDigest(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// ---- ReplayBundle ----

// bundleReplayHandler serves POST .../bundles/replay: the body is one JSON line
// (gitv1.ReplayBundleRequest) followed by the bundle bytes.
func (s *Service) bundleReplayHandler(w http.ResponseWriter, r *http.Request) {
	if !s.authenticateControl(w, r) {
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != gitv1.BundleMediaType {
		writeContractError(w, contractError(domain.CodeInvalidRequest, "the body must be %s", gitv1.BundleMediaType), "")
		return
	}
	releaseSlot, ok := s.acquireBundleSlot()
	if !ok {
		writeBundleBusy(w, s.cfg.Limits.MaxConcurrentBundles)
		return
	}
	defer releaseSlot()
	limit := s.cfg.Limits.MaxBundleBytes
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(s.cfg.Limits.BundleTimeout))
	body := bufio.NewReaderSize(http.MaxBytesReader(w, r.Body, limit+gitv1.MaxFrameLineBytes+1), 64<<10)
	line, err := gitv1.ReadFrameLine(body)
	if err != nil {
		writeContractError(w, contractError(domain.CodeInvalidRequest, "the body does not start with the request line"), "")
		return
	}
	req, err := gitv1.DecodeRequest[gitv1.ReplayBundleRequest](line)
	if err != nil {
		s.writeError(w, err, "")
		return
	}
	pathID := r.PathValue("id")
	dir, err := s.rpcRepo(pathID, req.RepositoryID)
	if err != nil {
		s.writeError(w, err, "")
		return
	}
	// A replay outside the restore fence is refused before the bundle is read;
	// the check is repeated under the exclusive gate.
	if !s.gates.of(req.RepositoryID).isRestoring() {
		s.writeError(w, errRestoreRequired, "")
		return
	}
	tmp, err := os.MkdirTemp(s.layout.tmp, "replay-")
	if err != nil {
		s.writeError(w, err, "")
		return
	}
	defer os.RemoveAll(tmp)
	path := filepath.Join(tmp, "repository.bundle")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		s.writeError(w, err, "")
		return
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case n > limit:
		s.writeError(w, contractError(gitv1.CodeBundleTooLarge, "the bundle exceeds %d bytes", limit), "")
		return
	case err != nil:
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.writeError(w, contractError(gitv1.CodeBundleTooLarge, "the bundle exceeds %d bytes", limit), "")
			return
		}
		s.writeError(w, contractError(domain.CodeInvalidRequest, "the bundle could not be read"), "")
		return
	case n == 0:
		s.writeError(w, contractError(domain.CodeInvalidRequest, "the bundle is empty"), "")
		return
	}
	if req.SHA256 != nil && *req.SHA256 != hex.EncodeToString(h.Sum(nil)) {
		s.writeError(w, contractError(domain.CodeInvalidRequest, "the bundle does not match sha256"), "")
		return
	}
	// From here the replay runs to its own end whatever the client does.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.cfg.Limits.BundleTimeout)
	defer cancel()
	resp, err := s.replayBundle(ctx, dir, req, path)
	if err != nil {
		s.writeError(w, err, "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// replayBundle replays the bundle at path into the repository, exclusively and
// only while the restore fence is raised. It stores the bundle's objects (no ref
// is touched), checks they are complete, and creates the refs that do not exist
// through the worker's own hook-compatible ref path, so each creation is
// journaled under the operation ID exactly like a server-side ref operation. A
// ref that exists at another commit is never changed: it is reported with how it
// relates to the bundle's value. No ref is advanced, rewound or deleted.
//
// Everything before the first ref is created is a definite refusal (the objects
// stored so far are unreferenced and harmless). From the first creation on, any
// failure is an unknown outcome (the response is aborted): the caller repeats
// the identical request, which finds the created refs unchanged, settles an
// interrupted journal entry and creates the rest.
func (s *Service) replayBundle(ctx context.Context, dir string, req gitv1.ReplayBundleRequest, path string) (gitv1.ReplayBundleResponse, error) {
	g := s.gates.of(req.RepositoryID)
	gctx, cancelGate := context.WithTimeout(ctx, s.cfg.Limits.GateWait)
	release, err := g.acquireReplay(gctx)
	cancelGate()
	if err != nil {
		if _, ok := asContract(err); ok {
			return gitv1.ReplayBundleResponse{}, err
		}
		return gitv1.ReplayBundleResponse{}, contractError(domain.CodeUnavailable, "the repository is busy; retry shortly")
	}
	defer release()
	if err := g.checkFence(); err != nil {
		return gitv1.ReplayBundleResponse{}, err
	}

	res, err := s.runGit(ctx, dir, false, objectEnv, "bundle", "list-heads", path)
	if err != nil {
		return gitv1.ReplayBundleResponse{}, err
	}
	if res.ExitCode != 0 {
		return gitv1.ReplayBundleResponse{}, contractError(domain.CodeInvalidRequest, "the body is not a Git bundle: %s", truncateText(res.Stderr, 200))
	}
	tips, err := parseBundleHeads(res.Stdout)
	if err != nil {
		return gitv1.ReplayBundleResponse{}, contractError(domain.CodeInvalidRequest, "%s", truncateText(err.Error(), 300))
	}
	switch {
	case len(tips) == 0:
		return gitv1.ReplayBundleResponse{}, contractError(domain.CodeInvalidRequest, "the bundle holds no refs")
	case len(tips) > gitv1.MaxBundleRefs:
		return gitv1.ReplayBundleResponse{}, contractError(gitv1.CodeBundleTooLarge, "the bundle holds more than %d refs", gitv1.MaxBundleRefs)
	}

	// Objects first. The pack is stored with index-pack --strict (the object
	// checks of a receive; `git bundle unbundle` does not apply them) and no ref
	// is updated. A missing prerequisite or a damaged or malformed pack is a
	// definite refusal.
	if err := s.storeBundleObjects(ctx, dir, req.RepositoryID, path); err != nil {
		return gitv1.ReplayBundleResponse{}, err
	}

	current := make(map[string]string, len(tips))
	var check []string
	for _, t := range tips {
		cur, err := s.readRef(ctx, dir, t.Ref)
		if err != nil {
			return gitv1.ReplayBundleResponse{}, err
		}
		current[t.Ref] = cur
		if cur != t.Commit {
			check = append(check, t.Commit)
		}
	}
	if len(check) > 0 {
		// The tips that will be referenced (or compared) must be complete down to
		// the roots: an unbundled pack proves only its own objects.
		res, err := s.execGitIO(ctx, dir, "", false, objectEnv, strings.NewReader(strings.Join(check, "\n")+"\n"), nil,
			"--git-dir="+dir, "rev-list", "--objects", "--quiet", "--stdin")
		if err != nil {
			return gitv1.ReplayBundleResponse{}, err
		}
		if res.ExitCode != 0 {
			return gitv1.ReplayBundleResponse{}, contractError(domain.CodeInvalidRequest, "the bundle's objects are incomplete: %s", truncateText(res.Stderr, 200))
		}
	}

	// Deterministic refusals come before the first ref is created: past that
	// point a failure is an unknown outcome that every retry would repeat.
	var create []gitv1.RefTip
	for _, t := range tips {
		if current[t.Ref] == "" {
			create = append(create, t)
		}
	}
	if len(create) > 0 {
		if err := s.checkCreations(ctx, dir, tips, create); err != nil {
			return gitv1.ReplayBundleResponse{}, err
		}
	}

	resp := gitv1.ReplayBundleResponse{OperationID: req.OperationID, Conflicting: []gitv1.BundleConflict{}, WorkerInstance: s.instance}
	for _, t := range tips {
		cur := current[t.Ref]
		u := gitv1.RefUpdate{Ref: t.Ref, Old: gitv1.ZeroObjectID(len(t.Commit)), New: t.Commit}
		switch {
		case cur == t.Commit:
			// An earlier attempt may have created it and died before it closed its
			// journal entry.
			if err := s.settleOperation(dir, req.OperationID, u, true); err != nil {
				return gitv1.ReplayBundleResponse{}, unknownOutcome(s.journalError(err))
			}
			resp.Unchanged++
		case cur == "":
			g.opMu.Lock()
			created, err := s.createRefHeld(ctx, dir, req.RepositoryID, req.OperationID, t.Ref, t.Commit, gitv1.CodeRefChanged, true)
			g.opMu.Unlock()
			if err != nil {
				return gitv1.ReplayBundleResponse{}, unknownOutcome(err)
			}
			if created {
				resp.Created++
			} else {
				resp.Unchanged++ // converged: the ref already had the commit
			}
		default:
			rel, err := s.bundleRelation(ctx, dir, cur, t.Commit)
			if err != nil {
				return gitv1.ReplayBundleResponse{}, unknownOutcome(err)
			}
			resp.Conflicting = append(resp.Conflicting, gitv1.BundleConflict{Ref: t.Ref, Current: cur, Bundle: t.Commit, Relation: rel})
		}
	}
	return resp, nil
}

// storeBundleObjects stores the bundle's pack: it checks that every
// prerequisite commit exists, then feeds the pack that follows the header to
// `git index-pack --stdin --fix-thin --strict`.
func (s *Service) storeBundleObjects(ctx context.Context, dir, repositoryID, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	var prereqs []string
	for n := 0; ; n++ {
		line, err := br.ReadString('\n')
		if err != nil || len(line) > 1<<16 || n > 2*gitv1.MaxBundleRefs+1000 {
			return contractError(domain.CodeInvalidRequest, "the body is not a Git bundle")
		}
		line = strings.TrimSuffix(line, "\n")
		if n == 0 {
			if line != "# v2 git bundle" && line != "# v3 git bundle" {
				return contractError(domain.CodeInvalidRequest, "the body is not a Git bundle")
			}
			continue
		}
		if line == "" {
			break // the pack follows
		}
		if strings.HasPrefix(line, "-") {
			id, _, _ := strings.Cut(line[1:], " ")
			if gitv1.ValidateObjectID(id) != nil {
				return contractError(domain.CodeInvalidRequest, "the bundle has a malformed prerequisite")
			}
			prereqs = append(prereqs, id)
		}
	}
	if len(prereqs) > 0 {
		types, err := s.objectTypes(ctx, dir, prereqs)
		if err != nil {
			return err
		}
		for _, id := range prereqs {
			if types[id] != "commit" {
				return contractError(domain.CodeInvalidRequest, "the bundle needs the commit %s, which the repository lacks", id)
			}
		}
	}
	res, err := s.execGitIO(ctx, dir, repositoryID, false, objectEnv, br, nil, "--git-dir="+dir, "index-pack", "--stdin", "--fix-thin", "--strict")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return contractError(domain.CodeInvalidRequest, "the bundle's objects were refused: %s", truncateText(res.Stderr, 200))
	}
	return nil
}

// objectTypes returns the type of each object in one call ("" for a missing one).
func (s *Service) objectTypes(ctx context.Context, dir string, oids []string) (map[string]string, error) {
	res, err := s.execGitIO(ctx, dir, "", false, objectEnv, strings.NewReader(strings.Join(oids, "\n")+"\n"), nil, "--git-dir="+dir, "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	if err := res.must("cat-file"); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
	if len(lines) != len(oids) {
		return nil, fmt.Errorf("%w: cat-file answered %d lines for %d objects", errGitFailed, len(lines), len(oids))
	}
	types := make(map[string]string, len(oids))
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != oids[i] {
			return nil, fmt.Errorf("%w: cat-file answered %q", errGitFailed, truncateText(line, 128))
		}
		if f[1] != "missing" {
			types[oids[i]] = f[1]
		}
	}
	return types, nil
}

// checkCreations refuses, with a definite invalid_request and before any ref
// exists, the creations that Git would refuse every time: a head or hidden ref
// whose tip is not a commit, and a ref that collides with another as a
// directory and a file (refs/heads/a against refs/heads/a/b), whether the other
// is in the repository or among the bundle's refs. The operator removes the
// colliding ref (or restores into a fresh repository) and repeats the replay.
func (s *Service) checkCreations(ctx context.Context, dir string, tips, create []gitv1.RefTip) error {
	oids := make([]string, 0, len(create))
	seen := map[string]bool{}
	for _, t := range create {
		if !seen[t.Commit] {
			seen[t.Commit] = true
			oids = append(oids, t.Commit)
		}
	}
	types, err := s.objectTypes(ctx, dir, oids)
	if err != nil {
		return err
	}
	for _, t := range create {
		if types[t.Commit] == "" {
			return contractError(domain.CodeInvalidRequest, "the bundle's object %s for %s is missing", t.Commit, truncateText(t.Ref, 128))
		}
		if !strings.HasPrefix(t.Ref, "refs/tags/") && types[t.Commit] != "commit" {
			return contractError(domain.CodeInvalidRequest, "%s would point to a %s, not a commit", truncateText(t.Ref, 128), types[t.Commit])
		}
	}
	names := map[string]bool{}
	for _, prefix := range bundleNamespaces {
		if err := s.forEachRef(ctx, dir, prefix, func(name, _ string) bool { names[name] = true; return false }); err != nil {
			return err
		}
	}
	for _, t := range tips {
		names[t.Ref] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, t := range create {
		for i := 0; i < len(t.Ref); i++ {
			if t.Ref[i] == '/' && names[t.Ref[:i]] {
				return contractError(domain.CodeInvalidRequest, "%s collides with the ref %s", truncateText(t.Ref, 128), truncateText(t.Ref[:i], 128))
			}
		}
		prefix := t.Ref + "/"
		if i := sort.SearchStrings(sorted, prefix); i < len(sorted) && strings.HasPrefix(sorted[i], prefix) {
			return contractError(domain.CodeInvalidRequest, "%s collides with the ref %s", truncateText(t.Ref, 128), truncateText(sorted[i], 128))
		}
	}
	return nil
}

// bundleRelation says how the repository's value of a ref relates to the
// bundle's. A pair Git cannot order (a tag that does not point at a commit, for
// example) is diverged.
func (s *Service) bundleRelation(ctx context.Context, dir, current, bundle string) (gitv1.BundleRelation, error) {
	for _, c := range []struct {
		ancestor, descendant string
		rel                  gitv1.BundleRelation
	}{
		{bundle, current, gitv1.RelationRepositoryAhead},
		{current, bundle, gitv1.RelationBundleAhead},
	} {
		res, err := s.runGit(ctx, dir, false, objectEnv, "merge-base", "--is-ancestor", c.ancestor, c.descendant)
		if err != nil {
			return "", err
		}
		if res.ExitCode == 0 {
			return c.rel, nil
		}
	}
	return gitv1.RelationDiverged, nil
}
