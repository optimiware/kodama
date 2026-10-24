// Package contract freezes the module-owned representation of log capture
// completeness: a small, versioned diagnostic sidecar that binds a completeness
// statement to one retained log artifact by its ID, digest and size.
//
// The frozen runtime artifact envelopes have no completeness field, so the
// existing plain-text diagnostic artifacts (`command.log`, `entrypoint.log`)
// are unchanged and the sidecar is uploaded as one more ordinary diagnostic
// file. A log artifact without a valid sidecar has UNKNOWN completeness, never
// "complete". Producers (T-063 entrypoint upload, T-064 controller fallback
// join) use this package for their fixtures; no runtime route, action or wire
// field is added.
package contract

import (
	"fmt"
	"regexp"

	"github.com/optimiware/kodama/internal/domain"
)

// Version is the sidecar document version.
const Version = 1

// MaxMetadataBytes bounds a sidecar document; a larger one is refused before
// it is parsed.
const MaxMetadataBytes = 4096

// MediaType is the media type of a sidecar artifact.
const MediaType = "application/vnd.kodama.log-diagnostic.v1+json"

// LogMediaType is the media type of the existing plain-text log artifacts.
const LogMediaType = "text/plain"

// Names of the artifacts the entrypoint already uploads.
const (
	CommandLogName    = "command.log"
	EntrypointLogName = "entrypoint.log"
	// MetadataSuffix is appended to a log artifact's name to form its sidecar.
	MetadataSuffix = ".meta.json"
)

// Source says which component captured the log.
type Source string

const (
	SourceWrapper    Source = "wrapper"
	SourceController Source = "controller"
)

// Valid reports whether s is a known source.
func (s Source) Valid() bool { return s == SourceWrapper || s == SourceController }

// Completeness describes what was captured of the command's diagnostics. It
// says nothing about the command's success or the durable finalization of the
// attempt, and a later retention removal does not change it.
type Completeness string

const (
	// Complete: the capturing component observed the whole command output.
	// Only a wrapper that read its bounded log to the end can say this.
	Complete Completeness = "complete"
	// Partial: part of the output is known to be missing.
	Partial Completeness = "partial"
	// Unknown: whether anything is missing cannot be established. This is the
	// state of a log with no sidecar and of any controller capture without
	// further evidence.
	Unknown Completeness = "unknown"
)

// Valid reports whether c is a known value.
func (c Completeness) Valid() bool { return c == Complete || c == Partial || c == Unknown }

// Reason is a classified explanation of a non-complete capture.
type Reason string

const (
	// ReasonTruncated: the bounded capture dropped output (size or line limit).
	ReasonTruncated Reason = "truncated"
	// ReasonInterrupted: the capturing stream ended abnormally.
	ReasonInterrupted Reason = "interrupted"
	// ReasonUploadFailed: the wrapper could not upload all of its log.
	ReasonUploadFailed Reason = "upload_failed"
	// ReasonWorkloadMissing: the Job or Pod was gone when the controller looked.
	ReasonWorkloadMissing Reason = "workload_missing"
	// ReasonRotated: the container log was rotated by the node.
	ReasonRotated Reason = "rotated"
	// ReasonControllerCapture: captured by the controller from the Kubernetes
	// log subresource; end of stream alone does not prove completeness.
	ReasonControllerCapture Reason = "controller_capture"
	// ReasonUnverified: no sidecar binds a completeness statement.
	ReasonUnverified Reason = "unverified"
)

var knownReasons = map[Reason]bool{
	ReasonTruncated: true, ReasonInterrupted: true, ReasonUploadFailed: true, ReasonWorkloadMissing: true,
	ReasonRotated: true, ReasonControllerCapture: true, ReasonUnverified: true,
}

// Valid reports whether r is a known reason.
func (r Reason) Valid() bool { return knownReasons[r] }

// MaxReasons bounds the reasons of one statement.
const MaxReasons = 8

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidName reports whether name is a safe artifact name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// MetadataName returns the sidecar's artifact name for a log artifact name.
func MetadataName(logName string) (string, error) {
	if !ValidName(logName) {
		return "", fmt.Errorf("%w: log name is not a safe artifact name", ErrInvalid)
	}
	n := logName + MetadataSuffix
	if !ValidName(n) {
		return "", fmt.Errorf("%w: sidecar name is too long", ErrInvalid)
	}
	return n, nil
}

// ControllerLogName returns the artifact name of the controller capture of a
// generation. It never equals a wrapper artifact name, so the two sources
// coexist without either overwriting the other.
func ControllerLogName(generation int) string {
	return fmt.Sprintf("controller-g%d.log", generation)
}

// Ref binds a statement to one log artifact.
type Ref struct {
	ArtifactID domain.ArtifactID `json:"artifactId"`
	Digest     string            `json:"digest"`
	SizeBytes  int64             `json:"sizeBytes"`
}

// Metadata is the sidecar document.
type Metadata struct {
	Version      int          `json:"version"`
	Source       Source       `json:"source"`
	LogName      string       `json:"logName"`
	Log          Ref          `json:"log"`
	Completeness Completeness `json:"completeness"`
	Reasons      []Reason     `json:"reasons"`
}

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Validate checks the document's own invariants:
//   - a complete capture has no reasons; a partial or unknown one has at least one;
//   - a controller capture is never complete (EOF of the Kubernetes log stream
//     cannot establish complete command capture) and says so;
//   - reasons are known and unique.
func (m Metadata) Validate() error {
	switch {
	case m.Version != Version:
		return fmt.Errorf("%w: unsupported version %d", ErrInvalid, m.Version)
	case !m.Source.Valid():
		return fmt.Errorf("%w: unknown source", ErrInvalid)
	case !ValidName(m.LogName):
		return fmt.Errorf("%w: log name is not a safe artifact name", ErrInvalid)
	case m.Log.ArtifactID.Validate() != nil:
		return fmt.Errorf("%w: log artifact id is not valid", ErrInvalid)
	case !digestRe.MatchString(m.Log.Digest):
		return fmt.Errorf("%w: log digest is not valid", ErrInvalid)
	case m.Log.SizeBytes < 0 || m.Log.SizeBytes > 1<<40:
		return fmt.Errorf("%w: log size is out of range", ErrInvalid)
	case !m.Completeness.Valid():
		return fmt.Errorf("%w: unknown completeness", ErrInvalid)
	case len(m.Reasons) > MaxReasons:
		return fmt.Errorf("%w: too many reasons", ErrInvalid)
	}
	seen := map[Reason]bool{}
	for _, r := range m.Reasons {
		if !r.Valid() {
			return fmt.Errorf("%w: unknown reason", ErrInvalid)
		}
		if seen[r] {
			return fmt.Errorf("%w: duplicate reason", ErrInvalid)
		}
		seen[r] = true
	}
	if m.Completeness == Complete && len(m.Reasons) > 0 {
		return fmt.Errorf("%w: a complete capture has no reasons", ErrInvalid)
	}
	if m.Completeness != Complete && len(m.Reasons) == 0 {
		return fmt.Errorf("%w: a %s capture needs a reason", ErrInvalid, m.Completeness)
	}
	if m.Source == SourceController {
		if m.Completeness == Complete {
			return fmt.Errorf("%w: a controller capture is never complete", ErrInvalid)
		}
		if !seen[ReasonControllerCapture] {
			return fmt.Errorf("%w: a controller capture states its source", ErrInvalid)
		}
	}
	return nil
}

// Bind checks that the statement is about exactly this verified artifact: the
// same ID, digest and size. A sidecar copied onto another artifact (or one
// whose log was replaced) is refused.
func (m Metadata) Bind(id domain.ArtifactID, digest string, size int64) error {
	if m.Log.ArtifactID != id || m.Log.Digest != digest || m.Log.SizeBytes != size {
		return fmt.Errorf("%w: the sidecar describes a different artifact", ErrBinding)
	}
	return nil
}
