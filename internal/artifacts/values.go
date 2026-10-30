package artifacts

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Kind distinguishes a file stored in the object store from a registration of
// content that lives in an OCI registry.
type Kind string

const (
	KindFile Kind = "file"
	KindOCI  Kind = "oci"
)

// Role says why the producer attempt created the artifact.
type Role string

const (
	RoleOutput     Role = "output"
	RoleDiagnostic Role = "diagnostic"
	RoleBundle     Role = "bundle"
)

// State is the artifact's content lifecycle. It deliberately mirrors the frozen
// runtime enumeration and nothing else: orphan and retention bookkeeping is the
// separate Disposition, so a cleanup concern never becomes a new runtime state.
type State string

const (
	// StateReserved: a file reservation whose bytes have not been verified.
	// Never usable.
	StateReserved State = "reserved"
	// StateRegistered: an OCI registration verified against the registry.
	// It is a usable trusted reference.
	StateRegistered State = "registered"
	// StateUsable: a file whose bytes were verified and sealed.
	StateUsable State = "usable"
)

// Disposition is local bookkeeping about the artifact's stored bytes.
type Disposition string

const (
	DispositionActive    Disposition = "active"
	DispositionAbandoned Disposition = "abandoned" // reserved, never completed, cleaned up
	DispositionRemoved   Disposition = "removed"   // content deleted under an explicit retention policy
)

// Digest is "sha256:" and 64 lowercase hexadecimal digits.
type Digest string

var (
	digestRe   = regexp.MustCompile(runtimev1.Patterns["Digest"])
	nameRe     = regexp.MustCompile(runtimev1.Patterns["SafeName"])
	mediaRe    = regexp.MustCompile(runtimev1.Patterns["MediaType"])
	commitRe   = regexp.MustCompile(runtimev1.Patterns["GitCommit"])
	repoURLRe  = regexp.MustCompile(runtimev1.Patterns["RepositoryURL"])
	registryRe = regexp.MustCompile(runtimev1.Patterns["RegistryRef"])
	// repositoryIDRe is the opaque identifier syntax of a repository.
	repositoryIDRe = regexp.MustCompile(domain.IDPattern)
)

// MaxFileBytes is the contract's upper bound for a declared size.
const MaxFileBytes int64 = 1 << 40

// NewDigest hashes nothing; it validates the textual form.
func NewDigest(s string) (Digest, error) {
	d := Digest(s)
	return d, d.Validate()
}

// DigestOfSum builds a digest from raw SHA-256 bytes.
func DigestOfSum(sum []byte) Digest { return Digest("sha256:" + hex.EncodeToString(sum)) }

// DigestOf hashes data.
func DigestOf(data []byte) Digest {
	s := sha256.Sum256(data)
	return DigestOfSum(s[:])
}

func (d Digest) Validate() error {
	if !digestRe.MatchString(string(d)) {
		return invalidf("digest must be sha256:<64 lowercase hex digits>")
	}
	return nil
}

// Hex returns the hexadecimal part of a valid digest.
func (d Digest) Hex() string { return strings.TrimPrefix(string(d), "sha256:") }

// Base64 returns the base64 of the raw digest, the form S3 uses for
// x-amz-checksum-sha256.
func (d Digest) Base64() string {
	raw, err := hex.DecodeString(d.Hex())
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// MaxSourceCommits bounds one source set (the Git contract allows 32 sources).
const MaxSourceCommits = 32

// SourceCommit is one repository of a source set at the commit the producing
// attempt worked from.
type SourceCommit struct {
	RepositoryID string
	Commit       string
}

// SourceSet is the provenance of an attempt of a multi-repository run: the
// canonical digest of the run's pinned source set and, for every repository of
// it, the commit the attempt worked from (its head when it began), ordered by
// repository ID. It replaces the single repository URL and commit of
// Provenance (DQ-17). The zero value is "no source set". A SourceSet is
// comparable: the commits are held in one canonical encoding, so two sets are
// equal exactly when their digests and commits are.
type SourceSet struct {
	Digest  string
	commits string
}

// NewSourceSet validates and canonicalizes a source set: a digest, 1 to
// MaxSourceCommits commits, a repository once, ordered by repository ID.
func NewSourceSet(digest string, commits []SourceCommit) (SourceSet, error) {
	if !digestRe.MatchString(digest) {
		return SourceSet{}, invalidf("a source set has a digest sha256:<64 lowercase hex digits>")
	}
	if len(commits) < 1 || len(commits) > MaxSourceCommits {
		return SourceSet{}, invalidf("a source set has 1 to %d commits", MaxSourceCommits)
	}
	sorted := append([]SourceCommit(nil), commits...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RepositoryID < sorted[j].RepositoryID })
	parts := make([]string, len(sorted))
	for i, c := range sorted {
		if !repositoryIDRe.MatchString(c.RepositoryID) || !commitRe.MatchString(c.Commit) {
			return SourceSet{}, invalidf("a source commit needs a repository ID and a Git commit")
		}
		if i > 0 && sorted[i-1].RepositoryID == c.RepositoryID {
			return SourceSet{}, invalidf("a source set names a repository once")
		}
		parts[i] = c.RepositoryID + "@" + c.Commit
	}
	return SourceSet{Digest: digest, commits: strings.Join(parts, ",")}, nil
}

// IsZero reports "no source set".
func (s SourceSet) IsZero() bool { return s == SourceSet{} }

// Commits returns the per-repository commits ordered by repository ID.
func (s SourceSet) Commits() []SourceCommit {
	if s.commits == "" {
		return nil
	}
	parts := strings.Split(s.commits, ",")
	out := make([]SourceCommit, len(parts))
	for i, p := range parts {
		id, commit, _ := strings.Cut(p, "@")
		out[i] = SourceCommit{RepositoryID: id, Commit: commit}
	}
	return out
}

// Validate checks the set as stored: it must be exactly what NewSourceSet builds.
func (s SourceSet) Validate() error {
	if s.IsZero() {
		return nil
	}
	n, err := NewSourceSet(s.Digest, s.Commits())
	if err != nil {
		return err
	}
	if n != s {
		return invalidf("a source set is in canonical form")
	}
	return nil
}

// Provenance is the trusted source a producing attempt worked from. It is
// resolved by the execution module, never taken from the caller or from
// registry annotations. The zero value means the producer had no source. A
// single-repository run records its repository URL and commit; a run with a
// source set records the SourceSet instead (never both), so earlier provenance
// is unchanged.
//
// A run_shared attempt works in a scratch workspace that earlier steps may have
// changed, so its provenance also carries what the server recorded when the
// attempt's command began (DQ-16): WorkspaceState is clean when the workspace was
// exactly the commit, and modified when it was not (head moved, tracked files
// changed or untracked files present); IgnoredPresent discloses ignored files;
// Lineage is the digest of the lineage the attempt's pinned context carried (the
// earlier attempts that touched the scratch). They are empty for an
// attempt_private producer, whose checkout is the pinned commit. A modified
// workspace is never the commit it started from (Modified), so an artifact built
// there cannot stand for the approved candidate.
type Provenance struct {
	RepositoryURL  string
	Commit         string
	WorkspaceState string
	IgnoredPresent bool
	Lineage        string
	SourceSet      SourceSet
}

// Workspace states of a recorded begin.
const (
	WorkspaceClean    = "clean"
	WorkspaceModified = "modified"
)

// IsZero reports that the producer had no source.
func (p Provenance) IsZero() bool { return p == Provenance{} }

// Modified reports that the producing workspace was not the commit it started
// from: the artifact does not stand for that commit.
func (p Provenance) Modified() bool { return p.WorkspaceState == WorkspaceModified }

// Validate checks that the provenance is zero, a single source or a source set.
func (p Provenance) Validate() error {
	if p.IsZero() {
		return nil
	}
	if !p.SourceSet.IsZero() {
		if p.RepositoryURL != "" || p.Commit != "" {
			return invalidf("provenance is a single source or a source set, not both")
		}
		if p.WorkspaceState != "" || p.IgnoredPresent || p.Lineage != "" {
			return invalidf("a source set carries no workspace provenance")
		}
		return p.SourceSet.Validate()
	}
	if !repoURLRe.MatchString(p.RepositoryURL) || !commitRe.MatchString(p.Commit) {
		return invalidf("provenance needs a credential-free HTTPS repository URL and a Git commit")
	}
	switch p.WorkspaceState {
	case "", WorkspaceClean, WorkspaceModified:
	default:
		return invalidf("provenance names an unknown workspace state")
	}
	if p.WorkspaceState == "" && (p.IgnoredPresent || p.Lineage != "") {
		return invalidf("workspace provenance needs a recorded state")
	}
	if p.Lineage != "" && !lineageRe.MatchString(p.Lineage) {
		return invalidf("the lineage reference is not a digest")
	}
	return nil
}

var lineageRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Limits bound what one attempt may keep. OutputBytes, OutputFiles and
// DiagnosticBytes come from the attempt's immutable context, never from the
// request; the remaining bounds are service configuration.
type Limits struct {
	OutputBytes     int64
	OutputFiles     int64
	DiagnosticBytes int64
	DiagnosticFiles int64
	BundleBytes     int64
	BundleFiles     int64
}

// Validate requires nonnegative bounds.
func (l Limits) Validate() error {
	if l.OutputBytes < 0 || l.OutputFiles < 0 || l.DiagnosticBytes < 0 || l.DiagnosticFiles < 0 || l.BundleBytes < 0 || l.BundleFiles < 0 {
		return invalidf("limits must not be negative")
	}
	return nil
}

// OutputDecl is one declared artifact output of the attempt.
type OutputDecl struct {
	Name string
	Kind Kind
}

// Caller is the verified identity the transport authenticated. The module never
// inspects it; it only hands it to its authority ports.
type Caller struct {
	Subject string
}

// ObjectKey builds a logical storage key. Every component is an identifier
// validated by its type, so a key can never contain a separator, a dot segment
// or caller-chosen text. The object-store adapter adds its bucket prefix.
func ObjectKey(scope domain.Scope, id domain.ArtifactID, segment, nonce string) string {
	return fmt.Sprintf("artifacts/%s/%s/%s/%s/%s", scope.TenantID, scope.ProjectID, id, segment, nonce)
}

// Key segments: where unverified uploads land and where verified bytes are
// sealed.
const (
	SegmentStaging = "staging"
	SegmentSealed  = "sealed"
)
