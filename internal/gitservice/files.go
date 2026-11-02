package gitservice

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Pipeline-definition reads (T-168). The control plane indexes
// .kodama/<processname>.yaml at the exact commit an applied push left on the
// default branch without mounting the tenant volume (GR-13, GR-18). The worker
// answers from the object database only: Git runs with the sanitized
// environment, the hooks disabled and replacement objects ignored, so a pushed
// refs/replace/* cannot change a file the control plane then trusts.

// The bounds, the directory and the DTOs are the Git contract's
// (gitv1.ReadFilesRequest and gitv1.ReadFilesResponse).
const (
	maxReadFilePaths  = gitv1.MaxReadFilePaths
	maxReadFileBytes  = gitv1.MaxReadFileBytes
	maxReadFilesBytes = gitv1.MaxReadFilesBytes
	definitionDir     = gitv1.DefinitionDir
)

var definitionPath = regexp.MustCompile(gitv1.Patterns["DefinitionPath"])

// treeEntry is one blob of a directory listing.
type treeEntry struct {
	path string
	size int64
}

// listDefinitionDir lists the blobs directly in .kodama/ that end in .yaml or
// .yml, sorted by path. A commit without the directory lists nothing.
func (s *Service) listDefinitionDir(ctx context.Context, dir, commit string) ([]string, error) {
	res, err := s.runGit(ctx, dir, false, objectEnv, "ls-tree", "-z", "--format=%(objecttype) %(path)", commit, definitionDir)
	if err != nil {
		return nil, err
	}
	if err := res.must("ls-tree"); err != nil {
		return nil, err
	}
	var out []string
	for _, rec := range strings.Split(string(res.Stdout), "\x00") {
		typ, path, ok := strings.Cut(rec, " ")
		if !ok || typ != "blob" || !definitionPath.MatchString(path) || strings.Contains(strings.TrimPrefix(path, definitionDir), "/") {
			continue
		}
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

// blobSizes asks Git for the type and size of commit:path for each path, in one
// process. A missing path, a directory or a submodule is absent from the answer.
func (s *Service) blobSizes(ctx context.Context, dir, commit string, paths []string) ([]treeEntry, error) {
	var in strings.Builder
	for _, p := range paths {
		in.WriteString(commit + ":" + p + "\n")
	}
	res, err := s.execGitIO(ctx, dir, "", false, objectEnv, strings.NewReader(in.String()), nil, "--git-dir="+dir, "cat-file", "--batch-check")
	if err != nil {
		return nil, err
	}
	if err := res.must("cat-file"); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n")
	if len(lines) != len(paths) {
		return nil, fmt.Errorf("%w: cat-file answered %d lines for %d paths", errGitFailed, len(lines), len(paths))
	}
	var out []treeEntry
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) != 3 || f[1] != "blob" {
			continue // "<spec> missing", a tree or anything that is not a file
		}
		n, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%w: cat-file size %q", errGitFailed, truncateText(f[2], 32))
		}
		out = append(out, treeEntry{path: paths[i], size: n})
	}
	return out, nil
}

func (s *Service) readFilesRPC(ctx context.Context, pathID string, req gitv1.ReadFilesRequest) (gitv1.ReadFilesResponse, error) {
	dir, err := s.rpcRepo(pathID, req.RepositoryID)
	if err != nil {
		return gitv1.ReadFilesResponse{}, err
	}
	if err := s.requireCommit(ctx, dir, req.Commit); err != nil {
		return gitv1.ReadFilesResponse{}, err
	}
	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, p := range req.Paths {
		if p != definitionDir {
			add(p)
			continue
		}
		listed, err := s.listDefinitionDir(ctx, dir, req.Commit)
		if err != nil {
			return gitv1.ReadFilesResponse{}, err
		}
		for _, l := range listed {
			add(l)
		}
	}
	if len(paths) > maxReadFilePaths {
		return gitv1.ReadFilesResponse{}, contractError(domain.CodeInvalidRequest, "the request names %d files; at most %d are read at once", len(paths), maxReadFilePaths)
	}
	sort.Strings(paths)
	resp := gitv1.ReadFilesResponse{Files: []gitv1.DefinitionFile{}}
	if len(paths) == 0 {
		return resp, nil
	}
	blobs, err := s.blobSizes(ctx, dir, req.Commit, paths)
	if err != nil {
		return gitv1.ReadFilesResponse{}, err
	}
	var total int64
	for _, b := range blobs {
		if b.size > maxReadFileBytes {
			return gitv1.ReadFilesResponse{}, contractError(domain.CodeInvalidRequest, "%s is %d bytes; a file larger than %d bytes is not read", b.path, b.size, maxReadFileBytes)
		}
		if total += b.size; total > maxReadFilesBytes {
			return gitv1.ReadFilesResponse{}, contractError(domain.CodeInvalidRequest, "the files exceed %d bytes together", maxReadFilesBytes)
		}
	}
	if len(blobs) == 0 {
		return resp, nil
	}
	var in strings.Builder
	for _, b := range blobs {
		in.WriteString(req.Commit + ":" + b.path + "\n")
	}
	var out cappedBuffer
	out.limit = total + int64(len(blobs)*(2*len(req.Commit)+len(definitionDir)+128))
	res, err := s.execGitIO(ctx, dir, "", false, objectEnv, strings.NewReader(in.String()), &out, "--git-dir="+dir, "cat-file", "--batch")
	if err != nil {
		return gitv1.ReadFilesResponse{}, err
	}
	if err := res.must("cat-file"); err != nil {
		return gitv1.ReadFilesResponse{}, err
	}
	r := bufio.NewReader(strings.NewReader(out.String()))
	for _, b := range blobs {
		header, err := r.ReadString('\n')
		if err != nil {
			return gitv1.ReadFilesResponse{}, fmt.Errorf("%w: cat-file output ended early", errGitFailed)
		}
		f := strings.Fields(header)
		if len(f) != 3 || f[1] != "blob" || f[2] != strconv.FormatInt(b.size, 10) {
			return gitv1.ReadFilesResponse{}, fmt.Errorf("%w: cat-file header %q", errGitFailed, truncateText(header, 128))
		}
		content := make([]byte, b.size)
		if _, err := io.ReadFull(r, content); err != nil {
			return gitv1.ReadFilesResponse{}, fmt.Errorf("%w: cat-file content ended early", errGitFailed)
		}
		if nl, err := r.ReadByte(); err != nil || nl != '\n' {
			return gitv1.ReadFilesResponse{}, fmt.Errorf("%w: cat-file framing", errGitFailed)
		}
		if !utf8.Valid(content) {
			return gitv1.ReadFilesResponse{}, contractError(domain.CodeInvalidRequest, "%s is not UTF-8 text", b.path)
		}
		sum := sha256.Sum256(content)
		resp.Files = append(resp.Files, gitv1.DefinitionFile{Path: b.path, Content: string(content), SHA256: hex.EncodeToString(sum[:])})
	}
	return resp, nil
}

// cappedBuffer collects up to limit bytes and fails the write beyond it, so a
// misbehaving object database cannot grow the answer past the bounds checked.
type cappedBuffer struct {
	buf   strings.Builder
	limit int64
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if int64(c.buf.Len()+len(p)) > c.limit {
		return 0, fmt.Errorf("%w: cat-file produced more than the bounded size", errGitFailed)
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string { return c.buf.String() }
