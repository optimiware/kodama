package git

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/optimiware/kodama/internal/workspace"
)

const (
	maxHeaderBytes   = 64 << 10
	maxPrerequisites = 16
	// minSecretScanLen is the shortest secret value scanned for; shorter values
	// would match innocent content.
	minSecretScanLen = 8
	maxObjectList    = 16 << 20
)

// bundleHeader is the parsed header of a Git bundle (v2 or v3).
type bundleHeader struct {
	prerequisites []string
	refs          map[string]string // refname -> oid
	hashLen       int
}

// parseBundleHeader reads the header only, so a malformed or hostile bundle is
// rejected before Git expands any object.
func parseBundleHeader(path string) (*bundleHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rd := bufio.NewReaderSize(io.LimitReader(f, maxHeaderBytes), 4096)
	sig, err := rd.ReadString('\n')
	if err != nil {
		return nil, &workspace.BundleError{Reason: workspace.ReasonTruncated}
	}
	sig = strings.TrimSuffix(sig, "\n")
	h := &bundleHeader{refs: map[string]string{}, hashLen: 40}
	switch sig {
	case "# v2 git bundle":
	case "# v3 git bundle":
	default:
		return nil, &workspace.BundleError{Reason: workspace.ReasonUnsupportedBound}
	}
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, &workspace.BundleError{Reason: workspace.ReasonTruncated}
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			return h, nil
		case strings.HasPrefix(line, "@"):
			if sig != "# v3 git bundle" {
				return nil, &workspace.BundleError{Reason: workspace.ReasonUnsupportedBound}
			}
			k, v, _ := strings.Cut(line[1:], "=")
			switch {
			case k == "object-format" && v == "sha256":
				h.hashLen = 64
			case k == "object-format" && v == "sha1":
			default:
				return nil, &workspace.BundleError{Reason: workspace.ReasonUnsupportedBound}
			}
		case strings.HasPrefix(line, "-"):
			oid, _, _ := strings.Cut(line[1:], " ")
			if len(h.prerequisites) >= maxPrerequisites {
				return nil, &workspace.BundleError{Reason: workspace.ReasonBadPrerequisite}
			}
			h.prerequisites = append(h.prerequisites, oid)
		default:
			oid, name, ok := strings.Cut(line, " ")
			if !ok || name == "" || len(h.refs) >= 1 {
				return nil, &workspace.BundleError{Reason: workspace.ReasonCommitMismatch}
			}
			h.refs[name] = oid
		}
	}
}

// Prepare implements workspace.SourcePreparer. It verifies, in bounded private
// scratch and with read-only credentials: the transferred size and digest, the
// header (one ref at the proposed commit; prerequisites limited to the pinned
// input and expected tip), the bundle's object integrity, that the commit
// descends from the pinned input, and that no injected secret appears in the
// objects the bundle introduces.
func (r *Reader) Prepare(ctx context.Context, req workspace.PrepareRequest) (workspace.Prepared, error) {
	if err := validateRepo(req.Repo); err != nil {
		return nil, err
	}
	if err := workspace.ValidateBranch(req.Branch); err != nil {
		return nil, err
	}
	for _, c := range []workspace.Commit{req.Input, req.ExpectedTip, req.Commit} {
		if err := c.Validate(); err != nil {
			return nil, err
		}
	}
	if req.Body == nil || req.Digest == "" {
		return nil, &workspace.BundleError{Reason: workspace.ReasonTruncated}
	}
	if req.Size > r.e.o.MaxBundleBytes {
		return nil, fmt.Errorf("%w: bundle is %d bytes, over the %d byte transfer limit", workspace.ErrLimitExceeded, req.Size, r.e.o.MaxBundleBytes)
	}
	cred, err := r.e.credential(ctx, req.Repo, req.Repo.CheckoutSecretRef)
	if err != nil {
		return nil, err
	}
	sc, err := r.e.newScratch()
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			sc.cleanup()
		}
	}()

	// 1. Bounded transfer into scratch, hashing as it arrives.
	bundlePath := filepath.Join(sc.dir, "source.bundle")
	n, sum, err := copyBounded(bundlePath, req.Body, r.e.o.MaxBundleBytes)
	switch {
	case errors.Is(err, workspace.ErrLimitExceeded):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%w: reading the bundle: %w", workspace.ErrUnavailable, err)
	case req.Size > 0 && n < req.Size:
		return nil, &workspace.BundleError{Reason: workspace.ReasonTruncated}
	case (req.Size > 0 && n != req.Size) || sum != req.Digest:
		return nil, &workspace.BundleError{Reason: workspace.ReasonDigestMismatch}
	}

	// 2. Header checks before any object is expanded.
	h, err := parseBundleHeader(bundlePath)
	if err != nil {
		return nil, err
	}
	if h.hashLen != len(req.Commit) {
		return nil, &workspace.BundleError{Reason: workspace.ReasonUnsupportedBound}
	}
	var bundleRef string
	for name, oid := range h.refs {
		if oid != string(req.Commit) {
			return nil, &workspace.BundleError{Reason: workspace.ReasonCommitMismatch}
		}
		bundleRef = name
	}
	if bundleRef == "" {
		return nil, &workspace.BundleError{Reason: workspace.ReasonCommitMismatch}
	}
	for _, p := range h.prerequisites {
		if p != string(req.Input) && p != string(req.ExpectedTip) {
			return nil, &workspace.BundleError{Reason: workspace.ReasonBadPrerequisite}
		}
	}

	// 3. Pinned source and current tip from the remote (read credential).
	dir, err := r.e.initBare(ctx, sc, req.Commit)
	if err != nil {
		return nil, err
	}
	tip, exists, err := r.e.lsRemote(ctx, sc, req.Repo, cred, req.Branch)
	if err != nil {
		return nil, err
	}
	if exists {
		if err := r.e.fetchBranch(ctx, sc, dir, req.Repo, cred, req.Branch, 1); err != nil {
			return nil, err
		}
	}
	for _, want := range uniqueCommits(req.Input, req.ExpectedTip) {
		has, err := r.e.hasCommit(ctx, sc, dir, want)
		if err != nil {
			return nil, err
		}
		if has {
			continue
		}
		if err := r.e.fetchCommit(ctx, sc, dir, req.Repo, cred, want); err != nil {
			if want == req.Input {
				return nil, err
			}
			// The expected tip may be gone after a human rewrite; the
			// publication then conflicts instead of failing here.
			continue
		}
	}

	// 4. Verify and import under Git's object integrity checks.
	if _, err := r.e.run(ctx, cmdSpec{dir: dir, proto: "file", args: []string{"bundle", "verify", "--quiet", bundlePath}, watch: sc.dir}); err != nil {
		return nil, classifyBundleFailure(err)
	}
	if _, err := r.e.run(ctx, cmdSpec{dir: dir, proto: "file", watch: sc.dir,
		args: []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", bundlePath, bundleRef + ":refs/kodama/candidate"}}); err != nil {
		return nil, classifyBundleFailure(err)
	}
	got, err := r.e.revParse(ctx, sc, dir, "refs/kodama/candidate")
	if err != nil || got != req.Commit {
		return nil, &workspace.BundleError{Reason: workspace.ReasonCommitMismatch}
	}
	desc, err := r.e.isAncestor(ctx, sc, dir, req.Input, req.Commit)
	if err != nil {
		return nil, err
	}
	if !desc {
		return nil, &workspace.BundleError{Reason: workspace.ReasonNotDescendant}
	}

	// 5. Credentials never enter source: scan only the objects introduced.
	if err := r.e.scanNewObjects(ctx, sc, dir, req.Commit, req.Input, req.Needles); err != nil {
		return nil, err
	}
	ok = true
	if !exists {
		tip = ""
	}
	return &prepared{e: r.e, sc: sc, dir: dir, observed: tip, commit: req.Commit}, nil
}

func uniqueCommits(a, b workspace.Commit) []workspace.Commit {
	if a == b {
		return []workspace.Commit{a}
	}
	return []workspace.Commit{a, b}
}

// fetchCommit fetches one commit by object name (depth 1).
func (e *engine) fetchCommit(ctx context.Context, sc *scratch, dir string, repo workspace.RepoRef, cred *Credential, c workspace.Commit) error {
	_, err := e.run(ctx, cmdSpec{dir: dir, proto: "https", cred: cred, url: repo.URL, watch: sc.dir,
		args: []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--depth=1", repo.URL, string(c)}})
	return err
}

func copyBounded(path string, body io.Reader, max int64) (int64, string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, max+1))
	if err != nil {
		return n, "", err
	}
	if n > max {
		return n, "", fmt.Errorf("%w: bundle exceeds the %d byte transfer limit", workspace.ErrLimitExceeded, max)
	}
	return n, "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// classifyBundleFailure maps a failed verify/import to a safe reason. A
// scratch or deadline failure keeps its own class.
func classifyBundleFailure(err error) error {
	if errors.Is(err, workspace.ErrLimitExceeded) || errors.Is(err, workspace.ErrUnavailable) || errors.Is(err, context.Canceled) {
		return err
	}
	var ce *errCommand
	msg := ""
	if errors.As(err, &ce) {
		msg = ce.stderr
	}
	switch {
	case strings.Contains(msg, "prerequisite"):
		return &workspace.BundleError{Reason: workspace.ReasonBadPrerequisite, Cause: err}
	case strings.Contains(msg, "early EOF") || strings.Contains(msg, "unexpected EOF") || strings.Contains(msg, "truncated"):
		return &workspace.BundleError{Reason: workspace.ReasonTruncated, Cause: err}
	}
	return &workspace.BundleError{Reason: workspace.ReasonCorruptObjects, Cause: err}
}

// scanNewObjects reads every commit, tree and blob reachable from commit but
// not from input (intermediate commits and messages included) and refuses the
// bundle if any needle appears.
func (e *engine) scanNewObjects(ctx context.Context, sc *scratch, dir string, commit, input workspace.Commit, needles [][]byte) error {
	var usable [][]byte
	for _, n := range needles {
		if len(n) >= minSecretScanLen {
			usable = append(usable, n)
		}
	}
	listing, err := e.run(ctx, cmdSpec{dir: dir, proto: "file", maxOut: maxObjectList, watch: sc.dir,
		args: []string{"rev-list", "--objects", string(commit), "^" + string(input)}})
	if err != nil {
		return classifyBundleFailure(err)
	}
	var oids []string
	for _, line := range strings.Split(string(listing), "\n") {
		if oid, _, _ := strings.Cut(line, " "); oid != "" {
			oids = append(oids, oid)
		}
	}
	if len(usable) == 0 || len(oids) == 0 {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, e.o.CommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, e.o.Binary, append(append([]string{}, baseConfig...), "cat-file", "--batch")...)
	cmd.Dir = dir
	cmd.Env = e.env(cmdSpec{proto: "file"})
	isolate(cmd)
	cmd.Stdin = strings.NewReader(strings.Join(oids, "\n") + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	finish := func() { _, _ = io.Copy(io.Discard, stdout); _ = cmd.Wait() }
	rd := bufio.NewReaderSize(stdout, 64<<10)
	for range oids {
		header, err := rd.ReadString('\n')
		if err != nil {
			finish()
			return fmt.Errorf("%w: object scan: %v", workspace.ErrUnavailable, err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 {
			finish()
			return &workspace.BundleError{Reason: workspace.ReasonCorruptObjects}
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			finish()
			return &workspace.BundleError{Reason: workspace.ReasonCorruptObjects}
		}
		body := io.LimitReader(rd, size)
		found := containsAny(body, usable)
		if _, err := io.Copy(io.Discard, body); err != nil {
			finish()
			return err
		}
		if _, err := rd.Discard(1); err != nil {
			finish()
			return err
		}
		if found {
			finish()
			return workspace.ErrSecretInSource
		}
	}
	finish()
	return nil
}

// containsAny streams r looking for any needle, keeping an overlap window.
func containsAny(r io.Reader, needles [][]byte) bool {
	maxLen := 0
	for _, n := range needles {
		maxLen = max(maxLen, len(n))
	}
	buf := make([]byte, 0, 64<<10+maxLen)
	chunk := make([]byte, 64<<10)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for _, needle := range needles {
			if bytes.Contains(buf, needle) {
				return true
			}
		}
		if len(buf) > maxLen-1 {
			buf = append(buf[:0], buf[len(buf)-(maxLen-1):]...)
		}
		if err != nil {
			return false
		}
	}
}

// prepared is verified source in private scratch.
type prepared struct {
	e        *engine
	sc       *scratch
	dir      string
	observed workspace.Commit
	commit   workspace.Commit
}

func (p *prepared) ObservedTip() workspace.Commit { return p.observed }

func (p *prepared) DescendsFrom(ctx context.Context, c workspace.Commit) (bool, error) {
	if err := c.Validate(); err != nil {
		return false, err
	}
	has, err := p.e.hasCommit(ctx, p.sc, p.dir, c)
	if err != nil || !has {
		return false, err
	}
	return p.e.isAncestor(ctx, p.sc, p.dir, c, p.commit)
}

func (p *prepared) Release() { p.sc.cleanup() }
