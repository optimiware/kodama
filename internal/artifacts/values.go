package artifacts

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
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

// Provenance is the trusted source a producing attempt worked from. It is
// resolved by the execution module, never taken from the caller or from
// registry annotations. The zero value means the producer had no source.
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
}

// Workspace states of a recorded begin.
const (
	WorkspaceClean    = "clean"
	WorkspaceModified = "modified"
)

func (p Provenance) IsZero() bool { return p == Provenance{} }

// Modified reports that the producing workspace was not the commit it started
// from: the artifact does not stand for that commit.
func (p Provenance) Modified() bool { return p.WorkspaceState == WorkspaceModified }

func (p Provenance) Validate() error {
	if p.IsZero() {
		return nil
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
