package logs

import (
	"regexp"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs/contract"
)

// Source, Completeness and Reason are the contract's values.
type (
	Source       = contract.Source
	Completeness = contract.Completeness
	Reason       = contract.Reason
)

// Re-exported constants.
const (
	SourceWrapper    = contract.SourceWrapper
	SourceController = contract.SourceController

	Complete = contract.Complete
	Partial  = contract.Partial
	Unknown  = contract.Unknown
)

// RecordID identifies one log record (one capture generation of one source).
type RecordID string

var recordIDRe = regexp.MustCompile(domain.IDPattern)

// Validate checks the identifier syntax.
func (id RecordID) Validate() error {
	if !recordIDRe.MatchString(string(id)) {
		return invalidf("record id must match %s", domain.IDPattern)
	}
	return nil
}

// State is the capture lifecycle of a record.
type State string

const (
	// StatePending: the capture is known but nobody collects it.
	StatePending State = "pending"
	// StateCollecting: a collector holds the fenced claim.
	StateCollecting State = "collecting"
	// StateRetained: verified artifacts are linked. Terminal and immutable.
	StateRetained State = "retained"
	// StateUnavailable: collection failed; Failure classifies why and whether
	// another claim may retry.
	StateUnavailable State = "unavailable"
)

// FailureClass classifies why a capture is unavailable.
type FailureClass string

const (
	FailureWorkloadMissing FailureClass = "workload_missing"
	FailureBackend         FailureClass = "backend_unavailable"
	FailureStore           FailureClass = "store_unavailable"
	FailureFrozenLost      FailureClass = "frozen_bytes_lost"
	FailureLimit           FailureClass = "limit_exceeded"
)

// Retryable reports whether another claim of the same generation may retry.
// A missing workload and lost frozen bytes will not recover by retrying the
// same capture.
func (f FailureClass) Retryable() bool {
	switch f {
	case FailureBackend, FailureStore:
		return true
	}
	return false
}

// Valid reports whether f is a known class.
func (f FailureClass) Valid() bool {
	switch f {
	case FailureWorkloadMissing, FailureBackend, FailureStore, FailureFrozenLost, FailureLimit:
		return true
	}
	return false
}

// Claim is the durable, fenced collection lease.
type Claim struct {
	Token string
	// Fence only increases; a collector holding an older fence is stale.
	Fence int64
	Until time.Time
}

// Active reports whether the lease is held at now.
func (c Claim) Active(now time.Time) bool { return c.Token != "" && now.Before(c.Until) }

// ArtifactRef names a verified, usable artifact.
type ArtifactRef struct {
	ID     domain.ArtifactID
	Digest string
	Size   int64
}

func (a ArtifactRef) validate() error {
	if err := a.ID.Validate(); err != nil {
		return invalidf("artifact id: %v", err)
	}
	if !digestRe.MatchString(a.Digest) {
		return invalidf("artifact digest is not valid")
	}
	if a.Size < 0 {
		return invalidf("artifact size is negative")
	}
	return nil
}

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Statement is the completeness evidence of one capture.
type Statement struct {
	Completeness Completeness
	Reasons      []Reason
}

func (s Statement) validate(src Source) error {
	// The contract's own invariants, checked through a throwaway document.
	m := contract.Metadata{Version: contract.Version, Source: src, LogName: "x", Completeness: s.Completeness, Reasons: s.Reasons,
		Log: contract.Ref{ArtifactID: "a", Digest: "sha256:" + zeros, SizeBytes: 0}}
	if err := m.Validate(); err != nil {
		return invalidf("completeness statement: %v", err)
	}
	return nil
}

const zeros = "0000000000000000000000000000000000000000000000000000000000000000"

func (s Statement) equal(o Statement) bool {
	if s.Completeness != o.Completeness || len(s.Reasons) != len(o.Reasons) {
		return false
	}
	for i := range s.Reasons {
		if s.Reasons[i] != o.Reasons[i] {
			return false
		}
	}
	return true
}

// Snapshot is the frozen identity of the bytes one capture uploads. It is
// recorded before any upload starts so an unknown outcome can be reconciled
// with the same declaration instead of being recaptured.
type Snapshot struct {
	LogName  string
	MetaName string
	Digest   string
	Size     int64
	Statement
}

func (s Snapshot) validate(src Source) error {
	if !contract.ValidName(s.LogName) || !contract.ValidName(s.MetaName) || s.LogName == s.MetaName {
		return invalidf("snapshot names are not safe artifact names")
	}
	if !digestRe.MatchString(s.Digest) || s.Size < 0 {
		return invalidf("snapshot digest or size is not valid")
	}
	return s.Statement.validate(src)
}

func (s Snapshot) equal(o Snapshot) bool {
	return s.LogName == o.LogName && s.MetaName == o.MetaName && s.Digest == o.Digest && s.Size == o.Size && s.Statement.equal(o.Statement)
}

// Link is the verified linkage of a retained capture: the usable log artifact,
// its sidecar (absent for a legacy wrapper log) and the completeness evidence.
type Link struct {
	Log  ArtifactRef
	Meta *ArtifactRef
	Statement
}

func (l Link) validate(src Source) error {
	if err := l.Log.validate(); err != nil {
		return err
	}
	if l.Meta != nil {
		if err := l.Meta.validate(); err != nil {
			return err
		}
		if l.Meta.ID == l.Log.ID {
			return invalidf("a sidecar is a different artifact than its log")
		}
	}
	return l.Statement.validate(src)
}

func (l Link) equal(o Link) bool {
	if l.Log != o.Log || !l.Statement.equal(o.Statement) {
		return false
	}
	if (l.Meta == nil) != (o.Meta == nil) {
		return false
	}
	return l.Meta == nil || *l.Meta == *o.Meta
}
