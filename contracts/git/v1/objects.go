package gitv1

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
)

// Object-level worker operations (T-160): ancestry, commit existence and time,
// and bundles for backup and restore. They exist so the control plane never
// mounts the tenant volume (GR-13, GR-15). Every request names the repository
// by ID only; commits are non-zero lowercase hexadecimal object IDs.

// Bounds of the object operations.
const (
	// MaxBundleRefs bounds the refs one bundle names (a request's refs and a
	// manifest's refs); a repository with more refs is refused with
	// bundle_too_large rather than silently truncated.
	MaxBundleRefs = 10000
	// MaxFrameLineBytes bounds the JSON line that precedes a bundle in a request
	// or response body.
	MaxFrameLineBytes = 4 << 20

	// DefinitionDir is the one directory a file read may name (as a directory
	// request, the trailing slash included) or take files from.
	DefinitionDir = ".kodama/"
	// MaxReadFilePaths bounds the paths a ReadFiles request names and, for a
	// directory request, the files it expands to. MaxReadFileBytes bounds one file
	// and MaxReadFilesBytes the sum of a response. A request that would exceed a
	// bound is refused whole (invalid_request): nothing is truncated.
	MaxReadFilePaths  = 64
	MaxReadFileBytes  = 256 << 10
	MaxReadFilesBytes = 1 << 20
)

// BundleMediaType is the Content-Type of a body made of one JSON line (the
// manifest of a CreateBundle response, or the ReplayBundleRequest) terminated
// by a single "\n", followed by the raw bundle bytes. The body is not JSON as a
// whole: read the line with ReadFrameLine, then the bundle.
const BundleMediaType = "application/vnd.kodama.git-bundle-frame"

// BundleRelation says how a ref's current value relates to the bundle's value
// when the ref exists at another commit.
type BundleRelation string

// Bundle relations. The ref is never changed whichever applies.
const (
	// RelationRepositoryAhead: the bundle's commit is an ancestor of the
	// repository's (the repository is further than the bundle).
	RelationRepositoryAhead BundleRelation = "repository_ahead"
	// RelationBundleAhead: the repository's commit is an ancestor of the
	// bundle's (the bundle would fast-forward the ref).
	RelationBundleAhead BundleRelation = "bundle_ahead"
	// RelationDiverged: neither is an ancestor of the other.
	RelationDiverged BundleRelation = "diverged"
)

// IsAncestorRequest asks whether Ancestor is reachable from Descendant (a commit
// is its own ancestor). Both commits must exist: a missing one is the
// commit_not_found refusal, never false.
type IsAncestorRequest struct {
	RepositoryID string `json:"repositoryId"`
	Ancestor     string `json:"ancestor"`
	Descendant   string `json:"descendant"`
}

// IsAncestorResponse answers IsAncestorRequest.
type IsAncestorResponse struct {
	IsAncestor bool `json:"isAncestor"`
}

// HasCommitRequest asks whether the repository holds the commit object.
type HasCommitRequest struct {
	RepositoryID string `json:"repositoryId"`
	Commit       string `json:"commit"`
}

// HasCommitResponse answers HasCommitRequest. Present is false for a missing
// object and for an object that is not a commit.
type HasCommitResponse struct {
	Present bool `json:"present"`
}

// CommitTimeRequest asks for a commit's committer time. A missing commit is the
// commit_not_found refusal.
type CommitTimeRequest struct {
	RepositoryID string `json:"repositoryId"`
	Commit       string `json:"commit"`
}

// CommitTimeResponse carries the committer time, truncated to whole seconds, UTC.
type CommitTimeResponse struct {
	CommittedAt Timestamp `json:"committedAt"`
}

// ReadFilesRequest asks for pipeline definitions of one commit (T-168). Each path
// is either a file under .kodama/ ending in .yaml or .yml, or the directory
// ".kodama/", which stands for every such file directly in it (not recursive).
// Commit is a non-zero object ID, never a ref name. A missing commit is the
// commit_not_found refusal.
type ReadFilesRequest struct {
	RepositoryID string   `json:"repositoryId"`
	Commit       string   `json:"commit"`
	Paths        []string `json:"paths"`
}

// DefinitionFile is one file of the commit: its path, its content (UTF-8 text)
// and the hex SHA-256 of the content.
type DefinitionFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}

// ReadFilesResponse answers ReadFilesRequest. A requested file that does not exist
// at the commit, or is not a file, is absent. Files are sorted by path, unique,
// at most MaxReadFilePaths of them, MaxReadFileBytes each and MaxReadFilesBytes
// together.
type ReadFilesResponse struct {
	Files []DefinitionFile `json:"files"`
}

// CreateBundleRequest asks for a bundle of Refs, or of every ref of the
// repository (heads, tags and the hidden refs/kodama refs) when Refs is null. A
// requested ref that does not exist is not_found; a repository without a
// requested ref set (no refs at all, for null) is repository_empty.
type CreateBundleRequest struct {
	RepositoryID string   `json:"repositoryId"`
	Refs         []string `json:"refs"`
}

// CreateBundleResponse is the manifest of a bundle. In the HTTP answer it is the
// JSON line (FrameLine) that precedes the bundle bytes. Refs are exactly the refs
// the bundle holds, sorted by name. JournalHead is the ref journal's head read
// before the refs were captured; Consistent is true only when no receive or
// server operation was running at that moment and none started until the bundle
// was complete, so Refs and JournalHead are one exact boundary. It is about concurrent
// mutation, not recovery state: a bundle taken while the restore fence is raised
// is still consistent. Size and SHA256
// describe the bundle bytes that follow.
type CreateBundleResponse struct {
	RepositoryID   string   `json:"repositoryId"`
	Refs           []RefTip `json:"refs"`
	JournalHead    int64    `json:"journalHead"`
	Consistent     bool     `json:"consistent"`
	Size           int64    `json:"size"`
	SHA256         string   `json:"sha256"`
	WorkerInstance string   `json:"workerInstance"`
}

// ReplayBundleRequest is the JSON line (FrameLine) that precedes the bundle bytes
// of a replay. OperationID identifies the replay in the ref journal. SHA256, when
// not null, is the digest of the bundle bytes the worker verifies before it
// changes anything.
type ReplayBundleRequest struct {
	RepositoryID string  `json:"repositoryId"`
	OperationID  string  `json:"operationId"`
	SHA256       *string `json:"sha256"`
}

// BundleConflict is a ref that already exists at a commit other than the
// bundle's. The worker never changes it.
type BundleConflict struct {
	Ref      string         `json:"ref"`
	Current  string         `json:"current"`
	Bundle   string         `json:"bundle"`
	Relation BundleRelation `json:"relation"`
}

// ReplayBundleResponse reports what a replay did. Created refs did not exist and
// now point to the bundle's commit; Unchanged refs already pointed to it;
// Conflicting refs exist at another commit and were left alone. A replay never
// advances, rewinds or deletes a ref. A repeat of a replay that was applied
// reports the created refs as unchanged.
type ReplayBundleResponse struct {
	OperationID    string           `json:"operationId"`
	Created        int64            `json:"created"`
	Unchanged      int64            `json:"unchanged"`
	Conflicting    []BundleConflict `json:"conflicting"`
	WorkerInstance string           `json:"workerInstance"`
}

// RestoreFenceRequest enters (Fenced true) or leaves (false) the repository's
// restore fence. While the repository is fenced for restore the worker refuses
// receives, server ref operations and maintenance (503 unavailable) and accepts
// ReplayBundle, which is refused (restore_fence_required) at any other time.
// Entering waits for the running receives and operations to end. Both
// directions are idempotent.
type RestoreFenceRequest struct {
	RepositoryID string `json:"repositoryId"`
	Fenced       bool   `json:"fenced"`
}

// RestoreFenceResponse reports the fence after the call; Changed is false when it
// already was in the requested state.
type RestoreFenceResponse struct {
	RepositoryID   string `json:"repositoryId"`
	Fenced         bool   `json:"fenced"`
	Changed        bool   `json:"changed"`
	WorkerInstance string `json:"workerInstance"`
}

func checkCommitPair(a, b string) error {
	if len(a) != len(b) {
		return fail("descendant", "must have the same object ID length as ancestor")
	}
	return nil
}

func (r IsAncestorRequest) Validate() error {
	if err := firstError(checkID("repositoryId", r.RepositoryID), checkLiveCommit("ancestor", r.Ancestor), checkLiveCommit("descendant", r.Descendant)); err != nil {
		return err
	}
	return checkCommitPair(r.Ancestor, r.Descendant)
}

func (IsAncestorResponse) Validate() error { return nil }

func (r HasCommitRequest) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID), checkLiveCommit("commit", r.Commit))
}

func (HasCommitResponse) Validate() error { return nil }

func (r CommitTimeRequest) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID), checkLiveCommit("commit", r.Commit))
}

func (r CommitTimeResponse) Validate() error { return checkTimestamp("committedAt", r.CommittedAt) }

func (r ReadFilesRequest) Validate() error {
	if err := firstError(checkID("repositoryId", r.RepositoryID), checkLiveCommit("commit", r.Commit)); err != nil {
		return err
	}
	if r.Paths == nil || len(r.Paths) < 1 || len(r.Paths) > MaxReadFilePaths {
		return fail("paths", "must list 1 to %d paths", MaxReadFilePaths)
	}
	for i, p := range r.Paths {
		if p == DefinitionDir {
			continue
		}
		if err := matches(indexed("paths", i), "DefinitionPath", p); err != nil {
			return err
		}
	}
	return nil
}

func (r ReadFilesResponse) Validate() error {
	if err := checkNonNil("files", r.Files); err != nil {
		return err
	}
	if len(r.Files) > MaxReadFilePaths {
		return fail("files", "lists more than %d files", MaxReadFilePaths)
	}
	var total int
	for i, f := range r.Files {
		path := indexed("files", i)
		if err := matches(path+".path", "DefinitionPath", f.Path); err != nil {
			return err
		}
		if i > 0 && f.Path <= r.Files[i-1].Path {
			return fail(path+".path", "files are sorted by path and unique")
		}
		if len(f.Content) > MaxReadFileBytes {
			return fail(path+".content", "longer than %d bytes", MaxReadFileBytes)
		}
		if total += len(f.Content); total > MaxReadFilesBytes {
			return fail("files", "hold more than %d bytes together", MaxReadFilesBytes)
		}
		if err := matches(path+".sha256", "SHA256", f.SHA256); err != nil {
			return err
		}
		if sum := sha256.Sum256([]byte(f.Content)); hex.EncodeToString(sum[:]) != f.SHA256 {
			return fail(path+".sha256", "is not the SHA-256 of the content")
		}
	}
	return nil
}

func (r CreateBundleRequest) Validate() error {
	if err := checkID("repositoryId", r.RepositoryID); err != nil {
		return err
	}
	if r.Refs == nil {
		return nil
	}
	if len(r.Refs) < 1 || len(r.Refs) > MaxBundleRefs {
		return fail("refs", "must be null (every ref) or list 1 to %d refs", MaxBundleRefs)
	}
	seen := make(map[string]bool, len(r.Refs))
	for i, ref := range r.Refs {
		if err := ValidateRefName(ref); err != nil {
			return wrap(indexed("refs", i), err)
		}
		if seen[ref] {
			return fail(indexed("refs", i), "repeats a ref")
		}
		seen[ref] = true
	}
	return nil
}

func (r CreateBundleResponse) Validate() error {
	if err := firstError(checkID("repositoryId", r.RepositoryID), checkNonNil("refs", r.Refs), inRange("journalHead", r.JournalHead, 0, MaxSafeInteger),
		inRange("size", r.Size, 1, MaxSafeInteger), matches("sha256", "SHA256", r.SHA256), checkID("workerInstance", r.WorkerInstance)); err != nil {
		return err
	}
	if len(r.Refs) < 1 || len(r.Refs) > MaxBundleRefs {
		return fail("refs", "must list 1 to %d refs", MaxBundleRefs)
	}
	for i, t := range r.Refs {
		if err := t.Validate(); err != nil {
			return wrap(indexed("refs", i), err)
		}
		if i > 0 && t.Ref <= r.Refs[i-1].Ref {
			return fail(indexed("refs", i)+".ref", "refs must be unique and sorted by name")
		}
	}
	return nil
}

func (r ReplayBundleRequest) Validate() error {
	if err := firstError(checkID("repositoryId", r.RepositoryID), checkID("operationId", r.OperationID)); err != nil {
		return err
	}
	if r.SHA256 != nil {
		return matches("sha256", "SHA256", *r.SHA256)
	}
	return nil
}

func (c BundleConflict) Validate() error {
	if err := firstError(ValidateRefName(c.Ref), checkLiveCommit("current", c.Current), checkLiveCommit("bundle", c.Bundle),
		oneOf("relation", string(c.Relation), string(RelationRepositoryAhead), string(RelationBundleAhead), string(RelationDiverged))); err != nil {
		return err
	}
	if c.Current == c.Bundle {
		return fail("bundle", "a conflict has different values")
	}
	return nil
}

func (r ReplayBundleResponse) Validate() error {
	if err := firstError(checkID("operationId", r.OperationID), inRange("created", r.Created, 0, MaxBundleRefs), inRange("unchanged", r.Unchanged, 0, MaxBundleRefs),
		checkNonNil("conflicting", r.Conflicting), checkID("workerInstance", r.WorkerInstance)); err != nil {
		return err
	}
	if len(r.Conflicting) > MaxBundleRefs {
		return fail("conflicting", "lists more than %d refs", MaxBundleRefs)
	}
	seen := map[string]bool{}
	for i, c := range r.Conflicting {
		if err := c.Validate(); err != nil {
			return wrap(indexed("conflicting", i), err)
		}
		if seen[c.Ref] {
			return fail(indexed("conflicting", i)+".ref", "repeats a ref")
		}
		seen[c.Ref] = true
	}
	return nil
}

func (r RestoreFenceRequest) Validate() error { return checkID("repositoryId", r.RepositoryID) }

func (r RestoreFenceResponse) Validate() error {
	return firstError(checkID("repositoryId", r.RepositoryID), checkID("workerInstance", r.WorkerInstance))
}

// FrameLine encodes v as the compact JSON line (terminated by "\n") that
// precedes a bundle in a request or response body.
func FrameLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxFrameLineBytes {
		return nil, errors.New("gitv1: frame line is too large")
	}
	return append(b, '\n'), nil
}

// ErrFrameLine is returned by ReadFrameLine for a missing, unterminated or
// oversized line.
var ErrFrameLine = errors.New("gitv1: the body does not start with a JSON line")

// ReadFrameLine reads the JSON line at the start of a bundle body (without its
// terminating newline). r is positioned at the first bundle byte afterwards.
func ReadFrameLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxFrameLineBytes+1 {
			return nil, ErrFrameLine
		}
		switch {
		case err == nil:
			return bytes.TrimSuffix(line, []byte("\n")), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return nil, ErrFrameLine
		default:
			return nil, err
		}
	}
}
