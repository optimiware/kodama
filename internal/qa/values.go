// Package qa maps the deployment-bound observations of an AI exploratory QA
// attempt to ordinary defect work items with durable evidence links
// (DQ-06, DQ-10, AR-05, AR-12, WM-01).
//
// A QA attempt is an ordinary run step. It finishes with a declared report
// artifact (the structured report of this package) and screenshot artifacts. After
// the attempt completed, Service.Record verifies from trusted records only
// (never from the report's own claims) that the report inspected the deployment
// and configuration the attempt was pinned to, that every evidence artifact is a
// verified output of that attempt, and then files one defect per distinct
// finding under a deterministic identity, so a retry, a lost response or a
// repeated QA pass converges on the same defect work items.
//
// QA produces evidence and defects. It has no operation that approves a gate,
// completes work or moves any work item: the human decision stays with a
// reviewer (DQ-06, AR-12, WM-07).
//
// This package holds the report contract, the mapping rules and the use case. It
// imports only the standard library, the shared identifiers and the runtime
// contract; the work, artifact, delivery and execution modules are reached
// through the small ports in ports.go, implemented in the adapters sub-package.
package qa

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// ReportFormat is the format identifier of the report contract.
const ReportFormat = "kodama/qa-report/v1"

// Severity classifies how serious an observation is.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityMajor    Severity = "major"
	SeverityMinor    Severity = "minor"
	SeverityTrivial  Severity = "trivial"
)

// Valid reports whether s is a known severity.
func (s Severity) Valid() bool {
	switch s {
	case SeverityCritical, SeverityMajor, SeverityMinor, SeverityTrivial:
		return true
	}
	return false
}

// EvidenceKind names the type of an evidence artifact.
type EvidenceKind string

const (
	// EvidenceScreenshot is an image artifact (media type image/*).
	EvidenceScreenshot EvidenceKind = "screenshot"
	// EvidenceTrace is a browser trace, HAR or similar structured recording.
	EvidenceTrace EvidenceKind = "trace"
	// EvidenceLog is a console or network log.
	EvidenceLog EvidenceKind = "log"
)

// Valid reports whether k is a known evidence kind.
func (k EvidenceKind) Valid() bool {
	switch k {
	case EvidenceScreenshot, EvidenceTrace, EvidenceLog:
		return true
	}
	return false
}

// Bounds of a report. They keep the retained artifact, the defect text and the
// defect's evidence links (at most 50 per work item) bounded.
const (
	MaxReportBytes        = 1 << 20
	MaxObservations       = 20
	MaxEvidencePerFinding = 8
	MaxSteps              = 20
	MaxStepChars          = 500
	MaxTitleChars         = 160
	MaxLocationChars      = 512
	MaxTextChars          = 1000
	MaxSummaryChars       = 2000
	MaxNoteChars          = 200
)

var (
	criterionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	locationPattern  = regexp.MustCompile(`^/[^\s?#]*$`)
	mediaImage       = regexp.MustCompile(`^image/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}$`)
)

// text validates a bounded UTF-8 string without control characters other than
// newline and tab (multi-line allows them) that is not blank.
func text(name, v string, max int, multiline bool) error {
	if !utf8.ValidString(v) {
		return invalidf("%s must be valid UTF-8", name)
	}
	if strings.TrimSpace(v) == "" {
		return invalidf("%s must not be blank", name)
	}
	if utf8.RuneCountInString(v) > max {
		return invalidf("%s must be at most %d characters", name, max)
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			if multiline && (r == '\n' || r == '\t') {
				continue
			}
			return invalidf("%s contains a control character", name)
		}
	}
	return nil
}
