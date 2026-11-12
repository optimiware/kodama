package git

import (
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
)

// JournalVersion is the durable receive-journal format version.
const JournalVersion = 1

// JournalState is one durable receive-journal state.
type JournalState string

const (
	// JournalIntended records a persisted intent before any ref effect.
	JournalIntended JournalState = "intended"
	// JournalAccepted records a durably observed successful ref update.
	JournalAccepted JournalState = "accepted"
	// JournalReported records a delivered verified report.
	JournalReported JournalState = "reported"
	// JournalRefused records a permanent refusal before any effect.
	JournalRefused JournalState = "refused"
	// JournalUncertain records an effect whose outcome is not yet known.
	JournalUncertain JournalState = "uncertain"
)

// journalStates is the declaration order.
var journalStates = []JournalState{
	JournalIntended,
	JournalAccepted,
	JournalReported,
	JournalRefused,
	JournalUncertain,
}

// Valid reports whether the state is one of the declared states.
func (state JournalState) Valid() bool {
	for _, candidate := range journalStates {
		if candidate == state {
			return true
		}
	}
	return false
}

// RecoveryOutcome is the reconciliation of one intent against observed state.
type RecoveryOutcome string

const (
	// RecoveryNotApplied proves the effect never happened.
	RecoveryNotApplied RecoveryOutcome = "not_applied"
	// RecoveryApplied proves the effect happened.
	RecoveryApplied RecoveryOutcome = "applied"
	// RecoveryUnknown proves neither; the effect needs operator reconciliation.
	RecoveryUnknown RecoveryOutcome = "unknown"
)

// JournalEntry is one durable receive-journal row. It never carries a
// credential, token or forwarded actor material.
type JournalEntry struct {
	Version        int          `json:"version"`
	ResourceID     string       `json:"resource_id"`
	Repository     string       `json:"repository"`
	Ref            string       `json:"ref"`
	ExpectedBase   string       `json:"expected_base"`
	Candidate      string       `json:"candidate"`
	SourceEventID  string       `json:"source_event_id"`
	VerificationID string       `json:"verification_id"`
	State          JournalState `json:"state"`
	At             time.Time    `json:"at"`
}

// Validate checks the versioned format, identity, ref, state and time.
func (entry JournalEntry) Validate() error {
	if entry.Version != JournalVersion {
		return fmt.Errorf("journal version %d is not %d", entry.Version, JournalVersion)
	}
	if !validJournalID(entry.ResourceID) {
		return fmt.Errorf("journal resource id %q is invalid", entry.ResourceID)
	}
	if !validRepositoryName(entry.Repository) {
		return fmt.Errorf("journal repository %q is invalid", entry.Repository)
	}
	if !validRef(entry.Ref) || strings.HasSuffix(entry.Ref, "/") {
		return fmt.Errorf("journal ref %q is invalid", entry.Ref)
	}
	if entry.Candidate == "" {
		return fmt.Errorf("journal candidate is empty")
	}
	if !validJournalID(entry.SourceEventID) {
		return fmt.Errorf("journal source event id %q is invalid", entry.SourceEventID)
	}
	if !validJournalID(entry.VerificationID) {
		return fmt.Errorf("journal verification id %q is invalid", entry.VerificationID)
	}
	if !entry.State.Valid() {
		return fmt.Errorf("journal state %q is unknown", entry.State)
	}
	if entry.At.IsZero() {
		return fmt.Errorf("journal time is zero")
	}
	return nil
}

// validJournalID reports whether value is a bounded ASCII identity.
func validJournalID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	if value == "." || value == ".." {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// Reconcile compares one intent against the observed ref. Only an unresolved
// intent or uncertainty may be reconciled; an accepted entry is already settled.
func (entry JournalEntry) Reconcile(observed RefState) (RecoveryOutcome, error) {
	if entry.State != JournalIntended && entry.State != JournalUncertain {
		return "", fmt.Errorf("journal state %q is already settled", entry.State)
	}
	if observed.Ref != entry.Ref {
		return "", fmt.Errorf("observation ref %q does not match journal ref %q", observed.Ref, entry.Ref)
	}
	switch observed.Commit {
	case entry.Candidate:
		return RecoveryApplied, nil
	case entry.ExpectedBase:
		return RecoveryNotApplied, nil
	default:
		return RecoveryUnknown, nil
	}
}

// Reportable reports whether a durably recorded acceptance may be reported.
func (entry JournalEntry) Reportable() bool {
	return entry.State == JournalAccepted
}

// ReportRequest builds the deterministic verified report for an accepted entry.
// The approval subject uses the canonical KODAMA field names: `operation`,
// `resource` (the registered repository resource identity), `ref`,
// `expected_base` and `candidate`. There is no `repository` alias. A retry
// repeats the same report identity and content; it never repeats the ref
// update.
func (entry JournalEntry) ReportRequest() (api.ReportRequest, error) {
	if !entry.Reportable() {
		return api.ReportRequest{}, fmt.Errorf("journal entry in state %q is not reportable", entry.State)
	}
	return api.ReportRequest{
		ResourceID:     entry.ResourceID,
		SourceEventID:  entry.SourceEventID,
		EventType:      "git.ref.updated",
		Operation:      string(api.ResourceOperationGitRefUpdate),
		VerificationID: entry.VerificationID,
		Subject: api.Subject{Fields: []api.SubjectField{
			{Name: "operation", Kind: "string", Value: string(api.ResourceOperationGitRefUpdate)},
			{Name: "resource", Kind: "string", Value: entry.ResourceID},
			{Name: "ref", Kind: "string", Value: entry.Ref},
			{Name: "expected_base", Kind: "string", Value: entry.ExpectedBase},
			{Name: "candidate", Kind: "string", Value: entry.Candidate},
		}},
	}, nil
}
